package bttest

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"sort"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/jhsenjaliya/little_bigtable/bttest/internal/gsql"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	preparedQueryMagic    = "lbq1"
	preparedQueryLifetime = time.Hour
	queryBatchBytes       = 1 << 20
)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// preparedQueries is kept for server wiring; prepared queries are
// self-describing tokens, so no per-query state is held.
type preparedQueries struct{}

func newPreparedQueries() *preparedQueries { return &preparedQueries{} }

// sqlCatalog exposes one instance's tables and views to the query engine.
type sqlCatalog struct {
	s        *server
	instance string
}

func (s *server) catalog(instance string) *sqlCatalog { return &sqlCatalog{s: s, instance: instance} }

func (c *sqlCatalog) LookupTable(name string) (*gsql.Table, bool) {
	c.s.mu.Lock()
	tbl, ok := c.s.tables[c.instance+"/tables/"+name]
	c.s.mu.Unlock()
	if !ok {
		return nil, false
	}
	tbl.mu.RLock()
	defer tbl.mu.RUnlock()
	t := &gsql.Table{Name: name}
	fams := make([]*columnFamily, 0, len(tbl.families))
	ids := map[*columnFamily]string{}
	for id, cf := range tbl.families {
		fams = append(fams, cf)
		ids[cf] = id
	}
	sort.Slice(fams, func(i, j int) bool { return fams[i].Order < fams[j].Order })
	for _, cf := range fams {
		kind := gsql.FamilyBytes
		switch familyAggregate(cf) {
		case aggregateSum, aggregateMin, aggregateMax:
			kind = gsql.FamilyInt64Aggregate
		case aggregateHLL:
			kind = gsql.FamilyHLLAggregate
		}
		t.Families = append(t.Families, gsql.Family{Name: ids[cf], Kind: kind})
	}
	if tbl.rowKeySchema != nil {
		if schema, err := dataStructType(tbl.rowKeySchema); err == nil {
			t.RowKeySchema = schema
		}
	}
	return t, true
}

// dataStructType converts an admin Type.Struct to the wire-identical data
// API message.
func dataStructType(s *btapb.Type_Struct) (*btpb.Type_Struct, error) {
	raw, err := proto.Marshal(s)
	if err != nil {
		return nil, err
	}
	out := &btpb.Type_Struct{}
	return out, proto.Unmarshal(raw, out)
}

func (c *sqlCatalog) LookupView(name string) (string, bool) {
	if lv, ok, err := c.s.lvBackend.store.get(context.Background(), nil, c.instance+"/logicalViews/"+name); err == nil && ok {
		return lv.GetQuery(), true
	}
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if mv, ok := c.s.mvs[c.instance+"/materializedViews/"+name]; ok {
		return mv.query, true
	}
	return "", false
}

func (c *sqlCatalog) Scan(ctx context.Context, tableName string, start, end []byte, fn func(gsql.Row) bool) error {
	c.s.mu.Lock()
	tbl, ok := c.s.tables[c.instance+"/tables/"+tableName]
	c.s.mu.Unlock()
	if !ok {
		return status.Errorf(codes.NotFound, "table %q not found", tableName)
	}
	tbl.mu.RLock()
	gcRules := tbl.gcRulesNoLock()
	families := make(map[string]bool, len(tbl.families))
	for f := range tbl.families {
		families[f] = true
	}
	var rows []*row
	err := tbl.rows.scan(ctx, nil, keyRange{start: string(start), end: string(end)}, false, func(r *row) (bool, error) {
		rows = append(rows, r)
		return true, nil
	})
	tbl.mu.RUnlock()
	if err != nil {
		return storageErr(err)
	}
	for _, r := range rows {
		for f := range r.families {
			if !families[f] {
				delete(r.families, f)
			}
		}
		r.gc(gcRules)
		if r.isEmpty() {
			continue
		}
		if !fn(toSQLRow(r)) {
			return nil
		}
	}
	return nil
}

func toSQLRow(r *row) gsql.Row {
	out := gsql.Row{Key: []byte(r.key), Families: make(map[string]map[string][]gsql.Cell, len(r.families))}
	for name, fam := range r.families {
		cols := make(map[string][]gsql.Cell, len(fam.Cells))
		for col, cs := range fam.Cells {
			cells := make([]gsql.Cell, len(cs))
			for i, c := range cs {
				cells[i] = gsql.Cell{TimestampMicros: c.Ts, Value: c.Value}
			}
			cols[col] = cells
		}
		out.Families[name] = cols
	}
	return out
}

func (s *server) validateViewQuery(instance, query string) error {
	_, err := gsql.Prepare(query, nil, s.catalog(instance))
	return err
}

// preparedToken is the opaque prepared_query: the original request plus the
// metadata the client was given, so a changed plan is detected on execute.
func encodePreparedToken(req *btpb.PrepareQueryRequest, md *btpb.ResultSetMetadata) ([]byte, error) {
	reqBytes, err := proto.Marshal(&btpb.PrepareQueryRequest{InstanceName: req.GetInstanceName(), Query: req.GetQuery(), ParamTypes: req.GetParamTypes()})
	if err != nil {
		return nil, err
	}
	mdBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(md)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString(preparedQueryMagic)
	var n [binary.MaxVarintLen64]byte
	buf.Write(n[:binary.PutUvarint(n[:], uint64(len(reqBytes)))])
	buf.Write(reqBytes)
	buf.Write(mdBytes)
	return buf.Bytes(), nil
}

func decodePreparedToken(token []byte) (*btpb.PrepareQueryRequest, []byte, error) {
	if !bytes.HasPrefix(token, []byte(preparedQueryMagic)) {
		return nil, nil, status.Error(codes.InvalidArgument, "prepared_query was not produced by this emulator")
	}
	rest := token[len(preparedQueryMagic):]
	n, k := binary.Uvarint(rest)
	if k <= 0 || uint64(len(rest)-k) < n {
		return nil, nil, status.Error(codes.InvalidArgument, "malformed prepared_query")
	}
	req := &btpb.PrepareQueryRequest{}
	if err := proto.Unmarshal(rest[k:k+int(n)], req); err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, "malformed prepared_query")
	}
	return req, rest[k+int(n):], nil
}

func (s *server) PrepareQuery(ctx context.Context, req *btpb.PrepareQueryRequest) (*btpb.PrepareQueryResponse, error) {
	if req.GetInstanceName() == "" || req.GetQuery() == "" {
		return nil, status.Error(codes.InvalidArgument, "instance_name and query are required")
	}
	if err := s.localRequireInstance(req.GetInstanceName()); err != nil {
		return nil, err
	}
	if _, err := s.resolveAppProfile(req.GetInstanceName(), req.GetAppProfileId()); err != nil {
		return nil, err
	}
	p, err := gsql.Prepare(req.GetQuery(), req.GetParamTypes(), s.catalog(req.GetInstanceName()))
	if err != nil {
		return nil, err
	}
	md := p.Metadata()
	token, err := encodePreparedToken(req, md)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode prepared query: %v", err)
	}
	return &btpb.PrepareQueryResponse{
		Metadata:      md,
		PreparedQuery: token,
		ValidUntil:    timestamppb.New(time.Now().Add(preparedQueryLifetime)),
	}, nil
}

func preparedQueryExpired(msg string) error {
	st := status.New(codes.FailedPrecondition, msg)
	if detailed, err := st.WithDetails(&errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{{
		Type: "PREPARED_QUERY_EXPIRED", Description: msg,
	}}}); err == nil {
		st = detailed
	}
	return st.Err()
}

func (s *server) ExecuteQuery(req *btpb.ExecuteQueryRequest, stream btpb.Bigtable_ExecuteQueryServer) error {
	ctx := stream.Context()
	instance := req.GetInstanceName()
	if instance == "" {
		return status.Error(codes.InvalidArgument, "instance_name is required")
	}
	if (len(req.GetPreparedQuery()) == 0) == (req.GetQuery() == "") {
		return status.Error(codes.InvalidArgument, "exactly one of query or prepared_query must be set")
	}
	if err := s.localRequireInstance(instance); err != nil {
		return err
	}
	if _, err := s.resolveAppProfile(instance, req.GetAppProfileId()); err != nil {
		return err
	}
	cat := s.catalog(instance)

	var p *gsql.Prepared
	sendMetadata := false
	if token := req.GetPreparedQuery(); len(token) > 0 {
		prepReq, mdBytes, err := decodePreparedToken(token)
		if err != nil {
			return err
		}
		if prepReq.GetInstanceName() != instance {
			return status.Error(codes.InvalidArgument, "prepared_query belongs to a different instance")
		}
		p, err = gsql.Prepare(prepReq.GetQuery(), prepReq.GetParamTypes(), cat)
		if err != nil {
			return preparedQueryExpired("the prepared query is no longer valid: " + status.Convert(err).Message())
		}
		current, _ := proto.MarshalOptions{Deterministic: true}.Marshal(p.Metadata())
		if !bytes.Equal(current, mdBytes) {
			return preparedQueryExpired("the table schema changed since the query was prepared")
		}
	} else {
		paramTypes := map[string]*btpb.Type{}
		for name, v := range req.GetParams() {
			if v.GetType() == nil {
				return status.Errorf(codes.InvalidArgument, "parameter %q needs a type when query is used instead of prepared_query", name)
			}
			paramTypes[name] = v.GetType()
		}
		var err error
		p, err = gsql.Prepare(req.GetQuery(), paramTypes, cat)
		if err != nil {
			return err
		}
		sendMetadata = len(req.GetResumeToken()) == 0
	}
	if sendMetadata {
		if err := stream.Send(&btpb.ExecuteQueryResponse{Response: &btpb.ExecuteQueryResponse_Metadata{Metadata: p.Metadata()}}); err != nil {
			return err
		}
	}

	// A resume token is the number of rows already delivered.
	skip := uint64(0)
	if rt := req.GetResumeToken(); len(rt) > 0 {
		if len(rt) != 8 {
			return status.Error(codes.InvalidArgument, "invalid resume_token")
		}
		skip = binary.BigEndian.Uint64(rt)
	}
	var delivered uint64
	batch := &btpb.ProtoRows{}
	batchBytes := 0
	flush := func() error {
		if len(batch.Values) == 0 {
			return nil
		}
		data, err := proto.Marshal(batch)
		if err != nil {
			return status.Errorf(codes.Internal, "encode results: %v", err)
		}
		sum := crc32.Checksum(data, crc32cTable)
		token := make([]byte, 8)
		binary.BigEndian.PutUint64(token, delivered)
		batch = &btpb.ProtoRows{}
		batchBytes = 0
		return stream.Send(&btpb.ExecuteQueryResponse{Response: &btpb.ExecuteQueryResponse_Results{Results: &btpb.PartialResultSet{
			PartialRows:        &btpb.PartialResultSet_ProtoRowsBatch{ProtoRowsBatch: &btpb.ProtoRowsBatch{BatchData: data}},
			BatchChecksum:      &sum,
			ResumeToken:        token,
			EstimatedBatchSize: int32(len(data)),
		}}})
	}
	var rowIndex uint64
	err := p.Execute(ctx, cat, req.GetParams(), func(values []*btpb.Value) error {
		rowIndex++
		if rowIndex <= skip {
			return nil
		}
		batch.Values = append(batch.Values, values...)
		for _, v := range values {
			batchBytes += proto.Size(v)
		}
		delivered = rowIndex
		if batchBytes >= queryBatchBytes {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

// HLL++ sketches for HLL aggregate families share the query engine's format.
type hllSketch struct{ h *gsql.HLL }

func newHLLSketch() *hllSketch { return &hllSketch{h: gsql.NewHLL()} }

func decodeHLLSketch(b []byte) (*hllSketch, error) {
	h, err := gsql.DecodeHLL(b)
	if err != nil {
		return nil, err
	}
	return &hllSketch{h: h}, nil
}

func (x *hllSketch) add(b []byte)       { x.h.Add(b) }
func (x *hllSketch) merge(o *hllSketch) { x.h.Merge(o.h) }
func (x *hllSketch) encode() []byte     { return x.h.Encode() }
func (x *hllSketch) estimate() int64    { return x.h.Estimate() }

// hllInputBytes returns the item an AddToCell adds to an HLL++ family,
// hashed exactly as HLL_COUNT.INIT hashes the same SQL value.
func hllInputBytes(v *btpb.Value) ([]byte, bool) {
	return gsql.HLLInputBytes(v)
}

// IsMaterializedView lets the engine plan materialized views as views with
// materialized-view semantics.
func (c *sqlCatalog) IsMaterializedView(name string) bool {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	_, ok := c.s.mvs[c.instance+"/materializedViews/"+name]
	return ok
}

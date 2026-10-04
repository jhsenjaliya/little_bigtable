package bttest

// Conformance tests for the write path: per-request atomicity, MutateRows
// per-entry status, storage error mapping, aggregate column families and
// idempotency tokens. Official references: google/bigtable/v2/bigtable.proto
// (MutateRow, MutateRows, CheckAndMutateRow, ReadModifyWriteRow), data.proto
// (Mutation, Idempotency), admin types.proto (Aggregate) and
// https://cloud.google.com/bigtable/docs/writes.

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// cwEnv is a running emulator with official Go clients and access to the
// server state, so tests can inspect stored rows and change records.
type cwEnv struct {
	ctx    context.Context
	s      *server
	admin  *bigtable.AdminClient
	client *bigtable.Client
	parent string
}

func cwSetup(t *testing.T) *cwEnv {
	t.Helper()
	const project, instance = "cw-project", "cw-instance"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	prevDialect, prevStrict := currentDialect(), isStrictAdmin()
	ConfigureStorage("sqlite3", true)
	t.Cleanup(func() { ConfigureStorage(string(prevDialect), prevStrict) })

	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared", newDBFile(t)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	require.NoError(t, CreateTables(ctx, db))

	srv, err := NewServer("127.0.0.1:0", db)
	require.NoError(t, err)
	t.Cleanup(srv.Close)
	t.Setenv("BIGTABLE_EMULATOR_HOST", srv.Addr)

	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	op, err := btapb.NewBigtableInstanceAdminClient(conn).CreateInstance(ctx, &btapb.CreateInstanceRequest{
		Parent:     "projects/" + project,
		InstanceId: instance,
		Instance:   &btapb.Instance{DisplayName: "Write conformance", Type: btapb.Instance_PRODUCTION},
		Clusters:   map[string]*btapb.Cluster{"cw-cluster": {}},
	})
	require.NoError(t, err)
	require.True(t, op.GetDone())

	admin, err := bigtable.NewAdminClient(ctx, project, instance, option.WithGRPCConn(conn))
	require.NoError(t, err)
	client, err := bigtable.NewClient(ctx, project, instance, option.WithGRPCConn(conn))
	require.NoError(t, err)
	return &cwEnv{ctx: ctx, s: srv.s, admin: admin, client: client,
		parent: "projects/" + project + "/instances/" + instance}
}

func (e *cwEnv) tableName(id string) string { return e.parent + "/tables/" + id }

// createTable creates a table whose change stream is enabled, so every
// committed write is observable as a change record.
func (e *cwEnv) createTable(t *testing.T, id string, fams map[string]bigtable.Family) *bigtable.Table {
	t.Helper()
	require.NoError(t, e.admin.CreateTableFromConf(e.ctx, &bigtable.TableConf{
		TableID:               id,
		ColumnFamilies:        fams,
		ChangeStreamRetention: 24 * time.Hour,
	}))
	return e.client.Open(id)
}

func (e *cwEnv) table(t *testing.T, id string) *table {
	t.Helper()
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	tbl := e.s.tables[e.tableName(id)]
	require.NotNil(t, tbl, "table %s", id)
	return tbl
}

// cwChangeRecords returns every change record of tableName in commit order.
func cwChangeRecords(t *testing.T, s *server, tableName string) []changeRecord {
	t.Helper()
	recs, err := s.changeLog.listAfter(context.Background(), tableName, 0, 100000)
	require.NoError(t, err)
	return recs
}

func cwInt64(v []byte) int64 {
	if len(v) != 8 {
		return -1 << 63
	}
	return int64(binary.BigEndian.Uint64(v))
}

// cwRowValues flattens a client row into "fam:col@ts" -> value.
func cwRowValues(r bigtable.Row) map[string]string {
	out := map[string]string{}
	for _, items := range r {
		for _, it := range items {
			out[fmt.Sprintf("%s@%d", it.Column, it.Timestamp)] = string(it.Value)
		}
	}
	return out
}

func cwAggFamily(agg bigtable.Aggregator) bigtable.Family {
	return bigtable.Family{ValueType: bigtable.AggregateType{Input: bigtable.Int64Type{}, Aggregator: agg}}
}

// cwMutateRowsStream captures MutateRows responses with a caller context.
type cwMutateRowsStream struct {
	grpc.ServerStream
	ctx       context.Context
	responses []*btpb.MutateRowsResponse
}

func (s *cwMutateRowsStream) Context() context.Context { return s.ctx }
func (s *cwMutateRowsStream) Send(r *btpb.MutateRowsResponse) error {
	s.responses = append(s.responses, r)
	return nil
}

func cwSetCell(fam, col string, ts int64, value string) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
		FamilyName: fam, ColumnQualifier: []byte(col), TimestampMicros: ts, Value: []byte(value)}}}
}

func cwAddToCell(fam, col string, ts, v int64) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_AddToCell_{AddToCell: &btpb.Mutation_AddToCell{
		FamilyName:      fam,
		ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte(col)}},
		Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: ts}},
		Input:           &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: v}},
	}}}
}

// --- Atomicity ---------------------------------------------------------------

// A MutateRow whose later mutation is invalid must not apply its earlier
// valid mutations and must not emit a change record.
func TestConformanceWriteMutateRowAtomicity(t *testing.T) {
	e := cwSetup(t)
	tbl := e.createTable(t, "atomic", map[string]bigtable.Family{"cf": {}})

	seed := bigtable.NewMutation()
	seed.Set("cf", "a", bigtable.Timestamp(1000), []byte("old"))
	require.NoError(t, tbl.Apply(e.ctx, "r", seed))
	before := len(cwChangeRecords(t, e.s, e.tableName("atomic")))
	require.Equal(t, 1, before)

	for _, tc := range []struct {
		name    string
		invalid func(m *bigtable.Mutation)
		code    codes.Code
	}{
		{"unknown_family", func(m *bigtable.Mutation) { m.Set("nope", "x", bigtable.Timestamp(1000), []byte("v")) }, codes.NotFound},
		{"negative_timestamp", func(m *bigtable.Mutation) { m.Set("cf", "b", bigtable.Timestamp(-2000), []byte("v")) }, codes.InvalidArgument},
		{"unknown_family_delete", func(m *bigtable.Mutation) { m.DeleteCellsInFamily("nope") }, codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := bigtable.NewMutation()
			m.Set("cf", "a", bigtable.Timestamp(1000), []byte("new"))
			m.DeleteCellsInColumn("cf", "a")
			m.Set("cf", "c", bigtable.Timestamp(2000), []byte("c"))
			tc.invalid(m)
			err := tbl.Apply(e.ctx, "r", m)
			require.Equal(t, tc.code, status.Code(err), "err: %v", err)

			r, err := tbl.ReadRow(e.ctx, "r")
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"cf:a@1000": "old"}, cwRowValues(r))
			assert.Len(t, cwChangeRecords(t, e.s, e.tableName("atomic")), before, "a failed MutateRow must not write a change record")
		})
	}

	// The Go client truncates timestamps to milliseconds, so a timestamp that
	// does not match the table granularity is sent through the raw handler.
	_, err := e.s.MutateRow(e.ctx, &btpb.MutateRowRequest{TableName: e.tableName("atomic"), RowKey: []byte("r"),
		Mutations: []*btpb.Mutation{cwSetCell("cf", "a", 1000, "new"), cwSetCell("cf", "b", 1500, "v")}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "sub-millisecond timestamp: %v", err)
	assert.Equal(t, "old", string(storedRow(t, e.table(t, "atomic"), "r").families["cf"].Cells["a"][0].Value))
	assert.Len(t, cwChangeRecords(t, e.s, e.tableName("atomic")), before)

	// Unknown family error text matches production.
	m := bigtable.NewMutation()
	m.Set("nope", "x", bigtable.Timestamp(1000), []byte("v"))
	err = tbl.Apply(e.ctx, "r", m)
	assert.Contains(t, status.Convert(err).Message(), "Requested column family not found")
}

func TestConformanceWriteCheckAndMutateRowAtomicity(t *testing.T) {
	e := cwSetup(t)
	tbl := e.createTable(t, "cam", map[string]bigtable.Family{"cf": {}})
	seed := bigtable.NewMutation()
	seed.Set("cf", "a", bigtable.Timestamp(1000), []byte("old"))
	require.NoError(t, tbl.Apply(e.ctx, "r", seed))
	before := len(cwChangeRecords(t, e.s, e.tableName("cam")))

	for _, tc := range []struct {
		name    string
		invalid func(m *bigtable.Mutation)
		code    codes.Code
	}{
		{"unknown_family", func(m *bigtable.Mutation) { m.Set("nope", "x", bigtable.Timestamp(1000), []byte("v")) }, codes.NotFound},
		{"negative_timestamp", func(m *bigtable.Mutation) { m.Set("cf", "b", bigtable.Timestamp(-3000), []byte("v")) }, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The predicate matches, so the true branch is chosen; its second
			// mutation is invalid.
			mTrue := bigtable.NewMutation()
			mTrue.Set("cf", "a", bigtable.Timestamp(1000), []byte("new"))
			tc.invalid(mTrue)
			cond := bigtable.NewCondMutation(bigtable.ColumnFilter("a"), mTrue, nil)
			var matched bool
			err := tbl.Apply(e.ctx, "r", cond, bigtable.GetCondMutationResult(&matched))
			require.Equal(t, tc.code, status.Code(err), "err: %v", err)

			r, err := tbl.ReadRow(e.ctx, "r")
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"cf:a@1000": "old"}, cwRowValues(r))
			assert.Len(t, cwChangeRecords(t, e.s, e.tableName("cam")), before)
		})
	}

	// Sub-millisecond timestamp in the chosen branch (raw handler; the Go
	// client truncates timestamps).
	_, err := e.s.CheckAndMutateRow(e.ctx, &btpb.CheckAndMutateRowRequest{TableName: e.tableName("cam"), RowKey: []byte("r"),
		TrueMutations: []*btpb.Mutation{cwSetCell("cf", "a", 1000, "new"), cwSetCell("cf", "b", 1001, "v")}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)

	// The false branch is chosen when the predicate does not match; an
	// invalid false branch fails without writing either branch.
	mTrue := bigtable.NewMutation()
	mTrue.Set("cf", "t", bigtable.Timestamp(1000), []byte("t"))
	mFalse := bigtable.NewMutation()
	mFalse.Set("cf", "f", bigtable.Timestamp(1000), []byte("f"))
	mFalse.Set("nope", "f", bigtable.Timestamp(1000), []byte("f"))
	err = tbl.Apply(e.ctx, "r", bigtable.NewCondMutation(bigtable.ColumnFilter("zzz"), mTrue, mFalse))
	require.Equal(t, codes.NotFound, status.Code(err), "err: %v", err)
	r, err := tbl.ReadRow(e.ctx, "r")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"cf:a@1000": "old"}, cwRowValues(r))
	assert.Len(t, cwChangeRecords(t, e.s, e.tableName("cam")), before)

	// A successful conditional mutation writes exactly one change record.
	var matched bool
	mFalse = bigtable.NewMutation()
	mFalse.Set("cf", "f", bigtable.Timestamp(1000), []byte("f"))
	require.NoError(t, tbl.Apply(e.ctx, "r", bigtable.NewCondMutation(bigtable.ColumnFilter("zzz"), mTrue, mFalse), bigtable.GetCondMutationResult(&matched)))
	assert.False(t, matched)
	r, err = tbl.ReadRow(e.ctx, "r")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"cf:a@1000": "old", "cf:f@1000": "f"}, cwRowValues(r))
	assert.Len(t, cwChangeRecords(t, e.s, e.tableName("cam")), before+1)
}

func TestConformanceWriteReadModifyWriteRowAtomicity(t *testing.T) {
	e := cwSetup(t)
	tbl := e.createTable(t, "rmw", map[string]bigtable.Family{"cf": {}, "sum": cwAggFamily(bigtable.SumAggregator{})})
	seed := bigtable.NewMutation()
	seed.Set("cf", "s", bigtable.Timestamp(1000), []byte("abc"))
	seed.Set("cf", "n", bigtable.Timestamp(1000), encodeInt64(5))
	require.NoError(t, tbl.Apply(e.ctx, "r", seed))
	before := len(cwChangeRecords(t, e.s, e.tableName("rmw")))
	want := map[string]string{"cf:s@1000": "abc", "cf:n@1000": string(encodeInt64(5))}

	for _, tc := range []struct {
		name    string
		invalid func(m *bigtable.ReadModifyWrite)
		code    codes.Code
	}{
		{"unknown_family", func(m *bigtable.ReadModifyWrite) { m.AppendValue("nope", "x", []byte("v")) }, codes.NotFound},
		// Incrementing a cell that is not a 64-bit big-endian integer.
		{"increment_non_int64", func(m *bigtable.ReadModifyWrite) { m.Increment("cf", "s", 1) }, codes.FailedPrecondition},
		{"aggregate_family", func(m *bigtable.ReadModifyWrite) { m.Increment("sum", "x", 1) }, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := bigtable.NewReadModifyWrite()
			m.AppendValue("cf", "s", []byte("def"))
			m.Increment("cf", "n", 10)
			tc.invalid(m)
			_, err := tbl.ApplyReadModifyWrite(e.ctx, "r", m)
			require.Equal(t, tc.code, status.Code(err), "err: %v", err)

			r, err := tbl.ReadRow(e.ctx, "r")
			require.NoError(t, err)
			assert.Equal(t, want, cwRowValues(r))
			assert.Len(t, cwChangeRecords(t, e.s, e.tableName("rmw")), before)
		})
	}

	// Rules apply in order; a later rule observes an earlier one.
	m := bigtable.NewReadModifyWrite()
	m.Increment("cf", "n", 10)
	m.Increment("cf", "n", 100)
	m.AppendValue("cf", "s", []byte("d"))
	got, err := tbl.ApplyReadModifyWrite(e.ctx, "r", m)
	require.NoError(t, err)
	vals := map[string][]byte{}
	for _, it := range got["cf"] {
		vals[it.Column] = it.Value
	}
	assert.Equal(t, int64(115), cwInt64(vals["cf:n"]))
	assert.Equal(t, "abcd", string(vals["cf:s"]))
	assert.Len(t, cwChangeRecords(t, e.s, e.tableName("rmw")), before+1)
}

// MutateRows reports a status per entry and applies only the valid entries.
func TestConformanceWriteMutateRowsPerEntryStatus(t *testing.T) {
	e := cwSetup(t)
	tbl := e.createTable(t, "bulk", map[string]bigtable.Family{"cf": {}, "sum": cwAggFamily(bigtable.SumAggregator{})})

	ok1 := bigtable.NewMutation()
	ok1.Set("cf", "a", bigtable.Timestamp(1000), []byte("1"))
	unknown := bigtable.NewMutation()
	unknown.Set("cf", "a", bigtable.Timestamp(1000), []byte("2"))
	unknown.Set("nope", "a", bigtable.Timestamp(1000), []byte("2"))
	badTs := bigtable.NewMutation()
	badTs.Set("cf", "a", bigtable.Timestamp(1000), []byte("3"))
	badTs.Set("cf", "b", bigtable.Timestamp(-5000), []byte("3"))
	setOnAgg := bigtable.NewMutation()
	setOnAgg.Set("sum", "a", bigtable.Timestamp(1000), []byte("4"))
	ok2 := bigtable.NewMutation()
	ok2.Set("cf", "a", bigtable.Timestamp(1000), []byte("5"))

	errs, err := tbl.ApplyBulk(e.ctx, []string{"r1", "r2", "r3", "r4", "r5"},
		[]*bigtable.Mutation{ok1, unknown, badTs, setOnAgg, ok2})
	require.NoError(t, err)
	require.Len(t, errs, 5)
	assert.NoError(t, errs[0])
	assert.Equal(t, codes.NotFound, status.Code(errs[1]), "%v", errs[1])
	assert.Equal(t, codes.InvalidArgument, status.Code(errs[2]), "%v", errs[2])
	assert.Equal(t, codes.InvalidArgument, status.Code(errs[3]), "%v", errs[3])
	assert.NoError(t, errs[4])

	var keys []string
	require.NoError(t, tbl.ReadRows(e.ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		keys = append(keys, r.Key())
		return true
	}))
	assert.Equal(t, []string{"r1", "r5"}, keys)
	recs := cwChangeRecords(t, e.s, e.tableName("bulk"))
	require.Len(t, recs, 2)
	assert.Equal(t, "r1", string(recs[0].rowKey))
	assert.Equal(t, "r5", string(recs[1].rowKey))

	// Raw handler: each entry carries its index and a real status code.
	stream := &cwMutateRowsStream{ctx: context.Background()}
	require.NoError(t, e.s.MutateRows(&btpb.MutateRowsRequest{
		TableName: e.tableName("bulk"),
		Entries: []*btpb.MutateRowsRequest_Entry{
			{RowKey: []byte("x1"), Mutations: []*btpb.Mutation{cwSetCell("cf", "a", 1000, "v")}},
			{RowKey: []byte("x2"), Mutations: []*btpb.Mutation{cwSetCell("cf", "a", 1000, "v"), cwSetCell("nope", "a", 1000, "v")}},
			{RowKey: nil, Mutations: []*btpb.Mutation{cwSetCell("cf", "a", 1000, "v")}},
			{RowKey: []byte("x4"), Mutations: []*btpb.Mutation{cwSetCell("cf", "a", 1000, "v"), cwSetCell("cf", "a", 1500, "v")}},
		},
	}, stream))
	require.Len(t, stream.responses, 1)
	entries := stream.responses[0].Entries
	require.Len(t, entries, 4)
	for i, want := range []codes.Code{codes.OK, codes.NotFound, codes.InvalidArgument, codes.InvalidArgument} {
		assert.Equal(t, int64(i), entries[i].Index)
		assert.Equal(t, int32(want), entries[i].GetStatus().GetCode(), "entry %d: %v", i, entries[i].GetStatus())
	}
	tb := e.table(t, "bulk")
	assert.NotNil(t, storedRow(t, tb, "x1"))
	assert.Nil(t, storedRow(t, tb, "x2"))
	assert.Nil(t, storedRow(t, tb, "x4"))

	// A request with no mutations at all is rejected outright.
	err = e.s.MutateRows(&btpb.MutateRowsRequest{TableName: e.tableName("bulk"),
		Entries: []*btpb.MutateRowsRequest_Entry{{RowKey: []byte("x")}}}, &cwMutateRowsStream{ctx: context.Background()})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

// Entries for the same row key inside one MutateRows batch apply in request
// order, each on top of the previous one.
func TestConformanceWriteMutateRowsDuplicateKeysInOrder(t *testing.T) {
	e := cwSetup(t)
	tbl := e.createTable(t, "dups", map[string]bigtable.Family{"cf": {}, "sum": cwAggFamily(bigtable.SumAggregator{})})

	m0 := bigtable.NewMutation()
	m0.Set("cf", "a", bigtable.Timestamp(1000), []byte("v1"))
	m0.Set("cf", "b", bigtable.Timestamp(1000), []byte("b"))
	m0.AddIntToCell("sum", "n", bigtable.Timestamp(0), 1)
	m1 := bigtable.NewMutation()
	m1.Set("cf", "a", bigtable.Timestamp(1000), []byte("v2")) // replaces v1 at the same timestamp
	m1.AddIntToCell("sum", "n", bigtable.Timestamp(0), 2)
	bad := bigtable.NewMutation()
	bad.Set("cf", "a", bigtable.Timestamp(1000), []byte("never"))
	bad.Set("nope", "a", bigtable.Timestamp(1000), []byte("never"))
	m2 := bigtable.NewMutation()
	m2.DeleteCellsInColumn("cf", "b")
	m2.AddIntToCell("sum", "n", bigtable.Timestamp(0), 4)
	m3 := bigtable.NewMutation()
	m3.Set("cf", "c", bigtable.Timestamp(2000), []byte("c"))

	errs, err := tbl.ApplyBulk(e.ctx, []string{"k", "k", "k", "other", "k", "k"},
		[]*bigtable.Mutation{m0, m1, bad, m3, m2, m3})
	require.NoError(t, err)
	require.Len(t, errs, 6)
	for i, err := range errs {
		if i == 2 {
			assert.Equal(t, codes.NotFound, status.Code(err))
			continue
		}
		assert.NoError(t, err, "entry %d", i)
	}

	r, err := tbl.ReadRow(e.ctx, "k")
	require.NoError(t, err)
	got := cwRowValues(r)
	assert.Equal(t, "v2", got["cf:a@1000"])
	assert.Equal(t, "c", got["cf:c@2000"])
	_, hasB := got["cf:b@1000"]
	assert.False(t, hasB, "entry 4 deleted cf:b written by entry 0")
	assert.Equal(t, int64(7), cwInt64([]byte(got["sum:n@0"])))
	assert.Len(t, got, 3)

	// The change records for "k" replay the successful entries in order.
	var setValues []string
	for _, rec := range cwChangeRecords(t, e.s, e.tableName("dups")) {
		if string(rec.rowKey) != "k" {
			continue
		}
		require.Equal(t, btpb.ReadChangeStreamResponse_DataChange_USER, rec.changeType)
		for _, m := range rec.mutations {
			switch mm := m.Mutation.(type) {
			case *btpb.Mutation_SetCell_:
				setValues = append(setValues, string(mm.SetCell.Value))
			case *btpb.Mutation_DeleteFromColumn_:
				setValues = append(setValues, "delete:"+string(mm.DeleteFromColumn.ColumnQualifier))
			}
		}
	}
	assert.Equal(t, []string{"v1", "b", "v2", "delete:b", "c"}, setValues)
}

// Cancelled and expired request contexts surface as Canceled and
// DeadlineExceeded, never Internal, and nothing is written.
func TestConformanceWriteStorageErrorMapping(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()
	name := parent + "/tables/t"
	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "t", Table: &btapb.Table{
		ColumnFamilies: map[string]*btapb.ColumnFamily{"cf": {}},
	}})
	require.NoError(t, err)
	_, err = s.MutateRow(ctx, &btpb.MutateRowRequest{TableName: name, RowKey: []byte("r"),
		Mutations: []*btpb.Mutation{cwSetCell("cf", "a", 1000, "old")}})
	require.NoError(t, err)
	tbl := s.tables[name]

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	expired, cancel2 := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel2()

	for _, tc := range []struct {
		ctx  context.Context
		code codes.Code
	}{{cancelled, codes.Canceled}, {expired, codes.DeadlineExceeded}} {
		t.Run(tc.code.String(), func(t *testing.T) {
			_, err := s.MutateRow(tc.ctx, &btpb.MutateRowRequest{TableName: name, RowKey: []byte("r"),
				Mutations: []*btpb.Mutation{cwSetCell("cf", "a", 1000, "new")}})
			assert.Equal(t, tc.code, status.Code(err), "MutateRow: %v", err)

			_, err = s.CheckAndMutateRow(tc.ctx, &btpb.CheckAndMutateRowRequest{TableName: name, RowKey: []byte("r"),
				TrueMutations: []*btpb.Mutation{cwSetCell("cf", "a", 1000, "new")}})
			assert.Equal(t, tc.code, status.Code(err), "CheckAndMutateRow: %v", err)

			_, err = s.ReadModifyWriteRow(tc.ctx, &btpb.ReadModifyWriteRowRequest{TableName: name, RowKey: []byte("r"),
				Rules: []*btpb.ReadModifyWriteRule{{FamilyName: "cf", ColumnQualifier: []byte("a"),
					Rule: &btpb.ReadModifyWriteRule_AppendValue{AppendValue: []byte("x")}}}})
			assert.Equal(t, tc.code, status.Code(err), "ReadModifyWriteRow: %v", err)

			stream := &cwMutateRowsStream{ctx: tc.ctx}
			require.NoError(t, s.MutateRows(&btpb.MutateRowsRequest{TableName: name, Entries: []*btpb.MutateRowsRequest_Entry{
				{RowKey: []byte("r"), Mutations: []*btpb.Mutation{cwSetCell("cf", "a", 1000, "new")}},
			}}, stream))
			require.Len(t, stream.responses, 1)
			assert.Equal(t, int32(tc.code), stream.responses[0].Entries[0].GetStatus().GetCode())

			r := storedRow(t, tbl, "r")
			require.NotNil(t, r)
			assert.Equal(t, "old", string(r.families["cf"].Cells["a"][0].Value))
		})
	}
}

// --- Aggregates --------------------------------------------------------------

func TestConformanceWriteAggregateSemantics(t *testing.T) {
	e := cwSetup(t)
	tbl := e.createTable(t, "agg", map[string]bigtable.Family{
		"sum": cwAggFamily(bigtable.SumAggregator{}),
		"min": cwAggFamily(bigtable.MinAggregator{}),
		"max": cwAggFamily(bigtable.MaxAggregator{}),
	})
	add := func(ts bigtable.Timestamp, v int64) {
		t.Helper()
		m := bigtable.NewMutation()
		for _, fam := range []string{"sum", "min", "max"} {
			m.AddIntToCell(fam, "c", ts, v)
		}
		require.NoError(t, tbl.Apply(e.ctx, "r", m))
	}
	// Same timestamp: values combine into one cell.
	add(1000, 3)
	add(1000, 5)
	add(1000, -2)
	// Same timestamp twice inside one request.
	m := bigtable.NewMutation()
	m.AddIntToCell("sum", "c", 1000, 10)
	m.AddIntToCell("sum", "c", 1000, 20)
	require.NoError(t, tbl.Apply(e.ctx, "r", m))
	// A different timestamp starts a separate cell.
	add(2000, 7)

	r, err := tbl.ReadRow(e.ctx, "r")
	require.NoError(t, err)
	got := map[string]int64{}
	for _, items := range r {
		for _, it := range items {
			require.Len(t, it.Value, 8, "aggregate cells are 64-bit big-endian")
			got[fmt.Sprintf("%s@%d", it.Column, it.Timestamp)] = cwInt64(it.Value)
		}
	}
	assert.Equal(t, map[string]int64{
		"sum:c@1000": 3 + 5 - 2 + 10 + 20, "sum:c@2000": 7,
		"min:c@1000": -2, "min:c@2000": 7,
		"max:c@1000": 5, "max:c@2000": 7,
	}, got)

	// Latest-cell read sees the newest timestamp only.
	r, err = tbl.ReadRow(e.ctx, "r", bigtable.RowFilter(bigtable.LatestNFilter(1)))
	require.NoError(t, err)
	for _, items := range r {
		require.Len(t, items, 1)
		assert.Equal(t, int64(7), cwInt64(items[0].Value))
	}
}

func TestConformanceWriteAggregateFamilyTypeChecks(t *testing.T) {
	e := cwSetup(t)
	tbl := e.createTable(t, "aggchk", map[string]bigtable.Family{"cf": {}, "sum": cwAggFamily(bigtable.SumAggregator{})})
	seed := bigtable.NewMutation()
	seed.AddIntToCell("sum", "c", 0, 1)
	seed.Set("cf", "a", bigtable.Timestamp(1000), []byte("x"))
	require.NoError(t, tbl.Apply(e.ctx, "r", seed))
	before := len(cwChangeRecords(t, e.s, e.tableName("aggchk")))

	t.Run("AddToCell_on_non_aggregate_family", func(t *testing.T) {
		m := bigtable.NewMutation()
		m.AddIntToCell("sum", "c", 0, 100)
		m.AddIntToCell("cf", "a", 0, 1)
		err := tbl.Apply(e.ctx, "r", m)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	})
	t.Run("SetCell_on_aggregate_family", func(t *testing.T) {
		m := bigtable.NewMutation()
		m.AddIntToCell("sum", "c", 0, 100)
		m.Set("sum", "c", bigtable.Timestamp(0), encodeInt64(9))
		err := tbl.Apply(e.ctx, "r", m)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	})
	t.Run("AddToCell_unknown_family", func(t *testing.T) {
		m := bigtable.NewMutation()
		m.AddIntToCell("nope", "c", 0, 1)
		assert.Equal(t, codes.NotFound, status.Code(tbl.Apply(e.ctx, "r", m)))
	})
	t.Run("AddToCell_bad_timestamp", func(t *testing.T) {
		m := bigtable.NewMutation()
		m.AddIntToCell("sum", "c", -2000, 1)
		assert.Equal(t, codes.InvalidArgument, status.Code(tbl.Apply(e.ctx, "r", m)))
		_, err := e.s.MutateRow(e.ctx, &btpb.MutateRowRequest{TableName: e.tableName("aggchk"), RowKey: []byte("r"),
			Mutations: []*btpb.Mutation{cwAddToCell("sum", "c", 1001, 1)}})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "sub-millisecond aggregate timestamp: %v", err)
	})
	t.Run("AddToCell_non_int64_input", func(t *testing.T) {
		_, err := e.s.MutateRow(e.ctx, &btpb.MutateRowRequest{TableName: e.tableName("aggchk"), RowKey: []byte("r"),
			Mutations: []*btpb.Mutation{{Mutation: &btpb.Mutation_AddToCell_{AddToCell: &btpb.Mutation_AddToCell{
				FamilyName:      "sum",
				ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte("c")}},
				Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: 0}},
				Input:           &btpb.Value{Kind: &btpb.Value_StringValue{StringValue: "1"}},
			}}}}})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	})

	r, err := tbl.ReadRow(e.ctx, "r")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"cf:a@1000": "x", "sum:c@0": string(encodeInt64(1))}, cwRowValues(r))
	assert.Len(t, cwChangeRecords(t, e.s, e.tableName("aggchk")), before)
}

// Idempotency tokens deduplicate retried aggregate writes inside the 15
// minute protection window (MutateRowRequest.idempotency and
// MutateRowsRequest.Entry.idempotency).
func TestConformanceWriteIdempotency(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()
	name := parent + "/tables/idem"
	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "idem", Table: &btapb.Table{
		ColumnFamilies:     map[string]*btapb.ColumnFamily{"sum": sumFamily()},
		ChangeStreamConfig: &btapb.ChangeStreamConfig{RetentionPeriod: durationpb.New(24 * time.Hour)},
	}})
	require.NoError(t, err)
	tbl := s.tables[name]
	sumOf := func(key string) int64 {
		t.Helper()
		r := storedRow(t, tbl, key)
		require.NotNil(t, r)
		return cwInt64(r.families["sum"].Cells["c"][0].Value)
	}
	mutate := func(key string, idem *btpb.Idempotency, v int64) error {
		_, err := s.MutateRow(ctx, &btpb.MutateRowRequest{TableName: name, RowKey: []byte(key),
			Mutations: []*btpb.Mutation{cwAddToCell("sum", "c", 0, v)}, Idempotency: idem})
		return err
	}
	tok := func(tok string) *btpb.Idempotency { return &btpb.Idempotency{Token: []byte(tok)} }

	require.NoError(t, mutate("r", tok("token-0001"), 5))
	require.NoError(t, mutate("r", tok("token-0001"), 5), "a replay succeeds")
	assert.Equal(t, int64(5), sumOf("r"), "a replay within the window is not applied twice")
	recs := len(cwChangeRecords(t, s, name))
	assert.Equal(t, 1, recs, "a deduplicated replay writes no change record")

	require.NoError(t, mutate("r", tok("token-0002"), 5))
	assert.Equal(t, int64(10), sumOf("r"), "a new token applies")
	require.NoError(t, mutate("other", tok("token-0001"), 3))
	assert.Equal(t, int64(3), sumOf("other"), "tokens are scoped to the row")
	require.NoError(t, mutate("r", nil, 1))
	require.NoError(t, mutate("r", nil, 1))
	assert.Equal(t, int64(12), sumOf("r"), "without a token every attempt applies")

	// MutateRows entries are deduplicated the same way, also within a batch.
	entry := func(key, token string, v int64) *btpb.MutateRowsRequest_Entry {
		return &btpb.MutateRowsRequest_Entry{RowKey: []byte(key), Mutations: []*btpb.Mutation{cwAddToCell("sum", "c", 0, v)},
			Idempotency: tok(token)}
	}
	stream := &cwMutateRowsStream{ctx: ctx}
	require.NoError(t, s.MutateRows(&btpb.MutateRowsRequest{TableName: name, Entries: []*btpb.MutateRowsRequest_Entry{
		entry("r", "token-0001", 100), entry("b", "token-0003", 2), entry("b", "token-0004", 4),
		{RowKey: []byte("b"), Mutations: []*btpb.Mutation{cwAddToCell("sum", "c", 0, 8)}, Idempotency: tok("short")},
	}}, stream))
	require.Len(t, stream.responses, 1)
	for i, want := range []codes.Code{codes.OK, codes.OK, codes.OK, codes.InvalidArgument} {
		assert.Equal(t, int32(want), stream.responses[0].Entries[i].GetStatus().GetCode(), "entry %d", i)
	}
	assert.Equal(t, int64(12), sumOf("r"), "batch replay of token-0001 deduplicated")
	assert.Equal(t, int64(6), sumOf("b"))
	stream = &cwMutateRowsStream{ctx: ctx}
	require.NoError(t, s.MutateRows(&btpb.MutateRowsRequest{TableName: name, Entries: []*btpb.MutateRowsRequest_Entry{
		entry("b", "token-0003", 2),
	}}, stream))
	assert.Equal(t, int64(6), sumOf("b"), "MutateRows replay deduplicated")

	// Tokens shorter than 8 bytes are rejected.
	err = mutate("r", tok("1234567"), 1)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)

	// start_time: zero or recent is accepted; older than the window is
	// rejected because protection may have expired.
	require.NoError(t, mutate("r", &btpb.Idempotency{Token: []byte("token-0005"), StartTime: timestamppb.New(time.Now().Add(-time.Minute))}, 1))
	require.NoError(t, mutate("r", &btpb.Idempotency{Token: []byte("token-0006"), StartTime: &timestamppb.Timestamp{}}, 1))
	err = mutate("r", &btpb.Idempotency{Token: []byte("token-0007"), StartTime: timestamppb.New(time.Now().Add(-time.Hour))}, 1)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
	assert.Equal(t, int64(14), sumOf("r"))

	// Once the protection window has passed, the same token applies again.
	_, err = s.db.Exec(bind("UPDATE idempotency_t SET expire_micros = ? WHERE token = ?"), time.Now().Add(-time.Second).UnixMicro(), []byte("token-0001"))
	require.NoError(t, err)
	require.NoError(t, mutate("r", tok("token-0001"), 5))
	assert.Equal(t, int64(19), sumOf("r"))
}

// Malformed write requests are rejected with InvalidArgument before anything
// is applied.
func TestConformanceWriteRequestValidation(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()
	name := parent + "/tables/v"
	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "v", Table: &btapb.Table{
		ColumnFamilies: map[string]*btapb.ColumnFamily{"cf": {}},
	}})
	require.NoError(t, err)
	set := []*btpb.Mutation{cwSetCell("cf", "a", 1000, "v")}
	bigKey := make([]byte, maxRowKeyBytes+1)
	appendRule := []*btpb.ReadModifyWriteRule{{FamilyName: "cf", ColumnQualifier: []byte("a"),
		Rule: &btpb.ReadModifyWriteRule_AppendValue{AppendValue: []byte("x")}}}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"MutateRow_no_mutations", func() error {
			_, err := s.MutateRow(ctx, &btpb.MutateRowRequest{TableName: name, RowKey: []byte("r")})
			return err
		}},
		{"MutateRow_empty_row_key", func() error {
			_, err := s.MutateRow(ctx, &btpb.MutateRowRequest{TableName: name, Mutations: set})
			return err
		}},
		{"MutateRow_row_key_over_4KiB", func() error {
			_, err := s.MutateRow(ctx, &btpb.MutateRowRequest{TableName: name, RowKey: bigKey, Mutations: set})
			return err
		}},
		{"MutateRow_empty_mutation", func() error {
			_, err := s.MutateRow(ctx, &btpb.MutateRowRequest{TableName: name, RowKey: []byte("r"),
				Mutations: append(append([]*btpb.Mutation(nil), set...), &btpb.Mutation{})})
			return err
		}},
		{"MutateRow_inverted_delete_range", func() error {
			_, err := s.MutateRow(ctx, &btpb.MutateRowRequest{TableName: name, RowKey: []byte("r"),
				Mutations: append(append([]*btpb.Mutation(nil), set...), &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromColumn_{
					DeleteFromColumn: &btpb.Mutation_DeleteFromColumn{FamilyName: "cf", ColumnQualifier: []byte("a"),
						TimeRange: &btpb.TimestampRange{StartTimestampMicros: 5000, EndTimestampMicros: 2000}}}})})
			return err
		}},
		{"CheckAndMutateRow_no_mutations", func() error {
			_, err := s.CheckAndMutateRow(ctx, &btpb.CheckAndMutateRowRequest{TableName: name, RowKey: []byte("r")})
			return err
		}},
		{"CheckAndMutateRow_empty_row_key", func() error {
			_, err := s.CheckAndMutateRow(ctx, &btpb.CheckAndMutateRowRequest{TableName: name, TrueMutations: set})
			return err
		}},
		{"ReadModifyWriteRow_no_rules", func() error {
			_, err := s.ReadModifyWriteRow(ctx, &btpb.ReadModifyWriteRowRequest{TableName: name, RowKey: []byte("r")})
			return err
		}},
		{"ReadModifyWriteRow_rule_without_operation", func() error {
			_, err := s.ReadModifyWriteRow(ctx, &btpb.ReadModifyWriteRowRequest{TableName: name, RowKey: []byte("r"),
				Rules: append(append([]*btpb.ReadModifyWriteRule(nil), appendRule...), &btpb.ReadModifyWriteRule{FamilyName: "cf", ColumnQualifier: []byte("b")})})
			return err
		}},
		{"ReadModifyWriteRow_empty_row_key", func() error {
			_, err := s.ReadModifyWriteRow(ctx, &btpb.ReadModifyWriteRowRequest{TableName: name, Rules: appendRule})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
		})
	}
	assert.Equal(t, 0, rowCount(t, s.tables[name]), "no request may have written a row")
}

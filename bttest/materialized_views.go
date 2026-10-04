package bttest

import (
	"context"
	"database/sql"
	"encoding/binary"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/jhsenjaliya/little_bigtable/bttest/internal/gsql"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxMaterializedViewsPerInstance = 50
	maxMaterializedViewsPerTable    = 5
	mvDefaultFamily                 = "default"
	mvStoragePrefix                 = "__mv__/"
)

// materializedView is a continuous materialized view. Its rows are derived
// from the source table by the view's query and stored like a table under a
// hidden storage ID. Source writes mark the view stale; it is recomputed
// before it is read, so reads always observe every committed source write.
type materializedView struct {
	name, instance, id string
	query              string
	deletionProtection bool
	source             string // source table resource name
	def                *gsql.ViewDefinition
	prepared           *gsql.Prepared
	storage            *table

	refreshMu sync.Mutex // serializes recomputation
	server    *server
	dirty     atomic.Bool // set by source commits; never taken with s.mu held in reverse order
}

func (mv *materializedView) proto() *btapb.MaterializedView {
	pb := &btapb.MaterializedView{Name: mv.name, Query: mv.query, DeletionProtection: mv.deletionProtection}
	pb.Etag = contentEtag(pb)
	return pb
}

// readable brings the view up to date with its source before a read.
func (mv *materializedView) readable() error {
	if !mv.dirty.Load() {
		return nil
	}
	mv.refreshMu.Lock()
	defer mv.refreshMu.Unlock()
	// Clear before recomputing so writes committed meanwhile mark it again.
	if !mv.dirty.Swap(false) {
		return nil
	}
	if err := mv.server.recompute(context.Background(), mv); err != nil {
		mv.dirty.Store(true)
		return err
	}
	return nil
}

func (s *server) newMaterializedView(name, query string, deletionProtection bool) (*materializedView, error) {
	instance := parentInstanceFromChild(name, "/materializedViews/")
	id := name[strings.LastIndex(name, "/")+1:]
	p, def, err := gsql.PrepareMaterializedView(query, s.catalog(instance))
	if err != nil {
		return nil, err
	}
	mv := &materializedView{
		name: name, instance: instance, id: id, query: query, deletionProtection: deletionProtection,
		source: instance + "/tables/" + def.Source, def: def, prepared: p, server: s,
	}
	mv.dirty.Store(true)
	storageID := mvStoragePrefix + id
	mv.storage = &table{
		parent: instance, tableId: storageID, storageId: storageID,
		families: map[string]*columnFamily{mvDefaultFamily: {Name: mvDefaultFamily, Order: 0}},
		rows:     NewSqlRows(s.db, instance, storageID),
	}
	columnTypes := map[string]*btpb.Type{}
	for _, c := range p.Metadata().GetProtoSchema().GetColumns() {
		columnTypes[c.GetName()] = c.GetType()
	}
	order := uint64(1)
	for _, col := range def.ValueColumns {
		if columnTypes[col].GetMapType() != nil {
			mv.storage.families[col] = &columnFamily{Name: col, Order: order}
			order++
		}
	}
	mv.storage.counter = order
	return mv, nil
}

// recompute rebuilds the view's rows from the current source data.
func (s *server) recompute(ctx context.Context, mv *materializedView) error {
	columns := mv.prepared.Metadata().GetProtoSchema().GetColumns()
	index := map[string]int{}
	for i, c := range columns {
		index[c.GetName()] = i
	}
	rows := map[string]*row{}
	err := mv.prepared.Execute(ctx, s.catalog(mv.instance), nil, func(values []*btpb.Value) error {
		key, err := mv.rowKey(values, index)
		if err != nil {
			return err
		}
		ts := int64(0)
		if mv.def.TimestampColumn != "" {
			if t := values[index[mv.def.TimestampColumn]].GetTimestampValue(); t != nil {
				ts = t.AsTime().UnixMicro()
				ts -= ts % 1000
			}
		}
		r := newRow(string(key))
		for _, col := range mv.def.ValueColumns {
			i := index[col]
			v, t := values[i], columns[i].GetType()
			if v.GetKind() == nil {
				continue
			}
			if t.GetMapType() != nil {
				fam := r.getOrCreateFamily(col, mv.storage.families[col].Order)
				for _, entry := range v.GetArrayValue().GetValues() {
					kv := entry.GetArrayValue().GetValues()
					if len(kv) != 2 || kv[1].GetKind() == nil {
						continue
					}
					cellValue, err := gsql.EncodeCellValue(t.GetMapType().GetValueType(), kv[1])
					if err != nil {
						return err
					}
					q := string(kv[0].GetBytesValue())
					if kv[0].GetStringValue() != "" {
						q = kv[0].GetStringValue()
					}
					fam.cellsByColumn(q)
					fam.Cells[q] = []cell{{Ts: ts, Value: cellValue}}
				}
				continue
			}
			cellValue, err := gsql.EncodeCellValue(t, v)
			if err != nil {
				return err
			}
			fam := r.getOrCreateFamily(mvDefaultFamily, 0)
			fam.cellsByColumn(col)
			fam.Cells[col] = []cell{{Ts: ts, Value: cellValue}}
		}
		if !r.isEmpty() {
			rows[r.key] = r
		}
		return nil
	})
	if err != nil {
		return err
	}
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := mv.storage.rows.clear(ctx, tx); err != nil {
			return err
		}
		for _, r := range rows {
			if err := mv.storage.rows.save(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})
}

func (mv *materializedView) rowKey(values []*btpb.Value, index map[string]int) ([]byte, error) {
	if mv.def.KeySchema == nil {
		if len(mv.def.KeyColumns) != 1 {
			return nil, status.Error(codes.Internal, "materialized view has no key")
		}
		return values[index[mv.def.KeyColumns[0]]].GetBytesValue(), nil
	}
	keyValues := make([]*btpb.Value, len(mv.def.KeyColumns))
	for i, col := range mv.def.KeyColumns {
		keyValues[i] = values[index[col]]
	}
	return gsql.EncodeKey(mv.def.KeySchema, keyValues)
}

func (s *server) materializedViewTarget(name string) (*dataTarget, error) {
	s.mu.Lock()
	mv, ok := s.mvs[name]
	s.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "materialized view %q not found", name)
	}
	return &dataTarget{tbl: mv.storage, mv: mv, resource: name}, nil
}

func (s *server) materializedViewsReadingLocked(tableName string) []string {
	var names []string
	for name, mv := range s.mvs {
		if mv.source == tableName {
			names = append(names, name)
		}
	}
	return names
}

// afterCommit marks views derived from tbl stale.
func (s *server) afterCommit(ctx context.Context, tbl *table, commits []*rowCommit) {
	if len(commits) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, mv := range s.mvs {
		if mv.source == tbl.name() {
			mv.dirty.Store(true)
		}
	}
}

func (s *server) dropMaterializedViewStorage(ctx context.Context, q sqlExecutor, name string) error {
	instance := parentInstanceFromChild(name, "/materializedViews/")
	id := name[strings.LastIndex(name, "/")+1:]
	if err := NewSqlRows(s.db, instance, mvStoragePrefix+id).clear(ctx, q); err != nil {
		return err
	}
	return s.mvBackend.remove(ctx, q, name)
}

func (s *server) forgetMaterializedViewLocked(name string) {
	delete(s.mvs, name)
	delete(s.materializedViews, name)
}

// LoadMaterializedViews restores views. Views persisted by earlier versions
// kept their rows in a regular table named after the view; that table is
// removed because views are now read through materialized_view_name.
func (s *server) LoadMaterializedViews() {
	ctx := context.Background()
	stored, err := s.mvBackend.getAll(ctx)
	if err != nil {
		log.Printf("load materialized views: %v", err)
		return
	}
	for _, v := range stored {
		instance := parentInstanceFromChild(v.name, "/materializedViews/")
		id := v.name[strings.LastIndex(v.name, "/")+1:]
		if legacy, ok := s.tables[instance+"/tables/"+id]; ok {
			if err := s.purgeTableStorage(ctx, legacy); err != nil {
				log.Printf("WARNING: could not remove legacy materialized view table %s: %v", legacy.name(), err)
			} else {
				delete(s.tables, legacy.name())
				log.Printf("removed legacy materialized view table %s; read the view with materialized_view_name", legacy.name())
			}
		}
		mv, err := s.newMaterializedView(v.name, v.query, v.deletionProtection)
		if err != nil {
			log.Printf("WARNING: skipping materialized view %q: %v", v.name, err)
			continue
		}
		s.mvs[v.name] = mv
		s.materializedViews[v.name] = mv.proto()
	}
}

func (s *server) CreateMaterializedView(ctx context.Context, req *btapb.CreateMaterializedViewRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	if req.GetParent() == "" || !tableIDPattern.MatchString(req.GetMaterializedViewId()) {
		return nil, status.Error(codes.InvalidArgument, "parent and a valid materialized_view_id are required")
	}
	if req.GetMaterializedView().GetQuery() == "" {
		return nil, status.Error(codes.InvalidArgument, "materialized_view.query is required")
	}
	if err := s.localRequireInstance(req.Parent); err != nil {
		return nil, err
	}
	name := req.Parent + "/materializedViews/" + req.MaterializedViewId
	mv, err := s.newMaterializedView(name, req.MaterializedView.Query, req.MaterializedView.GetDeletionProtection())
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if _, exists := s.mvs[name]; exists {
		s.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "materialized view %q already exists", name)
	}
	perInstance, perTable := 0, 0
	for n, other := range s.mvs {
		if strings.HasPrefix(n, req.Parent+"/") {
			perInstance++
			if other.source == mv.source {
				perTable++
			}
		}
	}
	if perInstance >= maxMaterializedViewsPerInstance || perTable >= maxMaterializedViewsPerTable {
		s.mu.Unlock()
		return nil, status.Errorf(codes.ResourceExhausted, "materialized view limit reached (%d per instance, %d per table)", maxMaterializedViewsPerInstance, maxMaterializedViewsPerTable)
	}
	if err := s.mvBackend.save(ctx, nil, name, mv.query, mv.deletionProtection); err != nil {
		s.mu.Unlock()
		return nil, internalErr(err)
	}
	s.mvs[name] = mv
	s.materializedViews[name] = mv.proto()
	s.mu.Unlock()

	// Initial population completes before the operation finishes.
	if err := mv.readable(); err != nil {
		return nil, err
	}
	return s.ops.complete(ctx, name, &btapb.CreateMaterializedViewMetadata{OriginalRequest: req, StartTime: start, EndTime: timestamppb.Now()}, mv.proto())
}

func (s *server) GetMaterializedView(ctx context.Context, req *btapb.GetMaterializedViewRequest) (*btapb.MaterializedView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mv, ok := s.mvs[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "materialized view %q not found", req.GetName())
	}
	return mv.proto(), nil
}

func (s *server) ListMaterializedViews(ctx context.Context, req *btapb.ListMaterializedViewsRequest) (*btapb.ListMaterializedViewsResponse, error) {
	if req.GetParent() == "" {
		return nil, status.Error(codes.InvalidArgument, "parent is required")
	}
	s.mu.Lock()
	var views []*btapb.MaterializedView
	for name, mv := range s.mvs {
		if strings.HasPrefix(name, req.Parent+"/materializedViews/") {
			views = append(views, mv.proto())
		}
	}
	s.mu.Unlock()
	page, next, err := paginate(views, (*btapb.MaterializedView).GetName, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &btapb.ListMaterializedViewsResponse{MaterializedViews: page, NextPageToken: next}, nil
}

// UpdateMaterializedView can change only deletion_protection; the query is
// immutable.
func (s *server) UpdateMaterializedView(ctx context.Context, req *btapb.UpdateMaterializedViewRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	update := req.GetMaterializedView()
	if update.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "materialized_view.name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mv, ok := s.mvs[update.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "materialized view %q not found", update.Name)
	}
	if err := checkEtag(update.GetEtag(), mv.proto().GetEtag()); err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_mask is required")
	}
	for _, path := range paths {
		switch path {
		case "deletion_protection":
		case "query":
			return nil, status.Error(codes.InvalidArgument, "the query of a materialized view is immutable")
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update_mask path %q", path)
		}
	}
	if err := s.mvBackend.save(ctx, nil, mv.name, mv.query, update.GetDeletionProtection()); err != nil {
		return nil, internalErr(err)
	}
	mv.deletionProtection = update.GetDeletionProtection()
	s.materializedViews[mv.name] = mv.proto()
	return s.ops.complete(ctx, mv.name, &btapb.UpdateMaterializedViewMetadata{OriginalRequest: req, StartTime: start, EndTime: timestamppb.Now()}, mv.proto())
}

func (s *server) DeleteMaterializedView(ctx context.Context, req *btapb.DeleteMaterializedViewRequest) (*empty.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mv, ok := s.mvs[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "materialized view %q not found", req.GetName())
	}
	if err := checkEtag(req.GetEtag(), mv.proto().GetEtag()); err != nil {
		return nil, err
	}
	if mv.deletionProtection {
		return nil, status.Errorf(codes.FailedPrecondition, "materialized view %q is protected against deletion", req.GetName())
	}
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := s.dropMaterializedViewStorage(ctx, tx, mv.name); err != nil {
			return err
		}
		return s.iamBackend.deletePrefix(ctx, tx, mv.name)
	})
	if err != nil {
		return nil, internalErr(err)
	}
	s.forgetMaterializedViewLocked(mv.name)
	return new(empty.Empty), nil
}

var (
	_ = binary.BigEndian
	_ = time.Now
	_ = proto.Clone
)

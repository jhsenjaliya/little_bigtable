package bttest

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	emptypb "github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// undeleteWindow is how long a deleted table can be restored.
const undeleteWindow = 7 * 24 * time.Hour

var (
	tableIDPattern  = regexp.MustCompile(`^[_a-zA-Z0-9][-_.a-zA-Z0-9]{0,49}$`)
	familyIDPattern = regexp.MustCompile(`^[-_.a-zA-Z0-9]{1,64}$`)
)

func validateGCRule(rule *btapb.GcRule) error {
	if rule == nil {
		return nil
	}
	if proto.Size(rule) > 500 {
		return status.Error(codes.InvalidArgument, "gc_rule must serialize to at most 500 bytes")
	}
	var walk func(*btapb.GcRule) error
	walk = func(r *btapb.GcRule) error {
		switch rr := r.GetRule().(type) {
		case *btapb.GcRule_MaxAge:
			if rr.MaxAge.AsDuration() < time.Millisecond {
				return status.Error(codes.InvalidArgument, "gc_rule max_age must be at least one millisecond")
			}
		case *btapb.GcRule_MaxNumVersions:
			if rr.MaxNumVersions < 1 {
				return status.Error(codes.InvalidArgument, "gc_rule max_num_versions must be at least 1")
			}
		case *btapb.GcRule_Union_:
			for _, sub := range rr.Union.GetRules() {
				if err := walk(sub); err != nil {
					return err
				}
			}
		case *btapb.GcRule_Intersection_:
			for _, sub := range rr.Intersection.GetRules() {
				if err := walk(sub); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(rule)
}

// validateValueType accepts unset types and the documented aggregate types:
// sum, min and max over Int64 and HyperLogLog++.
func validateValueType(t *btapb.Type) error {
	if t == nil || t.GetKind() == nil {
		return nil
	}
	agg := t.GetAggregateType()
	if agg == nil {
		return status.Errorf(codes.InvalidArgument, "column family value_type must be an Aggregate type, got %T", t.GetKind())
	}
	switch agg.GetAggregator().(type) {
	case *btapb.Type_Aggregate_Sum_, *btapb.Type_Aggregate_Min_, *btapb.Type_Aggregate_Max_:
		if agg.GetInputType().GetInt64Type() == nil {
			return status.Error(codes.InvalidArgument, "sum, min and max aggregates require an Int64 input_type")
		}
	case *btapb.Type_Aggregate_HllppUniqueCount:
	default:
		return status.Error(codes.InvalidArgument, "aggregate value_type requires an aggregator")
	}
	return nil
}

func validateFamily(id string, cf *btapb.ColumnFamily) error {
	if !familyIDPattern.MatchString(id) {
		return status.Errorf(codes.InvalidArgument, "invalid column family ID %q", id)
	}
	if err := validateGCRule(cf.GetGcRule()); err != nil {
		return err
	}
	return validateValueType(cf.GetValueType())
}

// validateRowKeySchema checks field names; Bigtable rejects names that
// collide with column families or the reserved _key and _timestamp columns.
func validateRowKeySchema(schema *btapb.Type_Struct, families map[string]bool) error {
	if schema == nil {
		return nil
	}
	if len(schema.GetFields()) == 0 {
		return status.Error(codes.InvalidArgument, "row_key_schema must define at least one field")
	}
	seen := map[string]bool{}
	for _, f := range schema.GetFields() {
		name := f.GetFieldName()
		if name == "" {
			return status.Error(codes.InvalidArgument, "row_key_schema fields must be named")
		}
		if name == "_key" || name == "_timestamp" || families[name] {
			return status.Errorf(codes.InvalidArgument, "row_key_schema field %q collides with a column family or reserved column", name)
		}
		if seen[name] {
			return status.Errorf(codes.InvalidArgument, "duplicate row_key_schema field %q", name)
		}
		seen[name] = true
		if f.GetType().GetKind() == nil {
			return status.Errorf(codes.InvalidArgument, "row_key_schema field %q has no type", name)
		}
	}
	return nil
}

func (s *server) CreateTable(ctx context.Context, req *btapb.CreateTableRequest) (*btapb.Table, error) {
	if req.GetParent() == "" {
		return nil, status.Error(codes.InvalidArgument, "parent is required")
	}
	if !tableIDPattern.MatchString(req.GetTableId()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid table ID %q", req.GetTableId())
	}
	if err := s.localRequireInstance(req.Parent); err != nil {
		return nil, err
	}
	families := map[string]bool{}
	for id, cf := range req.GetTable().GetColumnFamilies() {
		if err := validateFamily(id, cf); err != nil {
			return nil, err
		}
		families[id] = true
	}
	if err := validateChangeStreamConfig(req.GetTable().GetChangeStreamConfig()); err != nil {
		return nil, err
	}
	if err := validateRowKeySchema(req.GetTable().GetRowKeySchema(), families); err != nil {
		return nil, err
	}
	for _, split := range req.GetInitialSplits() {
		if len(split.GetKey()) == 0 {
			return nil, status.Error(codes.InvalidArgument, "initial split keys must be non-empty")
		}
	}
	name := req.Parent + "/tables/" + req.TableId

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tables[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "table %q already exists", name)
	}
	tbl := newTable(req, s.db)
	if err := s.tableBackend.Save(ctx, nil, tbl); err != nil {
		return nil, storageErr(err)
	}
	s.tables[name] = tbl
	return s.tableView(tbl, btapb.Table_SCHEMA_VIEW), nil
}

// tableView renders a table for the requested view. Callers hold s.mu.
func (s *server) tableView(tbl *table, view btapb.Table_View) *btapb.Table {
	tbl.mu.RLock()
	defer tbl.mu.RUnlock()
	switch view {
	case btapb.Table_NAME_ONLY:
		return &btapb.Table{Name: tbl.name()}
	case btapb.Table_REPLICATION_VIEW, btapb.Table_ENCRYPTION_VIEW:
		return &btapb.Table{Name: tbl.name(), ClusterStates: s.clusterStatesLocked(tbl, view == btapb.Table_ENCRYPTION_VIEW)}
	case btapb.Table_FULL:
		pb := tbl.schemaProtoNoLock()
		pb.ClusterStates = s.clusterStatesLocked(tbl, true)
		return pb
	default:
		return tbl.schemaProtoNoLock()
	}
}

// clusterStatesLocked reports one READY replica per registered cluster of
// the table's instance. A single process has no replication lag.
func (s *server) clusterStatesLocked(tbl *table, withEncryption bool) map[string]*btapb.Table_ClusterState {
	states := map[string]*btapb.Table_ClusterState{}
	prefix := tbl.parent + "/clusters/"
	for name := range s.clusters {
		if strings.HasPrefix(name, prefix) {
			states[strings.TrimPrefix(name, prefix)] = &btapb.Table_ClusterState{}
		}
	}
	if len(states) == 0 {
		states["local-cluster"] = &btapb.Table_ClusterState{}
	}
	for _, st := range states {
		st.ReplicationState = btapb.Table_ClusterState_READY
		if withEncryption {
			st.EncryptionInfo = []*btapb.EncryptionInfo{googleDefaultEncryption()}
		}
	}
	return states
}

func googleDefaultEncryption() *btapb.EncryptionInfo {
	return &btapb.EncryptionInfo{EncryptionType: btapb.EncryptionInfo_GOOGLE_DEFAULT_ENCRYPTION}
}

func (s *server) CreateTableFromSnapshot(context.Context, *btapb.CreateTableFromSnapshotRequest) (*longrunningpb.Operation, error) {
	return nil, status.Errorf(codes.Unimplemented, "table snapshots are a deprecated private-alpha feature; use backups")
}

func (s *server) ListTables(ctx context.Context, req *btapb.ListTablesRequest) (*btapb.ListTablesResponse, error) {
	if req.GetParent() == "" {
		return nil, status.Error(codes.InvalidArgument, "parent is required")
	}
	if err := s.localRequireInstance(req.Parent); err != nil {
		return nil, err
	}
	view := req.GetView()
	if view == btapb.Table_VIEW_UNSPECIFIED {
		view = btapb.Table_NAME_ONLY
	}
	prefix := req.Parent + "/tables/"
	s.mu.Lock()
	var tables []*btapb.Table
	for name, tbl := range s.tables {
		if strings.HasPrefix(name, prefix) {
			tables = append(tables, s.tableView(tbl, view))
		}
	}
	s.mu.Unlock()
	page, next, err := paginate(tables, func(t *btapb.Table) string { return t.Name }, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &btapb.ListTablesResponse{Tables: page, NextPageToken: next}, nil
}

func (s *server) GetTable(ctx context.Context, req *btapb.GetTableRequest) (*btapb.Table, error) {
	view := req.GetView()
	if view == btapb.Table_VIEW_UNSPECIFIED {
		view = btapb.Table_SCHEMA_VIEW
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tbl, ok := s.tables[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "table %q not found", req.GetName())
	}
	return s.tableView(tbl, view), nil
}

func (s *server) UpdateTable(ctx context.Context, req *btapb.UpdateTableRequest) (*longrunningpb.Operation, error) {
	start := time.Now()
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_mask is required")
	}
	if req.GetTable() == nil {
		return nil, status.Error(codes.InvalidArgument, "table is required")
	}
	for _, path := range paths {
		switch path {
		case "change_stream_config", "change_stream_config.retention_period", "deletion_protection", "row_key_schema",
			"automated_backup_policy", "automated_backup_policy.retention_period", "automated_backup_policy.frequency",
			"automated_backup_policy.locations", "automated_backup_policy.keep_hot_duration", "automated_backup_policy.disabled",
			"tiered_storage_config":
		case "column_families":
			return nil, status.Error(codes.Unimplemented, "column_families cannot be updated with UpdateTable; use ModifyColumnFamilies")
		case "*":
			return nil, status.Error(codes.InvalidArgument, "wildcard update masks are not supported")
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update_mask path %q", path)
		}
	}
	tbl, err := s.localRequireTable(req.Table.GetName())
	if err != nil {
		return nil, err
	}

	tbl.mu.Lock()
	next := tbl.cloneMeta() // candidate metadata; tbl changes only after it persists
	for _, path := range paths {
		switch {
		case path == "deletion_protection":
			next.deletionProtection = req.Table.GetDeletionProtection()
		case path == "change_stream_config", path == "change_stream_config.retention_period":
			cfg := req.Table.GetChangeStreamConfig()
			if err := validateChangeStreamConfig(cfg); err != nil {
				tbl.mu.Unlock()
				return nil, err
			}
			if cfg == nil || cfg.GetRetentionPeriod() == nil {
				next.changeStream = nil
			} else {
				next.changeStream = proto.Clone(cfg).(*btapb.ChangeStreamConfig)
			}
		case path == "row_key_schema":
			schema := req.Table.GetRowKeySchema()
			switch {
			case schema == nil && tbl.rowKeySchema != nil && !req.GetIgnoreWarnings():
				tbl.mu.Unlock()
				return nil, status.Error(codes.InvalidArgument, "removing a row_key_schema requires ignore_warnings")
			case schema != nil && tbl.rowKeySchema != nil && !proto.Equal(schema, tbl.rowKeySchema):
				tbl.mu.Unlock()
				return nil, status.Error(codes.InvalidArgument, "a row_key_schema cannot be modified; remove it and set a new one")
			}
			fams := map[string]bool{}
			for id := range tbl.families {
				fams[id] = true
			}
			if err := validateRowKeySchema(schema, fams); err != nil {
				tbl.mu.Unlock()
				return nil, err
			}
			next.rowKeySchema = schema
		case strings.HasPrefix(path, "automated_backup_policy"):
			next.backupPolicy = mergeBackupPolicy(next.backupPolicy, getAutomatedBackupPolicy(req.Table), path)
		case path == "tiered_storage_config":
			next.tieredStorage = req.Table.GetTieredStorageConfig()
		}
	}
	if err := s.tableBackend.Save(ctx, nil, next); err != nil {
		tbl.mu.Unlock()
		return nil, storageErr(err)
	}
	disabledStream := tbl.changeStreamEnabled() && !next.changeStreamEnabled()
	tbl.deletionProtection = next.deletionProtection
	tbl.changeStream = next.changeStream
	tbl.rowKeySchema = next.rowKeySchema
	tbl.backupPolicy = next.backupPolicy
	tbl.tieredStorage = next.tieredStorage
	tbl.mu.Unlock()
	if disabledStream {
		// A disabled change stream cannot be read, including earlier records.
		if err := s.changeLog.purgeTable(ctx, nil, tbl.name()); err != nil {
			return nil, storageErr(err)
		}
	}

	s.mu.Lock()
	resp := s.tableView(tbl, btapb.Table_SCHEMA_VIEW)
	s.mu.Unlock()
	return s.ops.complete(ctx, tbl.name(), &btapb.UpdateTableMetadata{
		Name: tbl.name(), StartTime: timestamppb.New(start), EndTime: timestamppb.Now(),
	}, resp)
}

func mergeBackupPolicy(cur, update *btapb.Table_AutomatedBackupPolicy, path string) *btapb.Table_AutomatedBackupPolicy {
	if path == "automated_backup_policy" {
		if update == nil {
			return nil
		}
		return proto.Clone(update).(*btapb.Table_AutomatedBackupPolicy)
	}
	out := &btapb.Table_AutomatedBackupPolicy{}
	if cur != nil {
		out = proto.Clone(cur).(*btapb.Table_AutomatedBackupPolicy)
	}
	switch path {
	case "automated_backup_policy.retention_period":
		out.RetentionPeriod = update.GetRetentionPeriod()
	case "automated_backup_policy.frequency":
		out.Frequency = update.GetFrequency()
	case "automated_backup_policy.locations":
		out.Locations = update.GetLocations()
	case "automated_backup_policy.keep_hot_duration":
		out.KeepHotDuration = update.GetKeepHotDuration()
	case "automated_backup_policy.disabled":
		out.Disabled = update.GetDisabled()
	}
	return out
}

// DeleteTable moves the table to an undeletable tombstone for the
// undelete window. Its rows and authorized views are retained with it.
func (s *server) DeleteTable(ctx context.Context, req *btapb.DeleteTableRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tbl, ok := s.tables[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "table %q not found", req.GetName())
	}
	if err := s.checkTableDeletableLocked(tbl); err != nil {
		return nil, err
	}
	tbl.mu.Lock()
	defer tbl.mu.Unlock()
	deleteTime := time.Now()
	tombstoneID := tbl.tableId + "@deleted@" + strconv.FormatInt(deleteTime.UnixMicro(), 10)
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		tbl.deleteTime = deleteTime
		if err := s.tableBackend.Rename(ctx, tx, tbl, tombstoneID); err != nil {
			return err
		}
		if err := s.changeLog.purgeTable(ctx, tx, tbl.name()); err != nil {
			return err
		}
		// Authorized views and schema bundles move with the tombstone, so a
		// re-created table starts without them and UndeleteTable restores them.
		if err := s.moveTableChildren(ctx, tx, tbl.name(), tbl.storageName()); err != nil {
			return err
		}
		return s.iamBackend.deletePrefix(ctx, tx, tbl.name())
	})
	if err != nil {
		tbl.deleteTime = time.Time{}
		tbl.storageId = tbl.tableId
		tbl.rows = NewSqlRows(s.db, tbl.parent, tbl.tableId)
		return nil, status.Errorf(codes.Internal, "delete table: %v", err)
	}
	delete(s.tables, req.GetName())
	s.deletedTables[tbl.storageName()] = tbl
	return &emptypb.Empty{}, nil
}

// checkTableDeletableLocked enforces deletion protection on the table, its
// authorized views, and materialized views that read from it.
func (s *server) checkTableDeletableLocked(tbl *table) error {
	tbl.mu.RLock()
	protected := tbl.deletionProtection
	tbl.mu.RUnlock()
	if protected {
		return status.Errorf(codes.FailedPrecondition, "table %q has deletion protection enabled", tbl.name())
	}
	views, err := s.avBackend.ListByTable(tbl.name())
	if err != nil {
		return storageErr(err)
	}
	for _, av := range views {
		if av.GetDeletionProtection() {
			return status.Errorf(codes.FailedPrecondition, "table %q has authorized view %q with deletion protection enabled", tbl.name(), av.GetName())
		}
	}
	for _, mv := range s.materializedViewsReadingLocked(tbl.name()) {
		return status.Errorf(codes.FailedPrecondition, "table %q is the source of materialized view %q", tbl.name(), mv)
	}
	return nil
}

func (s *server) UndeleteTable(ctx context.Context, req *btapb.UndeleteTableRequest) (*longrunningpb.Operation, error) {
	start := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tables[req.GetName()]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "table %q already exists", req.GetName())
	}
	var latest *table
	for _, t := range s.deletedTables {
		if t.name() == req.GetName() && (latest == nil || t.deleteTime.After(latest.deleteTime)) {
			latest = t
		}
	}
	if latest == nil || time.Since(latest.deleteTime) > undeleteWindow {
		return nil, status.Errorf(codes.NotFound, "no deleted table %q is available to undelete", req.GetName())
	}
	latest.mu.Lock()
	tombstoneName := latest.storageName()
	prevDelete := latest.deleteTime
	latest.deleteTime = time.Time{}
	// Bigtable enables deletion protection on undeleted tables.
	prevProtection := latest.deletionProtection
	latest.deletionProtection = true
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := s.moveTableChildren(ctx, tx, tombstoneName, latest.name()); err != nil {
			return err
		}
		return s.tableBackend.Rename(ctx, tx, latest, latest.tableId)
	})
	if err != nil {
		latest.deleteTime = prevDelete
		latest.deletionProtection = prevProtection
		latest.mu.Unlock()
		return nil, status.Errorf(codes.Internal, "undelete table: %v", err)
	}
	latest.mu.Unlock()
	delete(s.deletedTables, tombstoneName)
	s.tables[latest.name()] = latest
	return s.ops.complete(ctx, latest.name(), &btapb.UndeleteTableMetadata{
		Name: latest.name(), StartTime: timestamppb.New(start), EndTime: timestamppb.Now(),
	}, s.tableView(latest, btapb.Table_SCHEMA_VIEW))
}

func (s *server) purgeExpiredTombstones(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, t := range s.deletedTables {
		if time.Since(t.deleteTime) <= undeleteWindow {
			continue
		}
		if err := s.purgeTableStorage(ctx, t); err != nil {
			return err
		}
		if err := s.avBackend.DeleteByTable(name); err != nil {
			return err
		}
		if err := s.sbBackend.deleteByTable(ctx, name); err != nil {
			return err
		}
		delete(s.deletedTables, name)
	}
	return nil
}

// moveTableChildren re-parents a table's authorized views and schema
// bundles from one table resource name to another.
func (s *server) moveTableChildren(ctx context.Context, q sqlExecutor, from, to string) error {
	views, err := s.avBackend.store.listPrefix(ctx, q, from+"/authorizedViews/")
	if err != nil {
		return err
	}
	for _, av := range views {
		old := av.GetName()
		av.Name = to + strings.TrimPrefix(old, from)
		if err := s.avBackend.store.remove(ctx, q, old); err != nil {
			return err
		}
		if err := s.avBackend.store.put(ctx, q, av.Name, to, av); err != nil {
			return err
		}
	}
	bundles, err := s.sbBackend.store.listPrefix(ctx, q, from+"/schemaBundles/")
	if err != nil {
		return err
	}
	for _, sb := range bundles {
		old := sb.GetName()
		sb.Name = to + strings.TrimPrefix(old, from)
		if err := s.sbBackend.store.remove(ctx, q, old); err != nil {
			return err
		}
		if err := s.sbBackend.store.put(ctx, q, sb.Name, to, sb); err != nil {
			return err
		}
	}
	return nil
}

// purgeTableStorage permanently removes a table's rows and metadata.
func (s *server) purgeTableStorage(ctx context.Context, t *table) error {
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := t.rows.clear(ctx, tx); err != nil {
			return err
		}
		return s.tableBackend.Delete(ctx, tx, t)
	})
}

func (s *server) ModifyColumnFamilies(ctx context.Context, req *btapb.ModifyColumnFamiliesRequest) (*btapb.Table, error) {
	tbl, err := s.localRequireTable(req.GetName())
	if err != nil {
		return nil, err
	}
	if len(req.GetModifications()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one modification is required")
	}
	tbl.mu.Lock()
	defer tbl.mu.Unlock()

	// Validate and apply to a candidate family set; nothing changes on error.
	next := make(map[string]*columnFamily, len(tbl.families))
	for id, cf := range tbl.families {
		cp := *cf
		next[id] = &cp
	}
	counter := tbl.counter
	var dropped []string
	for _, mod := range req.Modifications {
		id := mod.GetId()
		switch m := mod.GetMod().(type) {
		case *btapb.ModifyColumnFamiliesRequest_Modification_Create:
			if err := validateFamily(id, m.Create); err != nil {
				return nil, err
			}
			if _, ok := next[id]; ok {
				return nil, status.Errorf(codes.AlreadyExists, "column family %q already exists", id)
			}
			next[id] = &columnFamily{
				Name:      req.Name + "/columnFamilies/" + id,
				Order:     counter,
				GCRule:    m.Create.GetGcRule(),
				ValueType: m.Create.GetValueType(),
			}
			counter++
		case *btapb.ModifyColumnFamiliesRequest_Modification_Update:
			cur, ok := next[id]
			if !ok {
				return nil, status.Errorf(codes.NotFound, "column family %q not found", id)
			}
			if err := validateGCRule(m.Update.GetGcRule()); err != nil {
				return nil, err
			}
			paths := mod.GetUpdateMask().GetPaths()
			if len(paths) == 0 {
				paths = []string{"gc_rule"}
				if m.Update.GetValueType() != nil {
					paths = append(paths, "value_type")
				}
			}
			for _, path := range paths {
				switch path {
				case "gc_rule":
					cur.GCRule = m.Update.GetGcRule()
				case "value_type":
					if !proto.Equal(cur.ValueType, m.Update.GetValueType()) {
						return nil, status.Errorf(codes.InvalidArgument, "the value_type of column family %q is immutable", id)
					}
				default:
					return nil, status.Errorf(codes.InvalidArgument, "unsupported column family update_mask path %q", path)
				}
			}
		case *btapb.ModifyColumnFamiliesRequest_Modification_Drop:
			if !m.Drop {
				return nil, status.Errorf(codes.InvalidArgument, "drop must be true for column family %q", id)
			}
			if _, ok := next[id]; !ok {
				return nil, status.Errorf(codes.NotFound, "column family %q not found", id)
			}
			delete(next, id)
			dropped = append(dropped, id)
		default:
			return nil, status.Errorf(codes.InvalidArgument, "modification for column family %q has no create, update or drop", id)
		}
	}
	if tbl.rowKeySchema != nil {
		fams := map[string]bool{}
		for id := range next {
			fams[id] = true
		}
		if err := validateRowKeySchema(tbl.rowKeySchema, fams); err != nil {
			return nil, err
		}
	}

	candidate := tbl.cloneMeta()
	candidate.families = next
	candidate.counter = counter
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := s.tableBackend.Save(ctx, tx, candidate); err != nil {
			return err
		}
		if len(dropped) == 0 {
			return nil
		}
		// Dropping a family deletes its data, so a re-created family of the
		// same name starts empty.
		return dropFamilyData(ctx, tx, tbl.rows, dropped)
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "modify column families: %v", err)
	}
	tbl.families = next
	tbl.counter = counter
	return tbl.schemaProtoNoLock(), nil
}

func dropFamilyData(ctx context.Context, q sqlExecutor, rows *SqlRows, families []string) error {
	var changed []*row
	err := rows.scan(ctx, q, keyRange{}, false, func(r *row) (bool, error) {
		touched := false
		for _, f := range families {
			if _, ok := r.families[f]; ok {
				delete(r.families, f)
				touched = true
			}
		}
		if touched {
			changed = append(changed, r)
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	for _, r := range changed {
		if err := rows.save(ctx, q, r); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) DropRowRange(ctx context.Context, req *btapb.DropRowRangeRequest) (*emptypb.Empty, error) {
	tbl, err := s.localRequireTable(req.GetName())
	if err != nil {
		return nil, err
	}
	var rng keyRange
	switch t := req.GetTarget().(type) {
	case *btapb.DropRowRangeRequest_DeleteAllDataFromTable:
		if !t.DeleteAllDataFromTable {
			return nil, status.Error(codes.InvalidArgument, "delete_all_data_from_table must be true if set")
		}
	case *btapb.DropRowRangeRequest_RowKeyPrefix:
		if len(t.RowKeyPrefix) == 0 {
			return nil, status.Error(codes.InvalidArgument, "row_key_prefix must be non-empty")
		}
		rng = prefixRange(string(t.RowKeyPrefix))
	default:
		return nil, status.Error(codes.InvalidArgument, "one of row_key_prefix or delete_all_data_from_table is required")
	}

	tbl.mu.Lock()
	var deleted []*rowCommit
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		deleted = nil
		err := tbl.rows.scan(ctx, tx, rng, false, func(r *row) (bool, error) {
			c := &rowCommit{key: r.key, before: r, after: newRow(r.key)}
			for _, fam := range r.sortedFamilies() {
				c.userChanges = append(c.userChanges, &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromFamily_{
					DeleteFromFamily: &btpb.Mutation_DeleteFromFamily{FamilyName: fam.Name},
				}})
			}
			deleted = append(deleted, c)
			return true, nil
		})
		if err != nil {
			return err
		}
		if _, ok := req.GetTarget().(*btapb.DropRowRangeRequest_DeleteAllDataFromTable); ok {
			if err := tbl.rows.clear(ctx, tx); err != nil {
				return err
			}
		} else {
			for _, c := range deleted {
				if err := tbl.rows.remove(ctx, tx, c.key); err != nil {
					return err
				}
			}
		}
		if !tbl.changeStreamEnabled() {
			return nil
		}
		commitMicros := time.Now().UnixMicro()
		for _, c := range deleted {
			if err := s.changeLog.append(ctx, tx, tbl.name(), c.key, btpb.ReadChangeStreamResponse_DataChange_USER, commitMicros, c.userChanges); err != nil {
				return err
			}
		}
		return nil
	})
	tbl.mu.Unlock()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "drop row range: %v", err)
	}
	s.afterCommit(ctx, tbl, deleted)
	return &emptypb.Empty{}, nil
}

// prefixRange is the key range covering every key with prefix p.
func prefixRange(p string) keyRange {
	end := []byte(p)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return keyRange{start: p, end: string(end[:i+1])}
		}
	}
	return keyRange{start: p}
}

// Consistency tokens identify the table and the moment they were issued.
// With a single committed store every write before issuance is visible, so
// any valid token is immediately consistent.
func (s *server) GenerateConsistencyToken(ctx context.Context, req *btapb.GenerateConsistencyTokenRequest) (*btapb.GenerateConsistencyTokenResponse, error) {
	if _, err := s.localRequireTable(req.GetName()); err != nil {
		return nil, err
	}
	raw := fmt.Sprintf("ct1|%s|%d", req.GetName(), time.Now().UnixMicro())
	return &btapb.GenerateConsistencyTokenResponse{ConsistencyToken: base64.RawURLEncoding.EncodeToString([]byte(raw))}, nil
}

func (s *server) CheckConsistency(ctx context.Context, req *btapb.CheckConsistencyRequest) (*btapb.CheckConsistencyResponse, error) {
	if _, err := s.localRequireTable(req.GetName()); err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(req.GetConsistencyToken())
	parts := strings.Split(string(raw), "|")
	if err != nil || len(parts) != 3 || parts[0] != "ct1" || parts[1] != req.GetName() {
		return nil, status.Errorf(codes.InvalidArgument, "consistency token %q is not valid for table %q", req.GetConsistencyToken(), req.GetName())
	}
	return &btapb.CheckConsistencyResponse{Consistent: true}, nil
}

func (s *server) SnapshotTable(context.Context, *btapb.SnapshotTableRequest) (*longrunningpb.Operation, error) {
	return nil, status.Errorf(codes.Unimplemented, "table snapshots are a deprecated private-alpha feature; use backups")
}

func (s *server) GetSnapshot(context.Context, *btapb.GetSnapshotRequest) (*btapb.Snapshot, error) {
	return nil, status.Errorf(codes.Unimplemented, "table snapshots are a deprecated private-alpha feature; use backups")
}

func (s *server) ListSnapshots(context.Context, *btapb.ListSnapshotsRequest) (*btapb.ListSnapshotsResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "table snapshots are a deprecated private-alpha feature; use backups")
}

func (s *server) DeleteSnapshot(context.Context, *btapb.DeleteSnapshotRequest) (*emptypb.Empty, error) {
	return nil, status.Errorf(codes.Unimplemented, "table snapshots are a deprecated private-alpha feature; use backups")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

/*
Copyright 2015 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
Package bttest contains a SQL-backed Cloud Bigtable emulator.

To use a Server, create it, and then connect to it with no security:

	srv, err := bttest.NewServer("localhost:0", db)
	...
	conn, err := grpc.Dial(srv.Addr, grpc.WithInsecure())
	...
	client, err := bigtable.NewClient(ctx, proj, instance,
	        option.WithGRPCConn(conn))
	...
*/
package bttest // import "cloud.google.com/go/bigtable/bttest"

import (
	"context"
	"database/sql"
	"encoding/gob"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Server is a SQL-backed Cloud Bigtable emulator.
// It is unauthenticated and serves plaintext gRPC.
type Server struct {
	Addr string

	l   net.Listener
	srv *grpc.Server
	s   *server
}

// server is the real implementation of the emulator. It is a separate and
// unexported type so the API won't be cluttered with methods that are only
// relevant to the implementation.
type server struct {
	mu            sync.Mutex
	tables        map[string]*table          // live tables keyed by resource name
	deletedTables map[string]*table          // undeletable tombstones keyed by resource name
	instances     map[string]*btapb.Instance // keyed by resource name
	clusters      map[string]*btapb.Cluster  // keyed by resource name
	appProfiles   map[string]*btapb.AppProfile
	gcc           chan int // closed when the server shuts down
	db            *sql.DB

	tableBackend  *SqlTables
	adminBackend  *SqlAdminMetadata
	changeLog     *SqlChangeLog
	mvBackend     *SqlMaterializedViews
	avBackend     *SqlAuthorizedViews
	backupBackend *SqlBackups
	lvBackend     *SqlLogicalViews
	sbBackend     *SqlSchemaBundles
	iamBackend    *SqlIAMPolicies
	ops           *operationsServer
	queries       *preparedQueries

	materializedViews map[string]*btapb.MaterializedView // keyed by resource name
	mvs               map[string]*materializedView       // compiled views keyed by resource name

	btapb.UnimplementedBigtableTableAdminServer
	btapb.UnimplementedBigtableInstanceAdminServer
	btpb.UnimplementedBigtableServer
}

// newServerState builds the emulator state backed by db without loading
// persisted resources or serving gRPC.
func newServerState(db *sql.DB) *server {
	return &server{
		tables:            make(map[string]*table),
		deletedTables:     make(map[string]*table),
		instances:         make(map[string]*btapb.Instance),
		clusters:          make(map[string]*btapb.Cluster),
		appProfiles:       make(map[string]*btapb.AppProfile),
		materializedViews: make(map[string]*btapb.MaterializedView),
		mvs:               make(map[string]*materializedView),
		gcc:               make(chan int),
		db:                db,
		tableBackend:      NewSqlTables(db),
		adminBackend:      NewSqlAdminMetadata(db),
		changeLog:         NewSqlChangeLog(db),
		mvBackend:         NewSqlMaterializedViews(db),
		avBackend:         NewSqlAuthorizedViews(db),
		backupBackend:     NewSqlBackups(db),
		lvBackend:         NewSqlLogicalViews(db),
		sbBackend:         NewSqlSchemaBundles(db),
		iamBackend:        NewSqlIAMPolicies(db),
		ops:               newOperationsServer(db),
		queries:           newPreparedQueries(),
	}
}

// load restores every persisted resource into memory.
func (s *server) load(ctx context.Context) error {
	s.LoadAdminMetadata()
	if err := s.loadTables(ctx); err != nil {
		return err
	}
	s.LoadMaterializedViews()
	return nil
}

// NewServer creates a new Server.
// The Server will be listening for gRPC connections, without TLS,
// on the provided address. The resolved address is named by the Addr field.
func NewServer(laddr string, db *sql.DB, opt ...grpc.ServerOption) (*Server, error) {
	state := newServerState(db)
	if err := state.load(context.Background()); err != nil {
		return nil, err
	}

	l, err := net.Listen("tcp", laddr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		Addr: l.Addr().String(),
		l:    l,
		srv:  grpc.NewServer(opt...),
		s:    state,
	}
	longrunningpb.RegisterOperationsServer(s.srv, state.ops)
	btapb.RegisterBigtableInstanceAdminServer(s.srv, state)
	btapb.RegisterBigtableTableAdminServer(s.srv, state)
	btpb.RegisterBigtableServer(s.srv, state)

	go s.srv.Serve(s.l)
	go state.maintenanceLoop()

	return s, nil
}

// Close shuts down the server.
func (s *Server) Close() {
	s.s.mu.Lock()
	select {
	case <-s.s.gcc:
	default:
		close(s.s.gcc)
	}
	s.s.mu.Unlock()

	s.srv.Stop()
	s.l.Close()
}

// maintenanceLoop periodically enforces time-based retention: change-stream
// retention, expired backups, and the undelete window for deleted tables.
func (s *server) maintenanceLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.gcc:
			return
		case <-ticker.C:
			s.runMaintenance(context.Background())
		}
	}
}

func (s *server) runMaintenance(ctx context.Context) {
	if err := s.purgeExpiredChangeRecords(ctx); err != nil {
		log.Printf("change stream retention: %v", err)
	}
	if err := s.purgeExpiredBackups(ctx); err != nil {
		log.Printf("backup expiration: %v", err)
	}
	if err := s.purgeExpiredTombstones(ctx); err != nil {
		log.Printf("deleted table retention: %v", err)
	}
}

func (s *server) loadTables(ctx context.Context) error {
	tables, err := s.tableBackend.GetAll(ctx)
	if err != nil {
		return err
	}
	for _, t := range tables {
		if t.deleted() {
			s.deletedTables[t.storageName()] = t
			continue
		}
		s.tables[t.name()] = t
	}
	return nil
}

// LoadTables restores persisted tables. It is kept for callers that construct
// state manually.
func (s *server) LoadTables() {
	if err := s.loadTables(context.Background()); err != nil {
		log.Printf("load tables: %v", err)
	}
}

func (s *server) LoadAdminMetadata() {
	if s.adminBackend == nil {
		return
	}
	for _, inst := range s.adminBackend.GetInstances() {
		s.instances[inst.Name] = inst
	}
	for _, cluster := range s.adminBackend.GetClusters() {
		s.clusters[cluster.Name] = cluster
	}
	for _, appProfile := range s.adminBackend.GetAppProfiles() {
		s.appProfiles[appProfile.Name] = appProfile
	}
}

// table is one Bigtable table's schema and row store.
type table struct {
	parent  string
	tableId string
	// storageId is the rows_t/tables_t key. It equals tableId for live
	// tables and carries a deletion suffix for undeletable tombstones.
	storageId string

	mu       sync.RWMutex
	counter  uint64                   // incremented when a family is created
	families map[string]*columnFamily // keyed by plain family name
	rows     *SqlRows

	backupPolicy       *btapb.Table_AutomatedBackupPolicy
	deletionProtection bool
	changeStream       *btapb.ChangeStreamConfig
	rowKeySchema       *btapb.Type_Struct
	tieredStorage      *btapb.TieredStorageConfig
	granularity        btapb.Table_TimestampGranularity
	restoreInfo        *btapb.RestoreInfo
	initialSplits      []string
	createTime         time.Time
	deleteTime         time.Time // non-zero for tombstones
}

func newTable(ctr *btapb.CreateTableRequest, db *sql.DB) *table {
	fams := make(map[string]*columnFamily)
	ids := make([]string, 0, len(ctr.GetTable().GetColumnFamilies()))
	for id := range ctr.GetTable().GetColumnFamilies() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	name := ctr.Parent + "/tables/" + ctr.TableId
	var c uint64
	for _, id := range ids {
		cf := ctr.Table.ColumnFamilies[id]
		fams[id] = &columnFamily{
			Name:      name + "/columnFamilies/" + id,
			Order:     c,
			GCRule:    cf.GetGcRule(),
			ValueType: cf.GetValueType(),
		}
		c++
	}
	var splits []string
	for _, s := range ctr.GetInitialSplits() {
		splits = append(splits, string(s.GetKey()))
	}
	sort.Strings(splits)
	t := &table{
		parent:        ctr.Parent,
		tableId:       ctr.TableId,
		storageId:     ctr.TableId,
		families:      fams,
		counter:       c,
		rows:          NewSqlRows(db, ctr.Parent, ctr.TableId),
		backupPolicy:  getAutomatedBackupPolicy(ctr.GetTable()),
		changeStream:  ctr.GetTable().GetChangeStreamConfig(),
		rowKeySchema:  ctr.GetTable().GetRowKeySchema(),
		tieredStorage: ctr.GetTable().GetTieredStorageConfig(),
		granularity:   ctr.GetTable().GetGranularity(),
		initialSplits: splits,
		createTime:    time.Now(),
	}
	t.deletionProtection = ctr.GetTable().GetDeletionProtection()
	if t.granularity == btapb.Table_TIMESTAMP_GRANULARITY_UNSPECIFIED {
		t.granularity = btapb.Table_MILLIS
	}
	return t
}

// cloneMeta copies the table's metadata (not its lock) so a candidate can be
// persisted before the live table is changed. Callers hold t.mu.
func (t *table) cloneMeta() *table {
	return &table{
		parent:             t.parent,
		tableId:            t.tableId,
		storageId:          t.storageId,
		counter:            t.counter,
		families:           t.families,
		rows:               t.rows,
		backupPolicy:       t.backupPolicy,
		deletionProtection: t.deletionProtection,
		changeStream:       t.changeStream,
		rowKeySchema:       t.rowKeySchema,
		tieredStorage:      t.tieredStorage,
		granularity:        t.granularity,
		restoreInfo:        t.restoreInfo,
		initialSplits:      t.initialSplits,
		createTime:         t.createTime,
		deleteTime:         t.deleteTime,
	}
}

func (t *table) name() string {
	return t.parent + "/tables/" + t.tableId
}

func (t *table) storageName() string {
	return t.parent + "/tables/" + t.storageId
}

func (t *table) deleted() bool {
	return !t.deleteTime.IsZero()
}

func (t *table) changeStreamEnabled() bool {
	return t.changeStream != nil && t.changeStream.GetRetentionPeriod().AsDuration() > 0
}

func (t *table) changeStreamRetention() time.Duration {
	if t.changeStream == nil {
		return 0
	}
	return t.changeStream.GetRetentionPeriod().AsDuration()
}

func (t *table) columnFamilies() map[string]*columnFamily {
	cp := make(map[string]*columnFamily)
	t.mu.RLock()
	for fam, cf := range t.families {
		cp[fam] = cf
	}
	t.mu.RUnlock()
	return cp
}

func (t *table) gcRules() map[string]*btapb.GcRule {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.gcRulesNoLock()
}

func (t *table) gcRulesNoLock() map[string]*btapb.GcRule {
	rules := make(map[string]*btapb.GcRule)
	for fam, cf := range t.families {
		if cf.GCRule != nil {
			rules[fam] = cf.GCRule
		}
	}
	if len(rules) == 0 {
		return nil
	}
	return rules
}

// schemaProto returns the table's schema fields. Callers hold t.mu.
func (t *table) schemaProtoNoLock() *btapb.Table {
	pb := &btapb.Table{
		Name:               t.name(),
		ColumnFamilies:     toColumnFamilies(t.families),
		Granularity:        t.granularity,
		DeletionProtection: t.deletionProtection,
		ChangeStreamConfig: t.changeStream,
		RowKeySchema:       t.rowKeySchema,
		RestoreInfo:        t.restoreInfo,
	}
	if pb.Granularity == btapb.Table_TIMESTAMP_GRANULARITY_UNSPECIFIED {
		pb.Granularity = btapb.Table_MILLIS
	}
	if t.backupPolicy != nil {
		pb.AutomatedBackupConfig = getAutomatedBackupConfig(t.backupPolicy)
		pb.EffectiveAutomatedBackupPolicy = effectiveBackupPolicy(t.backupPolicy)
	}
	pb.TieredStorageConfig = t.tieredStorage
	return pb
}

// effectiveBackupPolicy fills Bigtable's documented automated-backup
// defaults: 7-day retention and a 24-hour frequency.
func effectiveBackupPolicy(p *btapb.Table_AutomatedBackupPolicy) *btapb.Table_AutomatedBackupPolicy {
	if p == nil {
		return nil
	}
	out := &btapb.Table_AutomatedBackupPolicy{
		RetentionPeriod: p.GetRetentionPeriod(),
		Frequency:       p.GetFrequency(),
		Locations:       p.GetLocations(),
		KeepHotDuration: p.GetKeepHotDuration(),
		Disabled:        p.GetDisabled(),
	}
	if out.RetentionPeriod == nil {
		out.RetentionPeriod = durationpb.New(7 * 24 * time.Hour)
	}
	if out.Frequency == nil {
		out.Frequency = durationpb.New(24 * time.Hour)
	}
	return out
}

type byRowKey []*row

func (b byRowKey) Len() int           { return len(b) }
func (b byRowKey) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byRowKey) Less(i, j int) bool { return b[i].key < b[j].key }

type row struct {
	key string

	families map[string]*family // keyed by family name
}

func newRow(key string) *row {
	return &row{
		key:      key,
		families: make(map[string]*family),
	}
}

// copy returns a deep copy of the row's structure. Cell values are aliased
// because they are never modified in place.
func (r *row) copy() *row {
	nr := newRow(r.key)
	for _, fam := range r.families {
		nf := &family{
			Name:     fam.Name,
			Order:    fam.Order,
			ColNames: append([]string(nil), fam.ColNames...),
			Cells:    make(map[string][]cell, len(fam.Cells)),
		}
		for col, cs := range fam.Cells {
			nf.Cells[col] = append([]cell(nil), cs...)
		}
		nr.families[fam.Name] = nf
	}
	return nr
}

// isEmpty returns true if a row doesn't contain any cell
func (r *row) isEmpty() bool {
	for _, fam := range r.families {
		for _, cs := range fam.Cells {
			if len(cs) > 0 {
				return false
			}
		}
	}
	return true
}

func (r *row) cellCount() int {
	n := 0
	for _, fam := range r.families {
		for _, cs := range fam.Cells {
			n += len(cs)
		}
	}
	return n
}

// sortedFamilies returns the row's families in ascending creation order.
func (r *row) sortedFamilies() []*family {
	families := make([]*family, 0, len(r.families))
	for _, fam := range r.families {
		families = append(families, fam)
	}
	sort.Sort(byCreationOrder(families))
	return families
}

func (r *row) getOrCreateFamily(name string, order uint64) *family {
	if _, ok := r.families[name]; !ok {
		r.families[name] = &family{
			Name:  name,
			Order: order,
			Cells: make(map[string][]cell),
		}
	}
	return r.families[name]
}

// gc applies GC rules to the row and reports whether any cell was removed.
func (r *row) gc(rules map[string]*btapb.GcRule) bool {
	return len(r.gcWithChanges(rules)) > 0
}

// gcWithChanges applies GC rules and returns one DeleteFromColumn mutation per
// removed cell, the form in which change streams report garbage collection.
func (r *row) gcWithChanges(rules map[string]*btapb.GcRule) []*btpb.Mutation {
	if len(rules) == 0 {
		return nil
	}
	var changes []*btpb.Mutation
	now := time.Now().UnixMicro()
	for _, fam := range r.sortedFamilies() {
		rule, ok := rules[fam.Name]
		if !ok {
			continue
		}
		for _, col := range append([]string(nil), fam.ColNames...) {
			cs := fam.Cells[col]
			kept, removed := applyGCAt(cs, rule, now)
			if len(removed) == 0 {
				continue
			}
			fam.Cells[col] = kept
			for _, c := range removed {
				changes = append(changes, &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromColumn_{
					DeleteFromColumn: &btpb.Mutation_DeleteFromColumn{
						FamilyName:      fam.Name,
						ColumnQualifier: []byte(col),
						TimeRange:       &btpb.TimestampRange{StartTimestampMicros: c.Ts, EndTimestampMicros: c.Ts + 1000},
					},
				}})
			}
		}
	}
	r.normalize()
	return changes
}

// size returns the total size of all cell values in the row.
func (r *row) size() int {
	size := len(r.key)
	for _, fam := range r.families {
		for col, cells := range fam.Cells {
			for _, cell := range cells {
				size += len(cell.Value) + len(col)
			}
		}
	}
	return size
}

// btreeKey returns a key-only row.
func btreeKey(s string) *row { return &row{key: s} }

func (r *row) String() string {
	return r.key
}

func init() {
	// Legacy table metadata encoded GC rules with gob; these registrations keep
	// those records decodable for the one-time migration. See
	// https://github.com/bitly/little_bigtable/issues/24.
	gob.RegisterName("*admin.GcRule_Intersection_", &btapb.GcRule_Intersection_{})
	gob.RegisterName("*admin.GcRule_MaxNumVersions", &btapb.GcRule_MaxNumVersions{})
	gob.RegisterName("*admin.GcRule_Union", &btapb.GcRule_Union{})
	gob.RegisterName("*admin.GcRule_Union_", &btapb.GcRule_Union_{})
	gob.RegisterName("*admin.GcRule_MaxAge", &btapb.GcRule_MaxAge{})
}

// applyGC applies the given GC rule to cells ordered by descending
// timestamp. It returns the retained cells and whether any were removed.
func applyGC(cells []cell, rule *btapb.GcRule) ([]cell, bool) {
	kept, removed := applyGCAt(cells, rule, time.Now().UnixMicro())
	return kept, len(removed) > 0
}

func applyGCAt(cells []cell, rule *btapb.GcRule, nowMicros int64) (kept, removed []cell) {
	if len(cells) == 0 || rule == nil {
		return cells, nil
	}
	deleteFlags := gcDeletes(cells, rule, nowMicros)
	for i, c := range cells {
		if deleteFlags[i] {
			removed = append(removed, c)
		} else {
			kept = append(kept, c)
		}
	}
	if len(removed) == 0 {
		return cells, nil
	}
	return kept, removed
}

// gcDeletes reports, per cell, whether rule makes it eligible for garbage
// collection. Every child of a union or intersection judges the same input
// column: a union deletes a cell when any child deletes it, and an
// intersection deletes it only when every child deletes it.
func gcDeletes(cells []cell, rule *btapb.GcRule, nowMicros int64) []bool {
	flags := make([]bool, len(cells))
	switch r := rule.GetRule().(type) {
	case *btapb.GcRule_MaxNumVersions:
		for i := int(r.MaxNumVersions); i < len(cells); i++ {
			if i >= 0 {
				flags[i] = true
			}
		}
	case *btapb.GcRule_MaxAge:
		cutoff := nowMicros - r.MaxAge.AsDuration().Microseconds()
		for i, c := range cells {
			flags[i] = c.Ts < cutoff
		}
	case *btapb.GcRule_Union_:
		for _, sub := range r.Union.GetRules() {
			for i, del := range gcDeletes(cells, sub, nowMicros) {
				flags[i] = flags[i] || del
			}
		}
	case *btapb.GcRule_Intersection_:
		subs := r.Intersection.GetRules()
		if len(subs) == 0 {
			return flags
		}
		for i := range flags {
			flags[i] = true
		}
		for _, sub := range subs {
			for i, del := range gcDeletes(cells, sub, nowMicros) {
				flags[i] = flags[i] && del
			}
		}
	}
	return flags
}

type family struct {
	Name     string            // Column family name
	Order    uint64            // Creation order of column family
	ColNames []string          // Column names are sorted in lexicographical ascending order
	Cells    map[string][]cell // Keyed by column name; cells are in descending timestamp order
}

type byCreationOrder []*family

func (b byCreationOrder) Len() int      { return len(b) }
func (b byCreationOrder) Swap(i, j int) { b[i], b[j] = b[j], b[i] }
func (b byCreationOrder) Less(i, j int) bool {
	if b[i].Order != b[j].Order {
		return b[i].Order < b[j].Order
	}
	return b[i].Name < b[j].Name
}

// cellsByColumn adds the column name to colNames set if it does not exist
// and returns all cells within a column
func (f *family) cellsByColumn(name string) []cell {
	if _, ok := f.Cells[name]; !ok {
		f.ColNames = append(f.ColNames, name)
		sort.Strings(f.ColNames)
	}
	return f.Cells[name]
}

type cell struct {
	Ts     int64
	Value  []byte
	Labels []string
}

type byDescTS []cell

func (b byDescTS) Len() int           { return len(b) }
func (b byDescTS) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byDescTS) Less(i, j int) bool { return b[i].Ts > b[j].Ts }

type columnFamily struct {
	Name      string
	Order     uint64 // Creation order of column family
	GCRule    *btapb.GcRule
	ValueType *btapb.Type
}

func (c *columnFamily) proto() *btapb.ColumnFamily {
	return &btapb.ColumnFamily{
		GcRule:    c.GCRule,
		ValueType: c.ValueType,
	}
}

func toColumnFamilies(families map[string]*columnFamily) map[string]*btapb.ColumnFamily {
	fs := make(map[string]*btapb.ColumnFamily)
	for k, v := range families {
		fs[k] = v.proto()
	}
	return fs
}

func getAutomatedBackupConfig(policy *btapb.Table_AutomatedBackupPolicy) *btapb.Table_AutomatedBackupPolicy_ {
	if policy == nil {
		return nil
	}
	return &btapb.Table_AutomatedBackupPolicy_{
		AutomatedBackupPolicy: policy,
	}
}

func getAutomatedBackupPolicy(bTable *btapb.Table) *btapb.Table_AutomatedBackupPolicy {
	if bTable != nil && bTable.AutomatedBackupConfig != nil {
		if policy, ok := bTable.AutomatedBackupConfig.(*btapb.Table_AutomatedBackupPolicy_); ok {
			return policy.AutomatedBackupPolicy
		}
	}
	return nil
}

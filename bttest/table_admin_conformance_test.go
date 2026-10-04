package bttest

// Table admin conformance tests. Expected behavior follows the official
// BigtableTableAdmin protos (google/bigtable/admin/v2) and the managing-tables,
// row-key-schema and undelete documentation.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const (
	ctaProject  = "cta-project"
	ctaInstance = "cta-instance"
	ctaParent   = "projects/" + ctaProject + "/instances/" + ctaInstance
)

// ctaOpen starts a gRPC emulator with the official clients on dbFile. Opening
// it again on the same file simulates a process restart.
func ctaOpen(t *testing.T, ctx context.Context, dbFile string) *storageConformanceHarness {
	t.Helper()
	prevDialect, prevStrict := currentDialect(), isStrictAdmin()
	ConfigureStorage("sqlite3", false)
	t.Cleanup(func() { ConfigureStorage(string(prevDialect), prevStrict) })
	return openStorageConformanceHarness(t, ctx, storageConformanceBackend{
		name:      "sqlite",
		dialect:   "sqlite3",
		sqlDriver: "sqlite3",
		dsn:       "file:" + dbFile + "?cache=shared",
	}, ctaProject, ctaInstance)
}

// ctaOpenState loads emulator state from dbFile without serving gRPC.
func ctaOpenState(t *testing.T, dbFile string) (*server, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared", dbFile))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	require.NoError(t, CreateTables(ctx, db))
	s := newServerState(db)
	require.NoError(t, s.load(ctx))
	return s, func() { _ = db.Close() }
}

func ctaRequireCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, want, status.Code(err), "unexpected error: %v", err)
}

func ctaCreateTable(t *testing.T, s *server, id string, families ...string) string {
	t.Helper()
	fams := map[string]*btapb.ColumnFamily{}
	for _, f := range families {
		fams[f] = &btapb.ColumnFamily{}
	}
	_, err := s.CreateTable(context.Background(), &btapb.CreateTableRequest{
		Parent: ctaParent, TableId: id, Table: &btapb.Table{ColumnFamilies: fams},
	})
	require.NoError(t, err)
	return ctaParent + "/tables/" + id
}

func ctaWrite(t *testing.T, s *server, tableName, key, fam, qual, val string) {
	t.Helper()
	_, err := s.MutateRow(context.Background(), &btpb.MutateRowRequest{
		TableName: tableName,
		RowKey:    []byte(key),
		Mutations: []*btpb.Mutation{{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
			FamilyName: fam, ColumnQualifier: []byte(qual), TimestampMicros: 1000, Value: []byte(val),
		}}}},
	})
	require.NoError(t, err)
}

func ctaTable(t *testing.T, s *server, name string) *table {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	tbl := s.tables[name]
	require.NotNil(t, tbl, "table %s not loaded", name)
	return tbl
}

func ctaUpdate(ctx context.Context, s *server, tbl *btapb.Table, ignoreWarnings bool, paths ...string) (*longrunningpb.Operation, error) {
	return s.UpdateTable(ctx, &btapb.UpdateTableRequest{
		Table: tbl, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}, IgnoreWarnings: ignoreWarnings,
	})
}

func ctaReadKeys(t *testing.T, ctx context.Context, tbl *bigtable.Table) []string {
	t.Helper()
	var keys []string
	require.NoError(t, tbl.ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		keys = append(keys, r.Key())
		return true
	}))
	return keys
}

func ctaStringType() *btapb.Type {
	return &btapb.Type{Kind: &btapb.Type_StringType{StringType: &btapb.Type_String{
		Encoding: &btapb.Type_String_Encoding{Encoding: &btapb.Type_String_Encoding_Utf8Bytes_{Utf8Bytes: &btapb.Type_String_Encoding_Utf8Bytes{}}},
	}}}
}

func ctaRowKeySchema(fields ...string) *btapb.Type_Struct {
	st := &btapb.Type_Struct{Encoding: &btapb.Type_Struct_Encoding{Encoding: &btapb.Type_Struct_Encoding_DelimitedBytes_{
		DelimitedBytes: &btapb.Type_Struct_Encoding_DelimitedBytes{Delimiter: []byte("#")},
	}}}
	for _, f := range fields {
		st.Fields = append(st.Fields, &btapb.Type_Struct_Field{FieldName: f, Type: ctaStringType()})
	}
	return st
}

func TestConformanceTableAdminCreateTableValidation(t *testing.T) {
	s, _ := newFullTestServer(t)
	ctx := context.Background()

	for _, id := range []string{"", "-leading-hyphen", ".leading-dot", "has space", "slash/id", strings.Repeat("a", 51)} {
		_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: ctaParent, TableId: id})
		ctaRequireCode(t, err, codes.InvalidArgument)
	}
	for _, id := range []string{strings.Repeat("a", 50), "_ok.id-1", "A9"} {
		_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: ctaParent, TableId: id})
		require.NoError(t, err, id)
	}
	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: ctaParent, TableId: "A9"})
	ctaRequireCode(t, err, codes.AlreadyExists)
	_, err = s.CreateTable(ctx, &btapb.CreateTableRequest{TableId: "no-parent"})
	ctaRequireCode(t, err, codes.InvalidArgument)

	_, err = s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: ctaParent, TableId: "badfam",
		Table: &btapb.Table{ColumnFamilies: map[string]*btapb.ColumnFamily{"bad/fam": {}}}})
	ctaRequireCode(t, err, codes.InvalidArgument)
	_, err = s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: ctaParent, TableId: "badsplit",
		InitialSplits: []*btapb.CreateTableRequest_Split{{Key: []byte("a")}, {Key: nil}}})
	ctaRequireCode(t, err, codes.InvalidArgument)
	_, err = s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: ctaParent, TableId: "badcs",
		Table: &btapb.Table{ChangeStreamConfig: &btapb.ChangeStreamConfig{RetentionPeriod: durationpb.New(time.Hour)}}})
	ctaRequireCode(t, err, codes.InvalidArgument)
	for _, id := range []string{"badfam", "badsplit", "badcs"} {
		_, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: ctaParent + "/tables/" + id})
		ctaRequireCode(t, err, codes.NotFound)
	}

	got, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: ctaParent, TableId: "defaults",
		Table: &btapb.Table{ColumnFamilies: map[string]*btapb.ColumnFamily{"cf": {}}}})
	require.NoError(t, err)
	assert.Equal(t, ctaParent+"/tables/defaults", got.GetName())
	assert.Equal(t, btapb.Table_MILLIS, got.GetGranularity())
	assert.Contains(t, got.GetColumnFamilies(), "cf")
}

func TestConformanceTableAdminGetTableViews(t *testing.T) {
	s, _ := newFullTestServer(t)
	ctx := context.Background()
	_, err := s.CreateInstance(ctx, &btapb.CreateInstanceRequest{
		Parent: "projects/" + ctaProject, InstanceId: ctaInstance,
		Instance: &btapb.Instance{DisplayName: "Table admin conf"},
		Clusters: map[string]*btapb.Cluster{"cta-cluster1": {ServeNodes: 1}},
	})
	require.NoError(t, err)
	_, err = s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: ctaParent, TableId: "views", Table: &btapb.Table{
		ColumnFamilies: map[string]*btapb.ColumnFamily{
			"cf": {GcRule: &btapb.GcRule{Rule: &btapb.GcRule_MaxNumVersions{MaxNumVersions: 2}}},
		},
		DeletionProtection: true,
	}})
	require.NoError(t, err)
	name := ctaParent + "/tables/views"

	get := func(view btapb.Table_View) *btapb.Table {
		t.Helper()
		tbl, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: name, View: view})
		require.NoError(t, err)
		require.Equal(t, name, tbl.GetName())
		return tbl
	}

	// Default is SCHEMA_VIEW: schema fields, no cluster states.
	for _, view := range []btapb.Table_View{btapb.Table_VIEW_UNSPECIFIED, btapb.Table_SCHEMA_VIEW} {
		tbl := get(view)
		require.Contains(t, tbl.GetColumnFamilies(), "cf")
		assert.Equal(t, int32(2), tbl.GetColumnFamilies()["cf"].GetGcRule().GetMaxNumVersions())
		assert.Equal(t, btapb.Table_MILLIS, tbl.GetGranularity())
		assert.True(t, tbl.GetDeletionProtection())
		assert.Empty(t, tbl.GetClusterStates())
	}

	nameOnly := get(btapb.Table_NAME_ONLY)
	assert.True(t, proto.Equal(&btapb.Table{Name: name}, nameOnly), "NAME_ONLY returned %v", nameOnly)

	repl := get(btapb.Table_REPLICATION_VIEW)
	assert.Empty(t, repl.GetColumnFamilies())
	require.Contains(t, repl.GetClusterStates(), "cta-cluster1")
	assert.Equal(t, btapb.Table_ClusterState_READY, repl.GetClusterStates()["cta-cluster1"].GetReplicationState())
	assert.Empty(t, repl.GetClusterStates()["cta-cluster1"].GetEncryptionInfo())

	enc := get(btapb.Table_ENCRYPTION_VIEW)
	assert.Empty(t, enc.GetColumnFamilies())
	require.Contains(t, enc.GetClusterStates(), "cta-cluster1")
	require.Len(t, enc.GetClusterStates()["cta-cluster1"].GetEncryptionInfo(), 1)
	assert.Equal(t, btapb.EncryptionInfo_GOOGLE_DEFAULT_ENCRYPTION, enc.GetClusterStates()["cta-cluster1"].GetEncryptionInfo()[0].GetEncryptionType())

	full := get(btapb.Table_FULL)
	assert.Contains(t, full.GetColumnFamilies(), "cf")
	assert.True(t, full.GetDeletionProtection())
	require.Contains(t, full.GetClusterStates(), "cta-cluster1")
	assert.Equal(t, btapb.Table_ClusterState_READY, full.GetClusterStates()["cta-cluster1"].GetReplicationState())
	assert.NotEmpty(t, full.GetClusterStates()["cta-cluster1"].GetEncryptionInfo())

	_, err = s.GetTable(ctx, &btapb.GetTableRequest{Name: ctaParent + "/tables/missing"})
	ctaRequireCode(t, err, codes.NotFound)
}

func TestConformanceTableAdminListTablesViewsAndPagination(t *testing.T) {
	s, _ := newFullTestServer(t)
	ctx := context.Background()
	var want []string
	for i := 0; i < 5; i++ {
		want = append(want, ctaCreateTable(t, s, fmt.Sprintf("t%d", i), "cf"))
	}
	// Tables of another instance are not listed.
	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: "projects/other/instances/other-inst", TableId: "t0"})
	require.NoError(t, err)

	// Default view is NAME_ONLY.
	resp, err := s.ListTables(ctx, &btapb.ListTablesRequest{Parent: ctaParent})
	require.NoError(t, err)
	require.Len(t, resp.GetTables(), 5)
	assert.Empty(t, resp.GetNextPageToken())
	for i, tbl := range resp.GetTables() {
		assert.True(t, proto.Equal(&btapb.Table{Name: want[i]}, tbl), "NAME_ONLY list returned %v", tbl)
	}
	resp, err = s.ListTables(ctx, &btapb.ListTablesRequest{Parent: ctaParent, View: btapb.Table_SCHEMA_VIEW})
	require.NoError(t, err)
	for _, tbl := range resp.GetTables() {
		assert.Contains(t, tbl.GetColumnFamilies(), "cf")
	}

	// Pages are ordered by name and cover every table exactly once.
	var got []string
	token := ""
	pages := 0
	for {
		resp, err := s.ListTables(ctx, &btapb.ListTablesRequest{Parent: ctaParent, PageSize: 2, PageToken: token})
		require.NoError(t, err)
		require.LessOrEqual(t, len(resp.GetTables()), 2)
		for _, tbl := range resp.GetTables() {
			got = append(got, tbl.GetName())
		}
		pages++
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
	}
	assert.Equal(t, 3, pages)
	assert.Equal(t, want, got)

	// A page token stays valid and deterministic when the last returned
	// table is deleted before the next page is requested.
	first, err := s.ListTables(ctx, &btapb.ListTablesRequest{Parent: ctaParent, PageSize: 2})
	require.NoError(t, err)
	require.NotEmpty(t, first.GetNextPageToken())
	_, err = s.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: want[1]})
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		second, err := s.ListTables(ctx, &btapb.ListTablesRequest{Parent: ctaParent, PageSize: 2, PageToken: first.GetNextPageToken()})
		require.NoError(t, err)
		require.Len(t, second.GetTables(), 2)
		assert.Equal(t, want[2], second.GetTables()[0].GetName())
		assert.Equal(t, want[3], second.GetTables()[1].GetName())
	}

	_, err = s.ListTables(ctx, &btapb.ListTablesRequest{Parent: ctaParent, PageToken: "!!not-a-token!!"})
	ctaRequireCode(t, err, codes.InvalidArgument)
	_, err = s.ListTables(ctx, &btapb.ListTablesRequest{Parent: ctaParent, PageSize: -1})
	ctaRequireCode(t, err, codes.InvalidArgument)
	_, err = s.ListTables(ctx, &btapb.ListTablesRequest{})
	ctaRequireCode(t, err, codes.InvalidArgument)

	// With strict admin checks the parent instance must exist.
	prevDialect, prevStrict := currentDialect(), isStrictAdmin()
	ConfigureStorage(string(prevDialect), true)
	defer ConfigureStorage(string(prevDialect), prevStrict)
	_, err = s.ListTables(ctx, &btapb.ListTablesRequest{Parent: "projects/nope/instances/missing-inst"})
	ctaRequireCode(t, err, codes.NotFound)
}

func TestConformanceTableAdminUpdateTableMasks(t *testing.T) {
	s, _ := newFullTestServer(t)
	ctx := context.Background()
	name := ctaCreateTable(t, s, "upd", "cf")
	tbl := func() *btapb.Table {
		t.Helper()
		got, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
		require.NoError(t, err)
		return got
	}

	// Mask validation happens before anything is applied.
	_, err := ctaUpdate(ctx, s, &btapb.Table{Name: name, DeletionProtection: true}, false)
	ctaRequireCode(t, err, codes.InvalidArgument)
	_, err = s.UpdateTable(ctx, &btapb.UpdateTableRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}}})
	ctaRequireCode(t, err, codes.InvalidArgument)
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name, DeletionProtection: true}, false, "*")
	ctaRequireCode(t, err, codes.InvalidArgument)
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name, DeletionProtection: true}, false, "deletion_protection", "*")
	ctaRequireCode(t, err, codes.InvalidArgument)
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name}, false, "column_families")
	ctaRequireCode(t, err, codes.Unimplemented)
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name, Granularity: btapb.Table_MILLIS}, false, "granularity")
	ctaRequireCode(t, err, codes.InvalidArgument)
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: ctaParent + "/tables/missing", DeletionProtection: true}, false, "deletion_protection")
	ctaRequireCode(t, err, codes.NotFound)
	assert.False(t, tbl().GetDeletionProtection())

	// deletion_protection; the LRO is complete and carries UpdateTableMetadata.
	op, err := ctaUpdate(ctx, s, &btapb.Table{Name: name, DeletionProtection: true}, false, "deletion_protection")
	require.NoError(t, err)
	require.True(t, op.GetDone())
	assert.True(t, strings.HasPrefix(op.GetName(), "operations/"), op.GetName())
	md := &btapb.UpdateTableMetadata{}
	require.NoError(t, op.GetMetadata().UnmarshalTo(md))
	assert.Equal(t, name, md.GetName())
	require.NotNil(t, md.GetStartTime())
	require.NotNil(t, md.GetEndTime())
	assert.False(t, md.GetEndTime().AsTime().Before(md.GetStartTime().AsTime()))
	resp := &btapb.Table{}
	require.NoError(t, op.GetResponse().UnmarshalTo(resp))
	assert.Equal(t, name, resp.GetName())
	assert.True(t, resp.GetDeletionProtection())
	assert.True(t, tbl().GetDeletionProtection())
	_, err = s.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: name})
	ctaRequireCode(t, err, codes.FailedPrecondition)
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name}, false, "deletion_protection")
	require.NoError(t, err)
	assert.False(t, tbl().GetDeletionProtection())

	// change_stream_config.retention_period enables, change_stream_config
	// with no config disables; retention must be 1-7 days.
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name, ChangeStreamConfig: &btapb.ChangeStreamConfig{RetentionPeriod: durationpb.New(48 * time.Hour)}}, false, "change_stream_config.retention_period")
	require.NoError(t, err)
	assert.Equal(t, 48*time.Hour, tbl().GetChangeStreamConfig().GetRetentionPeriod().AsDuration())
	for _, bad := range []time.Duration{time.Hour, 8 * 24 * time.Hour} {
		_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name, ChangeStreamConfig: &btapb.ChangeStreamConfig{RetentionPeriod: durationpb.New(bad)}}, false, "change_stream_config")
		ctaRequireCode(t, err, codes.InvalidArgument)
	}
	assert.Equal(t, 48*time.Hour, tbl().GetChangeStreamConfig().GetRetentionPeriod().AsDuration())
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name, ChangeStreamConfig: &btapb.ChangeStreamConfig{RetentionPeriod: durationpb.New(24 * time.Hour)}}, false, "change_stream_config")
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, tbl().GetChangeStreamConfig().GetRetentionPeriod().AsDuration())
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name}, false, "change_stream_config")
	require.NoError(t, err)
	assert.Nil(t, tbl().GetChangeStreamConfig())

	// automated_backup_policy replaces the policy; sub-paths merge.
	policy := func(p *btapb.Table_AutomatedBackupPolicy) *btapb.Table {
		return &btapb.Table{Name: name, AutomatedBackupConfig: &btapb.Table_AutomatedBackupPolicy_{AutomatedBackupPolicy: p}}
	}
	_, err = ctaUpdate(ctx, s, policy(&btapb.Table_AutomatedBackupPolicy{RetentionPeriod: durationpb.New(72 * time.Hour)}), false, "automated_backup_policy")
	require.NoError(t, err)
	got := tbl()
	assert.Equal(t, 72*time.Hour, got.GetAutomatedBackupPolicy().GetRetentionPeriod().AsDuration())
	assert.Nil(t, got.GetAutomatedBackupPolicy().GetFrequency())
	assert.Equal(t, 24*time.Hour, got.GetEffectiveAutomatedBackupPolicy().GetFrequency().AsDuration(), "effective policy defaults frequency to 24h")
	_, err = ctaUpdate(ctx, s, policy(&btapb.Table_AutomatedBackupPolicy{
		RetentionPeriod: durationpb.New(5 * 24 * time.Hour),
		Frequency:       durationpb.New(24 * time.Hour),
		Locations:       []string{"projects/p/locations/us-east1-b"},
		KeepHotDuration: durationpb.New(48 * time.Hour),
		Disabled:        true,
	}), false, "automated_backup_policy.retention_period")
	require.NoError(t, err)
	got = tbl()
	assert.Equal(t, 5*24*time.Hour, got.GetAutomatedBackupPolicy().GetRetentionPeriod().AsDuration())
	assert.Nil(t, got.GetAutomatedBackupPolicy().GetFrequency(), "only the masked sub-field changes")
	assert.Empty(t, got.GetAutomatedBackupPolicy().GetLocations())
	assert.False(t, got.GetAutomatedBackupPolicy().GetDisabled())
	for _, path := range []string{"automated_backup_policy.frequency", "automated_backup_policy.locations", "automated_backup_policy.keep_hot_duration", "automated_backup_policy.disabled"} {
		_, err = ctaUpdate(ctx, s, policy(&btapb.Table_AutomatedBackupPolicy{
			Frequency:       durationpb.New(24 * time.Hour),
			Locations:       []string{"projects/p/locations/us-east1-b"},
			KeepHotDuration: durationpb.New(48 * time.Hour),
			Disabled:        true,
		}), false, path)
		require.NoError(t, err, path)
	}
	got = tbl()
	p := got.GetAutomatedBackupPolicy()
	assert.Equal(t, 5*24*time.Hour, p.GetRetentionPeriod().AsDuration())
	assert.Equal(t, 24*time.Hour, p.GetFrequency().AsDuration())
	assert.Equal(t, []string{"projects/p/locations/us-east1-b"}, p.GetLocations())
	assert.Equal(t, 48*time.Hour, p.GetKeepHotDuration().AsDuration())
	assert.True(t, p.GetDisabled())
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name}, false, "automated_backup_policy")
	require.NoError(t, err)
	assert.Nil(t, tbl().GetAutomatedBackupConfig())

	// tiered_storage_config set and removed.
	tiered := &btapb.TieredStorageConfig{InfrequentAccess: &btapb.TieredStorageRule{
		Rule: &btapb.TieredStorageRule_IncludeIfOlderThan{IncludeIfOlderThan: durationpb.New(30 * 24 * time.Hour)},
	}}
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name, TieredStorageConfig: tiered}, false, "tiered_storage_config")
	require.NoError(t, err)
	assert.True(t, proto.Equal(tiered, tbl().GetTieredStorageConfig()))
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name}, false, "tiered_storage_config")
	require.NoError(t, err)
	assert.Nil(t, tbl().GetTieredStorageConfig())

	// Several paths in one request apply together.
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name, DeletionProtection: true,
		ChangeStreamConfig: &btapb.ChangeStreamConfig{RetentionPeriod: durationpb.New(24 * time.Hour)}},
		false, "deletion_protection", "change_stream_config")
	require.NoError(t, err)
	got = tbl()
	assert.True(t, got.GetDeletionProtection())
	assert.NotNil(t, got.GetChangeStreamConfig())
}

func TestConformanceTableAdminUpdateTablePersistsAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dbFile := newDBFile(t)
	const tableID = "persisted"
	name := ctaParent + "/tables/" + tableID
	rowKeySchema := bigtable.StructType{
		Fields:   []bigtable.StructField{{FieldName: "user", FieldType: bigtable.StringType{Encoding: bigtable.StringUtf8BytesEncoding{}}}},
		Encoding: bigtable.StructDelimitedBytesEncoding{Delimiter: []byte("#")},
	}

	h1 := ctaOpen(t, ctx, dbFile)
	require.NoError(t, h1.admin.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: tableID,
		ColumnFamilies: map[string]bigtable.Family{"cf": {GCPolicy: bigtable.MaxVersionsPolicy(3)}}}))
	require.NoError(t, h1.admin.UpdateTableWithDeletionProtection(ctx, tableID, bigtable.Protected))
	require.NoError(t, h1.admin.UpdateTableWithChangeStream(ctx, tableID, 48*time.Hour))
	require.NoError(t, h1.admin.UpdateTableWithAutomatedBackupPolicy(ctx, tableID, bigtable.TableAutomatedBackupPolicy{
		RetentionPeriod: 5 * 24 * time.Hour, Frequency: 24 * time.Hour,
	}))
	require.NoError(t, h1.admin.UpdateTableWithAutomatedBackupPolicy(ctx, tableID, bigtable.TableAutomatedBackupPolicy{
		KeepHotDuration: 48 * time.Hour,
	}))
	require.NoError(t, h1.admin.UpdateTableWithRowKeySchema(ctx, tableID, rowKeySchema))
	require.NoError(t, h1.admin.UpdateTableWithTieredStorageConfig(ctx, tableID, &bigtable.TieredStorageConfig{
		InfrequentAccess: &bigtable.TieredStorageIncludeIfOlderThan{Duration: 30 * 24 * time.Hour},
	}))
	before, err := h1.server.s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)
	h1.close()

	h2 := ctaOpen(t, ctx, dbFile)
	after, err := h2.server.s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)
	assert.True(t, proto.Equal(before, after), "schema changed across restart:\nbefore %v\nafter  %v", before, after)
	assert.True(t, after.GetDeletionProtection())
	assert.Equal(t, 48*time.Hour, after.GetChangeStreamConfig().GetRetentionPeriod().AsDuration())
	assert.Equal(t, 5*24*time.Hour, after.GetAutomatedBackupPolicy().GetRetentionPeriod().AsDuration())
	assert.Equal(t, 24*time.Hour, after.GetAutomatedBackupPolicy().GetFrequency().AsDuration())
	assert.Equal(t, 48*time.Hour, after.GetAutomatedBackupPolicy().GetKeepHotDuration().AsDuration())
	require.Len(t, after.GetRowKeySchema().GetFields(), 1)
	assert.Equal(t, "user", after.GetRowKeySchema().GetFields()[0].GetFieldName())
	assert.Equal(t, 30*24*time.Hour, after.GetTieredStorageConfig().GetInfrequentAccess().GetIncludeIfOlderThan().AsDuration())
	info, err := h2.admin.TableInfo(ctx, tableID)
	require.NoError(t, err)
	assert.Equal(t, bigtable.Protected, info.DeletionProtection)
	assert.Equal(t, "versions() > 3", info.FamilyInfos[0].GCPolicy)
	require.Error(t, h2.admin.DeleteTable(ctx, tableID), "deletion protection survives restart")

	// Remove or disable everything through the client, then restart again.
	require.NoError(t, h2.admin.UpdateTableWithDeletionProtection(ctx, tableID, bigtable.Unprotected))
	require.NoError(t, h2.admin.UpdateTableDisableChangeStream(ctx, tableID))
	require.NoError(t, h2.admin.UpdateTableWithAutomatedBackupPolicy(ctx, tableID, bigtable.TableAutomatedBackupPolicy{Disabled: true}))
	require.NoError(t, h2.admin.UpdateTableRemoveRowKeySchema(ctx, tableID))
	require.NoError(t, h2.admin.UpdateTableRemoveTieredStorageConfig(ctx, tableID))
	h2.close()

	h3 := ctaOpen(t, ctx, dbFile)
	defer h3.close()
	final, err := h3.server.s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)
	assert.False(t, final.GetDeletionProtection())
	assert.Nil(t, final.GetChangeStreamConfig())
	assert.True(t, final.GetAutomatedBackupPolicy().GetDisabled())
	assert.Nil(t, final.GetRowKeySchema())
	assert.Nil(t, final.GetTieredStorageConfig())
	require.NoError(t, h3.admin.UpdateTableDisableAutomatedBackupPolicy(ctx, tableID))
	final, err = h3.server.s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)
	assert.Nil(t, final.GetAutomatedBackupConfig())
	require.NoError(t, h3.admin.DeleteTable(ctx, tableID))
}

func TestConformanceTableAdminRowKeySchemaRules(t *testing.T) {
	s, _ := newFullTestServer(t)
	ctx := context.Background()
	name := ctaCreateTable(t, s, "rks", "cf")
	set := func(schema *btapb.Type_Struct, ignore bool) error {
		_, err := ctaUpdate(ctx, s, &btapb.Table{Name: name, RowKeySchema: schema}, ignore, "row_key_schema")
		return err
	}
	current := func() *btapb.Type_Struct {
		got, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
		require.NoError(t, err)
		return got.GetRowKeySchema()
	}

	// Invalid schemas: collisions with families or reserved columns,
	// duplicate or unnamed fields, missing types, no fields.
	for desc, schema := range map[string]*btapb.Type_Struct{
		"family collision": ctaRowKeySchema("cf"),
		"_key":             ctaRowKeySchema("_key"),
		"_timestamp":       ctaRowKeySchema("_timestamp"),
		"duplicate":        ctaRowKeySchema("a", "a"),
		"unnamed":          ctaRowKeySchema(""),
		"no fields":        ctaRowKeySchema(),
		"untyped":          {Fields: []*btapb.Type_Struct_Field{{FieldName: "a"}}},
	} {
		ctaRequireCode(t, set(schema, true), codes.InvalidArgument)
		assert.Nil(t, current(), desc)
	}

	one := ctaRowKeySchema("user")
	two := ctaRowKeySchema("user", "ts")
	require.NoError(t, set(one, false))
	assert.True(t, proto.Equal(one, current()))
	require.NoError(t, set(one, false), "setting the identical schema is a no-op")

	// A schema cannot be modified in place, even with ignore_warnings.
	ctaRequireCode(t, set(two, false), codes.InvalidArgument)
	ctaRequireCode(t, set(two, true), codes.InvalidArgument)
	assert.True(t, proto.Equal(one, current()))

	// A new family may not collide with a schema field.
	_, err := s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{Name: name, Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{{
		Id: "user", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}},
	}}})
	ctaRequireCode(t, err, codes.InvalidArgument)

	// Removal is backward incompatible and needs ignore_warnings.
	ctaRequireCode(t, set(nil, false), codes.InvalidArgument)
	assert.NotNil(t, current())
	require.NoError(t, set(nil, true))
	assert.Nil(t, current())
	require.NoError(t, set(two, false), "a new schema may be set after removal")
	assert.True(t, proto.Equal(two, current()))

	// CreateTable validates the schema against the requested families.
	_, err = s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: ctaParent, TableId: "rks2", Table: &btapb.Table{
		ColumnFamilies: map[string]*btapb.ColumnFamily{"user": {}},
		RowKeySchema:   ctaRowKeySchema("user"),
	}})
	ctaRequireCode(t, err, codes.InvalidArgument)
	_, err = s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: ctaParent, TableId: "rks2", Table: &btapb.Table{
		ColumnFamilies: map[string]*btapb.ColumnFamily{"cf": {}},
		RowKeySchema:   ctaRowKeySchema("user"),
	}})
	require.NoError(t, err)
}

func TestConformanceTableAdminLegacyMetadataMigration(t *testing.T) {
	ctx := context.Background()
	dbFile := newDBFile(t)
	const tableID = "legacy"
	name := ctaParent + "/tables/" + tableID
	union := &btapb.GcRule{Rule: &btapb.GcRule_Union_{Union: &btapb.GcRule_Union{Rules: []*btapb.GcRule{
		{Rule: &btapb.GcRule_MaxAge{MaxAge: durationpb.New(time.Hour)}},
		{Rule: &btapb.GcRule_MaxNumVersions{MaxNumVersions: 1}},
	}}}}

	// Write a pre-migration record: a bare gob-encoded family map.
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared", dbFile))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	require.NoError(t, CreateTables(ctx, db))
	var buf bytes.Buffer
	require.NoError(t, gob.NewEncoder(&buf).Encode(map[string]*legacyColumnFamily{
		"cf1": {Name: name + "/columnFamilies/cf1", Order: 0, GCRule: &btapb.GcRule{Rule: &btapb.GcRule_MaxNumVersions{MaxNumVersions: 2}}},
		"cf2": {Name: name + "/columnFamilies/cf2", Order: 3, GCRule: union},
		"cf3": {Name: name + "/columnFamilies/cf3", Order: 1},
	}))
	_, err = db.ExecContext(ctx, "INSERT INTO tables_t (parent, table_id, metadata) VALUES (?, ?, ?)", ctaParent, tableID, buf.Bytes())
	require.NoError(t, err)
	require.NoError(t, db.Close())

	s, closeDB := ctaOpenState(t, dbFile)
	got, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)
	require.Len(t, got.GetColumnFamilies(), 3)
	assert.Equal(t, int32(2), got.GetColumnFamilies()["cf1"].GetGcRule().GetMaxNumVersions())
	assert.True(t, proto.Equal(union, got.GetColumnFamilies()["cf2"].GetGcRule()), "union rule: %v", got.GetColumnFamilies()["cf2"].GetGcRule())
	assert.Nil(t, got.GetColumnFamilies()["cf3"].GetGcRule())
	assert.Equal(t, btapb.Table_MILLIS, got.GetGranularity())

	// The record was rewritten in the current encoding.
	var raw []byte
	require.NoError(t, s.db.QueryRowContext(ctx, "SELECT metadata FROM tables_t WHERE parent = ? AND table_id = ?", ctaParent, tableID).Scan(&raw))
	assert.True(t, bytes.HasPrefix(raw, tableMetadataMagic), "metadata not migrated: %q", raw[:min(len(raw), 8)])

	// Creation order continues after the highest legacy order.
	tbl := ctaTable(t, s, name)
	assert.Equal(t, uint64(4), tbl.counter)
	_, err = s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{Name: name, Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{{
		Id: "cf4", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}},
	}}})
	require.NoError(t, err)
	assert.Equal(t, uint64(4), tbl.columnFamilies()["cf4"].Order)
	ctaWrite(t, s, name, "r1", "cf3", "q", "v")
	closeDB()

	// The migrated record loads again unchanged.
	s2, closeDB2 := ctaOpenState(t, dbFile)
	defer closeDB2()
	got2, err := s2.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)
	require.Len(t, got2.GetColumnFamilies(), 4)
	assert.True(t, proto.Equal(union, got2.GetColumnFamilies()["cf2"].GetGcRule()))
	assert.Equal(t, uint64(4), ctaTable(t, s2, name).columnFamilies()["cf4"].Order)
	r := storedRow(t, ctaTable(t, s2, name), "r1")
	require.NotNil(t, r)
	assert.Contains(t, r.families, "cf3")
}

func TestConformanceTableAdminModifyColumnFamiliesAtomic(t *testing.T) {
	s, _ := newFullTestServer(t)
	ctx := context.Background()
	name := ctaCreateTable(t, s, "mcf", "cf1", "cf2")
	ctaWrite(t, s, name, "r1", "cf2", "q", "keep")
	before, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)

	create := func(id string, cf *btapb.ColumnFamily) *btapb.ModifyColumnFamiliesRequest_Modification {
		return &btapb.ModifyColumnFamiliesRequest_Modification{Id: id, Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: cf}}
	}
	update := func(id string, cf *btapb.ColumnFamily, paths ...string) *btapb.ModifyColumnFamiliesRequest_Modification {
		m := &btapb.ModifyColumnFamiliesRequest_Modification{Id: id, Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Update{Update: cf}}
		if len(paths) > 0 {
			m.UpdateMask = &fieldmaskpb.FieldMask{Paths: paths}
		}
		return m
	}
	drop := func(id string) *btapb.ModifyColumnFamiliesRequest_Modification {
		return &btapb.ModifyColumnFamiliesRequest_Modification{Id: id, Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Drop{Drop: true}}
	}
	versions := func(n int32) *btapb.ColumnFamily {
		return &btapb.ColumnFamily{GcRule: &btapb.GcRule{Rule: &btapb.GcRule_MaxNumVersions{MaxNumVersions: n}}}
	}

	for desc, tc := range map[string]struct {
		mods []*btapb.ModifyColumnFamiliesRequest_Modification
		code codes.Code
	}{
		"invalid family id last":   {[]*btapb.ModifyColumnFamiliesRequest_Modification{create("cf3", versions(1)), update("cf1", versions(1)), drop("cf2"), create("bad id!", versions(1))}, codes.InvalidArgument},
		"update of missing family": {[]*btapb.ModifyColumnFamiliesRequest_Modification{drop("cf2"), update("nope", versions(1))}, codes.NotFound},
		"drop of missing family":   {[]*btapb.ModifyColumnFamiliesRequest_Modification{create("cf3", versions(1)), drop("nope")}, codes.NotFound},
		"create existing family":   {[]*btapb.ModifyColumnFamiliesRequest_Modification{update("cf1", versions(1)), create("cf2", versions(1))}, codes.AlreadyExists},
		"invalid gc rule":          {[]*btapb.ModifyColumnFamiliesRequest_Modification{drop("cf2"), update("cf1", &btapb.ColumnFamily{GcRule: &btapb.GcRule{Rule: &btapb.GcRule_MaxAge{MaxAge: durationpb.New(time.Microsecond)}}})}, codes.InvalidArgument},
		"unknown update mask path": {[]*btapb.ModifyColumnFamiliesRequest_Modification{drop("cf2"), update("cf1", versions(1), "stats")}, codes.InvalidArgument},
		"drop false":               {[]*btapb.ModifyColumnFamiliesRequest_Modification{create("cf3", versions(1)), {Id: "cf1", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Drop{Drop: false}}}, codes.InvalidArgument},
		"no mod":                   {[]*btapb.ModifyColumnFamiliesRequest_Modification{drop("cf2"), {Id: "cf1"}}, codes.InvalidArgument},
		"no modifications":         {nil, codes.InvalidArgument},
	} {
		_, err := s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{Name: name, Modifications: tc.mods})
		require.Error(t, err, desc)
		assert.Equal(t, tc.code, status.Code(err), "%s: %v", desc, err)
		after, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
		require.NoError(t, err)
		assert.True(t, proto.Equal(before, after), "%s: schema changed after failed request: %v", desc, after)
		r := storedRow(t, ctaTable(t, s, name), "r1")
		require.NotNil(t, r, desc)
		assert.Contains(t, r.families, "cf2", desc)
	}
	_, err = s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{Name: ctaParent + "/tables/missing", Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{create("x", versions(1))}})
	ctaRequireCode(t, err, codes.NotFound)

	// A valid batch applies every modification in order.
	got, err := s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{Name: name, Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{
		create("cf3", versions(4)), update("cf1", versions(1), "gc_rule"), drop("cf2"), create("cf2", nil),
	}})
	require.NoError(t, err)
	require.Len(t, got.GetColumnFamilies(), 3)
	assert.Equal(t, int32(4), got.GetColumnFamilies()["cf3"].GetGcRule().GetMaxNumVersions())
	assert.Equal(t, int32(1), got.GetColumnFamilies()["cf1"].GetGcRule().GetMaxNumVersions())
	// cf2 was dropped and re-created in the same request: its data is gone.
	assert.Nil(t, storedRow(t, ctaTable(t, s, name), "r1"))
}

func TestConformanceTableAdminDroppedFamilyDataNotResurrected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dbFile := newDBFile(t)
	h := ctaOpen(t, ctx, dbFile)
	require.NoError(t, h.admin.CreateTable(ctx, "drop"))
	require.NoError(t, h.admin.CreateColumnFamily(ctx, "drop", "keep"))
	require.NoError(t, h.admin.CreateColumnFamily(ctx, "drop", "gone"))
	tbl := h.client.Open("drop")
	for _, key := range []string{"r1", "r2"} {
		m := bigtable.NewMutation()
		m.Set("gone", "q", bigtable.Timestamp(1000), []byte("old-"+key))
		if key == "r1" {
			m.Set("keep", "q", bigtable.Timestamp(1000), []byte("kept"))
		}
		require.NoError(t, tbl.Apply(ctx, key, m))
	}

	require.NoError(t, h.admin.DeleteColumnFamily(ctx, "drop", "gone"))
	require.NoError(t, h.admin.CreateColumnFamily(ctx, "drop", "gone"))
	check := func(h *storageConformanceHarness) {
		t.Helper()
		tbl := h.client.Open("drop")
		assert.Equal(t, []string{"r1"}, ctaReadKeys(t, ctx, tbl), "a row whose only family was dropped disappears")
		row, err := tbl.ReadRow(ctx, "r1")
		require.NoError(t, err)
		assert.Empty(t, row["gone"], "re-created family must start empty")
		require.Len(t, row["keep"], 1)
		assert.Equal(t, "kept", string(row["keep"][0].Value))
		stored := storedRow(t, ctaTable(t, h.server.s, ctaParent+"/tables/drop"), "r1")
		require.NotNil(t, stored)
		assert.NotContains(t, stored.families, "gone", "dropped family data must be removed from storage")
		assert.Nil(t, storedRow(t, ctaTable(t, h.server.s, ctaParent+"/tables/drop"), "r2"))
	}
	check(h)
	h.close()

	h2 := ctaOpen(t, ctx, dbFile)
	defer h2.close()
	check(h2)
}

func TestConformanceTableAdminValueTypeImmutable(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	ctx := env.ctx
	require.NoError(t, env.admin.CreateTable(ctx, "agg"))
	sum := bigtable.AggregateType{Input: bigtable.Int64Type{}, Aggregator: bigtable.SumAggregator{}}
	require.NoError(t, env.admin.CreateColumnFamilyWithConfig(ctx, "agg", "counters", bigtable.Family{ValueType: sum}))

	info, err := env.admin.TableInfo(ctx, "agg")
	require.NoError(t, err)
	require.Len(t, info.FamilyInfos, 1)
	_, isAgg := info.FamilyInfos[0].ValueType.(bigtable.AggregateType)
	assert.True(t, isAgg, "value type: %#v", info.FamilyInfos[0].ValueType)

	// The aggregate type is immutable once set.
	maxType := bigtable.AggregateType{Input: bigtable.Int64Type{}, Aggregator: bigtable.MaxAggregator{}}
	err = env.admin.UpdateFamily(ctx, "agg", "counters", bigtable.Family{ValueType: maxType})
	ctaRequireCode(t, err, codes.InvalidArgument)
	// Restating the same type, and updating the GC rule, are allowed.
	require.NoError(t, env.admin.UpdateFamily(ctx, "agg", "counters", bigtable.Family{ValueType: sum, GCPolicy: bigtable.MaxVersionsPolicy(1)}))
	info, err = env.admin.TableInfo(ctx, "agg")
	require.NoError(t, err)
	assert.Equal(t, "versions() > 1", info.FamilyInfos[0].GCPolicy)
	// A family without a type cannot gain one.
	require.NoError(t, env.admin.CreateColumnFamily(ctx, "agg", "raw"))
	err = env.admin.UpdateFamily(ctx, "agg", "raw", bigtable.Family{ValueType: sum})
	ctaRequireCode(t, err, codes.InvalidArgument)

	// Only Aggregate types with Int64 input (or HLL) are valid family types.
	for desc, vt := range map[string]bigtable.Type{
		"string type":         bigtable.StringType{},
		"sum of strings":      bigtable.AggregateType{Input: bigtable.StringType{}, Aggregator: bigtable.SumAggregator{}},
		"min of bytes":        bigtable.AggregateType{Input: bigtable.BytesType{}, Aggregator: bigtable.MinAggregator{}},
		"int64 non-aggregate": bigtable.Int64Type{},
	} {
		err := env.admin.CreateColumnFamilyWithConfig(ctx, "agg", "bad", bigtable.Family{ValueType: vt})
		require.Error(t, err, desc)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", desc, err)
	}
	for _, agg := range []bigtable.Aggregator{bigtable.MinAggregator{}, bigtable.MaxAggregator{}, bigtable.HllppUniqueCountAggregator{}} {
		id := fmt.Sprintf("ok%T", agg)
		id = strings.NewReplacer("bigtable.", "", "{", "", "}", "").Replace(id)
		require.NoError(t, env.admin.CreateColumnFamilyWithConfig(ctx, "agg", id, bigtable.Family{
			ValueType: bigtable.AggregateType{Input: bigtable.Int64Type{}, Aggregator: agg},
		}), id)
	}
}

func TestConformanceTableAdminInitialSplitsPersisted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dbFile := newDBFile(t)
	splits := []string{"m", "b", "t"}

	h1 := ctaOpen(t, ctx, dbFile)
	require.NoError(t, h1.admin.CreatePresplitTable(ctx, "presplit", splits))
	require.NoError(t, h1.admin.CreateColumnFamily(ctx, "presplit", "cf"))
	tbl := h1.client.Open("presplit")
	for _, key := range []string{"a", "c", "n"} {
		m := bigtable.NewMutation()
		m.Set("cf", "q", bigtable.Timestamp(1000), []byte(key))
		require.NoError(t, tbl.Apply(ctx, key, m))
	}
	keys1, err := tbl.SampleRowKeys(ctx)
	require.NoError(t, err)
	h1.close()

	h2 := ctaOpen(t, ctx, dbFile)
	defer h2.close()
	assert.Equal(t, []string{"b", "m", "t"}, ctaTable(t, h2.server.s, ctaParent+"/tables/presplit").initialSplits)
	keys2, err := h2.client.Open("presplit").SampleRowKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, keys1, keys2, "samples differ across restart")
	for _, split := range []string{"b", "m", "t"} {
		assert.Contains(t, keys2, split)
	}
	sorted := append([]string(nil), keys2...)
	if len(sorted) > 0 && sorted[len(sorted)-1] == "" {
		sorted = sorted[:len(sorted)-1] // end-of-table key
	}
	assert.True(t, sort.StringsAreSorted(sorted), "sample keys out of order: %q", keys2)
}

func TestConformanceTableAdminDeleteUndelete(t *testing.T) {
	ctx := context.Background()
	dbFile := newDBFile(t)
	s, closeDB := ctaOpenState(t, dbFile)
	name := ctaCreateTable(t, s, "undel", "cf")
	for _, key := range []string{"r1", "r2", "r3"} {
		ctaWrite(t, s, name, key, "cf", "q", "v-"+key)
	}

	_, err := s.UndeleteTable(ctx, &btapb.UndeleteTableRequest{Name: name})
	ctaRequireCode(t, err, codes.AlreadyExists)
	_, err = s.UndeleteTable(ctx, &btapb.UndeleteTableRequest{Name: ctaParent + "/tables/never"})
	ctaRequireCode(t, err, codes.NotFound)
	_, err = s.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: ctaParent + "/tables/never"})
	ctaRequireCode(t, err, codes.NotFound)

	_, err = s.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: name})
	require.NoError(t, err)
	_, err = s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	ctaRequireCode(t, err, codes.NotFound)
	list, err := s.ListTables(ctx, &btapb.ListTablesRequest{Parent: ctaParent})
	require.NoError(t, err)
	assert.Empty(t, list.GetTables())
	_, err = s.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: name})
	ctaRequireCode(t, err, codes.NotFound)

	// The tombstone survives a restart.
	closeDB()
	s, closeDB = ctaOpenState(t, dbFile)
	defer func() { closeDB() }()
	_, err = s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	ctaRequireCode(t, err, codes.NotFound)

	op, err := s.UndeleteTable(ctx, &btapb.UndeleteTableRequest{Name: name})
	require.NoError(t, err)
	require.True(t, op.GetDone())
	md := &btapb.UndeleteTableMetadata{}
	require.NoError(t, op.GetMetadata().UnmarshalTo(md))
	assert.Equal(t, name, md.GetName())
	assert.NotNil(t, md.GetStartTime())
	assert.NotNil(t, md.GetEndTime())
	restored := &btapb.Table{}
	require.NoError(t, op.GetResponse().UnmarshalTo(restored))
	assert.Equal(t, name, restored.GetName())
	assert.True(t, restored.GetDeletionProtection(), "undelete enables deletion protection")
	assert.Contains(t, restored.GetColumnFamilies(), "cf")
	tbl := ctaTable(t, s, name)
	assert.Equal(t, 3, rowCount(t, tbl))
	r := storedRow(t, tbl, "r2")
	require.NotNil(t, r)
	assert.Equal(t, "v-r2", string(r.families["cf"].Cells["q"][0].Value))
	ctaWrite(t, s, name, "r4", "cf", "q", "after-undelete")
	assert.Equal(t, 4, rowCount(t, tbl))

	// Undelete when a live table exists is a conflict.
	_, err = s.UndeleteTable(ctx, &btapb.UndeleteTableRequest{Name: name})
	ctaRequireCode(t, err, codes.AlreadyExists)
	// The undeleted table is protected.
	_, err = s.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: name})
	ctaRequireCode(t, err, codes.FailedPrecondition)

	// Delete, recreate the same ID, then undelete: the live table wins.
	_, err = ctaUpdate(ctx, s, &btapb.Table{Name: name}, false, "deletion_protection")
	require.NoError(t, err)
	_, err = s.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: name})
	require.NoError(t, err)
	ctaCreateTable(t, s, "undel", "other")
	assert.Equal(t, 0, rowCount(t, ctaTable(t, s, name)), "a recreated table starts empty")
	_, err = s.UndeleteTable(ctx, &btapb.UndeleteTableRequest{Name: name})
	ctaRequireCode(t, err, codes.AlreadyExists)
	got, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)
	assert.Contains(t, got.GetColumnFamilies(), "other")
	assert.NotContains(t, got.GetColumnFamilies(), "cf")

	// Deleting the recreated table makes the most recent deletion the one
	// that is undeleted.
	_, err = s.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: name})
	require.NoError(t, err)
	_, err = s.UndeleteTable(ctx, &btapb.UndeleteTableRequest{Name: name})
	require.NoError(t, err)
	got, err = s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)
	assert.Contains(t, got.GetColumnFamilies(), "other")
	assert.Equal(t, 0, rowCount(t, ctaTable(t, s, name)))
}

func TestConformanceTableAdminDeletionProtectionBlocksDelete(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	ctx := env.ctx
	require.NoError(t, env.admin.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "guarded", DeletionProtection: bigtable.Protected}))
	info, err := env.admin.TableInfo(ctx, "guarded")
	require.NoError(t, err)
	assert.Equal(t, bigtable.Protected, info.DeletionProtection)
	err = env.admin.DeleteTable(ctx, "guarded")
	ctaRequireCode(t, err, codes.FailedPrecondition)
	tables, err := env.admin.Tables(ctx)
	require.NoError(t, err)
	assert.Contains(t, tables, "guarded")
	require.NoError(t, env.admin.UpdateTableWithDeletionProtection(ctx, "guarded", bigtable.Unprotected))
	require.NoError(t, env.admin.DeleteTable(ctx, "guarded"))
	tables, err = env.admin.Tables(ctx)
	require.NoError(t, err)
	assert.NotContains(t, tables, "guarded")
}

func TestConformanceTableAdminDropRowRange(t *testing.T) {
	// Request validation.
	s, _ := newFullTestServer(t)
	name := ctaCreateTable(t, s, "drr", "cf")
	for desc, req := range map[string]*btapb.DropRowRangeRequest{
		"no target":        {Name: name},
		"empty prefix":     {Name: name, Target: &btapb.DropRowRangeRequest_RowKeyPrefix{RowKeyPrefix: nil}},
		"delete all false": {Name: name, Target: &btapb.DropRowRangeRequest_DeleteAllDataFromTable{DeleteAllDataFromTable: false}},
	} {
		_, err := s.DropRowRange(context.Background(), req)
		require.Error(t, err, desc)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", desc, err)
	}
	_, err := s.DropRowRange(context.Background(), &btapb.DropRowRangeRequest{Name: ctaParent + "/tables/missing",
		Target: &btapb.DropRowRangeRequest_DeleteAllDataFromTable{DeleteAllDataFromTable: true}})
	ctaRequireCode(t, err, codes.NotFound)

	env := setupTestEnv(t)
	defer env.cancel()
	ctx := env.ctx
	tbl := env.createTable(t, "drr", "cf")
	keys := []string{"a\xff1", "a\xff\xff", "b", "user1#1", "user1#2", "user10#1", "user2#1"}
	for _, key := range keys {
		m := bigtable.NewMutation()
		m.Set("cf", "q", bigtable.Timestamp(1000), []byte(key))
		require.NoError(t, tbl.Apply(ctx, key, m))
	}

	require.NoError(t, env.admin.DropRowRange(ctx, "drr", "user1#"))
	assert.Equal(t, []string{"a\xff1", "a\xff\xff", "b", "user10#1", "user2#1"}, ctaReadKeys(t, ctx, tbl))
	require.NoError(t, env.admin.DropRowRange(ctx, "drr", "user1"))
	assert.Equal(t, []string{"a\xff1", "a\xff\xff", "b", "user2#1"}, ctaReadKeys(t, ctx, tbl))
	// A prefix ending in 0xff covers every key under it, and nothing after.
	require.NoError(t, env.admin.DropRowRange(ctx, "drr", "a\xff"))
	assert.Equal(t, []string{"b", "user2#1"}, ctaReadKeys(t, ctx, tbl))
	require.NoError(t, env.admin.DropRowRange(ctx, "drr", "zzz"), "dropping an empty range succeeds")
	require.NoError(t, env.admin.DropAllRows(ctx, "drr"))
	assert.Empty(t, ctaReadKeys(t, ctx, tbl))
	info, err := env.admin.TableInfo(ctx, "drr")
	require.NoError(t, err)
	assert.Equal(t, []string{"cf"}, info.Families, "DropRowRange keeps the schema")

}

func TestConformanceTableAdminConsistencyTokens(t *testing.T) {
	s, _ := newFullTestServer(t)
	t1 := ctaCreateTable(t, s, "ct1", "cf")
	t2 := ctaCreateTable(t, s, "ct2", "cf")
	tok1, err := s.GenerateConsistencyToken(context.Background(), &btapb.GenerateConsistencyTokenRequest{Name: t1})
	require.NoError(t, err)
	require.NotEmpty(t, tok1.GetConsistencyToken())
	tok2, err := s.GenerateConsistencyToken(context.Background(), &btapb.GenerateConsistencyTokenRequest{Name: t2})
	require.NoError(t, err)

	resp, err := s.CheckConsistency(context.Background(), &btapb.CheckConsistencyRequest{Name: t1, ConsistencyToken: tok1.GetConsistencyToken()})
	require.NoError(t, err)
	assert.True(t, resp.GetConsistent())

	for desc, tok := range map[string]string{
		"other table": tok2.GetConsistencyToken(),
		"garbage":     "not a token",
		"empty":       "",
	} {
		_, err := s.CheckConsistency(context.Background(), &btapb.CheckConsistencyRequest{Name: t1, ConsistencyToken: tok})
		require.Error(t, err, desc)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", desc, err)
	}
	_, err = s.GenerateConsistencyToken(context.Background(), &btapb.GenerateConsistencyTokenRequest{Name: ctaParent + "/tables/missing"})
	ctaRequireCode(t, err, codes.NotFound)
	_, err = s.CheckConsistency(context.Background(), &btapb.CheckConsistencyRequest{Name: ctaParent + "/tables/missing", ConsistencyToken: tok1.GetConsistencyToken()})
	ctaRequireCode(t, err, codes.NotFound)

	// The official client's WaitForReplication completes immediately.
	env := setupTestEnv(t)
	defer env.cancel()
	env.createTable(t, "ct1", "cf")
	require.NoError(t, env.admin.WaitForReplication(env.ctx, "ct1"))
}

func TestConformanceTableAdminSnapshotsUnimplemented(t *testing.T) {
	s, _ := newFullTestServer(t)
	ctx := context.Background()
	name := ctaCreateTable(t, s, "snap", "cf")
	snapshot := ctaParent + "/clusters/c1/snapshots/s1"
	_, err := s.SnapshotTable(ctx, &btapb.SnapshotTableRequest{Name: name, Cluster: ctaParent + "/clusters/c1", SnapshotId: "s1"})
	ctaRequireCode(t, err, codes.Unimplemented)
	_, err = s.GetSnapshot(ctx, &btapb.GetSnapshotRequest{Name: snapshot})
	ctaRequireCode(t, err, codes.Unimplemented)
	_, err = s.ListSnapshots(ctx, &btapb.ListSnapshotsRequest{Parent: ctaParent + "/clusters/c1"})
	ctaRequireCode(t, err, codes.Unimplemented)
	_, err = s.DeleteSnapshot(ctx, &btapb.DeleteSnapshotRequest{Name: snapshot})
	ctaRequireCode(t, err, codes.Unimplemented)
	_, err = s.CreateTableFromSnapshot(ctx, &btapb.CreateTableFromSnapshotRequest{Parent: ctaParent, TableId: "fromsnap", SourceSnapshot: snapshot})
	ctaRequireCode(t, err, codes.Unimplemented)
}

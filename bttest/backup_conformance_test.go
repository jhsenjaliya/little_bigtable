package bttest

// Conformance tests for Bigtable backups (CreateBackup, GetBackup,
// UpdateBackup, ListBackups, DeleteBackup, CopyBackup, RestoreTable) against
// the official Go client where it exposes the feature, and raw admin gRPC for
// fields it does not (backup_type validation, filter, order_by, LRO metadata).
//
// References: docs/backups.txt, docs/managing-backups.txt and
// protos/admin_v2_{table,bigtable_table_admin}.proto in the research scratchpad.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	cbkProject  = "cbk-project"
	cbkInstance = "cbk-instance"
	cbkCluster1 = "cluster-one"
	cbkCluster2 = "cluster-two"
	cbkParent   = "projects/" + cbkProject + "/instances/" + cbkInstance
	cbkC1       = cbkParent + "/clusters/" + cbkCluster1
	cbkC2       = cbkParent + "/clusters/" + cbkCluster2
	cbkDay      = 24 * time.Hour
)

// cbkHarness is one emulator process lifetime over a persistent DB file.
type cbkHarness struct {
	srv    *Server
	db     *sql.DB
	conn   *grpc.ClientConn
	admin  *bigtable.AdminClient
	client *bigtable.Client
	tables btapb.BigtableTableAdminClient
	insts  btapb.BigtableInstanceAdminClient
	ops    longrunningpb.OperationsClient
	closed bool
}

// cbkStrict enables strict admin mode (registered instances only) for t.
func cbkStrict(t *testing.T) {
	t.Helper()
	prevDialect, prevStrict := currentDialect(), isStrictAdmin()
	ConfigureStorage("sqlite3", true)
	t.Cleanup(func() { ConfigureStorage(string(prevDialect), prevStrict) })
}

// cbkStart serves an emulator from dbFile. With createInstance it registers
// cbkInstance with two SSD clusters (the default app profile routes to
// cbkCluster1, so cbkCluster2 is deletable).
func cbkStart(t *testing.T, ctx context.Context, dbFile string, createInstance bool) *cbkHarness {
	t.Helper()
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared", dbFile))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	require.NoError(t, CreateTables(ctx, db))
	srv, err := NewServer("127.0.0.1:0", db)
	require.NoError(t, err)
	t.Setenv("BIGTABLE_EMULATOR_HOST", srv.Addr)
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	h := &cbkHarness{
		srv: srv, db: db, conn: conn,
		tables: btapb.NewBigtableTableAdminClient(conn),
		insts:  btapb.NewBigtableInstanceAdminClient(conn),
		ops:    longrunningpb.NewOperationsClient(conn),
	}
	t.Cleanup(h.close)
	if createInstance {
		cbkCreateInstance(t, ctx, h, cbkInstance, btapb.Instance_ENTERPRISE, map[string]*btapb.Cluster{cbkCluster1: {}, cbkCluster2: {}})
	}
	h.admin, err = bigtable.NewAdminClient(ctx, cbkProject, cbkInstance, option.WithGRPCConn(conn))
	require.NoError(t, err)
	h.client, err = bigtable.NewClient(ctx, cbkProject, cbkInstance, option.WithGRPCConn(conn))
	require.NoError(t, err)
	return h
}

func (h *cbkHarness) close() {
	if h.closed {
		return
	}
	h.closed = true
	if h.client != nil {
		_ = h.client.Close()
	}
	if h.admin != nil {
		_ = h.admin.Close()
	}
	_ = h.conn.Close()
	h.srv.Close()
	_ = h.db.Close()
}

func cbkCreateInstance(t *testing.T, ctx context.Context, h *cbkHarness, id string, edition btapb.Instance_Edition, clusters map[string]*btapb.Cluster) {
	t.Helper()
	_, err := h.insts.CreateInstance(ctx, &btapb.CreateInstanceRequest{
		Parent:     "projects/" + cbkProject,
		InstanceId: id,
		Instance:   &btapb.Instance{DisplayName: "backup conformance", Edition: edition},
		Clusters:   clusters,
	})
	require.NoError(t, err)
}

// cbkCreateSourceTable creates parent/tables/id with a GC-ruled family, a
// plain family and an Int64 sum aggregate family, and writes fixed rows.
func cbkCreateSourceTable(t *testing.T, ctx context.Context, h *cbkHarness, parent, id string) {
	t.Helper()
	_, err := h.tables.CreateTable(ctx, &btapb.CreateTableRequest{
		Parent:  parent,
		TableId: id,
		Table: &btapb.Table{ColumnFamilies: map[string]*btapb.ColumnFamily{
			"cf1": {GcRule: &btapb.GcRule{Rule: &btapb.GcRule_MaxNumVersions{MaxNumVersions: 5}}},
			"cf2": {},
			"agg": sumFamily(),
		}},
	})
	require.NoError(t, err)
	if parent != cbkParent {
		return
	}
	tbl := h.client.Open(id)
	ts := bigtable.Timestamp(1_700_000_000_000_000)
	for i, key := range []string{"r1", "r2", "r3"} {
		m := bigtable.NewMutation()
		m.Set("cf1", "a", ts, []byte(fmt.Sprintf("%s-a-old", key)))
		m.Set("cf1", "a", ts+1000, []byte(fmt.Sprintf("%s-a-new", key)))
		m.Set("cf2", "b", ts, []byte(fmt.Sprintf("%s-b", key)))
		m.AddIntToCell("agg", "total", ts, int64(5+i))
		m.AddIntToCell("agg", "total", ts, 7)
		require.NoError(t, tbl.Apply(ctx, key, m))
	}
}

func cbkReadAll(t *testing.T, ctx context.Context, h *cbkHarness, table string) map[string]bigtable.Row {
	t.Helper()
	rows := map[string]bigtable.Row{}
	err := h.client.Open(table).ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		rows[r.Key()] = r
		return true
	})
	require.NoError(t, err)
	return rows
}

// cbkEditBackup rewrites a stored backup descriptor, simulating the passage
// of time (an old creation time or an elapsed expire_time).
func cbkEditBackup(t *testing.T, ctx context.Context, s *server, name string, edit func(*btapb.Backup)) {
	t.Helper()
	b, ok, err := s.backupBackend.get(ctx, name)
	require.NoError(t, err)
	require.True(t, ok, "backup %s not stored", name)
	edit(b)
	require.NoError(t, s.backupBackend.store.put(ctx, nil, name, "", b))
}

func cbkCode(t *testing.T, want codes.Code, err error, msgAndArgs ...any) {
	t.Helper()
	require.Error(t, err, msgAndArgs...)
	require.Equal(t, want, status.Code(err), append([]any{"error: %v", err}, msgAndArgs...)...)
}

func cbkNames(backups []*btapb.Backup) []string {
	out := make([]string, len(backups))
	for i, b := range backups {
		out[i] = b.GetName()[len(backupCluster(b.GetName()))+len("/backups/"):]
	}
	return out
}

// TestConformanceBackupExpireTimeAndEdition covers Backup.expire_time: it is
// required and must be 6 hours to 90 days after creation (365 days on
// Enterprise Plus instances), on create and on UpdateBackup.
func TestConformanceBackupExpireTimeAndEdition(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	cbkCreateSourceTable(t, ctx, h, cbkParent, "src")

	now := time.Now()
	for name, expire := range map[string]time.Time{
		"in the past":     now.Add(-time.Hour),
		"under 6 hours":   now.Add(5 * time.Hour),
		"over 90 days":    now.Add(91 * cbkDay),
		"365 days (Ent.)": now.Add(300 * cbkDay),
	} {
		cbkCode(t, codes.InvalidArgument, h.admin.CreateBackup(ctx, "src", cbkCluster1, "bad", expire), name)
	}
	_, err := h.tables.CreateBackup(ctx, &btapb.CreateBackupRequest{
		Parent: cbkC1, BackupId: "noexpire", Backup: &btapb.Backup{SourceTable: cbkParent + "/tables/src"},
	})
	cbkCode(t, codes.InvalidArgument, err, "missing expire_time")

	require.NoError(t, h.admin.CreateBackup(ctx, "src", cbkCluster1, "ok", now.Add(90*cbkDay-time.Minute)))
	info, err := h.admin.BackupInfo(ctx, cbkCluster1, "ok")
	require.NoError(t, err)
	assert.Equal(t, "READY", info.State)
	assert.Equal(t, "src", info.SourceTable)
	assert.Equal(t, bigtable.BackupTypeStandard, info.BackupType)
	assert.Positive(t, info.SizeBytes)
	assert.False(t, info.EndTime.Before(info.StartTime))
	require.NotNil(t, info.EncryptionInfo)
	assert.Equal(t, bigtable.GoogleDefaultEncryption, info.EncryptionInfo.Type)

	cbkCode(t, codes.AlreadyExists, h.admin.CreateBackup(ctx, "src", cbkCluster1, "ok", now.Add(cbkDay)))
	cbkCode(t, codes.NotFound, h.admin.CreateBackup(ctx, "missing", cbkCluster1, "nosrc", now.Add(cbkDay)))
	cbkCode(t, codes.NotFound, h.admin.CreateBackup(ctx, "src", "cluster-missing", "nocluster", now.Add(cbkDay)))
	_, err = h.tables.CreateBackup(ctx, &btapb.CreateBackupRequest{
		Parent: cbkC1, BackupId: "bad id!", Backup: &btapb.Backup{SourceTable: cbkParent + "/tables/src", ExpireTime: timestamppb.New(now.Add(cbkDay))},
	})
	cbkCode(t, codes.InvalidArgument, err, "invalid backup_id")

	// UpdateBackup applies the same window, measured from creation.
	cbkCode(t, codes.InvalidArgument, h.admin.UpdateBackup(ctx, cbkCluster1, "ok", now.Add(100*cbkDay)))
	cbkCode(t, codes.InvalidArgument, h.admin.UpdateBackup(ctx, cbkCluster1, "ok", now.Add(time.Hour)))
	newExpire := now.Add(30 * cbkDay).Truncate(time.Microsecond)
	require.NoError(t, h.admin.UpdateBackup(ctx, cbkCluster1, "ok", newExpire))
	info, err = h.admin.BackupInfo(ctx, cbkCluster1, "ok")
	require.NoError(t, err)
	assert.True(t, info.ExpireTime.Equal(newExpire), "expire_time = %v, want %v", info.ExpireTime, newExpire)
	_, err = h.tables.UpdateBackup(ctx, &btapb.UpdateBackupRequest{
		Backup:     &btapb.Backup{Name: cbkC1 + "/backups/ok", SourceTable: "other"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"source_table"}},
	})
	cbkCode(t, codes.InvalidArgument, err, "only expire_time/hot_to_standard_time are updatable")
	_, err = h.tables.UpdateBackup(ctx, &btapb.UpdateBackupRequest{Backup: &btapb.Backup{Name: cbkC1 + "/backups/ok"}})
	cbkCode(t, codes.InvalidArgument, err, "update_mask required")

	// Enterprise Plus allows up to 365 days.
	const plusInstance = "cbk-plus-instance"
	cbkCreateInstance(t, ctx, h, plusInstance, btapb.Instance_ENTERPRISE_PLUS, map[string]*btapb.Cluster{"plus-cluster": {}})
	plus, err := bigtable.NewAdminClient(ctx, cbkProject, plusInstance, option.WithGRPCConn(h.conn))
	require.NoError(t, err)
	require.NoError(t, plus.CreateTable(ctx, "src"))
	require.NoError(t, plus.CreateBackup(ctx, "src", "plus-cluster", "long", now.Add(300*cbkDay)))
	cbkCode(t, codes.InvalidArgument, plus.CreateBackup(ctx, "src", "plus-cluster", "too-long", now.Add(366*cbkDay)))
	require.NoError(t, plus.UpdateBackup(ctx, "plus-cluster", "long", now.Add(364*cbkDay)))
	cbkCode(t, codes.InvalidArgument, plus.UpdateBackup(ctx, "plus-cluster", "long", now.Add(366*cbkDay)))
}

// TestConformanceBackupHotBackupRules covers Backup.backup_type and
// hot_to_standard_time: the transition time is only valid on HOT backups and
// must be at least 24 hours after creation; backup_type is immutable (a
// standard backup can't become hot); hot backups can't live on HDD clusters.
func TestConformanceBackupHotBackupRules(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	cbkCreateSourceTable(t, ctx, h, cbkParent, "src")
	now := time.Now()
	expire := bigtable.WithExpiry(now.Add(7 * cbkDay))

	cbkCode(t, codes.InvalidArgument, h.admin.CreateBackupWithOptions(ctx, "src", cbkCluster1, "hot-early", expire, bigtable.WithHotToStandardBackup(now.Add(12*time.Hour))))

	transition := now.Add(48 * time.Hour).Truncate(time.Microsecond)
	require.NoError(t, h.admin.CreateBackupWithOptions(ctx, "src", cbkCluster1, "hot", expire, bigtable.WithHotToStandardBackup(transition)))
	info, err := h.admin.BackupInfo(ctx, cbkCluster1, "hot")
	require.NoError(t, err)
	assert.Equal(t, bigtable.BackupTypeHot, info.BackupType)
	require.NotNil(t, info.HotToStandardTime)
	assert.True(t, info.HotToStandardTime.Equal(transition))

	require.NoError(t, h.admin.CreateBackupWithOptions(ctx, "src", cbkCluster1, "hot-forever", expire, bigtable.WithHotBackup()))
	info, err = h.admin.BackupInfo(ctx, cbkCluster1, "hot-forever")
	require.NoError(t, err)
	assert.Equal(t, bigtable.BackupTypeHot, info.BackupType)
	assert.Nil(t, info.HotToStandardTime)

	// hot_to_standard_time on a standard backup fails the request.
	_, err = h.tables.CreateBackup(ctx, &btapb.CreateBackupRequest{
		Parent: cbkC1, BackupId: "std-transition",
		Backup: &btapb.Backup{
			SourceTable: cbkParent + "/tables/src", ExpireTime: timestamppb.New(now.Add(7 * cbkDay)),
			BackupType: btapb.Backup_STANDARD, HotToStandardTime: timestamppb.New(now.Add(48 * time.Hour)),
		},
	})
	cbkCode(t, codes.InvalidArgument, err, "standard backup with hot_to_standard_time")
	require.NoError(t, h.admin.CreateBackup(ctx, "src", cbkCluster1, "std", now.Add(7*cbkDay)))
	cbkCode(t, codes.InvalidArgument, h.admin.UpdateBackupHotToStandardTime(ctx, cbkCluster1, "std", now.Add(48*time.Hour)))
	_, err = h.tables.UpdateBackup(ctx, &btapb.UpdateBackupRequest{
		Backup:     &btapb.Backup{Name: cbkC1 + "/backups/std", BackupType: btapb.Backup_HOT},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"backup_type"}},
	})
	cbkCode(t, codes.InvalidArgument, err, "standard backups can't be converted to hot")

	// The transition time can be modified, but stays >= 24h after creation.
	cbkCode(t, codes.InvalidArgument, h.admin.UpdateBackupHotToStandardTime(ctx, cbkCluster1, "hot", now.Add(12*time.Hour)))
	later := now.Add(72 * time.Hour).Truncate(time.Microsecond)
	require.NoError(t, h.admin.UpdateBackupHotToStandardTime(ctx, cbkCluster1, "hot", later))
	info, err = h.admin.BackupInfo(ctx, cbkCluster1, "hot")
	require.NoError(t, err)
	require.NotNil(t, info.HotToStandardTime)
	assert.True(t, info.HotToStandardTime.Equal(later))
	require.NoError(t, h.admin.UpdateBackupRemoveHotToStandardTime(ctx, cbkCluster1, "hot"))
	info, err = h.admin.BackupInfo(ctx, cbkCluster1, "hot")
	require.NoError(t, err)
	assert.Nil(t, info.HotToStandardTime)
	assert.Equal(t, bigtable.BackupTypeHot, info.BackupType)

	// Hot backups can't be created on an HDD cluster; standard ones can.
	const hddInstance = "cbk-hdd-instance"
	cbkCreateInstance(t, ctx, h, hddInstance, btapb.Instance_ENTERPRISE, map[string]*btapb.Cluster{"hdd-cluster": {DefaultStorageType: btapb.StorageType_HDD}})
	hddParent := "projects/" + cbkProject + "/instances/" + hddInstance
	cbkCreateSourceTable(t, ctx, h, hddParent, "src")
	hddBackup := func(id string, typ btapb.Backup_BackupType) error {
		_, err := h.tables.CreateBackup(ctx, &btapb.CreateBackupRequest{
			Parent: hddParent + "/clusters/hdd-cluster", BackupId: id,
			Backup: &btapb.Backup{SourceTable: hddParent + "/tables/src", ExpireTime: timestamppb.New(now.Add(7 * cbkDay)), BackupType: typ},
		})
		return err
	}
	cbkCode(t, codes.InvalidArgument, hddBackup("hot", btapb.Backup_HOT), "hot backup on HDD")
	require.NoError(t, hddBackup("std", btapb.Backup_STANDARD))
}

// TestConformanceBackupRestoreAfterSourceDeletedAndRestart covers backups as
// immutable snapshots: data written after the backup is excluded, the backup
// survives deleting its source table and an emulator restart, and RestoreTable
// reproduces the exact rows and families (value types included) without
// inheriting GC policies or deletion protection, with RestoreInfo set.
func TestConformanceBackupRestoreAfterSourceDeletedAndRestart(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dbFile := newDBFile(t)
	h := cbkStart(t, ctx, dbFile, true)
	cbkCreateSourceTable(t, ctx, h, cbkParent, "src")
	require.NoError(t, h.admin.UpdateTableWithDeletionProtection(ctx, "src", bigtable.Protected))
	before := cbkReadAll(t, ctx, h, "src")
	require.Len(t, before, 3)
	require.Len(t, before["r1"]["cf1"], 2)
	require.Len(t, before["r1"]["agg"], 1)

	require.NoError(t, h.admin.CreateBackup(ctx, "src", cbkCluster1, "bk", time.Now().Add(7*cbkDay)))

	// Writes after the backup must not leak into it.
	m := bigtable.NewMutation()
	m.Set("cf2", "b", bigtable.Timestamp(1_800_000_000_000_000), []byte("after-backup"))
	require.NoError(t, h.client.Open("src").Apply(ctx, "r1", m))
	require.NoError(t, h.client.Open("src").Apply(ctx, "late", m))
	require.NoError(t, h.admin.UpdateTableWithDeletionProtection(ctx, "src", bigtable.Unprotected))
	require.NoError(t, h.admin.DeleteTable(ctx, "src"))
	h.close()

	h = cbkStart(t, ctx, dbFile, false)
	info, err := h.admin.BackupInfo(ctx, cbkCluster1, "bk")
	require.NoError(t, err, "backup must survive source deletion and restart")
	assert.Equal(t, "READY", info.State)

	require.NoError(t, h.admin.RestoreTable(ctx, "restored", cbkCluster1, "bk"))
	assert.Equal(t, before, cbkReadAll(t, ctx, h, "restored"), "restored rows must equal the source at backup time")

	got, err := h.tables.GetTable(ctx, &btapb.GetTableRequest{Name: cbkParent + "/tables/restored", View: btapb.Table_FULL})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"cf1", "cf2", "agg"}, sortedKeys(got.GetColumnFamilies()))
	assert.Empty(t, got.GetColumnFamilies()["cf1"].GetGcRule().GetRule(), "GC policies are not inherited")
	assert.True(t, proto.Equal(sumFamily().GetValueType(), got.GetColumnFamilies()["agg"].GetValueType()), "value types are part of the schema")
	assert.False(t, got.GetDeletionProtection(), "deletion protection is not inherited")
	require.NotNil(t, got.GetRestoreInfo())
	assert.Equal(t, btapb.RestoreSourceType_BACKUP, got.GetRestoreInfo().GetSourceType())
	assert.Equal(t, cbkC1+"/backups/bk", got.GetRestoreInfo().GetBackupInfo().GetBackup())
	assert.Equal(t, cbkParent+"/tables/src", got.GetRestoreInfo().GetBackupInfo().GetSourceTable())

	// Without the source's max-versions(5) rule all six new versions stay.
	tbl := h.client.Open("restored")
	for i := 0; i < 6; i++ {
		m := bigtable.NewMutation()
		m.Set("cf1", "z", bigtable.Timestamp(1_700_000_000_000_000+int64(i)*1000), []byte{byte(i)})
		require.NoError(t, tbl.Apply(ctx, "gc", m))
	}
	r, err := tbl.ReadRow(ctx, "gc")
	require.NoError(t, err)
	assert.Len(t, r["cf1"], 6)

	// The deleted source's ID is free again and restores identically.
	require.NoError(t, h.admin.RestoreTable(ctx, "src", cbkCluster1, "bk"))
	assert.Equal(t, before, cbkReadAll(t, ctx, h, "src"))

	cbkCode(t, codes.AlreadyExists, h.admin.RestoreTable(ctx, "restored", cbkCluster1, "bk"))
	cbkCode(t, codes.NotFound, h.admin.RestoreTable(ctx, "other", cbkCluster1, "missing"))
}

// TestConformanceBackupOperationMetadata covers the typed LRO metadata and
// responses of CreateBackup, CopyBackup and RestoreTable (including the
// OptimizeRestoredTable operation it names), all retrievable by name.
func TestConformanceBackupOperationMetadata(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	cbkCreateSourceTable(t, ctx, h, cbkParent, "src")
	now := time.Now()

	op, err := h.tables.CreateBackup(ctx, &btapb.CreateBackupRequest{
		Parent: cbkC1, BackupId: "bk",
		Backup: &btapb.Backup{SourceTable: cbkParent + "/tables/src", ExpireTime: timestamppb.New(now.Add(7 * cbkDay))},
	})
	require.NoError(t, err)
	require.True(t, op.GetDone())
	createMD := &btapb.CreateBackupMetadata{}
	require.NoError(t, op.GetMetadata().UnmarshalTo(createMD))
	assert.Equal(t, cbkC1+"/backups/bk", createMD.GetName())
	assert.Equal(t, cbkParent+"/tables/src", createMD.GetSourceTable())
	assert.NotNil(t, createMD.GetStartTime())
	assert.NotNil(t, createMD.GetEndTime())
	backup := &btapb.Backup{}
	require.NoError(t, op.GetResponse().UnmarshalTo(backup))
	assert.Equal(t, btapb.Backup_READY, backup.GetState())

	op, err = h.tables.CopyBackup(ctx, &btapb.CopyBackupRequest{
		Parent: cbkC2, BackupId: "copy", SourceBackup: cbkC1 + "/backups/bk", ExpireTime: timestamppb.New(now.Add(7 * cbkDay)),
	})
	require.NoError(t, err)
	copyMD := &btapb.CopyBackupMetadata{}
	require.NoError(t, op.GetMetadata().UnmarshalTo(copyMD))
	assert.Equal(t, cbkC2+"/backups/copy", copyMD.GetName())
	assert.Equal(t, cbkC1+"/backups/bk", copyMD.GetSourceBackupInfo().GetBackup())
	assert.EqualValues(t, 100, copyMD.GetProgress().GetProgressPercent())

	op, err = h.tables.RestoreTable(ctx, &btapb.RestoreTableRequest{
		Parent: cbkParent, TableId: "restored", Source: &btapb.RestoreTableRequest_Backup{Backup: cbkC2 + "/backups/copy"},
	})
	require.NoError(t, err)
	restoreMD := &btapb.RestoreTableMetadata{}
	require.NoError(t, op.GetMetadata().UnmarshalTo(restoreMD))
	assert.Equal(t, cbkParent+"/tables/restored", restoreMD.GetName())
	assert.Equal(t, btapb.RestoreSourceType_BACKUP, restoreMD.GetSourceType())
	assert.Equal(t, cbkC2+"/backups/copy", restoreMD.GetBackupInfo().GetBackup())
	assert.Equal(t, cbkC1+"/backups/bk", restoreMD.GetBackupInfo().GetSourceBackup())
	require.NotEmpty(t, restoreMD.GetOptimizeTableOperationName())
	restored := &btapb.Table{}
	require.NoError(t, op.GetResponse().UnmarshalTo(restored))
	assert.Equal(t, cbkParent+"/tables/restored", restored.GetName())

	optimize, err := h.ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: restoreMD.GetOptimizeTableOperationName()})
	require.NoError(t, err)
	optimizeMD := &btapb.OptimizeRestoredTableMetadata{}
	require.NoError(t, optimize.GetMetadata().UnmarshalTo(optimizeMD))
	assert.Equal(t, cbkParent+"/tables/restored", optimizeMD.GetName())

	fetched, err := h.ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()})
	require.NoError(t, err)
	assert.True(t, proto.Equal(op, fetched))
}

// TestConformanceBackupCopyRules covers CopyBackup: a copy is always a
// standard backup, a copy of a copy is rejected, a backup within 24 hours of
// expiring can't be copied, the copy's expire_time is 6 hours to 30 days from
// the copy request (not from the source's creation), and the copy is an
// independent snapshot that survives deleting the source table and backup.
func TestConformanceBackupCopyRules(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	cbkCreateSourceTable(t, ctx, h, cbkParent, "src")
	before := cbkReadAll(t, ctx, h, "src")
	now := time.Now()
	copyTo := func(src, dst string, expire time.Time) error {
		return h.admin.CopyBackup(ctx, cbkCluster1, src, cbkProject, cbkInstance, cbkCluster2, dst, expire)
	}

	require.NoError(t, h.admin.CreateBackup(ctx, "src", cbkCluster1, "orig", now.Add(20*cbkDay)))
	require.NoError(t, h.admin.CreateBackupWithOptions(ctx, "src", cbkCluster1, "hot", bigtable.WithExpiry(now.Add(7*cbkDay)), bigtable.WithHotBackup()))

	require.NoError(t, copyTo("orig", "copy1", now.Add(7*cbkDay)))
	info, err := h.admin.BackupInfo(ctx, cbkCluster2, "copy1")
	require.NoError(t, err)
	assert.Equal(t, cbkC1+"/backups/orig", info.SourceBackup)
	assert.Equal(t, "src", info.SourceTable)
	assert.Equal(t, bigtable.BackupTypeStandard, info.BackupType)
	assert.Equal(t, "READY", info.State)

	require.NoError(t, copyTo("hot", "hot-copy", now.Add(7*cbkDay)))
	info, err = h.admin.BackupInfo(ctx, cbkCluster2, "hot-copy")
	require.NoError(t, err)
	assert.Equal(t, bigtable.BackupTypeStandard, info.BackupType, "a copy of a hot backup is always standard")

	err = h.admin.CopyBackup(ctx, cbkCluster2, "copy1", cbkProject, cbkInstance, cbkCluster1, "copy-of-copy", now.Add(7*cbkDay))
	cbkCode(t, codes.FailedPrecondition, err, "copy of a copy")
	cbkCode(t, codes.InvalidArgument, copyTo("orig", "too-long", now.Add(31*cbkDay)))
	cbkCode(t, codes.InvalidArgument, copyTo("orig", "too-short", now.Add(5*time.Hour)))
	cbkCode(t, codes.AlreadyExists, copyTo("orig", "copy1", now.Add(7*cbkDay)))
	cbkCode(t, codes.NotFound, copyTo("missing", "x-copy", now.Add(7*cbkDay)))
	_, err = h.tables.CopyBackup(ctx, &btapb.CopyBackupRequest{Parent: cbkC2, BackupId: "noexpire", SourceBackup: cbkC1 + "/backups/orig"})
	cbkCode(t, codes.InvalidArgument, err, "missing expire_time")

	// The 30-day limit is measured from the copy, even for an old source.
	cbkEditBackup(t, ctx, h.srv.s, cbkC1+"/backups/orig", func(b *btapb.Backup) {
		b.StartTime = timestamppb.New(now.Add(-20 * cbkDay))
		b.EndTime = b.StartTime
	})
	require.NoError(t, copyTo("orig", "late-copy", now.Add(25*cbkDay)))

	// A backup within 24 hours of expiring can't be copied.
	require.NoError(t, h.admin.CreateBackup(ctx, "src", cbkCluster1, "expiring", now.Add(7*time.Hour)))
	cbkCode(t, codes.FailedPrecondition, copyTo("expiring", "expiring-copy", now.Add(7*cbkDay)))

	// UpdateBackup on a copy keeps the 30-day limit.
	cbkCode(t, codes.InvalidArgument, h.admin.UpdateBackup(ctx, cbkCluster2, "copy1", now.Add(31*cbkDay)))
	require.NoError(t, h.admin.UpdateBackup(ctx, cbkCluster2, "copy1", now.Add(29*cbkDay)))

	// The copy is independent of the source table and the original backup.
	require.NoError(t, h.admin.DeleteTable(ctx, "src"))
	require.NoError(t, h.admin.DeleteBackup(ctx, cbkCluster1, "orig"))
	_, err = h.admin.BackupInfo(ctx, cbkCluster1, "orig")
	cbkCode(t, codes.NotFound, err)
	require.NoError(t, h.admin.RestoreTable(ctx, "from-copy", cbkCluster2, "copy1"))
	assert.Equal(t, before, cbkReadAll(t, ctx, h, "from-copy"))
}

// TestConformanceBackupListFilterOrderPagination covers ListBackups: the '-'
// cluster wildcard, the documented filter grammar (fields, comparison
// operators, HAS, parenthesized AND terms, case-insensitive field names),
// order_by (default start_time desc) and pagination.
func TestConformanceBackupListFilterOrderPagination(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	cbkCreateSourceTable(t, ctx, h, cbkParent, "alpha")
	require.NoError(t, h.admin.CreateTable(ctx, "beta"))
	now := time.Now()

	require.NoError(t, h.admin.CreateBackup(ctx, "alpha", cbkCluster1, "bk-a", now.Add(10*cbkDay)))
	require.NoError(t, h.admin.CreateBackupWithOptions(ctx, "beta", cbkCluster1, "bk-b", bigtable.WithExpiry(now.Add(5*cbkDay)), bigtable.WithHotBackup()))
	require.NoError(t, h.admin.CreateBackup(ctx, "alpha", cbkCluster2, "bk-c", now.Add(20*cbkDay)))
	require.NoError(t, h.admin.CopyBackup(ctx, cbkCluster1, "bk-a", cbkProject, cbkInstance, cbkCluster2, "bk-d", now.Add(8*cbkDay)))

	iterate := func(cluster string) []string {
		var names []string
		it := h.admin.Backups(ctx, cluster)
		for {
			b, err := it.Next()
			if err == iterator.Done {
				break
			}
			require.NoError(t, err)
			names = append(names, b.Name)
		}
		sort.Strings(names)
		return names
	}
	assert.Equal(t, []string{"bk-a", "bk-b", "bk-c", "bk-d"}, iterate("-"))
	assert.Equal(t, []string{"bk-a", "bk-b"}, iterate(cbkCluster1))
	assert.Equal(t, []string{"bk-c", "bk-d"}, iterate(cbkCluster2))

	list := func(filter, orderBy string) ([]string, error) {
		resp, err := h.tables.ListBackups(ctx, &btapb.ListBackupsRequest{Parent: cbkParent + "/clusters/-", Filter: filter, OrderBy: orderBy})
		return cbkNames(resp.GetBackups()), err
	}
	mustList := func(filter, orderBy string) []string {
		t.Helper()
		names, err := list(filter, orderBy)
		require.NoError(t, err, "filter %q order_by %q", filter, orderBy)
		return names
	}

	// Ordering: the default is most recently created first.
	assert.Equal(t, []string{"bk-d", "bk-c", "bk-b", "bk-a"}, mustList("", ""))
	assert.Equal(t, []string{"bk-a", "bk-b", "bk-c", "bk-d"}, mustList("", "name"))
	assert.Equal(t, []string{"bk-d", "bk-c", "bk-b", "bk-a"}, mustList("", "name desc"))
	assert.Equal(t, []string{"bk-c", "bk-a", "bk-d", "bk-b"}, mustList("", "expire_time desc"))
	assert.Equal(t, []string{"bk-a", "bk-b", "bk-c", "bk-d"}, mustList("", "  start_time   asc "))
	for _, bad := range []string{"bogus", "name sideways", "name desc extra"} {
		_, err := list("", bad)
		cbkCode(t, codes.InvalidArgument, err, "order_by %q", bad)
	}

	// Filtering.
	dayAfter := func(d int) string { return now.Add(time.Duration(d) * cbkDay).UTC().Format(time.RFC3339) }
	for filter, want := range map[string][]string{
		`source_table:alpha`:                             {"bk-a", "bk-c", "bk-d"},
		`source_table = "` + cbkParent + `/tables/beta"`: {"bk-b"},
		`backup_type = HOT`:                              {"bk-b"},
		`backup_type:hot`:                                {"bk-b"},
		`state:READY AND source_table:beta`:              {"bk-b"},
		`state = READY and source_table != "` + cbkParent + `/tables/beta"`: {"bk-a", "bk-c", "bk-d"},
		`name:"bk-c"`:        {"bk-c"},
		`NAME:bk-a`:          {"bk-a"},
		`source_backup:bk-a`: {"bk-d"},
		`(name:bk) AND (expire_time < "` + dayAfter(9) + `")`: {"bk-b", "bk-d"},
		`expire_time >= "` + dayAfter(10) + `"`:               {"bk-a", "bk-c"},
		`start_time > "2000-01-01T00:00:00Z"`:                 {"bk-a", "bk-b", "bk-c", "bk-d"},
		`size_bytes > 0`:                                      {"bk-a", "bk-c", "bk-d"},
		`size_bytes = 0`:                                      {"bk-b"},
		`state:CREATING`:                                      nil,
	} {
		got, err := list(filter, "name")
		require.NoError(t, err, "filter %q", filter)
		assert.Equal(t, want, nilIfEmpty(got), "filter %q", filter)
	}
	for _, bad := range []string{`foo = bar`, `name:a OR name:b`, `NOT name:a`, `start_time < yesterday`, `size_bytes > big`} {
		_, err := list(bad, "")
		cbkCode(t, codes.InvalidArgument, err, "filter %q", bad)
	}

	// Pagination over the wildcard with stable ordering.
	var paged []string
	token := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 10)
		resp, err := h.tables.ListBackups(ctx, &btapb.ListBackupsRequest{Parent: cbkParent + "/clusters/-", OrderBy: "name", PageSize: 1, PageToken: token})
		require.NoError(t, err)
		require.LessOrEqual(t, len(resp.GetBackups()), 1)
		paged = append(paged, cbkNames(resp.GetBackups())...)
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	assert.Equal(t, []string{"bk-a", "bk-b", "bk-c", "bk-d"}, paged)
	_, err := h.tables.ListBackups(ctx, &btapb.ListBackupsRequest{Parent: cbkParent + "/clusters/-", PageToken: "not-a-token!"})
	cbkCode(t, codes.InvalidArgument, err, "invalid page_token")
	resp, err := h.tables.ListBackups(ctx, &btapb.ListBackupsRequest{Parent: cbkParent + "/clusters/-", PageSize: -1})
	require.NoError(t, err, "page_size <= 0 means the server maximum")
	assert.Len(t, resp.GetBackups(), 4)
	assert.Empty(t, resp.GetNextPageToken())
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// TestConformanceBackupExpiredHiddenAndPurged covers expiration: once
// expire_time passes a backup is invisible (Get/List/Copy/Restore NotFound),
// stops blocking cluster deletion, and maintenance purges its descriptor,
// manifest and snapshot rows.
func TestConformanceBackupExpiredHiddenAndPurged(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	cbkCreateSourceTable(t, ctx, h, cbkParent, "src")
	now := time.Now()
	require.NoError(t, h.admin.CreateBackup(ctx, "src", cbkCluster2, "old", now.Add(7*cbkDay)))
	require.NoError(t, h.admin.CreateBackup(ctx, "src", cbkCluster1, "keep", now.Add(7*cbkDay)))

	s := h.srv.s
	name := cbkC2 + "/backups/old"
	snapshotRows := func() int {
		n, err := s.backupBackend.snapshotRows(name).count(ctx, nil)
		require.NoError(t, err)
		return n
	}
	require.Equal(t, 3, snapshotRows())

	cbkEditBackup(t, ctx, s, name, func(b *btapb.Backup) { b.ExpireTime = timestamppb.New(now.Add(-time.Minute)) })
	_, err := h.admin.BackupInfo(ctx, cbkCluster2, "old")
	cbkCode(t, codes.NotFound, err, "expired backup is hidden from Get")
	resp, err := h.tables.ListBackups(ctx, &btapb.ListBackupsRequest{Parent: cbkParent + "/clusters/-"})
	require.NoError(t, err)
	assert.Equal(t, []string{"keep"}, cbkNames(resp.GetBackups()))
	cbkCode(t, codes.NotFound, h.admin.RestoreTable(ctx, "restored", cbkCluster2, "old"))
	cbkCode(t, codes.NotFound, h.admin.CopyBackup(ctx, cbkCluster2, "old", cbkProject, cbkInstance, cbkCluster1, "copy", now.Add(7*cbkDay)))
	_, err = h.tables.UpdateBackup(ctx, &btapb.UpdateBackupRequest{
		Backup:     &btapb.Backup{Name: name, ExpireTime: timestamppb.New(now.Add(7 * cbkDay))},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}},
	})
	cbkCode(t, codes.NotFound, err, "expired backups can't be revived")

	s.runMaintenance(ctx)
	_, ok, err := s.backupBackend.get(ctx, name)
	require.NoError(t, err)
	assert.False(t, ok, "expired backup descriptor purged")
	_, err = s.backupBackend.manifest(ctx, s.db, name)
	assert.Error(t, err, "expired backup manifest purged")
	assert.Zero(t, snapshotRows(), "expired backup snapshot rows purged")
	_, ok, err = s.backupBackend.get(ctx, cbkC1+"/backups/keep")
	require.NoError(t, err)
	assert.True(t, ok, "live backups are kept")
}

// TestConformanceBackupBlocksClusterAndInstanceDeletion covers "you can't
// delete a cluster that contains backups or an instance that has backups in
// any cluster"; expired backups don't count.
func TestConformanceBackupBlocksClusterAndInstanceDeletion(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	cbkCreateSourceTable(t, ctx, h, cbkParent, "src")
	now := time.Now()
	require.NoError(t, h.admin.CreateBackup(ctx, "src", cbkCluster2, "in-two", now.Add(7*cbkDay)))
	require.NoError(t, h.admin.CreateBackup(ctx, "src", cbkCluster1, "in-one", now.Add(7*cbkDay)))

	_, err := h.insts.DeleteCluster(ctx, &btapb.DeleteClusterRequest{Name: cbkC2})
	cbkCode(t, codes.FailedPrecondition, err, "cluster with a backup")
	cbkEditBackup(t, ctx, h.srv.s, cbkC2+"/backups/in-two", func(b *btapb.Backup) { b.ExpireTime = timestamppb.New(now.Add(-time.Minute)) })
	_, err = h.insts.DeleteCluster(ctx, &btapb.DeleteClusterRequest{Name: cbkC2})
	require.NoError(t, err, "expired backups don't block cluster deletion")

	_, err = h.insts.DeleteInstance(ctx, &btapb.DeleteInstanceRequest{Name: cbkParent})
	cbkCode(t, codes.FailedPrecondition, err, "instance with a backup")
	require.NoError(t, h.admin.DeleteBackup(ctx, cbkCluster1, "in-one"))
	_, err = h.insts.DeleteInstance(ctx, &btapb.DeleteInstanceRequest{Name: cbkParent})
	require.NoError(t, err)
}

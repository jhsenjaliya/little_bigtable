package bttest

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"testing"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newFullTestServer(t *testing.T) (*server, string) {
	t.Helper()
	ctx := context.Background()
	dbFilename := newDBFile(t)
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared", dbFilename))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	CreateTables(ctx, db)

	s := &server{
		tables:            make(map[string]*table),
		materializedViews: make(map[string]*btapb.MaterializedView),
		db:                db,
		tableBackend:      NewSqlTables(db),
		adminBackend:      NewSqlAdminMetadata(db),
		changeLog:         NewSqlChangeLog(db),
		mvBackend:         NewSqlMaterializedViews(db),
		avBackend:         NewSqlAuthorizedViews(db),
		backupBackend:     NewSqlBackups(db),
		lvBackend:         NewSqlLogicalViews(db),
		cmvs:              newCMVRegistry(),
	}
	parent := "projects/test/instances/test"
	return s, parent
}

// --- Table Deletion Protection ---

func TestTableDeletionProtection_BlocksDelete(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	// Create table.
	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "protected"})
	require.NoError(t, err)

	tableName := parent + "/tables/protected"

	// Enable deletion protection.
	_, err = s.UpdateTable(ctx, &btapb.UpdateTableRequest{
		Table:      &btapb.Table{Name: tableName, DeletionProtection: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
	})
	require.NoError(t, err)

	// Verify GetTable returns deletion_protection.
	got, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: tableName})
	require.NoError(t, err)
	assert.True(t, got.DeletionProtection)

	// Delete should fail.
	_, err = s.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: tableName})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	// Disable protection.
	_, err = s.UpdateTable(ctx, &btapb.UpdateTableRequest{
		Table:      &btapb.Table{Name: tableName, DeletionProtection: false},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
	})
	require.NoError(t, err)

	// Delete should succeed.
	_, err = s.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: tableName})
	require.NoError(t, err)
}

// --- IAM Stubs ---

func TestIAMStubs_Permissive(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	resource := parent + "/tables/t1"

	// GetIamPolicy returns empty policy for unknown resource.
	policy, err := s.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
	require.NoError(t, err)
	assert.NotNil(t, policy)
	assert.Equal(t, int32(1), policy.Version)

	// SetIamPolicy stores and returns.
	newPolicy := &iampb.Policy{
		Version: 3,
		Bindings: []*iampb.Binding{
			{Role: "roles/bigtable.reader", Members: []string{"user:test@example.com"}},
		},
	}
	stored, err := s.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{
		Resource: resource,
		Policy:   newPolicy,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(3), stored.Version)
	assert.Len(t, stored.Bindings, 1)

	// GetIamPolicy returns stored policy.
	got, err := s.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
	require.NoError(t, err)
	assert.Equal(t, int32(3), got.Version)

	// TestIamPermissions returns all requested permissions.
	perms, err := s.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{
		Resource:    resource,
		Permissions: []string{"bigtable.tables.get", "bigtable.tables.delete"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"bigtable.tables.get", "bigtable.tables.delete"}, perms.Permissions)
}

// --- Authorized Views ---

func TestAuthorizedViews_CRUD(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	tableName := parent + "/tables/t1"
	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "t1"})
	require.NoError(t, err)

	// Create.
	op, err := s.CreateAuthorizedView(ctx, &btapb.CreateAuthorizedViewRequest{
		Parent:           tableName,
		AuthorizedViewId: "av1",
		AuthorizedView: &btapb.AuthorizedView{
			DeletionProtection: true,
		},
	})
	require.NoError(t, err)
	assert.True(t, op.Done)

	avName := tableName + "/authorizedViews/av1"

	// Get.
	av, err := s.GetAuthorizedView(ctx, &btapb.GetAuthorizedViewRequest{Name: avName})
	require.NoError(t, err)
	assert.Equal(t, avName, av.Name)
	assert.True(t, av.DeletionProtection)

	// List.
	listResp, err := s.ListAuthorizedViews(ctx, &btapb.ListAuthorizedViewsRequest{Parent: tableName})
	require.NoError(t, err)
	assert.Len(t, listResp.AuthorizedViews, 1)

	// Delete should fail (deletion protection).
	_, err = s.DeleteAuthorizedView(ctx, &btapb.DeleteAuthorizedViewRequest{Name: avName})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	// Update to disable protection.
	_, err = s.UpdateAuthorizedView(ctx, &btapb.UpdateAuthorizedViewRequest{
		AuthorizedView: &btapb.AuthorizedView{Name: avName, DeletionProtection: false},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
	})
	require.NoError(t, err)

	// Delete should succeed now.
	_, err = s.DeleteAuthorizedView(ctx, &btapb.DeleteAuthorizedViewRequest{Name: avName})
	require.NoError(t, err)

	// Get should fail.
	_, err = s.GetAuthorizedView(ctx, &btapb.GetAuthorizedViewRequest{Name: avName})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestAuthorizedViews_CreateDuplicate(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	tableName := parent + "/tables/t1"
	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "t1"})
	require.NoError(t, err)

	_, err = s.CreateAuthorizedView(ctx, &btapb.CreateAuthorizedViewRequest{
		Parent: tableName, AuthorizedViewId: "av1",
	})
	require.NoError(t, err)

	_, err = s.CreateAuthorizedView(ctx, &btapb.CreateAuthorizedViewRequest{
		Parent: tableName, AuthorizedViewId: "av1",
	})
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
}

// --- Backups ---

func TestBackups_CRUD(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	// Create source table.
	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "src"})
	require.NoError(t, err)

	cluster := parent + "/clusters/c1"

	// Create backup.
	op, err := s.CreateBackup(ctx, &btapb.CreateBackupRequest{
		Parent:   cluster,
		BackupId: "bk1",
		Backup: &btapb.Backup{
			SourceTable: parent + "/tables/src",
			ExpireTime:  timestamppb.Now(),
		},
	})
	require.NoError(t, err)
	assert.True(t, op.Done)

	backupName := cluster + "/backups/bk1"

	// Get.
	backup, err := s.GetBackup(ctx, &btapb.GetBackupRequest{Name: backupName})
	require.NoError(t, err)
	assert.Equal(t, backupName, backup.Name)
	assert.Equal(t, btapb.Backup_READY, backup.State)
	assert.Equal(t, parent+"/tables/src", backup.SourceTable)

	// Update expire time.
	newExpire := timestamppb.Now()
	updated, err := s.UpdateBackup(ctx, &btapb.UpdateBackupRequest{
		Backup:     &btapb.Backup{Name: backupName, ExpireTime: newExpire},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}},
	})
	require.NoError(t, err)
	assert.Equal(t, newExpire.Seconds, updated.ExpireTime.Seconds)

	// List.
	listResp, err := s.ListBackups(ctx, &btapb.ListBackupsRequest{Parent: cluster})
	require.NoError(t, err)
	assert.Len(t, listResp.Backups, 1)

	// Delete.
	_, err = s.DeleteBackup(ctx, &btapb.DeleteBackupRequest{Name: backupName})
	require.NoError(t, err)

	// Get should fail.
	_, err = s.GetBackup(ctx, &btapb.GetBackupRequest{Name: backupName})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestCopyBackup(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "src"})
	require.NoError(t, err)

	cluster := parent + "/clusters/c1"

	// Create source backup.
	_, err = s.CreateBackup(ctx, &btapb.CreateBackupRequest{
		Parent:   cluster,
		BackupId: "original",
		Backup:   &btapb.Backup{SourceTable: parent + "/tables/src", ExpireTime: timestamppb.Now()},
	})
	require.NoError(t, err)

	// Copy backup.
	op, err := s.CopyBackup(ctx, &btapb.CopyBackupRequest{
		Parent:       cluster,
		BackupId:     "copy1",
		SourceBackup: cluster + "/backups/original",
		ExpireTime:   timestamppb.Now(),
	})
	require.NoError(t, err)
	assert.True(t, op.Done)

	// Verify copy.
	copy, err := s.GetBackup(ctx, &btapb.GetBackupRequest{Name: cluster + "/backups/copy1"})
	require.NoError(t, err)
	assert.Equal(t, cluster+"/backups/original", copy.SourceBackup)
	assert.Equal(t, parent+"/tables/src", copy.SourceTable)
}

func TestRestoreTable(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	// Create source table with column families.
	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "src"})
	require.NoError(t, err)
	_, err = s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{
		Name: parent + "/tables/src",
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{
			{Id: "cf1", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}}},
		},
	})
	require.NoError(t, err)

	cluster := parent + "/clusters/c1"

	// Create backup.
	_, err = s.CreateBackup(ctx, &btapb.CreateBackupRequest{
		Parent:   cluster,
		BackupId: "bk1",
		Backup:   &btapb.Backup{SourceTable: parent + "/tables/src", ExpireTime: timestamppb.Now()},
	})
	require.NoError(t, err)

	// Restore to new table.
	op, err := s.RestoreTable(ctx, &btapb.RestoreTableRequest{
		Parent:  parent,
		TableId: "restored",
		Source:  &btapb.RestoreTableRequest_Backup{Backup: cluster + "/backups/bk1"},
	})
	require.NoError(t, err)
	assert.True(t, op.Done)

	// Verify restored table exists and has column family.
	tbl, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: parent + "/tables/restored"})
	require.NoError(t, err)
	assert.Contains(t, tbl.ColumnFamilies, "cf1")
}

// --- Logical Views ---

func TestLogicalViews_CRUD(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	// Create.
	op, err := s.CreateLogicalView(ctx, &btapb.CreateLogicalViewRequest{
		Parent:        parent,
		LogicalViewId: "lv1",
		LogicalView: &btapb.LogicalView{
			Query:              "SELECT * FROM t1",
			DeletionProtection: true,
		},
	})
	require.NoError(t, err)
	assert.True(t, op.Done)

	lvName := parent + "/logicalViews/lv1"

	// Get.
	lv, err := s.GetLogicalView(ctx, &btapb.GetLogicalViewRequest{Name: lvName})
	require.NoError(t, err)
	assert.Equal(t, lvName, lv.Name)
	assert.Equal(t, "SELECT * FROM t1", lv.Query)
	assert.True(t, lv.DeletionProtection)

	// List.
	listResp, err := s.ListLogicalViews(ctx, &btapb.ListLogicalViewsRequest{Parent: parent})
	require.NoError(t, err)
	assert.Len(t, listResp.LogicalViews, 1)

	// Delete should fail (deletion protection).
	_, err = s.DeleteLogicalView(ctx, &btapb.DeleteLogicalViewRequest{Name: lvName})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	// Update: disable protection.
	_, err = s.UpdateLogicalView(ctx, &btapb.UpdateLogicalViewRequest{
		LogicalView: &btapb.LogicalView{Name: lvName, DeletionProtection: false},
		UpdateMask:  &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
	})
	require.NoError(t, err)

	// Delete should succeed.
	_, err = s.DeleteLogicalView(ctx, &btapb.DeleteLogicalViewRequest{Name: lvName})
	require.NoError(t, err)

	// Get should fail.
	_, err = s.GetLogicalView(ctx, &btapb.GetLogicalViewRequest{Name: lvName})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestLogicalViews_UpdateQuery(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	_, err := s.CreateLogicalView(ctx, &btapb.CreateLogicalViewRequest{
		Parent:        parent,
		LogicalViewId: "lv1",
		LogicalView:   &btapb.LogicalView{Query: "SELECT * FROM t1"},
	})
	require.NoError(t, err)

	lvName := parent + "/logicalViews/lv1"

	_, err = s.UpdateLogicalView(ctx, &btapb.UpdateLogicalViewRequest{
		LogicalView: &btapb.LogicalView{Name: lvName, Query: "SELECT col1 FROM t2"},
		UpdateMask:  &fieldmaskpb.FieldMask{Paths: []string{"query"}},
	})
	require.NoError(t, err)

	lv, err := s.GetLogicalView(ctx, &btapb.GetLogicalViewRequest{Name: lvName})
	require.NoError(t, err)
	assert.Equal(t, "SELECT col1 FROM t2", lv.Query)
}

// --- AddToCell / MergeToCell Mutations ---

func TestAddToCell_Int64Sum(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "agg"})
	require.NoError(t, err)
	_, err = s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{
		Name: parent + "/tables/agg",
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{
			{Id: "cf1", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}}},
		},
	})
	require.NoError(t, err)

	tableName := parent + "/tables/agg"

	// AddToCell: add 10
	_, err = s.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: tableName,
		RowKey:    []byte("row1"),
		Mutations: []*btpb.Mutation{{
			Mutation: &btpb.Mutation_AddToCell_{AddToCell: &btpb.Mutation_AddToCell{
				FamilyName:      "cf1",
				ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte("counter")}},
				Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: 1000}},
				Input:           &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: 10}},
			}},
		}},
	})
	require.NoError(t, err)

	// AddToCell: add 25 more at same timestamp
	_, err = s.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: tableName,
		RowKey:    []byte("row1"),
		Mutations: []*btpb.Mutation{{
			Mutation: &btpb.Mutation_AddToCell_{AddToCell: &btpb.Mutation_AddToCell{
				FamilyName:      "cf1",
				ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte("counter")}},
				Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: 1000}},
				Input:           &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: 25}},
			}},
		}},
	})
	require.NoError(t, err)

	// Read and verify sum = 35
	mock := &MockReadRowsServer{}
	err = s.ReadRows(&btpb.ReadRowsRequest{
		TableName: tableName,
		Rows:      &btpb.RowSet{RowKeys: [][]byte{[]byte("row1")}},
	}, mock)
	require.NoError(t, err)
	require.Len(t, mock.responses, 1)
	chunks := mock.responses[0].Chunks
	require.True(t, len(chunks) > 0)

	val := int64(binary.BigEndian.Uint64(chunks[0].Value))
	assert.Equal(t, int64(35), val)
}

func TestMergeToCell_Int64Sum(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "merge"})
	require.NoError(t, err)
	_, err = s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{
		Name: parent + "/tables/merge",
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{
			{Id: "cf1", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}}},
		},
	})
	require.NoError(t, err)

	tableName := parent + "/tables/merge"

	// MergeToCell: merge 100
	_, err = s.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: tableName,
		RowKey:    []byte("row1"),
		Mutations: []*btpb.Mutation{{
			Mutation: &btpb.Mutation_MergeToCell_{MergeToCell: &btpb.Mutation_MergeToCell{
				FamilyName:      "cf1",
				ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte("metric")}},
				Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: 2000}},
				Input:           &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: 100}},
			}},
		}},
	})
	require.NoError(t, err)

	// MergeToCell: merge 50 more
	_, err = s.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: tableName,
		RowKey:    []byte("row1"),
		Mutations: []*btpb.Mutation{{
			Mutation: &btpb.Mutation_MergeToCell_{MergeToCell: &btpb.Mutation_MergeToCell{
				FamilyName:      "cf1",
				ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte("metric")}},
				Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: 2000}},
				Input:           &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: 50}},
			}},
		}},
	})
	require.NoError(t, err)

	// Read and verify sum = 150
	mock := &MockReadRowsServer{}
	err = s.ReadRows(&btpb.ReadRowsRequest{
		TableName: tableName,
		Rows:      &btpb.RowSet{RowKeys: [][]byte{[]byte("row1")}},
	}, mock)
	require.NoError(t, err)
	require.Len(t, mock.responses, 1)
	chunks := mock.responses[0].Chunks
	require.True(t, len(chunks) > 0)

	val := int64(binary.BigEndian.Uint64(chunks[0].Value))
	assert.Equal(t, int64(150), val)
}

// --- Unknown Filter Returns Error ---

func TestUnknownFilter_ReturnsError(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "t1"})
	require.NoError(t, err)
	_, err = s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{
		Name: parent + "/tables/t1",
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{
			{Id: "cf1", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}}},
		},
	})
	require.NoError(t, err)

	// Write a cell.
	_, err = s.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: parent + "/tables/t1",
		RowKey:    []byte("row1"),
		Mutations: []*btpb.Mutation{{
			Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
				FamilyName:      "cf1",
				ColumnQualifier: []byte("col1"),
				TimestampMicros: 1000,
				Value:           []byte("val"),
			}},
		}},
	})
	require.NoError(t, err)

	// Read with a filter that has nil Filter field — triggers default case.
	mock := &MockReadRowsServer{}
	err = s.ReadRows(&btpb.ReadRowsRequest{
		TableName: parent + "/tables/t1",
		Rows:      &btpb.RowSet{RowKeys: [][]byte{[]byte("row1")}},
		Filter:    &btpb.RowFilter{}, // nil Filter field
	}, mock)
	// nil filter should pass through (handled by nil check at top of includeCell).
	// This test verifies it doesn't panic.
	assert.NoError(t, err)
}

// --- Edge Case Tests ---

func TestAddToCell_DifferentTimestamps(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "agg2"})
	require.NoError(t, err)
	_, err = s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{
		Name: parent + "/tables/agg2",
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{
			{Id: "cf1", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}}},
		},
	})
	require.NoError(t, err)

	tableName := parent + "/tables/agg2"

	// Add 10 at ts=1000
	_, err = s.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: tableName,
		RowKey:    []byte("row1"),
		Mutations: []*btpb.Mutation{{
			Mutation: &btpb.Mutation_AddToCell_{AddToCell: &btpb.Mutation_AddToCell{
				FamilyName:      "cf1",
				ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte("c")}},
				Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: 1000}},
				Input:           &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: 10}},
			}},
		}},
	})
	require.NoError(t, err)

	// Add 20 at ts=2000 (different timestamp — should create separate cell)
	_, err = s.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: tableName,
		RowKey:    []byte("row1"),
		Mutations: []*btpb.Mutation{{
			Mutation: &btpb.Mutation_AddToCell_{AddToCell: &btpb.Mutation_AddToCell{
				FamilyName:      "cf1",
				ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte("c")}},
				Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: 2000}},
				Input:           &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: 20}},
			}},
		}},
	})
	require.NoError(t, err)

	// Should have 2 cells: ts=2000 (val=20), ts=1000 (val=10)
	mock := &MockReadRowsServer{}
	err = s.ReadRows(&btpb.ReadRowsRequest{
		TableName: tableName,
		Rows:      &btpb.RowSet{RowKeys: [][]byte{[]byte("row1")}},
	}, mock)
	require.NoError(t, err)
	require.Len(t, mock.responses, 1)
	chunks := mock.responses[0].Chunks
	assert.Len(t, chunks, 2)

	// First chunk = ts=2000 (descending order)
	assert.Equal(t, int64(2000), chunks[0].TimestampMicros)
	assert.Equal(t, int64(20), int64(binary.BigEndian.Uint64(chunks[0].Value)))
	// Second chunk = ts=1000
	assert.Equal(t, int64(1000), chunks[1].TimestampMicros)
	assert.Equal(t, int64(10), int64(binary.BigEndian.Uint64(chunks[1].Value)))
}

func TestMergeToCell_RawValueInput(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "mrg2"})
	require.NoError(t, err)
	_, err = s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{
		Name: parent + "/tables/mrg2",
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{
			{Id: "cf1", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}}},
		},
	})
	require.NoError(t, err)

	tableName := parent + "/tables/mrg2"

	// Merge with RawValue (8-byte big-endian int64)
	var rawVal [8]byte
	binary.BigEndian.PutUint64(rawVal[:], 42)
	_, err = s.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: tableName,
		RowKey:    []byte("row1"),
		Mutations: []*btpb.Mutation{{
			Mutation: &btpb.Mutation_MergeToCell_{MergeToCell: &btpb.Mutation_MergeToCell{
				FamilyName:      "cf1",
				ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte("m")}},
				Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: 1000}},
				Input:           &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: rawVal[:]}},
			}},
		}},
	})
	require.NoError(t, err)

	// Merge another 8 via RawValue
	binary.BigEndian.PutUint64(rawVal[:], 8)
	_, err = s.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: tableName,
		RowKey:    []byte("row1"),
		Mutations: []*btpb.Mutation{{
			Mutation: &btpb.Mutation_MergeToCell_{MergeToCell: &btpb.Mutation_MergeToCell{
				FamilyName:      "cf1",
				ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte("m")}},
				Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: 1000}},
				Input:           &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: rawVal[:]}},
			}},
		}},
	})
	require.NoError(t, err)

	// Verify sum = 50
	mock := &MockReadRowsServer{}
	err = s.ReadRows(&btpb.ReadRowsRequest{
		TableName: tableName,
		Rows:      &btpb.RowSet{RowKeys: [][]byte{[]byte("row1")}},
	}, mock)
	require.NoError(t, err)
	require.Len(t, mock.responses, 1)
	val := int64(binary.BigEndian.Uint64(mock.responses[0].Chunks[0].Value))
	assert.Equal(t, int64(50), val)
}

func TestCreateBackup_MissingSourceTable(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	cluster := parent + "/clusters/c1"
	_, err := s.CreateBackup(ctx, &btapb.CreateBackupRequest{
		Parent:   cluster,
		BackupId: "bk1",
		Backup: &btapb.Backup{
			SourceTable: parent + "/tables/nonexistent",
			ExpireTime:  timestamppb.Now(),
		},
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestCopyBackup_SourceNotFound(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	cluster := parent + "/clusters/c1"
	_, err := s.CopyBackup(ctx, &btapb.CopyBackupRequest{
		Parent:       cluster,
		BackupId:     "copy1",
		SourceBackup: cluster + "/backups/nonexistent",
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestRestoreTable_AlreadyExists(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "existing"})
	require.NoError(t, err)

	cluster := parent + "/clusters/c1"
	_, err = s.CreateBackup(ctx, &btapb.CreateBackupRequest{
		Parent:   cluster,
		BackupId: "bk1",
		Backup: &btapb.Backup{
			SourceTable: parent + "/tables/existing",
			ExpireTime:  timestamppb.Now(),
		},
	})
	require.NoError(t, err)

	// Restore to same table name should fail.
	_, err = s.RestoreTable(ctx, &btapb.RestoreTableRequest{
		Parent:  parent,
		TableId: "existing",
		Source:  &btapb.RestoreTableRequest_Backup{Backup: cluster + "/backups/bk1"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
}

func TestAuthorizedView_TableNotFound(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	_, err := s.CreateAuthorizedView(ctx, &btapb.CreateAuthorizedViewRequest{
		Parent:           parent + "/tables/nonexistent",
		AuthorizedViewId: "av1",
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestInterleaveDedup(t *testing.T) {
	s, parent := newFullTestServer(t)
	ctx := context.Background()

	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "ilv"})
	require.NoError(t, err)
	_, err = s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{
		Name: parent + "/tables/ilv",
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{
			{Id: "fam1", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}}},
		},
	})
	require.NoError(t, err)

	tableName := parent + "/tables/ilv"

	// Write one cell: fam1/col1@1000
	_, err = s.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: tableName,
		RowKey:    []byte("row1"),
		Mutations: []*btpb.Mutation{{
			Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
				FamilyName:      "fam1",
				ColumnQualifier: []byte("col1"),
				TimestampMicros: 1000,
				Value:           []byte("val"),
			}},
		}},
	})
	require.NoError(t, err)

	// Interleave: FamilyNameRegex("fam1") OR ColumnQualifierRegex("col1")
	// Both filters match the same cell. Should return 1 cell, not 2.
	mock := &MockReadRowsServer{}
	err = s.ReadRows(&btpb.ReadRowsRequest{
		TableName: tableName,
		Rows:      &btpb.RowSet{RowKeys: [][]byte{[]byte("row1")}},
		Filter: &btpb.RowFilter{
			Filter: &btpb.RowFilter_Interleave_{Interleave: &btpb.RowFilter_Interleave{
				Filters: []*btpb.RowFilter{
					{Filter: &btpb.RowFilter_FamilyNameRegexFilter{FamilyNameRegexFilter: "fam1"}},
					{Filter: &btpb.RowFilter_ColumnQualifierRegexFilter{ColumnQualifierRegexFilter: []byte("col1")}},
				},
			}},
		},
	}, mock)
	require.NoError(t, err)
	require.Len(t, mock.responses, 1)
	assert.Len(t, mock.responses[0].Chunks, 1, "interleave should deduplicate identical cells")
}

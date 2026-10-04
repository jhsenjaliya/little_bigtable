package bttest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	cadProjectID  = "cad-project"
	cadInstanceID = "cad-instance"
	cadClusterID  = "cad-cluster"
	cadProject    = "projects/" + cadProjectID
	cadInstance   = cadProject + "/instances/" + cadInstanceID
	cadCluster    = cadInstance + "/clusters/" + cadClusterID
)

// cadEnv is a gRPC-served emulator on a reusable SQLite file so tests can
// restart the server and observe what was persisted.
type cadEnv struct {
	t      *testing.T
	ctx    context.Context
	dbFile string
	db     *sql.DB
	srv    *Server
	conn   *grpc.ClientConn
	ops    longrunningpb.OperationsClient
	ia     btapb.BigtableInstanceAdminClient
	ta     btapb.BigtableTableAdminClient
	closed bool
}

// cadStrictAdmin enables strict admin validation (the production contract:
// resources must exist) for the duration of the test.
func cadStrictAdmin(t *testing.T) {
	t.Helper()
	prevDialect, prevStrict := currentDialect(), isStrictAdmin()
	ConfigureStorage("sqlite3", true)
	t.Cleanup(func() { ConfigureStorage(string(prevDialect), prevStrict) })
}

func cadOpen(t *testing.T, dbFile string) *cadEnv {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared", dbFile))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	require.NoError(t, CreateTables(ctx, db))
	srv, err := NewServer("127.0.0.1:0", db)
	require.NoError(t, err)
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	e := &cadEnv{
		t: t, ctx: ctx, dbFile: dbFile, db: db, srv: srv, conn: conn,
		ops: longrunningpb.NewOperationsClient(conn),
		ia:  btapb.NewBigtableInstanceAdminClient(conn),
		ta:  btapb.NewBigtableTableAdminClient(conn),
	}
	t.Cleanup(e.close)
	return e
}

// cadNewEnv starts a strict-admin emulator on a fresh database.
func cadNewEnv(t *testing.T) *cadEnv {
	t.Helper()
	cadStrictAdmin(t)
	return cadOpen(t, newDBFile(t))
}

func (e *cadEnv) close() {
	if e.closed {
		return
	}
	e.closed = true
	_ = e.conn.Close()
	e.srv.Close()
	_ = e.db.Close()
}

// restart stops the server and serves the same database file again.
func (e *cadEnv) restart() *cadEnv {
	e.t.Helper()
	e.close()
	return cadOpen(e.t, e.dbFile)
}

// createInstance creates cadInstance with one cluster, cadCluster.
func (e *cadEnv) createInstance() *longrunningpb.Operation {
	e.t.Helper()
	op, err := e.ia.CreateInstance(e.ctx, &btapb.CreateInstanceRequest{
		Parent:     cadProject,
		InstanceId: cadInstanceID,
		Instance:   &btapb.Instance{DisplayName: "Conformance instance"},
		Clusters:   map[string]*btapb.Cluster{cadClusterID: {ServeNodes: 1}},
	})
	require.NoError(e.t, err)
	return op
}

func (e *cadEnv) createTable(id string, families ...string) string {
	e.t.Helper()
	cfs := map[string]*btapb.ColumnFamily{}
	for _, f := range families {
		cfs[f] = &btapb.ColumnFamily{}
	}
	_, err := e.ta.CreateTable(e.ctx, &btapb.CreateTableRequest{
		Parent: cadInstance, TableId: id, Table: &btapb.Table{ColumnFamilies: cfs},
	})
	require.NoError(e.t, err)
	return cadInstance + "/tables/" + id
}

// cadDescriptorSet is a valid serialized FileDescriptorSet for schema bundles.
func cadDescriptorSet(t *testing.T) []byte {
	t.Helper()
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto),
	}}
	raw, err := proto.Marshal(set)
	require.NoError(t, err)
	return raw
}

func cadTypeURL(m proto.Message) string {
	return "type.googleapis.com/" + string(proto.MessageName(m))
}

// cadCheckOperation asserts the documented LRO shape: done, typed metadata
// and response, a resource-scoped name, and that GetOperation and
// WaitOperation return the registered operation.
func (e *cadEnv) cadCheckOperation(label string, op *longrunningpb.Operation, resource string, wantMeta, wantResp proto.Message) {
	e.t.Helper()
	require.NotNil(e.t, op, label)
	require.True(e.t, op.GetDone(), "%s: local operations complete synchronously", label)
	require.Truef(e.t, strings.HasPrefix(op.GetName(), "operations/"+resource+"/locations/"),
		"%s: operation name %q is not scoped to %q", label, op.GetName(), resource)
	require.Equal(e.t, cadTypeURL(wantMeta), op.GetMetadata().GetTypeUrl(), "%s metadata type", label)
	require.NoError(e.t, anypb.UnmarshalTo(op.GetMetadata(), wantMeta, proto.UnmarshalOptions{}), label)
	require.Nil(e.t, op.GetError(), label)
	require.Equal(e.t, cadTypeURL(wantResp), op.GetResponse().GetTypeUrl(), "%s response type", label)
	require.NoError(e.t, anypb.UnmarshalTo(op.GetResponse(), wantResp, proto.UnmarshalOptions{}), label)

	got, err := e.ops.GetOperation(e.ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()})
	require.NoError(e.t, err, label)
	require.True(e.t, proto.Equal(op, got), "%s: GetOperation differs from the returned operation", label)
	waited, err := e.ops.WaitOperation(e.ctx, &longrunningpb.WaitOperationRequest{Name: op.GetName()})
	require.NoError(e.t, err, label)
	require.True(e.t, proto.Equal(op, waited), "%s: WaitOperation differs from the returned operation", label)
}

// TestConformanceOperationsMetadataTypes checks that every LRO-returning
// admin RPC registers an operation carrying the metadata and response types
// declared by google.longrunning.operation_info in the admin protos.
func TestConformanceOperationsMetadataTypes(t *testing.T) {
	e := cadNewEnv(t)
	ctx := e.ctx
	weekLater := timestamppb.New(time.Now().Add(7 * 24 * time.Hour))

	// --- Instance admin ---
	createReqOp := e.createInstance()
	createMeta := &btapb.CreateInstanceMetadata{}
	e.cadCheckOperation("CreateInstance", createReqOp, cadInstance, createMeta, &btapb.Instance{})
	require.Equal(t, cadInstanceID, createMeta.GetOriginalRequest().GetInstanceId())
	require.NotNil(t, createMeta.GetRequestTime())
	require.NotNil(t, createMeta.GetFinishTime())

	op, err := e.ia.PartialUpdateInstance(ctx, &btapb.PartialUpdateInstanceRequest{
		Instance:   &btapb.Instance{Name: cadInstance, DisplayName: "Renamed instance"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}},
	})
	require.NoError(t, err)
	inst := &btapb.Instance{}
	e.cadCheckOperation("PartialUpdateInstance", op, cadInstance, &btapb.UpdateInstanceMetadata{}, inst)
	require.Equal(t, "Renamed instance", inst.GetDisplayName())

	clusterB := cadInstance + "/clusters/cad-cluster-b"
	op, err = e.ia.CreateCluster(ctx, &btapb.CreateClusterRequest{Parent: cadInstance, ClusterId: "cad-cluster-b", Cluster: &btapb.Cluster{ServeNodes: 1}})
	require.NoError(t, err)
	e.cadCheckOperation("CreateCluster", op, clusterB, &btapb.CreateClusterMetadata{}, &btapb.Cluster{})

	op, err = e.ia.UpdateCluster(ctx, &btapb.Cluster{Name: clusterB, ServeNodes: 3})
	require.NoError(t, err)
	cluster := &btapb.Cluster{}
	e.cadCheckOperation("UpdateCluster", op, clusterB, &btapb.UpdateClusterMetadata{}, cluster)
	require.EqualValues(t, 3, cluster.GetServeNodes())

	op, err = e.ia.PartialUpdateCluster(ctx, &btapb.PartialUpdateClusterRequest{
		Cluster:    &btapb.Cluster{Name: clusterB, ServeNodes: 4},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"serve_nodes"}},
	})
	require.NoError(t, err)
	e.cadCheckOperation("PartialUpdateCluster", op, clusterB, &btapb.PartialUpdateClusterMetadata{}, &btapb.Cluster{})

	profile := cadInstance + "/appProfiles/cad-profile"
	_, err = e.ia.CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{
		Parent: cadInstance, AppProfileId: "cad-profile",
		AppProfile: &btapb.AppProfile{RoutingPolicy: &btapb.AppProfile_MultiClusterRoutingUseAny_{MultiClusterRoutingUseAny: &btapb.AppProfile_MultiClusterRoutingUseAny{}}},
	})
	require.NoError(t, err)
	op, err = e.ia.UpdateAppProfile(ctx, &btapb.UpdateAppProfileRequest{
		AppProfile: &btapb.AppProfile{Name: profile, Description: "updated"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
	})
	require.NoError(t, err)
	ap := &btapb.AppProfile{}
	e.cadCheckOperation("UpdateAppProfile", op, profile, &btapb.UpdateAppProfileMetadata{}, ap)
	require.Equal(t, "updated", ap.GetDescription())

	// --- Table admin ---
	src := e.createTable("lro-src", "cf")
	op, err = e.ta.UpdateTable(ctx, &btapb.UpdateTableRequest{
		Table:      &btapb.Table{Name: src, DeletionProtection: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
	})
	require.NoError(t, err)
	updMeta := &btapb.UpdateTableMetadata{}
	tbl := &btapb.Table{}
	e.cadCheckOperation("UpdateTable", op, src, updMeta, tbl)
	require.Equal(t, src, updMeta.GetName())
	require.True(t, tbl.GetDeletionProtection())

	undel := e.createTable("lro-undelete", "cf")
	_, err = e.ta.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: undel})
	require.NoError(t, err)
	op, err = e.ta.UndeleteTable(ctx, &btapb.UndeleteTableRequest{Name: undel})
	require.NoError(t, err)
	undelMeta := &btapb.UndeleteTableMetadata{}
	e.cadCheckOperation("UndeleteTable", op, undel, undelMeta, &btapb.Table{})
	require.Equal(t, undel, undelMeta.GetName())

	av := src + "/authorizedViews/lro-av"
	op, err = e.ta.CreateAuthorizedView(ctx, &btapb.CreateAuthorizedViewRequest{
		Parent: src, AuthorizedViewId: "lro-av",
		AuthorizedView: &btapb.AuthorizedView{AuthorizedView: &btapb.AuthorizedView_SubsetView_{
			SubsetView: &btapb.AuthorizedView_SubsetView{RowPrefixes: [][]byte{[]byte("a")}},
		}},
	})
	require.NoError(t, err)
	avResp := &btapb.AuthorizedView{}
	// The create operation is scoped to the parent table.
	e.cadCheckOperation("CreateAuthorizedView", op, src, &btapb.CreateAuthorizedViewMetadata{}, avResp)
	require.Equal(t, av, avResp.GetName())
	op, err = e.ta.UpdateAuthorizedView(ctx, &btapb.UpdateAuthorizedViewRequest{
		AuthorizedView: &btapb.AuthorizedView{Name: av, DeletionProtection: true},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
	})
	require.NoError(t, err)
	e.cadCheckOperation("UpdateAuthorizedView", op, av, &btapb.UpdateAuthorizedViewMetadata{}, &btapb.AuthorizedView{})

	sb := src + "/schemaBundles/lro-sb"
	protoSchema := &btapb.SchemaBundle_ProtoSchema{ProtoSchema: &btapb.ProtoSchema{ProtoDescriptors: cadDescriptorSet(t)}}
	op, err = e.ta.CreateSchemaBundle(ctx, &btapb.CreateSchemaBundleRequest{
		Parent: src, SchemaBundleId: "lro-sb", SchemaBundle: &btapb.SchemaBundle{Type: protoSchema},
	})
	require.NoError(t, err)
	sbMeta := &btapb.CreateSchemaBundleMetadata{}
	e.cadCheckOperation("CreateSchemaBundle", op, sb, sbMeta, &btapb.SchemaBundle{})
	require.Equal(t, sb, sbMeta.GetName())
	op, err = e.ta.UpdateSchemaBundle(ctx, &btapb.UpdateSchemaBundleRequest{
		SchemaBundle: &btapb.SchemaBundle{Name: sb, Type: protoSchema},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"proto_schema"}},
	})
	require.NoError(t, err)
	e.cadCheckOperation("UpdateSchemaBundle", op, sb, &btapb.UpdateSchemaBundleMetadata{}, &btapb.SchemaBundle{})

	backup := cadCluster + "/backups/lro-backup"
	op, err = e.ta.CreateBackup(ctx, &btapb.CreateBackupRequest{
		Parent: cadCluster, BackupId: "lro-backup",
		Backup: &btapb.Backup{SourceTable: src, ExpireTime: weekLater},
	})
	require.NoError(t, err)
	bMeta := &btapb.CreateBackupMetadata{}
	e.cadCheckOperation("CreateBackup", op, backup, bMeta, &btapb.Backup{})
	require.Equal(t, backup, bMeta.GetName())
	require.Equal(t, src, bMeta.GetSourceTable())

	copyName := cadCluster + "/backups/lro-copy"
	op, err = e.ta.CopyBackup(ctx, &btapb.CopyBackupRequest{
		Parent: cadCluster, BackupId: "lro-copy", SourceBackup: backup, ExpireTime: weekLater,
	})
	require.NoError(t, err)
	copyMeta := &btapb.CopyBackupMetadata{}
	e.cadCheckOperation("CopyBackup", op, copyName, copyMeta, &btapb.Backup{})
	require.Equal(t, backup, copyMeta.GetSourceBackupInfo().GetBackup())

	restored := cadInstance + "/tables/lro-restored"
	op, err = e.ta.RestoreTable(ctx, &btapb.RestoreTableRequest{
		Parent: cadInstance, TableId: "lro-restored", Source: &btapb.RestoreTableRequest_Backup{Backup: backup},
	})
	require.NoError(t, err)
	restoreMeta := &btapb.RestoreTableMetadata{}
	e.cadCheckOperation("RestoreTable", op, restored, restoreMeta, &btapb.Table{})
	require.Equal(t, btapb.RestoreSourceType_BACKUP, restoreMeta.GetSourceType())
	require.NotEmpty(t, restoreMeta.GetOptimizeTableOperationName())
	optimize, err := e.ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: restoreMeta.GetOptimizeTableOperationName()})
	require.NoError(t, err)
	e.cadCheckOperation("OptimizeRestoredTable", optimize, restored, &btapb.OptimizeRestoredTableMetadata{}, &emptypb.Empty{})

	// --- SQL-backed views (depend on the GoogleSQL engine) ---
	t.Run("LogicalView", func(t *testing.T) {
		lv := cadInstance + "/logicalViews/lro-lv"
		query := "SELECT _key FROM `lro-src`"
		op, err := e.ia.CreateLogicalView(ctx, &btapb.CreateLogicalViewRequest{
			Parent: cadInstance, LogicalViewId: "lro-lv", LogicalView: &btapb.LogicalView{Query: query},
		})
		if status.Code(err) == codes.Unimplemented {
			t.Skipf("GoogleSQL is unavailable in this build: %v", err)
		}
		require.NoError(t, err)
		e.cadCheckOperation("CreateLogicalView", op, lv, &btapb.CreateLogicalViewMetadata{}, &btapb.LogicalView{})
		op, err = e.ia.UpdateLogicalView(ctx, &btapb.UpdateLogicalViewRequest{
			LogicalView: &btapb.LogicalView{Name: lv, Query: query},
			UpdateMask:  &fieldmaskpb.FieldMask{Paths: []string{"query"}},
		})
		require.NoError(t, err)
		e.cadCheckOperation("UpdateLogicalView", op, lv, &btapb.UpdateLogicalViewMetadata{}, &btapb.LogicalView{})
	})
	t.Run("MaterializedView", func(t *testing.T) {
		mv := cadInstance + "/materializedViews/lro-mv"
		op, err := e.ia.CreateMaterializedView(ctx, &btapb.CreateMaterializedViewRequest{
			Parent: cadInstance, MaterializedViewId: "lro-mv",
			MaterializedView: &btapb.MaterializedView{Query: "SELECT _key, count(*) AS n FROM `lro-src` GROUP BY _key"},
		})
		if status.Code(err) == codes.Unimplemented {
			t.Skipf("GoogleSQL is unavailable in this build: %v", err)
		}
		require.NoError(t, err)
		e.cadCheckOperation("CreateMaterializedView", op, mv, &btapb.CreateMaterializedViewMetadata{}, &btapb.MaterializedView{})
		op, err = e.ia.UpdateMaterializedView(ctx, &btapb.UpdateMaterializedViewRequest{
			MaterializedView: &btapb.MaterializedView{Name: mv, DeletionProtection: true},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
		})
		require.NoError(t, err)
		e.cadCheckOperation("UpdateMaterializedView", op, mv, &btapb.UpdateMaterializedViewMetadata{}, &btapb.MaterializedView{})
	})
}

// TestConformanceOperationsListFilterPagination covers ListOperations: the
// parent name scopes results, done=true|false is the supported filter, and
// pagination is stable.
func TestConformanceOperationsListFilterPagination(t *testing.T) {
	e := cadNewEnv(t)
	ctx := e.ctx
	e.createInstance()
	for i := 0; i < 3; i++ {
		_, err := e.ia.PartialUpdateInstance(ctx, &btapb.PartialUpdateInstanceRequest{
			Instance:   &btapb.Instance{Name: cadInstance, Labels: map[string]string{"n": fmt.Sprint(i)}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
		})
		require.NoError(t, err)
	}
	// An operation in another instance must not be listed under cadInstance.
	_, err := e.ia.CreateInstance(ctx, &btapb.CreateInstanceRequest{
		Parent: cadProject, InstanceId: "cad-other", Instance: &btapb.Instance{DisplayName: "Other instance"},
		Clusters: map[string]*btapb.Cluster{"cad-other-c1": {}},
	})
	require.NoError(t, err)

	collection := "operations/" + cadInstance
	all, err := e.ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: collection})
	require.NoError(t, err)
	require.Len(t, all.GetOperations(), 4, "CreateInstance + 3 PartialUpdateInstance")
	require.Empty(t, all.GetNextPageToken())
	for _, op := range all.GetOperations() {
		require.True(t, strings.HasPrefix(op.GetName(), collection+"/"), op.GetName())
		require.True(t, op.GetDone())
	}
	project, err := e.ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: "operations/" + cadProject})
	require.NoError(t, err)
	require.Len(t, project.GetOperations(), 5, "a project-level parent lists every instance's operations")

	done, err := e.ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: collection, Filter: "done = true"})
	require.NoError(t, err)
	require.Len(t, done.GetOperations(), 4)
	pending, err := e.ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: collection, Filter: "done=false"})
	require.NoError(t, err)
	require.Empty(t, pending.GetOperations(), "local operations complete synchronously")
	_, err = e.ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: collection, Filter: "metadata.@type:CreateInstanceMetadata"})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "unsupported filter")

	var paged []string
	token := ""
	for {
		resp, err := e.ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: collection, PageSize: 1, PageToken: token})
		require.NoError(t, err)
		require.LessOrEqual(t, len(resp.GetOperations()), 1)
		for _, op := range resp.GetOperations() {
			paged = append(paged, op.GetName())
		}
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
	}
	var want []string
	for _, op := range all.GetOperations() {
		want = append(want, op.GetName())
	}
	require.Equal(t, want, paged, "pagination returns every operation exactly once, in order")

	_, err = e.ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: collection, PageToken: "%%%not-a-token"})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "invalid page token")
	_, err = e.ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: collection, PageSize: -1})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "negative page size")
	_, err = e.ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "missing collection name")
	_, err = e.ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: collection, ReturnPartialSuccess: true})
	require.Equal(t, codes.Unimplemented, status.Code(err), "return_partial_success is not supported unless documented")
}

// TestConformanceOperationsWaitDeleteCancel covers WaitOperation,
// CancelOperation (best effort, a no-op on finished operations) and
// DeleteOperation.
func TestConformanceOperationsWaitDeleteCancel(t *testing.T) {
	e := cadNewEnv(t)
	ctx := e.ctx
	op := e.createInstance()

	waited, err := e.ops.WaitOperation(ctx, &longrunningpb.WaitOperationRequest{Name: op.GetName(), Timeout: nil})
	require.NoError(t, err)
	require.True(t, proto.Equal(op, waited))

	_, err = e.ops.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op.GetName()})
	require.NoError(t, err, "cancelling a finished operation is a successful no-op")
	got, err := e.ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()})
	require.NoError(t, err)
	require.True(t, proto.Equal(op, got), "a finished operation keeps its response after cancellation")
	require.Nil(t, got.GetError())

	_, err = e.ops.DeleteOperation(ctx, &longrunningpb.DeleteOperationRequest{Name: op.GetName()})
	require.NoError(t, err)
	_, err = e.ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()})
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = e.ops.DeleteOperation(ctx, &longrunningpb.DeleteOperationRequest{Name: op.GetName()})
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = e.ops.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op.GetName()})
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = e.ops.WaitOperation(ctx, &longrunningpb.WaitOperationRequest{Name: op.GetName()})
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = e.ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// Deleting the operation does not affect the resource it created.
	_, err = e.ia.GetInstance(ctx, &btapb.GetInstanceRequest{Name: cadInstance})
	require.NoError(t, err)
}

// TestConformanceOperationsDurableAcrossRestart verifies operations are
// stored durably and new operation names never collide with earlier ones.
func TestConformanceOperationsDurableAcrossRestart(t *testing.T) {
	e := cadNewEnv(t)
	first := e.createInstance()

	e = e.restart()
	got, err := e.ops.GetOperation(e.ctx, &longrunningpb.GetOperationRequest{Name: first.GetName()})
	require.NoError(t, err)
	require.True(t, proto.Equal(first, got), "operation survives a restart unchanged")

	second, err := e.ia.PartialUpdateInstance(e.ctx, &btapb.PartialUpdateInstanceRequest{
		Instance:   &btapb.Instance{Name: cadInstance, DisplayName: "After restart"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}},
	})
	require.NoError(t, err)
	require.NotEqual(t, first.GetName(), second.GetName())

	list, err := e.ops.ListOperations(e.ctx, &longrunningpb.ListOperationsRequest{Name: "operations/" + cadInstance})
	require.NoError(t, err)
	var names []string
	for _, op := range list.GetOperations() {
		names = append(names, op.GetName())
	}
	require.ElementsMatch(t, []string{first.GetName(), second.GetName()}, names)
}

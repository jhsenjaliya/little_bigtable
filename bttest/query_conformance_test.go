package bttest

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"testing"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// seedUsers writes rows user#1..user#n with info:name and info:city.
func seedUsers(t *testing.T, env *testEnv, tbl *bigtable.Table, n int) {
	t.Helper()
	cities := []string{"nyc", "sf"}
	for i := 1; i <= n; i++ {
		mut := bigtable.NewMutation()
		mut.Set("info", "name", 1000, []byte(fmt.Sprintf("u%d", i)))
		mut.Set("info", "city", 1000, []byte(cities[i%2]))
		require.NoError(t, tbl.Apply(env.ctx, fmt.Sprintf("user#%d", i), mut))
	}
}

func (env *testEnv) instanceAdmin(t *testing.T) *bigtable.InstanceAdminClient {
	t.Helper()
	iac, err := bigtable.NewInstanceAdminClient(env.ctx, env.projectID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = iac.Close() })
	return iac
}

func TestConformanceSQLQueryThroughClient(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	seedUsers(t, env, env.createTable(t, "users", "info"), 4)

	ps, err := env.client.PrepareStatement(env.ctx,
		"SELECT _key, info['name'] AS name FROM users WHERE _key >= @start LIMIT 2",
		map[string]bigtable.SQLType{"start": bigtable.BytesSQLType{}})
	require.NoError(t, err)
	bs, err := ps.Bind(map[string]any{"start": []byte("user#2")})
	require.NoError(t, err)
	var keys, names []string
	require.NoError(t, bs.Execute(env.ctx, func(rr bigtable.ResultRow) bool {
		var key, name []byte
		require.NoError(t, rr.GetByName("_key", &key))
		require.NoError(t, rr.GetByName("name", &name))
		keys, names = append(keys, string(key)), append(names, string(name))
		return true
	}))
	assert.Equal(t, []string{"user#2", "user#3"}, keys)
	assert.Equal(t, []string{"u2", "u3"}, names)

	// Aggregation over the whole table.
	ps, err = env.client.PrepareStatement(env.ctx,
		"SELECT CAST(info['city'] AS STRING) AS city, COUNT(*) AS n FROM users GROUP BY city", nil)
	require.NoError(t, err)
	bs, err = ps.Bind(nil)
	require.NoError(t, err)
	counts := map[string]int64{}
	require.NoError(t, bs.Execute(env.ctx, func(rr bigtable.ResultRow) bool {
		var city string
		var n int64
		require.NoError(t, rr.GetByName("city", &city))
		require.NoError(t, rr.GetByName("n", &n))
		counts[city] = n
		return true
	}))
	assert.Equal(t, map[string]int64{"nyc": 2, "sf": 2}, counts)

	// Unknown tables and unsupported ORDER BY fail at prepare time.
	_, err = env.client.PrepareStatement(env.ctx, "SELECT * FROM missing", nil)
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = env.client.PrepareStatement(env.ctx, "SELECT _key FROM users ORDER BY info", nil)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestConformanceSQLPreparedQueryRefreshesAfterSchemaChange(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	seedUsers(t, env, env.createTable(t, "users", "info"), 2)
	ps, err := env.client.PrepareStatement(env.ctx, "SELECT * FROM users", nil)
	require.NoError(t, err)
	require.NoError(t, env.admin.CreateColumnFamily(env.ctx, "users", "extra"))

	// The plan changed, so the server reports PREPARED_QUERY_EXPIRED and the
	// client re-prepares transparently.
	bs, err := ps.Bind(nil)
	require.NoError(t, err)
	rows := 0
	require.NoError(t, bs.Execute(env.ctx, func(rr bigtable.ResultRow) bool {
		rows++
		assert.Len(t, rr.Metadata.Columns, 3, "_key, info, extra")
		return true
	}))
	assert.Equal(t, 2, rows)
}

func TestConformanceLogicalViewQuery(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	seedUsers(t, env, env.createTable(t, "users", "info"), 3)
	iac := env.instanceAdmin(t)

	err := iac.CreateLogicalView(env.ctx, env.instanceID, &bigtable.LogicalViewInfo{LogicalViewID: "names", Query: "SELECT _key, info['name'] AS name FROM users"})
	require.NoError(t, err)
	err = iac.CreateLogicalView(env.ctx, env.instanceID, &bigtable.LogicalViewInfo{LogicalViewID: "broken", Query: "SELECT * FROM nope"})
	assert.Equal(t, codes.NotFound, status.Code(err))

	ps, err := env.client.PrepareStatement(env.ctx, "SELECT name FROM names WHERE _key = 'user#3'", nil)
	require.NoError(t, err)
	bs, err := ps.Bind(nil)
	require.NoError(t, err)
	var got []string
	require.NoError(t, bs.Execute(env.ctx, func(rr bigtable.ResultRow) bool {
		var name []byte
		require.NoError(t, rr.GetByName("name", &name))
		got = append(got, string(name))
		return true
	}))
	assert.Equal(t, []string{"u3"}, got)
}

func TestConformanceMaterializedViewReadsAndRefresh(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	users := env.createTable(t, "users", "info")
	seedUsers(t, env, users, 4)
	iac := env.instanceAdmin(t)

	require.NoError(t, iac.CreateMaterializedView(env.ctx, env.instanceID, &bigtable.MaterializedViewInfo{
		MaterializedViewID: "by_city",
		Query:              "SELECT info['city'] AS city, COUNT(*) AS n FROM users GROUP BY city",
	}))
	readCounts := func() map[string]int64 {
		t.Helper()
		counts := map[string]int64{}
		mv := env.client.OpenMaterializedView("by_city")
		require.NoError(t, mv.ReadRows(env.ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
			for _, item := range r["default"] {
				if item.Column == "default:n" {
					counts[r.Key()] = int64(binary.BigEndian.Uint64(item.Value))
				}
			}
			return true
		}))
		return counts
	}
	before := readCounts()
	require.Len(t, before, 2, "one row per city")

	// A source write is visible on the next view read.
	mut := bigtable.NewMutation()
	mut.Set("info", "city", 1000, []byte("la"))
	require.NoError(t, users.Apply(env.ctx, "user#9", mut))
	assert.Len(t, readCounts(), 3)

	// The source table cannot be deleted while a view reads from it.
	err := env.admin.DeleteTable(env.ctx, "users")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	// ORDER BY definitions re-key the source; the query is immutable.
	require.NoError(t, iac.CreateMaterializedView(env.ctx, env.instanceID, &bigtable.MaterializedViewInfo{
		MaterializedViewID: "by_name",
		Query:              "SELECT info['name'] AS name, _key AS src, info['city'] AS city FROM users ORDER BY name, src",
	}))
	rows := 0
	require.NoError(t, env.client.OpenMaterializedView("by_name").ReadRows(env.ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		rows++
		return true
	}))
	assert.Equal(t, 4, rows, "user#9 has no name, so it has no key and no row")

	_, err = env.client.OpenMaterializedView("by_city").SampleRowKeys(env.ctx)
	require.NoError(t, err)

	// Views are not tables.
	_, err = env.admin.TableInfo(env.ctx, "by_city")
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestConformanceMaterializedViewSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dbFile := newDBFile(t)
	open := func() (*server, *sql.DB) {
		db, err := sql.Open("sqlite3", "file:"+dbFile+"?cache=shared")
		require.NoError(t, err)
		db.SetMaxOpenConns(1)
		require.NoError(t, CreateTables(ctx, db))
		s := newServerState(db)
		require.NoError(t, s.load(ctx))
		return s, db
	}
	s, db := open()
	parent := "projects/p/instances/i"
	mustCreateTable(t, s, parent, "src", "cf")
	_, err := s.MutateRow(ctx, mutateReq(parent+"/tables/src", "a#1", "cf", "q", "v"))
	require.NoError(t, err)
	_, err = s.CreateMaterializedView(ctx, &btapb.CreateMaterializedViewRequest{
		Parent: parent, MaterializedViewId: "mv",
		MaterializedView: &btapb.MaterializedView{Query: "SELECT _key, COUNT(*) AS n FROM src GROUP BY _key"},
	})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	s2, db2 := open()
	defer db2.Close()
	target, err := s2.resolveReadTarget("", "", parent+"/materializedViews/mv", "")
	require.NoError(t, err)
	require.NoError(t, target.mv.readable())
	assert.NotNil(t, storedRow(t, target.tbl, "a#1"))

	// CRUD: get, list, deletion protection, immutable query, delete.
	name := parent + "/materializedViews/mv"
	got, err := s2.GetMaterializedView(ctx, &btapb.GetMaterializedViewRequest{Name: name})
	require.NoError(t, err)
	assert.NotEmpty(t, got.GetEtag())
	list, err := s2.ListMaterializedViews(ctx, &btapb.ListMaterializedViewsRequest{Parent: parent})
	require.NoError(t, err)
	assert.Len(t, list.GetMaterializedViews(), 1)
	_, err = s2.UpdateMaterializedView(ctx, &btapb.UpdateMaterializedViewRequest{
		MaterializedView: &btapb.MaterializedView{Name: name, Query: "SELECT 1"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"query"}}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s2.UpdateMaterializedView(ctx, &btapb.UpdateMaterializedViewRequest{
		MaterializedView: &btapb.MaterializedView{Name: name, DeletionProtection: true}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}}})
	require.NoError(t, err)
	_, err = s2.DeleteMaterializedView(ctx, &btapb.DeleteMaterializedViewRequest{Name: name})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = s2.UpdateMaterializedView(ctx, &btapb.UpdateMaterializedViewRequest{
		MaterializedView: &btapb.MaterializedView{Name: name}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}}})
	require.NoError(t, err)
	_, err = s2.DeleteMaterializedView(ctx, &btapb.DeleteMaterializedViewRequest{Name: name})
	require.NoError(t, err)
	_, err = s2.GetMaterializedView(ctx, &btapb.GetMaterializedViewRequest{Name: name})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func mutateReq(tableName, key, fam, col, value string) *btpb.MutateRowRequest {
	return &btpb.MutateRowRequest{TableName: tableName, RowKey: []byte(key), Mutations: []*btpb.Mutation{{
		Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{FamilyName: fam, ColumnQualifier: []byte(col), TimestampMicros: 1000, Value: []byte(value)}},
	}}}
}

func TestConformanceMaterializedViewSession(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	seedUsers(t, env, env.createTable(t, "users", "info"), 2)
	require.NoError(t, env.instanceAdmin(t).CreateMaterializedView(env.ctx, env.instanceID, &bigtable.MaterializedViewInfo{
		MaterializedViewID: "keys", Query: "SELECT _key, COUNT(*) AS n FROM users GROUP BY _key",
	}))
	conn, err := grpc.NewClient(os.Getenv("BIGTABLE_EMULATOR_HOST"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	stream, err := btpb.NewBigtableClient(conn).OpenMaterializedView(env.ctx)
	require.NoError(t, err)
	open, _ := proto.Marshal(&btpb.OpenMaterializedViewRequest{
		MaterializedViewName: "projects/" + env.projectID + "/instances/" + env.instanceID + "/materializedViews/keys",
	})
	require.NoError(t, stream.Send(&btpb.SessionRequest{Payload: &btpb.SessionRequest_OpenSession{OpenSession: &btpb.OpenSessionRequest{ProtocolVersion: 1, Payload: open}}}))
	resp, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, resp.GetOpenSession())

	send := func(id int64, req *btpb.TableRequest) *btpb.SessionResponse {
		payload, _ := proto.Marshal(req)
		require.NoError(t, stream.Send(&btpb.SessionRequest{Payload: &btpb.SessionRequest_VirtualRpc{VirtualRpc: &btpb.VirtualRpcRequest{RpcId: id, Payload: payload}}}))
		r, err := stream.Recv()
		require.NoError(t, err)
		return r
	}
	read := send(1, &btpb.TableRequest{Payload: &btpb.TableRequest_ReadRow{ReadRow: &btpb.SessionReadRowRequest{Key: []byte("user#1")}}})
	var tr btpb.TableResponse
	require.NoError(t, proto.Unmarshal(read.GetVirtualRpc().GetPayload(), &tr))
	assert.Equal(t, "user#1", string(tr.GetReadRow().GetRow().GetKey()))

	// Materialized views are read-only: the session has no write permission.
	write := send(2, &btpb.TableRequest{Payload: &btpb.TableRequest_MutateRow{MutateRow: &btpb.SessionMutateRowRequest{Key: []byte("x")}}})
	assert.Equal(t, int32(codes.PermissionDenied), write.GetError().GetStatus().GetCode())
}

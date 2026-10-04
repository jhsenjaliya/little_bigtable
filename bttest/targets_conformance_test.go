package bttest

// Conformance tests for Data API target resolution: exactly one of
// table_name / authorized_view_name / materialized_view_name, and app profile
// resolution (default profile, unknown profile, Data Boost read-only
// isolation, transactional-write routing) as documented in
// google/bigtable/v2/bigtable.proto and google/bigtable/admin/v2/instance.proto.

import (
	"context"
	"os"
	"sort"
	"testing"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	ctgInstance = "projects/ctg-project/instances/ctg-instance"
	ctgTable    = ctgInstance + "/tables/tbl"
)

// ctgMutateRowsServer captures MutateRows responses.
type ctgMutateRowsServer struct {
	grpc.ServerStream
	responses []*btpb.MutateRowsResponse
}

func (m *ctgMutateRowsServer) Send(r *btpb.MutateRowsResponse) error {
	m.responses = append(m.responses, r)
	return nil
}

func (m *ctgMutateRowsServer) Context() context.Context { return context.Background() }

// ctgRegisteredServer returns a server with a registered instance holding two
// clusters, table "tbl" with family "cf", and app profiles:
//   - boost:  single-cluster routing with Data Boost read-only isolation
//   - multi:  multi-cluster routing (no transactional writes)
//   - nontx:  single-cluster routing without allow_transactional_writes
func ctgRegisteredServer(t *testing.T) *server {
	t.Helper()
	s := newTestServer(t)
	ctx := context.Background()
	_, err := s.CreateInstance(ctx, &btapb.CreateInstanceRequest{
		Parent:     "projects/ctg-project",
		InstanceId: "ctg-instance",
		Instance:   &btapb.Instance{DisplayName: "ctg instance"},
		Clusters:   map[string]*btapb.Cluster{"ctg-cluster-a": {}, "ctg-cluster-b": {}},
	})
	require.NoError(t, err)
	_, err = s.CreateTable(ctx, &btapb.CreateTableRequest{
		Parent: ctgInstance, TableId: "tbl",
		Table: &btapb.Table{ColumnFamilies: map[string]*btapb.ColumnFamily{"cf": {}}},
	})
	require.NoError(t, err)
	single := func(tx bool) *btapb.AppProfile_SingleClusterRouting_ {
		return &btapb.AppProfile_SingleClusterRouting_{SingleClusterRouting: &btapb.AppProfile_SingleClusterRouting{
			ClusterId: "ctg-cluster-a", AllowTransactionalWrites: tx,
		}}
	}
	profiles := map[string]*btapb.AppProfile{
		"boost": {
			RoutingPolicy: single(false),
			Isolation: &btapb.AppProfile_DataBoostIsolationReadOnly_{DataBoostIsolationReadOnly: &btapb.AppProfile_DataBoostIsolationReadOnly{
				ComputeBillingOwner: btapb.AppProfile_DataBoostIsolationReadOnly_HOST_PAYS.Enum(),
			}},
		},
		"multi": {RoutingPolicy: &btapb.AppProfile_MultiClusterRoutingUseAny_{MultiClusterRoutingUseAny: &btapb.AppProfile_MultiClusterRoutingUseAny{}}},
		"nontx": {RoutingPolicy: single(false)},
	}
	for id, p := range profiles {
		_, err := s.CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{Parent: ctgInstance, AppProfileId: id, AppProfile: p})
		require.NoError(t, err, id)
	}
	return s
}

type ctgTarget struct {
	table, av, mv, profile string
	reversed               bool
}

func ctgSetCell(col, value string) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
		FamilyName: "cf", ColumnQualifier: []byte(col), TimestampMicros: 1000, Value: []byte(value),
	}}}
}

// ctgCallAll issues every Data API RPC that takes a target and returns the
// RPC-level error of each.
func ctgCallAll(s *server, tg ctgTarget, key string) map[string]error {
	ctx := context.Background()
	out := map[string]error{}
	out["ReadRows"] = s.ReadRows(&btpb.ReadRowsRequest{
		TableName: tg.table, AuthorizedViewName: tg.av, MaterializedViewName: tg.mv,
		AppProfileId: tg.profile, Reversed: tg.reversed,
	}, &MockReadRowsServer{})
	out["SampleRowKeys"] = s.SampleRowKeys(&btpb.SampleRowKeysRequest{
		TableName: tg.table, AuthorizedViewName: tg.av, MaterializedViewName: tg.mv, AppProfileId: tg.profile,
	}, &MockSampleRowKeysServer{})
	_, out["MutateRow"] = s.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: tg.table, AuthorizedViewName: tg.av, AppProfileId: tg.profile,
		RowKey: []byte(key), Mutations: []*btpb.Mutation{ctgSetCell("a", "v")},
	})
	out["MutateRows"] = s.MutateRows(&btpb.MutateRowsRequest{
		TableName: tg.table, AuthorizedViewName: tg.av, AppProfileId: tg.profile,
		Entries: []*btpb.MutateRowsRequest_Entry{{RowKey: []byte(key), Mutations: []*btpb.Mutation{ctgSetCell("b", "v")}}},
	}, &ctgMutateRowsServer{})
	_, out["CheckAndMutateRow"] = s.CheckAndMutateRow(ctx, &btpb.CheckAndMutateRowRequest{
		TableName: tg.table, AuthorizedViewName: tg.av, AppProfileId: tg.profile,
		RowKey: []byte(key), TrueMutations: []*btpb.Mutation{ctgSetCell("c", "v")},
	})
	_, out["ReadModifyWriteRow"] = s.ReadModifyWriteRow(ctx, &btpb.ReadModifyWriteRowRequest{
		TableName: tg.table, AuthorizedViewName: tg.av, AppProfileId: tg.profile,
		RowKey: []byte(key), Rules: []*btpb.ReadModifyWriteRule{{
			FamilyName: "cf", ColumnQualifier: []byte("d"), Rule: &btpb.ReadModifyWriteRule_AppendValue{AppendValue: []byte("x")},
		}},
	})
	return out
}

func ctgExpectCodes(t *testing.T, label string, got map[string]error, want map[string]codes.Code) {
	t.Helper()
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if c := status.Code(got[name]); c != want[name] {
			t.Errorf("%s: %s returned %v (%v), want %v", label, name, c, got[name], want[name])
		}
	}
}

func ctgAll(c codes.Code) map[string]codes.Code {
	return map[string]codes.Code{
		"ReadRows": c, "SampleRowKeys": c, "MutateRow": c, "MutateRows": c, "CheckAndMutateRow": c, "ReadModifyWriteRow": c,
	}
}

func TestConformanceTargetExactlyOneTarget(t *testing.T) {
	s := ctgRegisteredServer(t)
	_, err := s.CreateAuthorizedView(context.Background(), &btapb.CreateAuthorizedViewRequest{
		Parent: ctgTable, AuthorizedViewId: "all",
		AuthorizedView: &btapb.AuthorizedView{AuthorizedView: &btapb.AuthorizedView_SubsetView_{SubsetView: &btapb.AuthorizedView_SubsetView{
			RowPrefixes:   [][]byte{[]byte("")},
			FamilySubsets: map[string]*btapb.AuthorizedView_FamilySubsets{"cf": {QualifierPrefixes: [][]byte{[]byte("")}}},
		}}},
	})
	require.NoError(t, err)
	av := ctgTable + "/authorizedViews/all"
	mv := ctgInstance + "/materializedViews/mv"

	ctgExpectCodes(t, "no target", ctgCallAll(s, ctgTarget{}, "r"), ctgAll(codes.InvalidArgument))
	ctgExpectCodes(t, "table+authorized view", ctgCallAll(s, ctgTarget{table: ctgTable, av: av}, "r"), ctgAll(codes.InvalidArgument))
	// Nothing was written by the rejected requests.
	require.Equal(t, 0, rowCount(t, s.tables[ctgTable]))
	// Write requests have no materialized_view_name; only reads are checked.
	reads := map[string]codes.Code{"ReadRows": codes.InvalidArgument, "SampleRowKeys": codes.InvalidArgument}
	ctgExpectCodes(t, "table+materialized view", ctgCallAll(s, ctgTarget{table: ctgTable, mv: mv}, "r"), reads)
	ctgExpectCodes(t, "authorized+materialized view", ctgCallAll(s, ctgTarget{av: av, mv: mv}, "r"), reads)

	// Each single target alone is accepted.
	ctgExpectCodes(t, "table only", ctgCallAll(s, ctgTarget{table: ctgTable}, "r1"), ctgAll(codes.OK))
	ctgExpectCodes(t, "authorized view only", ctgCallAll(s, ctgTarget{av: av}, "r2"), ctgAll(codes.OK))
	// Targets that do not exist are NotFound, not silently ignored.
	ctgExpectCodes(t, "missing table", ctgCallAll(s, ctgTarget{table: ctgInstance + "/tables/missing"}, "r"), ctgAll(codes.NotFound))
	ctgExpectCodes(t, "missing authorized view", ctgCallAll(s, ctgTarget{av: ctgTable + "/authorizedViews/missing"}, "r"), ctgAll(codes.NotFound))
}

func TestConformanceTargetAppProfileResolution(t *testing.T) {
	s := ctgRegisteredServer(t)

	// "" resolves to the instance's default profile, which Bigtable creates
	// with single-cluster routing and transactional writes allowed.
	ctgExpectCodes(t, "empty profile", ctgCallAll(s, ctgTarget{table: ctgTable}, "r1"), ctgAll(codes.OK))
	ctgExpectCodes(t, "default profile", ctgCallAll(s, ctgTarget{table: ctgTable, profile: "default"}, "r2"), ctgAll(codes.OK))
	def, err := s.GetAppProfile(context.Background(), &btapb.GetAppProfileRequest{Name: ctgInstance + "/appProfiles/default"})
	require.NoError(t, err)
	require.True(t, def.GetSingleClusterRouting().GetAllowTransactionalWrites())

	// An unknown profile on a registered instance is NotFound for every RPC.
	before := rowCount(t, s.tables[ctgTable])
	ctgExpectCodes(t, "unknown profile", ctgCallAll(s, ctgTarget{table: ctgTable, profile: "no-such-profile"}, "r3"), ctgAll(codes.NotFound))
	require.Equal(t, before, rowCount(t, s.tables[ctgTable]))

	// Multi-cluster routing and single-cluster routing without
	// allow_transactional_writes reject CheckAndMutateRow and
	// ReadModifyWriteRow but accept plain reads and writes.
	for _, profile := range []string{"multi", "nontx"} {
		want := ctgAll(codes.OK)
		want["CheckAndMutateRow"] = codes.FailedPrecondition
		want["ReadModifyWriteRow"] = codes.FailedPrecondition
		ctgExpectCodes(t, profile, ctgCallAll(s, ctgTarget{table: ctgTable, profile: profile}, "r-"+profile), want)
		r := storedRow(t, s.tables[ctgTable], "r-"+profile)
		require.NotNil(t, r)
		cf := r.families["cf"]
		require.NotNil(t, cf)
		require.Contains(t, cf.Cells, "a")
		require.Contains(t, cf.Cells, "b")
		require.NotContains(t, cf.Cells, "c", "CheckAndMutateRow must not apply under %s", profile)
		require.NotContains(t, cf.Cells, "d", "ReadModifyWriteRow must not apply under %s", profile)
	}

	// Data Boost profiles are read-only: every write fails with
	// FailedPrecondition, forward reads succeed, reversed reads are rejected.
	before = rowCount(t, s.tables[ctgTable])
	want := ctgAll(codes.FailedPrecondition)
	want["ReadRows"] = codes.OK
	want["SampleRowKeys"] = codes.OK
	ctgExpectCodes(t, "data boost", ctgCallAll(s, ctgTarget{table: ctgTable, profile: "boost"}, "r-boost"), want)
	require.Equal(t, before, rowCount(t, s.tables[ctgTable]))
	ctgExpectCodes(t, "data boost reversed", ctgCallAll(s, ctgTarget{table: ctgTable, profile: "boost", reversed: true}, "r-boost"),
		map[string]codes.Code{"ReadRows": codes.InvalidArgument})
	ctgExpectCodes(t, "default reversed", ctgCallAll(s, ctgTarget{table: ctgTable, reversed: true}, "r4"),
		map[string]codes.Code{"ReadRows": codes.OK})

	// PingAndWarm validates the profile the same way.
	ctx := context.Background()
	_, err = s.PingAndWarm(ctx, &btpb.PingAndWarmRequest{Name: ctgInstance, AppProfileId: "no-such-profile"})
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = s.PingAndWarm(ctx, &btpb.PingAndWarmRequest{Name: ctgInstance})
	require.NoError(t, err)
}

// A deleted profile stops resolving immediately.
func TestConformanceTargetDeletedAppProfile(t *testing.T) {
	s := ctgRegisteredServer(t)
	ctgExpectCodes(t, "before delete", ctgCallAll(s, ctgTarget{table: ctgTable, profile: "multi"}, "r1"),
		map[string]codes.Code{"MutateRow": codes.OK, "ReadRows": codes.OK})
	_, err := s.DeleteAppProfile(context.Background(), &btapb.DeleteAppProfileRequest{Name: ctgInstance + "/appProfiles/multi", IgnoreWarnings: true})
	require.NoError(t, err)
	ctgExpectCodes(t, "after delete", ctgCallAll(s, ctgTarget{table: ctgTable, profile: "multi"}, "r1"), ctgAll(codes.NotFound))
}

// Instances that were never registered through the instance admin API accept
// any app profile ID, like the official emulator.
func TestConformanceTargetUnregisteredInstanceLeniency(t *testing.T) {
	s, parent := newFullTestServer(t)
	_, err := s.CreateTable(context.Background(), &btapb.CreateTableRequest{
		Parent: parent, TableId: "lenient",
		Table: &btapb.Table{ColumnFamilies: map[string]*btapb.ColumnFamily{"cf": {}}},
	})
	require.NoError(t, err)
	name := parent + "/tables/lenient"
	for _, profile := range []string{"", "default", "anything-goes"} {
		ctgExpectCodes(t, "profile "+profile, ctgCallAll(s, ctgTarget{table: name, profile: profile}, "row-"+profile), ctgAll(codes.OK))
	}
	r := storedRow(t, s.tables[name], "row-anything-goes")
	require.NotNil(t, r)
	for _, col := range []string{"a", "b", "c", "d"} {
		require.Contains(t, r.families["cf"].Cells, col)
	}
}

// ctgDial connects to the emulator started by setupTestEnv.
func ctgDial(t *testing.T, ctx context.Context) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.DialContext(ctx, os.Getenv("BIGTABLE_EMULATOR_HOST"),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func ctgProfileClient(t *testing.T, env *testEnv, profile string) *bigtable.Client {
	t.Helper()
	c, err := bigtable.NewClientWithConfig(env.ctx, env.projectID, env.instanceID,
		bigtable.ClientConfig{AppProfile: profile, MetricsProvider: bigtable.NoopMetricsProvider{}},
		option.WithGRPCConn(ctgDial(t, env.ctx)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// The official Go client sends app_profile_id from ClientConfig.AppProfile;
// the emulator enforces the profile's routing and isolation.
func TestConformanceTargetGoClientAppProfiles(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	ctx := env.ctx
	env.createTable(t, "profiles", "cf")

	iac, err := bigtable.NewInstanceAdminClient(ctx, env.projectID, option.WithGRPCConn(ctgDial(t, ctx)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = iac.Close() })
	_, err = iac.CreateAppProfile(ctx, bigtable.ProfileConf{
		ProfileID: "boost", InstanceID: env.instanceID,
		RoutingConfig: &bigtable.SingleClusterRoutingConfig{ClusterID: "test-cluster"},
		Isolation:     &bigtable.DataBoostIsolationReadOnly{ComputeBillingOwner: bigtable.HostPays},
	})
	require.NoError(t, err)
	_, err = iac.CreateAppProfile(ctx, bigtable.ProfileConf{
		ProfileID: "multi", InstanceID: env.instanceID,
		RoutingConfig: &bigtable.MultiClusterRoutingUseAnyConfig{},
	})
	require.NoError(t, err)
	// Routing must reference clusters of the instance.
	_, err = iac.CreateAppProfile(ctx, bigtable.ProfileConf{
		ProfileID: "bad", InstanceID: env.instanceID,
		RoutingConfig: &bigtable.SingleClusterRoutingConfig{ClusterID: "no-such-cluster"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	set := func(v string) *bigtable.Mutation {
		m := bigtable.NewMutation()
		m.Set("cf", "q", 1000, []byte(v))
		return m
	}
	cond := bigtable.NewCondMutation(bigtable.ColumnFilter("q"), set("t"), set("f"))
	rmw := bigtable.NewReadModifyWrite()
	rmw.AppendValue("cf", "q", []byte("+"))

	// Default profile: everything works.
	def := env.client.OpenTable("profiles")
	require.NoError(t, def.Apply(ctx, "r1", set("v1")))
	require.NoError(t, def.Apply(ctx, "r1", cond))
	_, err = def.ApplyReadModifyWrite(ctx, "r1", rmw)
	require.NoError(t, err)

	// Unknown profile: NotFound for reads and writes.
	missing := ctgProfileClient(t, env, "no-such-profile").OpenTable("profiles")
	require.Equal(t, codes.NotFound, status.Code(missing.Apply(ctx, "r2", set("x"))))
	_, err = missing.ReadRow(ctx, "r1")
	require.Equal(t, codes.NotFound, status.Code(err))

	// Data Boost: read-only, forward reads only.
	boost := ctgProfileClient(t, env, "boost").OpenTable("profiles")
	require.Equal(t, codes.FailedPrecondition, status.Code(boost.Apply(ctx, "r2", set("x"))))
	_, err = boost.ApplyReadModifyWrite(ctx, "r1", rmw)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = boost.ApplyBulk(ctx, []string{"r2"}, []*bigtable.Mutation{set("x")})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	row, err := boost.ReadRow(ctx, "r1")
	require.NoError(t, err)
	require.Equal(t, "r1", row.Key())
	err = boost.ReadRows(ctx, bigtable.InfiniteRange(""), func(bigtable.Row) bool { return true }, bigtable.ReverseScan())
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = boost.SampleRowKeys(ctx)
	require.NoError(t, err)

	// Multi-cluster routing: no single-row transactions.
	multi := ctgProfileClient(t, env, "multi").OpenTable("profiles")
	require.NoError(t, multi.Apply(ctx, "r3", set("v3")))
	require.Equal(t, codes.FailedPrecondition, status.Code(multi.Apply(ctx, "r3", cond)))
	_, err = multi.ApplyReadModifyWrite(ctx, "r3", rmw)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	row, err = multi.ReadRow(ctx, "r3")
	require.NoError(t, err)
	require.Len(t, row["cf"], 1)
	require.Equal(t, "v3", string(row["cf"][0].Value))

	// Nothing the rejected requests attempted reached storage.
	row, err = def.ReadRow(ctx, "r2")
	require.NoError(t, err)
	require.Empty(t, row)
}

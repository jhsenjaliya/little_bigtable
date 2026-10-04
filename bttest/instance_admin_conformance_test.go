package bttest

import (
	"context"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func cadCreateInstanceReq(id string, clusterIDs ...string) *btapb.CreateInstanceRequest {
	clusters := map[string]*btapb.Cluster{}
	for _, c := range clusterIDs {
		clusters[c] = &btapb.Cluster{ServeNodes: 1}
	}
	return &btapb.CreateInstanceRequest{
		Parent: cadProject, InstanceId: id,
		Instance: &btapb.Instance{DisplayName: "Display " + id[:2]},
		Clusters: clusters,
	}
}

func cadSingleRouting(cluster string) *btapb.AppProfile {
	return &btapb.AppProfile{RoutingPolicy: &btapb.AppProfile_SingleClusterRouting_{
		SingleClusterRouting: &btapb.AppProfile_SingleClusterRouting{ClusterId: cluster},
	}}
}

// TestConformanceInstanceAdminInstanceIDValidation: instance IDs are 6-33
// characters, start with a lowercase letter, contain lowercase letters,
// digits or hyphens, and do not end with a hyphen.
func TestConformanceInstanceAdminInstanceIDValidation(t *testing.T) {
	s := newInstanceTestServer(t)
	ctx := context.Background()
	cases := []struct {
		id string
		ok bool
	}{
		{"abcdef", true},
		{"a" + strings.Repeat("b", 32), true}, // 33 characters
		{"inst-01", true},
		{"abcde", false},                       // 5 characters
		{"a" + strings.Repeat("b", 33), false}, // 34 characters
		{"1abcdef", false},
		{"Abcdef", false},
		{"abcdef-", false},
		{"abc_def", false},
		{"", false},
	}
	for _, c := range cases {
		req := cadCreateInstanceReq("xx-placeholder", "cluster-1")
		req.InstanceId = c.id
		_, err := s.CreateInstance(ctx, req)
		if c.ok {
			require.NoError(t, err, "instance ID %q", c.id)
		} else {
			require.Equal(t, codes.InvalidArgument, status.Code(err), "instance ID %q", c.id)
		}
	}
	_, err := s.CreateInstance(ctx, &btapb.CreateInstanceRequest{
		Parent: "not-a-project", InstanceId: "abcdefg", Instance: &btapb.Instance{DisplayName: "Display"},
		Clusters: map[string]*btapb.Cluster{"cluster-1": {}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "parent must be projects/{project}")
}

// TestConformanceInstanceAdminClusterIDValidation: cluster IDs are 6-30
// characters with the same character rules, at create and on CreateCluster.
func TestConformanceInstanceAdminClusterIDValidation(t *testing.T) {
	s := newInstanceTestServer(t)
	ctx := context.Background()
	for _, bad := range []string{"short", "1cluster", "Cluster", "cluster-", "c" + strings.Repeat("x", 30)} {
		_, err := s.CreateInstance(ctx, cadCreateInstanceReq("cluster-check", bad))
		require.Equal(t, codes.InvalidArgument, status.Code(err), "cluster ID %q at CreateInstance", bad)
	}
	_, err := s.CreateInstance(ctx, cadCreateInstanceReq("cluster-check", "c"+strings.Repeat("x", 29)))
	require.NoError(t, err, "30-character cluster ID")
	parent := cadProject + "/instances/cluster-check"
	for _, bad := range []string{"short", "Cluster", "cluster-"} {
		_, err := s.CreateCluster(ctx, &btapb.CreateClusterRequest{Parent: parent, ClusterId: bad, Cluster: &btapb.Cluster{}})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "cluster ID %q at CreateCluster", bad)
	}
	_, err = s.CreateCluster(ctx, &btapb.CreateClusterRequest{Parent: parent, ClusterId: "second-cluster", Cluster: &btapb.Cluster{ServeNodes: -1}})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "negative serve_nodes")
	_, err = s.CreateCluster(ctx, &btapb.CreateClusterRequest{Parent: cadProject + "/instances/missing-inst", ClusterId: "second-cluster"})
	require.Equal(t, codes.NotFound, status.Code(err), "cluster in a missing instance")
	_, err = s.CreateCluster(ctx, &btapb.CreateClusterRequest{Parent: parent, ClusterId: "c" + strings.Repeat("x", 29)})
	require.Equal(t, codes.AlreadyExists, status.Code(err))
}

// TestConformanceInstanceAdminCreateRequirements covers the remaining
// CreateInstance rules and the defaults Bigtable applies.
func TestConformanceInstanceAdminCreateRequirements(t *testing.T) {
	s := newInstanceTestServer(t)
	ctx := context.Background()

	noClusters := cadCreateInstanceReq("no-clusters")
	_, err := s.CreateInstance(ctx, noClusters)
	require.Equal(t, codes.InvalidArgument, status.Code(err), "at least one cluster is required")

	for _, display := range []string{"abc", strings.Repeat("d", 31)} {
		req := cadCreateInstanceReq("display-check", "cluster-1")
		req.Instance.DisplayName = display
		_, err := s.CreateInstance(ctx, req)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "display name %q", display)
	}

	op, err := s.CreateInstance(ctx, cadCreateInstanceReq("defaults", "zeta-cluster", "alpha-cluster"))
	require.NoError(t, err)
	inst := &btapb.Instance{}
	require.NoError(t, op.GetResponse().UnmarshalTo(inst))
	require.Equal(t, btapb.Instance_PRODUCTION, inst.GetType(), "type defaults to PRODUCTION")
	require.Equal(t, btapb.Instance_ENTERPRISE, inst.GetEdition(), "edition defaults to ENTERPRISE")
	require.Equal(t, btapb.Instance_READY, inst.GetState())
	require.NotNil(t, inst.GetCreateTime())
	stored, err := s.GetInstance(ctx, &btapb.GetInstanceRequest{Name: inst.GetName()})
	require.NoError(t, err)
	require.True(t, proto.Equal(inst, stored))

	cluster, err := s.GetCluster(ctx, &btapb.GetClusterRequest{Name: inst.GetName() + "/clusters/alpha-cluster"})
	require.NoError(t, err)
	require.Equal(t, btapb.Cluster_READY, cluster.GetState())
	require.Equal(t, btapb.StorageType_SSD, cluster.GetDefaultStorageType(), "storage type defaults to SSD")

	// Every instance gets a "default" app profile: single-cluster routing to
	// its first cluster with transactional writes allowed.
	def, err := s.GetAppProfile(ctx, &btapb.GetAppProfileRequest{Name: inst.GetName() + "/appProfiles/default"})
	require.NoError(t, err)
	require.Equal(t, "alpha-cluster", def.GetSingleClusterRouting().GetClusterId())
	require.True(t, def.GetSingleClusterRouting().GetAllowTransactionalWrites())
	require.NotEmpty(t, def.GetEtag())

	_, err = s.CreateInstance(ctx, cadCreateInstanceReq("defaults", "other-cluster"))
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	_, err = s.GetInstance(ctx, &btapb.GetInstanceRequest{Name: cadProject + "/instances/missing-inst"})
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestConformanceInstanceAdminOfficialClient drives the instance lifecycle
// through the official Go InstanceAdminClient, including its LRO waits.
func TestConformanceInstanceAdminOfficialClient(t *testing.T) {
	e := cadNewEnv(t)
	ctx := e.ctx
	iac, err := bigtable.NewInstanceAdminClient(ctx, cadProjectID, option.WithGRPCConn(e.conn))
	require.NoError(t, err)
	defer iac.Close()

	require.NoError(t, iac.CreateInstance(ctx, &bigtable.InstanceConf{
		InstanceId: "client-inst", DisplayName: "Client instance", ClusterId: "client-c1",
		Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD, InstanceType: bigtable.PRODUCTION,
		Labels: map[string]string{"env": "test"},
	}))
	info, err := iac.InstanceInfo(ctx, "client-inst")
	require.NoError(t, err)
	require.Equal(t, "Client instance", info.DisplayName)
	require.Equal(t, bigtable.PRODUCTION, info.InstanceType)
	require.Equal(t, map[string]string{"env": "test"}, info.Labels)

	require.NoError(t, iac.UpdateInstanceWithClusters(ctx, &bigtable.InstanceWithClustersConfig{
		InstanceID: "client-inst", DisplayName: "Renamed client", Labels: map[string]string{"env": "prod"},
		Clusters: []bigtable.ClusterConfig{{ClusterID: "client-c1", NumNodes: 3}},
	}))
	info, err = iac.InstanceInfo(ctx, "client-inst")
	require.NoError(t, err)
	require.Equal(t, "Renamed client", info.DisplayName)
	require.Equal(t, map[string]string{"env": "prod"}, info.Labels)
	c1, err := iac.GetCluster(ctx, "client-inst", "client-c1")
	require.NoError(t, err)
	require.Equal(t, 3, c1.ServeNodes)
	require.Equal(t, "us-central1-b", c1.Zone)

	require.NoError(t, iac.CreateCluster(ctx, &bigtable.ClusterConfig{
		InstanceID: "client-inst", ClusterID: "client-c2", Zone: "us-east1-b", NumNodes: 1, StorageType: bigtable.HDD,
	}))
	clusters, err := iac.Clusters(ctx, "client-inst")
	require.NoError(t, err)
	require.Len(t, clusters, 2)

	profile, err := iac.CreateAppProfile(ctx, bigtable.ProfileConf{
		InstanceID: "client-inst", ProfileID: "client-profile", Description: "routes to c2",
		RoutingConfig: &bigtable.SingleClusterRoutingConfig{ClusterID: "client-c2"},
	})
	require.NoError(t, err)
	require.Equal(t, "client-c2", profile.GetSingleClusterRouting().GetClusterId())
	require.NoError(t, iac.UpdateAppProfile(ctx, "client-inst", "client-profile", bigtable.ProfileAttrsToUpdate{
		Description: "any cluster", RoutingConfig: &bigtable.MultiClusterRoutingUseAnyConfig{},
	}))
	profile, err = iac.GetAppProfile(ctx, "client-inst", "client-profile")
	require.NoError(t, err)
	require.Equal(t, "any cluster", profile.GetDescription())
	require.NotNil(t, profile.GetMultiClusterRoutingUseAny())
	var profiles []string
	it := iac.ListAppProfiles(ctx, "client-inst")
	for {
		p, err := it.Next()
		if err == iterator.Done {
			break
		}
		require.NoError(t, err)
		profiles = append(profiles, p.GetName())
	}
	require.Len(t, profiles, 2, "default and client-profile")

	require.NoError(t, iac.DeleteCluster(ctx, "client-inst", "client-c2"))
	err = iac.DeleteCluster(ctx, "client-inst", "client-c1")
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "the last cluster cannot be deleted")
	require.NoError(t, iac.DeleteAppProfile(ctx, "client-inst", "client-profile"))

	instances, err := iac.Instances(ctx)
	require.NoError(t, err)
	require.Len(t, instances, 1)
	require.NoError(t, iac.DeleteInstance(ctx, "client-inst"))
	_, err = iac.InstanceInfo(ctx, "client-inst")
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestConformanceInstanceAdminPartialUpdateMasks: only masked fields change;
// a PRODUCTION instance cannot become DEVELOPMENT.
func TestConformanceInstanceAdminPartialUpdateMasks(t *testing.T) {
	s := newInstanceTestServer(t)
	ctx := context.Background()
	req := cadCreateInstanceReq("masked-inst", "masked-c1")
	req.Instance.Type = btapb.Instance_DEVELOPMENT
	req.Instance.Labels = map[string]string{"keep": "me"}
	_, err := s.CreateInstance(ctx, req)
	require.NoError(t, err)
	name := cadProject + "/instances/masked-inst"
	update := func(in *btapb.Instance, paths ...string) (*btapb.Instance, error) {
		in.Name = name
		op, err := s.PartialUpdateInstance(ctx, &btapb.PartialUpdateInstanceRequest{Instance: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		if err != nil {
			return nil, err
		}
		got := &btapb.Instance{}
		require.NoError(t, op.GetResponse().UnmarshalTo(got))
		return got, nil
	}

	got, err := update(&btapb.Instance{DisplayName: "New display", Labels: map[string]string{"dropped": "x"}}, "display_name")
	require.NoError(t, err)
	require.Equal(t, "New display", got.GetDisplayName())
	require.Equal(t, map[string]string{"keep": "me"}, got.GetLabels(), "labels are not in the mask")

	got, err = update(&btapb.Instance{Labels: map[string]string{"team": "data"}}, "labels")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"team": "data"}, got.GetLabels())
	require.Equal(t, "New display", got.GetDisplayName())

	got, err = update(&btapb.Instance{Type: btapb.Instance_PRODUCTION}, "type")
	require.NoError(t, err, "DEVELOPMENT can be upgraded to PRODUCTION")
	require.Equal(t, btapb.Instance_PRODUCTION, got.GetType())
	_, err = update(&btapb.Instance{Type: btapb.Instance_DEVELOPMENT}, "type")
	require.Equal(t, codes.InvalidArgument, status.Code(err), "PRODUCTION cannot be downgraded to DEVELOPMENT")

	got, err = update(&btapb.Instance{Edition: btapb.Instance_ENTERPRISE_PLUS}, "edition")
	require.NoError(t, err)
	require.Equal(t, btapb.Instance_ENTERPRISE_PLUS, got.GetEdition())

	for _, bad := range []string{"", "abc", strings.Repeat("d", 31)} {
		_, err = update(&btapb.Instance{DisplayName: bad}, "display_name")
		require.Equal(t, codes.InvalidArgument, status.Code(err), "display name %q", bad)
	}
	_, err = update(&btapb.Instance{State: btapb.Instance_CREATING}, "state")
	require.Equal(t, codes.InvalidArgument, status.Code(err), "output-only field in the mask")
	_, err = update(&btapb.Instance{DisplayName: "No mask"})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "update_mask is required")
	_, err = s.PartialUpdateInstance(ctx, &btapb.PartialUpdateInstanceRequest{
		Instance:   &btapb.Instance{Name: cadProject + "/instances/missing-inst", DisplayName: "Missing"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}},
	})
	require.Equal(t, codes.NotFound, status.Code(err))

	stored, err := s.GetInstance(ctx, &btapb.GetInstanceRequest{Name: name})
	require.NoError(t, err)
	require.Equal(t, "New display", stored.GetDisplayName(), "rejected updates leave the instance unchanged")
	require.Equal(t, btapb.Instance_PRODUCTION, stored.GetType())
}

// TestConformanceInstanceAdminUpdateInstanceScope: UpdateInstance updates
// only the display name and type; labels require PartialUpdateInstance.
func TestConformanceInstanceAdminUpdateInstanceScope(t *testing.T) {
	s := newInstanceTestServer(t)
	ctx := context.Background()
	req := cadCreateInstanceReq("update-inst", "update-c1")
	req.Instance.Labels = map[string]string{"keep": "me"}
	_, err := s.CreateInstance(ctx, req)
	require.NoError(t, err)
	name := cadProject + "/instances/update-inst"

	got, err := s.UpdateInstance(ctx, &btapb.Instance{Name: name, DisplayName: "Updated name", Labels: map[string]string{"ignored": "x"}})
	require.NoError(t, err)
	require.Equal(t, "Updated name", got.GetDisplayName())
	require.Equal(t, map[string]string{"keep": "me"}, got.GetLabels(), "UpdateInstance does not change labels")

	_, err = s.UpdateInstance(ctx, &btapb.Instance{Name: name})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "display_name is required")
	_, err = s.UpdateInstance(ctx, &btapb.Instance{Name: name, DisplayName: "Updated name", Type: btapb.Instance_DEVELOPMENT})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "PRODUCTION cannot be downgraded to DEVELOPMENT")
	_, err = s.UpdateInstance(ctx, &btapb.Instance{Name: cadProject + "/instances/missing-inst", DisplayName: "Missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestConformanceInstanceAdminClusterUpdates covers UpdateCluster and the
// PartialUpdateCluster masks.
func TestConformanceInstanceAdminClusterUpdates(t *testing.T) {
	s := newInstanceTestServer(t)
	ctx := context.Background()
	_, err := s.CreateInstance(ctx, cadCreateInstanceReq("cluster-upd", "upd-cluster"))
	require.NoError(t, err)
	name := cadProject + "/instances/cluster-upd/clusters/upd-cluster"

	_, err = s.UpdateCluster(ctx, &btapb.Cluster{Name: name, ServeNodes: -2})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s.UpdateCluster(ctx, &btapb.Cluster{Name: name + "x", ServeNodes: 2})
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = s.UpdateCluster(ctx, &btapb.Cluster{Name: name, ServeNodes: 2, Location: "projects/p/locations/elsewhere"})
	require.NoError(t, err)
	c, err := s.GetCluster(ctx, &btapb.GetClusterRequest{Name: name})
	require.NoError(t, err)
	require.EqualValues(t, 2, c.GetServeNodes())
	require.NotEqual(t, "projects/p/locations/elsewhere", c.GetLocation(), "location is immutable")

	autoscaling := &btapb.Cluster_ClusterConfig_{ClusterConfig: &btapb.Cluster_ClusterConfig{
		ClusterAutoscalingConfig: &btapb.Cluster_ClusterAutoscalingConfig{
			AutoscalingLimits:  &btapb.AutoscalingLimits{MinServeNodes: 1, MaxServeNodes: 5},
			AutoscalingTargets: &btapb.AutoscalingTargets{CpuUtilizationPercent: 60},
		},
	}}
	_, err = s.PartialUpdateCluster(ctx, &btapb.PartialUpdateClusterRequest{
		Cluster:    &btapb.Cluster{Name: name, ServeNodes: 7, Config: autoscaling, NodeScalingFactor: btapb.Cluster_NODE_SCALING_FACTOR_2X},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cluster_config.cluster_autoscaling_config"}},
	})
	require.NoError(t, err)
	c, err = s.GetCluster(ctx, &btapb.GetClusterRequest{Name: name})
	require.NoError(t, err)
	require.EqualValues(t, 5, c.GetClusterConfig().GetClusterAutoscalingConfig().GetAutoscalingLimits().GetMaxServeNodes())
	require.EqualValues(t, 2, c.GetServeNodes(), "serve_nodes is not in the mask")
	require.Equal(t, btapb.Cluster_NODE_SCALING_FACTOR_UNSPECIFIED, c.GetNodeScalingFactor(), "node_scaling_factor is not in the mask")

	_, err = s.PartialUpdateCluster(ctx, &btapb.PartialUpdateClusterRequest{
		Cluster:    &btapb.Cluster{Name: name, ServeNodes: 4},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"serve_nodes"}},
	})
	require.NoError(t, err)
	c, err = s.GetCluster(ctx, &btapb.GetClusterRequest{Name: name})
	require.NoError(t, err)
	require.EqualValues(t, 4, c.GetServeNodes())

	_, err = s.PartialUpdateCluster(ctx, &btapb.PartialUpdateClusterRequest{
		Cluster: &btapb.Cluster{Name: name, Location: "x"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"location"}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "immutable field in the mask")
	_, err = s.PartialUpdateCluster(ctx, &btapb.PartialUpdateClusterRequest{Cluster: &btapb.Cluster{Name: name}})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "update_mask is required")
}

// TestConformanceInstanceAdminDeleteClusterRules: the last cluster cannot be
// deleted, nor a cluster that an app profile routes to.
func TestConformanceInstanceAdminDeleteClusterRules(t *testing.T) {
	s := newInstanceTestServer(t)
	ctx := context.Background()
	_, err := s.CreateInstance(ctx, cadCreateInstanceReq("del-cluster", "first-cluster"))
	require.NoError(t, err)
	inst := cadProject + "/instances/del-cluster"
	first := inst + "/clusters/first-cluster"
	second := inst + "/clusters/second-cluster"

	_, err = s.DeleteCluster(ctx, &btapb.DeleteClusterRequest{Name: first})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "last cluster")
	_, err = s.DeleteCluster(ctx, &btapb.DeleteClusterRequest{Name: inst + "/clusters/missing-cluster"})
	require.Equal(t, codes.NotFound, status.Code(err))

	_, err = s.CreateCluster(ctx, &btapb.CreateClusterRequest{Parent: inst, ClusterId: "second-cluster", Cluster: &btapb.Cluster{}})
	require.NoError(t, err)
	_, err = s.CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{Parent: inst, AppProfileId: "pinned", AppProfile: cadSingleRouting("second-cluster")})
	require.NoError(t, err)
	_, err = s.DeleteCluster(ctx, &btapb.DeleteClusterRequest{Name: second})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "an app profile routes to the cluster")
	_, err = s.DeleteCluster(ctx, &btapb.DeleteClusterRequest{Name: first})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "the default app profile routes to the first cluster")

	_, err = s.DeleteAppProfile(ctx, &btapb.DeleteAppProfileRequest{Name: inst + "/appProfiles/pinned"})
	require.NoError(t, err)
	_, err = s.DeleteCluster(ctx, &btapb.DeleteClusterRequest{Name: second})
	require.NoError(t, err)
	_, err = s.GetCluster(ctx, &btapb.GetClusterRequest{Name: second})
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = s.DeleteCluster(ctx, &btapb.DeleteClusterRequest{Name: first})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "first-cluster is the last cluster again")
}

// TestConformanceInstanceAdminAppProfileRouting: routing policies must name
// existing clusters of the instance.
func TestConformanceInstanceAdminAppProfileRouting(t *testing.T) {
	s := newInstanceTestServer(t)
	ctx := context.Background()
	_, err := s.CreateInstance(ctx, cadCreateInstanceReq("routing-inst", "route-c1", "route-c2"))
	require.NoError(t, err)
	inst := cadProject + "/instances/routing-inst"
	create := func(id string, p *btapb.AppProfile) error {
		_, err := s.CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{Parent: inst, AppProfileId: id, AppProfile: p})
		return err
	}
	multi := func(ids ...string) *btapb.AppProfile {
		return &btapb.AppProfile{RoutingPolicy: &btapb.AppProfile_MultiClusterRoutingUseAny_{
			MultiClusterRoutingUseAny: &btapb.AppProfile_MultiClusterRoutingUseAny{ClusterIds: ids},
		}}
	}

	require.Equal(t, codes.InvalidArgument, status.Code(create("bad-single", cadSingleRouting("missing-cluster"))))
	require.Equal(t, codes.InvalidArgument, status.Code(create("empty-single", cadSingleRouting(""))))
	require.Equal(t, codes.InvalidArgument, status.Code(create("bad-multi", multi("route-c1", "missing-cluster"))))
	require.Equal(t, codes.InvalidArgument, status.Code(create("no-routing", &btapb.AppProfile{})))
	require.Equal(t, codes.InvalidArgument, status.Code(create("bad id!", cadSingleRouting("route-c1"))))

	require.NoError(t, create("single", cadSingleRouting("route-c1")))
	require.NoError(t, create("multi-subset", multi("route-c1", "route-c2")))
	require.NoError(t, create("multi-any", multi()))
	require.Equal(t, codes.AlreadyExists, status.Code(create("single", cadSingleRouting("route-c2"))))
	_, err = s.CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{Parent: cadProject + "/instances/missing-inst", AppProfileId: "p", AppProfile: multi()})
	require.Equal(t, codes.NotFound, status.Code(err))

	p, err := s.GetAppProfile(ctx, &btapb.GetAppProfileRequest{Name: inst + "/appProfiles/single"})
	require.NoError(t, err)
	require.Equal(t, btapb.AppProfile_PRIORITY_HIGH, p.GetStandardIsolation().GetPriority(), "standard isolation with high priority is the default")

	_, err = s.UpdateAppProfile(ctx, &btapb.UpdateAppProfileRequest{
		AppProfile: cadSingleRoutingNamed(inst+"/appProfiles/single", "missing-cluster"),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"single_cluster_routing"}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "update routing to a missing cluster")
	_, err = s.DeleteAppProfile(ctx, &btapb.DeleteAppProfileRequest{Name: inst + "/appProfiles/missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func cadSingleRoutingNamed(name, cluster string) *btapb.AppProfile {
	p := cadSingleRouting(cluster)
	p.Name = name
	return p
}

// TestConformanceInstanceAdminAppProfileEtag: UpdateAppProfile with a stale
// etag is rejected with ABORTED, and every update produces a new etag.
func TestConformanceInstanceAdminAppProfileEtag(t *testing.T) {
	s := newInstanceTestServer(t)
	ctx := context.Background()
	_, err := s.CreateInstance(ctx, cadCreateInstanceReq("etag-inst", "etag-c1", "etag-c2"))
	require.NoError(t, err)
	name := cadProject + "/instances/etag-inst/appProfiles/etagged"
	created, err := s.CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{
		Parent: cadProject + "/instances/etag-inst", AppProfileId: "etagged", AppProfile: cadSingleRouting("etag-c1"),
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.GetEtag())

	op, err := s.UpdateAppProfile(ctx, &btapb.UpdateAppProfileRequest{
		AppProfile: &btapb.AppProfile{Name: name, Description: "v2", Etag: created.GetEtag()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
	})
	require.NoError(t, err)
	updated := &btapb.AppProfile{}
	require.NoError(t, op.GetResponse().UnmarshalTo(updated))
	require.NotEqual(t, created.GetEtag(), updated.GetEtag())
	require.Equal(t, "etag-c1", updated.GetSingleClusterRouting().GetClusterId(), "routing is not in the mask")

	_, err = s.UpdateAppProfile(ctx, &btapb.UpdateAppProfileRequest{
		AppProfile: &btapb.AppProfile{Name: name, Description: "stale", Etag: created.GetEtag()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
	})
	require.Equal(t, codes.Aborted, status.Code(err))

	_, err = s.UpdateAppProfile(ctx, &btapb.UpdateAppProfileRequest{
		AppProfile: cadSingleRoutingNamed(name, "etag-c2"),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"single_cluster_routing"}},
	})
	require.NoError(t, err, "no etag means an unconditional update")
	got, err := s.GetAppProfile(ctx, &btapb.GetAppProfileRequest{Name: name})
	require.NoError(t, err)
	require.Equal(t, "etag-c2", got.GetSingleClusterRouting().GetClusterId())
	require.Equal(t, "v2", got.GetDescription())

	_, err = s.UpdateAppProfile(ctx, &btapb.UpdateAppProfileRequest{
		AppProfile: &btapb.AppProfile{Name: name}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "unsupported update path")
	_, err = s.UpdateAppProfile(ctx, &btapb.UpdateAppProfileRequest{AppProfile: &btapb.AppProfile{Name: name, Description: "x"}})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "update_mask is required")
	_, err = s.UpdateAppProfile(ctx, &btapb.UpdateAppProfileRequest{
		AppProfile: &btapb.AppProfile{Name: name + "x", Description: "x"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
	})
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestConformanceInstanceAdminListPaginationAndWildcard covers list
// pagination tokens and the '-' instance wildcard.
func TestConformanceInstanceAdminListPaginationAndWildcard(t *testing.T) {
	s := newInstanceTestServer(t)
	ctx := context.Background()
	_, err := s.CreateInstance(ctx, cadCreateInstanceReq("list-inst-a", "list-a-c1", "list-a-c2"))
	require.NoError(t, err)
	_, err = s.CreateInstance(ctx, cadCreateInstanceReq("list-inst-b", "list-b-c1"))
	require.NoError(t, err)
	_, err = s.CreateInstance(ctx, &btapb.CreateInstanceRequest{
		Parent: "projects/other-project", InstanceId: "list-inst-c", Instance: &btapb.Instance{DisplayName: "Other"},
		Clusters: map[string]*btapb.Cluster{"list-c-c1": {}},
	})
	require.NoError(t, err)
	instA := cadProject + "/instances/list-inst-a"
	_, err = s.CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{Parent: instA, AppProfileId: "extra", AppProfile: cadSingleRouting("list-a-c2")})
	require.NoError(t, err)

	insts, err := s.ListInstances(ctx, &btapb.ListInstancesRequest{Parent: cadProject})
	require.NoError(t, err)
	var names []string
	for _, i := range insts.GetInstances() {
		names = append(names, i.GetName())
	}
	require.Equal(t, []string{instA, cadProject + "/instances/list-inst-b"}, names, "only the parent project's instances, in name order")
	require.Empty(t, insts.GetNextPageToken())
	require.Empty(t, insts.GetFailedLocations())
	_, err = s.ListInstances(ctx, &btapb.ListInstancesRequest{Parent: cadProject, PageToken: "%%%"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	clusters, err := s.ListClusters(ctx, &btapb.ListClustersRequest{Parent: instA})
	require.NoError(t, err)
	require.Len(t, clusters.GetClusters(), 2)
	all, err := s.ListClusters(ctx, &btapb.ListClustersRequest{Parent: cadProject + "/instances/-"})
	require.NoError(t, err)
	require.Len(t, all.GetClusters(), 3, "'-' lists clusters of every instance in the project")
	for _, c := range all.GetClusters() {
		require.True(t, strings.HasPrefix(c.GetName(), cadProject+"/"), c.GetName())
	}
	_, err = s.ListClusters(ctx, &btapb.ListClustersRequest{Parent: instA, PageToken: "%%%"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	wild, err := s.ListAppProfiles(ctx, &btapb.ListAppProfilesRequest{Parent: cadProject + "/instances/-"})
	require.NoError(t, err)
	require.Len(t, wild.GetAppProfiles(), 3, "two default profiles and one extra")
	full, err := s.ListAppProfiles(ctx, &btapb.ListAppProfilesRequest{Parent: instA})
	require.NoError(t, err)
	require.Len(t, full.GetAppProfiles(), 2)
	var want, paged []string
	for _, p := range full.GetAppProfiles() {
		want = append(want, p.GetName())
	}
	token := ""
	for {
		resp, err := s.ListAppProfiles(ctx, &btapb.ListAppProfilesRequest{Parent: instA, PageSize: 1, PageToken: token})
		require.NoError(t, err)
		require.Len(t, resp.GetAppProfiles(), 1)
		paged = append(paged, resp.GetAppProfiles()[0].GetName())
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	require.True(t, sort.StringsAreSorted(paged))
	require.Equal(t, want, paged)
	_, err = s.ListAppProfiles(ctx, &btapb.ListAppProfilesRequest{Parent: instA, PageToken: "%%%"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestConformanceInstanceAdminProductionHardwareUnimplemented: hot tablets
// and memory layers describe production hardware; the registered RPCs
// return UNIMPLEMENTED.
func TestConformanceInstanceAdminProductionHardwareUnimplemented(t *testing.T) {
	e := cadNewEnv(t)
	e.createInstance()
	ctx := e.ctx
	_, err := e.ia.ListHotTablets(ctx, &btapb.ListHotTabletsRequest{Parent: cadCluster})
	require.Equal(t, codes.Unimplemented, status.Code(err), "ListHotTablets")
	_, err = e.ia.GetMemoryLayer(ctx, &btapb.GetMemoryLayerRequest{Name: cadCluster + "/memoryLayer"})
	require.Equal(t, codes.Unimplemented, status.Code(err), "GetMemoryLayer")
	_, err = e.ia.ListMemoryLayers(ctx, &btapb.ListMemoryLayersRequest{Parent: cadInstance + "/clusters/-"})
	require.Equal(t, codes.Unimplemented, status.Code(err), "ListMemoryLayers")
	_, err = e.ia.UpdateMemoryLayer(ctx, &btapb.UpdateMemoryLayerRequest{MemoryLayer: &btapb.MemoryLayer{Name: cadCluster + "/memoryLayer"}})
	require.Equal(t, codes.Unimplemented, status.Code(err), "UpdateMemoryLayer")
}

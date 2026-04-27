package bttest

import (
	"context"
	"fmt"
	"strings"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *server) localCreateInstance(ctx context.Context, req *btapb.CreateInstanceRequest) (*longrunning.Operation, error) {
	if req.Parent == "" || req.InstanceId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "parent and instance_id are required")
	}
	name := req.Parent + "/instances/" + req.InstanceId

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "instance %q already exists", name)
	}

	inst := &btapb.Instance{}
	if req.Instance != nil {
		inst = proto.Clone(req.Instance).(*btapb.Instance)
	}
	inst.Name = name
	if inst.DisplayName == "" {
		inst.DisplayName = req.InstanceId
	}
	if inst.Type == btapb.Instance_TYPE_UNSPECIFIED {
		inst.Type = btapb.Instance_PRODUCTION
	}
	inst.State = btapb.Instance_READY
	inst.CreateTime = timestamppb.Now()

	s.instances[name] = inst
	if s.adminBackend != nil {
		s.adminBackend.SaveInstance(inst)
	}

	for clusterID, cluster := range req.Clusters {
		if clusterID == "" {
			continue
		}
		stored := &btapb.Cluster{}
		if cluster != nil {
			stored = proto.Clone(cluster).(*btapb.Cluster)
		}
		stored.Name = name + "/clusters/" + clusterID
		if stored.Location == "" {
			stored.Location = req.Parent + "/locations/local"
		}
		stored.State = btapb.Cluster_READY
		s.clusters[stored.Name] = stored
		s.adminBackend.SaveCluster(name, stored)
	}

	s.ensureDefaultAppProfileLocked(name)
	return doneOperation(inst)
}

func (s *server) localGetInstance(ctx context.Context, req *btapb.GetInstanceRequest) (*btapb.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[req.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", req.Name)
	}
	return proto.Clone(inst).(*btapb.Instance), nil
}

func (s *server) localListInstances(ctx context.Context, req *btapb.ListInstancesRequest) (*btapb.ListInstancesResponse, error) {
	prefix := req.Parent + "/instances/"
	res := &btapb.ListInstancesResponse{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, inst := range s.instances {
		if strings.HasPrefix(name, prefix) {
			res.Instances = append(res.Instances, proto.Clone(inst).(*btapb.Instance))
		}
	}
	return res, nil
}

func (s *server) localUpdateInstance(ctx context.Context, req *btapb.Instance) (*btapb.Instance, error) {
	if req.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "instance name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.instances[req.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", req.Name)
	}
	stored.DisplayName = req.DisplayName
	if req.Type != btapb.Instance_TYPE_UNSPECIFIED {
		stored.Type = req.Type
	}
	if s.adminBackend != nil {
		s.adminBackend.SaveInstance(stored)
	}
	return proto.Clone(stored).(*btapb.Instance), nil
}

func (s *server) localPartialUpdateInstance(ctx context.Context, req *btapb.PartialUpdateInstanceRequest) (*longrunning.Operation, error) {
	inst := req.GetInstance()
	if inst == nil || inst.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "instance name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.instances[inst.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", inst.Name)
	}
	if req.GetUpdateMask() == nil || len(req.GetUpdateMask().GetPaths()) == 0 {
		stored.DisplayName = inst.DisplayName
		stored.Type = inst.Type
		stored.Labels = inst.Labels
	} else {
		for _, path := range req.GetUpdateMask().GetPaths() {
			switch path {
			case "display_name":
				stored.DisplayName = inst.DisplayName
			case "type":
				stored.Type = inst.Type
			case "labels":
				stored.Labels = inst.Labels
			default:
				return nil, status.Errorf(codes.InvalidArgument, "unsupported instance update field %q", path)
			}
		}
	}
	if s.adminBackend != nil {
		s.adminBackend.SaveInstance(stored)
	}
	return doneOperation(stored)
}

func (s *server) localDeleteInstance(ctx context.Context, req *btapb.DeleteInstanceRequest) (*empty.Empty, error) {
	name := req.GetName()
	if !regInstanceName.Match([]byte(name)) {
		return nil, status.Errorf(codes.InvalidArgument,
			"Error in field 'instance_name' : Invalid name for collection instances : Should match %s but found '%s'",
			instanceNameRegRaw, name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[name]; !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", name)
	}
	for tableName, tbl := range s.tables {
		if strings.HasPrefix(tableName, name+"/tables/") {
			tbl.rows.DeleteAll()
			s.tableBackend.Delete(tbl)
			delete(s.tables, tableName)
		}
	}
	for clusterName := range s.clusters {
		if strings.HasPrefix(clusterName, name+"/clusters/") {
			delete(s.clusters, clusterName)
		}
	}
	for appProfileName := range s.appProfiles {
		if strings.HasPrefix(appProfileName, name+"/appProfiles/") {
			delete(s.appProfiles, appProfileName)
		}
	}
	delete(s.instances, name)
	if s.adminBackend != nil {
		s.adminBackend.DeleteInstance(name)
	}
	return new(empty.Empty), nil
}

func (s *server) localCreateCluster(ctx context.Context, req *btapb.CreateClusterRequest) (*longrunning.Operation, error) {
	if req.Parent == "" || req.ClusterId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "parent and cluster_id are required")
	}
	name := req.Parent + "/clusters/" + req.ClusterId
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[req.Parent]; !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", req.Parent)
	}
	if _, ok := s.clusters[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "cluster %q already exists", name)
	}
	cluster := &btapb.Cluster{}
	if req.Cluster != nil {
		cluster = proto.Clone(req.Cluster).(*btapb.Cluster)
	}
	cluster.Name = name
	if cluster.Location == "" {
		cluster.Location = instanceProject(req.Parent) + "/locations/local"
	}
	cluster.State = btapb.Cluster_READY
	s.clusters[name] = cluster
	if s.adminBackend != nil {
		s.adminBackend.SaveCluster(req.Parent, cluster)
	}
	return doneOperation(cluster)
}

func (s *server) localGetCluster(ctx context.Context, req *btapb.GetClusterRequest) (*btapb.Cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cluster, ok := s.clusters[req.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", req.Name)
	}
	return proto.Clone(cluster).(*btapb.Cluster), nil
}

func (s *server) localListClusters(ctx context.Context, req *btapb.ListClustersRequest) (*btapb.ListClustersResponse, error) {
	prefix := req.Parent + "/clusters/"
	res := &btapb.ListClustersResponse{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, cluster := range s.clusters {
		if strings.HasPrefix(name, prefix) {
			res.Clusters = append(res.Clusters, proto.Clone(cluster).(*btapb.Cluster))
		}
	}
	return res, nil
}

func (s *server) localUpdateCluster(ctx context.Context, req *btapb.Cluster) (*longrunning.Operation, error) {
	if req.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "cluster name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clusters[req.Name]; !ok {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", req.Name)
	}
	req.State = btapb.Cluster_READY
	s.clusters[req.Name] = proto.Clone(req).(*btapb.Cluster)
	if s.adminBackend != nil {
		s.adminBackend.SaveCluster(parentInstanceFromChild(req.Name, "/clusters/"), req)
	}
	return doneOperation(req)
}

func (s *server) localPartialUpdateCluster(ctx context.Context, req *btapb.PartialUpdateClusterRequest) (*longrunning.Operation, error) {
	cluster := req.GetCluster()
	if cluster == nil || cluster.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "cluster name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.clusters[cluster.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", cluster.Name)
	}
	if req.GetUpdateMask() == nil || len(req.GetUpdateMask().GetPaths()) == 0 {
		stored = proto.Clone(cluster).(*btapb.Cluster)
		stored.State = btapb.Cluster_READY
	} else {
		for _, path := range req.GetUpdateMask().GetPaths() {
			switch path {
			case "serve_nodes":
				stored.ServeNodes = cluster.ServeNodes
			default:
				return nil, status.Errorf(codes.InvalidArgument, "unsupported cluster update field %q", path)
			}
		}
	}
	s.clusters[stored.Name] = stored
	if s.adminBackend != nil {
		s.adminBackend.SaveCluster(parentInstanceFromChild(stored.Name, "/clusters/"), stored)
	}
	return doneOperation(stored)
}

func (s *server) localDeleteCluster(ctx context.Context, req *btapb.DeleteClusterRequest) (*empty.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clusters[req.Name]; !ok {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", req.Name)
	}
	delete(s.clusters, req.Name)
	if s.adminBackend != nil {
		s.adminBackend.DeleteCluster(req.Name)
	}
	return new(empty.Empty), nil
}

func (s *server) localCreateAppProfile(ctx context.Context, req *btapb.CreateAppProfileRequest) (*btapb.AppProfile, error) {
	if req.Parent == "" || req.AppProfileId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "parent and app_profile_id are required")
	}
	name := req.Parent + "/appProfiles/" + req.AppProfileId
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[req.Parent]; !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", req.Parent)
	}
	if _, ok := s.appProfiles[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "app profile %q already exists", name)
	}
	appProfile := &btapb.AppProfile{}
	if req.AppProfile != nil {
		appProfile = proto.Clone(req.AppProfile).(*btapb.AppProfile)
	}
	appProfile.Name = name
	s.setDefaultAppProfileFieldsLocked(req.Parent, appProfile)
	s.appProfiles[name] = appProfile
	if s.adminBackend != nil {
		s.adminBackend.SaveAppProfile(req.Parent, appProfile)
	}
	return proto.Clone(appProfile).(*btapb.AppProfile), nil
}

func (s *server) localGetAppProfile(ctx context.Context, req *btapb.GetAppProfileRequest) (*btapb.AppProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	appProfile, ok := s.appProfiles[req.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "app profile %q not found", req.Name)
	}
	return proto.Clone(appProfile).(*btapb.AppProfile), nil
}

func (s *server) localListAppProfiles(ctx context.Context, req *btapb.ListAppProfilesRequest) (*btapb.ListAppProfilesResponse, error) {
	prefix := req.Parent + "/appProfiles/"
	res := &btapb.ListAppProfilesResponse{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, appProfile := range s.appProfiles {
		if strings.HasPrefix(name, prefix) {
			res.AppProfiles = append(res.AppProfiles, proto.Clone(appProfile).(*btapb.AppProfile))
		}
	}
	return res, nil
}

func (s *server) localUpdateAppProfile(ctx context.Context, req *btapb.UpdateAppProfileRequest) (*longrunning.Operation, error) {
	appProfile := req.GetAppProfile()
	if appProfile == nil || appProfile.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "app profile name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.appProfiles[appProfile.Name]; !ok {
		return nil, status.Errorf(codes.NotFound, "app profile %q not found", appProfile.Name)
	}
	parent := parentInstanceFromChild(appProfile.Name, "/appProfiles/")
	s.setDefaultAppProfileFieldsLocked(parent, appProfile)
	s.appProfiles[appProfile.Name] = proto.Clone(appProfile).(*btapb.AppProfile)
	if s.adminBackend != nil {
		s.adminBackend.SaveAppProfile(parent, appProfile)
	}
	return doneOperation(appProfile)
}

func (s *server) localDeleteAppProfile(ctx context.Context, req *btapb.DeleteAppProfileRequest) (*empty.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.appProfiles[req.Name]; !ok {
		return nil, status.Errorf(codes.NotFound, "app profile %q not found", req.Name)
	}
	delete(s.appProfiles, req.Name)
	if s.adminBackend != nil {
		s.adminBackend.DeleteAppProfile(req.Name)
	}
	return new(empty.Empty), nil
}

func (s *server) ensureDefaultAppProfileLocked(parent string) {
	name := parent + "/appProfiles/default"
	if _, ok := s.appProfiles[name]; ok {
		return
	}
	appProfile := &btapb.AppProfile{Name: name, Description: "LocalCloud default app profile"}
	s.setDefaultAppProfileFieldsLocked(parent, appProfile)
	s.appProfiles[name] = appProfile
	if s.adminBackend != nil {
		s.adminBackend.SaveAppProfile(parent, appProfile)
	}
}

func (s *server) localRequireInstance(parent string) error {
	if !isStrictAdmin() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[parent]; !ok {
		return status.Errorf(codes.NotFound, "instance %q not found", parent)
	}
	return nil
}

func (s *server) setDefaultAppProfileFieldsLocked(parent string, appProfile *btapb.AppProfile) {
	if appProfile.GetRoutingPolicy() == nil {
		clusterID := "local-cluster"
		for name := range s.clusters {
			if strings.HasPrefix(name, parent+"/clusters/") {
				clusterID = strings.TrimPrefix(name, parent+"/clusters/")
				break
			}
		}
		appProfile.RoutingPolicy = &btapb.AppProfile_SingleClusterRouting_{
			SingleClusterRouting: &btapb.AppProfile_SingleClusterRouting{
				ClusterId:                 clusterID,
				AllowTransactionalWrites: true,
			},
		}
	}
	if appProfile.GetIsolation() == nil {
		appProfile.Isolation = &btapb.AppProfile_StandardIsolation_{
			StandardIsolation: &btapb.AppProfile_StandardIsolation{
				Priority: btapb.AppProfile_PRIORITY_HIGH,
			},
		}
	}
}

func doneOperation(msg proto.Message) (*longrunning.Operation, error) {
	respAny, err := anypb.New(msg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to wrap result: %v", err)
	}
	return &longrunning.Operation{
		Name:   fmt.Sprintf("operations/op-%d", time.Now().UnixNano()),
		Done:   true,
		Result: &longrunning.Operation_Response{Response: respAny},
	}, nil
}

func instanceProject(instanceName string) string {
	idx := strings.LastIndex(instanceName, "/instances/")
	if idx < 0 {
		return instanceName
	}
	return instanceName[:idx]
}

func parentInstanceFromChild(name, marker string) string {
	idx := strings.LastIndex(name, marker)
	if idx < 0 {
		return name
	}
	return name[:idx]
}

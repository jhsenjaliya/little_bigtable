package bttest

import (
	"context"
	"database/sql"
	"regexp"
	"sort"
	"strings"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var _ btapb.BigtableInstanceAdminServer = (*server)(nil)

var (
	// Instance IDs are 6-33 characters: a letter, then letters, digits or
	// hyphens, ending with a letter or digit.
	instanceIDPattern = regexp.MustCompile(`^[a-z][-a-z0-9]{4,31}[a-z0-9]$`)
	clusterIDPattern  = regexp.MustCompile(`^[a-z][-a-z0-9]{4,28}[a-z0-9]$`)
	// instanceNameRegRaw validates fully qualified instance names.
	instanceNameRegRaw = `^projects/[a-z0-9][-a-z0-9.:]*[a-z0-9]/instances/[a-z][-a-z0-9]*[a-z0-9]$`
	regInstanceName    = regexp.MustCompile(instanceNameRegRaw)
	appProfileIDRegexp = regexp.MustCompile(`^[_a-zA-Z0-9][-_.a-zA-Z0-9]*$`)
)

func (s *server) CreateInstance(ctx context.Context, req *btapb.CreateInstanceRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	if !strings.HasPrefix(req.GetParent(), "projects/") {
		return nil, status.Error(codes.InvalidArgument, "parent must be projects/{project}")
	}
	if !instanceIDPattern.MatchString(req.GetInstanceId()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid instance ID %q: use 6-33 lowercase letters, digits or hyphens, starting with a letter", req.GetInstanceId())
	}
	if len(req.GetClusters()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one cluster must be specified")
	}
	if req.GetInstance().GetDisplayName() != "" {
		if err := validateInstanceDisplayName(req.Instance.DisplayName); err != nil {
			return nil, err
		}
	}
	name := req.Parent + "/instances/" + req.InstanceId
	clusters := make([]*btapb.Cluster, 0, len(req.Clusters))
	for _, id := range sortedKeys(req.Clusters) {
		c, err := newCluster(name, id, req.Clusters[id])
		if err != nil {
			return nil, err
		}
		clusters = append(clusters, c)
	}

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
	if inst.Edition == btapb.Instance_EDITION_UNSPECIFIED {
		inst.Edition = btapb.Instance_ENTERPRISE
	}
	inst.State = btapb.Instance_READY
	inst.CreateTime = start
	if err := s.adminBackend.SaveInstance(inst); err != nil {
		return nil, internalErr(err)
	}
	s.instances[name] = inst
	for _, c := range clusters {
		if err := s.adminBackend.SaveCluster(name, c); err != nil {
			return nil, internalErr(err)
		}
		s.clusters[c.Name] = c
	}
	if err := s.ensureDefaultAppProfileLocked(name); err != nil {
		return nil, err
	}
	return s.ops.complete(ctx, name, &btapb.CreateInstanceMetadata{OriginalRequest: req, RequestTime: start, FinishTime: timestamppb.Now()}, inst)
}

func newCluster(instance, id string, in *btapb.Cluster) (*btapb.Cluster, error) {
	if !clusterIDPattern.MatchString(id) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid cluster ID %q: use 6-30 lowercase letters, digits or hyphens, starting with a letter", id)
	}
	c := &btapb.Cluster{}
	if in != nil {
		c = proto.Clone(in).(*btapb.Cluster)
	}
	c.Name = instance + "/clusters/" + id
	if c.Location == "" {
		c.Location = instanceProject(instance) + "/locations/local"
	}
	if c.ServeNodes < 0 {
		return nil, status.Error(codes.InvalidArgument, "serve_nodes must not be negative")
	}
	if c.DefaultStorageType == btapb.StorageType_STORAGE_TYPE_UNSPECIFIED {
		c.DefaultStorageType = btapb.StorageType_SSD
	}
	c.State = btapb.Cluster_READY
	return c, nil
}

func (s *server) GetInstance(ctx context.Context, req *btapb.GetInstanceRequest) (*btapb.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", req.GetName())
	}
	return proto.Clone(inst).(*btapb.Instance), nil
}

func (s *server) ListInstances(ctx context.Context, req *btapb.ListInstancesRequest) (*btapb.ListInstancesResponse, error) {
	prefix := req.GetParent() + "/instances/"
	s.mu.Lock()
	var out []*btapb.Instance
	for name, inst := range s.instances {
		if strings.HasPrefix(name, prefix) {
			out = append(out, proto.Clone(inst).(*btapb.Instance))
		}
	}
	s.mu.Unlock()
	page, next, err := paginate(out, (*btapb.Instance).GetName, 0, req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &btapb.ListInstancesResponse{Instances: page, NextPageToken: next}, nil
}

// validateInstanceDisplayName enforces the required 4-30 character display name.
func validateInstanceDisplayName(name string) error {
	if len(name) < 4 || len(name) > 30 {
		return status.Error(codes.InvalidArgument, "display_name is required and must be 4-30 characters")
	}
	return nil
}

// UpdateInstance updates only display_name and type; other properties, such
// as labels, require PartialUpdateInstance.
func (s *server) UpdateInstance(ctx context.Context, req *btapb.Instance) (*btapb.Instance, error) {
	if req.GetName() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "instance name is required")
	}
	if err := validateInstanceDisplayName(req.DisplayName); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.instances[req.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", req.Name)
	}
	next := proto.Clone(stored).(*btapb.Instance)
	next.DisplayName = req.DisplayName
	if req.Type != btapb.Instance_TYPE_UNSPECIFIED {
		if stored.Type == btapb.Instance_PRODUCTION && req.Type == btapb.Instance_DEVELOPMENT {
			return nil, status.Error(codes.InvalidArgument, "a PRODUCTION instance cannot be changed to DEVELOPMENT")
		}
		next.Type = req.Type
	}
	if err := s.adminBackend.SaveInstance(next); err != nil {
		return nil, internalErr(err)
	}
	s.instances[req.Name] = next
	return proto.Clone(next).(*btapb.Instance), nil
}

func (s *server) PartialUpdateInstance(ctx context.Context, req *btapb.PartialUpdateInstanceRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	inst := req.GetInstance()
	if inst.GetName() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "instance name is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_mask is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.instances[inst.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", inst.Name)
	}
	next := proto.Clone(stored).(*btapb.Instance)
	for _, path := range paths {
		switch path {
		case "display_name":
			if err := validateInstanceDisplayName(inst.DisplayName); err != nil {
				return nil, err
			}
			next.DisplayName = inst.DisplayName
		case "type":
			if stored.Type == btapb.Instance_PRODUCTION && inst.Type == btapb.Instance_DEVELOPMENT {
				return nil, status.Error(codes.InvalidArgument, "a PRODUCTION instance cannot be changed to DEVELOPMENT")
			}
			next.Type = inst.Type
		case "labels":
			next.Labels = inst.Labels
		case "edition":
			next.Edition = inst.Edition
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported instance update field %q", path)
		}
	}
	if err := s.adminBackend.SaveInstance(next); err != nil {
		return nil, internalErr(err)
	}
	s.instances[inst.Name] = next
	return s.ops.complete(ctx, inst.Name, &btapb.UpdateInstanceMetadata{OriginalRequest: req, RequestTime: start, FinishTime: timestamppb.Now()}, next)
}

func (s *server) DeleteInstance(ctx context.Context, req *btapb.DeleteInstanceRequest) (*empty.Empty, error) {
	name := req.GetName()
	if !regInstanceName.MatchString(name) {
		return nil, status.Errorf(codes.InvalidArgument,
			"Error in field 'instance_name' : Invalid name for collection instances : Should match %s but found '%s'",
			instanceNameRegRaw, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[name]; !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", name)
	}
	// Bigtable refuses to delete an instance that holds protected resources
	// or backups.
	for tableName, tbl := range s.tables {
		if !strings.HasPrefix(tableName, name+"/tables/") {
			continue
		}
		if err := s.checkTableDeletableLocked(tbl); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "instance %q cannot be deleted: %s", name, status.Convert(err).Message())
		}
	}
	if backups, err := s.backupsUnder(ctx, name+"/clusters/"); err != nil {
		return nil, internalErr(err)
	} else if len(backups) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "instance %q cannot be deleted while it contains backups", name)
	}
	views, err := s.lvBackend.store.listPrefix(ctx, nil, name+"/logicalViews/")
	if err != nil {
		return nil, internalErr(err)
	}
	for _, lv := range views {
		if lv.GetDeletionProtection() {
			return nil, status.Errorf(codes.FailedPrecondition, "instance %q has logical view %q with deletion protection enabled", name, lv.GetName())
		}
	}
	for mvName, mv := range s.materializedViews {
		if strings.HasPrefix(mvName, name+"/") && mv.GetDeletionProtection() {
			return nil, status.Errorf(codes.FailedPrecondition, "instance %q has materialized view %q with deletion protection enabled", name, mvName)
		}
	}

	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		for tableName, tbl := range s.tables {
			if !strings.HasPrefix(tableName, name+"/tables/") {
				continue
			}
			if err := tbl.rows.clear(ctx, tx); err != nil {
				return err
			}
			if err := s.tableBackend.Delete(ctx, tx, tbl); err != nil {
				return err
			}
			if err := s.changeLog.purgeTable(ctx, tx, tableName); err != nil {
				return err
			}
		}
		for _, t := range s.deletedTables {
			if strings.HasPrefix(t.name(), name+"/tables/") {
				if err := t.rows.clear(ctx, tx); err != nil {
					return err
				}
				if err := s.tableBackend.Delete(ctx, tx, t); err != nil {
					return err
				}
			}
		}
		for _, store := range []interface {
			removePrefix(context.Context, sqlExecutor, string) error
		}{&s.avBackend.store, &s.sbBackend.store, &s.lvBackend.store} {
			if err := store.removePrefix(ctx, tx, name+"/"); err != nil {
				return err
			}
		}
		for mvName := range s.materializedViews {
			if strings.HasPrefix(mvName, name+"/") {
				if err := s.dropMaterializedViewStorage(ctx, tx, mvName); err != nil {
					return err
				}
			}
		}
		if err := s.iamBackend.deletePrefix(ctx, tx, name); err != nil {
			return err
		}
		return s.adminBackend.deleteInstanceTx(ctx, tx, name)
	})
	if err != nil {
		return nil, internalErr(err)
	}
	for tableName := range s.tables {
		if strings.HasPrefix(tableName, name+"/tables/") {
			delete(s.tables, tableName)
		}
	}
	for key, t := range s.deletedTables {
		if strings.HasPrefix(t.name(), name+"/tables/") {
			delete(s.deletedTables, key)
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
	for mvName := range s.materializedViews {
		if strings.HasPrefix(mvName, name+"/") {
			s.forgetMaterializedViewLocked(mvName)
		}
	}
	delete(s.instances, name)
	return new(empty.Empty), nil
}

func (s *server) CreateCluster(ctx context.Context, req *btapb.CreateClusterRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	if req.GetParent() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "parent is required")
	}
	c, err := newCluster(req.Parent, req.GetClusterId(), req.GetCluster())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[req.Parent]; !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", req.Parent)
	}
	if _, ok := s.clusters[c.Name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "cluster %q already exists", c.Name)
	}
	if err := s.adminBackend.SaveCluster(req.Parent, c); err != nil {
		return nil, internalErr(err)
	}
	s.clusters[c.Name] = c
	md := &btapb.CreateClusterMetadata{OriginalRequest: req, RequestTime: start, FinishTime: timestamppb.Now(), Tables: map[string]*btapb.CreateClusterMetadata_TableProgress{}}
	for tableName := range s.tables {
		if strings.HasPrefix(tableName, req.Parent+"/tables/") {
			md.Tables[tableName] = &btapb.CreateClusterMetadata_TableProgress{State: btapb.CreateClusterMetadata_TableProgress_COMPLETED}
		}
	}
	return s.ops.complete(ctx, c.Name, md, c)
}

func (s *server) GetCluster(ctx context.Context, req *btapb.GetClusterRequest) (*btapb.Cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cluster, ok := s.clusters[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", req.GetName())
	}
	return proto.Clone(cluster).(*btapb.Cluster), nil
}

// ListClusters accepts projects/p/instances/- to list every instance's clusters.
func (s *server) ListClusters(ctx context.Context, req *btapb.ListClustersRequest) (*btapb.ListClustersResponse, error) {
	parent := req.GetParent()
	prefix := parent + "/clusters/"
	wildcard := strings.HasSuffix(parent, "/instances/-")
	if wildcard {
		prefix = strings.TrimSuffix(parent, "-")
	}
	s.mu.Lock()
	var out []*btapb.Cluster
	for name, cluster := range s.clusters {
		if strings.HasPrefix(name, prefix) {
			out = append(out, proto.Clone(cluster).(*btapb.Cluster))
		}
	}
	s.mu.Unlock()
	page, next, err := paginate(out, (*btapb.Cluster).GetName, 0, req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &btapb.ListClustersResponse{Clusters: page, NextPageToken: next}, nil
}

// UpdateCluster updates serve_nodes; other fields are immutable.
func (s *server) UpdateCluster(ctx context.Context, req *btapb.Cluster) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	if req.GetName() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "cluster name is required")
	}
	if req.ServeNodes < 0 {
		return nil, status.Error(codes.InvalidArgument, "serve_nodes must not be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.clusters[req.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", req.Name)
	}
	next := proto.Clone(stored).(*btapb.Cluster)
	next.ServeNodes = req.ServeNodes
	if err := s.adminBackend.SaveCluster(parentInstanceFromChild(next.Name, "/clusters/"), next); err != nil {
		return nil, internalErr(err)
	}
	s.clusters[next.Name] = next
	return s.ops.complete(ctx, next.Name, &btapb.UpdateClusterMetadata{OriginalRequest: req, RequestTime: start, FinishTime: timestamppb.Now()}, next)
}

func (s *server) PartialUpdateCluster(ctx context.Context, req *btapb.PartialUpdateClusterRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	cluster := req.GetCluster()
	if cluster.GetName() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "cluster name is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_mask is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.clusters[cluster.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", cluster.Name)
	}
	next := proto.Clone(stored).(*btapb.Cluster)
	for _, path := range paths {
		switch {
		case path == "serve_nodes":
			next.ServeNodes = cluster.ServeNodes
		case path == "cluster_config" || strings.HasPrefix(path, "cluster_config.cluster_autoscaling_config"):
			next.Config = proto.Clone(cluster).(*btapb.Cluster).Config
		case path == "node_scaling_factor":
			next.NodeScalingFactor = cluster.NodeScalingFactor
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported cluster update field %q", path)
		}
	}
	if err := s.adminBackend.SaveCluster(parentInstanceFromChild(next.Name, "/clusters/"), next); err != nil {
		return nil, internalErr(err)
	}
	s.clusters[next.Name] = next
	return s.ops.complete(ctx, next.Name, &btapb.PartialUpdateClusterMetadata{OriginalRequest: req, RequestTime: start, FinishTime: timestamppb.Now()}, next)
}

func (s *server) DeleteCluster(ctx context.Context, req *btapb.DeleteClusterRequest) (*empty.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clusters[req.GetName()]; !ok {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", req.GetName())
	}
	instance := parentInstanceFromChild(req.Name, "/clusters/")
	remaining := 0
	for name := range s.clusters {
		if strings.HasPrefix(name, instance+"/clusters/") {
			remaining++
		}
	}
	if remaining <= 1 {
		return nil, status.Errorf(codes.FailedPrecondition, "cluster %q is the last cluster of its instance; delete the instance instead", req.Name)
	}
	if backups, err := s.backupsUnder(ctx, req.Name+"/backups/"); err != nil {
		return nil, internalErr(err)
	} else if len(backups) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "cluster %q cannot be deleted while it contains backups", req.Name)
	}
	for _, p := range s.appProfiles {
		clusterID := strings.TrimPrefix(req.Name, instance+"/clusters/")
		routed := p.GetSingleClusterRouting().GetClusterId() == clusterID
		for _, id := range p.GetMultiClusterRoutingUseAny().GetClusterIds() {
			routed = routed || id == clusterID
		}
		if routed && strings.HasPrefix(p.GetName(), instance+"/") {
			return nil, status.Errorf(codes.FailedPrecondition, "cluster %q is used by app profile %q", req.Name, p.GetName())
		}
	}
	if err := s.adminBackend.DeleteCluster(req.Name); err != nil {
		return nil, internalErr(err)
	}
	delete(s.clusters, req.Name)
	return new(empty.Empty), nil
}

// validateRoutingLocked checks that an app profile routes to existing
// clusters. With ignore_warnings Bigtable skips safety checks, but routing
// must still name real clusters.
func (s *server) validateRoutingLocked(instance string, p *btapb.AppProfile) error {
	clusterExists := func(id string) bool {
		_, ok := s.clusters[instance+"/clusters/"+id]
		return ok
	}
	switch r := p.GetRoutingPolicy().(type) {
	case *btapb.AppProfile_SingleClusterRouting_:
		if r.SingleClusterRouting.GetClusterId() == "" || !clusterExists(r.SingleClusterRouting.GetClusterId()) {
			return status.Errorf(codes.InvalidArgument, "single_cluster_routing.cluster_id %q is not a cluster of %q", r.SingleClusterRouting.GetClusterId(), instance)
		}
	case *btapb.AppProfile_MultiClusterRoutingUseAny_:
		for _, id := range r.MultiClusterRoutingUseAny.GetClusterIds() {
			if !clusterExists(id) {
				return status.Errorf(codes.InvalidArgument, "multi_cluster_routing_use_any cluster %q is not a cluster of %q", id, instance)
			}
		}
	case nil:
		return status.Error(codes.InvalidArgument, "an app profile requires a routing policy")
	}
	return nil
}

func (s *server) CreateAppProfile(ctx context.Context, req *btapb.CreateAppProfileRequest) (*btapb.AppProfile, error) {
	if req.GetParent() == "" || !appProfileIDRegexp.MatchString(req.GetAppProfileId()) {
		return nil, status.Errorf(codes.InvalidArgument, "parent and a valid app_profile_id are required")
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
	p := &btapb.AppProfile{}
	if req.AppProfile != nil {
		p = proto.Clone(req.AppProfile).(*btapb.AppProfile)
	}
	p.Name = name
	if err := s.validateRoutingLocked(req.Parent, p); err != nil {
		return nil, err
	}
	s.setDefaultIsolation(p)
	p.Etag = contentEtag(p)
	if err := s.adminBackend.SaveAppProfile(req.Parent, p); err != nil {
		return nil, internalErr(err)
	}
	s.appProfiles[name] = p
	return proto.Clone(p).(*btapb.AppProfile), nil
}

func (s *server) GetAppProfile(ctx context.Context, req *btapb.GetAppProfileRequest) (*btapb.AppProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.appProfiles[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "app profile %q not found", req.GetName())
	}
	return proto.Clone(p).(*btapb.AppProfile), nil
}

func (s *server) ListAppProfiles(ctx context.Context, req *btapb.ListAppProfilesRequest) (*btapb.ListAppProfilesResponse, error) {
	parent := req.GetParent()
	prefix := parent + "/appProfiles/"
	if strings.HasSuffix(parent, "/instances/-") {
		prefix = strings.TrimSuffix(parent, "-")
	}
	s.mu.Lock()
	var out []*btapb.AppProfile
	for name, p := range s.appProfiles {
		if strings.HasPrefix(name, prefix) {
			out = append(out, proto.Clone(p).(*btapb.AppProfile))
		}
	}
	s.mu.Unlock()
	page, next, err := paginate(out, (*btapb.AppProfile).GetName, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &btapb.ListAppProfilesResponse{AppProfiles: page, NextPageToken: next}, nil
}

func (s *server) UpdateAppProfile(ctx context.Context, req *btapb.UpdateAppProfileRequest) (*longrunning.Operation, error) {
	update := req.GetAppProfile()
	if update.GetName() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "app profile name is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_mask is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.appProfiles[update.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "app profile %q not found", update.Name)
	}
	if err := checkEtag(update.GetEtag(), cur.GetEtag()); err != nil {
		return nil, err
	}
	next := proto.Clone(cur).(*btapb.AppProfile)
	src := proto.Clone(update).(*btapb.AppProfile)
	for _, path := range paths {
		switch path {
		case "description":
			next.Description = src.Description
		case "multi_cluster_routing_use_any", "single_cluster_routing":
			next.RoutingPolicy = src.RoutingPolicy
		case "standard_isolation", "data_boost_isolation_read_only", "priority", "standard_isolation.priority":
			next.Isolation = src.Isolation
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported app profile update field %q", path)
		}
	}
	instance := parentInstanceFromChild(next.Name, "/appProfiles/")
	if err := s.validateRoutingLocked(instance, next); err != nil {
		return nil, err
	}
	s.setDefaultIsolation(next)
	next.Etag = contentEtag(next)
	if err := s.adminBackend.SaveAppProfile(instance, next); err != nil {
		return nil, internalErr(err)
	}
	s.appProfiles[next.Name] = next
	return s.ops.complete(ctx, next.Name, &btapb.UpdateAppProfileMetadata{}, next)
}

func (s *server) DeleteAppProfile(ctx context.Context, req *btapb.DeleteAppProfileRequest) (*empty.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.appProfiles[req.GetName()]; !ok {
		return nil, status.Errorf(codes.NotFound, "app profile %q not found", req.GetName())
	}
	if err := s.adminBackend.DeleteAppProfile(req.Name); err != nil {
		return nil, internalErr(err)
	}
	delete(s.appProfiles, req.Name)
	return new(empty.Empty), nil
}

// ensureDefaultAppProfileLocked creates the "default" profile Bigtable gives
// every instance: single-cluster routing to the first cluster with
// transactional writes allowed.
func (s *server) ensureDefaultAppProfileLocked(instance string) error {
	name := instance + "/appProfiles/default"
	if _, ok := s.appProfiles[name]; ok {
		return nil
	}
	var clusterIDs []string
	for c := range s.clusters {
		if strings.HasPrefix(c, instance+"/clusters/") {
			clusterIDs = append(clusterIDs, strings.TrimPrefix(c, instance+"/clusters/"))
		}
	}
	sort.Strings(clusterIDs)
	clusterID := "local-cluster"
	if len(clusterIDs) > 0 {
		clusterID = clusterIDs[0]
	}
	p := &btapb.AppProfile{
		Name:        name,
		Description: "Default app profile",
		RoutingPolicy: &btapb.AppProfile_SingleClusterRouting_{SingleClusterRouting: &btapb.AppProfile_SingleClusterRouting{
			ClusterId: clusterID, AllowTransactionalWrites: true,
		}},
	}
	s.setDefaultIsolation(p)
	p.Etag = contentEtag(p)
	if err := s.adminBackend.SaveAppProfile(instance, p); err != nil {
		return internalErr(err)
	}
	s.appProfiles[name] = p
	return nil
}

func (s *server) setDefaultIsolation(p *btapb.AppProfile) {
	if p.GetIsolation() == nil {
		p.Isolation = &btapb.AppProfile_StandardIsolation_{
			StandardIsolation: &btapb.AppProfile_StandardIsolation{Priority: btapb.AppProfile_PRIORITY_HIGH},
		}
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

// Hot tablets and memory layers describe production tablet servers and
// provisioned memory hardware, which a single-process emulator does not have.

func (s *server) ListHotTablets(ctx context.Context, req *btapb.ListHotTabletsRequest) (*btapb.ListHotTabletsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "hot tablets describe production tablet servers and are not emulated")
}

func (s *server) GetMemoryLayer(ctx context.Context, req *btapb.GetMemoryLayerRequest) (*btapb.MemoryLayer, error) {
	return nil, status.Error(codes.Unimplemented, "memory layers require production Bigtable hardware and are not emulated")
}

func (s *server) ListMemoryLayers(ctx context.Context, req *btapb.ListMemoryLayersRequest) (*btapb.ListMemoryLayersResponse, error) {
	return nil, status.Error(codes.Unimplemented, "memory layers require production Bigtable hardware and are not emulated")
}

func (s *server) UpdateMemoryLayer(ctx context.Context, req *btapb.UpdateMemoryLayerRequest) (*longrunning.Operation, error) {
	return nil, status.Error(codes.Unimplemented, "memory layers require production Bigtable hardware and are not emulated")
}

// IAM is shared by the instance and table admin services.

func (s *server) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return s.getIamPolicy(ctx, req)
}

func (s *server) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return s.setIamPolicy(ctx, req)
}

func (s *server) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return s.testIamPermissions(ctx, req)
}

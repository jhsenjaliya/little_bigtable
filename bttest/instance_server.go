// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bttest

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
)

var _ btapb.BigtableInstanceAdminServer = (*server)(nil)

var errUnimplemented = status.Error(codes.Unimplemented, "unimplemented feature")

func (s *server) CreateInstance(ctx context.Context, req *btapb.CreateInstanceRequest) (*longrunning.Operation, error) {
	return s.localCreateInstance(ctx, req)
}

func (s *server) GetInstance(ctx context.Context, req *btapb.GetInstanceRequest) (*btapb.Instance, error) {
	return s.localGetInstance(ctx, req)
}

func (s *server) ListInstances(ctx context.Context, req *btapb.ListInstancesRequest) (*btapb.ListInstancesResponse, error) {
	return s.localListInstances(ctx, req)
}

func (s *server) UpdateInstance(ctx context.Context, req *btapb.Instance) (*btapb.Instance, error) {
	return s.localUpdateInstance(ctx, req)
}

func (s *server) PartialUpdateInstance(ctx context.Context, req *btapb.PartialUpdateInstanceRequest) (*longrunning.Operation, error) {
	return s.localPartialUpdateInstance(ctx, req)
}

var (
	// As per https://godoc.org/google.golang.org/genproto/googleapis/bigtable/admin/v2#DeleteInstanceRequest.Name
	// the Name should be of the form:
	//    `projects/<project>/instances/<instance>`
	instanceNameRegRaw = `^projects/[a-z][a-z0-9\\-]+[a-z0-9]/instances/[a-z][a-z0-9\\-]+[a-z0-9]$`
	regInstanceName    = regexp.MustCompile(instanceNameRegRaw)
)

func (s *server) DeleteInstance(ctx context.Context, req *btapb.DeleteInstanceRequest) (*empty.Empty, error) {
	return s.localDeleteInstance(ctx, req)
}

func (s *server) CreateCluster(ctx context.Context, req *btapb.CreateClusterRequest) (*longrunning.Operation, error) {
	return s.localCreateCluster(ctx, req)
}

func (s *server) GetCluster(ctx context.Context, req *btapb.GetClusterRequest) (*btapb.Cluster, error) {
	return s.localGetCluster(ctx, req)
}

func (s *server) ListClusters(ctx context.Context, req *btapb.ListClustersRequest) (*btapb.ListClustersResponse, error) {
	return s.localListClusters(ctx, req)
}

func (s *server) UpdateCluster(ctx context.Context, req *btapb.Cluster) (*longrunning.Operation, error) {
	return s.localUpdateCluster(ctx, req)
}

func (s *server) PartialUpdateCluster(ctx context.Context, req *btapb.PartialUpdateClusterRequest) (*longrunning.Operation, error) {
	return s.localPartialUpdateCluster(ctx, req)
}

func (s *server) DeleteCluster(ctx context.Context, req *btapb.DeleteClusterRequest) (*empty.Empty, error) {
	return s.localDeleteCluster(ctx, req)
}

func (s *server) CreateAppProfile(ctx context.Context, req *btapb.CreateAppProfileRequest) (*btapb.AppProfile, error) {
	return s.localCreateAppProfile(ctx, req)
}

func (s *server) GetAppProfile(ctx context.Context, req *btapb.GetAppProfileRequest) (*btapb.AppProfile, error) {
	return s.localGetAppProfile(ctx, req)
}

func (s *server) ListAppProfiles(ctx context.Context, req *btapb.ListAppProfilesRequest) (*btapb.ListAppProfilesResponse, error) {
	return s.localListAppProfiles(ctx, req)
}

func (s *server) UpdateAppProfile(ctx context.Context, req *btapb.UpdateAppProfileRequest) (*longrunning.Operation, error) {
	return s.localUpdateAppProfile(ctx, req)
}

func (s *server) DeleteAppProfile(ctx context.Context, req *btapb.DeleteAppProfileRequest) (*empty.Empty, error) {
	return s.localDeleteAppProfile(ctx, req)
}

func (s *server) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.iamPolicies[req.GetResource()]; ok {
		return p, nil
	}
	return &iampb.Policy{Version: 1}, nil
}

func (s *server) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.iamPolicies == nil {
		s.iamPolicies = make(map[string]*iampb.Policy)
	}
	s.iamPolicies[req.GetResource()] = req.GetPolicy()
	return req.GetPolicy(), nil
}

func (s *server) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return &iampb.TestIamPermissionsResponse{
		Permissions: req.GetPermissions(),
	}, nil
}

func (s *server) ListHotTablets(ctx context.Context, req *btapb.ListHotTabletsRequest) (*btapb.ListHotTabletsResponse, error) {
	return nil, errUnimplemented
}

// CreateMaterializedView parses the SQL query in the request, registers a CMV
// config on the server, and stores the view metadata for later retrieval.
func (s *server) CreateMaterializedView(ctx context.Context, req *btapb.CreateMaterializedViewRequest) (*longrunning.Operation, error) {
	if req.MaterializedViewId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "materialized_view_id is required")
	}
	mv := req.GetMaterializedView()
	if mv == nil || mv.Query == "" {
		return nil, status.Errorf(codes.InvalidArgument, "materialized_view.query is required")
	}

	cfg, err := ParseCMVConfigFromSQL(req.MaterializedViewId, mv.Query)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid materialized view query: %v", err)
	}

	name := req.Parent + "/materializedViews/" + req.MaterializedViewId

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.materializedViews[name]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "materialized view %q already exists", name)
	}

	s.cmvs.register(*cfg)
	stored := &btapb.MaterializedView{
		Name:               name,
		Query:              mv.Query,
		DeletionProtection: mv.DeletionProtection,
	}
	s.materializedViews[name] = stored
	s.mvBackend.Save(name, mv.Query, mv.DeletionProtection)

	respAny, err := anypb.New(stored)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to wrap result: %v", err)
	}
	return &longrunning.Operation{
		Name:   fmt.Sprintf("operations/op-%d", time.Now().UnixNano()),
		Done:   true,
		Result: &longrunning.Operation_Response{Response: respAny},
	}, nil
}

func (s *server) GetMaterializedView(ctx context.Context, req *btapb.GetMaterializedViewRequest) (*btapb.MaterializedView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	mv, ok := s.materializedViews[req.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "materialized view %q not found", req.Name)
	}
	return mv, nil
}

func (s *server) ListMaterializedViews(ctx context.Context, req *btapb.ListMaterializedViewsRequest) (*btapb.ListMaterializedViewsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var views []*btapb.MaterializedView
	for name, mv := range s.materializedViews {
		if strings.HasPrefix(name, req.Parent+"/") {
			views = append(views, mv)
		}
	}
	return &btapb.ListMaterializedViewsResponse{MaterializedViews: views}, nil
}

// UpdateMaterializedView supports toggling DeletionProtection. Query changes
// are not supported since CMV queries are immutable after creation.
func (s *server) UpdateMaterializedView(ctx context.Context, req *btapb.UpdateMaterializedViewRequest) (*longrunning.Operation, error) {
	mv := req.GetMaterializedView()
	if mv == nil {
		return nil, status.Errorf(codes.InvalidArgument, "materialized_view is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.materializedViews[mv.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "materialized view %q not found", mv.Name)
	}

	for _, path := range req.GetUpdateMask().GetPaths() {
		switch path {
		case "deletion_protection":
			stored.DeletionProtection = mv.DeletionProtection
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update field: %q", path)
		}
	}
	s.mvBackend.Save(stored.Name, stored.Query, stored.DeletionProtection)

	respAny, err := anypb.New(stored)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to wrap result: %v", err)
	}
	return &longrunning.Operation{
		Name:   fmt.Sprintf("operations/op-%d", time.Now().UnixNano()),
		Done:   true,
		Result: &longrunning.Operation_Response{Response: respAny},
	}, nil
}

func (s *server) DeleteMaterializedView(ctx context.Context, req *btapb.DeleteMaterializedViewRequest) (*empty.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	mv, ok := s.materializedViews[req.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "materialized view %q not found", req.Name)
	}
	if mv.DeletionProtection {
		return nil, status.Errorf(codes.FailedPrecondition, "materialized view %q is protected against deletion", req.Name)
	}

	// Extract parent and view ID from the full resource name.
	parts := strings.Split(mv.Name, "/materializedViews/")
	if len(parts) == 2 {
		s.cmvs.deregister(parts[1])
		fqShadow := parts[0] + "/tables/" + parts[1]
		if shadowTbl, exists := s.tables[fqShadow]; exists {
			s.tableBackend.Delete(shadowTbl)
			shadowTbl.rows.DeleteAll()
			delete(s.tables, fqShadow)
		}
	}
	s.mvBackend.Delete(req.Name)
	delete(s.materializedViews, req.Name)
	return new(empty.Empty), nil
}

// Logical view methods moved to localcloud_logical_views.go

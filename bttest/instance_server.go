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

var (
	// As per https://godoc.org/google.golang.org/genproto/googleapis/bigtable/admin/v2#DeleteInstanceRequest.Name
	// the Name should be of the form:
	//    `projects/<project>/instances/<instance>`
	instanceNameRegRaw = `^projects/[a-z][a-z0-9\\-]+[a-z0-9]/instances/[a-z][a-z0-9\\-]+[a-z0-9]$`
	regInstanceName    = regexp.MustCompile(instanceNameRegRaw)
)

// createInstanceName builds a fully qualified instance resource name.
func createInstanceName(projectId, instanceId string) string {
	return "projects/" + projectId + "/instances/" + instanceId
}

func (s *server) CreateInstance(ctx context.Context, req *btapb.CreateInstanceRequest) (*longrunning.Operation, error) {
	if req.InstanceId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "instance_id is required")
	}
	// Validate parent matches expected project pattern.
	parentRE := regexp.MustCompile(`^projects/[a-z][a-z0-9\-]+[a-z0-9]$`)
	if !parentRE.MatchString(req.Parent) {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid parent %q: must match projects/{project}", req.Parent)
	}
	inst := req.GetInstance()
	if inst == nil {
		inst = &btapb.Instance{}
	}
	name := createInstanceName(instanceNameFromParent(req.Parent), req.InstanceId)

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.instances[name]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "instance %q already exists", name)
	}

	// Build the instance object with provided or default values.
	displayName := inst.DisplayName
	if displayName == "" {
		displayName = req.InstanceId
	}
	instType := inst.Type
	if instType == btapb.Instance_TYPE_UNSPECIFIED {
		instType = btapb.Instance_PRODUCTION
	}

	stored := &btapb.Instance{
		Name:        name,
		DisplayName: displayName,
		Type:        instType,
		State:       btapb.Instance_READY,
		Labels:      inst.Labels,
	}

	if err := s.instanceBackend.Save(instanceNameFromParent(req.Parent), req.InstanceId, stored); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to persist instance: %v", err)
	}
	s.instances[name] = stored

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

func (s *server) GetInstance(ctx context.Context, req *btapb.GetInstanceRequest) (*btapb.Instance, error) {
	name := req.GetName()
	if !regInstanceName.Match([]byte(name)) {
		return nil, status.Errorf(codes.InvalidArgument,
			"Error in field 'instance_name' : Invalid name for collection instances : Should match %s but found '%s'",
			instanceNameRegRaw, name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	inst, ok := s.instances[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", name)
	}
	return inst, nil
}

func (s *server) ListInstances(ctx context.Context, req *btapb.ListInstancesRequest) (*btapb.ListInstancesResponse, error) {
	parent := req.GetParent()

	s.mu.Lock()
	defer s.mu.Unlock()

	var instances []*btapb.Instance
	prefix := parent + "/instances/"
	for name, inst := range s.instances {
		if strings.HasPrefix(name, prefix) {
			// Ensure exact segment boundary: "projects/a" must not match "projects/ab".
			suffix := name[len(prefix):]
			if !strings.Contains(suffix, "/") {
				instances = append(instances, inst)
			}
		}
	}
	return &btapb.ListInstancesResponse{Instances: instances}, nil
}

func (s *server) UpdateInstance(ctx context.Context, req *btapb.Instance) (*btapb.Instance, error) {
	name := req.GetName()
	if !regInstanceName.Match([]byte(name)) {
		return nil, status.Errorf(codes.InvalidArgument,
			"Error in field 'instance_name' : Invalid name for collection instances : Should match %s but found '%s'",
			instanceNameRegRaw, name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.instances[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", name)
	}

	// Update mutable fields.
	if req.DisplayName != "" {
		stored.DisplayName = req.DisplayName
	}
	if req.Type != btapb.Instance_TYPE_UNSPECIFIED {
		stored.Type = req.Type
	}
	if req.Labels != nil {
		stored.Labels = req.Labels
	}
	if req.State != btapb.Instance_STATE_NOT_KNOWN {
		if _, ok := btapb.Instance_State_name[int32(req.State)]; !ok {
			return nil, status.Errorf(codes.InvalidArgument, "invalid state %v", req.State)
		}
		stored.State = req.State
	}

	parent, instanceId := instanceParentAndId(name)
	if err := s.instanceBackend.Save(parent, instanceId, stored); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to persist instance: %v", err)
	}

	return stored, nil
}

// instanceNameFromParent extracts the project ID from a parent string of the form
// "projects/{project}" or "projects/{project}/instances/{instance}".
func instanceNameFromParent(parent string) string {
	if idx := strings.Index(parent, "/instances/"); idx >= 0 {
		return parent[:idx]
	}
	return parent
}

// instanceParentAndId splits a full instance name "projects/{p}/instances/{i}"
// into the parent "projects/{p}" and instance ID.
func instanceParentAndId(name string) (parent, instanceId string) {
	if idx := strings.LastIndex(name, "/instances/"); idx >= 0 {
		return name[:idx], name[idx+len("/instances/"):]
	}
	return name, name
}

func (s *server) DeleteInstance(ctx context.Context, req *btapb.DeleteInstanceRequest) (*empty.Empty, error) {
	name := req.GetName()
	if !regInstanceName.Match([]byte(name)) {
		return nil, status.Errorf(codes.InvalidArgument,
			"Error in field 'instance_name' : Invalid name for collection instances : Should match %s but found '%s'",
			instanceNameRegRaw, name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.instances[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", name)
	}

	// Delete all tables under this instance.
	prefix := name + "/tables/"
	for tblName, tbl := range s.tables {
		if strings.HasPrefix(tblName, prefix) {
			s.tableBackend.Delete(tbl)
			tbl.rows.DeleteAll()
			delete(s.tables, tblName)
		}
	}

	// Clean up materialized views whose source tables belong to this instance.
	for mvName := range s.materializedViews {
		if strings.HasPrefix(mvName, name+"/") {
			s.cmvs.deregister(viewIDFromName(mvName))
			s.mvBackend.Delete(mvName)
			delete(s.materializedViews, mvName)
		}
	}

	// Remove the instance from memory and persistence.
	delete(s.instances, name)
	parent, instanceId := instanceParentAndId(name)
	s.instanceBackend.Delete(parent, instanceId)

	return new(empty.Empty), nil
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

// IAM methods must be explicitly implemented to resolve ambiguity between
// UnimplementedBigtableTableAdminServer and UnimplementedBigtableInstanceAdminServer,
// which both embed these methods.
func (s *server) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return nil, status.Errorf(codes.Unimplemented, "method GetIamPolicy not implemented")
}

func (s *server) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return nil, status.Errorf(codes.Unimplemented, "method SetIamPolicy not implemented")
}

func (s *server) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method TestIamPermissions not implemented")
}

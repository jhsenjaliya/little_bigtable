package bttest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// SqlIAMPolicies persists IAM policies so they survive restarts. Policies are
// stored for tooling round trips; the emulator does not authenticate callers
// and therefore does not enforce them.
type SqlIAMPolicies struct {
	db *sql.DB
}

func NewSqlIAMPolicies(db *sql.DB) *SqlIAMPolicies {
	return &SqlIAMPolicies{db: db}
}

func (p *SqlIAMPolicies) get(ctx context.Context, resource string) (*iampb.Policy, bool, error) {
	var data []byte
	err := p.db.QueryRowContext(ctx, bind("SELECT policy FROM iam_policies_t WHERE resource = ?"), resource).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load IAM policy: %w", err)
	}
	policy := &iampb.Policy{}
	if err := proto.Unmarshal(data, policy); err != nil {
		return nil, false, fmt.Errorf("decode IAM policy: %w", err)
	}
	return policy, true, nil
}

func (p *SqlIAMPolicies) put(ctx context.Context, resource string, policy *iampb.Policy) error {
	data, err := proto.Marshal(policy)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(ctx,
		bind("INSERT INTO iam_policies_t (resource, policy) VALUES (?, ?) ON CONFLICT (resource) DO UPDATE SET policy = ?"),
		resource, data, data)
	return err
}

// deletePrefix removes the policies of a resource and its children.
func (p *SqlIAMPolicies) deletePrefix(ctx context.Context, q sqlExecutor, resource string) error {
	if q == nil {
		q = p.db
	}
	if _, err := q.ExecContext(ctx, bind("DELETE FROM iam_policies_t WHERE resource = ?"), resource); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, bind("DELETE FROM iam_policies_t WHERE resource LIKE ?"), likePrefix(resource+"/"))
	return err
}

// iamResourceExists reports whether an IAM-bearing resource exists. Bigtable
// IAM covers instances, tables, backups, authorized views, logical views,
// materialized views and schema bundles.
func (s *server) iamResourceExists(ctx context.Context, resource string) (bool, error) {
	switch {
	case strings.Contains(resource, "/authorizedViews/"):
		_, ok, err := s.avBackend.Get(resource)
		return ok, err
	case strings.Contains(resource, "/schemaBundles/"):
		_, ok, err := s.sbBackend.store.get(ctx, nil, resource)
		return ok, err
	case strings.Contains(resource, "/backups/"):
		_, ok, err := s.backupBackend.get(ctx, resource)
		return ok, err
	case strings.Contains(resource, "/logicalViews/"):
		_, ok, err := s.lvBackend.store.get(ctx, nil, resource)
		return ok, err
	case strings.Contains(resource, "/materializedViews/"):
		s.mu.Lock()
		defer s.mu.Unlock()
		_, ok := s.materializedViews[resource]
		return ok, nil
	case strings.Contains(resource, "/tables/"):
		s.mu.Lock()
		defer s.mu.Unlock()
		_, ok := s.tables[resource]
		return ok, nil
	case strings.Contains(resource, "/instances/"):
		s.mu.Lock()
		defer s.mu.Unlock()
		_, ok := s.instances[resource]
		return ok || !isStrictAdmin(), nil
	}
	return false, nil
}

func (s *server) requireIAMResource(ctx context.Context, resource string) error {
	if resource == "" {
		return status.Error(codes.InvalidArgument, "resource is required")
	}
	ok, err := s.iamResourceExists(ctx, resource)
	if err != nil {
		return internalErr(err)
	}
	if !ok {
		return status.Errorf(codes.NotFound, "resource %q not found", resource)
	}
	return nil
}

func (s *server) getIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	if err := s.requireIAMResource(ctx, req.GetResource()); err != nil {
		return nil, err
	}
	policy, ok, err := s.iamBackend.get(ctx, req.GetResource())
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok {
		policy = &iampb.Policy{Version: 1}
		policy.Etag = []byte(contentEtag(policy))
	}
	return policy, nil
}

func (s *server) setIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	if err := s.requireIAMResource(ctx, req.GetResource()); err != nil {
		return nil, err
	}
	if req.GetPolicy() == nil {
		return nil, status.Error(codes.InvalidArgument, "policy is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok, err := s.iamBackend.get(ctx, req.GetResource())
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok {
		cur = &iampb.Policy{Version: 1}
		cur.Etag = []byte(contentEtag(cur))
	}
	if len(req.Policy.GetEtag()) > 0 && string(req.Policy.GetEtag()) != string(cur.GetEtag()) {
		return nil, status.Error(codes.Aborted, "the policy etag does not match the current policy; read-modify-write the latest policy")
	}
	next := proto.Clone(req.Policy).(*iampb.Policy)
	if mask := req.GetUpdateMask(); mask != nil && len(mask.GetPaths()) > 0 {
		merged := proto.Clone(cur).(*iampb.Policy)
		for _, path := range mask.GetPaths() {
			switch path {
			case "bindings":
				merged.Bindings = next.Bindings
			case "etag":
			case "version":
				merged.Version = next.Version
			case "audit_configs":
				merged.AuditConfigs = next.AuditConfigs
			default:
				return nil, status.Errorf(codes.InvalidArgument, "unsupported update_mask path %q", path)
			}
		}
		next = merged
	}
	next.Etag = nil
	next.Etag = []byte(contentEtag(next))
	if err := s.iamBackend.put(ctx, req.GetResource(), next); err != nil {
		return nil, internalErr(err)
	}
	return next, nil
}

// testIamPermissions grants every requested permission because the emulator
// serves unauthenticated requests; it only checks that the resource exists.
func (s *server) testIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	if err := s.requireIAMResource(ctx, req.GetResource()); err != nil {
		return nil, err
	}
	return &iampb.TestIamPermissionsResponse{Permissions: req.GetPermissions()}, nil
}

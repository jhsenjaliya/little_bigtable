package bttest

import (
	"context"
	"database/sql"
	"strings"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// maxAuthorizedViewQualifierPrefixes is Bigtable's per-view limit on
// distinct qualifier prefixes.
const maxAuthorizedViewQualifierPrefixes = 10

// SqlAuthorizedViews persists authorized views to authorized_views_t.
type SqlAuthorizedViews struct {
	store protoStore[*btapb.AuthorizedView]
}

func NewSqlAuthorizedViews(db *sql.DB) *SqlAuthorizedViews {
	return &SqlAuthorizedViews{store: protoStore[*btapb.AuthorizedView]{
		db: db, table: "authorized_views_t", parentCol: "table_name",
		newT: func() *btapb.AuthorizedView { return &btapb.AuthorizedView{} },
	}}
}

func (s *SqlAuthorizedViews) Get(name string) (*btapb.AuthorizedView, bool, error) {
	return s.store.get(context.Background(), nil, name)
}

func (s *SqlAuthorizedViews) Save(name, tableName string, av *btapb.AuthorizedView) error {
	return s.store.put(context.Background(), nil, name, tableName, av)
}

func (s *SqlAuthorizedViews) Delete(name string) error {
	return s.store.remove(context.Background(), nil, name)
}

func (s *SqlAuthorizedViews) ListByTable(tableName string) ([]*btapb.AuthorizedView, error) {
	return s.store.listPrefix(context.Background(), nil, tableName+"/authorizedViews/")
}

func (s *SqlAuthorizedViews) DeleteByTable(tableName string) error {
	return s.store.removePrefix(context.Background(), nil, tableName+"/authorizedViews/")
}

var authorizedViewIDPattern = tableIDPattern

func validateSubsetView(av *btapb.AuthorizedView) error {
	sv := av.GetSubsetView()
	if sv == nil {
		return nil
	}
	prefixes := map[string]bool{}
	for fam, fs := range sv.GetFamilySubsets() {
		if !familyIDPattern.MatchString(fam) {
			return status.Errorf(codes.InvalidArgument, "invalid column family %q in subset_view", fam)
		}
		for _, p := range fs.GetQualifierPrefixes() {
			prefixes[fam+"\x00"+string(p)] = true
		}
	}
	if len(prefixes) > maxAuthorizedViewQualifierPrefixes {
		return status.Errorf(codes.InvalidArgument, "an authorized view may define at most %d distinct qualifier prefixes", maxAuthorizedViewQualifierPrefixes)
	}
	return nil
}

func authorizedViewResponse(av *btapb.AuthorizedView, view btapb.AuthorizedView_ResponseView) *btapb.AuthorizedView {
	switch view {
	case btapb.AuthorizedView_NAME_ONLY:
		return &btapb.AuthorizedView{Name: av.GetName()}
	case btapb.AuthorizedView_BASIC:
		return &btapb.AuthorizedView{Name: av.GetName(), DeletionProtection: av.GetDeletionProtection(), Etag: av.GetEtag()}
	default:
		return proto.Clone(av).(*btapb.AuthorizedView)
	}
}

func (s *server) CreateAuthorizedView(ctx context.Context, req *btapb.CreateAuthorizedViewRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	if req.GetParent() == "" || !authorizedViewIDPattern.MatchString(req.GetAuthorizedViewId()) {
		return nil, status.Errorf(codes.InvalidArgument, "a parent table and valid authorized_view_id are required")
	}
	if _, err := s.localRequireTable(req.Parent); err != nil {
		return nil, err
	}
	av := &btapb.AuthorizedView{}
	if req.AuthorizedView != nil {
		av = proto.Clone(req.AuthorizedView).(*btapb.AuthorizedView)
	}
	if err := validateSubsetView(av); err != nil {
		return nil, err
	}
	name := req.Parent + "/authorizedViews/" + req.AuthorizedViewId
	av.Name = name

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists, err := s.avBackend.Get(name); err != nil {
		return nil, internalErr(err)
	} else if exists {
		return nil, status.Errorf(codes.AlreadyExists, "authorized view %q already exists", name)
	}
	av.Etag = contentEtag(av)
	if err := s.avBackend.Save(name, req.Parent, av); err != nil {
		return nil, internalErr(err)
	}
	return s.ops.complete(ctx, req.Parent, &btapb.CreateAuthorizedViewMetadata{
		OriginalRequest: req, RequestTime: start, FinishTime: timestamppb.Now(),
	}, av)
}

func (s *server) GetAuthorizedView(ctx context.Context, req *btapb.GetAuthorizedViewRequest) (*btapb.AuthorizedView, error) {
	av, ok, err := s.avBackend.Get(req.GetName())
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "authorized view %q not found", req.GetName())
	}
	view := req.GetView()
	if view == btapb.AuthorizedView_RESPONSE_VIEW_UNSPECIFIED {
		view = btapb.AuthorizedView_BASIC
	}
	return authorizedViewResponse(av, view), nil
}

func (s *server) ListAuthorizedViews(ctx context.Context, req *btapb.ListAuthorizedViewsRequest) (*btapb.ListAuthorizedViewsResponse, error) {
	if _, err := s.localRequireTable(req.GetParent()); err != nil {
		return nil, err
	}
	views, err := s.avBackend.ListByTable(req.GetParent())
	if err != nil {
		return nil, internalErr(err)
	}
	view := req.GetView()
	if view == btapb.AuthorizedView_RESPONSE_VIEW_UNSPECIFIED {
		view = btapb.AuthorizedView_NAME_ONLY
	}
	page, next, err := paginate(views, (*btapb.AuthorizedView).GetName, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	out := make([]*btapb.AuthorizedView, len(page))
	for i, av := range page {
		out[i] = authorizedViewResponse(av, view)
	}
	return &btapb.ListAuthorizedViewsResponse{AuthorizedViews: out, NextPageToken: next}, nil
}

func (s *server) UpdateAuthorizedView(ctx context.Context, req *btapb.UpdateAuthorizedViewRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	av := req.GetAuthorizedView()
	if av == nil || av.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "authorized view name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok, err := s.avBackend.Get(av.Name)
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "authorized view %q not found", av.Name)
	}
	if err := checkEtag(av.GetEtag(), existing.GetEtag()); err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		// An empty mask overwrites only the fields set in the request.
		if av.GetSubsetView() != nil {
			paths = append(paths, "subset_view")
		}
		if av.GetDeletionProtection() {
			paths = append(paths, "deletion_protection")
		}
	}
	next := proto.Clone(existing).(*btapb.AuthorizedView)
	for _, path := range paths {
		switch path {
		case "deletion_protection":
			next.DeletionProtection = av.GetDeletionProtection()
		case "subset_view":
			next.AuthorizedView = proto.Clone(av).(*btapb.AuthorizedView).AuthorizedView
		case "*":
			// "*" overwrites every field, including those not set.
			next.DeletionProtection = av.GetDeletionProtection()
			next.AuthorizedView = proto.Clone(av).(*btapb.AuthorizedView).AuthorizedView
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update_mask path %q", path)
		}
	}
	if err := validateSubsetView(next); err != nil {
		return nil, err
	}
	next.Etag = contentEtag(next)
	if err := s.avBackend.Save(next.Name, authorizedViewParentTable(next.Name), next); err != nil {
		return nil, internalErr(err)
	}
	return s.ops.complete(ctx, next.Name, &btapb.UpdateAuthorizedViewMetadata{
		OriginalRequest: req, RequestTime: start, FinishTime: timestamppb.Now(),
	}, next)
}

func (s *server) DeleteAuthorizedView(ctx context.Context, req *btapb.DeleteAuthorizedViewRequest) (*empty.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	av, ok, err := s.avBackend.Get(req.GetName())
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "authorized view %q not found", req.GetName())
	}
	if err := checkEtag(req.GetEtag(), av.GetEtag()); err != nil {
		return nil, err
	}
	if av.DeletionProtection {
		return nil, status.Errorf(codes.FailedPrecondition, "authorized view %q has deletion protection enabled", req.GetName())
	}
	if err := s.avBackend.Delete(req.GetName()); err != nil {
		return nil, internalErr(err)
	}
	// The view's IAM policy is deleted with it; a re-created view starts empty.
	if err := s.iamBackend.deletePrefix(ctx, nil, req.GetName()); err != nil {
		return nil, internalErr(err)
	}
	return &empty.Empty{}, nil
}

func authorizedViewParentTable(name string) string {
	idx := strings.LastIndex(name, "/authorizedViews/")
	if idx < 0 {
		return name
	}
	return name[:idx]
}

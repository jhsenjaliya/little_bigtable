package bttest

import (
	"context"
	"database/sql"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SqlLogicalViews persists logical views to logical_views_t.
type SqlLogicalViews struct {
	store protoStore[*btapb.LogicalView]
}

func NewSqlLogicalViews(db *sql.DB) *SqlLogicalViews {
	return &SqlLogicalViews{store: protoStore[*btapb.LogicalView]{
		db: db, table: "logical_views_t",
		newT: func() *btapb.LogicalView { return &btapb.LogicalView{} },
	}}
}

func (s *server) CreateLogicalView(ctx context.Context, req *btapb.CreateLogicalViewRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	if req.GetParent() == "" || !tableIDPattern.MatchString(req.GetLogicalViewId()) {
		return nil, status.Errorf(codes.InvalidArgument, "parent and a valid logical_view_id are required")
	}
	if err := s.localRequireInstance(req.Parent); err != nil {
		return nil, err
	}
	lv := &btapb.LogicalView{}
	if req.LogicalView != nil {
		lv = proto.Clone(req.LogicalView).(*btapb.LogicalView)
	}
	if lv.GetQuery() == "" {
		return nil, status.Error(codes.InvalidArgument, "logical_view.query is required")
	}
	if err := s.validateViewQuery(req.Parent, lv.GetQuery()); err != nil {
		return nil, err
	}
	name := req.Parent + "/logicalViews/" + req.LogicalViewId
	lv.Name = name

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists, err := s.lvBackend.store.get(ctx, nil, name); err != nil {
		return nil, internalErr(err)
	} else if exists {
		return nil, status.Errorf(codes.AlreadyExists, "logical view %q already exists", name)
	}
	lv.Etag = contentEtag(lv)
	if err := s.lvBackend.store.put(ctx, nil, name, "", lv); err != nil {
		return nil, internalErr(err)
	}
	return s.ops.complete(ctx, name, &btapb.CreateLogicalViewMetadata{
		OriginalRequest: req, StartTime: start, EndTime: timestamppb.Now(),
	}, lv)
}

func (s *server) GetLogicalView(ctx context.Context, req *btapb.GetLogicalViewRequest) (*btapb.LogicalView, error) {
	lv, ok, err := s.lvBackend.store.get(ctx, nil, req.GetName())
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "logical view %q not found", req.GetName())
	}
	return lv, nil
}

func (s *server) ListLogicalViews(ctx context.Context, req *btapb.ListLogicalViewsRequest) (*btapb.ListLogicalViewsResponse, error) {
	if req.GetParent() == "" {
		return nil, status.Error(codes.InvalidArgument, "parent is required")
	}
	views, err := s.lvBackend.store.listPrefix(ctx, nil, req.GetParent()+"/logicalViews/")
	if err != nil {
		return nil, internalErr(err)
	}
	page, next, err := paginate(views, (*btapb.LogicalView).GetName, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &btapb.ListLogicalViewsResponse{LogicalViews: page, NextPageToken: next}, nil
}

func (s *server) UpdateLogicalView(ctx context.Context, req *btapb.UpdateLogicalViewRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	lv := req.GetLogicalView()
	if lv == nil || lv.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "logical view name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok, err := s.lvBackend.store.get(ctx, nil, lv.Name)
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "logical view %q not found", lv.Name)
	}
	if err := checkEtag(lv.GetEtag(), cur.GetEtag()); err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"query", "deletion_protection"}
	}
	next := proto.Clone(cur).(*btapb.LogicalView)
	for _, path := range paths {
		switch path {
		case "query":
			if lv.GetQuery() == "" {
				return nil, status.Error(codes.InvalidArgument, "logical_view.query must not be empty")
			}
			next.Query = lv.GetQuery()
		case "deletion_protection":
			next.DeletionProtection = lv.GetDeletionProtection()
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update_mask path %q", path)
		}
	}
	s.mu.Unlock()
	err = s.validateViewQuery(tableInstance(next.Name), next.Query)
	s.mu.Lock()
	if err != nil {
		return nil, err
	}
	next.Etag = contentEtag(next)
	if err := s.lvBackend.store.put(ctx, nil, next.Name, "", next); err != nil {
		return nil, internalErr(err)
	}
	return s.ops.complete(ctx, next.Name, &btapb.UpdateLogicalViewMetadata{
		OriginalRequest: req, StartTime: start, EndTime: timestamppb.Now(),
	}, next)
}

func (s *server) DeleteLogicalView(ctx context.Context, req *btapb.DeleteLogicalViewRequest) (*empty.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lv, ok, err := s.lvBackend.store.get(ctx, nil, req.GetName())
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "logical view %q not found", req.GetName())
	}
	if err := checkEtag(req.GetEtag(), lv.GetEtag()); err != nil {
		return nil, err
	}
	if lv.DeletionProtection {
		return nil, status.Errorf(codes.FailedPrecondition, "logical view %q has deletion protection enabled", req.GetName())
	}
	if err := s.lvBackend.store.remove(ctx, nil, req.GetName()); err != nil {
		return nil, internalErr(err)
	}
	if err := s.iamBackend.deletePrefix(ctx, nil, req.GetName()); err != nil {
		return nil, internalErr(err)
	}
	return &empty.Empty{}, nil
}

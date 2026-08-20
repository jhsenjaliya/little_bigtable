package bttest

import (
	"context"
	"database/sql"
	"log"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// SqlLogicalViews persists logical view metadata to SQL.
type SqlLogicalViews struct {
	db *sql.DB
}

func NewSqlLogicalViews(db *sql.DB) *SqlLogicalViews {
	return &SqlLogicalViews{db: db}
}

func (l *SqlLogicalViews) Save(name string, lv *btapb.LogicalView) {
	data, err := proto.Marshal(lv)
	if err != nil {
		log.Fatal(err)
	}
	_, err = l.db.Exec(
		bind("INSERT INTO logical_views_t (name, metadata) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET metadata = ?"),
		name, data, data,
	)
	if err != nil {
		log.Fatalf("saving logical view %q: %v", name, err)
	}
}

func (l *SqlLogicalViews) Delete(name string) {
	_, err := l.db.Exec(bind("DELETE FROM logical_views_t WHERE name = ?"), name)
	if err != nil {
		log.Fatal(err)
	}
}

func (l *SqlLogicalViews) Get(name string) (*btapb.LogicalView, bool) {
	var data []byte
	err := l.db.QueryRow(bind("SELECT metadata FROM logical_views_t WHERE name = ?"), name).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false
	}
	if err != nil {
		log.Fatal(err)
	}
	lv := &btapb.LogicalView{}
	if err := proto.Unmarshal(data, lv); err != nil {
		log.Fatal(err)
	}
	return lv, true
}

func (l *SqlLogicalViews) GetAll() []*btapb.LogicalView {
	rows, err := l.db.Query("SELECT metadata FROM logical_views_t")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	var result []*btapb.LogicalView
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			log.Fatal(err)
		}
		lv := &btapb.LogicalView{}
		if err := proto.Unmarshal(data, lv); err != nil {
			log.Fatal(err)
		}
		result = append(result, lv)
	}
	return result
}

// --- server methods ---

func (s *server) CreateLogicalView(ctx context.Context, req *btapb.CreateLogicalViewRequest) (*longrunning.Operation, error) {
	if req.Parent == "" || req.LogicalViewId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "parent and logical_view_id are required")
	}
	name := req.Parent + "/logicalViews/" + req.LogicalViewId

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lvBackend == nil {
		return nil, status.Errorf(codes.Unimplemented, "logical view storage not available")
	}

	if _, exists := s.lvBackend.Get(name); exists {
		return nil, status.Errorf(codes.AlreadyExists, "logical view %q already exists", name)
	}

	lv := &btapb.LogicalView{}
	if req.LogicalView != nil {
		lv = proto.Clone(req.LogicalView).(*btapb.LogicalView)
	}
	lv.Name = name

	s.lvBackend.Save(name, lv)
	return doneOperation(lv)
}

func (s *server) GetLogicalView(ctx context.Context, req *btapb.GetLogicalViewRequest) (*btapb.LogicalView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lvBackend == nil {
		return nil, status.Errorf(codes.NotFound, "logical view %q not found", req.Name)
	}
	lv, ok := s.lvBackend.Get(req.Name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "logical view %q not found", req.Name)
	}
	return lv, nil
}

func (s *server) ListLogicalViews(ctx context.Context, req *btapb.ListLogicalViewsRequest) (*btapb.ListLogicalViewsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var views []*btapb.LogicalView
	if s.lvBackend != nil {
		views = s.lvBackend.GetAll()
	}
	return &btapb.ListLogicalViewsResponse{LogicalViews: views}, nil
}

func (s *server) UpdateLogicalView(ctx context.Context, req *btapb.UpdateLogicalViewRequest) (*longrunning.Operation, error) {
	lv := req.GetLogicalView()
	if lv == nil || lv.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "logical view name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lvBackend == nil {
		return nil, status.Errorf(codes.NotFound, "logical view %q not found", lv.Name)
	}

	existing, ok := s.lvBackend.Get(lv.Name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "logical view %q not found", lv.Name)
	}

	if req.UpdateMask != nil {
		for _, path := range req.UpdateMask.GetPaths() {
			switch path {
			case "query":
				existing.Query = lv.Query
			case "deletion_protection":
				existing.DeletionProtection = lv.DeletionProtection
			}
		}
	} else {
		existing = proto.Clone(lv).(*btapb.LogicalView)
	}

	s.lvBackend.Save(existing.Name, existing)
	return doneOperation(existing)
}

func (s *server) DeleteLogicalView(ctx context.Context, req *btapb.DeleteLogicalViewRequest) (*empty.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lvBackend == nil {
		return nil, status.Errorf(codes.NotFound, "logical view %q not found", req.Name)
	}

	lv, ok := s.lvBackend.Get(req.Name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "logical view %q not found", req.Name)
	}
	if lv.DeletionProtection {
		return nil, status.Errorf(codes.FailedPrecondition, "logical view %q has deletion protection enabled", req.Name)
	}

	s.lvBackend.Delete(req.Name)
	return &empty.Empty{}, nil
}

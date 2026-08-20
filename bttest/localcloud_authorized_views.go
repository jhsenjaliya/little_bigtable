package bttest

import (
	"context"
	"database/sql"
	"log"
	"strings"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// SqlAuthorizedViews persists authorized view metadata to SQL.
type SqlAuthorizedViews struct {
	db *sql.DB
}

func NewSqlAuthorizedViews(db *sql.DB) *SqlAuthorizedViews {
	return &SqlAuthorizedViews{db: db}
}

func (s *SqlAuthorizedViews) Save(name, tableName string, av *btapb.AuthorizedView) {
	data, err := proto.Marshal(av)
	if err != nil {
		log.Fatal(err)
	}
	_, err = s.db.Exec(
		bind("INSERT INTO authorized_views_t (name, table_name, metadata) VALUES (?, ?, ?) ON CONFLICT (name) DO UPDATE SET table_name = ?, metadata = ?"),
		name, tableName, data, tableName, data,
	)
	if err != nil {
		log.Fatalf("saving authorized view %q: %v", name, err)
	}
}

func (s *SqlAuthorizedViews) Delete(name string) {
	_, err := s.db.Exec(bind("DELETE FROM authorized_views_t WHERE name = ?"), name)
	if err != nil {
		log.Fatal(err)
	}
}

func (s *SqlAuthorizedViews) Get(name string) (*btapb.AuthorizedView, bool) {
	var data []byte
	err := s.db.QueryRow(bind("SELECT metadata FROM authorized_views_t WHERE name = ?"), name).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false
	}
	if err != nil {
		log.Fatal(err)
	}
	av := &btapb.AuthorizedView{}
	if err := proto.Unmarshal(data, av); err != nil {
		log.Fatal(err)
	}
	return av, true
}

func (s *SqlAuthorizedViews) ListByTable(tableName string) []*btapb.AuthorizedView {
	rows, err := s.db.Query(bind("SELECT metadata FROM authorized_views_t WHERE table_name = ?"), tableName)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	var result []*btapb.AuthorizedView
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			log.Fatal(err)
		}
		av := &btapb.AuthorizedView{}
		if err := proto.Unmarshal(data, av); err != nil {
			log.Fatal(err)
		}
		result = append(result, av)
	}
	return result
}

func (s *SqlAuthorizedViews) DeleteByTable(tableName string) {
	_, err := s.db.Exec(bind("DELETE FROM authorized_views_t WHERE table_name = ?"), tableName)
	if err != nil {
		log.Fatal(err)
	}
}

// --- server methods ---

func (s *server) CreateAuthorizedView(ctx context.Context, req *btapb.CreateAuthorizedViewRequest) (*longrunning.Operation, error) {
	if req.Parent == "" || req.AuthorizedViewId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "parent and authorized_view_id are required")
	}
	name := req.Parent + "/authorizedViews/" + req.AuthorizedViewId

	s.mu.Lock()
	defer s.mu.Unlock()

	// Verify table exists.
	if _, ok := s.tables[req.Parent]; !ok {
		return nil, status.Errorf(codes.NotFound, "table %q not found", req.Parent)
	}

	av := &btapb.AuthorizedView{}
	if req.AuthorizedView != nil {
		av = proto.Clone(req.AuthorizedView).(*btapb.AuthorizedView)
	}
	av.Name = name

	if s.avBackend != nil {
		if _, exists := s.avBackend.Get(name); exists {
			return nil, status.Errorf(codes.AlreadyExists, "authorized view %q already exists", name)
		}
		s.avBackend.Save(name, req.Parent, av)
	}

	return doneOperation(av)
}

func (s *server) GetAuthorizedView(ctx context.Context, req *btapb.GetAuthorizedViewRequest) (*btapb.AuthorizedView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.avBackend == nil {
		return nil, status.Errorf(codes.NotFound, "authorized view %q not found", req.Name)
	}
	av, ok := s.avBackend.Get(req.Name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "authorized view %q not found", req.Name)
	}
	return av, nil
}

func (s *server) ListAuthorizedViews(ctx context.Context, req *btapb.ListAuthorizedViewsRequest) (*btapb.ListAuthorizedViewsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var views []*btapb.AuthorizedView
	if s.avBackend != nil {
		views = s.avBackend.ListByTable(req.Parent)
	}
	return &btapb.ListAuthorizedViewsResponse{AuthorizedViews: views}, nil
}

func (s *server) UpdateAuthorizedView(ctx context.Context, req *btapb.UpdateAuthorizedViewRequest) (*longrunning.Operation, error) {
	av := req.GetAuthorizedView()
	if av == nil || av.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "authorized view name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.avBackend == nil {
		return nil, status.Errorf(codes.NotFound, "authorized view %q not found", av.Name)
	}

	existing, ok := s.avBackend.Get(av.Name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "authorized view %q not found", av.Name)
	}

	// Apply updates.
	if req.UpdateMask != nil {
		for _, path := range req.UpdateMask.GetPaths() {
			switch path {
			case "deletion_protection":
				existing.DeletionProtection = av.DeletionProtection
			case "authorized_view":
				existing.AuthorizedView = av.AuthorizedView
			}
		}
	} else {
		existing = proto.Clone(av).(*btapb.AuthorizedView)
	}

	tableName := authorizedViewParentTable(existing.Name)
	s.avBackend.Save(existing.Name, tableName, existing)
	return doneOperation(existing)
}

func (s *server) DeleteAuthorizedView(ctx context.Context, req *btapb.DeleteAuthorizedViewRequest) (*empty.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.avBackend == nil {
		return nil, status.Errorf(codes.NotFound, "authorized view %q not found", req.Name)
	}

	av, ok := s.avBackend.Get(req.Name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "authorized view %q not found", req.Name)
	}
	if av.DeletionProtection {
		return nil, status.Errorf(codes.FailedPrecondition, "authorized view %q has deletion protection enabled", req.Name)
	}

	s.avBackend.Delete(req.Name)
	return &empty.Empty{}, nil
}

func authorizedViewParentTable(name string) string {
	idx := strings.LastIndex(name, "/authorizedViews/")
	if idx < 0 {
		return name
	}
	return name[:idx]
}

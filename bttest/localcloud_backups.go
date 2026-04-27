package bttest

import (
	"context"
	"database/sql"
	"log"
	"strings"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	emptypb "github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SqlBackups persists backup metadata to SQL.
type SqlBackups struct {
	db *sql.DB
}

func NewSqlBackups(db *sql.DB) *SqlBackups {
	return &SqlBackups{db: db}
}

func (b *SqlBackups) Save(name string, backup *btapb.Backup) {
	data, err := proto.Marshal(backup)
	if err != nil {
		log.Fatal(err)
	}
	_, err = b.db.Exec(
		bind("INSERT INTO backups_t (name, metadata) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET metadata = ?"),
		name, data, data,
	)
	if err != nil {
		log.Fatalf("saving backup %q: %v", name, err)
	}
}

func (b *SqlBackups) Delete(name string) {
	_, err := b.db.Exec(bind("DELETE FROM backups_t WHERE name = ?"), name)
	if err != nil {
		log.Fatal(err)
	}
}

func (b *SqlBackups) Get(name string) (*btapb.Backup, bool) {
	var data []byte
	err := b.db.QueryRow(bind("SELECT metadata FROM backups_t WHERE name = ?"), name).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false
	}
	if err != nil {
		log.Fatal(err)
	}
	backup := &btapb.Backup{}
	if err := proto.Unmarshal(data, backup); err != nil {
		log.Fatal(err)
	}
	return backup, true
}

func (b *SqlBackups) ListByCluster(clusterPrefix string) []*btapb.Backup {
	rows, err := b.db.Query("SELECT metadata FROM backups_t")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	var result []*btapb.Backup
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			log.Fatal(err)
		}
		backup := &btapb.Backup{}
		if err := proto.Unmarshal(data, backup); err != nil {
			log.Fatal(err)
		}
		if strings.HasPrefix(backup.Name, clusterPrefix) {
			result = append(result, backup)
		}
	}
	return result
}

// --- server methods ---

func (s *server) CreateBackup(ctx context.Context, req *btapb.CreateBackupRequest) (*longrunning.Operation, error) {
	if req.Parent == "" || req.BackupId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "parent and backup_id are required")
	}
	name := req.Parent + "/backups/" + req.BackupId

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.backupBackend == nil {
		return nil, status.Errorf(codes.Unimplemented, "backup storage not available")
	}

	if _, exists := s.backupBackend.Get(name); exists {
		return nil, status.Errorf(codes.AlreadyExists, "backup %q already exists", name)
	}

	// Verify source table exists.
	if req.Backup != nil && req.Backup.SourceTable != "" {
		if _, ok := s.tables[req.Backup.SourceTable]; !ok {
			return nil, status.Errorf(codes.NotFound, "source table %q not found", req.Backup.SourceTable)
		}
	}

	backup := &btapb.Backup{}
	if req.Backup != nil {
		backup = proto.Clone(req.Backup).(*btapb.Backup)
	}
	backup.Name = name
	backup.State = btapb.Backup_READY
	backup.StartTime = timestamppb.Now()
	backup.EndTime = timestamppb.Now()
	backup.SizeBytes = 0

	s.backupBackend.Save(name, backup)
	return doneOperation(backup)
}

func (s *server) GetBackup(ctx context.Context, req *btapb.GetBackupRequest) (*btapb.Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.backupBackend == nil {
		return nil, status.Errorf(codes.NotFound, "backup %q not found", req.Name)
	}
	backup, ok := s.backupBackend.Get(req.Name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "backup %q not found", req.Name)
	}
	return backup, nil
}

func (s *server) UpdateBackup(ctx context.Context, req *btapb.UpdateBackupRequest) (*btapb.Backup, error) {
	if req.Backup == nil || req.Backup.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "backup name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.backupBackend == nil {
		return nil, status.Errorf(codes.NotFound, "backup %q not found", req.Backup.Name)
	}

	existing, ok := s.backupBackend.Get(req.Backup.Name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "backup %q not found", req.Backup.Name)
	}

	if req.UpdateMask != nil {
		for _, path := range req.UpdateMask.GetPaths() {
			switch path {
			case "expire_time":
				existing.ExpireTime = req.Backup.ExpireTime
			}
		}
	}

	s.backupBackend.Save(existing.Name, existing)
	return proto.Clone(existing).(*btapb.Backup), nil
}

func (s *server) DeleteBackup(ctx context.Context, req *btapb.DeleteBackupRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.backupBackend == nil {
		return nil, status.Errorf(codes.NotFound, "backup %q not found", req.Name)
	}

	if _, ok := s.backupBackend.Get(req.Name); !ok {
		return nil, status.Errorf(codes.NotFound, "backup %q not found", req.Name)
	}

	s.backupBackend.Delete(req.Name)
	return &emptypb.Empty{}, nil
}

func (s *server) ListBackups(ctx context.Context, req *btapb.ListBackupsRequest) (*btapb.ListBackupsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var backups []*btapb.Backup
	if s.backupBackend != nil {
		backups = s.backupBackend.ListByCluster(req.Parent + "/backups/")
	}
	return &btapb.ListBackupsResponse{Backups: backups}, nil
}

func (s *server) RestoreTable(ctx context.Context, req *btapb.RestoreTableRequest) (*longrunning.Operation, error) {
	if req.Parent == "" || req.TableId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "parent and table_id are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	backupName := ""
	if b := req.GetBackup(); b != "" {
		backupName = b
	}
	if backupName == "" {
		return nil, status.Errorf(codes.InvalidArgument, "backup source is required")
	}

	if s.backupBackend == nil {
		return nil, status.Errorf(codes.NotFound, "backup %q not found", backupName)
	}

	backup, ok := s.backupBackend.Get(backupName)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "backup %q not found", backupName)
	}

	tableName := req.Parent + "/tables/" + req.TableId
	if _, exists := s.tables[tableName]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "table %q already exists", tableName)
	}

	// Create empty table with same schema as source (metadata-only restore).
	if backup.SourceTable != "" {
		if srcTbl, ok := s.tables[backup.SourceTable]; ok {
			newTbl := &table{
				parent:   req.Parent,
				tableId:  req.TableId,
				families: make(map[string]*columnFamily),
				rows:     NewSqlRows(s.db, req.Parent, req.TableId),
			}
			srcTbl.mu.RLock()
			for name, cf := range srcTbl.families {
				newTbl.families[name] = &columnFamily{
					Name:   cf.Name,
					Order:  cf.Order,
					GCRule: cf.GCRule,
				}
			}
			newTbl.counter = srcTbl.counter
			srcTbl.mu.RUnlock()
			s.tables[tableName] = newTbl
			s.tableBackend.Save(newTbl)
		}
	}

	// If source table not found, create empty table.
	if _, exists := s.tables[tableName]; !exists {
		newTbl := &table{
			parent:   req.Parent,
			tableId:  req.TableId,
			families: make(map[string]*columnFamily),
			rows:     NewSqlRows(s.db, req.Parent, req.TableId),
		}
		s.tables[tableName] = newTbl
		s.tableBackend.Save(newTbl)
	}

	result := &btapb.Table{Name: tableName}
	return doneOperation(result)
}

func (s *server) CopyBackup(ctx context.Context, req *btapb.CopyBackupRequest) (*longrunning.Operation, error) {
	if req.Parent == "" || req.BackupId == "" || req.SourceBackup == "" {
		return nil, status.Errorf(codes.InvalidArgument, "parent, backup_id, and source_backup are required")
	}
	name := req.Parent + "/backups/" + req.BackupId

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.backupBackend == nil {
		return nil, status.Errorf(codes.Unimplemented, "backup storage not available")
	}

	if _, exists := s.backupBackend.Get(name); exists {
		return nil, status.Errorf(codes.AlreadyExists, "backup %q already exists", name)
	}

	source, ok := s.backupBackend.Get(req.SourceBackup)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "source backup %q not found", req.SourceBackup)
	}

	backup := proto.Clone(source).(*btapb.Backup)
	backup.Name = name
	backup.SourceBackup = req.SourceBackup
	if req.ExpireTime != nil {
		backup.ExpireTime = req.ExpireTime
	}

	s.backupBackend.Save(name, backup)
	return doneOperation(backup)
}

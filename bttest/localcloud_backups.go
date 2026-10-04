package bttest

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	emptypb "github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	minBackupRetention        = 6 * time.Hour
	maxBackupRetention        = 90 * 24 * time.Hour
	maxBackupRetentionPlus    = 365 * 24 * time.Hour // Enterprise Plus edition
	maxCopyBackupRetention    = 30 * 24 * time.Hour  // measured from the copy's creation
	minCopySourceRemaining    = 24 * time.Hour       // a backup within 24h of expiring can't be copied
	minHotToStandardDelay     = 24 * time.Hour
	maxStandardBackupsPerTbl  = 150
	maxHotBackupsPerTbl       = 10
	backupSnapshotStorageID   = "__backup__"
	restoredTableOptimizeName = "optimize"
)

// SqlBackups stores backup descriptors in backups_t, immutable schema
// manifests in backup_manifests_t, and snapshot rows in rows_t under the
// backup's resource name.
type SqlBackups struct {
	db    *sql.DB
	store protoStore[*btapb.Backup]
}

func NewSqlBackups(db *sql.DB) *SqlBackups {
	return &SqlBackups{db: db, store: protoStore[*btapb.Backup]{
		db: db, table: "backups_t",
		newT: func() *btapb.Backup { return &btapb.Backup{} },
	}}
}

func (b *SqlBackups) get(ctx context.Context, name string) (*btapb.Backup, bool, error) {
	return b.store.get(ctx, nil, name)
}

func (b *SqlBackups) snapshotRows(name string) *SqlRows {
	return NewSqlRows(b.db, name, backupSnapshotStorageID)
}

func (b *SqlBackups) putManifest(ctx context.Context, q sqlExecutor, name string, data []byte) error {
	_, err := q.ExecContext(ctx,
		bind("INSERT INTO backup_manifests_t (name, metadata) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET metadata = ?"),
		name, data, data)
	return err
}

func (b *SqlBackups) manifest(ctx context.Context, q sqlExecutor, name string) ([]byte, error) {
	var data []byte
	err := q.QueryRowContext(ctx, bind("SELECT metadata FROM backup_manifests_t WHERE name = ?"), name).Scan(&data)
	if err != nil {
		return nil, fmt.Errorf("load backup manifest %s: %w", name, err)
	}
	return data, nil
}

// purge permanently removes a backup, its manifest and its snapshot rows.
func (b *SqlBackups) purge(ctx context.Context, q sqlExecutor, name string) error {
	if err := b.snapshotRows(name).clear(ctx, q); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, bind("DELETE FROM backup_manifests_t WHERE name = ?"), name); err != nil {
		return err
	}
	return b.store.remove(ctx, q, name)
}

func backupCluster(name string) string {
	if idx := strings.LastIndex(name, "/backups/"); idx >= 0 {
		return name[:idx]
	}
	return name
}

// requireClusterLocked checks a backup parent cluster. Clusters of instances
// that were never registered (non-strict mode) are accepted.
func (s *server) requireClusterLocked(cluster string) error {
	instance := parentInstanceFromChild(cluster, "/clusters/")
	if _, ok := s.instances[instance]; !ok {
		if isStrictAdmin() {
			return status.Errorf(codes.NotFound, "instance %q not found", instance)
		}
		return nil
	}
	if _, ok := s.clusters[cluster]; !ok {
		return status.Errorf(codes.NotFound, "cluster %q not found", cluster)
	}
	return nil
}

func (s *server) maxBackupRetentionLocked(instance string) time.Duration {
	if inst, ok := s.instances[instance]; ok && inst.GetEdition() == btapb.Instance_ENTERPRISE_PLUS {
		return maxBackupRetentionPlus
	}
	return maxBackupRetention
}

func validateBackupTimes(created time.Time, b *btapb.Backup, maxRetention time.Duration) error {
	if b.GetExpireTime() == nil {
		return status.Error(codes.InvalidArgument, "backup expire_time is required")
	}
	expire := b.GetExpireTime().AsTime()
	if expire.Before(created.Add(minBackupRetention)) || expire.After(created.Add(maxRetention)) {
		return status.Errorf(codes.InvalidArgument, "expire_time must be at least 6 hours and at most %v after the backup creation time", maxRetention)
	}
	switch b.GetBackupType() {
	case btapb.Backup_BACKUP_TYPE_UNSPECIFIED, btapb.Backup_STANDARD:
		if b.GetHotToStandardTime() != nil {
			return status.Error(codes.InvalidArgument, "hot_to_standard_time can only be set on HOT backups")
		}
	case btapb.Backup_HOT:
		if h := b.GetHotToStandardTime(); h != nil && h.AsTime().Before(created.Add(minHotToStandardDelay)) {
			return status.Error(codes.InvalidArgument, "hot_to_standard_time must be at least 24 hours after the backup creation time")
		}
	default:
		return status.Errorf(codes.InvalidArgument, "unknown backup_type %v", b.GetBackupType())
	}
	return nil
}

func (s *server) CreateBackup(ctx context.Context, req *btapb.CreateBackupRequest) (*longrunning.Operation, error) {
	start := time.Now()
	if req.GetParent() == "" || !tableIDPattern.MatchString(req.GetBackupId()) {
		return nil, status.Error(codes.InvalidArgument, "a parent cluster and valid backup_id are required")
	}
	if req.GetBackup().GetSourceTable() == "" {
		return nil, status.Error(codes.InvalidArgument, "backup.source_table is required")
	}
	name := req.Parent + "/backups/" + req.BackupId
	backup := proto.Clone(req.Backup).(*btapb.Backup)
	if backup.BackupType == btapb.Backup_BACKUP_TYPE_UNSPECIFIED {
		backup.BackupType = btapb.Backup_STANDARD
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireClusterLocked(req.Parent); err != nil {
		return nil, err
	}
	if c, ok := s.clusters[req.Parent]; ok && backup.BackupType == btapb.Backup_HOT && c.GetDefaultStorageType() == btapb.StorageType_HDD {
		return nil, status.Errorf(codes.InvalidArgument, "hot backups can't be created on HDD cluster %q", req.Parent)
	}
	if tableInstance(backup.SourceTable) != tableInstance(req.Parent) {
		return nil, status.Error(codes.InvalidArgument, "the source table must be in the backup's instance")
	}
	src, ok := s.tables[backup.SourceTable]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "source table %q not found", backup.SourceTable)
	}
	if err := validateBackupTimes(start, backup, s.maxBackupRetentionLocked(tableInstance(req.Parent))); err != nil {
		return nil, err
	}
	if _, exists, err := s.backupBackend.get(ctx, name); err != nil {
		return nil, internalErr(err)
	} else if exists {
		return nil, status.Errorf(codes.AlreadyExists, "backup %q already exists", name)
	}
	if err := s.checkBackupQuotaLocked(ctx, req.Parent, backup); err != nil {
		return nil, err
	}

	src.mu.RLock()
	defer src.mu.RUnlock()
	manifest, err := src.encodeMetadata()
	if err != nil {
		return nil, internalErr(err)
	}
	var size int64
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		snapshot := s.backupBackend.snapshotRows(name)
		if err := snapshot.clear(ctx, tx); err != nil {
			return err
		}
		if err := src.rows.copyTo(ctx, tx, snapshot); err != nil {
			return err
		}
		size = 0
		if err := snapshot.scan(ctx, tx, keyRange{}, false, func(r *row) (bool, error) {
			size += int64(r.size())
			return true, nil
		}); err != nil {
			return err
		}
		if err := s.backupBackend.putManifest(ctx, tx, name, manifest); err != nil {
			return err
		}
		backup.Name = name
		backup.SourceBackup = ""
		backup.State = btapb.Backup_READY
		backup.StartTime = timestamppb.New(start)
		backup.EndTime = timestamppb.Now()
		backup.SizeBytes = size
		backup.EncryptionInfo = googleDefaultEncryption()
		return s.backupBackend.store.put(ctx, tx, name, "", backup)
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create backup: %v", err)
	}
	return s.ops.complete(ctx, name, &btapb.CreateBackupMetadata{
		Name: name, SourceTable: backup.SourceTable, StartTime: backup.StartTime, EndTime: backup.EndTime,
	}, backup)
}

func (s *server) checkBackupQuotaLocked(ctx context.Context, cluster string, b *btapb.Backup) error {
	existing, err := s.backupBackend.store.listPrefix(ctx, nil, cluster+"/backups/")
	if err != nil {
		return internalErr(err)
	}
	standard, hot := 0, 0
	for _, e := range existing {
		if e.GetSourceTable() != b.GetSourceTable() {
			continue
		}
		if e.GetBackupType() == btapb.Backup_HOT {
			hot++
		} else {
			standard++
		}
	}
	if b.GetBackupType() == btapb.Backup_HOT && hot >= maxHotBackupsPerTbl {
		return status.Errorf(codes.ResourceExhausted, "table %q already has %d hot backups in this cluster", b.GetSourceTable(), maxHotBackupsPerTbl)
	}
	if b.GetBackupType() != btapb.Backup_HOT && standard >= maxStandardBackupsPerTbl {
		return status.Errorf(codes.ResourceExhausted, "table %q already has %d standard backups in this cluster", b.GetSourceTable(), maxStandardBackupsPerTbl)
	}
	return nil
}

// liveBackup returns a backup that has not expired.
func (s *server) liveBackup(ctx context.Context, name string) (*btapb.Backup, error) {
	b, ok, err := s.backupBackend.get(ctx, name)
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok || (b.GetExpireTime() != nil && time.Now().After(b.GetExpireTime().AsTime())) {
		return nil, status.Errorf(codes.NotFound, "backup %q not found", name)
	}
	return b, nil
}

func (s *server) GetBackup(ctx context.Context, req *btapb.GetBackupRequest) (*btapb.Backup, error) {
	return s.liveBackup(ctx, req.GetName())
}

func (s *server) UpdateBackup(ctx context.Context, req *btapb.UpdateBackupRequest) (*btapb.Backup, error) {
	if req.GetBackup().GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "backup.name is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_mask is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.liveBackup(ctx, req.Backup.Name)
	if err != nil {
		return nil, err
	}
	next := proto.Clone(cur).(*btapb.Backup)
	for _, path := range paths {
		switch path {
		case "expire_time":
			next.ExpireTime = req.Backup.GetExpireTime()
		case "hot_to_standard_time":
			next.HotToStandardTime = req.Backup.GetHotToStandardTime()
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update_mask path %q; only expire_time and hot_to_standard_time can be updated", path)
		}
	}
	maxRetention := s.maxBackupRetentionLocked(tableInstance(next.Name))
	if next.GetSourceBackup() != "" {
		maxRetention = maxCopyBackupRetention
	}
	if err := validateBackupTimes(cur.GetStartTime().AsTime(), next, maxRetention); err != nil {
		return nil, err
	}
	if err := s.backupBackend.store.put(ctx, nil, next.Name, "", next); err != nil {
		return nil, internalErr(err)
	}
	return next, nil
}

func (s *server) DeleteBackup(ctx context.Context, req *btapb.DeleteBackupRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok, err := s.backupBackend.get(ctx, req.GetName()); err != nil {
		return nil, internalErr(err)
	} else if !ok {
		return nil, status.Errorf(codes.NotFound, "backup %q not found", req.GetName())
	}
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := s.backupBackend.purge(ctx, tx, req.GetName()); err != nil {
			return err
		}
		return s.iamBackend.deletePrefix(ctx, tx, req.GetName())
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return &emptypb.Empty{}, nil
}

// ListBackupsRequest.filter: `field op value` terms (optionally parenthesized)
// joined by AND, with ops <, >, <=, >=, !=, = and : (HAS). Field names are
// case insensitive. OR and NOT are not supported locally (InvalidArgument).
var (
	backupFilterTerm = regexp.MustCompile(`(?i)^\s*\(?\s*(name|source_table|state|backup_type|source_backup|start_time|end_time|expire_time|size_bytes)\s*(<=|>=|!=|=|:|<|>)\s*(?:"([^"]*)"|([^\s"()]*))\s*\)?\s*$`)
	backupFilterAnd  = regexp.MustCompile(`(?i)\s+AND\s+`)
)

func backupMatches(b *btapb.Backup, filter string) (bool, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true, nil
	}
	for _, term := range backupFilterAnd.Split(filter, -1) {
		m := backupFilterTerm.FindStringSubmatch(term)
		if m == nil {
			return false, status.Errorf(codes.InvalidArgument, "unsupported backup filter term %q", term)
		}
		field, op, value := strings.ToLower(m[1]), m[2], m[3]+m[4]
		var c int
		switch field {
		case "start_time", "end_time", "expire_time":
			want, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return false, status.Errorf(codes.InvalidArgument, "backup filter %s value %q must be YYYY-MM-DDTHH:MM:SSZ", field, value)
			}
			got := map[string]*timestamppb.Timestamp{"start_time": b.GetStartTime(), "end_time": b.GetEndTime(), "expire_time": b.GetExpireTime()}[field]
			c = got.AsTime().Compare(want)
		case "size_bytes":
			want, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return false, status.Errorf(codes.InvalidArgument, "backup filter size_bytes value %q must be an integer", value)
			}
			c = cmp.Compare(b.GetSizeBytes(), want)
		default:
			var got string
			switch field {
			case "name":
				got = b.GetName()
			case "source_table":
				got = b.GetSourceTable()
			case "source_backup":
				got = b.GetSourceBackup()
			case "state":
				got, value = b.GetState().String(), strings.ToUpper(value)
			case "backup_type":
				got, value = b.GetBackupType().String(), strings.ToUpper(value)
			}
			if op == ":" {
				if !strings.Contains(got, value) {
					return false, nil
				}
				continue
			}
			c = strings.Compare(got, value)
		}
		var ok bool
		switch op {
		case "=", ":":
			ok = c == 0
		case "!=":
			ok = c != 0
		case "<":
			ok = c < 0
		case "<=":
			ok = c <= 0
		case ">":
			ok = c > 0
		case ">=":
			ok = c >= 0
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func (s *server) ListBackups(ctx context.Context, req *btapb.ListBackupsRequest) (*btapb.ListBackupsResponse, error) {
	parent := req.GetParent()
	if parent == "" {
		return nil, status.Error(codes.InvalidArgument, "parent is required")
	}
	prefix := parent + "/backups/"
	if strings.HasSuffix(parent, "/clusters/-") {
		prefix = strings.TrimSuffix(parent, "-")
	}
	all, err := s.backupBackend.store.listPrefix(ctx, nil, prefix)
	if err != nil {
		return nil, internalErr(err)
	}
	var backups []*btapb.Backup
	for _, b := range all {
		if !strings.Contains(strings.TrimPrefix(b.GetName(), prefix), "/backups/") && prefix != parent+"/backups/" {
			continue
		}
		if b.GetExpireTime() != nil && time.Now().After(b.GetExpireTime().AsTime()) {
			continue
		}
		ok, err := backupMatches(b, req.GetFilter())
		if err != nil {
			return nil, err
		}
		if ok {
			backups = append(backups, b)
		}
	}
	if err := orderBackups(backups, req.GetOrderBy()); err != nil {
		return nil, err
	}
	// page_size: "If 0 or less, defaults to the server's maximum allowed page size."
	page, next, err := paginateOrdered(backups, max(req.GetPageSize(), 0), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &btapb.ListBackupsResponse{Backups: page, NextPageToken: next}, nil
}

func orderBackups(backups []*btapb.Backup, orderBy string) error {
	orderBy = strings.TrimSpace(orderBy)
	if orderBy == "" {
		// Bigtable's default: most recently created backup first.
		orderBy = "start_time desc"
	}
	parts := strings.Fields(orderBy)
	desc := len(parts) == 2 && strings.EqualFold(parts[1], "desc")
	if len(parts) > 2 || len(parts) == 2 && !desc && !strings.EqualFold(parts[1], "asc") {
		return status.Errorf(codes.InvalidArgument, "unsupported order_by %q", orderBy)
	}
	var less func(a, b *btapb.Backup) bool
	switch parts[0] {
	case "name":
		less = func(a, b *btapb.Backup) bool { return a.Name < b.Name }
	case "source_table":
		less = func(a, b *btapb.Backup) bool { return a.SourceTable < b.SourceTable }
	case "expire_time":
		less = func(a, b *btapb.Backup) bool { return a.GetExpireTime().AsTime().Before(b.GetExpireTime().AsTime()) }
	case "start_time":
		less = func(a, b *btapb.Backup) bool { return a.GetStartTime().AsTime().Before(b.GetStartTime().AsTime()) }
	case "end_time":
		less = func(a, b *btapb.Backup) bool { return a.GetEndTime().AsTime().Before(b.GetEndTime().AsTime()) }
	case "size_bytes":
		less = func(a, b *btapb.Backup) bool { return a.SizeBytes < b.SizeBytes }
	case "state":
		less = func(a, b *btapb.Backup) bool { return a.State < b.State }
	default:
		return status.Errorf(codes.InvalidArgument, "unsupported order_by field %q", parts[0])
	}
	sort.SliceStable(backups, func(i, j int) bool {
		if desc {
			return less(backups[j], backups[i])
		}
		return less(backups[i], backups[j])
	})
	return nil
}

// paginateOrdered pages an already ordered slice with offset tokens.
func paginateOrdered[T any](items []T, pageSize int32, pageToken string) ([]T, string, error) {
	if pageSize < 0 {
		return nil, "", status.Error(codes.InvalidArgument, "page_size must not be negative")
	}
	start := 0
	if pageToken != "" {
		raw, err := base64.RawURLEncoding.DecodeString(pageToken)
		n, convErr := strconv.Atoi(strings.TrimPrefix(string(raw), "o:"))
		if err != nil || convErr != nil || n < 0 || !strings.HasPrefix(string(raw), "o:") {
			return nil, "", status.Errorf(codes.InvalidArgument, "invalid page_token %q", pageToken)
		}
		start = min(n, len(items))
	}
	items = items[start:]
	if pageSize == 0 || int(pageSize) >= len(items) {
		return items, "", nil
	}
	return items[:pageSize], base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(start+int(pageSize)))), nil
}

func (s *server) CopyBackup(ctx context.Context, req *btapb.CopyBackupRequest) (*longrunning.Operation, error) {
	start := time.Now()
	if req.GetParent() == "" || !tableIDPattern.MatchString(req.GetBackupId()) || req.GetSourceBackup() == "" {
		return nil, status.Error(codes.InvalidArgument, "parent, a valid backup_id and source_backup are required")
	}
	name := req.Parent + "/backups/" + req.BackupId
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireClusterLocked(req.Parent); err != nil {
		return nil, err
	}
	source, err := s.liveBackup(ctx, req.SourceBackup)
	if err != nil {
		return nil, err
	}
	if source.GetState() != btapb.Backup_READY {
		return nil, status.Errorf(codes.FailedPrecondition, "source backup %q is not READY", req.SourceBackup)
	}
	if source.GetSourceBackup() != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %q is itself a copy; copying a copied backup is not allowed", req.SourceBackup)
	}
	if source.GetExpireTime() != nil && source.GetExpireTime().AsTime().Before(start.Add(minCopySourceRemaining)) {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %q expires within 24 hours and cannot be copied", req.SourceBackup)
	}
	if _, exists, err := s.backupBackend.get(ctx, name); err != nil {
		return nil, internalErr(err)
	} else if exists {
		return nil, status.Errorf(codes.AlreadyExists, "backup %q already exists", name)
	}
	copied := &btapb.Backup{
		Name:         name,
		SourceTable:  source.SourceTable,
		SourceBackup: req.SourceBackup,
		ExpireTime:   req.GetExpireTime(),
		BackupType:   btapb.Backup_STANDARD,
	}
	if copied.ExpireTime == nil {
		return nil, status.Error(codes.InvalidArgument, "expire_time is required")
	}
	expire := copied.ExpireTime.AsTime()
	// CopyBackupRequest.expire_time: at least 6 hours and at most 30 days from
	// the time the request is received.
	if expire.Before(start.Add(minBackupRetention)) || expire.After(start.Add(maxCopyBackupRetention)) {
		return nil, status.Error(codes.InvalidArgument, "a copied backup's expire_time must be at least 6 hours and at most 30 days from now")
	}
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		manifest, err := s.backupBackend.manifest(ctx, tx, req.SourceBackup)
		if err != nil {
			return err
		}
		snapshot := s.backupBackend.snapshotRows(name)
		if err := snapshot.clear(ctx, tx); err != nil {
			return err
		}
		if err := s.backupBackend.snapshotRows(req.SourceBackup).copyTo(ctx, tx, snapshot); err != nil {
			return err
		}
		if err := s.backupBackend.putManifest(ctx, tx, name, manifest); err != nil {
			return err
		}
		copied.State = btapb.Backup_READY
		copied.StartTime = timestamppb.New(start)
		copied.EndTime = timestamppb.Now()
		copied.SizeBytes = source.SizeBytes
		copied.EncryptionInfo = googleDefaultEncryption()
		return s.backupBackend.store.put(ctx, tx, name, "", copied)
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "copy backup: %v", err)
	}
	return s.ops.complete(ctx, name, &btapb.CopyBackupMetadata{
		Name:             name,
		SourceBackupInfo: backupInfo(source),
		Progress:         completeProgress(start),
	}, copied)
}

func backupInfo(b *btapb.Backup) *btapb.BackupInfo {
	return &btapb.BackupInfo{
		Backup:       b.GetName(),
		StartTime:    b.GetStartTime(),
		EndTime:      b.GetEndTime(),
		SourceTable:  b.GetSourceTable(),
		SourceBackup: b.GetSourceBackup(),
	}
}

func completeProgress(start time.Time) *btapb.OperationProgress {
	return &btapb.OperationProgress{ProgressPercent: 100, StartTime: timestamppb.New(start), EndTime: timestamppb.Now()}
}

// RestoreTable creates a new table from a backup's immutable schema and rows.
// Like Bigtable, the restored table does not inherit garbage-collection
// policies, automated backup or deletion protection.
func (s *server) RestoreTable(ctx context.Context, req *btapb.RestoreTableRequest) (*longrunning.Operation, error) {
	start := time.Now()
	if req.GetParent() == "" || !tableIDPattern.MatchString(req.GetTableId()) {
		return nil, status.Error(codes.InvalidArgument, "parent and a valid table_id are required")
	}
	backupName := req.GetBackup()
	if backupName == "" {
		return nil, status.Error(codes.InvalidArgument, "backup is required")
	}
	if err := s.localRequireInstance(req.Parent); err != nil {
		return nil, err
	}
	tableName := req.Parent + "/tables/" + req.TableId

	s.mu.Lock()
	defer s.mu.Unlock()
	backup, err := s.liveBackup(ctx, backupName)
	if err != nil {
		return nil, err
	}
	if backup.GetState() != btapb.Backup_READY {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %q is not READY", backupName)
	}
	if _, exists := s.tables[tableName]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "table %q already exists", tableName)
	}
	restored := &table{parent: req.Parent, tableId: req.TableId, storageId: req.TableId, rows: NewSqlRows(s.db, req.Parent, req.TableId)}
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		manifest, err := s.backupBackend.manifest(ctx, tx, backupName)
		if err != nil {
			return err
		}
		if _, err := restored.decodeMetadata(manifest); err != nil {
			return err
		}
		restored.tableId, restored.storageId = req.TableId, req.TableId
		for id, cf := range restored.families {
			cf.Name = tableName + "/columnFamilies/" + id
			cf.GCRule = nil
		}
		restored.backupPolicy = nil
		restored.deletionProtection = false
		restored.changeStream = nil
		restored.deleteTime = time.Time{}
		restored.createTime = time.Now()
		restored.restoreInfo = &btapb.RestoreInfo{
			SourceType: btapb.RestoreSourceType_BACKUP,
			SourceInfo: &btapb.RestoreInfo_BackupInfo{BackupInfo: backupInfo(backup)},
		}
		if err := restored.rows.clear(ctx, tx); err != nil {
			return err
		}
		if err := s.backupBackend.snapshotRows(backupName).copyTo(ctx, tx, restored.rows); err != nil {
			return err
		}
		return s.tableBackend.Save(ctx, tx, restored)
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "restore table: %v", err)
	}
	s.tables[tableName] = restored

	optimize, err := s.ops.complete(ctx, tableName, &btapb.OptimizeRestoredTableMetadata{Name: tableName, Progress: completeProgress(start)}, nil)
	if err != nil {
		return nil, err
	}
	return s.ops.complete(ctx, tableName, &btapb.RestoreTableMetadata{
		Name:                       tableName,
		SourceType:                 btapb.RestoreSourceType_BACKUP,
		SourceInfo:                 &btapb.RestoreTableMetadata_BackupInfo{BackupInfo: backupInfo(backup)},
		OptimizeTableOperationName: optimize.GetName(),
		Progress:                   completeProgress(start),
	}, s.tableView(restored, btapb.Table_SCHEMA_VIEW))
}

func (s *server) purgeExpiredBackups(ctx context.Context) error {
	all, err := s.backupBackend.store.listPrefix(ctx, nil, "")
	if err != nil {
		return err
	}
	for _, b := range all {
		if b.GetExpireTime() == nil || time.Now().Before(b.GetExpireTime().AsTime()) {
			continue
		}
		if err := withTx(ctx, s.db, func(tx *sql.Tx) error {
			return s.backupBackend.purge(ctx, tx, b.GetName())
		}); err != nil {
			return err
		}
	}
	return nil
}

// backupsUnder lists live (unexpired) backups stored under a cluster or
// instance. Expired backups are deleted by Bigtable, so they never block
// cluster or instance deletion even before maintenance purges them.
func (s *server) backupsUnder(ctx context.Context, prefix string) ([]*btapb.Backup, error) {
	all, err := s.backupBackend.store.listPrefix(ctx, nil, prefix)
	if err != nil {
		return nil, err
	}
	live := all[:0]
	for _, b := range all {
		if b.GetExpireTime() == nil || time.Now().Before(b.GetExpireTime().AsTime()) {
			live = append(live, b)
		}
	}
	return live, nil
}

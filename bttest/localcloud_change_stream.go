package bttest

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	statpb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	minChangeStreamRetention = 24 * time.Hour
	maxChangeStreamRetention = 7 * 24 * time.Hour
	defaultHeartbeat         = 5 * time.Second
	changeStreamPollInterval = 50 * time.Millisecond
	changeStreamBatchSize    = 500
)

// SqlChangeLog is the transactional change-stream outbox. Each record is one
// atomic row commit, written in the same transaction as the row itself.
type SqlChangeLog struct {
	db *sql.DB
}

type changeRecord struct {
	id           int64
	rowKey       []byte
	changeType   btpb.ReadChangeStreamResponse_DataChange_Type
	commitMicros int64
	mutations    []*btpb.Mutation
}

func NewSqlChangeLog(db *sql.DB) *SqlChangeLog {
	return &SqlChangeLog{db: db}
}

func (c *SqlChangeLog) append(ctx context.Context, q sqlExecutor, tableName, rowKey string,
	changeType btpb.ReadChangeStreamResponse_DataChange_Type, commitMicros int64, muts []*btpb.Mutation) error {
	data, err := proto.Marshal(&btpb.MutateRowRequest{Mutations: muts})
	if err != nil {
		return fmt.Errorf("encode change record: %w", err)
	}
	_, err = q.ExecContext(ctx,
		bind("INSERT INTO change_stream_t (table_name, row_key, change_type, commit_micros, mutations) VALUES (?, ?, ?, ?, ?)"),
		tableName, []byte(rowKey), int64(changeType), commitMicros, data)
	if err != nil {
		return fmt.Errorf("append change record: %w", err)
	}
	return nil
}

func (c *SqlChangeLog) listAfter(ctx context.Context, tableName string, afterID int64, limit int) ([]changeRecord, error) {
	rows, err := c.db.QueryContext(ctx,
		bind("SELECT id, row_key, change_type, commit_micros, mutations FROM change_stream_t WHERE table_name = ? AND id > ? ORDER BY id ASC LIMIT ?"),
		tableName, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []changeRecord
	for rows.Next() {
		var rec changeRecord
		var changeType int64
		var data []byte
		if err := rows.Scan(&rec.id, &rec.rowKey, &changeType, &rec.commitMicros, &data); err != nil {
			return nil, err
		}
		rec.changeType = btpb.ReadChangeStreamResponse_DataChange_Type(changeType)
		wrapper := &btpb.MutateRowRequest{}
		if err := proto.Unmarshal(data, wrapper); err != nil {
			return nil, err
		}
		rec.mutations = wrapper.Mutations
		records = append(records, rec)
	}
	return records, rows.Err()
}

// lastIDBefore returns the largest record ID committed before commitMicros,
// so a stream starting at commitMicros resumes right after it.
func (c *SqlChangeLog) lastIDBefore(ctx context.Context, tableName string, commitMicros int64) (int64, error) {
	var id int64
	err := c.db.QueryRowContext(ctx,
		bind("SELECT COALESCE(MAX(id), 0) FROM change_stream_t WHERE table_name = ? AND commit_micros < ?"),
		tableName, commitMicros).Scan(&id)
	return id, err
}

func (c *SqlChangeLog) maxID(ctx context.Context, tableName string) (int64, error) {
	var id int64
	err := c.db.QueryRowContext(ctx,
		bind("SELECT COALESCE(MAX(id), 0) FROM change_stream_t WHERE table_name = ?"), tableName).Scan(&id)
	return id, err
}

func (c *SqlChangeLog) purgeBefore(ctx context.Context, q sqlExecutor, tableName string, commitMicros int64) error {
	if q == nil {
		q = c.db
	}
	_, err := q.ExecContext(ctx, bind("DELETE FROM change_stream_t WHERE table_name = ? AND commit_micros < ?"), tableName, commitMicros)
	return err
}

func (c *SqlChangeLog) purgeTable(ctx context.Context, q sqlExecutor, tableName string) error {
	if q == nil {
		q = c.db
	}
	_, err := q.ExecContext(ctx, bind("DELETE FROM change_stream_t WHERE table_name = ?"), tableName)
	return err
}

// validateChangeStreamConfig enforces Bigtable's 1-7 day retention range.
func validateChangeStreamConfig(cfg interface {
	GetRetentionPeriod() *durationpb.Duration
}) error {
	if cfg == nil || cfg.GetRetentionPeriod() == nil {
		return nil
	}
	d := cfg.GetRetentionPeriod().AsDuration()
	if d < minChangeStreamRetention || d > maxChangeStreamRetention {
		return status.Errorf(codes.InvalidArgument, "change_stream_config.retention_period must be between 1 day and 7 days, got %v", d)
	}
	return nil
}

func (s *server) purgeExpiredChangeRecords(ctx context.Context) error {
	s.mu.Lock()
	tables := make([]*table, 0, len(s.tables))
	for _, t := range s.tables {
		tables = append(tables, t)
	}
	s.mu.Unlock()
	for _, t := range tables {
		t.mu.RLock()
		retention := t.changeStreamRetention()
		name := t.name()
		t.mu.RUnlock()
		if retention == 0 {
			continue
		}
		if err := s.changeLog.purgeBefore(ctx, nil, name, time.Now().Add(-retention).UnixMicro()); err != nil {
			return err
		}
	}
	return nil
}

// changeStreamTable resolves the table of a change-stream request and checks
// that streaming is enabled and the app profile uses single-cluster routing.
func (s *server) changeStreamTable(tableName, appProfileID string) (*table, error) {
	tbl, err := s.localRequireTable(tableName)
	if err != nil {
		return nil, err
	}
	if profile, err := s.resolveAppProfile(tableInstance(tableName), appProfileID); err != nil {
		return nil, err
	} else if profile != nil && profile.GetSingleClusterRouting() == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "change streams require an app profile with single-cluster routing; %q is not single-cluster", profile.GetName())
	}
	tbl.mu.RLock()
	enabled := tbl.changeStreamEnabled()
	tbl.mu.RUnlock()
	if !enabled {
		return nil, status.Errorf(codes.FailedPrecondition, "change stream is not enabled for table %q", tableName)
	}
	return tbl, nil
}

func (s *server) GenerateInitialChangeStreamPartitions(req *btpb.GenerateInitialChangeStreamPartitionsRequest, stream btpb.Bigtable_GenerateInitialChangeStreamPartitionsServer) error {
	if _, err := s.changeStreamTable(req.GetTableName(), req.GetAppProfileId()); err != nil {
		return err
	}
	// The local store is one tablet, so a single partition covers the keyspace.
	return stream.Send(&btpb.GenerateInitialChangeStreamPartitionsResponse{Partition: fullStreamPartition()})
}

// changeStreamToken is the opaque continuation token: the ID of the last
// record delivered for the table.
func encodeChangeStreamToken(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cs1:" + strconv.FormatInt(id, 10)))
}

func decodeChangeStreamToken(token string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || !strings.HasPrefix(string(raw), "cs1:") {
		return 0, status.Errorf(codes.InvalidArgument, "invalid continuation token %q", token)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(string(raw), "cs1:"), 10, 64)
	if err != nil || id < 0 {
		return 0, status.Errorf(codes.InvalidArgument, "invalid continuation token %q", token)
	}
	return id, nil
}

func (s *server) ReadChangeStream(req *btpb.ReadChangeStreamRequest, stream btpb.Bigtable_ReadChangeStreamServer) error {
	ctx := stream.Context()
	tbl, err := s.changeStreamTable(req.GetTableName(), req.GetAppProfileId())
	if err != nil {
		return err
	}
	partition, nextAfterID, err := s.changeStreamStart(ctx, tbl, req)
	if err != nil {
		return err
	}
	rng := partitionRange(partition)

	heartbeat := defaultHeartbeat
	if d := req.GetHeartbeatDuration(); d != nil {
		if d.AsDuration() <= 0 {
			return status.Error(codes.InvalidArgument, "heartbeat_duration must be positive")
		}
		heartbeat = d.AsDuration()
	}
	var endTime time.Time
	if et := req.GetEndTime(); et != nil {
		// end_time is inclusive and truncated to microsecond granularity.
		endTime = et.AsTime().Truncate(time.Microsecond)
	}
	clusterID := s.sourceClusterID(tableInstance(req.GetTableName()))
	lastHeartbeat := time.Now()
	for {
		// Read the high-water mark before listing so the low watermark never
		// passes a record that has not been delivered.
		watermark := time.Now()
		records, err := s.changeLog.listAfter(ctx, req.GetTableName(), nextAfterID, changeStreamBatchSize)
		if err != nil {
			return status.Errorf(codes.Internal, "read change stream: %v", err)
		}
		for _, rec := range records {
			if !endTime.IsZero() && time.UnixMicro(rec.commitMicros).After(endTime) {
				return sendCloseStream(stream)
			}
			nextAfterID = rec.id
			if !rng.contains(string(rec.rowKey)) {
				continue
			}
			if err := stream.Send(changeRecordResponse(rec, clusterID, watermark)); err != nil {
				return err
			}
		}
		if len(records) == changeStreamBatchSize {
			continue
		}
		if !endTime.IsZero() && watermark.Truncate(time.Microsecond).After(endTime) {
			return sendCloseStream(stream)
		}
		if time.Since(lastHeartbeat) >= heartbeat {
			if err := stream.Send(heartbeatResponse(partition, nextAfterID, watermark)); err != nil {
				return err
			}
			lastHeartbeat = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.gcc:
			return status.Error(codes.Unavailable, "server is shutting down")
		case <-time.After(changeStreamPollInterval):
		}
	}
}

// changeStreamStart validates the requested position and returns the
// partition and the record ID after which delivery starts.
func (s *server) changeStreamStart(ctx context.Context, tbl *table, req *btpb.ReadChangeStreamRequest) (*btpb.StreamPartition, int64, error) {
	partition := req.GetPartition()
	if tokens := req.GetContinuationTokens().GetTokens(); len(tokens) > 0 {
		var afterID int64
		var tokenPartition *btpb.StreamPartition
		for i, token := range tokens {
			id, err := decodeChangeStreamToken(token.GetToken())
			if err != nil {
				return nil, 0, err
			}
			// Resume after the earliest token so no change is skipped.
			if i == 0 || id < afterID {
				afterID = id
			}
			tokenPartition = token.GetPartition()
		}
		if partition == nil {
			partition = tokenPartition
		} else if tokenPartition != nil && !proto.Equal(partition.GetRowRange(), tokenPartition.GetRowRange()) {
			return nil, 0, status.Error(codes.InvalidArgument, "continuation tokens must exactly match the requested partition")
		}
		if partition == nil {
			partition = fullStreamPartition()
		}
		return partition, afterID, nil
	}
	if partition == nil {
		partition = fullStreamPartition()
	}
	start := req.GetStartTime()
	if start == nil {
		// Start from now: only changes committed after the request.
		id, err := s.changeLog.maxID(ctx, req.GetTableName())
		if err != nil {
			return nil, 0, status.Errorf(codes.Internal, "read change stream: %v", err)
		}
		return partition, id, nil
	}
	startTime := start.AsTime()
	tbl.mu.RLock()
	retention := tbl.changeStreamRetention()
	tbl.mu.RUnlock()
	now := time.Now()
	if startTime.After(now) {
		return nil, 0, status.Error(codes.InvalidArgument, "start_time must not be in the future")
	}
	if startTime.Before(now.Add(-retention)) {
		return nil, 0, status.Errorf(codes.InvalidArgument, "start_time must be within the change stream retention period of %v", retention)
	}
	id, err := s.changeLog.lastIDBefore(ctx, req.GetTableName(), startTime.UnixMicro())
	if err != nil {
		return nil, 0, status.Errorf(codes.Internal, "read change stream: %v", err)
	}
	return partition, id, nil
}

func partitionRange(p *btpb.StreamPartition) keyRange {
	rr := p.GetRowRange()
	var rng keyRange
	switch sk := rr.GetStartKey().(type) {
	case *btpb.RowRange_StartKeyClosed:
		rng.start = string(sk.StartKeyClosed)
	case *btpb.RowRange_StartKeyOpen:
		rng.start = string(sk.StartKeyOpen) + "\x00"
	}
	switch ek := rr.GetEndKey().(type) {
	case *btpb.RowRange_EndKeyClosed:
		rng.end = string(ek.EndKeyClosed) + "\x00"
	case *btpb.RowRange_EndKeyOpen:
		rng.end = string(ek.EndKeyOpen)
	}
	return rng
}

func sendCloseStream(stream btpb.Bigtable_ReadChangeStreamServer) error {
	return stream.Send(&btpb.ReadChangeStreamResponse{
		StreamRecord: &btpb.ReadChangeStreamResponse_CloseStream_{
			CloseStream: &btpb.ReadChangeStreamResponse_CloseStream{Status: &statpb.Status{Code: int32(codes.OK)}},
		},
	})
}

func changeRecordResponse(rec changeRecord, clusterID string, watermark time.Time) *btpb.ReadChangeStreamResponse {
	chunks := make([]*btpb.ReadChangeStreamResponse_MutationChunk, 0, len(rec.mutations))
	for _, m := range rec.mutations {
		chunks = append(chunks, &btpb.ReadChangeStreamResponse_MutationChunk{Mutation: m})
	}
	dc := &btpb.ReadChangeStreamResponse_DataChange{
		Type:                  rec.changeType,
		RowKey:                rec.rowKey,
		CommitTimestamp:       timestamppb.New(time.UnixMicro(rec.commitMicros)),
		Chunks:                chunks,
		Done:                  true,
		Token:                 encodeChangeStreamToken(rec.id),
		EstimatedLowWatermark: timestamppb.New(watermark),
	}
	if rec.changeType == btpb.ReadChangeStreamResponse_DataChange_USER {
		dc.SourceClusterId = clusterID
	}
	return &btpb.ReadChangeStreamResponse{StreamRecord: &btpb.ReadChangeStreamResponse_DataChange_{DataChange: dc}}
}

func heartbeatResponse(partition *btpb.StreamPartition, afterID int64, watermark time.Time) *btpb.ReadChangeStreamResponse {
	return &btpb.ReadChangeStreamResponse{
		StreamRecord: &btpb.ReadChangeStreamResponse_Heartbeat_{
			Heartbeat: &btpb.ReadChangeStreamResponse_Heartbeat{
				ContinuationToken:     &btpb.StreamContinuationToken{Partition: partition, Token: encodeChangeStreamToken(afterID)},
				EstimatedLowWatermark: timestamppb.New(watermark),
			},
		},
	}
}

func fullStreamPartition() *btpb.StreamPartition {
	return &btpb.StreamPartition{RowRange: &btpb.RowRange{}}
}

// sourceClusterID is the cluster that applied local writes: the instance's
// first cluster, or "local-cluster" when no cluster is registered.
func (s *server) sourceClusterID(instance string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := ""
	for name := range s.clusters {
		if strings.HasPrefix(name, instance+"/clusters/") {
			id := strings.TrimPrefix(name, instance+"/clusters/")
			if best == "" || id < best {
				best = id
			}
		}
	}
	if best == "" {
		return "local-cluster"
	}
	return best
}

func (s *server) localRequireTable(tableName string) (*table, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tbl, ok := s.tables[tableName]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "table %q not found", tableName)
	}
	return tbl, nil
}

// tableInstance returns projects/p/instances/i for a table, view or backup name.
func tableInstance(name string) string {
	parts := strings.SplitN(name, "/", 5)
	if len(parts) >= 4 {
		return strings.Join(parts[:4], "/")
	}
	return name
}

func (s *server) PingAndWarm(ctx context.Context, req *btpb.PingAndWarmRequest) (*btpb.PingAndWarmResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "instance name is required")
	}
	if !regInstanceName.MatchString(req.GetName()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid instance name %q", req.GetName())
	}
	if err := s.localRequireInstance(req.GetName()); err != nil {
		return nil, err
	}
	if _, err := s.resolveAppProfile(req.GetName(), req.GetAppProfileId()); err != nil {
		return nil, err
	}
	return &btpb.PingAndWarmResponse{}, nil
}

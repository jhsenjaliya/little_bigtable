package bttest

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	statpb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type SqlChangeLog struct {
	db *sql.DB
}

type changeRecord struct {
	id           int64
	tableName    string
	rowKey       []byte
	mutation     *btpb.Mutation
	mutationType string
	commitMicros int64
}

func NewSqlChangeLog(db *sql.DB) *SqlChangeLog {
	return &SqlChangeLog{db: db}
}

func (c *SqlChangeLog) Append(tableName string, rowKey []byte, mutation *btpb.Mutation, mutationType string) error {
	data, err := proto.Marshal(mutation)
	if err != nil {
		return err
	}
	_, err = c.db.Exec(
		bind("INSERT INTO change_log_t (table_name, row_key, mutation_bytes, mutation_type, commit_micros) VALUES (?, ?, ?, ?, ?)"),
		tableName, rowKey, data, mutationType, time.Now().UnixMicro(),
	)
	return err
}

func (c *SqlChangeLog) ListAfter(tableName string, afterID int64, limit int) ([]changeRecord, error) {
	rows, err := c.db.Query(
		bind("SELECT id, table_name, row_key, mutation_bytes, mutation_type, commit_micros FROM change_log_t WHERE table_name = ? AND id > ? ORDER BY id ASC LIMIT ?"),
		tableName, afterID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []changeRecord
	for rows.Next() {
		var rec changeRecord
		var data []byte
		if err := rows.Scan(&rec.id, &rec.tableName, &rec.rowKey, &data, &rec.mutationType, &rec.commitMicros); err != nil {
			return nil, err
		}
		rec.mutation = &btpb.Mutation{}
		if err := proto.Unmarshal(data, rec.mutation); err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func (c *SqlChangeLog) FirstIDAtOrAfter(tableName string, commitMicros int64) (int64, error) {
	var id int64
	err := c.db.QueryRow(
		bind("SELECT COALESCE(MIN(id), 0) FROM change_log_t WHERE table_name = ? AND commit_micros >= ?"),
		tableName, commitMicros,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, nil
	}
	return id - 1, nil
}

func (s *server) localAppendChange(tableName string, rowKey []byte, muts []*btpb.Mutation) {
	if s.changeLog == nil {
		return
	}
	for _, mut := range muts {
		mutationType := "unknown"
		switch mut.Mutation.(type) {
		case *btpb.Mutation_SetCell_:
			mutationType = "set_cell"
		case *btpb.Mutation_DeleteFromColumn_:
			mutationType = "delete_from_column"
		case *btpb.Mutation_DeleteFromFamily_:
			mutationType = "delete_from_family"
		case *btpb.Mutation_DeleteFromRow_:
			mutationType = "delete_from_row"
		}
		if err := s.changeLog.Append(tableName, rowKey, mut, mutationType); err != nil {
			log.Printf("failed to append Bigtable change stream record: %v", err)
		}
	}
}

func (s *server) localAppendReadModifyWrite(tableName string, rowKey []byte, resultRow *row) {
	for _, fam := range resultRow.sortedFamilies() {
		for _, colName := range fam.ColNames {
			for _, cell := range fam.Cells[colName] {
				s.localAppendChange(tableName, rowKey, []*btpb.Mutation{{
					Mutation: &btpb.Mutation_SetCell_{
						SetCell: &btpb.Mutation_SetCell{
							FamilyName:      fam.Name,
							ColumnQualifier: []byte(colName),
							TimestampMicros: cell.Ts,
							Value:           cell.Value,
						},
					},
				}})
			}
		}
	}
}

func (s *server) GenerateInitialChangeStreamPartitions(req *btpb.GenerateInitialChangeStreamPartitionsRequest, stream btpb.Bigtable_GenerateInitialChangeStreamPartitionsServer) error {
	if _, err := s.localRequireTable(req.GetTableName()); err != nil {
		return err
	}
	return stream.Send(&btpb.GenerateInitialChangeStreamPartitionsResponse{
		Partition: fullStreamPartition(),
	})
}

func (s *server) ReadChangeStream(req *btpb.ReadChangeStreamRequest, stream btpb.Bigtable_ReadChangeStreamServer) error {
	if _, err := s.localRequireTable(req.GetTableName()); err != nil {
		return err
	}

	nextAfterID, err := s.localChangeStreamStartID(req)
	if err != nil {
		return err
	}

	heartbeat := req.GetHeartbeatDuration()
	if heartbeat == nil {
		heartbeat = durationpb.New(5 * time.Second)
	}
	ticker := time.NewTicker(heartbeat.AsDuration())
	defer ticker.Stop()

	for {
		records, err := s.changeLog.ListAfter(req.GetTableName(), nextAfterID, 1000)
		if err != nil {
			return status.Errorf(codes.Internal, "read change stream: %v", err)
		}
		for _, rec := range records {
			nextAfterID = rec.id
			if err := stream.Send(s.changeRecordResponse(rec)); err != nil {
				return err
			}
		}

		if end := req.GetEndTime(); end != nil && time.Now().After(end.AsTime()) {
			return stream.Send(&btpb.ReadChangeStreamResponse{
				StreamRecord: &btpb.ReadChangeStreamResponse_CloseStream_{
					CloseStream: &btpb.ReadChangeStreamResponse_CloseStream{
						Status: &statpb.Status{Code: int32(codes.OK)},
					},
				},
			})
		}

		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ticker.C:
			if err := stream.Send(heartbeatResponse(nextAfterID)); err != nil {
				return err
			}
		default:
			if len(records) == 0 {
				select {
				case <-stream.Context().Done():
					return stream.Context().Err()
				case <-ticker.C:
					if err := stream.Send(heartbeatResponse(nextAfterID)); err != nil {
						return err
					}
				}
			}
		}
	}
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

func (s *server) localChangeStreamStartID(req *btpb.ReadChangeStreamRequest) (int64, error) {
	var afterID int64
	for _, token := range req.GetContinuationTokens().GetTokens() {
		if token.GetToken() == "" {
			continue
		}
		id, err := strconv.ParseInt(token.GetToken(), 10, 64)
		if err != nil {
			return 0, status.Errorf(codes.InvalidArgument, "invalid continuation token %q", token.GetToken())
		}
		if id > afterID {
			afterID = id
		}
	}
	if afterID > 0 {
		return afterID, nil
	}
	if start := req.GetStartTime(); start != nil {
		return s.changeLog.FirstIDAtOrAfter(req.GetTableName(), start.AsTime().UnixMicro())
	}
	return 0, nil
}

func (s *server) changeRecordResponse(rec changeRecord) *btpb.ReadChangeStreamResponse {
	return &btpb.ReadChangeStreamResponse{
		StreamRecord: &btpb.ReadChangeStreamResponse_DataChange_{
			DataChange: &btpb.ReadChangeStreamResponse_DataChange{
				Type:            btpb.ReadChangeStreamResponse_DataChange_USER,
				SourceClusterId: "local-cluster",
				RowKey:          rec.rowKey,
				CommitTimestamp: timestamppb.New(time.UnixMicro(rec.commitMicros)),
				Chunks: []*btpb.ReadChangeStreamResponse_MutationChunk{{
					Mutation: rec.mutation,
				}},
				Done:                  true,
				Token:                 strconv.FormatInt(rec.id, 10),
				EstimatedLowWatermark: timestamppb.Now(),
			},
		},
	}
}

func heartbeatResponse(afterID int64) *btpb.ReadChangeStreamResponse {
	return &btpb.ReadChangeStreamResponse{
		StreamRecord: &btpb.ReadChangeStreamResponse_Heartbeat_{
			Heartbeat: &btpb.ReadChangeStreamResponse_Heartbeat{
				ContinuationToken: &btpb.StreamContinuationToken{
					Partition: fullStreamPartition(),
					Token:     strconv.FormatInt(afterID, 10),
				},
				EstimatedLowWatermark: timestamppb.Now(),
			},
		},
	}
}

func fullStreamPartition() *btpb.StreamPartition {
	return &btpb.StreamPartition{RowRange: &btpb.RowRange{}}
}

func (s *server) PingAndWarm(ctx context.Context, req *btpb.PingAndWarmRequest) (*btpb.PingAndWarmResponse, error) {
	return &btpb.PingAndWarmResponse{}, nil
}

func (s *server) PrepareQuery(ctx context.Context, req *btpb.PrepareQueryRequest) (*btpb.PrepareQueryResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "GoogleSQL for Bigtable is not supported by the LocalCloud emulator")
}

func (s *server) ExecuteQuery(req *btpb.ExecuteQueryRequest, stream btpb.Bigtable_ExecuteQueryServer) error {
	return status.Errorf(codes.Unimplemented, "GoogleSQL for Bigtable is not supported by the LocalCloud emulator")
}

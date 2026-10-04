package bttest

// Conformance tests for Bigtable change streams (ReadChangeStream and
// GenerateInitialChangeStreamPartitions), checked against the v2 data API
// proto contract and the change streams overview documentation.

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const ccsParent = "projects/p/instances/i"

// ccsStream captures ReadChangeStream responses.
type ccsStream struct {
	grpc.ServerStream
	ctx  context.Context
	mu   sync.Mutex
	msgs []*btpb.ReadChangeStreamResponse
}

func (s *ccsStream) Context() context.Context { return s.ctx }

func (s *ccsStream) Send(r *btpb.ReadChangeStreamResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, proto.Clone(r).(*btpb.ReadChangeStreamResponse))
	return nil
}

// ccsPartitionStream captures GenerateInitialChangeStreamPartitions responses.
type ccsPartitionStream struct {
	grpc.ServerStream
	msgs []*btpb.GenerateInitialChangeStreamPartitionsResponse
}

func (s *ccsPartitionStream) Context() context.Context { return context.Background() }

func (s *ccsPartitionStream) Send(r *btpb.GenerateInitialChangeStreamPartitionsResponse) error {
	s.msgs = append(s.msgs, r)
	return nil
}

// ccsResult is a fully drained, OK-closed change stream.
type ccsResult struct {
	changes    []*btpb.ReadChangeStreamResponse_DataChange
	heartbeats []*btpb.ReadChangeStreamResponse_Heartbeat
	close      *btpb.ReadChangeStreamResponse_CloseStream
}

// ccsRead runs ReadChangeStream to completion with a safety timeout.
func ccsRead(s *server, req *btpb.ReadChangeStreamRequest) ([]*btpb.ReadChangeStreamResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := &ccsStream{ctx: ctx}
	err := s.ReadChangeStream(req, st)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.msgs, err
}

// ccsReadAll reads until end_time (now when unset) and requires the stream
// to end with exactly one CloseStream carrying an OK status.
func ccsReadAll(t *testing.T, s *server, req *btpb.ReadChangeStreamRequest) ccsResult {
	t.Helper()
	if req.EndTime == nil {
		req.EndTime = timestamppb.Now()
	}
	msgs, err := ccsRead(s, req)
	require.NoError(t, err)
	var res ccsResult
	for i, m := range msgs {
		switch r := m.StreamRecord.(type) {
		case *btpb.ReadChangeStreamResponse_DataChange_:
			res.changes = append(res.changes, r.DataChange)
		case *btpb.ReadChangeStreamResponse_Heartbeat_:
			res.heartbeats = append(res.heartbeats, r.Heartbeat)
		case *btpb.ReadChangeStreamResponse_CloseStream_:
			require.Equal(t, len(msgs)-1, i, "CloseStream must be the last message")
			res.close = r.CloseStream
		default:
			t.Fatalf("unexpected stream record %T", r)
		}
	}
	require.NotNil(t, res.close, "stream with end_time must end with CloseStream")
	require.Equal(t, int32(codes.OK), res.close.GetStatus().GetCode())
	return res
}

func ccsFrom(ts *timestamppb.Timestamp) *btpb.ReadChangeStreamRequest_StartTime {
	return &btpb.ReadChangeStreamRequest_StartTime{StartTime: ts}
}

func ccsRetention(d time.Duration) *btapb.ChangeStreamConfig {
	return &btapb.ChangeStreamConfig{RetentionPeriod: durationpb.New(d)}
}

func ccsCreateTable(t *testing.T, s *server, parent, id string, enabled bool, fams map[string]*btapb.ColumnFamily) string {
	t.Helper()
	tbl := &btapb.Table{ColumnFamilies: fams}
	if enabled {
		tbl.ChangeStreamConfig = ccsRetention(24 * time.Hour)
	}
	_, err := s.CreateTable(context.Background(), &btapb.CreateTableRequest{Parent: parent, TableId: id, Table: tbl})
	require.NoError(t, err)
	return parent + "/tables/" + id
}

func ccsFamilies(names ...string) map[string]*btapb.ColumnFamily {
	fams := map[string]*btapb.ColumnFamily{}
	for _, n := range names {
		fams[n] = &btapb.ColumnFamily{}
	}
	return fams
}

func ccsSetCell(fam, col string, ts int64, val string) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
		FamilyName: fam, ColumnQualifier: []byte(col), TimestampMicros: ts, Value: []byte(val),
	}}}
}

func ccsMutate(t *testing.T, s *server, tableName, key string, muts ...*btpb.Mutation) {
	t.Helper()
	_, err := s.MutateRow(context.Background(), &btpb.MutateRowRequest{TableName: tableName, RowKey: []byte(key), Mutations: muts})
	require.NoError(t, err)
}

func ccsSetChangeStream(t *testing.T, s *server, tableName string, cfg *btapb.ChangeStreamConfig) error {
	t.Helper()
	_, err := s.UpdateTable(context.Background(), &btapb.UpdateTableRequest{
		Table:      &btapb.Table{Name: tableName, ChangeStreamConfig: cfg},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"change_stream_config"}},
	})
	return err
}

// ccsStoredRecords counts the change records persisted for a table.
func ccsStoredRecords(t *testing.T, s *server, tableName string) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(bind("SELECT COUNT(*) FROM change_stream_t WHERE table_name = ?"), tableName).Scan(&n))
	return n
}

func ccsRowKeys(changes []*btpb.ReadChangeStreamResponse_DataChange) []string {
	keys := make([]string, 0, len(changes))
	for _, dc := range changes {
		keys = append(keys, string(dc.GetRowKey()))
	}
	return keys
}

func ccsDeletedFamilies(t *testing.T, dc *btpb.ReadChangeStreamResponse_DataChange) []string {
	t.Helper()
	var fams []string
	for _, ch := range dc.GetChunks() {
		del := ch.GetMutation().GetDeleteFromFamily()
		require.NotNil(t, del, "expected DeleteFromFamily, got %v", ch.GetMutation())
		fams = append(fams, del.GetFamilyName())
	}
	sort.Strings(fams)
	return fams
}

func ccsCode(err error) codes.Code { return status.Code(err) }

func TestConformanceChangeStreamDisabledTableAndEnableViaUpdateTable(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	// Missing table.
	_, err := ccsRead(s, &btpb.ReadChangeStreamRequest{TableName: ccsParent + "/tables/missing", EndTime: timestamppb.Now()})
	require.Equal(t, codes.NotFound, ccsCode(err), "%v", err)

	name := ccsCreateTable(t, s, ccsParent, "plain", false, ccsFamilies("cf"))
	ccsMutate(t, s, name, "r1", ccsSetCell("cf", "c", 1000, "v"))
	assert.Equal(t, 0, ccsStoredRecords(t, s, name), "a table without change_stream_config must not record changes")

	_, err = ccsRead(s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(timestamppb.Now()), EndTime: timestamppb.Now()})
	require.Equal(t, codes.FailedPrecondition, ccsCode(err), "%v", err)
	ps := &ccsPartitionStream{}
	err = s.GenerateInitialChangeStreamPartitions(&btpb.GenerateInitialChangeStreamPartitionsRequest{TableName: name}, ps)
	require.Equal(t, codes.FailedPrecondition, ccsCode(err), "%v", err)

	// Retention must be 1-7 days.
	for _, d := range []time.Duration{12 * time.Hour, 8 * 24 * time.Hour} {
		require.Equal(t, codes.InvalidArgument, ccsCode(ccsSetChangeStream(t, s, name, ccsRetention(d))), "retention %v", d)
		_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: ccsParent, TableId: "badretention",
			Table: &btapb.Table{ChangeStreamConfig: ccsRetention(d)}})
		require.Equal(t, codes.InvalidArgument, ccsCode(err), "CreateTable retention %v", d)
	}

	// Enable through UpdateTable(change_stream_config).
	require.NoError(t, ccsSetChangeStream(t, s, name, ccsRetention(3*24*time.Hour)))
	got, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)
	require.Equal(t, 3*24*time.Hour, got.GetChangeStreamConfig().GetRetentionPeriod().AsDuration())

	// The retention_period sub-path also updates the retention.
	_, err = s.UpdateTable(ctx, &btapb.UpdateTableRequest{
		Table:      &btapb.Table{Name: name, ChangeStreamConfig: ccsRetention(7 * 24 * time.Hour)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"change_stream_config.retention_period"}},
	})
	require.NoError(t, err)
	got, err = s.GetTable(ctx, &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)
	require.Equal(t, 7*24*time.Hour, got.GetChangeStreamConfig().GetRetentionPeriod().AsDuration())

	// Writes before enabling are not part of the stream; later writes are.
	start := timestamppb.Now()
	ccsMutate(t, s, name, "r2", ccsSetCell("cf", "c", 2000, "v2"))
	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)})
	require.Equal(t, []string{"r2"}, ccsRowKeys(res.changes))
}

func TestConformanceChangeStreamMutateRowIsOneDataChange(t *testing.T) {
	s := newTestServer(t)
	name := ccsCreateTable(t, s, ccsParent, "t", true, ccsFamilies("cf1", "cf2"))

	before := time.Now()
	start := timestamppb.New(before)
	muts := []*btpb.Mutation{
		ccsSetCell("cf1", "a", 1000, "va"),
		ccsSetCell("cf2", "b", -1, "vb"), // server-assigned timestamp
		{Mutation: &btpb.Mutation_DeleteFromColumn_{DeleteFromColumn: &btpb.Mutation_DeleteFromColumn{
			FamilyName: "cf1", ColumnQualifier: []byte("old"), TimeRange: &btpb.TimestampRange{StartTimestampMicros: 0, EndTimestampMicros: 5000},
		}}},
		{Mutation: &btpb.Mutation_DeleteFromFamily_{DeleteFromFamily: &btpb.Mutation_DeleteFromFamily{FamilyName: "cf2"}}},
		ccsSetCell("cf2", "c", 3000, "vc"),
	}
	ccsMutate(t, s, name, "row", muts...)
	after := time.Now()

	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)})
	require.Len(t, res.changes, 1, "one MutateRow must produce exactly one DataChange")
	dc := res.changes[0]
	assert.Equal(t, btpb.ReadChangeStreamResponse_DataChange_USER, dc.GetType())
	assert.Equal(t, "row", string(dc.GetRowKey()))
	assert.Equal(t, "local-cluster", dc.GetSourceClusterId())
	assert.True(t, dc.GetDone(), "a complete DataChange must have done=true")
	assert.NotEmpty(t, dc.GetToken())
	assert.NotNil(t, dc.GetEstimatedLowWatermark())
	commit := dc.GetCommitTimestamp().AsTime()
	assert.False(t, commit.Before(before.Truncate(time.Microsecond)), "commit %v before request %v", commit, before)
	assert.False(t, commit.After(after), "commit %v after response %v", commit, after)

	require.Len(t, dc.GetChunks(), len(muts), "every mutation is one chunk, in request order")
	for _, ch := range dc.GetChunks() {
		assert.Nil(t, ch.GetChunkInfo(), "small values are not chunked")
	}
	set0 := dc.Chunks[0].GetMutation().GetSetCell()
	require.NotNil(t, set0)
	assert.Equal(t, "cf1", set0.GetFamilyName())
	assert.Equal(t, int64(1000), set0.GetTimestampMicros())
	assert.Equal(t, "va", string(set0.GetValue()))
	set1 := dc.Chunks[1].GetMutation().GetSetCell()
	require.NotNil(t, set1)
	assert.Greater(t, set1.GetTimestampMicros(), int64(0), "server-assigned timestamps are recorded resolved")
	assert.Zero(t, set1.GetTimestampMicros()%1000, "server timestamps use millisecond granularity")
	assert.NotNil(t, dc.Chunks[2].GetMutation().GetDeleteFromColumn())
	assert.Equal(t, "cf2", dc.Chunks[3].GetMutation().GetDeleteFromFamily().GetFamilyName())
	assert.Equal(t, "vc", string(dc.Chunks[4].GetMutation().GetSetCell().GetValue()))
}

func TestConformanceChangeStreamOtherWriteRPCsRecordOneChangePerRow(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	name := ccsCreateTable(t, s, ccsParent, "t", true, ccsFamilies("cf"))
	start := timestamppb.Now()

	// MutateRows: entries for the same row in one batch form one record.
	err := s.MutateRows(&btpb.MutateRowsRequest{TableName: name, Entries: []*btpb.MutateRowsRequest_Entry{
		{RowKey: []byte("r1"), Mutations: []*btpb.Mutation{ccsSetCell("cf", "a", 1000, "1")}},
		{RowKey: []byte("r2"), Mutations: []*btpb.Mutation{ccsSetCell("cf", "b", 1000, "2")}},
		{RowKey: []byte("r1"), Mutations: []*btpb.Mutation{ccsSetCell("cf", "c", 1000, "3")}},
	}}, &bigtableTestingMutateRowsServer{})
	require.NoError(t, err)

	// CheckAndMutateRow records only the applied branch.
	_, err = s.CheckAndMutateRow(ctx, &btpb.CheckAndMutateRowRequest{TableName: name, RowKey: []byte("r2"),
		TrueMutations:  []*btpb.Mutation{ccsSetCell("cf", "t", 2000, "true")},
		FalseMutations: []*btpb.Mutation{ccsSetCell("cf", "f", 2000, "false")},
	})
	require.NoError(t, err)

	// ReadModifyWriteRow is recorded as the resulting SetCell.
	_, err = s.ReadModifyWriteRow(ctx, &btpb.ReadModifyWriteRowRequest{TableName: name, RowKey: []byte("r3"),
		Rules: []*btpb.ReadModifyWriteRule{{FamilyName: "cf", ColumnQualifier: []byte("n"),
			Rule: &btpb.ReadModifyWriteRule_IncrementAmount{IncrementAmount: 5}}}})
	require.NoError(t, err)

	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)})
	byKey := map[string][]*btpb.ReadChangeStreamResponse_DataChange{}
	for _, dc := range res.changes {
		byKey[string(dc.GetRowKey())] = append(byKey[string(dc.GetRowKey())], dc)
	}
	require.Len(t, res.changes, 4, "rows: r1(MutateRows) r2(MutateRows) r2(CheckAndMutate) r3(RMW): %v", ccsRowKeys(res.changes))
	require.Len(t, byKey["r1"], 1)
	require.Len(t, byKey["r1"][0].GetChunks(), 2)
	assert.Equal(t, "a", string(byKey["r1"][0].Chunks[0].GetMutation().GetSetCell().GetColumnQualifier()))
	assert.Equal(t, "c", string(byKey["r1"][0].Chunks[1].GetMutation().GetSetCell().GetColumnQualifier()))

	require.Len(t, byKey["r2"], 2)
	cm := byKey["r2"][1]
	require.Len(t, cm.GetChunks(), 1)
	assert.Equal(t, "true", string(cm.Chunks[0].GetMutation().GetSetCell().GetValue()))

	require.Len(t, byKey["r3"], 1)
	rmw := byKey["r3"][0].GetChunks()
	require.Len(t, rmw, 1)
	assert.Equal(t, encodeInt64(5), rmw[0].GetMutation().GetSetCell().GetValue())
	for _, dc := range res.changes {
		assert.Equal(t, btpb.ReadChangeStreamResponse_DataChange_USER, dc.GetType())
	}
}

func TestConformanceChangeStreamDeleteFromRowIsDeleteFromFamilyPerFamily(t *testing.T) {
	s := newTestServer(t)
	name := ccsCreateTable(t, s, ccsParent, "t", true, ccsFamilies("cf1", "cf2", "cf3"))
	ccsMutate(t, s, name, "row", ccsSetCell("cf1", "a", 1000, "1"), ccsSetCell("cf2", "b", 1000, "2"))

	start := timestamppb.Now()
	ccsMutate(t, s, name, "row", &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromRow_{DeleteFromRow: &btpb.Mutation_DeleteFromRow{}}})
	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)})
	require.Len(t, res.changes, 1)
	// Only the families that hold data in the row (cf3 is empty).
	assert.Equal(t, []string{"cf1", "cf2"}, ccsDeletedFamilies(t, res.changes[0]))
	assert.Nil(t, storedRow(t, s.tables[name], "row"))
}

func TestConformanceChangeStreamGarbageCollectionRecords(t *testing.T) {
	s := newTestServer(t)
	name := ccsCreateTable(t, s, ccsParent, "t", true, map[string]*btapb.ColumnFamily{
		"v": {GcRule: &btapb.GcRule{Rule: &btapb.GcRule_MaxNumVersions{MaxNumVersions: 1}}},
	})
	start := timestamppb.Now()
	ccsMutate(t, s, name, "row", ccsSetCell("v", "c", 1000, "old"))
	ccsMutate(t, s, name, "row", ccsSetCell("v", "c", 2000, "new"))

	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)})
	require.Len(t, res.changes, 3, "two user writes and one garbage collection record")
	assert.Equal(t, btpb.ReadChangeStreamResponse_DataChange_USER, res.changes[0].GetType())
	assert.Equal(t, btpb.ReadChangeStreamResponse_DataChange_USER, res.changes[1].GetType())
	gc := res.changes[2]
	assert.Equal(t, btpb.ReadChangeStreamResponse_DataChange_GARBAGE_COLLECTION, gc.GetType())
	assert.Empty(t, gc.GetSourceClusterId(), "source_cluster_id is not set for GARBAGE_COLLECTION")
	assert.Equal(t, "row", string(gc.GetRowKey()))
	assert.True(t, gc.GetDone())
	require.Len(t, gc.GetChunks(), 1)
	del := gc.Chunks[0].GetMutation().GetDeleteFromColumn()
	require.NotNil(t, del)
	assert.Equal(t, "v", del.GetFamilyName())
	assert.Equal(t, "c", string(del.GetColumnQualifier()))
	assert.Equal(t, int64(1000), del.GetTimeRange().GetStartTimestampMicros())
	assert.Greater(t, del.GetTimeRange().GetEndTimestampMicros(), int64(1000))
	assert.LessOrEqual(t, del.GetTimeRange().GetEndTimestampMicros(), int64(2000), "the GC range must not cover the surviving cell")
	assert.Equal(t, res.changes[1].GetCommitTimestamp().AsTime(), gc.GetCommitTimestamp().AsTime())
}

func TestConformanceChangeStreamDropRowRangeRecords(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	name := ccsCreateTable(t, s, ccsParent, "t", true, ccsFamilies("a", "b"))
	ccsMutate(t, s, name, "p1", ccsSetCell("a", "c", 1000, "1"), ccsSetCell("b", "c", 1000, "1"))
	ccsMutate(t, s, name, "p2", ccsSetCell("b", "c", 1000, "2"))
	ccsMutate(t, s, name, "q1", ccsSetCell("a", "c", 1000, "3"))

	start := timestamppb.Now()
	_, err := s.DropRowRange(ctx, &btapb.DropRowRangeRequest{Name: name, Target: &btapb.DropRowRangeRequest_RowKeyPrefix{RowKeyPrefix: []byte("p")}})
	require.NoError(t, err)
	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)})
	require.ElementsMatch(t, []string{"p1", "p2"}, ccsRowKeys(res.changes))
	for _, dc := range res.changes {
		assert.Equal(t, btpb.ReadChangeStreamResponse_DataChange_USER, dc.GetType())
		switch string(dc.GetRowKey()) {
		case "p1":
			assert.Equal(t, []string{"a", "b"}, ccsDeletedFamilies(t, dc))
		case "p2":
			assert.Equal(t, []string{"b"}, ccsDeletedFamilies(t, dc))
		}
	}

	start = timestamppb.Now()
	_, err = s.DropRowRange(ctx, &btapb.DropRowRangeRequest{Name: name, Target: &btapb.DropRowRangeRequest_DeleteAllDataFromTable{DeleteAllDataFromTable: true}})
	require.NoError(t, err)
	res = ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)})
	require.Equal(t, []string{"q1"}, ccsRowKeys(res.changes))
	assert.Equal(t, []string{"a"}, ccsDeletedFamilies(t, res.changes[0]))
	assert.Equal(t, 0, rowCount(t, s.tables[name]))
}

func TestConformanceChangeStreamResumeWithContinuationToken(t *testing.T) {
	s := newTestServer(t)
	name := ccsCreateTable(t, s, ccsParent, "t", true, ccsFamilies("cf"))
	start := timestamppb.Now()
	for _, k := range []string{"r1", "r2", "r3"} {
		ccsMutate(t, s, name, k, ccsSetCell("cf", "c", 1000, k))
	}
	first := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)})
	require.Equal(t, []string{"r1", "r2", "r3"}, ccsRowKeys(first.changes))
	tokens := map[string]bool{}
	for _, dc := range first.changes {
		require.NotEmpty(t, dc.GetToken())
		require.False(t, tokens[dc.GetToken()], "tokens must be unique per record")
		tokens[dc.GetToken()] = true
	}

	// The reader "crashes" after processing r1; more writes happen meanwhile.
	ccsMutate(t, s, name, "r4", ccsSetCell("cf", "c", 1000, "r4"))
	full := fullStreamPartition()
	resume := func(tokens ...string) ccsResult {
		ct := &btpb.StreamContinuationTokens{}
		for _, tok := range tokens {
			ct.Tokens = append(ct.Tokens, &btpb.StreamContinuationToken{Partition: full, Token: tok})
		}
		return ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, Partition: full,
			StartFrom: &btpb.ReadChangeStreamRequest_ContinuationTokens{ContinuationTokens: ct}})
	}
	res := resume(first.changes[0].GetToken())
	require.Equal(t, []string{"r2", "r3", "r4"}, ccsRowKeys(res.changes), "resume must neither lose nor duplicate records")

	// With several tokens, delivery resumes after the earliest position.
	res = resume(first.changes[2].GetToken(), first.changes[0].GetToken())
	require.Equal(t, []string{"r2", "r3", "r4"}, ccsRowKeys(res.changes))

	// Resuming from the last delivered record yields nothing old.
	res = resume(res.changes[2].GetToken())
	require.Empty(t, res.changes)

	// A token is opaque but must be valid, and its partition must match.
	_, err := ccsRead(s, &btpb.ReadChangeStreamRequest{TableName: name, EndTime: timestamppb.Now(),
		StartFrom: &btpb.ReadChangeStreamRequest_ContinuationTokens{ContinuationTokens: &btpb.StreamContinuationTokens{
			Tokens: []*btpb.StreamContinuationToken{{Partition: full, Token: "not-a-token"}}}}})
	require.Equal(t, codes.InvalidArgument, ccsCode(err), "%v", err)
	other := &btpb.StreamPartition{RowRange: &btpb.RowRange{StartKey: &btpb.RowRange_StartKeyClosed{StartKeyClosed: []byte("m")}}}
	_, err = ccsRead(s, &btpb.ReadChangeStreamRequest{TableName: name, Partition: full, EndTime: timestamppb.Now(),
		StartFrom: &btpb.ReadChangeStreamRequest_ContinuationTokens{ContinuationTokens: &btpb.StreamContinuationTokens{
			Tokens: []*btpb.StreamContinuationToken{{Partition: other, Token: first.changes[0].GetToken()}}}}})
	require.Equal(t, codes.InvalidArgument, ccsCode(err), "%v", err)
}

func TestConformanceChangeStreamStartTimeValidation(t *testing.T) {
	s := newTestServer(t)
	name := ccsCreateTable(t, s, ccsParent, "t", true, ccsFamilies("cf")) // 1 day retention

	for _, tc := range []struct {
		desc  string
		start time.Time
	}{
		{"older than retention", time.Now().Add(-25 * time.Hour)},
		{"in the future", time.Now().Add(time.Hour)},
	} {
		_, err := ccsRead(s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(timestamppb.New(tc.start)),
			EndTime: timestamppb.New(time.Now().Add(2 * time.Hour))})
		require.Equal(t, codes.InvalidArgument, ccsCode(err), "%s: %v", tc.desc, err)
	}

	// Within retention is accepted.
	ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(timestamppb.New(time.Now().Add(-23 * time.Hour)))})

	// start_time is inclusive: starting at a record's commit timestamp
	// includes that record and nothing earlier.
	start := timestamppb.Now()
	ccsMutate(t, s, name, "r1", ccsSetCell("cf", "c", 1000, "1"))
	time.Sleep(2 * time.Millisecond)
	ccsMutate(t, s, name, "r2", ccsSetCell("cf", "c", 1000, "2"))
	all := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)})
	require.Equal(t, []string{"r1", "r2"}, ccsRowKeys(all.changes))
	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(all.changes[1].GetCommitTimestamp())})
	require.Equal(t, []string{"r2"}, ccsRowKeys(res.changes))

	// Negative heartbeat_duration is invalid.
	_, err := ccsRead(s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start), EndTime: timestamppb.Now(),
		HeartbeatDuration: durationpb.New(-time.Second)})
	require.Equal(t, codes.InvalidArgument, ccsCode(err), "%v", err)
}

func TestConformanceChangeStreamEndTimeClosesStreamOK(t *testing.T) {
	s := newTestServer(t)
	name := ccsCreateTable(t, s, ccsParent, "t", true, ccsFamilies("cf"))
	start := timestamppb.Now()
	ccsMutate(t, s, name, "r1", ccsSetCell("cf", "c", 1000, "1"))
	time.Sleep(20 * time.Millisecond)
	ccsMutate(t, s, name, "r2", ccsSetCell("cf", "c", 1000, "2"))

	all := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)})
	require.Equal(t, []string{"r1", "r2"}, ccsRowKeys(all.changes))

	// end_time is inclusive: a record committed exactly at end_time is
	// delivered, later records are not, then the stream closes with OK.
	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start), EndTime: all.changes[0].GetCommitTimestamp()})
	require.Equal(t, []string{"r1"}, ccsRowKeys(res.changes))

	// end_time equal to start_time before any record closes immediately.
	res = ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start), EndTime: start})
	require.Empty(t, res.changes)

	// Without end_time the stream stays open until the client cancels.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	st := &ccsStream{ctx: ctx}
	err := s.ReadChangeStream(&btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)}, st)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	st.mu.Lock()
	defer st.mu.Unlock()
	var keys []string
	for _, m := range st.msgs {
		require.Nil(t, m.GetCloseStream(), "no CloseStream without end_time")
		if dc := m.GetDataChange(); dc != nil {
			keys = append(keys, string(dc.GetRowKey()))
		}
	}
	require.Equal(t, []string{"r1", "r2"}, keys)
}

func TestConformanceChangeStreamHeartbeats(t *testing.T) {
	s := newTestServer(t)
	name := ccsCreateTable(t, s, ccsParent, "t", true, ccsFamilies("cf"))
	start := timestamppb.Now()
	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start),
		HeartbeatDuration: durationpb.New(20 * time.Millisecond),
		EndTime:           timestamppb.New(time.Now().Add(300 * time.Millisecond))})
	require.Empty(t, res.changes)
	require.GreaterOrEqual(t, len(res.heartbeats), 2, "expected periodic heartbeats")
	var prev time.Time
	for _, hb := range res.heartbeats {
		ct := hb.GetContinuationToken()
		require.NotNil(t, ct)
		assert.NotEmpty(t, ct.GetToken())
		assert.True(t, proto.Equal(fullStreamPartition(), ct.GetPartition()), "heartbeat token partition must be the read partition")
		wm := hb.GetEstimatedLowWatermark()
		require.NotNil(t, wm)
		assert.False(t, wm.AsTime().Before(prev), "low watermark must not move backwards")
		prev = wm.AsTime()
	}

	// A heartbeat token resumes exactly after everything already seen.
	ccsMutate(t, s, name, "later", ccsSetCell("cf", "c", 1000, "x"))
	last := res.heartbeats[len(res.heartbeats)-1].GetContinuationToken()
	resumed := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name,
		StartFrom: &btpb.ReadChangeStreamRequest_ContinuationTokens{ContinuationTokens: &btpb.StreamContinuationTokens{
			Tokens: []*btpb.StreamContinuationToken{last}}}})
	require.Equal(t, []string{"later"}, ccsRowKeys(resumed.changes))

	// With several tokens delivery resumes after the earliest one, even when
	// the earliest is the position before the first record of the table.
	ccsMutate(t, s, name, "latest", ccsSetCell("cf", "c", 1000, "y"))
	resumed = ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name,
		StartFrom: &btpb.ReadChangeStreamRequest_ContinuationTokens{ContinuationTokens: &btpb.StreamContinuationTokens{
			Tokens: []*btpb.StreamContinuationToken{last, {Partition: last.GetPartition(), Token: resumed.changes[0].GetToken()}}}}})
	require.Equal(t, []string{"later", "latest"}, ccsRowKeys(resumed.changes))
}

func TestConformanceChangeStreamDisablePurgesRecords(t *testing.T) {
	s := newTestServer(t)
	name := ccsCreateTable(t, s, ccsParent, "t", true, ccsFamilies("cf"))
	start := timestamppb.Now()
	ccsMutate(t, s, name, "r1", ccsSetCell("cf", "c", 1000, "1"))
	require.Equal(t, 1, ccsStoredRecords(t, s, name))

	require.NoError(t, ccsSetChangeStream(t, s, name, nil))
	got, err := s.GetTable(context.Background(), &btapb.GetTableRequest{Name: name})
	require.NoError(t, err)
	assert.Nil(t, got.GetChangeStreamConfig())
	require.Equal(t, 0, ccsStoredRecords(t, s, name), "disabling must purge earlier records")
	_, err = ccsRead(s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start), EndTime: timestamppb.Now()})
	require.Equal(t, codes.FailedPrecondition, ccsCode(err), "%v", err)

	ccsMutate(t, s, name, "r2", ccsSetCell("cf", "c", 1000, "2"))
	require.Equal(t, 0, ccsStoredRecords(t, s, name), "a disabled stream records nothing")

	require.NoError(t, ccsSetChangeStream(t, s, name, ccsRetention(24*time.Hour)))
	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(start)})
	require.Empty(t, res.changes, "records from before the stream was disabled must not be readable")
}

func TestConformanceChangeStreamRetentionPurge(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	name := ccsCreateTable(t, s, ccsParent, "t", true, ccsFamilies("cf"))
	old := time.Now().Add(-25 * time.Hour).UnixMicro()
	require.NoError(t, s.changeLog.append(ctx, s.db, name, "old", btpb.ReadChangeStreamResponse_DataChange_USER, old,
		[]*btpb.Mutation{ccsSetCell("cf", "c", 1000, "old")}))
	ccsMutate(t, s, name, "new", ccsSetCell("cf", "c", 1000, "new"))
	require.Equal(t, 2, ccsStoredRecords(t, s, name))

	require.NoError(t, s.purgeExpiredChangeRecords(ctx))
	require.Equal(t, 1, ccsStoredRecords(t, s, name), "records older than the retention period are purged")
	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, StartFrom: ccsFrom(timestamppb.New(time.Now().Add(-23 * time.Hour)))})
	require.Equal(t, []string{"new"}, ccsRowKeys(res.changes))
}

func TestConformanceChangeStreamPartitions(t *testing.T) {
	s := newTestServer(t)
	name := ccsCreateTable(t, s, ccsParent, "t", true, ccsFamilies("cf"))

	ps := &ccsPartitionStream{}
	require.NoError(t, s.GenerateInitialChangeStreamPartitions(&btpb.GenerateInitialChangeStreamPartitionsRequest{TableName: name}, ps))
	require.Len(t, ps.msgs, 1, "the local store is a single tablet")
	rr := ps.msgs[0].GetPartition().GetRowRange()
	require.NotNil(t, rr)
	assert.Nil(t, rr.GetStartKey(), "full keyspace starts unbounded")
	assert.Nil(t, rr.GetEndKey(), "full keyspace ends unbounded")

	ps = &ccsPartitionStream{}
	err := s.GenerateInitialChangeStreamPartitions(&btpb.GenerateInitialChangeStreamPartitionsRequest{TableName: ccsParent + "/tables/missing"}, ps)
	require.Equal(t, codes.NotFound, ccsCode(err), "%v", err)

	// A partition restricts records to its row range.
	start := timestamppb.Now()
	for _, k := range []string{"a", "b", "l", "m", "z"} {
		ccsMutate(t, s, name, k, ccsSetCell("cf", "c", 1000, k))
	}
	part := &btpb.StreamPartition{RowRange: &btpb.RowRange{
		StartKey: &btpb.RowRange_StartKeyClosed{StartKeyClosed: []byte("b")},
		EndKey:   &btpb.RowRange_EndKeyOpen{EndKeyOpen: []byte("m")},
	}}
	res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, Partition: part, StartFrom: ccsFrom(start)})
	require.Equal(t, []string{"b", "l"}, ccsRowKeys(res.changes))
	res = ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, Partition: ccsInitialPartition(t, s, name), StartFrom: ccsFrom(start)})
	require.Equal(t, []string{"a", "b", "l", "m", "z"}, ccsRowKeys(res.changes))
}

// ccsInitialPartition returns the single initial partition of a table.
func ccsInitialPartition(t *testing.T, s *server, name string) *btpb.StreamPartition {
	t.Helper()
	ps := &ccsPartitionStream{}
	require.NoError(t, s.GenerateInitialChangeStreamPartitions(&btpb.GenerateInitialChangeStreamPartitionsRequest{TableName: name}, ps))
	require.Len(t, ps.msgs, 1)
	return ps.msgs[0].GetPartition()
}

func TestConformanceChangeStreamAppProfileRouting(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	_, err := s.CreateInstance(ctx, &btapb.CreateInstanceRequest{Parent: "projects/p", InstanceId: "csinst1",
		Instance: &btapb.Instance{DisplayName: "cs instance"},
		Clusters: map[string]*btapb.Cluster{"cluster-a": {}, "cluster-b": {}}})
	require.NoError(t, err)
	instance := "projects/p/instances/csinst1"
	_, err = s.CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{Parent: instance, AppProfileId: "multi",
		AppProfile: &btapb.AppProfile{RoutingPolicy: &btapb.AppProfile_MultiClusterRoutingUseAny_{
			MultiClusterRoutingUseAny: &btapb.AppProfile_MultiClusterRoutingUseAny{}}}})
	require.NoError(t, err)
	_, err = s.CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{Parent: instance, AppProfileId: "single",
		AppProfile: &btapb.AppProfile{RoutingPolicy: &btapb.AppProfile_SingleClusterRouting_{
			SingleClusterRouting: &btapb.AppProfile_SingleClusterRouting{ClusterId: "cluster-b"}}}})
	require.NoError(t, err)
	name := ccsCreateTable(t, s, instance, "t", true, ccsFamilies("cf"))

	_, err = ccsRead(s, &btpb.ReadChangeStreamRequest{TableName: name, AppProfileId: "multi", StartFrom: ccsFrom(timestamppb.Now()), EndTime: timestamppb.Now()})
	require.Equal(t, codes.FailedPrecondition, ccsCode(err), "%v", err)
	err = s.GenerateInitialChangeStreamPartitions(&btpb.GenerateInitialChangeStreamPartitionsRequest{TableName: name, AppProfileId: "multi"}, &ccsPartitionStream{})
	require.Equal(t, codes.FailedPrecondition, ccsCode(err), "%v", err)
	_, err = ccsRead(s, &btpb.ReadChangeStreamRequest{TableName: name, AppProfileId: "missing", StartFrom: ccsFrom(timestamppb.Now()), EndTime: timestamppb.Now()})
	require.Equal(t, codes.NotFound, ccsCode(err), "%v", err)

	start := timestamppb.Now()
	ccsMutate(t, s, name, "row", ccsSetCell("cf", "c", 1000, "v"))
	for _, profile := range []string{"", "default", "single"} {
		res := ccsReadAll(t, s, &btpb.ReadChangeStreamRequest{TableName: name, AppProfileId: profile, StartFrom: ccsFrom(start)})
		require.Len(t, res.changes, 1, "profile %q", profile)
		assert.Equal(t, "cluster-a", res.changes[0].GetSourceClusterId(), "the cluster that applied the write")
	}
}

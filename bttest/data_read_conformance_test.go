package bttest

// Conformance tests for ReadRows and SampleRowKeys against the contracts in
// google/bigtable/v2/bigtable.proto (CellChunk, RequestStats, rows_limit,
// reversed, SampleRowKeysRequest.row_range, SampleRowKeysResponse).

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// crdNewTable creates a table with the given families (no GC rules) and
// optional initial splits on an unregistered instance.
func crdNewTable(t *testing.T, s *server, parent, id string, splits []string, families ...string) string {
	t.Helper()
	tbl := &btapb.Table{ColumnFamilies: map[string]*btapb.ColumnFamily{}}
	for _, f := range families {
		tbl.ColumnFamilies[f] = &btapb.ColumnFamily{}
	}
	req := &btapb.CreateTableRequest{Parent: parent, TableId: id, Table: tbl}
	for _, k := range splits {
		req.InitialSplits = append(req.InitialSplits, &btapb.CreateTableRequest_Split{Key: []byte(k)})
	}
	created, err := s.CreateTable(context.Background(), req)
	require.NoError(t, err)
	return created.Name
}

// crdSet writes one cell through the MutateRow handler.
func crdSet(t *testing.T, s *server, tableName, key, fam, qual string, ts int64, value []byte) {
	t.Helper()
	_, err := s.MutateRow(context.Background(), &btpb.MutateRowRequest{
		TableName: tableName,
		RowKey:    []byte(key),
		Mutations: []*btpb.Mutation{{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
			FamilyName: fam, ColumnQualifier: []byte(qual), TimestampMicros: ts, Value: value,
		}}}},
	})
	require.NoError(t, err)
}

// crdRead runs ReadRows through the handler and returns the mock stream.
func crdRead(s *server, req *btpb.ReadRowsRequest) (*MockReadRowsServer, error) {
	mock := &MockReadRowsServer{}
	return mock, s.ReadRows(req, mock)
}

// crdChunks flattens the raw (unfilled) chunks of every response.
func crdChunks(mock *MockReadRowsServer) []*btpb.ReadRowsResponse_CellChunk {
	var out []*btpb.ReadRowsResponse_CellChunk
	for _, resp := range mock.raw {
		out = append(out, resp.Chunks...)
	}
	return out
}

// crdCommittedKeys returns the row keys of committed rows, in stream order.
func crdCommittedKeys(mock *MockReadRowsServer) []string {
	var keys []string
	var cur string
	for _, c := range crdChunks(mock) {
		if c.RowKey != nil {
			cur = string(c.RowKey)
		}
		if c.GetCommitRow() {
			keys = append(keys, cur)
		}
	}
	return keys
}

// crdSamples runs SampleRowKeys through the handler.
func crdSamples(t *testing.T, s *server, req *btpb.SampleRowKeysRequest) []*btpb.SampleRowKeysResponse {
	t.Helper()
	mock := &MockSampleRowKeysServer{}
	require.NoError(t, s.SampleRowKeys(req, mock))
	return mock.responses
}

// crdCheckSampleOrder asserts the SampleRowKeysResponse ordering contract:
// keys strictly increasing, an empty key only as the last sample, and
// offsets non-decreasing.
func crdCheckSampleOrder(t *testing.T, samples []*btpb.SampleRowKeysResponse) {
	t.Helper()
	require.NotEmpty(t, samples)
	for i, smp := range samples {
		if len(smp.RowKey) == 0 {
			assert.Equal(t, len(samples)-1, i, "empty end-of-table key must be the last sample")
		}
		if i == 0 {
			assert.GreaterOrEqual(t, smp.OffsetBytes, int64(0))
			continue
		}
		prev := samples[i-1]
		if len(smp.RowKey) != 0 {
			assert.Equal(t, 1, bytes.Compare(smp.RowKey, prev.RowKey), "sample keys must be strictly increasing: %q then %q", prev.RowKey, smp.RowKey)
		}
		assert.GreaterOrEqual(t, smp.OffsetBytes, prev.OffsetBytes, "offsets must be non-decreasing")
	}
}

func crdSampleKeys(samples []*btpb.SampleRowKeysResponse) []string {
	keys := make([]string, len(samples))
	for i, smp := range samples {
		keys[i] = string(smp.RowKey)
	}
	return keys
}

// crdSetReadLimits lowers the response shaping limits for one test.
func crdSetReadLimits(t *testing.T, chunk, resp int) {
	prevChunk, prevResp := readChunkValueBytes, readResponseBytes
	readChunkValueBytes, readResponseBytes = chunk, resp
	t.Cleanup(func() { readChunkValueBytes, readResponseBytes = prevChunk, prevResp })
}

// TestConformanceReadChunkShape verifies CellChunk field presence: row_key
// only on the first chunk of a row, family_name only when the family
// changes, qualifier only when the column changes, timestamp on every cell,
// and commit_row exactly on the last chunk of each row.
func TestConformanceReadChunkShape(t *testing.T) {
	s, parent := newFullTestServer(t)
	name := crdNewTable(t, s, parent, "t", nil, "a", "b")
	crdSet(t, s, name, "r1", "a", "q1", 2000, []byte("v1"))
	crdSet(t, s, name, "r1", "a", "q1", 1000, []byte("v0"))
	crdSet(t, s, name, "r1", "a", "q2", 1000, []byte("x"))
	crdSet(t, s, name, "r1", "b", "q1", 1000, []byte("y"))
	crdSet(t, s, name, "r2", "a", "q1", 1000, []byte("z"))

	mock, err := crdRead(s, &btpb.ReadRowsRequest{TableName: name})
	require.NoError(t, err)
	chunks := crdChunks(mock)
	require.Len(t, chunks, 5)

	type want struct {
		key, fam, qual string // "" means absent
		ts             int64
		value          string
		commit         bool
	}
	wants := []want{
		{key: "r1", fam: "a", qual: "q1", ts: 2000, value: "v1"},
		{ts: 1000, value: "v0"},
		{qual: "q2", ts: 1000, value: "x"},
		{fam: "b", qual: "q1", ts: 1000, value: "y", commit: true},
		{key: "r2", fam: "a", qual: "q1", ts: 1000, value: "z", commit: true},
	}
	for i, w := range wants {
		c := chunks[i]
		if w.key == "" {
			assert.Nil(t, c.RowKey, "chunk %d: row_key must be omitted on continuation chunks", i)
		} else {
			assert.Equal(t, w.key, string(c.RowKey), "chunk %d row_key", i)
		}
		if w.fam == "" {
			assert.Nil(t, c.FamilyName, "chunk %d: family_name must be omitted when unchanged", i)
		} else if assert.NotNil(t, c.FamilyName, "chunk %d family_name", i) {
			assert.Equal(t, w.fam, c.FamilyName.Value)
		}
		if w.qual == "" {
			assert.Nil(t, c.Qualifier, "chunk %d: qualifier must be omitted when unchanged", i)
		} else if assert.NotNil(t, c.Qualifier, "chunk %d qualifier", i) {
			assert.Equal(t, w.qual, string(c.Qualifier.Value))
		}
		assert.Equal(t, w.ts, c.TimestampMicros, "chunk %d timestamp", i)
		assert.Equal(t, w.value, string(c.Value), "chunk %d value", i)
		assert.Zero(t, c.ValueSize, "chunk %d: unsplit cells have no value_size", i)
		assert.Equal(t, w.commit, c.GetCommitRow(), "chunk %d commit_row", i)
		assert.False(t, c.GetResetRow(), "chunk %d reset_row", i)
	}
	for _, resp := range mock.raw {
		assert.Nil(t, resp.RequestStats, "request_stats must be absent unless requested")
	}
}

// TestConformanceReadLargeValueSplit verifies that a value larger than the
// chunk limit is split: value_size on every non-final piece, timestamp and
// labels only on the first piece, continuation across responses, and exact
// reassembly.
func TestConformanceReadLargeValueSplit(t *testing.T) {
	crdSetReadLimits(t, 4, 8)
	s, parent := newFullTestServer(t)
	name := crdNewTable(t, s, parent, "t", nil, "f")
	crdSet(t, s, name, "row", "f", "q", 5000, []byte("0123456789"))

	mock, err := crdRead(s, &btpb.ReadRowsRequest{
		TableName: name,
		Filter:    &btpb.RowFilter{Filter: &btpb.RowFilter_ApplyLabelTransformer{ApplyLabelTransformer: "lbl"}},
	})
	require.NoError(t, err)
	require.Len(t, mock.raw, 1, "a row never spans responses (the Go client validates each response)")
	chunks := crdChunks(mock)
	require.Len(t, chunks, 3)

	first := chunks[0]
	assert.Equal(t, "row", string(first.RowKey))
	assert.Equal(t, "f", first.FamilyName.GetValue())
	assert.Equal(t, "q", string(first.Qualifier.GetValue()))
	assert.Equal(t, int64(5000), first.TimestampMicros)
	assert.Equal(t, []string{"lbl"}, first.Labels)
	assert.Equal(t, int32(10), first.ValueSize)
	assert.False(t, first.GetCommitRow())

	for i, c := range chunks[1:] {
		assert.Nil(t, c.RowKey, "piece %d row_key", i+1)
		assert.Nil(t, c.FamilyName, "piece %d family_name", i+1)
		assert.Nil(t, c.Qualifier, "piece %d qualifier", i+1)
		assert.Zero(t, c.TimestampMicros, "piece %d: timestamp only on the first piece", i+1)
		assert.Empty(t, c.Labels, "piece %d: labels only on the first piece", i+1)
	}
	assert.Equal(t, int32(10), chunks[1].ValueSize, "non-final piece carries value_size")
	assert.Zero(t, chunks[2].ValueSize, "final piece has no value_size")
	assert.True(t, chunks[2].GetCommitRow())

	var value []byte
	for _, c := range chunks {
		value = append(value, c.Value...)
	}
	assert.Equal(t, "0123456789", string(value))

	// Responses are flushed at row boundaries once readResponseBytes is
	// reached: every response ends with a committed row.
	crdSet(t, s, name, "row2", "f", "q", 5000, []byte("abcdefghij"))
	crdSet(t, s, name, "row3", "f", "q", 5000, []byte("k"))
	mock, err = crdRead(s, &btpb.ReadRowsRequest{TableName: name})
	require.NoError(t, err)
	require.Len(t, mock.raw, 3)
	for i, resp := range mock.raw {
		require.NotEmpty(t, resp.Chunks)
		assert.True(t, resp.Chunks[len(resp.Chunks)-1].GetCommitRow(), "response %d must end on a row boundary", i)
	}
	assert.Equal(t, []string{"row", "row2", "row3"}, crdCommittedKeys(mock))
}

// TestConformanceReadLargeValueClientReassembly verifies through the official
// Go client (which enforces the chunk protocol) that split values and many
// rows spanning several responses reassemble exactly.
func TestConformanceReadLargeValueClientReassembly(t *testing.T) {
	crdSetReadLimits(t, 100, 250)
	env := setupTestEnv(t)
	defer env.cancel()
	tbl := env.createTable(t, "large", "f", "g")

	rng := rand.New(rand.NewSource(7))
	big := make([]byte, 3001)
	rng.Read(big)
	medium := make([]byte, 250)
	rng.Read(medium)

	m := bigtable.NewMutation()
	m.Set("f", "big", bigtable.Timestamp(3000), big)
	m.Set("f", "big", bigtable.Timestamp(2000), medium)
	m.Set("f", "small", bigtable.Timestamp(1000), []byte("s"))
	m.Set("g", "empty", bigtable.Timestamp(1000), []byte{})
	require.NoError(t, tbl.Apply(env.ctx, "row-1", m))
	for i := 2; i <= 6; i++ {
		m := bigtable.NewMutation()
		m.Set("f", "big", bigtable.Timestamp(1000), medium)
		require.NoError(t, tbl.Apply(env.ctx, "row-"+string(rune('0'+i)), m))
	}

	r, err := tbl.ReadRow(env.ctx, "row-1")
	require.NoError(t, err)
	require.Len(t, r["f"], 3)
	assert.Equal(t, "f:big", r["f"][0].Column)
	assert.Equal(t, bigtable.Timestamp(3000), r["f"][0].Timestamp)
	assert.True(t, bytes.Equal(big, r["f"][0].Value), "3001-byte value must reassemble exactly")
	assert.Equal(t, bigtable.Timestamp(2000), r["f"][1].Timestamp)
	assert.True(t, bytes.Equal(medium, r["f"][1].Value))
	assert.Equal(t, "f:small", r["f"][2].Column)
	assert.Equal(t, "s", string(r["f"][2].Value))
	require.Len(t, r["g"], 1)
	assert.Empty(t, r["g"][0].Value)

	var keys []string
	err = tbl.ReadRows(env.ctx, bigtable.InfiniteRange(""), func(row bigtable.Row) bool {
		keys = append(keys, row.Key())
		if row.Key() != "row-1" {
			assert.True(t, bytes.Equal(medium, row["f"][0].Value), "row %s value", row.Key())
		}
		return true
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"row-1", "row-2", "row-3", "row-4", "row-5", "row-6"}, keys)
}

// TestConformanceReadRequestStatsFull verifies REQUEST_STATS_FULL: the
// stream ends with a response carrying only request_stats (also for empty
// results), with sane seen/returned counts; NONE/UNSPECIFIED return none.
func TestConformanceReadRequestStatsFull(t *testing.T) {
	s, parent := newFullTestServer(t)
	name := crdNewTable(t, s, parent, "t", nil, "a", "b")
	crdSet(t, s, name, "r1", "a", "q", 1000, []byte("1"))
	crdSet(t, s, name, "r1", "b", "q", 1000, []byte("2"))
	crdSet(t, s, name, "r2", "b", "q", 1000, []byte("3"))
	crdSet(t, s, name, "r3", "a", "q", 1000, []byte("4"))
	familyA := &btpb.RowFilter{Filter: &btpb.RowFilter_FamilyNameRegexFilter{FamilyNameRegexFilter: "a"}}

	mock, err := crdRead(s, &btpb.ReadRowsRequest{
		TableName:        name,
		Filter:           familyA,
		RequestStatsView: btpb.ReadRowsRequest_REQUEST_STATS_FULL,
	})
	require.NoError(t, err)
	require.NotEmpty(t, mock.raw)
	last := mock.raw[len(mock.raw)-1]
	require.NotNil(t, last.RequestStats, "final response must carry request_stats")
	assert.Empty(t, last.Chunks, "the stats response carries only request_stats")
	for _, resp := range mock.raw[:len(mock.raw)-1] {
		assert.Nil(t, resp.RequestStats, "request_stats only on the last response")
	}
	full := last.RequestStats.GetFullReadStatsView()
	require.NotNil(t, full)
	it := full.GetReadIterationStats()
	assert.Equal(t, int64(3), it.GetRowsSeenCount())
	assert.Equal(t, int64(2), it.GetRowsReturnedCount())
	assert.Equal(t, int64(4), it.GetCellsSeenCount())
	assert.Equal(t, int64(2), it.GetCellsReturnedCount())
	assert.NotNil(t, full.GetRequestLatencyStats().GetFrontendServerLatency())
	assert.Equal(t, []string{"r1", "r3"}, crdCommittedKeys(mock))

	// Empty result: stats are still returned.
	mock, err = crdRead(s, &btpb.ReadRowsRequest{
		TableName:        name,
		Rows:             &btpb.RowSet{RowKeys: [][]byte{[]byte("missing")}},
		RequestStatsView: btpb.ReadRowsRequest_REQUEST_STATS_FULL,
	})
	require.NoError(t, err)
	require.Len(t, mock.raw, 1)
	require.NotNil(t, mock.raw[0].RequestStats)
	assert.Zero(t, mock.raw[0].RequestStats.GetFullReadStatsView().GetReadIterationStats().GetRowsReturnedCount())

	for _, view := range []btpb.ReadRowsRequest_RequestStatsView{btpb.ReadRowsRequest_REQUEST_STATS_NONE, btpb.ReadRowsRequest_REQUEST_STATS_VIEW_UNSPECIFIED} {
		mock, err = crdRead(s, &btpb.ReadRowsRequest{TableName: name, RequestStatsView: view})
		require.NoError(t, err)
		for _, resp := range mock.raw {
			assert.Nil(t, resp.RequestStats, "view %v must not return request_stats", view)
		}
		assert.Equal(t, []string{"r1", "r2", "r3"}, crdCommittedKeys(mock))
	}

	_, err = crdRead(s, &btpb.ReadRowsRequest{TableName: name, RequestStatsView: btpb.ReadRowsRequest_RequestStatsView(99)})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestConformanceReadRequestStatsClient verifies the Go client's
// WithFullReadStats callback receives the server's FULL stats.
func TestConformanceReadRequestStatsClient(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	tbl := env.createTable(t, "stats", "f")
	for _, k := range []string{"a", "b", "c"} {
		m := bigtable.NewMutation()
		m.Set("f", "q", bigtable.Timestamp(1000), []byte(k))
		m.Set("f", "q2", bigtable.Timestamp(1000), []byte(k))
		require.NoError(t, tbl.Apply(env.ctx, k, m))
	}
	var got *bigtable.FullReadStats
	var rows int
	err := tbl.ReadRows(env.ctx, bigtable.InfiniteRange(""), func(bigtable.Row) bool { rows++; return true },
		bigtable.RowFilter(bigtable.ColumnFilter("q2")),
		bigtable.WithFullReadStats(func(s *bigtable.FullReadStats) { got = s }))
	require.NoError(t, err)
	assert.Equal(t, 3, rows)
	require.NotNil(t, got, "client must receive the FULL read stats")
	assert.Equal(t, int64(3), got.ReadIterationStats.RowsSeenCount)
	assert.Equal(t, int64(3), got.ReadIterationStats.RowsReturnedCount)
	assert.Equal(t, int64(6), got.ReadIterationStats.CellsSeenCount)
	assert.Equal(t, int64(3), got.ReadIterationStats.CellsReturnedCount)
	assert.GreaterOrEqual(t, got.RequestLatencyStats.FrontendServerLatency.Nanoseconds(), int64(0))
}

// TestConformanceReadReversedWithLimit verifies reversed scans return rows
// in descending key order and rows_limit counts returned rows only.
func TestConformanceReadReversedWithLimit(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	tbl := env.createTable(t, "rev", "f", "g")
	for _, k := range []string{"k1", "k2", "k3", "k4", "k5"} {
		m := bigtable.NewMutation()
		m.Set("f", "q", bigtable.Timestamp(1000), []byte(k))
		require.NoError(t, tbl.Apply(env.ctx, k, m))
	}
	// k5 has only family g, so a family-f filter drops it before rows_limit
	// is applied.
	m := bigtable.NewMutation()
	m.DeleteRow()
	require.NoError(t, tbl.Apply(env.ctx, "k5", m))
	m = bigtable.NewMutation()
	m.Set("g", "q", bigtable.Timestamp(1000), []byte("k5"))
	require.NoError(t, tbl.Apply(env.ctx, "k5", m))

	read := func(rs bigtable.RowSet, opts ...bigtable.ReadOption) []string {
		t.Helper()
		var keys []string
		require.NoError(t, tbl.ReadRows(env.ctx, rs, func(r bigtable.Row) bool {
			keys = append(keys, r.Key())
			return true
		}, opts...))
		return keys
	}
	assert.Equal(t, []string{"k5", "k4"}, read(bigtable.InfiniteRange(""), bigtable.ReverseScan(), bigtable.LimitRows(2)))
	assert.Equal(t, []string{"k4", "k3"}, read(bigtable.InfiniteRange(""), bigtable.ReverseScan(), bigtable.LimitRows(2),
		bigtable.RowFilter(bigtable.FamilyFilter("f"))), "rows_limit counts returned rows, after filtering")
	assert.Equal(t, []string{"k3", "k2"}, read(bigtable.NewRange("k2", "k4"), bigtable.ReverseScan()))
	assert.Equal(t, []string{"k4", "k2", "k1"}, read(bigtable.RowList{"k1", "k4", "k2"}, bigtable.ReverseScan()))
	assert.Equal(t, []string{"k1", "k2", "k3"}, read(bigtable.InfiniteRange(""), bigtable.LimitRows(3)))

	// Reversed reads keep row contents in forward (family, qualifier,
	// timestamp desc) order.
	m = bigtable.NewMutation()
	m.Set("f", "a", bigtable.Timestamp(2000), []byte("new"))
	m.Set("f", "a", bigtable.Timestamp(1000), []byte("old"))
	require.NoError(t, tbl.Apply(env.ctx, "k1", m))
	var row bigtable.Row
	require.NoError(t, tbl.ReadRows(env.ctx, bigtable.RowList{"k1"}, func(r bigtable.Row) bool { row = r; return true }, bigtable.ReverseScan()))
	require.Len(t, row["f"], 3)
	assert.Equal(t, "f:a", row["f"][0].Column)
	assert.Equal(t, bigtable.Timestamp(2000), row["f"][0].Timestamp)
	assert.Equal(t, bigtable.Timestamp(1000), row["f"][1].Timestamp)
	assert.Equal(t, "f:q", row["f"][2].Column)
}

// TestConformanceReadNegativeRowsLimit verifies rows_limit < 0 is rejected.
func TestConformanceReadNegativeRowsLimit(t *testing.T) {
	s, parent := newFullTestServer(t)
	name := crdNewTable(t, s, parent, "t", nil, "f")
	crdSet(t, s, name, "r", "f", "q", 1000, []byte("v"))
	mock, err := crdRead(s, &btpb.ReadRowsRequest{TableName: name, RowsLimit: -1})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "err=%v", err)
	assert.Empty(t, mock.raw)

	mock, err = crdRead(s, &btpb.ReadRowsRequest{TableName: name, RowsLimit: 0})
	require.NoError(t, err)
	assert.Equal(t, []string{"r"}, crdCommittedKeys(mock), "rows_limit 0 means unlimited")
}

// TestConformanceReadMixedKeysAndRanges verifies a RowSet mixing row_keys and
// overlapping row_ranges returns each row once, in key order (descending
// when reversed), and skips missing keys.
func TestConformanceReadMixedKeysAndRanges(t *testing.T) {
	s, parent := newFullTestServer(t)
	name := crdNewTable(t, s, parent, "t", nil, "f")
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		crdSet(t, s, name, k, "f", "q", 1000, []byte(k))
	}
	rs := &btpb.RowSet{
		RowKeys: [][]byte{[]byte("e"), []byte("b"), []byte("b"), []byte("zz"), []byte("a")},
		RowRanges: []*btpb.RowRange{
			{StartKey: &btpb.RowRange_StartKeyClosed{StartKeyClosed: []byte("b")}, EndKey: &btpb.RowRange_EndKeyOpen{EndKeyOpen: []byte("d")}},
			{StartKey: &btpb.RowRange_StartKeyClosed{StartKeyClosed: []byte("c")}, EndKey: &btpb.RowRange_EndKeyClosed{EndKeyClosed: []byte("e")}},
			{StartKey: &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("e")}, EndKey: &btpb.RowRange_EndKeyOpen{EndKeyOpen: []byte("g")}},
		},
	}
	mock, err := crdRead(s, &btpb.ReadRowsRequest{TableName: name, Rows: rs})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c", "d", "e", "f"}, crdCommittedKeys(mock))
	for _, c := range crdChunks(mock) {
		if c.RowKey != nil {
			assert.Equal(t, string(c.RowKey), string(c.Value), "each row's content belongs to that row")
		}
	}

	mock, err = crdRead(s, &btpb.ReadRowsRequest{TableName: name, Rows: rs, Reversed: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"f", "e", "d", "c", "b", "a"}, crdCommittedKeys(mock))

	mock, err = crdRead(s, &btpb.ReadRowsRequest{TableName: name, Rows: rs, Reversed: true, RowsLimit: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{"f", "e"}, crdCommittedKeys(mock))

	// An inverted range is rejected.
	_, err = crdRead(s, &btpb.ReadRowsRequest{TableName: name, Rows: &btpb.RowSet{RowRanges: []*btpb.RowRange{
		{StartKey: &btpb.RowRange_StartKeyClosed{StartKeyClosed: []byte("d")}, EndKey: &btpb.RowRange_EndKeyOpen{EndKeyOpen: []byte("b")}},
	}}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestConformanceSampleRowKeysEndOfTable verifies an unrestricted sample
// ends with the empty "end of table" key carrying the total offset.
func TestConformanceSampleRowKeysEndOfTable(t *testing.T) {
	s, parent := newFullTestServer(t)
	name := crdNewTable(t, s, parent, "t", nil, "f")

	samples := crdSamples(t, s, &btpb.SampleRowKeysRequest{TableName: name})
	require.Len(t, samples, 1, "an empty table samples only the end of table")
	assert.Empty(t, samples[0].RowKey)
	assert.Zero(t, samples[0].OffsetBytes)

	crdSet(t, s, name, "a", "f", "q", 1000, []byte("12345"))
	crdSet(t, s, name, "b", "f", "q", 1000, []byte("123"))
	samples = crdSamples(t, s, &btpb.SampleRowKeysRequest{TableName: name})
	crdCheckSampleOrder(t, samples)
	last := samples[len(samples)-1]
	assert.Empty(t, last.RowKey, "the final sample is the empty end-of-table key")
	assert.Equal(t, int64((1+1+5)+(1+1+3)), last.OffsetBytes, "end-of-table offset is the total size")
}

// TestConformanceSampleRowKeysInitialSplits verifies through the Go client
// that initial split keys are reported as sorted, deduplicated sample keys.
// The client drops the trailing empty end-of-table key (see
// TestConformanceSampleRowKeysOffsets for the raw stream).
func TestConformanceSampleRowKeysInitialSplits(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	require.NoError(t, env.admin.CreatePresplitTable(env.ctx, "split", []string{"m", "c", "t", "c"}))
	require.NoError(t, env.admin.CreateColumnFamily(env.ctx, "split", "f"))
	tbl := env.client.Open("split")

	keys, err := tbl.SampleRowKeys(env.ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"c", "m", "t"}, keys, "empty table: split keys only")

	for _, k := range []string{"a", "d", "n", "z"} {
		m := bigtable.NewMutation()
		m.Set("f", "q", bigtable.Timestamp(1000), []byte("vvvv"))
		require.NoError(t, tbl.Apply(env.ctx, k, m))
	}
	keys, err = tbl.SampleRowKeys(env.ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"c", "m", "t"}, keys)
}

// TestConformanceSampleRowKeysOffsets verifies offsets at split keys equal
// the size of all preceding rows, and that every sample's key is strictly
// increasing even when a periodic sample lands on a split key.
func TestConformanceSampleRowKeysOffsets(t *testing.T) {
	s, parent := newFullTestServer(t)
	name := crdNewTable(t, s, parent, "t", []string{"m", "c", "t", "d", "c"}, "f")
	for _, k := range []string{"a", "d", "n", "z"} {
		crdSet(t, s, name, k, "f", "q", 1000, []byte("vvvv"))
	}
	const rowSize = 1 + 1 + 4 // key + qualifier + value
	samples := crdSamples(t, s, &btpb.SampleRowKeysRequest{TableName: name})
	crdCheckSampleOrder(t, samples)
	assert.Equal(t, []string{"c", "d", "m", "t", ""}, crdSampleKeys(samples))
	offsets := map[string]int64{}
	for _, smp := range samples {
		offsets[string(smp.RowKey)] = smp.OffsetBytes
	}
	assert.Equal(t, int64(rowSize), offsets["c"], "rows before c: a")
	assert.Equal(t, int64(rowSize), offsets["d"], "rows before d: a")
	assert.Equal(t, int64(2*rowSize), offsets["m"], "rows before m: a, d")
	assert.Equal(t, int64(3*rowSize), offsets["t"], "rows before t: a, d, n")
	assert.Equal(t, int64(4*rowSize), offsets[""], "end of table: all rows")

	// Periodic samples: one per sampleEveryBytes of data. Lower the interval
	// so every row produces a sample, including row "d" which is also a
	// split key.
	prev := sampleEveryBytes
	sampleEveryBytes = 1
	t.Cleanup(func() { sampleEveryBytes = prev })
	samples = crdSamples(t, s, &btpb.SampleRowKeysRequest{TableName: name})
	crdCheckSampleOrder(t, samples)
	keys := crdSampleKeys(samples)
	assert.Equal(t, "", keys[len(keys)-1])
	for _, want := range []string{"a", "c", "d", "m", "n", "t", "z"} {
		assert.Contains(t, keys, want)
	}
}

// TestConformanceSampleRowKeysRowRange verifies row_range restricts samples:
// split keys outside the range are omitted, offsets count only rows inside
// the range, and the last sample is the range's end key (empty when the
// range is unbounded).
func TestConformanceSampleRowKeysRowRange(t *testing.T) {
	s, parent := newFullTestServer(t)
	name := crdNewTable(t, s, parent, "t", []string{"c", "m", "t"}, "f")
	for _, k := range []string{"a", "d", "n", "z"} {
		crdSet(t, s, name, k, "f", "q", 1000, []byte("vvvv"))
	}
	const rowSize = 1 + 1 + 4

	// [d, t): split m inside; end key t.
	samples := crdSamples(t, s, &btpb.SampleRowKeysRequest{TableName: name, RowRange: &btpb.RowRange{
		StartKey: &btpb.RowRange_StartKeyClosed{StartKeyClosed: []byte("d")},
		EndKey:   &btpb.RowRange_EndKeyOpen{EndKeyOpen: []byte("t")},
	}})
	crdCheckSampleOrder(t, samples)
	assert.Equal(t, []string{"m", "t"}, crdSampleKeys(samples))
	assert.Equal(t, int64(rowSize), samples[0].OffsetBytes, "rows in range before m: d")
	assert.Equal(t, int64(2*rowSize), samples[1].OffsetBytes, "rows in range before t: d, n")

	// (a, n]: closed end key n is the last sample and includes row n.
	samples = crdSamples(t, s, &btpb.SampleRowKeysRequest{TableName: name, RowRange: &btpb.RowRange{
		StartKey: &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("a")},
		EndKey:   &btpb.RowRange_EndKeyClosed{EndKeyClosed: []byte("n")},
	}})
	crdCheckSampleOrder(t, samples)
	assert.Equal(t, []string{"c", "m", "n"}, crdSampleKeys(samples))
	assert.Equal(t, []int64{0, rowSize, 2 * rowSize}, []int64{samples[0].OffsetBytes, samples[1].OffsetBytes, samples[2].OffsetBytes})

	// [n, ∞): no end key, so the last sample is the empty end-of-table key.
	samples = crdSamples(t, s, &btpb.SampleRowKeysRequest{TableName: name, RowRange: &btpb.RowRange{
		StartKey: &btpb.RowRange_StartKeyClosed{StartKeyClosed: []byte("n")},
	}})
	crdCheckSampleOrder(t, samples)
	assert.Equal(t, []string{"t", ""}, crdSampleKeys(samples))
	assert.Equal(t, int64(rowSize), samples[0].OffsetBytes)
	assert.Equal(t, int64(2*rowSize), samples[1].OffsetBytes)

	// A range with no rows still ends with its end key.
	samples = crdSamples(t, s, &btpb.SampleRowKeysRequest{TableName: name, RowRange: &btpb.RowRange{
		StartKey: &btpb.RowRange_StartKeyClosed{StartKeyClosed: []byte("e")},
		EndKey:   &btpb.RowRange_EndKeyOpen{EndKeyOpen: []byte("f")},
	}})
	assert.Equal(t, []string{"f"}, crdSampleKeys(samples))
	assert.Zero(t, samples[0].OffsetBytes)
}

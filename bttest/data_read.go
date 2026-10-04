package bttest

import (
	"context"
	"sort"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/golang/protobuf/ptypes/wrappers"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Response shaping limits. Cell values larger than readChunkValueBytes are
// split across chunks with value_size set; responses are flushed at the
// first row boundary after readResponseBytes of cell data. A row never spans
// responses: the official Go client validates its chunk state machine at the
// end of every response. Tests lower the limits to exercise chunking.
var (
	readChunkValueBytes = 1 << 20
	readResponseBytes   = 1 << 20
)

// readPlan is the normalized set of rows a read touches.
type readPlan struct {
	keys     []string
	ranges   []keyRange
	all      bool
	reversed bool
}

func newReadPlan(rs *btpb.RowSet, reversed bool) readPlan {
	plan := readPlan{reversed: reversed}
	if rs == nil || len(rs.RowKeys)+len(rs.RowRanges) == 0 {
		plan.all = true
		return plan
	}
	for _, k := range rs.RowKeys {
		plan.keys = append(plan.keys, string(k))
	}
	for _, rr := range rs.RowRanges {
		plan.ranges = append(plan.ranges, rowRangeToKeyRange(rr))
	}
	return plan
}

func rowRangeToKeyRange(rr *btpb.RowRange) keyRange {
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

// load returns the planned rows, deduplicated and ordered by key in the
// requested direction.
func (p readPlan) load(ctx context.Context, rows *SqlRows) ([]*row, error) {
	if p.all {
		var out []*row
		err := rows.scan(ctx, nil, keyRange{}, p.reversed, func(r *row) (bool, error) {
			out = append(out, r)
			return true, nil
		})
		return out, err
	}
	seen := make(map[string]*row)
	for _, k := range p.keys {
		if _, ok := seen[k]; ok {
			continue
		}
		r, err := rows.load(ctx, nil, k)
		if err != nil {
			return nil, err
		}
		if r != nil {
			seen[k] = r
		}
	}
	for _, rng := range p.ranges {
		err := rows.scan(ctx, nil, rng, false, func(r *row) (bool, error) {
			if _, ok := seen[r.key]; !ok {
				seen[r.key] = r
			}
			return true, nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]*row, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	if p.reversed {
		sort.Sort(sort.Reverse(byRowKey(out)))
	} else {
		sort.Sort(byRowKey(out))
	}
	return out, nil
}

// readStats counts work done by one read for REQUEST_STATS_FULL.
type readStats struct {
	rowsSeen, rowsReturned, cellsSeen, cellsReturned int64
}

func (s *server) ReadRows(req *btpb.ReadRowsRequest, stream btpb.Bigtable_ReadRowsServer) error {
	start := time.Now()
	ctx := stream.Context()
	target, err := s.resolveReadTarget(req.GetTableName(), req.GetAuthorizedViewName(), req.GetMaterializedViewName(), req.GetAppProfileId())
	if err != nil {
		return err
	}
	if req.RowsLimit < 0 {
		return status.Error(codes.InvalidArgument, "rows_limit must not be negative")
	}
	if req.Reversed && isDataBoostProfile(target.appProfile) {
		return status.Error(codes.InvalidArgument, "reverse scans are not supported with Data Boost app profiles")
	}
	switch req.RequestStatsView {
	case btpb.ReadRowsRequest_REQUEST_STATS_VIEW_UNSPECIFIED, btpb.ReadRowsRequest_REQUEST_STATS_NONE, btpb.ReadRowsRequest_REQUEST_STATS_FULL:
	default:
		return status.Errorf(codes.InvalidArgument, "unknown request_stats_view %v", req.RequestStatsView)
	}
	if err := validateRowRanges(req); err != nil {
		return err
	}
	if err := validateFilter(req.Filter); err != nil {
		return err
	}
	if target.mv != nil {
		if err := target.mv.readable(); err != nil {
			return err
		}
	}

	tbl := target.tbl
	tbl.mu.RLock()
	gcRules := tbl.gcRulesNoLock()
	families := make(map[string]bool, len(tbl.families))
	for f := range tbl.families {
		families[f] = true
	}
	rows, err := newReadPlan(req.Rows, req.Reversed).load(ctx, tbl.rows)
	tbl.mu.RUnlock()
	if err != nil {
		return storageErr(err)
	}

	w := newRowWriter(stream)
	var stats readStats
	for _, stored := range rows {
		if req.RowsLimit > 0 && stats.rowsReturned >= req.RowsLimit {
			break
		}
		r := stored
		for f := range r.families {
			if !families[f] {
				delete(r.families, f)
			}
		}
		r.gc(gcRules)
		if target.view != nil {
			if !target.view.rowAllowed(r.key) {
				continue
			}
			r = target.view.restrict(r)
		}
		if r.isEmpty() {
			continue
		}
		stats.rowsSeen++
		stats.cellsSeen += int64(r.cellCount())
		if _, err := filterRow(req.Filter, r); err != nil {
			return err
		}
		if r.isEmpty() {
			continue
		}
		stats.rowsReturned++
		stats.cellsReturned += int64(r.cellCount())
		if err := w.writeRow(r); err != nil {
			return err
		}
	}
	if err := w.flush(); err != nil {
		return err
	}
	if req.RequestStatsView == btpb.ReadRowsRequest_REQUEST_STATS_FULL {
		return stream.Send(&btpb.ReadRowsResponse{RequestStats: &btpb.RequestStats{
			StatsView: &btpb.RequestStats_FullReadStatsView{FullReadStatsView: &btpb.FullReadStatsView{
				ReadIterationStats: &btpb.ReadIterationStats{
					RowsSeenCount:      stats.rowsSeen,
					RowsReturnedCount:  stats.rowsReturned,
					CellsSeenCount:     stats.cellsSeen,
					CellsReturnedCount: stats.cellsReturned,
				},
				RequestLatencyStats: &btpb.RequestLatencyStats{FrontendServerLatency: durationpb.New(time.Since(start))},
			}},
		}})
	}
	return nil
}

// rowWriter encodes rows as ReadRows cell chunks, batching rows into
// responses and splitting large cell values across chunks.
type rowWriter struct {
	stream btpb.Bigtable_ReadRowsServer
	resp   *btpb.ReadRowsResponse
	bytes  int
}

func newRowWriter(stream btpb.Bigtable_ReadRowsServer) *rowWriter {
	return &rowWriter{stream: stream, resp: &btpb.ReadRowsResponse{}}
}

func (w *rowWriter) writeRow(r *row) error {
	first := true
	for _, fam := range r.sortedFamilies() {
		newFamily := true
		for _, col := range fam.ColNames {
			newQualifier := true
			for _, c := range fam.Cells[col] {
				value := c.Value
				firstPiece := true
				for {
					piece := value
					if len(piece) > readChunkValueBytes {
						piece = value[:readChunkValueBytes]
					}
					chunk := &btpb.ReadRowsResponse_CellChunk{Value: piece}
					if firstPiece {
						if first {
							chunk.RowKey = []byte(r.key)
						}
						if newFamily {
							chunk.FamilyName = &wrappers.StringValue{Value: fam.Name}
						}
						if newFamily || newQualifier {
							chunk.Qualifier = &wrappers.BytesValue{Value: []byte(col)}
						}
						chunk.TimestampMicros = c.Ts
						chunk.Labels = c.Labels
					}
					if len(piece) < len(value) {
						chunk.ValueSize = int32(len(c.Value))
					}
					w.resp.Chunks = append(w.resp.Chunks, chunk)
					w.bytes += len(piece)
					first, newFamily, newQualifier, firstPiece = false, false, false, false
					value = value[len(piece):]
					if len(value) == 0 {
						break
					}
				}
			}
		}
	}
	last := w.resp.Chunks[len(w.resp.Chunks)-1]
	last.RowStatus = &btpb.ReadRowsResponse_CellChunk_CommitRow{CommitRow: true}
	if w.bytes >= readResponseBytes {
		return w.flush()
	}
	return nil
}

func (w *rowWriter) flush() error {
	if len(w.resp.Chunks) == 0 {
		return nil
	}
	err := w.stream.Send(w.resp)
	w.resp = &btpb.ReadRowsResponse{}
	w.bytes = 0
	return err
}

// streamRow filters the given row and sends it via the given stream.
// Returns true if at least one cell matched the filter and was streamed.
func streamRow(stream btpb.Bigtable_ReadRowsServer, r *row, f *btpb.RowFilter) (bool, error) {
	r = r.copy()
	match, err := filterRow(f, r)
	if err != nil || !match {
		return false, err
	}
	w := newRowWriter(stream)
	if err := w.writeRow(r); err != nil {
		return false, err
	}
	return true, w.flush()
}

// SampleRowKeys returns approximate split points with cumulative byte
// offsets. Samples include the table's initial split keys and one key per
// sampleEveryBytes of data, and always end with the last key in range.
func (s *server) SampleRowKeys(req *btpb.SampleRowKeysRequest, stream btpb.Bigtable_SampleRowKeysServer) error {
	ctx := stream.Context()
	target, err := s.resolveReadTarget(req.GetTableName(), req.GetAuthorizedViewName(), req.GetMaterializedViewName(), req.GetAppProfileId())
	if err != nil {
		return err
	}
	if target.mv != nil {
		if err := target.mv.readable(); err != nil {
			return err
		}
	}
	rng := keyRange{}
	if rr := req.GetRowRange(); rr != nil {
		rng = rowRangeToKeyRange(rr)
	}
	tbl := target.tbl
	tbl.mu.RLock()
	splits := append([]string(nil), tbl.initialSplits...)
	type sample struct {
		key    string
		offset int64
	}
	var samples []sample
	var offset, sinceLast int64
	var lastKey string
	nextSplit := 0
	err = tbl.rows.scan(ctx, nil, rng, false, func(r *row) (bool, error) {
		if target.view != nil {
			if !target.view.rowAllowed(r.key) {
				return true, nil
			}
			r = target.view.restrict(r)
			if r.isEmpty() {
				return true, nil
			}
		}
		for nextSplit < len(splits) && splits[nextSplit] <= r.key {
			if rng.contains(splits[nextSplit]) && splits[nextSplit] > lastKey {
				samples = append(samples, sample{splits[nextSplit], offset})
			}
			nextSplit++
		}
		size := int64(r.size())
		offset += size
		sinceLast += size
		lastKey = r.key
		if sinceLast >= sampleEveryBytes {
			samples = append(samples, sample{r.key, offset})
			sinceLast = 0
		}
		return true, nil
	})
	tbl.mu.RUnlock()
	if err != nil {
		return storageErr(err)
	}
	for ; nextSplit < len(splits); nextSplit++ {
		if rng.contains(splits[nextSplit]) && splits[nextSplit] > lastKey {
			samples = append(samples, sample{splits[nextSplit], offset})
		}
	}
	// The final sample is the end of the requested range: the range's end
	// key, or the empty key for "end of table".
	end := sample{offset: offset}
	if rng.end != "" {
		end.key = rng.end
		if rr := req.GetRowRange(); rr != nil {
			if ek, ok := rr.GetEndKey().(*btpb.RowRange_EndKeyClosed); ok {
				end.key = string(ek.EndKeyClosed)
			}
		}
	}
	for n := len(samples); n > 0 && samples[n-1].key == end.key; n-- {
		samples = samples[:n-1]
	}
	samples = append(samples, end)
	for i, smp := range samples {
		// Sample keys are strictly increasing: duplicate split keys, or a
		// periodic sample landing on a split key, keep the first (smaller)
		// offset.
		if i > 0 && smp.key == samples[i-1].key {
			continue
		}
		if err := stream.Send(&btpb.SampleRowKeysResponse{RowKey: []byte(smp.key), OffsetBytes: smp.offset}); err != nil {
			return err
		}
	}
	return nil
}

// sampleEveryBytes is the local sampling interval, standing in for tablet
// boundaries that a single-process store does not have.
var sampleEveryBytes int64 = 512 << 10

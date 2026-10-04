package bttest

// Conformance tests for RowFilter evaluation (google/bigtable/v2/data.proto
// RowFilter) and garbage-collection rules (google/bigtable/admin/v2/table.proto
// GcRule). The official Go client is used where it exposes the feature; Sink
// and ValueBitmask are not exposed by the client, so those cases call the
// handlers directly.

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// cflCell is one reassembled cell of a ReadRows response.
type cflCell struct {
	Row, Fam, Qual string
	Ts             int64
	Value          string
	Labels         string
}

func (c cflCell) String() string {
	return fmt.Sprintf("%s/%s:%s@%d=%q[%s]", c.Row, c.Fam, c.Qual, c.Ts, c.Value, c.Labels)
}

// cflNewTable creates table "t" with the given families on a fresh server
// whose instance is not registered (lenient admin mode).
func cflNewTable(t *testing.T, families map[string]*btapb.ColumnFamily) (*server, string) {
	t.Helper()
	s, parent := newFullTestServer(t)
	_, err := s.CreateTable(context.Background(), &btapb.CreateTableRequest{
		Parent:  parent,
		TableId: "t",
		Table:   &btapb.Table{ColumnFamilies: families},
	})
	require.NoError(t, err)
	return s, parent + "/tables/t"
}

func cflSet(fam, qual string, ts int64, value []byte) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
		FamilyName: fam, ColumnQualifier: []byte(qual), TimestampMicros: ts, Value: value,
	}}}
}

func cflMutate(t *testing.T, s *server, table, key string, muts ...*btpb.Mutation) {
	t.Helper()
	_, err := s.MutateRow(context.Background(), &btpb.MutateRowRequest{TableName: table, RowKey: []byte(key), Mutations: muts})
	require.NoError(t, err)
}

// cflRead runs ReadRows through the handler and reassembles cells, joining
// split values. It also returns the raw mock so callers can check that a
// rejected request streamed nothing.
func cflRead(s *server, req *btpb.ReadRowsRequest) ([]cflCell, *MockReadRowsServer, error) {
	mock := &MockReadRowsServer{}
	if err := s.ReadRows(req, mock); err != nil {
		return nil, mock, err
	}
	var out []cflCell
	var cur *cflCell
	for _, resp := range mock.responses {
		for _, ch := range resp.Chunks {
			if cur == nil {
				cur = &cflCell{
					Row:    string(ch.RowKey),
					Fam:    ch.FamilyName.GetValue(),
					Qual:   string(ch.Qualifier.GetValue()),
					Ts:     ch.TimestampMicros,
					Labels: strings.Join(ch.Labels, ","),
				}
			}
			cur.Value += string(ch.Value)
			if ch.ValueSize > 0 {
				continue
			}
			out = append(out, *cur)
			cur = nil
		}
	}
	return out, mock, nil
}

func cflReadFilter(t *testing.T, s *server, table string, f *btpb.RowFilter) []cflCell {
	t.Helper()
	cells, _, err := cflRead(s, &btpb.ReadRowsRequest{TableName: table, Filter: f})
	require.NoError(t, err)
	return cells
}

// cflSorted renders cells as sorted strings so tests can compare multisets
// (duplicate cells appear in an unspecified mutual order).
func cflSorted(cells []cflCell) []string {
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		out = append(out, c.String())
	}
	sort.Strings(out)
	return out
}

func cflRejected(t *testing.T, s *server, table string, f *btpb.RowFilter, want codes.Code) {
	t.Helper()
	_, mock, err := cflRead(s, &btpb.ReadRowsRequest{TableName: table, Filter: f})
	require.Error(t, err, "filter %v", f)
	require.Equal(t, want, status.Code(err), "filter %v: %v", f, err)
	require.Empty(t, mock.raw, "a rejected filter must not stream any response")
}

// Raw filter constructors for filters the Go client does not expose.
func cflChain(fs ...*btpb.RowFilter) *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_Chain_{Chain: &btpb.RowFilter_Chain{Filters: fs}}}
}

func cflInterleave(fs ...*btpb.RowFilter) *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_Interleave_{Interleave: &btpb.RowFilter_Interleave{Filters: fs}}}
}

func cflCondition(pred, yes, no *btpb.RowFilter) *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_Condition_{Condition: &btpb.RowFilter_Condition{
		PredicateFilter: pred, TrueFilter: yes, FalseFilter: no,
	}}}
}

func cflSink() *btpb.RowFilter { return &btpb.RowFilter{Filter: &btpb.RowFilter_Sink{Sink: true}} }
func cflPassAll() *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_PassAllFilter{PassAllFilter: true}}
}
func cflBlockAll() *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_BlockAllFilter{BlockAllFilter: true}}
}
func cflLabel(l string) *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_ApplyLabelTransformer{ApplyLabelTransformer: l}}
}
func cflStrip() *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_StripValueTransformer{StripValueTransformer: true}}
}
func cflFamilyRegex(p string) *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_FamilyNameRegexFilter{FamilyNameRegexFilter: p}}
}
func cflQualifierRegex(p string) *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_ColumnQualifierRegexFilter{ColumnQualifierRegexFilter: []byte(p)}}
}
func cflBitmask(mask []byte) *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_ValueBitmaskFilter{ValueBitmaskFilter: &btpb.ValueBitmask{Mask: mask}}}
}
func cflRowLimit(n int32) *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_CellsPerRowLimitFilter{CellsPerRowLimitFilter: n}}
}
func cflColumnLimit(n int32) *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_CellsPerColumnLimitFilter{CellsPerColumnLimitFilter: n}}
}
func cflRowOffset(n int32) *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_CellsPerRowOffsetFilter{CellsPerRowOffsetFilter: n}}
}

// cflClientCells flattens a Go client row into sorted strings.
func cflClientCells(r bigtable.Row) []string {
	var out []string
	for _, items := range r {
		for _, it := range items {
			out = append(out, fmt.Sprintf("%s@%d=%q[%s]", it.Column, it.Timestamp, it.Value, strings.Join(it.Labels, ",")))
		}
	}
	sort.Strings(out)
	return out
}

// TestConformanceFilterInterleaveDuplicatesAndLimits verifies, through the
// official Go client, that Interleave keeps identical cells produced by
// several branches and that cells_per_row_limit, cells_per_row_offset and
// cells_per_column_limit count each duplicate copy separately.
func TestConformanceFilterInterleaveDuplicatesAndLimits(t *testing.T) {
	env := setupTestEnv(t)
	tbl := env.createTable(t, "interleave", "fam")
	mut := bigtable.NewMutation()
	mut.Set("fam", "col", bigtable.Timestamp(2000), []byte("v2"))
	mut.Set("fam", "col", bigtable.Timestamp(1000), []byte("v1"))
	mut.Set("fam", "other", bigtable.Timestamp(1000), []byte("o"))
	require.NoError(t, tbl.Apply(env.ctx, "r", mut))

	read := func(f bigtable.Filter) []string {
		t.Helper()
		r, err := tbl.ReadRow(env.ctx, "r", bigtable.RowFilter(f))
		require.NoError(t, err)
		return cflClientCells(r)
	}
	dup := bigtable.InterleaveFilters(bigtable.PassAllFilter(), bigtable.PassAllFilter())

	require.Equal(t, []string{
		`fam:col@1000="v1"[]`, `fam:col@1000="v1"[]`,
		`fam:col@2000="v2"[]`, `fam:col@2000="v2"[]`,
		`fam:other@1000="o"[]`, `fam:other@1000="o"[]`,
	}, read(dup), "Interleave must return every copy of identical cells")

	// cells_per_column_limit=1 keeps one copy of the newest cell per column.
	require.Equal(t, []string{`fam:col@2000="v2"[]`, `fam:other@1000="o"[]`},
		read(bigtable.ChainFilters(dup, bigtable.LatestNFilter(1))))
	// cells_per_column_limit=2 is consumed by the two copies of the newest
	// cell, so the older version of fam:col is excluded.
	require.Equal(t, []string{
		`fam:col@2000="v2"[]`, `fam:col@2000="v2"[]`,
		`fam:other@1000="o"[]`, `fam:other@1000="o"[]`,
	}, read(bigtable.ChainFilters(dup, bigtable.LatestNFilter(2))))
	// cells_per_row_limit=3: col@2000 x2, col@1000 x1.
	require.Equal(t, []string{`fam:col@1000="v1"[]`, `fam:col@2000="v2"[]`, `fam:col@2000="v2"[]`},
		read(bigtable.ChainFilters(dup, bigtable.CellsPerRowLimitFilter(3))))
	// cells_per_row_offset=3 skips col@2000 x2 and one copy of col@1000.
	require.Equal(t, []string{`fam:col@1000="v1"[]`, `fam:other@1000="o"[]`, `fam:other@1000="o"[]`},
		read(bigtable.ChainFilters(dup, bigtable.CellsPerRowOffsetFilter(3))))

	// Labels from separate Interleave branches land on separate copies.
	require.Equal(t, []string{
		`fam:col@1000="v1"[a]`, `fam:col@1000="v1"[b]`,
		`fam:col@2000="v2"[a]`, `fam:col@2000="v2"[b]`,
		`fam:other@1000="o"[a]`, `fam:other@1000="o"[b]`,
	}, read(bigtable.InterleaveFilters(bigtable.LabelFilter("a"), bigtable.LabelFilter("b"))))
}

// TestConformanceFilterInterleaveDocExample reproduces the data.proto
// Interleave example: identical cells from different branches all appear.
func TestConformanceFilterInterleaveDocExample(t *testing.T) {
	s, table := cflNewTable(t, map[string]*btapb.ColumnFamily{"far": {}, "foo": {}})
	cflMutate(t, s, table, "r",
		cflSet("foo", "bar", 10000, []byte("x")),
		cflSet("foo", "blah", 11000, []byte("z")),
		cflSet("far", "bar", 7000, []byte("a")),
		cflSet("far", "blah", 5000, []byte("x")),
	)
	// f(0) = foo family, f(1) = far:blah, f(2) = far family.
	f := cflInterleave(
		cflFamilyRegex("foo"),
		cflChain(cflFamilyRegex("far"), cflQualifierRegex("blah")),
		cflFamilyRegex("far"),
	)
	require.Equal(t, []string{
		`r/far:bar@7000="a"[]`,
		`r/far:blah@5000="x"[]`,
		`r/far:blah@5000="x"[]`,
		`r/foo:bar@10000="x"[]`,
		`r/foo:blah@11000="z"[]`,
	}, cflSorted(cflReadFilter(t, s, table, f)))
}

// TestConformanceFilterSinkDocExample reproduces the data.proto Sink example
// exactly: Chain(FamilyRegex("A"), Interleave(All(), Chain(Label("foo"),
// Sink())), QualifierRegex("B")).
func TestConformanceFilterSinkDocExample(t *testing.T) {
	s, table := cflNewTable(t, map[string]*btapb.ColumnFamily{"A": {}, "B": {}})
	cflMutate(t, s, table, "r",
		cflSet("A", "A", 1000, []byte("w")),
		cflSet("A", "B", 2000, []byte("x")),
		cflSet("B", "B", 4000, []byte("z")),
	)
	docFilter := cflChain(
		cflFamilyRegex("A"),
		cflInterleave(cflPassAll(), cflChain(cflLabel("foo"), cflSink())),
		cflQualifierRegex("B"),
	)
	got := cflReadFilter(t, s, table, docFilter)
	require.Equal(t, []string{
		`r/A:A@1000="w"[foo]`,
		`r/A:B@2000="x"[]`,
		`r/A:B@2000="x"[foo]`,
	}, cflSorted(got), "every cell reaching the sink is in the result despite the qualifier filter")
	// Cells are still streamed in column order: A:A before both A:B copies.
	require.Len(t, got, 3)
	require.Equal(t, "A", got[0].Qual)

	// A top-level Sink returns every cell; nothing reaches later chain steps,
	// so the strip_value_transformer after it never sees a cell.
	require.Equal(t, []string{
		`r/A:A@1000="w"[]`, `r/A:B@2000="x"[]`, `r/B:B@4000="z"[]`,
	}, cflSorted(cflReadFilter(t, s, table, cflChain(cflSink(), cflStrip()))))

	// Sink output bypasses limits applied by its parents.
	require.Equal(t, []string{
		`r/A:A@1000="w"[]`,
		`r/A:A@1000="w"[s]`, `r/A:B@2000="x"[s]`, `r/B:B@4000="z"[s]`,
	}, cflSorted(cflReadFilter(t, s, table, cflChain(
		cflInterleave(cflPassAll(), cflChain(cflLabel("s"), cflSink())),
		cflRowLimit(1),
	))))
}

// TestConformanceFilterSinkInsideConditionRejected verifies that Sink is
// rejected anywhere inside a Condition's predicate, true or false filter, for
// both ReadRows and CheckAndMutateRow, before any data is read or written.
func TestConformanceFilterSinkInsideConditionRejected(t *testing.T) {
	s, table := cflNewTable(t, map[string]*btapb.ColumnFamily{"cf": {}})
	cflMutate(t, s, table, "r", cflSet("cf", "q", 1000, []byte("v")))

	for name, f := range map[string]*btpb.RowFilter{
		"predicate":            cflCondition(cflSink(), cflPassAll(), nil),
		"true_filter":          cflCondition(cflPassAll(), cflChain(cflLabel("x"), cflSink()), nil),
		"false_filter":         cflCondition(cflBlockAll(), nil, cflInterleave(cflPassAll(), cflSink())),
		"nested in chain":      cflChain(cflPassAll(), cflInterleave(cflPassAll(), cflCondition(cflPassAll(), cflSink(), nil))),
		"sink must be true":    {Filter: &btpb.RowFilter_Sink{Sink: false}},
		"condition in a chain": cflChain(cflCondition(cflChain(cflPassAll(), cflSink()), nil, nil), cflPassAll()),
	} {
		t.Run(name, func(t *testing.T) { cflRejected(t, s, table, f, codes.InvalidArgument) })
	}

	before := storedRow(t, s.tables[table], "r")
	_, err := s.CheckAndMutateRow(context.Background(), &btpb.CheckAndMutateRowRequest{
		TableName:       table,
		RowKey:          []byte("r"),
		PredicateFilter: cflCondition(cflSink(), nil, nil),
		TrueMutations:   []*btpb.Mutation{cflSet("cf", "q", 2000, []byte("changed"))},
		FalseMutations:  []*btpb.Mutation{cflSet("cf", "q", 3000, []byte("changed"))},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	require.Equal(t, cflDump(before), cflDump(storedRow(t, s.tables[table], "r")))

	// A Sink outside any Condition is a valid read filter.
	require.Len(t, cflReadFilter(t, s, table, cflSink()), 1)
}

// TestConformanceFilterValueBitmask verifies value_bitmask_filter:
// (value & mask) == mask, a length mismatch never matches, and an empty mask
// is rejected.
func TestConformanceFilterValueBitmask(t *testing.T) {
	s, table := cflNewTable(t, map[string]*btapb.ColumnFamily{"cf": {}})
	cflMutate(t, s, table, "r",
		cflSet("cf", "match", 1000, []byte{0x0F}),
		cflSet("cf", "allones", 1000, []byte{0xFF}),
		cflSet("cf", "mismatch", 1000, []byte{0x0A}),
		cflSet("cf", "long", 1000, []byte{0x0F, 0x0F}),
		cflSet("cf", "empty", 1000, nil),
	)
	quals := func(f *btpb.RowFilter) []string {
		var out []string
		for _, c := range cflReadFilter(t, s, table, f) {
			out = append(out, c.Qual)
		}
		sort.Strings(out)
		return out
	}
	require.Equal(t, []string{"allones", "match"}, quals(cflBitmask([]byte{0x05})))
	require.Equal(t, []string{"allones"}, quals(cflBitmask([]byte{0xF0})))
	require.Equal(t, []string{"long"}, quals(cflBitmask([]byte{0x0F, 0x0F})),
		"the mask length must equal the value length")
	require.Equal(t, []string{"long"}, quals(cflBitmask([]byte{0x00, 0x01})))
	require.Empty(t, quals(cflBitmask([]byte{0x00, 0x00, 0x00})))

	cflRejected(t, s, table, cflBitmask(nil), codes.InvalidArgument)
	cflRejected(t, s, table, cflBitmask([]byte{}), codes.InvalidArgument)
	cflRejected(t, s, table, &btpb.RowFilter{Filter: &btpb.RowFilter_ValueBitmaskFilter{}}, codes.InvalidArgument)

	// The bitmask also works as a CheckAndMutateRow predicate.
	resp, err := s.CheckAndMutateRow(context.Background(), &btpb.CheckAndMutateRowRequest{
		TableName:       table,
		RowKey:          []byte("r"),
		PredicateFilter: cflChain(cflQualifierRegex("mismatch"), cflBitmask([]byte{0x08})),
		TrueMutations:   []*btpb.Mutation{cflSet("cf", "hit", 1000, []byte("t"))},
		FalseMutations:  []*btpb.Mutation{cflSet("cf", "miss", 1000, []byte("f"))},
	})
	require.NoError(t, err)
	require.True(t, resp.PredicateMatched)
	r := storedRow(t, s.tables[table], "r")
	require.Contains(t, r.families["cf"].Cells, "hit")
	require.NotContains(t, r.families["cf"].Cells, "miss")
}

// TestConformanceFilterLabelValidation verifies apply_label_transformer rules
// through the official Go client: at most 15 characters of [a-z0-9-], and a
// Chain may contain at most one sub-filter with a label transformer, while an
// Interleave may contain several.
func TestConformanceFilterLabelValidation(t *testing.T) {
	env := setupTestEnv(t)
	tbl := env.createTable(t, "labels", "fam")
	mut := bigtable.NewMutation()
	mut.Set("fam", "col", bigtable.Timestamp(1000), []byte("v"))
	require.NoError(t, tbl.Apply(env.ctx, "r", mut))

	read := func(f bigtable.Filter) ([]string, error) {
		r, err := tbl.ReadRow(env.ctx, "r", bigtable.RowFilter(f))
		if err != nil {
			return nil, err
		}
		return cflClientCells(r), nil
	}

	got, err := read(bigtable.LabelFilter("abcdefghijklm-5"))
	require.NoError(t, err)
	require.Equal(t, []string{`fam:col@1000="v"[abcdefghijklm-5]`}, got, "15 characters is the maximum label length")

	got, err = read(bigtable.InterleaveFilters(bigtable.LabelFilter("a"), bigtable.LabelFilter("b")))
	require.NoError(t, err)
	require.Equal(t, []string{`fam:col@1000="v"[a]`, `fam:col@1000="v"[b]`}, got)

	got, err = read(bigtable.ChainFilters(
		bigtable.InterleaveFilters(bigtable.LabelFilter("a"), bigtable.LabelFilter("b")),
		bigtable.LatestNFilter(5),
	))
	require.NoError(t, err)
	require.Len(t, got, 2, "one Chain sub-filter may hold several labels inside an Interleave")

	for name, f := range map[string]bigtable.Filter{
		"16 characters":         bigtable.LabelFilter("abcdefghijklmnop"),
		"uppercase":             bigtable.LabelFilter("Label"),
		"underscore":            bigtable.LabelFilter("a_b"),
		"empty":                 bigtable.LabelFilter(""),
		"two labels in a chain": bigtable.ChainFilters(bigtable.LabelFilter("a"), bigtable.LabelFilter("b")),
		"label nested in a second chain element": bigtable.ChainFilters(
			bigtable.LabelFilter("a"),
			bigtable.InterleaveFilters(bigtable.PassAllFilter(), bigtable.LabelFilter("b")),
		),
		"label in a condition branch": bigtable.ChainFilters(
			bigtable.LabelFilter("a"),
			bigtable.ConditionFilter(bigtable.PassAllFilter(), bigtable.LabelFilter("b"), nil),
		),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := read(f)
			require.Error(t, err)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
		})
	}
}

// TestConformanceFilterValidationBeforeRead verifies that malformed filters
// fail with InvalidArgument before any row is streamed.
func TestConformanceFilterValidationBeforeRead(t *testing.T) {
	s, table := cflNewTable(t, map[string]*btapb.ColumnFamily{"cf": {}})
	cflMutate(t, s, table, "a", cflSet("cf", "q", 1000, []byte("v")))
	cflMutate(t, s, table, "z", cflSet("cf", "q", 1000, []byte("v")))

	deep := cflPassAll()
	for i := 0; i < maxFilterDepth+1; i++ {
		deep = cflChain(deep, cflPassAll())
	}
	for name, f := range map[string]*btpb.RowFilter{
		"negative cells_per_row_limit":    cflRowLimit(-1),
		"negative cells_per_row_offset":   cflRowOffset(-1),
		"negative cells_per_column_limit": cflColumnLimit(-1),
		"nesting deeper than 20":          deep,
		"bad label inside a condition":    cflCondition(cflPassAll(), cflLabel("BAD"), nil),
		"microsecond timestamp range": {Filter: &btpb.RowFilter_TimestampRangeFilter{
			TimestampRangeFilter: &btpb.TimestampRange{StartTimestampMicros: 1}}},
	} {
		t.Run(name, func(t *testing.T) { cflRejected(t, s, table, f, codes.InvalidArgument) })
	}
}

// cflDump renders a stored row deterministically for equality checks.
func cflDump(r *row) []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, fam := range r.families {
		for col, cs := range fam.Cells {
			for _, c := range cs {
				out = append(out, fmt.Sprintf("%s:%s@%d=%q%v", fam.Name, col, c.Ts, c.Value, c.Labels))
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestConformanceFilterNeverMutatesStoredRows verifies that transformers,
// limits, conditions, interleaves and sinks only change the response and
// never the stored row, and that a Condition predicate's transformations do
// not leak into the chosen branch.
func TestConformanceFilterNeverMutatesStoredRows(t *testing.T) {
	s, table := cflNewTable(t, map[string]*btapb.ColumnFamily{"a": {}, "b": {}})
	cflMutate(t, s, table, "r",
		cflSet("a", "q1", 3000, []byte("a3")),
		cflSet("a", "q1", 2000, []byte("a2")),
		cflSet("a", "q2", 1000, []byte("a1")),
		cflSet("b", "q1", 1000, []byte("b1")),
	)
	tbl := s.tables[table]
	want := cflDump(storedRow(t, tbl, "r"))
	require.Len(t, want, 4)
	unfiltered := cflSorted(cflReadFilter(t, s, table, nil))

	filters := map[string]*btpb.RowFilter{
		"strip_value":            cflStrip(),
		"apply_label":            cflLabel("lbl"),
		"cells_per_column_limit": cflColumnLimit(1),
		"cells_per_row_limit":    cflRowLimit(1),
		"cells_per_row_offset":   cflRowOffset(2),
		"interleave duplicates":  cflInterleave(cflPassAll(), cflLabel("dup")),
		"sink":                   cflChain(cflInterleave(cflStrip(), cflChain(cflLabel("s"), cflSink())), cflBlockAll()),
		"condition":              cflCondition(cflChain(cflStrip(), cflLabel("p")), cflChain(cflStrip(), cflLabel("t")), cflLabel("f")),
		"value_bitmask":          cflBitmask([]byte("a")),
		"block_all":              cflBlockAll(),
	}
	for name, f := range filters {
		cflReadFilter(t, s, table, f)
		require.Equal(t, want, cflDump(storedRow(t, tbl, "r")), "stored row changed by %s", name)
		require.Equal(t, unfiltered, cflSorted(cflReadFilter(t, s, table, nil)), "unfiltered read changed after %s", name)
	}

	// The predicate sees a private copy: its strip_value and label do not
	// reach the true branch output.
	got := cflReadFilter(t, s, table, cflCondition(cflChain(cflStrip(), cflLabel("p")), cflPassAll(), nil))
	require.Equal(t, unfiltered, cflSorted(got))
	// A missing branch yields no cells.
	require.Empty(t, cflReadFilter(t, s, table, cflCondition(cflPassAll(), nil, cflPassAll())))
	require.Equal(t, unfiltered, cflSorted(cflReadFilter(t, s, table, cflCondition(cflBlockAll(), nil, cflPassAll()))))

	// A CheckAndMutateRow predicate with transformers leaves the row as it was
	// apart from the chosen branch's mutation.
	resp, err := s.CheckAndMutateRow(context.Background(), &btpb.CheckAndMutateRowRequest{
		TableName:       table,
		RowKey:          []byte("r"),
		PredicateFilter: cflChain(cflStrip(), cflLabel("p")),
		TrueMutations:   []*btpb.Mutation{cflSet("b", "q2", 1000, []byte("new"))},
	})
	require.NoError(t, err)
	require.True(t, resp.PredicateMatched)
	after := cflDump(storedRow(t, tbl, "r"))
	require.Equal(t, append(append([]string(nil), want...), `b:q2@1000="new"[]`), cflSortedCopy(after))
}

func cflSortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// cflAges are cell ages, newest first, used by the GC truth tables.
var cflAges = []time.Duration{0, 2 * time.Hour, 30 * time.Hour, 50 * time.Hour, 70 * time.Hour}

func cflMaxAge(d time.Duration) *btapb.GcRule {
	return &btapb.GcRule{Rule: &btapb.GcRule_MaxAge{MaxAge: durationpb.New(d)}}
}

func cflMaxVersions(n int32) *btapb.GcRule {
	return &btapb.GcRule{Rule: &btapb.GcRule_MaxNumVersions{MaxNumVersions: n}}
}

func cflUnion(rs ...*btapb.GcRule) *btapb.GcRule {
	return &btapb.GcRule{Rule: &btapb.GcRule_Union_{Union: &btapb.GcRule_Union{Rules: rs}}}
}

func cflIntersection(rs ...*btapb.GcRule) *btapb.GcRule {
	return &btapb.GcRule{Rule: &btapb.GcRule_Intersection_{Intersection: &btapb.GcRule_Intersection{Rules: rs}}}
}

// cflGCCase is one row of the GC truth table. keep lists the indexes of
// cflAges that survive.
type cflGCCase struct {
	name   string
	rule   *btapb.GcRule
	client bigtable.GCPolicy
	keep   []int
}

// Per-cell deletion verdicts for the five cflAges cells (T = delete):
//
//	max_num_versions=2: F F T T T     max_age=1h:  F T T T T
//	max_num_versions=4: F F F F T     max_age=40h: F F F T T
//	max_num_versions=1: F T T T T     max_age=60h: F F F F T
func cflGCCases() []cflGCCase {
	h := time.Hour
	return []cflGCCase{
		{"max_num_versions=2", cflMaxVersions(2), bigtable.MaxVersionsPolicy(2), []int{0, 1}},
		{"max_age=1h", cflMaxAge(h), bigtable.MaxAgePolicy(h), []int{0}},
		{"union(versions=4, age=40h)", cflUnion(cflMaxVersions(4), cflMaxAge(40*h)),
			bigtable.UnionPolicy(bigtable.MaxVersionsPolicy(4), bigtable.MaxAgePolicy(40*h)), []int{0, 1, 2}},
		{"intersection(versions=2, age=1h)", cflIntersection(cflMaxVersions(2), cflMaxAge(h)),
			bigtable.IntersectionPolicy(bigtable.MaxVersionsPolicy(2), bigtable.MaxAgePolicy(h)), []int{0, 1}},
		{"intersection(versions=1, age=40h)", cflIntersection(cflMaxVersions(1), cflMaxAge(40*h)),
			bigtable.IntersectionPolicy(bigtable.MaxVersionsPolicy(1), bigtable.MaxAgePolicy(40*h)), []int{0, 1, 2}},
		{"union(intersection(versions=1, age=40h), intersection(versions=4, age=1h))",
			cflUnion(cflIntersection(cflMaxVersions(1), cflMaxAge(40*h)), cflIntersection(cflMaxVersions(4), cflMaxAge(h))),
			bigtable.UnionPolicy(
				bigtable.IntersectionPolicy(bigtable.MaxVersionsPolicy(1), bigtable.MaxAgePolicy(40*h)),
				bigtable.IntersectionPolicy(bigtable.MaxVersionsPolicy(4), bigtable.MaxAgePolicy(h))),
			[]int{0, 1, 2}},
		{"intersection(union(versions=4, age=1h), union(versions=2, age=60h))",
			cflIntersection(cflUnion(cflMaxVersions(4), cflMaxAge(h)), cflUnion(cflMaxVersions(2), cflMaxAge(60*h))),
			bigtable.IntersectionPolicy(
				bigtable.UnionPolicy(bigtable.MaxVersionsPolicy(4), bigtable.MaxAgePolicy(h)),
				bigtable.UnionPolicy(bigtable.MaxVersionsPolicy(2), bigtable.MaxAgePolicy(60*h))),
			[]int{0, 1}},
		{"intersection(age=1h, union(versions=4, intersection(versions=3, age=60h)))",
			cflIntersection(cflMaxAge(h), cflUnion(cflMaxVersions(4), cflIntersection(cflMaxVersions(3), cflMaxAge(60*h)))),
			bigtable.IntersectionPolicy(bigtable.MaxAgePolicy(h), bigtable.UnionPolicy(bigtable.MaxVersionsPolicy(4),
				bigtable.IntersectionPolicy(bigtable.MaxVersionsPolicy(3), bigtable.MaxAgePolicy(60*h)))),
			[]int{0, 1, 2, 3}},
		{"single-element union", cflUnion(cflMaxVersions(2)),
			bigtable.UnionPolicy(bigtable.MaxVersionsPolicy(2)), []int{0, 1}},
		{"single-element intersection", cflIntersection(cflMaxAge(40 * h)),
			bigtable.IntersectionPolicy(bigtable.MaxAgePolicy(40 * h)), []int{0, 1, 2}},
	}
}

// TestConformanceGCRuleTruthTables checks union and intersection semantics,
// including nested rules, at the unit level: a union deletes a cell when any
// child would delete it, an intersection only when every child would, and
// every child judges the same input column.
func TestConformanceGCRuleTruthTables(t *testing.T) {
	now := int64(1_000_000) * int64(time.Hour/time.Microsecond)
	cells := make([]cell, len(cflAges))
	for i, age := range cflAges {
		cells[i] = cell{Ts: now - age.Microseconds(), Value: []byte{byte(i)}}
	}
	for _, tc := range cflGCCases() {
		t.Run(tc.name, func(t *testing.T) {
			kept, removed := applyGCAt(append([]cell(nil), cells...), tc.rule, now)
			var keptIdx []int
			for _, c := range kept {
				keptIdx = append(keptIdx, int(c.Value[0]))
			}
			require.Equal(t, tc.keep, keptIdx)
			require.Len(t, removed, len(cells)-len(tc.keep))
		})
	}
	// Rules that delete nothing.
	for name, rule := range map[string]*btapb.GcRule{
		"nil rule":           nil,
		"unset rule":         {},
		"empty union":        cflUnion(),
		"empty intersection": cflIntersection(),
	} {
		kept, removed := applyGCAt(append([]cell(nil), cells...), rule, now)
		require.Len(t, kept, len(cells), name)
		require.Empty(t, removed, name)
	}
	// max_age deletes only cells strictly older than the age.
	edge := []cell{{Ts: now - time.Hour.Microseconds()}, {Ts: now - time.Hour.Microseconds() - 1000}}
	kept, removed := applyGCAt(edge, cflMaxAge(time.Hour), now)
	require.Len(t, kept, 1)
	require.Len(t, removed, 1)
	require.Equal(t, edge[1].Ts, removed[0].Ts)
}

// TestConformanceGCRulesEndToEnd writes the truth-table cells through the
// official Go client into one column family per rule (GC policies set by the
// admin client) and checks which versions a subsequent read returns.
func TestConformanceGCRulesEndToEnd(t *testing.T) {
	env := setupTestEnv(t)
	cases := cflGCCases()
	families := map[string]bigtable.Family{}
	for i, tc := range cases {
		families[fmt.Sprintf("f%d", i)] = bigtable.Family{GCPolicy: tc.client}
	}
	require.NoError(t, env.admin.CreateTableFromConf(env.ctx, &bigtable.TableConf{TableID: "gc", ColumnFamilies: families}))
	info, err := env.admin.TableInfo(env.ctx, "gc")
	require.NoError(t, err)
	require.Len(t, info.FamilyInfos, len(cases))

	now := time.Now().Truncate(time.Millisecond)
	stamp := func(i int) bigtable.Timestamp { return bigtable.Time(now.Add(-cflAges[i])) }
	tbl := env.client.Open("gc")
	mut := bigtable.NewMutation()
	for i := range cases {
		for j := range cflAges {
			mut.Set(fmt.Sprintf("f%d", i), "col", stamp(j), []byte{byte(j)})
		}
	}
	require.NoError(t, tbl.Apply(env.ctx, "r", mut))

	r, err := tbl.ReadRow(env.ctx, "r")
	require.NoError(t, err)
	for i, tc := range cases {
		var got []int
		for _, it := range r[fmt.Sprintf("f%d", i)] {
			got = append(got, int(it.Value[0]))
		}
		require.Equal(t, tc.keep, got, "family f%d %s", i, tc.name)
	}

	// Tightening a policy with SetGCPolicy (ModifyColumnFamilies update) takes
	// effect for subsequent reads.
	require.NoError(t, env.admin.SetGCPolicy(env.ctx, "gc", "f0", bigtable.IntersectionPolicy(
		bigtable.MaxVersionsPolicy(1), bigtable.MaxAgePolicy(time.Hour))))
	r, err = tbl.ReadRow(env.ctx, "r", bigtable.RowFilter(bigtable.FamilyFilter("f0")))
	require.NoError(t, err)
	require.Len(t, r["f0"], 1)
	require.Equal(t, stamp(0), r["f0"][0].Timestamp)
}

// TestConformanceGCPersistedOnWrite verifies GC runs on the written row: the
// stored row only holds surviving cells, and a policy tightened later hides
// cells on read without rewriting storage until the row is next written.
func TestConformanceGCPersistedOnWrite(t *testing.T) {
	rule := cflIntersection(cflMaxVersions(2), cflMaxAge(time.Hour))
	s, table := cflNewTable(t, map[string]*btapb.ColumnFamily{"cf": {GcRule: rule}})
	tbl := s.tables[table]
	now := time.Now().Truncate(time.Millisecond)
	var muts []*btpb.Mutation
	for i, age := range cflAges {
		muts = append(muts, cflSet("cf", "q", now.Add(-age).UnixMicro(), []byte{byte(i)}))
	}
	cflMutate(t, s, table, "r", muts...)
	stored := storedRow(t, tbl, "r")
	require.Len(t, stored.families["cf"].Cells["q"], 2, "write must persist the collected row")
	require.Len(t, cflReadFilter(t, s, table, nil), 2)

	_, err := s.ModifyColumnFamilies(context.Background(), &btapb.ModifyColumnFamiliesRequest{
		Name: table,
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{{
			Id:  "cf",
			Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Update{Update: &btapb.ColumnFamily{GcRule: cflMaxVersions(1)}},
		}},
	})
	require.NoError(t, err)
	got := cflReadFilter(t, s, table, nil)
	require.Len(t, got, 1)
	require.Equal(t, now.UnixMicro(), got[0].Ts)
	require.Len(t, storedRow(t, tbl, "r").families["cf"].Cells["q"], 2, "reads apply GC to a copy only")

	cflMutate(t, s, table, "r", cflSet("cf", "other", now.UnixMicro(), []byte("x")))
	require.Len(t, storedRow(t, tbl, "r").families["cf"].Cells["q"], 1, "the next write persists the tightened policy")
}

// cflOversizedUnion returns the largest union of max_num_versions rules that
// serializes to at most 500 bytes, and one that serializes to more.
func cflOversizedUnion() (fits, tooBig *btapb.GcRule) {
	var rules []*btapb.GcRule
	for {
		next := cflUnion(append(append([]*btapb.GcRule(nil), rules...), cflMaxVersions(1))...)
		if proto.Size(next) > 500 {
			return cflUnion(rules...), next
		}
		rules = append(rules, cflMaxVersions(1))
	}
}

// TestConformanceGCRuleValidation verifies that GC rules with max_age below
// one millisecond (at any nesting depth) or serializing to more than 500
// bytes are rejected by CreateTable and ModifyColumnFamilies, and that a
// rejected ModifyColumnFamilies changes nothing.
func TestConformanceGCRuleValidation(t *testing.T) {
	ctx := context.Background()
	fits, tooBig := cflOversizedUnion()
	require.LessOrEqual(t, proto.Size(fits), 500)
	require.Greater(t, proto.Size(tooBig), 500)

	bad := map[string]*btapb.GcRule{
		"max_age 999us":               cflMaxAge(999 * time.Microsecond),
		"max_age 0":                   cflMaxAge(0),
		"nested max_age 500us":        cflUnion(cflMaxVersions(1), cflIntersection(cflMaxVersions(2), cflMaxAge(500*time.Microsecond))),
		"rule larger than 500 bytes":  tooBig,
		"nested rule over 500 bytes":  cflIntersection(cflMaxAge(time.Hour), tooBig),
		"max_age negative":            cflMaxAge(-time.Second),
		"max_age 1ms minus a nanosec": cflMaxAge(time.Millisecond - time.Nanosecond),
	}
	good := map[string]*btapb.GcRule{
		"max_age 1ms":          cflMaxAge(time.Millisecond),
		"rule of 500 bytes":    fits,
		"nested max_age 1ms":   cflIntersection(cflMaxVersions(1), cflUnion(cflMaxAge(time.Millisecond))),
		"max_age sub-ms extra": cflMaxAge(time.Millisecond + time.Microsecond),
	}

	s, parent := newFullTestServer(t)
	for name, rule := range bad {
		_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "bad", Table: &btapb.Table{
			ColumnFamilies: map[string]*btapb.ColumnFamily{"cf": {GcRule: rule}},
		}})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "CreateTable %s: %v", name, err)
	}
	require.NotContains(t, s.tables, parent+"/tables/bad")

	i := 0
	for name, rule := range good {
		i++
		_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: fmt.Sprintf("good%d", i), Table: &btapb.Table{
			ColumnFamilies: map[string]*btapb.ColumnFamily{"cf": {GcRule: rule}},
		}})
		require.NoError(t, err, "CreateTable %s", name)
	}

	_, err := s.CreateTable(ctx, &btapb.CreateTableRequest{Parent: parent, TableId: "t", Table: &btapb.Table{
		ColumnFamilies: map[string]*btapb.ColumnFamily{"cf": {GcRule: cflMaxVersions(3)}},
	}})
	require.NoError(t, err)
	table := parent + "/tables/t"
	schemaBefore, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: table, View: btapb.Table_FULL})
	require.NoError(t, err)
	for name, rule := range bad {
		for _, mod := range []*btapb.ModifyColumnFamiliesRequest_Modification{
			{Id: "new", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{GcRule: rule}}},
			{Id: "cf", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Update{Update: &btapb.ColumnFamily{GcRule: rule}}},
		} {
			_, err := s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{
				Name: table,
				// A valid modification first: the request must still apply atomically.
				Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{
					{Id: "ok", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}}},
					mod,
				},
			})
			require.Equal(t, codes.InvalidArgument, status.Code(err), "ModifyColumnFamilies %s %T: %v", name, mod.Mod, err)
		}
	}
	schemaAfter, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: table, View: btapb.Table_FULL})
	require.NoError(t, err)
	require.True(t, proto.Equal(schemaBefore.ColumnFamilies["cf"], schemaAfter.ColumnFamilies["cf"]))
	require.True(t, reflect.DeepEqual(cflFamilyIDs(schemaBefore), cflFamilyIDs(schemaAfter)), "rejected requests must not add families")

	for name, rule := range good {
		_, err := s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{
			Name: table,
			Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{
				{Id: "cf", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Update{Update: &btapb.ColumnFamily{GcRule: rule}}},
			},
		})
		require.NoError(t, err, "ModifyColumnFamilies %s", name)
	}

	// The official admin client hits the same validation.
	env := setupTestEnv(t)
	require.NoError(t, env.admin.CreateTable(env.ctx, "gcclient"))
	err = env.admin.CreateColumnFamilyWithConfig(env.ctx, "gcclient", "cf", bigtable.Family{GCPolicy: bigtable.MaxAgePolicy(500 * time.Microsecond)})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	require.NoError(t, env.admin.CreateColumnFamily(env.ctx, "gcclient", "cf"))
	err = env.admin.SetGCPolicy(env.ctx, "gcclient", "cf", bigtable.UnionPolicy(bigtable.MaxVersionsPolicy(1), bigtable.MaxAgePolicy(time.Microsecond)))
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	err = env.admin.CreateTableFromConf(env.ctx, &bigtable.TableConf{TableID: "gcclient2", ColumnFamilies: map[string]bigtable.Family{
		"cf": {GCPolicy: bigtable.MaxAgePolicy(100 * time.Microsecond)},
	}})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	require.NoError(t, env.admin.SetGCPolicy(env.ctx, "gcclient", "cf", bigtable.MaxAgePolicy(time.Millisecond)))
}

func cflFamilyIDs(tbl *btapb.Table) []string {
	var ids []string
	for id := range tbl.ColumnFamilies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

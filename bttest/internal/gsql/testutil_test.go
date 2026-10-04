package gsql

import (
	"bytes"
	"context"
	"encoding/binary"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// memCatalog is an in-memory Catalog for tests.
type memCatalog struct {
	tables  map[string]*memTable
	views   map[string]string
	mvs     map[string]bool // used by mvCatalog
	mu      sync.Mutex
	scans   []keyRange
	visited int
}

type memTable struct {
	t    *Table
	rows map[string]*Row
}

func newMemCatalog() *memCatalog {
	return &memCatalog{tables: map[string]*memTable{}, views: map[string]string{}}
}

func (c *memCatalog) addTable(t *Table) *memTable {
	mt := &memTable{t: t, rows: map[string]*Row{}}
	c.tables[t.Name] = mt
	return mt
}

// set writes a cell, keeping cells sorted newest first (replacing same timestamp).
func (mt *memTable) set(key, fam, qual string, ts int64, val []byte) {
	r, ok := mt.rows[key]
	if !ok {
		r = &Row{Key: []byte(key), Families: map[string]map[string][]Cell{}}
		mt.rows[key] = r
	}
	if r.Families[fam] == nil {
		r.Families[fam] = map[string][]Cell{}
	}
	cells := r.Families[fam][qual]
	for i, c := range cells {
		if c.TimestampMicros == ts {
			cells[i].Value = val
			return
		}
	}
	cells = append(cells, Cell{TimestampMicros: ts, Value: val})
	sort.Slice(cells, func(i, j int) bool { return cells[i].TimestampMicros > cells[j].TimestampMicros })
	r.Families[fam][qual] = cells
}

func (mt *memTable) setStr(key, fam, qual string, ts int64, val string) {
	mt.set(key, fam, qual, ts, []byte(val))
}

func (mt *memTable) setInt(key, fam, qual string, ts int64, v int64) {
	mt.set(key, fam, qual, ts, binary.BigEndian.AppendUint64(nil, uint64(v)))
}

func (c *memCatalog) LookupTable(name string) (*Table, bool) {
	mt, ok := c.tables[name]
	if !ok {
		return nil, false
	}
	return mt.t, true
}

func (c *memCatalog) LookupView(name string) (string, bool) {
	q, ok := c.views[name]
	return q, ok
}

func (c *memCatalog) Scan(ctx context.Context, table string, start, end []byte, fn func(Row) bool) error {
	mt, ok := c.tables[table]
	if !ok {
		return status.Errorf(codes.NotFound, "table %s not found", table)
	}
	c.mu.Lock()
	c.scans = append(c.scans, keyRange{start: start, end: end})
	c.mu.Unlock()
	keys := make([]string, 0, len(mt.rows))
	for k := range mt.rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kb := []byte(k)
		if len(start) > 0 && bytes.Compare(kb, start) < 0 {
			continue
		}
		if len(end) > 0 && bytes.Compare(kb, end) >= 0 {
			break
		}
		c.mu.Lock()
		c.visited++
		c.mu.Unlock()
		if !fn(*mt.rows[k]) {
			return nil
		}
	}
	return nil
}

// mvCatalog adds IsMaterializedView.
type mvCatalog struct{ *memCatalog }

func (c mvCatalog) IsMaterializedView(name string) bool { return c.mvs[name] }

func ts(sec int64) int64 { return sec * 1e6 }

// stdCatalog builds the catalog used by most tests:
//
//	table t: families cf1, cf2 (bytes), agg (int64 aggregate), hll (HLL aggregate)
func stdCatalog() (*memCatalog, *memTable) {
	c := newMemCatalog()
	t := c.addTable(&Table{Name: "t", Families: []Family{
		{Name: "cf1", Kind: FamilyBytes},
		{Name: "cf2", Kind: FamilyBytes},
		{Name: "agg", Kind: FamilyInt64Aggregate},
		{Name: "hll", Kind: FamilyHLLAggregate},
	}})
	t.setStr("a#01", "cf1", "c1", ts(100), "xyz")
	t.setStr("a#01", "cf1", "c2", ts(100), "nop")
	t.setStr("a#01", "cf2", "n", ts(100), "5")
	t.setStr("a#02", "cf1", "c1", ts(100), "zyx")
	t.setStr("a#02", "cf2", "n", ts(100), "7")
	t.setStr("a#03", "cf1", "c1", ts(100), "abc")
	t.setStr("a#03", "cf1", "c2", ts(100), "def")
	t.setStr("b#01", "cf1", "c1", ts(100), "jkl")
	t.setStr("b#01", "cf2", "n", ts(100), "11")
	t.setInt("b#01", "agg", "sum", ts(100), 42)
	t.setStr("b#02", "cf1", "c1", ts(100), "hi")
	t.setStr("b#02", "cf1", "c1", ts(200), "hello")
	t.setStr("b#02", "cf1", "c1", ts(300), "howdy")
	t.setStr("c#03", "cf2", "n", ts(100), "3")
	return c, t
}

// render converts result rows into readable strings using SQL literal syntax.
func render(t *testing.T, meta *btpb.ResultSetMetadata, rows [][]*btpb.Value) []string {
	t.Helper()
	cols := meta.GetProtoSchema().GetColumns()
	var out []string
	for _, r := range rows {
		parts := make([]string, len(r))
		for i, v := range r {
			typ, err := typeFromProto(cols[i].GetType())
			require.NoError(t, err)
			x, err := fromProtoValue(v, typ, false)
			require.NoError(t, err)
			s, err := sqlLiteral(x, typ)
			require.NoError(t, err)
			parts[i] = s
		}
		out = append(out, strings.Join(parts, ", "))
	}
	return out
}

func paramValue(v any) (*btpb.Type, *btpb.Value) {
	switch x := v.(type) {
	case int64:
		return typInt64.toProto(), &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: x}}
	case int:
		return typInt64.toProto(), &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: int64(x)}}
	case string:
		return typString.toProto(), &btpb.Value{Kind: &btpb.Value_StringValue{StringValue: x}}
	case []byte:
		return typBytes.toProto(), &btpb.Value{Kind: &btpb.Value_BytesValue{BytesValue: x}}
	case bool:
		return typBool.toProto(), &btpb.Value{Kind: &btpb.Value_BoolValue{BoolValue: x}}
	case float64:
		return typFloat64.toProto(), &btpb.Value{Kind: &btpb.Value_FloatValue{FloatValue: x}}
	case time.Time:
		return typTimestamp.toProto(), &btpb.Value{Kind: &btpb.Value_TimestampValue{TimestampValue: timestamppb.New(x)}}
	case []string:
		vals := make([]*btpb.Value, len(x))
		for i, s := range x {
			vals[i] = &btpb.Value{Kind: &btpb.Value_StringValue{StringValue: s}}
		}
		return arrayOf(typString).toProto(), &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{Values: vals}}}
	}
	panic("unsupported param type")
}

type qopt struct {
	params     map[string]any
	noPushdown bool
}

func execQuery(cat Catalog, sql string, o qopt) (*Prepared, [][]*btpb.Value, error) {
	types := map[string]*btpb.Type{}
	vals := map[string]*btpb.Value{}
	for n, v := range o.params {
		t, pv := paramValue(v)
		types[n] = t
		pv.Type = t
		vals[n] = pv
	}
	p, err := Prepare(sql, types, cat)
	if err != nil {
		return nil, nil, err
	}
	p.noPushdown = o.noPushdown
	var rows [][]*btpb.Value
	err = p.Execute(context.Background(), cat, vals, func(r []*btpb.Value) error {
		rows = append(rows, r)
		return nil
	})
	return p, rows, err
}

// q runs a query and returns rendered rows.
func q(t *testing.T, cat Catalog, sql string, params ...map[string]any) []string {
	t.Helper()
	o := qopt{}
	if len(params) > 0 {
		o.params = params[0]
	}
	p, rows, err := execQuery(cat, sql, o)
	require.NoError(t, err, sql)
	return render(t, p.Metadata(), rows)
}

// qErr asserts that preparing or executing sql fails with the given code and
// message substring.
func qErr(t *testing.T, cat Catalog, sql string, code codes.Code, substr string, params ...map[string]any) {
	t.Helper()
	o := qopt{}
	if len(params) > 0 {
		o.params = params[0]
	}
	_, _, err := execQuery(cat, sql, o)
	require.Error(t, err, sql)
	st, ok := status.FromError(err)
	require.True(t, ok, "not a status error: %v", err)
	require.Equal(t, code, st.Code(), "%s: %v", sql, err)
	if substr != "" {
		require.Contains(t, strings.ToLower(st.Message()), strings.ToLower(substr), sql)
	}
}

// eval1 evaluates a single scalar expression without FROM.
func eval1(t *testing.T, expr string) string {
	t.Helper()
	rows := q(t, newMemCatalog(), "SELECT "+expr)
	require.Len(t, rows, 1, expr)
	return rows[0]
}

package gsql

import (
	"encoding/binary"
	"errors"
	"sort"
	"strings"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/status"
)

// source produces the rows of a FROM item.
type source interface {
	scan(env *execEnv, ec *evalCtx, ranges []keyRange, fn func(rowAccessor) (bool, error)) error
}

var errStopScan = errors.New("stop scan")

type emptyRow struct{}

func (emptyRow) column(int) (any, error) { return nil, internalf("no columns without FROM") }

type valuesRow []any

func (r valuesRow) column(i int) (any, error) { return r[i], nil }

// ---------------------------------------------------------------------------
// Base tables.

type temporalSpec struct {
	withHistory bool
	asOf        *bexpr
	after       *bexpr
	afterEq     *bexpr
	before      *bexpr
	latestN     *bexpr
}

type temporalFilter struct {
	withHistory bool
	hasAsOf     bool
	asOf        int64
	hasAfter    bool
	after       int64 // exclusive
	hasAfterEq  bool
	afterEq     int64
	hasBefore   bool
	before      int64 // exclusive
	latestN     int64 // 0 = unlimited
}

func (f *temporalFilter) pass(ts int64) bool {
	switch {
	case f.hasAsOf && ts > f.asOf:
		return false
	case f.hasAfter && ts <= f.after:
		return false
	case f.hasAfterEq && ts < f.afterEq:
		return false
	case f.hasBefore && ts >= f.before:
		return false
	}
	return true
}

func (s *temporalSpec) eval(ec *evalCtx) (*temporalFilter, error) {
	tf := &temporalFilter{withHistory: s.withHistory}
	tsArg := func(e *bexpr, name string, has *bool, out *int64) error {
		if e == nil {
			return nil
		}
		v, err := e.eval(ec)
		if err != nil {
			return err
		}
		if v == nil {
			return invalidf("temporal argument %s must not be NULL", name)
		}
		*has, *out = true, int64(v.(tsVal))
		return nil
	}
	if err := tsArg(s.asOf, "as_of", &tf.hasAsOf, &tf.asOf); err != nil {
		return nil, err
	}
	if err := tsArg(s.after, "after", &tf.hasAfter, &tf.after); err != nil {
		return nil, err
	}
	if err := tsArg(s.afterEq, "after_or_equal", &tf.hasAfterEq, &tf.afterEq); err != nil {
		return nil, err
	}
	if err := tsArg(s.before, "before", &tf.hasBefore, &tf.before); err != nil {
		return nil, err
	}
	if s.latestN != nil {
		v, err := s.latestN.eval(ec)
		if err != nil {
			return nil, err
		}
		if v == nil || v.(int64) < 1 {
			return nil, invalidf("latest_n must be greater than or equal to 1")
		}
		tf.latestN = v.(int64)
	}
	return tf, nil
}

type tableSource struct {
	table    *Table
	keyTypes []*sqlType
	famStart int
	temporal temporalSpec
}

func familyValueType(k FamilyKind) *sqlType {
	if k == FamilyInt64Aggregate {
		return typInt64
	}
	return typBytes
}

func (a *analyzer) newTableSource(t *Table, f *astFrom) (*tableSource, *scope, error) {
	ts := &tableSource{table: t}
	seen := map[string]bool{}
	cb := &binder{a: a, aggCtx: "table arguments"}
	for _, arg := range f.tableArgs {
		n := strings.ToLower(arg.name)
		if seen[n] {
			return nil, nil, invalidf("Duplicate argument %s for table %s", arg.name, t.Name)
		}
		seen[n] = true
		e, err := cb.bindExpr(arg.x)
		if err != nil {
			return nil, nil, err
		}
		if !isConst(e) {
			return nil, nil, invalidf("Argument %s for table %s must be a constant expression", arg.name, t.Name)
		}
		switch n {
		case "with_history":
			e, err = coerce(e, typBool)
			if err != nil || e.kind != ekConst || e.val == nil {
				return nil, nil, invalidf("with_history must be a BOOL literal (TRUE or FALSE)")
			}
			ts.temporal.withHistory = e.val.(bool)
		case "as_of", "after", "after_or_equal", "before":
			e, err = coerce(e, typTimestamp)
			if err != nil {
				return nil, nil, invalidf("Argument %s for table %s must be a TIMESTAMP", arg.name, t.Name)
			}
			switch n {
			case "as_of":
				ts.temporal.asOf = e
			case "after":
				ts.temporal.after = e
			case "after_or_equal":
				ts.temporal.afterEq = e
			default:
				ts.temporal.before = e
			}
		case "latest_n":
			e, err = coerce(e, typInt64)
			if err != nil {
				return nil, nil, invalidf("latest_n must be an INT64")
			}
			if e.kind == ekConst && (e.val == nil || e.val.(int64) < 1) {
				return nil, nil, invalidf("latest_n must be greater than or equal to 1")
			}
			ts.temporal.latestN = e
		default:
			return nil, nil, invalidf("Unknown argument %s for table %s; supported arguments are with_history, as_of, after, after_or_equal, before and latest_n", arg.name, t.Name)
		}
	}
	tp := &ts.temporal
	if !tp.withHistory {
		for name, e := range map[string]*bexpr{"after": tp.after, "after_or_equal": tp.afterEq, "before": tp.before, "latest_n": tp.latestN} {
			if e != nil {
				return nil, nil, invalidf("%s requires with_history => TRUE", name)
			}
		}
	}
	if tp.after != nil && tp.afterEq != nil {
		return nil, nil, invalidf("after and after_or_equal cannot both be specified")
	}
	sc := &scope{alias: t.Name, keyCol: 0, src: ts}
	if f.alias != "" {
		sc.alias = f.alias
	}
	sc.cols = append(sc.cols, scopeCol{name: "_key", typ: typBytes})
	if t.RowKeySchema != nil {
		types, err := keySchemaTypes(t.RowKeySchema)
		if err != nil {
			return nil, nil, invalidf("table %s has an invalid row key schema: %v", t.Name, status.Convert(err).Message())
		}
		ts.keyTypes = types
		for i, fld := range t.RowKeySchema.GetFields() {
			sc.cols = append(sc.cols, scopeCol{name: fld.GetFieldName(), typ: types[i]})
		}
	}
	ts.famStart = len(sc.cols)
	for _, fam := range t.Families {
		vt := familyValueType(fam.Kind)
		var ct *sqlType
		if tp.withHistory {
			ct = mapOf(typBytes, arrayOf(historyCellType(vt)))
		} else {
			ct = mapOf(typBytes, vt)
		}
		sc.cols = append(sc.cols, scopeCol{name: fam.Name, typ: ct})
	}
	return ts, sc, nil
}

// visible reports whether the row has at least one cell (in a known family)
// that passes the temporal filter.
func (s *tableSource) visible(row Row, tf *temporalFilter) bool {
	for _, fam := range s.table.Families {
		for _, cells := range row.Families[fam.Name] {
			for _, c := range cells {
				if tf.pass(c.TimestampMicros) {
					return true
				}
			}
		}
	}
	return false
}

func (s *tableSource) scan(env *execEnv, ec *evalCtx, ranges []keyRange, fn func(rowAccessor) (bool, error)) error {
	tf, err := s.temporal.eval(ec)
	if err != nil {
		return err
	}
	var ferr error
	stopped := false
	for _, r := range ranges {
		err := env.cat.Scan(env.ctx, s.table.Name, r.start, r.end, func(row Row) bool {
			if err := env.ctx.Err(); err != nil {
				ferr = toStatus(err)
				return false
			}
			if !s.visible(row, tf) {
				return true
			}
			br := &baseRow{src: s, tf: tf, row: row}
			cont, err := fn(br)
			if err != nil {
				ferr = err
				return false
			}
			if !cont {
				stopped = true
				return false
			}
			return true
		})
		if ferr != nil {
			return ferr
		}
		if err != nil {
			return toStatus(err)
		}
		if stopped {
			return nil
		}
	}
	return nil
}

type baseRow struct {
	src     *tableSource
	tf      *temporalFilter
	row     Row
	vals    []any
	done    []bool
	keyVals []any
	keyErr  error
	keyDone bool
}

func (r *baseRow) column(i int) (any, error) {
	if r.vals == nil {
		n := r.src.famStart + len(r.src.table.Families)
		r.vals = make([]any, n)
		r.done = make([]bool, n)
	}
	if r.done[i] {
		return r.vals[i], nil
	}
	var v any
	switch {
	case i == 0:
		v = append([]byte{}, r.row.Key...)
	case i < r.src.famStart:
		if !r.keyDone {
			r.keyDone = true
			r.keyVals, r.keyErr = decodeKeyInternal(r.src.table.RowKeySchema, r.row.Key)
		}
		if r.keyErr != nil {
			return nil, r.keyErr
		}
		v = r.keyVals[i-1]
	default:
		fam := r.src.table.Families[i-r.src.famStart]
		v = buildFamilyValue(fam, r.row.Families[fam.Name], r.tf)
	}
	r.vals[i], r.done[i] = v, true
	return v, nil
}

func cellValue(kind FamilyKind, b []byte) any {
	if kind == FamilyInt64Aggregate {
		if len(b) != 8 {
			return nil
		}
		return int64(binary.BigEndian.Uint64(b))
	}
	return append([]byte{}, b...)
}

// buildFamilyValue builds the MAP value of a column family for one row:
// the latest passing cell per qualifier, or all passing cells (newest first,
// bounded by latest_n) with with_history. An empty family is NULL.
func buildFamilyValue(fam Family, quals map[string][]Cell, tf *temporalFilter) any {
	if len(quals) == 0 {
		return nil
	}
	names := make([]string, 0, len(quals))
	for q := range quals {
		names = append(names, q)
	}
	sort.Strings(names)
	var keys, vals []any
	for _, q := range names {
		cells := quals[q]
		if tf.withHistory {
			var hist arrayV
			for _, c := range cells {
				if !tf.pass(c.TimestampMicros) {
					continue
				}
				hist = append(hist, structV{tsVal(c.TimestampMicros), cellValue(fam.Kind, c.Value)})
				if tf.latestN > 0 && int64(len(hist)) >= tf.latestN {
					break
				}
			}
			if len(hist) == 0 {
				continue
			}
			keys = append(keys, []byte(q))
			vals = append(vals, hist)
			continue
		}
		for _, c := range cells {
			if tf.pass(c.TimestampMicros) {
				keys = append(keys, []byte(q))
				vals = append(vals, cellValue(fam.Kind, c.Value))
				break
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return &mapV{keys: keys, vals: vals}
}

// ---------------------------------------------------------------------------
// Derived sources (views and wrapped parenthesized queries).

type derivedSource struct {
	plan      *queryPlan
	ownParams bool // views run without the outer query's parameters
	synthKey  bool // append a hidden _key column computed from the view key
}

func (s *derivedSource) scan(env *execEnv, ec *evalCtx, _ []keyRange, fn func(rowAccessor) (bool, error)) error {
	inner := *env
	if s.ownParams {
		inner.params = nil
	}
	err := s.plan.run(&inner, func(vals []any) error {
		row := vals
		if s.synthKey {
			k, err := s.plan.mv.encodeKey(vals)
			if err != nil {
				return nil
			}
			row = append(append([]any{}, vals...), k)
		}
		cont, err := fn(valuesRow(row))
		if err != nil {
			return err
		}
		if !cont {
			return errStopScan
		}
		return nil
	})
	if err == errStopScan {
		return nil
	}
	return err
}

func (a *analyzer) newViewSource(name, sql, alias string) (*derivedSource, *scope, error) {
	if a.viewDepth >= 8 {
		return nil, nil, invalidf("views are nested too deeply (at view %s)", name)
	}
	q, _, err := parseStatement(sql)
	if err != nil {
		return nil, nil, wrapViewErr(name, err)
	}
	sub := &analyzer{cat: a.cat, params: map[string]int{}, viewDepth: a.viewDepth + 1}
	var plan *queryPlan
	mc, hasKind := a.cat.(MaterializedViewCatalog)
	switch {
	case hasKind && mc.IsMaterializedView(name):
		sub.mv = true
		plan, _, err = sub.analyzeMV(q)
	default:
		plan, err = sub.analyzeQuery(q)
		if err != nil && !hasKind {
			// Without catalog guidance, accept materialized-view definitions
			// (e.g. secondary indexes ordered by non-key columns).
			sub2 := &analyzer{cat: a.cat, params: map[string]int{}, viewDepth: a.viewDepth + 1, mv: true}
			if p2, _, err2 := sub2.analyzeMV(q); err2 == nil {
				plan, err = p2, nil
			}
		}
	}
	if err != nil {
		return nil, nil, wrapViewErr(name, err)
	}
	ds := &derivedSource{plan: plan, ownParams: true}
	sc := &scope{alias: name, keyCol: -1, src: ds}
	if alias != "" {
		sc.alias = alias
	}
	for _, c := range plan.outCols {
		sc.cols = append(sc.cols, scopeCol{name: c.name, typ: c.typ})
	}
	for i, c := range sc.cols {
		if c.name == "_key" {
			sc.keyCol = i
			break
		}
	}
	if sc.keyCol < 0 && plan.mv != nil {
		ds.synthKey = true
		sc.keyCol = len(sc.cols)
		sc.cols = append(sc.cols, scopeCol{name: "_key", typ: typBytes, hidden: true})
	}
	return ds, sc, nil
}

func wrapViewErr(name string, err error) error {
	st := status.Convert(err)
	return status.Errorf(st.Code(), "invalid view %s: %s", name, st.Message())
}

// ---------------------------------------------------------------------------
// UNPACK.

type unpackSource struct {
	plan  *queryPlan
	kinds []byte // per inner column: 'm' map history, 'a' array history, 0 unchanged
	table string
}

// historyValueType returns V if t is ARRAY<STRUCT<timestamp TIMESTAMP, value V>>.
func historyValueType(t *sqlType) *sqlType {
	if t.kind != tkArray || t.elem.kind != tkStruct || len(t.elem.fields) != 2 {
		return nil
	}
	f := t.elem.fields
	if !strings.EqualFold(f[0].name, "timestamp") || f[0].typ.kind != tkTimestamp || !strings.EqualFold(f[1].name, "value") {
		return nil
	}
	return f[1].typ
}

func (a *analyzer) newUnpackSource(f *astFrom) (*unpackSource, *scope, error) {
	inner, err := a.analyzeQuery(f.unpack)
	if err != nil {
		return nil, nil, err
	}
	ts, ok := inner.src.(*tableSource)
	if !ok || !ts.temporal.withHistory {
		return nil, nil, invalidf("UNPACK requires a subquery that reads a table with with_history => TRUE")
	}
	us := &unpackSource{plan: inner, table: ts.table.Name}
	sc := &scope{alias: f.alias, keyCol: -1, src: us}
	for i, c := range inner.outCols {
		t := c.typ
		kind := byte(0)
		if t.kind == tkMap {
			if vt := historyValueType(t.val); vt != nil {
				t, kind = mapOf(t.key, vt), 'm'
			}
		} else if vt := historyValueType(t); vt != nil {
			t, kind = vt, 'a'
		}
		us.kinds = append(us.kinds, kind)
		sc.cols = append(sc.cols, scopeCol{name: c.name, typ: t})
		if c.name == "_key" && sc.keyCol < 0 {
			sc.keyCol = i
		}
	}
	sc.cols = append(sc.cols, scopeCol{name: "_timestamp", typ: typTimestamp})
	return us, sc, nil
}

func (s *unpackSource) scan(env *execEnv, ec *evalCtx, _ []keyRange, fn func(rowAccessor) (bool, error)) error {
	err := s.plan.run(env, func(vals []any) error {
		set := map[tsVal]bool{}
		for i, k := range s.kinds {
			if vals[i] == nil {
				continue
			}
			switch k {
			case 'm':
				for _, hv := range vals[i].(*mapV).vals {
					for _, e := range hv.(arrayV) {
						set[e.(structV)[0].(tsVal)] = true
					}
				}
			case 'a':
				for _, e := range vals[i].(arrayV) {
					set[e.(structV)[0].(tsVal)] = true
				}
			}
		}
		tss := make([]tsVal, 0, len(set))
		for t := range set {
			tss = append(tss, t)
		}
		sort.Slice(tss, func(i, j int) bool { return tss[i] > tss[j] })
		for _, t := range tss {
			row := make(valuesRow, len(vals)+1)
			for i, k := range s.kinds {
				switch {
				case vals[i] == nil || k == 0:
					row[i] = vals[i]
				case k == 'm':
					m := vals[i].(*mapV)
					var keys, mv []any
					for j, hv := range m.vals {
						for _, e := range hv.(arrayV) {
							st := e.(structV)
							if st[0].(tsVal) == t {
								keys = append(keys, m.keys[j])
								mv = append(mv, st[1])
								break
							}
						}
					}
					if len(keys) > 0 {
						row[i] = &mapV{keys: keys, vals: mv}
					}
				case k == 'a':
					for _, e := range vals[i].(arrayV) {
						st := e.(structV)
						if st[0].(tsVal) == t {
							row[i] = st[1]
							break
						}
					}
				}
			}
			row[len(vals)] = t
			cont, err := fn(row)
			if err != nil {
				return err
			}
			if !cont {
				return errStopScan
			}
		}
		return nil
	})
	if err == errStopScan {
		return nil
	}
	return err
}

// keySchemaForView builds the OrderedCodeBytes row key schema used for
// materialized view keys.
func keySchemaForView(names []string, types []*sqlType) *btpb.Type_Struct {
	fs := make([]*btpb.Type_Struct_Field, len(names))
	for i, n := range names {
		fs[i] = &btpb.Type_Struct_Field{FieldName: n, Type: keyFieldProto(types[i])}
	}
	return &btpb.Type_Struct{
		Fields:   fs,
		Encoding: &btpb.Type_Struct_Encoding{Encoding: &btpb.Type_Struct_Encoding_OrderedCodeBytes_{OrderedCodeBytes: &btpb.Type_Struct_Encoding_OrderedCodeBytes{}}},
	}
}

func keyFieldProto(t *sqlType) *btpb.Type {
	ocInt := &btpb.Type_Int64_Encoding{Encoding: &btpb.Type_Int64_Encoding_OrderedCodeBytes_{OrderedCodeBytes: &btpb.Type_Int64_Encoding_OrderedCodeBytes{}}}
	switch t.kind {
	case tkString:
		return &btpb.Type{Kind: &btpb.Type_StringType{StringType: &btpb.Type_String{Encoding: &btpb.Type_String_Encoding{
			Encoding: &btpb.Type_String_Encoding_Utf8Bytes_{Utf8Bytes: &btpb.Type_String_Encoding_Utf8Bytes{}}}}}}
	case tkBytes:
		return &btpb.Type{Kind: &btpb.Type_BytesType{BytesType: &btpb.Type_Bytes{Encoding: &btpb.Type_Bytes_Encoding{
			Encoding: &btpb.Type_Bytes_Encoding_Raw_{Raw: &btpb.Type_Bytes_Encoding_Raw{}}}}}}
	case tkInt64:
		return &btpb.Type{Kind: &btpb.Type_Int64Type{Int64Type: &btpb.Type_Int64{Encoding: ocInt}}}
	case tkTimestamp:
		return &btpb.Type{Kind: &btpb.Type_TimestampType{TimestampType: &btpb.Type_Timestamp{Encoding: &btpb.Type_Timestamp_Encoding{
			Encoding: &btpb.Type_Timestamp_Encoding_UnixMicrosInt64{UnixMicrosInt64: ocInt}}}}}
	}
	return nil
}

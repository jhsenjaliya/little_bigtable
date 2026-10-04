package gsql

import (
	"strings"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// ViewDefinition describes the storage layout of a continuous materialized
// view derived from its defining query.
type ViewDefinition struct {
	// Source is the source table ID.
	Source string
	// KeyColumns are the output columns forming the view's row key, in key order.
	KeyColumns []string
	// KeySchema is the row key schema automatically created for the view, or
	// nil when the key is the _key column itself. It uses the OrderedCodeBytes
	// struct encoding with STRING utf8_bytes, BYTES raw, INT64
	// ordered_code_bytes and TIMESTAMP unix_micros_int64(ordered_code_bytes)
	// fields. This is the emulator's choice: production does not document the
	// encoding of auto-created view key schemas.
	KeySchema *btpb.Type_Struct
	// ValueColumns are the remaining output columns (excluding the key and
	// _timestamp columns), in SELECT order.
	ValueColumns []string
	// TimestampColumn is the name of the _timestamp output column, or "".
	TimestampColumn string
	// Aggregated is true for GROUP BY views.
	Aggregated bool
}

type mvPlan struct {
	keyIdx    []int
	tsIdx     int
	valueIdx  []int
	keySchema *btpb.Type_Struct // nil when the key is _key
}

// accept implements the row exclusion rules of continuous materialized views:
// key columns must not be NULL, _timestamp must be a non-negative multiple of
// 1000 microseconds, and rows whose value columns are all NULL are omitted.
func (m *mvPlan) accept(vals []any) bool {
	for _, i := range m.keyIdx {
		if vals[i] == nil {
			return false
		}
	}
	if m.tsIdx >= 0 {
		ts, ok := vals[m.tsIdx].(tsVal)
		if !ok || ts < 0 || ts%1000 != 0 {
			return false
		}
	}
	if len(m.valueIdx) > 0 {
		for _, i := range m.valueIdx {
			if vals[i] != nil {
				return true
			}
		}
		return false
	}
	return true
}

// less orders view rows by key, then by _timestamp descending.
func (m *mvPlan) less(a, b []any) bool {
	for _, i := range m.keyIdx {
		if c := compareValues(a[i], b[i]); c != 0 {
			return c < 0
		}
	}
	if m.tsIdx >= 0 {
		return compareValues(a[m.tsIdx], b[m.tsIdx]) > 0
	}
	return false
}

// encodeKey computes the row key of a view row.
func (m *mvPlan) encodeKey(vals []any) ([]byte, error) {
	if m.keySchema == nil {
		b, _ := vals[m.keyIdx[0]].([]byte)
		return b, nil
	}
	kv := make([]any, len(m.keyIdx))
	for i, idx := range m.keyIdx {
		kv[i] = vals[idx]
	}
	return encodeKeyInternal(m.keySchema, kv)
}

// PrepareMaterializedView analyzes a continuous materialized view definition.
// The returned Prepared computes the full contents of the view (rows in key
// order, with the view's row exclusion rules applied); ViewDefinition
// describes its key and value columns.
func PrepareMaterializedView(query string, cat Catalog) (*Prepared, *ViewDefinition, error) {
	q, hints, err := parseStatement(query)
	if err != nil {
		return nil, nil, err
	}
	if len(hints) > 0 {
		return nil, nil, invalidf("statement hints are not supported in continuous materialized view definitions")
	}
	a := &analyzer{cat: cat, params: map[string]int{}, mv: true}
	plan, def, err := a.analyzeMV(q)
	if err != nil {
		return nil, nil, err
	}
	return newPrepared(query, plan, nil, nil), def, nil
}

func (a *analyzer) analyzeMV(q *astQuery) (*queryPlan, *ViewDefinition, error) {
	a.mv = true
	for q.inner != nil && len(q.orderBy) == 0 && q.limit == nil {
		q = q.inner
	}
	if q.limit != nil || q.offset != nil {
		return nil, nil, invalidf("LIMIT and OFFSET are not supported in continuous materialized views")
	}
	if q.inner != nil {
		return nil, nil, invalidf("continuous materialized views must be defined by a single SELECT")
	}
	sel := q.sel
	if sel.distinct {
		return nil, nil, invalidf("SELECT DISTINCT is not supported in continuous materialized views")
	}
	hasGroup, hasOrder := len(sel.groupBy) > 0, len(q.orderBy) > 0
	switch {
	case hasGroup && hasOrder:
		return nil, nil, invalidf("a continuous materialized view must have a GROUP BY clause or an ORDER BY clause, but not both")
	case !hasGroup && !hasOrder:
		return nil, nil, invalidf("a continuous materialized view must have a GROUP BY clause (aggregation) or an ORDER BY clause (asynchronous secondary index)")
	}
	for _, it := range sel.items {
		if it.star {
			return nil, nil, invalidf("SELECT * is not supported in continuous materialized views")
		}
	}
	if sel.from == nil {
		return nil, nil, invalidf("a continuous materialized view must read from a table")
	}
	if sel.from.unpack == nil {
		if t, ok := a.cat.LookupTable(sel.from.table); !ok || t == nil {
			if _, isView := a.cat.LookupView(sel.from.table); isView {
				return nil, nil, invalidf("continuous materialized views cannot be defined over views (%s)", sel.from.table)
			}
			return nil, nil, notFoundf("Table not found: %s", sel.from.table)
		}
	}
	if sel.having != nil {
		return nil, nil, invalidf("HAVING is not supported in continuous materialized views")
	}
	var orderIdx []int
	var srcName string
	plan, err := a.analyzeSelect(sel, q.orderBy, nil, nil, analyzeOpts{mvOrder: hasOrder, orderIx: &orderIdx, srcName: &srcName})
	if err != nil {
		return nil, nil, err
	}
	if a.sawNondet {
		return nil, nil, invalidf("non-deterministic functions are not supported in continuous materialized views")
	}
	if a.sawStruct {
		return nil, nil, invalidf("STRUCT is not supported in continuous materialized views")
	}
	if hasOrder && len(plan.aggs) > 0 {
		return nil, nil, invalidf("aggregate functions require GROUP BY in continuous materialized views")
	}
	seen := map[string]bool{}
	tsIdx := -1
	for i, c := range plan.outCols {
		if c.name == "" {
			return nil, nil, invalidf("every column of a continuous materialized view must have an alias (column %d does not)", i+1)
		}
		ln := strings.ToLower(c.name)
		if seen[ln] {
			return nil, nil, invalidf("duplicate column name %s in continuous materialized view", c.name)
		}
		seen[ln] = true
		switch c.typ.kind {
		case tkDate:
			return nil, nil, invalidf("DATE output columns are not supported in continuous materialized views (column %s); use TIMESTAMP or STRING", c.name)
		case tkArray:
			return nil, nil, invalidf("ARRAY output columns are not supported in continuous materialized views (column %s)", c.name)
		case tkStruct:
			return nil, nil, invalidf("STRUCT output columns are not supported in continuous materialized views (column %s)", c.name)
		case tkMap:
			e := plan.outExprs[i]
			if !hasOrder || e.kind != ekColumn || plan.scope == nil {
				return nil, nil, invalidf("continuous materialized views cannot create map columns (column %s); only whole column families can be selected in ORDER BY views", c.name)
			}
		}
		if ln == "_timestamp" {
			if c.typ.kind != tkTimestamp {
				return nil, nil, invalidf("_timestamp must be of type TIMESTAMP, got %s", c.typ)
			}
			tsIdx = i
		}
	}

	var keyIdx []int
	if hasGroup {
		byGroup := make([]int, len(plan.groupKeys))
		for i := range byGroup {
			byGroup[i] = -1
		}
		for i, e := range plan.outExprs {
			switch {
			case e.kind == ekGroupRef:
				if byGroup[e.idx] >= 0 {
					return nil, nil, invalidf("GROUP BY expression is selected more than once (%s and %s)", plan.outCols[byGroup[e.idx]].name, plan.outCols[i].name)
				}
				byGroup[e.idx] = i
			case containsAgg(e):
			default:
				return nil, nil, invalidf("column %s must be aggregated or included in the GROUP BY clause", plan.outCols[i].name)
			}
		}
		for g, i := range byGroup {
			if i < 0 {
				return nil, nil, invalidf("GROUP BY expression %d must appear in the SELECT list of a continuous materialized view", g+1)
			}
			if i != tsIdx {
				keyIdx = append(keyIdx, i)
			}
		}
	} else {
		for _, i := range orderIdx {
			if i == tsIdx {
				return nil, nil, invalidf("_timestamp cannot be part of the row key of a continuous materialized view")
			}
			for _, k := range keyIdx {
				if k == i {
					return nil, nil, invalidf("column %s appears more than once in ORDER BY", plan.outCols[i].name)
				}
			}
			keyIdx = append(keyIdx, i)
		}
	}
	if len(keyIdx) == 0 {
		return nil, nil, invalidf("a continuous materialized view needs at least one key column")
	}
	keyIsKey := false
	for i, c := range plan.outCols {
		if !strings.EqualFold(c.name, "_key") {
			continue
		}
		isKeyCol := false
		for _, k := range keyIdx {
			if k == i {
				isKeyCol = true
			}
		}
		if !isKeyCol {
			return nil, nil, invalidf("_key must be a key column of the view (grouped or ordered by)")
		}
		if len(keyIdx) != 1 {
			return nil, nil, invalidf("when _key is specified the view cannot be keyed by anything else (except _timestamp)")
		}
		if c.typ.kind != tkBytes {
			return nil, nil, invalidf("the _key column must be of type BYTES, got %s", c.typ)
		}
		keyIsKey = true
	}
	var keyNames []string
	var keyTypes []*sqlType
	for _, i := range keyIdx {
		c := plan.outCols[i]
		switch c.typ.kind {
		case tkString, tkBytes, tkInt64, tkTimestamp:
		default:
			return nil, nil, invalidf("column %s of type %s cannot be part of the row key of a continuous materialized view", c.name, c.typ)
		}
		keyNames = append(keyNames, c.name)
		keyTypes = append(keyTypes, c.typ)
	}
	mv := &mvPlan{keyIdx: keyIdx, tsIdx: tsIdx}
	def := &ViewDefinition{Source: srcName, KeyColumns: keyNames, Aggregated: hasGroup}
	if !keyIsKey {
		mv.keySchema = keySchemaForView(keyNames, keyTypes)
		def.KeySchema = mv.keySchema
	}
	for i, c := range plan.outCols {
		if i == tsIdx {
			def.TimestampColumn = c.name
			continue
		}
		isKey := false
		for _, k := range keyIdx {
			if k == i {
				isKey = true
			}
		}
		if !isKey {
			mv.valueIdx = append(mv.valueIdx, i)
			def.ValueColumns = append(def.ValueColumns, c.name)
		}
	}
	plan.mv = mv
	plan.order = nil
	return plan, def, nil
}

// mvOrderIndex resolves an ORDER BY item of a secondary-index view to a
// SELECT column: by alias, ordinal or an identical expression.
func (a *analyzer) mvOrderIndex(x astExpr, items []selItem, sc *scope) (int, error) {
	if lit, ok := x.(*astLit); ok && lit.kind == litInt {
		n := lit.val.(int64)
		if n < 1 || n > int64(len(items)) {
			return 0, invalidf("ORDER BY column number %d is out of range", n)
		}
		return int(n - 1), nil
	}
	if id, ok := x.(*astIdent); ok {
		idx := -1
		for i, it := range items {
			if it.name != "" && strings.EqualFold(it.name, id.name) {
				if idx >= 0 {
					return 0, invalidf("ORDER BY name %s is ambiguous", id.name)
				}
				idx = i
			}
		}
		if idx >= 0 {
			return idx, nil
		}
	}
	var aggs []*aggSpec
	b := &binder{a: a, scope: sc, aggs: &aggs}
	e, err := b.bindExpr(x)
	if err != nil {
		return 0, err
	}
	fp := e.fingerprint()
	for i, it := range items {
		if it.expr.fingerprint() == fp {
			return i, nil
		}
	}
	return 0, invalidf("ORDER BY expressions of a continuous materialized view must be columns of the SELECT list")
}

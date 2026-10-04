package gsql

import (
	"strconv"
	"strings"
)

type outCol struct {
	name string
	typ  *sqlType
}

// queryPlan is an analyzed SELECT.
type queryPlan struct {
	src        source
	scope      *scope
	where      *bexpr
	aggregated bool
	groupKeys  []*bexpr
	aggs       []*aggSpec
	having     *bexpr
	outExprs   []*bexpr
	outCols    []outCol
	distinct   bool
	order      []orderSpec
	orderNoop  bool // ORDER BY matches the scan order of the base table
	limit      *bexpr
	offset     *bexpr
	mv         *mvPlan
}

type selItem struct {
	name     string
	expr     *bexpr
	explicit bool
}

type analyzeOpts struct {
	mvOrder bool    // ORDER BY follows materialized-view (secondary index) rules
	scope   *scope  // pre-resolved FROM scope (wrapped queries)
	orderIx *[]int  // receives ORDER BY output column indices (mvOrder)
	srcName *string // receives the base table name (materialized views)
}

func (a *analyzer) analyzeQuery(q *astQuery) (*queryPlan, error) {
	if q.inner != nil {
		if len(q.orderBy) == 0 && q.limit == nil {
			return a.analyzeQuery(q.inner)
		}
		if q.inner.sel != nil && len(q.inner.orderBy) == 0 && q.inner.limit == nil {
			return a.analyzeSelect(q.inner.sel, q.orderBy, q.limit, q.offset, analyzeOpts{})
		}
		inner, err := a.analyzeQuery(q.inner)
		if err != nil {
			return nil, err
		}
		ds := &derivedSource{plan: inner}
		sc := &scope{keyCol: -1, src: ds}
		for i, c := range inner.outCols {
			sc.cols = append(sc.cols, scopeCol{name: c.name, typ: c.typ})
			if c.name == "_key" && sc.keyCol < 0 {
				sc.keyCol = i
			}
		}
		sel := &astSelect{items: []astSelectItem{{star: true}}}
		return a.analyzeSelect(sel, q.orderBy, q.limit, q.offset, analyzeOpts{scope: sc})
	}
	return a.analyzeSelect(q.sel, q.orderBy, q.limit, q.offset, analyzeOpts{})
}

func (a *analyzer) resolveFrom(f *astFrom) (source, *scope, error) {
	for _, h := range f.hints {
		if !strings.EqualFold(h.name, "allow_incomplete_view") {
			return nil, nil, unimplementedf("table hint %s is not supported by the emulator", h.name)
		}
		// allow_incomplete_view is a no-op: emulator views are always complete.
	}
	if f.unpack != nil {
		us, sc, err := a.newUnpackSource(f)
		if err != nil {
			return nil, nil, err
		}
		return us, sc, nil
	}
	if t, ok := a.cat.LookupTable(f.table); ok && t != nil {
		ts, sc, err := a.newTableSource(t, f)
		if err != nil {
			return nil, nil, err
		}
		return ts, sc, nil
	}
	if sql, ok := a.cat.LookupView(f.table); ok {
		if f.hasArgs {
			return nil, nil, invalidf("temporal arguments are only supported on tables, not on view %s", f.table)
		}
		ds, sc, err := a.newViewSource(f.table, sql, f.alias)
		if err != nil {
			return nil, nil, err
		}
		return ds, sc, nil
	}
	return nil, nil, notFoundf("Table not found: %s", f.table)
}

func containsAgg(e *bexpr) bool {
	found := false
	walk(e, func(x *bexpr) bool {
		if x.kind == ekAggRef {
			found = true
		}
		return !found
	})
	return found
}

func (a *analyzer) analyzeSelect(sel *astSelect, orderBy []astOrderItem, limit, offset astExpr, opts analyzeOpts) (*queryPlan, error) {
	p := &queryPlan{distinct: sel.distinct}
	sc := opts.scope
	if sc == nil && sel.from != nil {
		src, s, err := a.resolveFrom(sel.from)
		if err != nil {
			return nil, err
		}
		sc = s
		if opts.srcName != nil {
			switch x := src.(type) {
			case *tableSource:
				*opts.srcName = x.table.Name
			case *unpackSource:
				*opts.srcName = x.table
			}
		}
	}
	if sc != nil {
		p.src = sc.src
	}
	p.scope = sc

	if sel.where != nil {
		b := &binder{a: a, scope: sc, aggCtx: "WHERE clause"}
		w, err := b.bindExpr(sel.where)
		if err != nil {
			return nil, err
		}
		if w, err = coerce(w, typBool); err != nil {
			return nil, invalidf("WHERE clause should return type BOOL, but returns %s", w.typ)
		}
		p.where = w
	}

	var aggs []*aggSpec
	rowB := &binder{a: a, scope: sc, aggs: &aggs}
	var items []selItem
	for _, it := range sel.items {
		if it.star {
			ex, err := a.expandStar(rowB, sc, it)
			if err != nil {
				return nil, err
			}
			items = append(items, ex...)
			continue
		}
		e, err := rowB.bindExpr(it.expr)
		if err != nil {
			return nil, err
		}
		name := it.alias
		if !it.hasAlias {
			name = implicitAlias(it.expr)
		}
		if a.mv && strings.EqualFold(name, "_key") && e.kind == ekConst && e.typ.kind == tkString {
			e, _ = coerce(e, typBytes)
		}
		items = append(items, selItem{name: name, expr: e, explicit: it.hasAlias})
	}
	aliasLookup := func(name string) (*bexpr, bool, error) {
		if strings.HasPrefix(name, "$col") {
			if n, err := strconv.Atoi(name[4:]); err == nil && n >= 1 && n <= len(items) {
				return items[n-1].expr, true, nil
			}
		}
		var found *bexpr
		for _, it := range items {
			if it.name != "" && strings.EqualFold(it.name, name) {
				if found != nil && found.fingerprint() != it.expr.fingerprint() {
					return nil, false, invalidf("Name %s is ambiguous: it refers to multiple SELECT list columns", name)
				}
				found = it.expr
			}
		}
		return found, found != nil, nil
	}
	ordinal := func(x astExpr, clause string) (*bexpr, bool, error) {
		lit, ok := x.(*astLit)
		if !ok || lit.kind != litInt {
			return nil, false, nil
		}
		n := lit.val.(int64)
		if n < 1 || n > int64(len(items)) {
			return nil, false, invalidf("%s column number %d is out of range [1, %d]", clause, n, len(items))
		}
		return items[n-1].expr, true, nil
	}

	gb := &binder{a: a, scope: sc, aggCtx: "GROUP BY clause", aliases: aliasLookup}
	for _, g := range sel.groupBy {
		e, ok, err := ordinal(g, "GROUP BY")
		if err != nil {
			return nil, err
		}
		if !ok {
			if e, err = gb.bindExpr(g); err != nil {
				return nil, err
			}
		}
		if containsAgg(e) {
			return nil, invalidf("GROUP BY cannot refer to an aggregate expression")
		}
		if !e.typ.groupable() {
			return nil, invalidf("Grouping by expressions of type %s is not allowed", e.typ)
		}
		p.groupKeys = append(p.groupKeys, e)
	}

	if sel.having != nil {
		hb := &binder{a: a, scope: sc, aggs: &aggs, aggCtx: "HAVING clause", aliases: aliasLookup}
		h, err := hb.bindExpr(sel.having)
		if err != nil {
			return nil, err
		}
		if h, err = coerce(h, typBool); err != nil {
			return nil, invalidf("HAVING clause should return type BOOL, but returns %s", h.typ)
		}
		p.having = h
	}

	var orderIdx []int
	for _, o := range orderBy {
		if opts.mvOrder {
			if o.desc {
				return nil, invalidf("DESC sort order is not supported in continuous materialized views")
			}
			if o.nullsSeen {
				return nil, invalidf("NULLS FIRST/LAST is not supported in continuous materialized views")
			}
			idx, err := a.mvOrderIndex(o.x, items, sc)
			if err != nil {
				return nil, err
			}
			orderIdx = append(orderIdx, idx)
			continue
		}
		e, ok, err := ordinal(o.x, "ORDER BY")
		if err != nil {
			return nil, err
		}
		if !ok {
			ob := &binder{a: a, scope: sc, aggs: &aggs, aggCtx: "ORDER BY clause", aliases: aliasLookup}
			if e, err = ob.bindExpr(o.x); err != nil {
				return nil, err
			}
		}
		if e.typ.kind == tkArray || !e.typ.orderable() {
			return nil, invalidf("ORDER BY does not support expressions of type %s", e.typ)
		}
		p.order = append(p.order, orderSpec{expr: e, desc: o.desc, nullsLast: nullsLast(o)})
	}
	if opts.orderIx != nil {
		*opts.orderIx = orderIdx
	}

	p.aggs = aggs
	p.aggregated = len(p.groupKeys) > 0 || len(aggs) > 0
	if p.aggregated {
		var err error
		for i := range items {
			if items[i].expr, err = mapToGroup(items[i].expr, p.groupKeys, sc, "SELECT list"); err != nil {
				return nil, err
			}
		}
		if p.having != nil {
			if p.having, err = mapToGroup(p.having, p.groupKeys, sc, "HAVING clause"); err != nil {
				return nil, err
			}
		}
		for i := range p.order {
			if p.order[i].expr, err = mapToGroup(p.order[i].expr, p.groupKeys, sc, "ORDER BY clause"); err != nil {
				return nil, err
			}
		}
	} else if p.having != nil {
		return nil, invalidf("HAVING clause requires GROUP BY or aggregation")
	}

	for _, it := range items {
		p.outExprs = append(p.outExprs, it.expr)
		p.outCols = append(p.outCols, outCol{name: it.name, typ: it.expr.typ})
	}
	if p.distinct {
		for _, c := range p.outCols {
			if !c.typ.groupable() {
				return nil, invalidf("Column %s of type %s cannot be used in SELECT DISTINCT", c.name, c.typ)
			}
		}
		for _, o := range p.order {
			fp := o.expr.fingerprint()
			ok := false
			for _, e := range p.outExprs {
				if e.fingerprint() == fp {
					ok = true
				}
			}
			if !ok {
				return nil, invalidf("ORDER BY clause expression references a column that is not visible after SELECT DISTINCT")
			}
		}
	}

	if !opts.mvOrder && !a.mv && len(p.order) > 0 {
		if err := p.validateKeyOrder(orderBy); err != nil {
			return nil, err
		}
	}
	if len(p.order) > 0 && !p.aggregated {
		if _, ok := p.src.(*tableSource); ok && isKeyRef(p.order[0].expr, p) && !p.order[0].desc {
			p.orderNoop = true
		}
	}

	cb := &binder{a: a, aggCtx: "LIMIT clause"}
	for i, x := range []astExpr{limit, offset} {
		if x == nil {
			continue
		}
		clause := []string{"LIMIT", "OFFSET"}[i]
		e, err := cb.bindExpr(x)
		if err != nil {
			return nil, err
		}
		if e, err = coerce(e, typInt64); err != nil || !isConst(e) {
			return nil, invalidf("%s expects an integer literal or parameter", clause)
		}
		if e.kind == ekConst && (e.val == nil || e.val.(int64) < 0) {
			return nil, invalidf("%s must be a non-negative, non-NULL INT64", clause)
		}
		if i == 0 {
			p.limit = e
		} else {
			p.offset = e
		}
	}
	return p, nil
}

// isKeyRef reports whether e refers to the source's _key column.
func isKeyRef(e *bexpr, p *queryPlan) bool {
	if p.scope == nil || p.scope.keyCol < 0 {
		return false
	}
	switch e.kind {
	case ekColumn:
		return e.idx == p.scope.keyCol
	case ekGroupRef:
		return isKeyRef(p.groupKeys[e.idx], p)
	}
	return false
}

func (p *queryPlan) validateKeyOrder(items []astOrderItem) error {
	if len(p.order) != 1 || !isKeyRef(p.order[0].expr, p) {
		return invalidf("GoogleSQL for Bigtable only supports ORDER BY _key [ASC]")
	}
	if p.order[0].desc {
		return invalidf("ORDER BY _key DESC is not supported; GoogleSQL for Bigtable only supports ORDER BY _key [ASC]")
	}
	if len(items) == 1 && items[0].nullsSeen {
		return invalidf("NULLS FIRST/LAST is not supported; GoogleSQL for Bigtable only supports ORDER BY _key [ASC]")
	}
	return nil
}

// mapToGroup rewrites a post-aggregation expression in terms of GROUP BY keys
// and aggregate results.
func mapToGroup(e *bexpr, keys []*bexpr, sc *scope, clause string) (*bexpr, error) {
	fp := e.fingerprint()
	for i, k := range keys {
		if k.fingerprint() == fp {
			return &bexpr{kind: ekGroupRef, idx: i, typ: e.typ, name: "group"}, nil
		}
	}
	switch e.kind {
	case ekColumn:
		name := e.name
		if sc != nil && e.idx < len(sc.cols) {
			name = sc.cols[e.idx].name
		}
		return nil, invalidf("%s expression references column %s which is neither grouped nor aggregated", clause, name)
	case ekAggRef, ekConst, ekParam, ekGroupRef:
		return e, nil
	}
	cp := *e
	cp.fp = ""
	cp.args = make([]*bexpr, len(e.args))
	for i, a := range e.args {
		m, err := mapToGroup(a, keys, sc, clause)
		if err != nil {
			return nil, err
		}
		cp.args[i] = m
	}
	return &cp, nil
}

func (a *analyzer) expandStar(b *binder, sc *scope, it astSelectItem) ([]selItem, error) {
	var items []selItem
	rangeVar := false
	if id, ok := it.starExpr.(*astIdent); ok && sc != nil && sc.alias != "" && strings.EqualFold(id.name, sc.alias) {
		if ci, _ := sc.lookup(id.name); ci < 0 {
			rangeVar = true
		}
	}
	switch {
	case it.starExpr == nil || rangeVar:
		if sc == nil {
			return nil, invalidf("SELECT * must have a FROM clause")
		}
		for i, c := range sc.cols {
			if c.hidden {
				continue
			}
			items = append(items, selItem{name: c.name, expr: &bexpr{kind: ekColumn, idx: i, typ: c.typ, name: c.name}})
		}
	default:
		x, err := b.bindExpr(it.starExpr)
		if err != nil {
			return nil, err
		}
		if x.typ.kind != tkStruct {
			return nil, invalidf("Dot-star is not supported for type %s", x.typ)
		}
		for i, f := range x.typ.fields {
			idx := i
			fe := callExpr("$field", f.typ, func(a []any) (any, error) { return a[0].(structV)[idx], nil }, x)
			fe.extra = strconv.Itoa(i)
			items = append(items, selItem{name: f.name, expr: fe})
		}
	}
	for _, ex := range it.except {
		found := false
		kept := items[:0]
		for _, item := range items {
			if strings.EqualFold(item.name, ex) {
				found = true
				continue
			}
			kept = append(kept, item)
		}
		if !found {
			return nil, invalidf("Column %s in SELECT * EXCEPT list does not exist", ex)
		}
		items = kept
	}
	for _, r := range it.replace {
		found := false
		for i := range items {
			if strings.EqualFold(items[i].name, r.name) {
				e, err := b.bindExpr(r.x)
				if err != nil {
					return nil, err
				}
				items[i].expr = e
				found = true
			}
		}
		if !found {
			return nil, invalidf("Column %s in SELECT * REPLACE list does not exist", r.name)
		}
	}
	return items, nil
}

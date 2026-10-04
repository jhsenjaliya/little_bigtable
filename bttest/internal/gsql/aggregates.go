package gsql

import (
	"math"
	"sort"
	"strings"
)

// aggState accumulates one aggregate over the rows of a group.
type aggState interface {
	add(args []any) error
	result() (any, error)
}

type orderSpec struct {
	expr      *bexpr
	desc      bool
	nullsLast bool
}

type aggSpec struct {
	name        string
	args        []*bexpr
	star        bool
	distinct    bool
	ignoreNulls bool
	orderBy     []orderSpec
	limit       *bexpr
	typ         *sqlType
	newState    func() aggState
	fp          string
}

type aggCall struct {
	name string
	ast  *astCall
	args []*bexpr
}

type aggDef struct {
	bind          func(b *binder, c *aggCall) (*aggSpec, error)
	allowDistinct bool
	allowOrder    bool // ORDER BY, LIMIT and IGNORE/RESPECT NULLS
}

var aggFuncs = map[string]*aggDef{}

// mvAggregates are the aggregates allowed in continuous materialized views.
var mvAggregates = setOf("COUNT", "SUM", "MIN", "MAX", "AVG", "ANY_VALUE", "BIT_AND", "BIT_OR", "BIT_XOR",
	"HLL_COUNT.INIT", "HLL_COUNT.MERGE", "HLL_COUNT.MERGE_PARTIAL")

func aggNoSignature(c *aggCall) error {
	return invalidf("No matching signature for aggregate function %s for argument types: %s", c.name, argTypesString(c.args))
}

func (b *binder) bindAggregate(e *astCall, def *aggDef) (*bexpr, error) {
	name := e.name
	if b.aggs == nil {
		ctx := b.aggCtx
		if ctx == "" {
			ctx = "this context"
		}
		return nil, invalidf("Aggregate function %s not allowed in %s", name, ctx)
	}
	if b.inAgg {
		return nil, invalidf("Aggregations of aggregations are not allowed")
	}
	if e.safe {
		return nil, invalidf("SAFE. prefix is not supported for aggregate function %s", name)
	}
	if b.a.mv {
		if !mvAggregates[name] {
			return nil, invalidf("Aggregate function %s is not supported in continuous materialized views", name)
		}
		if e.distinct {
			return nil, invalidf("DISTINCT aggregates are not supported in continuous materialized views")
		}
	}
	if e.distinct && !def.allowDistinct {
		return nil, invalidf("DISTINCT is not allowed for aggregate function %s", name)
	}
	if (len(e.orderBy) > 0 || e.limit != nil || e.ignoreNulls || e.respectNull) && !def.allowOrder {
		return nil, invalidf("ORDER BY, LIMIT and IGNORE/RESPECT NULLS are not allowed for aggregate function %s", name)
	}
	inner := &binder{a: b.a, scope: b.scope, aggs: b.aggs, aggCtx: b.aggCtx, inAgg: true}
	c := &aggCall{name: name, ast: e}
	if e.star {
		if name != "COUNT" {
			return nil, invalidf("%s(*) is not supported", name)
		}
	} else {
		for _, ax := range e.args {
			x, err := inner.bindExpr(ax)
			if err != nil {
				return nil, err
			}
			c.args = append(c.args, x)
		}
	}
	spec, err := def.bind(inner, c)
	if err != nil {
		return nil, err
	}
	spec.name = name
	spec.star = e.star
	spec.distinct = e.distinct
	spec.ignoreNulls = e.ignoreNulls
	if spec.distinct && len(spec.args) > 0 && !spec.args[0].typ.groupable() {
		return nil, invalidf("Aggregate functions with DISTINCT cannot be used with arguments of type %s", spec.args[0].typ)
	}
	for _, o := range e.orderBy {
		x, err := inner.bindExpr(o.x)
		if err != nil {
			return nil, err
		}
		if x.typ.kind == tkArray || !x.typ.orderable() {
			return nil, invalidf("ORDER BY in aggregate function %s does not support type %s", name, x.typ)
		}
		spec.orderBy = append(spec.orderBy, orderSpec{expr: x, desc: o.desc, nullsLast: nullsLast(o)})
	}
	if e.limit != nil {
		x, err := (&binder{a: b.a}).bindExpr(e.limit)
		if err != nil {
			return nil, err
		}
		if x, err = coerce(x, typInt64); err != nil || !isConst(x) {
			return nil, invalidf("LIMIT in aggregate function %s must be a constant INT64", name)
		}
		spec.limit = x
	}
	var sb strings.Builder
	sb.WriteString(name)
	if spec.star {
		sb.WriteString("*")
	}
	if spec.distinct {
		sb.WriteString(" DISTINCT")
	}
	if spec.ignoreNulls {
		sb.WriteString(" IGNORE NULLS")
	}
	sb.WriteString("(")
	for _, a := range spec.args {
		sb.WriteString(a.fingerprint() + ",")
	}
	for _, o := range spec.orderBy {
		sb.WriteString("ORDER:" + o.expr.fingerprint())
		if o.desc {
			sb.WriteString(" DESC")
		}
		if o.nullsLast {
			sb.WriteString(" NL")
		}
	}
	if spec.limit != nil {
		sb.WriteString("LIMIT:" + spec.limit.fingerprint())
	}
	sb.WriteString(")")
	spec.fp = sb.String()
	for i, s := range *b.aggs {
		if s.fp == spec.fp {
			return &bexpr{kind: ekAggRef, idx: i, typ: s.typ, name: s.fp}, nil
		}
	}
	*b.aggs = append(*b.aggs, spec)
	return &bexpr{kind: ekAggRef, idx: len(*b.aggs) - 1, typ: spec.typ, name: spec.fp}, nil
}

func nullsLast(o astOrderItem) bool {
	if o.nullsSeen {
		return o.nulls == "LAST"
	}
	return o.desc
}

// compareOrder compares two rows of order keys under the given specs.
func compareOrder(specs []orderSpec, a, b []any) int {
	for i, s := range specs {
		x, y := a[i], b[i]
		var c int
		switch {
		case x == nil && y == nil:
			c = 0
		case x == nil || y == nil:
			c = -1
			if y == nil {
				c = 1
			}
			if s.nullsLast {
				c = -c
			}
			if c != 0 {
				return c
			}
			continue
		default:
			c = compareValues(x, y)
		}
		if s.desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// aggRunner wraps an aggState with DISTINCT, IGNORE NULLS, ORDER BY and LIMIT.
type aggRunner struct {
	spec  *aggSpec
	st    aggState
	seen  map[string]bool
	rows  []aggRow
	limit int64
}

type aggRow struct {
	args []any
	keys []any
}

func newAggRunner(spec *aggSpec, limit int64) *aggRunner {
	r := &aggRunner{spec: spec, st: spec.newState(), limit: limit}
	if spec.distinct {
		r.seen = map[string]bool{}
	}
	return r
}

func (r *aggRunner) add(args, keys []any) error {
	if r.spec.ignoreNulls && len(args) > 0 && args[0] == nil {
		return nil
	}
	if r.seen != nil {
		if len(args) > 0 && args[0] == nil {
			return nil
		}
		k := valueKey(args[0])
		if r.seen[k] {
			return nil
		}
		r.seen[k] = true
	}
	if len(r.spec.orderBy) > 0 || r.limit >= 0 {
		r.rows = append(r.rows, aggRow{args: append([]any(nil), args...), keys: keys})
		return nil
	}
	return r.st.add(args)
}

func (r *aggRunner) result() (any, error) {
	if len(r.spec.orderBy) > 0 || r.limit >= 0 {
		if len(r.spec.orderBy) > 0 {
			sort.SliceStable(r.rows, func(i, j int) bool {
				return compareOrder(r.spec.orderBy, r.rows[i].keys, r.rows[j].keys) < 0
			})
		}
		rows := r.rows
		if r.limit >= 0 && int64(len(rows)) > r.limit {
			rows = rows[:r.limit]
		}
		for _, row := range rows {
			if err := r.st.add(row.args); err != nil {
				return nil, err
			}
		}
		r.rows = nil
	}
	return r.st.result()
}

// ---------------------------------------------------------------------------
// States.

type countState struct {
	n    int64
	star bool
	cond bool // COUNTIF
}

func (s *countState) add(a []any) error {
	switch {
	case s.star:
		s.n++
	case s.cond:
		if a[0] == true {
			s.n++
		}
	case a[0] != nil:
		s.n++
	}
	return nil
}
func (s *countState) result() (any, error) { return s.n, nil }

type sumState struct {
	isInt bool
	any   bool
	i     int64
	f     float64
}

func (s *sumState) add(a []any) error {
	if a[0] == nil {
		return nil
	}
	if s.isInt {
		r, err := addInt64(s.i, a[0].(int64))
		if err != nil {
			return evalErrorf("int64 overflow in SUM")
		}
		s.i = r.(int64)
	} else {
		s.f += a[0].(float64)
	}
	s.any = true
	return nil
}

func (s *sumState) result() (any, error) {
	if !s.any {
		return nil, nil
	}
	if s.isInt {
		return s.i, nil
	}
	return s.f, nil
}

type avgState struct {
	n   int64
	sum float64
	c   float64 // Kahan compensation
}

func (s *avgState) add(a []any) error {
	if a[0] == nil {
		return nil
	}
	x := toF64(a[0])
	y := x - s.c
	t := s.sum + y
	s.c = (t - s.sum) - y
	s.sum = t
	s.n++
	return nil
}

func (s *avgState) result() (any, error) {
	if s.n == 0 {
		return nil, nil
	}
	return s.sum / float64(s.n), nil
}

type minMaxState struct {
	max bool
	v   any
	nan bool
}

func (s *minMaxState) add(a []any) error {
	if a[0] == nil {
		return nil
	}
	if isNaNValue(a[0]) {
		s.nan = true
		return nil
	}
	if s.v == nil {
		s.v = a[0]
		return nil
	}
	c := compareValues(a[0], s.v)
	if (s.max && c > 0) || (!s.max && c < 0) {
		s.v = a[0]
	}
	return nil
}

func (s *minMaxState) result() (any, error) {
	if s.nan {
		return math.NaN(), nil
	}
	return s.v, nil
}

type anyValueState struct{ v any }

func (s *anyValueState) add(a []any) error {
	if s.v == nil && a[0] != nil {
		s.v = a[0]
	}
	return nil
}
func (s *anyValueState) result() (any, error) { return s.v, nil }

type logicalState struct {
	and  bool
	v    bool
	seen bool
}

func (s *logicalState) add(a []any) error {
	if a[0] == nil {
		return nil
	}
	b := a[0].(bool)
	if !s.seen {
		s.v, s.seen = b, true
		return nil
	}
	if s.and {
		s.v = s.v && b
	} else {
		s.v = s.v || b
	}
	return nil
}

func (s *logicalState) result() (any, error) {
	if !s.seen {
		return nil, nil
	}
	return s.v, nil
}

type bitState struct {
	op   byte
	v    int64
	seen bool
}

func (s *bitState) add(a []any) error {
	if a[0] == nil {
		return nil
	}
	x := a[0].(int64)
	if !s.seen {
		s.v, s.seen = x, true
		return nil
	}
	switch s.op {
	case '&':
		s.v &= x
	case '|':
		s.v |= x
	default:
		s.v ^= x
	}
	return nil
}

func (s *bitState) result() (any, error) {
	if !s.seen {
		return nil, nil
	}
	return s.v, nil
}

type arrayAggState struct {
	vals   arrayV
	any    bool
	concat bool
}

func (s *arrayAggState) add(a []any) error {
	if s.concat {
		if a[0] == nil {
			return nil
		}
		s.vals = append(s.vals, a[0].(arrayV)...)
		s.any = true
		return nil
	}
	s.vals = append(s.vals, a[0])
	s.any = true
	return nil
}

func (s *arrayAggState) result() (any, error) {
	if !s.any {
		return nil, nil
	}
	if s.vals == nil {
		return arrayV{}, nil
	}
	return s.vals, nil
}

type stringAggState struct {
	isStr bool
	parts [][]byte
	delim []byte
	any   bool
}

func (s *stringAggState) add(a []any) error {
	if a[0] == nil {
		return nil
	}
	if !s.any && len(a) > 1 {
		if a[1] == nil {
			return nil
		}
		s.delim = asBytes(a[1])
	}
	s.parts = append(s.parts, asBytes(a[0]))
	s.any = true
	return nil
}

func (s *stringAggState) result() (any, error) {
	if !s.any {
		return nil, nil
	}
	var out []byte
	for i, p := range s.parts {
		if i > 0 {
			out = append(out, s.delim...)
		}
		out = append(out, p...)
	}
	return asStringOrBytes(out, s.isStr), nil
}

type momentState struct {
	kind string // VAR_SAMP, VAR_POP, STDDEV_SAMP, STDDEV_POP, CORR, COVAR_POP, COVAR_SAMP
	n    float64
	mx   float64
	my   float64
	m2x  float64
	m2y  float64
	cxy  float64
}

func (s *momentState) add(a []any) error {
	if a[0] == nil || (len(a) > 1 && a[1] == nil) {
		return nil
	}
	x := toF64(a[0])
	y := x
	if len(a) > 1 {
		y = toF64(a[1])
	}
	s.n++
	dx := x - s.mx
	s.mx += dx / s.n
	dy := y - s.my
	s.my += dy / s.n
	s.m2x += dx * (x - s.mx)
	s.m2y += dy * (y - s.my)
	s.cxy += dx * (y - s.my)
	return nil
}

func (s *momentState) result() (any, error) {
	switch s.kind {
	case "VAR_POP", "STDDEV_POP":
		if s.n < 1 {
			return nil, nil
		}
		v := s.m2x / s.n
		if s.kind == "STDDEV_POP" {
			return math.Sqrt(v), nil
		}
		return v, nil
	case "VAR_SAMP", "STDDEV_SAMP":
		if s.n < 2 {
			return nil, nil
		}
		v := s.m2x / (s.n - 1)
		if s.kind == "STDDEV_SAMP" {
			return math.Sqrt(v), nil
		}
		return v, nil
	case "COVAR_POP":
		if s.n < 1 {
			return nil, nil
		}
		return s.cxy / s.n, nil
	case "COVAR_SAMP":
		if s.n < 2 {
			return nil, nil
		}
		return s.cxy / (s.n - 1), nil
	case "CORR":
		if s.n < 2 {
			return nil, nil
		}
		d := math.Sqrt(s.m2x * s.m2y)
		if d == 0 {
			return math.NaN(), nil
		}
		return s.cxy / d, nil
	}
	return nil, nil
}

type hllState struct {
	h         *HLL
	precision int
	merge     bool // inputs are sketches
	count     bool // result is the estimate (HLL_COUNT.MERGE / APPROX_COUNT_DISTINCT)
}

func (s *hllState) add(a []any) error {
	if a[0] == nil {
		return nil
	}
	if s.merge {
		o, err := DecodeHLL(a[0].([]byte))
		if err != nil {
			return evalErrorf("invalid HLL++ sketch: %v", err)
		}
		if s.h == nil {
			s.h = o
			return nil
		}
		s.h.Merge(o)
		return nil
	}
	if s.h == nil {
		s.h = newHLLPrecision(s.precision)
	}
	s.h.Add(hllItem(a[0]))
	return nil
}

func (s *hllState) result() (any, error) {
	if s.count {
		if s.h == nil {
			return int64(0), nil
		}
		return s.h.Estimate(), nil
	}
	if s.h == nil {
		return nil, nil
	}
	return s.h.Encode(), nil
}

// hllItem returns the canonical bytes hashed into HLL++ sketches.
func hllItem(v any) []byte {
	if b, ok := hllInput(v); ok {
		return b
	}
	return []byte(valueKey(v))
}

// ---------------------------------------------------------------------------
// Registration.

func simpleAgg(argTypes []*sqlType, ret *sqlType, mk func() aggState) func(b *binder, c *aggCall) (*aggSpec, error) {
	return func(b *binder, c *aggCall) (*aggSpec, error) {
		if len(c.args) != len(argTypes) {
			return nil, aggNoSignature(c)
		}
		args := make([]*bexpr, len(c.args))
		for i, a := range c.args {
			x, err := coerce(a, argTypes[i])
			if err != nil {
				return nil, aggNoSignature(c)
			}
			args[i] = x
		}
		return &aggSpec{args: args, typ: ret, newState: mk}, nil
	}
}

func init() {
	I, F, B, S := typInt64, typFloat64, typBool, typString

	aggFuncs["COUNT"] = &aggDef{allowDistinct: true, bind: func(b *binder, c *aggCall) (*aggSpec, error) {
		if c.ast.star {
			return &aggSpec{typ: I, newState: func() aggState { return &countState{star: true} }}, nil
		}
		if len(c.args) != 1 {
			return nil, aggNoSignature(c)
		}
		return &aggSpec{args: c.args, typ: I, newState: func() aggState { return &countState{} }}, nil
	}}
	aggFuncs["COUNTIF"] = &aggDef{bind: simpleAgg([]*sqlType{B}, I, func() aggState { return &countState{cond: true} })}
	aggFuncs["SUM"] = &aggDef{allowDistinct: true, bind: func(b *binder, c *aggCall) (*aggSpec, error) {
		if len(c.args) != 1 {
			return nil, aggNoSignature(c)
		}
		a := c.args[0]
		if a.typ.kind == tkInt64 || isNullLit(a) {
			x, _ := coerce(a, I)
			return &aggSpec{args: []*bexpr{x}, typ: I, newState: func() aggState { return &sumState{isInt: true} }}, nil
		}
		x, err := coerce(a, F)
		if err != nil {
			return nil, aggNoSignature(c)
		}
		return &aggSpec{args: []*bexpr{x}, typ: F, newState: func() aggState { return &sumState{} }}, nil
	}}
	aggFuncs["AVG"] = &aggDef{allowDistinct: true, bind: func(b *binder, c *aggCall) (*aggSpec, error) {
		if len(c.args) != 1 {
			return nil, aggNoSignature(c)
		}
		a := c.args[0]
		if a.typ.kind != tkInt64 && !a.typ.isFloat() && !isNullLit(a) {
			return nil, aggNoSignature(c)
		}
		if a.typ.kind == tkFloat32 {
			a, _ = coerce(a, F)
		}
		if isNullLit(a) {
			a, _ = coerce(a, I)
		}
		return &aggSpec{args: []*bexpr{a}, typ: F, newState: func() aggState { return &avgState{} }}, nil
	}}
	minMax := func(max bool) *aggDef {
		return &aggDef{allowDistinct: true, bind: func(b *binder, c *aggCall) (*aggSpec, error) {
			if len(c.args) != 1 {
				return nil, aggNoSignature(c)
			}
			a := c.args[0]
			if isNullLit(a) {
				a, _ = coerce(a, I)
			}
			if a.typ.kind == tkArray || !a.typ.orderable() {
				return nil, invalidf("%s does not support arguments of type %s", c.name, a.typ)
			}
			return &aggSpec{args: []*bexpr{a}, typ: a.typ, newState: func() aggState { return &minMaxState{max: max} }}, nil
		}}
	}
	aggFuncs["MIN"] = minMax(false)
	aggFuncs["MAX"] = minMax(true)
	aggFuncs["ANY_VALUE"] = &aggDef{bind: func(b *binder, c *aggCall) (*aggSpec, error) {
		if len(c.args) != 1 {
			return nil, aggNoSignature(c)
		}
		return &aggSpec{args: c.args, typ: c.args[0].typ, newState: func() aggState { return &anyValueState{} }}, nil
	}}
	aggFuncs["LOGICAL_AND"] = &aggDef{bind: simpleAgg([]*sqlType{B}, B, func() aggState { return &logicalState{and: true} })}
	aggFuncs["LOGICAL_OR"] = &aggDef{bind: simpleAgg([]*sqlType{B}, B, func() aggState { return &logicalState{} })}
	for name, op := range map[string]byte{"BIT_AND": '&', "BIT_OR": '|', "BIT_XOR": '^'} {
		o := op
		aggFuncs[name] = &aggDef{allowDistinct: true, bind: simpleAgg([]*sqlType{I}, I, func() aggState { return &bitState{op: o} })}
	}
	aggFuncs["ARRAY_AGG"] = &aggDef{allowDistinct: true, allowOrder: true, bind: func(b *binder, c *aggCall) (*aggSpec, error) {
		if len(c.args) != 1 {
			return nil, aggNoSignature(c)
		}
		a := c.args[0]
		if isNullLit(a) {
			a, _ = coerce(a, I)
		}
		if a.typ.kind == tkArray {
			return nil, invalidf("ARRAY_AGG cannot produce an array of arrays (argument type %s)", a.typ)
		}
		return &aggSpec{args: []*bexpr{a}, typ: arrayOf(a.typ), newState: func() aggState { return &arrayAggState{} }}, nil
	}}
	aggFuncs["ARRAY_CONCAT_AGG"] = &aggDef{allowOrder: true, bind: func(b *binder, c *aggCall) (*aggSpec, error) {
		if len(c.args) != 1 || c.args[0].typ.kind != tkArray {
			return nil, aggNoSignature(c)
		}
		return &aggSpec{args: c.args, typ: c.args[0].typ, newState: func() aggState { return &arrayAggState{concat: true} }}, nil
	}}
	aggFuncs["STRING_AGG"] = &aggDef{allowDistinct: true, allowOrder: true, bind: func(b *binder, c *aggCall) (*aggSpec, error) {
		if len(c.args) < 1 || len(c.args) > 2 {
			return nil, aggNoSignature(c)
		}
		for _, t := range []*sqlType{S, typBytes} {
			ok := true
			for _, a := range c.args {
				if !canCoerce(a, t) {
					ok = false
				}
			}
			if !ok {
				continue
			}
			args, _ := coerceAll(c.args, t)
			isStr := t.kind == tkString
			return &aggSpec{args: args, typ: t, newState: func() aggState {
				return &stringAggState{isStr: isStr, delim: []byte(",")}
			}}, nil
		}
		return nil, aggNoSignature(c)
	}}
	aggFuncs["APPROX_COUNT_DISTINCT"] = &aggDef{bind: func(b *binder, c *aggCall) (*aggSpec, error) {
		if len(c.args) != 1 || !c.args[0].typ.groupable() {
			return nil, aggNoSignature(c)
		}
		return &aggSpec{args: c.args, typ: I, newState: func() aggState { return &hllState{precision: hllDefaultPrecision, count: true} }}, nil
	}}
	aggFuncs["HLL_COUNT.INIT"] = &aggDef{bind: func(b *binder, c *aggCall) (*aggSpec, error) {
		if len(c.args) < 1 || len(c.args) > 2 {
			return nil, aggNoSignature(c)
		}
		a := c.args[0]
		if isNullLit(a) {
			a, _ = coerce(a, I)
		}
		switch a.typ.kind {
		case tkInt64, tkString, tkBytes:
		default:
			return nil, invalidf("HLL_COUNT.INIT does not support arguments of type %s", a.typ)
		}
		p := hllInitPrecision
		if len(c.args) == 2 {
			pe, err := coerce(c.args[1], I)
			if err != nil || pe.kind != ekConst || pe.val == nil {
				return nil, invalidf("HLL_COUNT.INIT precision must be an INT64 literal")
			}
			p = int(pe.val.(int64))
			if p < hllMinPrecision || p > hllMaxPrecision {
				return nil, invalidf("HLL_COUNT.INIT precision must be between %d and %d, got %d", hllMinPrecision, hllMaxPrecision, p)
			}
		}
		prec := p
		return &aggSpec{args: []*bexpr{a}, typ: typBytes, newState: func() aggState { return &hllState{precision: prec} }}, nil
	}}
	aggFuncs["HLL_COUNT.MERGE"] = &aggDef{bind: simpleAgg([]*sqlType{typBytes}, I, func() aggState { return &hllState{merge: true, count: true} })}
	aggFuncs["HLL_COUNT.MERGE_PARTIAL"] = &aggDef{bind: simpleAgg([]*sqlType{typBytes}, typBytes, func() aggState { return &hllState{merge: true} })}
	for _, name := range []string{"STDDEV", "STDDEV_SAMP", "STDDEV_POP", "VARIANCE", "VAR_SAMP", "VAR_POP"} {
		kind := map[string]string{"STDDEV": "STDDEV_SAMP", "VARIANCE": "VAR_SAMP"}[name]
		if kind == "" {
			kind = name
		}
		aggFuncs[name] = &aggDef{allowDistinct: true, bind: simpleAgg([]*sqlType{F}, F, func() aggState { return &momentState{kind: kind} })}
	}
	for _, name := range []string{"CORR", "COVAR_POP", "COVAR_SAMP"} {
		kind := name
		aggFuncs[name] = &aggDef{bind: simpleAgg([]*sqlType{F, F}, F, func() aggState { return &momentState{kind: kind} })}
	}
}

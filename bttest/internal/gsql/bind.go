package gsql

import (
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Bound expressions.

type exprKind uint8

const (
	ekConst exprKind = iota
	ekParam
	ekColumn
	ekGroupRef
	ekAggRef
	ekCall // arguments are evaluated eagerly, then fn is applied
	ekLazy // lazy controls evaluation of its arguments
)

// bexpr is a type-checked expression. Lazy evaluators receive their argument
// nodes as parameters and must never capture child nodes, so that trees can
// be rewritten (e.g. when mapping expressions onto GROUP BY keys).
type bexpr struct {
	kind   exprKind
	typ    *sqlType
	name   string // canonical operator/function name, or parameter name
	extra  string // additional fingerprint data (field index, cast target, ...)
	args   []*bexpr
	val    any // ekConst
	idx    int // column / group / aggregate / parameter index
	fn     func(args []any) (any, error)
	lazy   func(ec *evalCtx, args []*bexpr) (any, error)
	lit    litKind // literal-ness of constants, for literal coercion rules
	strict bool    // ekCall: any NULL argument yields NULL without calling fn
	safe   bool    // evaluation errors produced by this node yield NULL
	ctor   string  // "array" or "struct" for constructors (re-coercible)
	fp     string
}

type rowAccessor interface {
	column(i int) (any, error)
}

type evalCtx struct {
	row    rowAccessor
	params []any
	group  []any
	aggs   []any
	now    tsVal
}

func (e *bexpr) eval(ec *evalCtx) (any, error) {
	switch e.kind {
	case ekConst:
		return e.val, nil
	case ekParam:
		return ec.params[e.idx], nil
	case ekColumn:
		if ec.row == nil {
			return nil, internalf("column reference evaluated without a row")
		}
		return ec.row.column(e.idx)
	case ekGroupRef:
		return ec.group[e.idx], nil
	case ekAggRef:
		return ec.aggs[e.idx], nil
	case ekCall:
		var buf [4]any
		args := buf[:0]
		hasNull := false
		for _, a := range e.args {
			v, err := a.eval(ec)
			if err != nil {
				return nil, err
			}
			if v == nil {
				hasNull = true
			}
			args = append(args, v)
		}
		if hasNull && e.strict {
			return nil, nil
		}
		v, err := e.fn(args)
		if err != nil && e.safe && isEvalError(err) {
			return nil, nil
		}
		return v, err
	case ekLazy:
		v, err := e.lazy(ec, e.args)
		if err != nil && e.safe && isEvalError(err) {
			return nil, nil
		}
		return v, err
	}
	return nil, internalf("unknown expression kind")
}

func (e *bexpr) fingerprint() string {
	if e.fp != "" {
		return e.fp
	}
	var sb strings.Builder
	switch e.kind {
	case ekConst:
		sb.WriteString("K")
		sb.WriteString(e.typ.String())
		sb.WriteString(":")
		sb.WriteString(strconv.Quote(valueKey(e.val)))
	case ekParam:
		sb.WriteString("P" + strconv.Itoa(e.idx))
	case ekColumn:
		sb.WriteString("C" + strconv.Itoa(e.idx))
	case ekGroupRef:
		sb.WriteString("G" + strconv.Itoa(e.idx))
	case ekAggRef:
		sb.WriteString("A" + e.name)
	default:
		sb.WriteString(e.name)
		sb.WriteString("[" + e.extra + "](")
		for i, a := range e.args {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(a.fingerprint())
		}
		sb.WriteString(")")
		if e.safe {
			sb.WriteString("!safe")
		}
	}
	e.fp = sb.String()
	return e.fp
}

func constExpr(v any, t *sqlType, lit litKind) *bexpr {
	return &bexpr{kind: ekConst, typ: t, val: v, lit: lit}
}

func callExpr(name string, t *sqlType, fn func([]any) (any, error), args ...*bexpr) *bexpr {
	return &bexpr{kind: ekCall, name: name, typ: t, fn: fn, args: args, strict: true}
}

func lazyExpr(name string, t *sqlType, lazy func(*evalCtx, []*bexpr) (any, error), args ...*bexpr) *bexpr {
	return &bexpr{kind: ekLazy, name: name, typ: t, lazy: lazy, args: args}
}

// isConst reports whether e can be evaluated without a row (literals,
// parameters and functions thereof).
func isConst(e *bexpr) bool {
	switch e.kind {
	case ekColumn, ekGroupRef, ekAggRef:
		return false
	}
	for _, a := range e.args {
		if !isConst(a) {
			return false
		}
	}
	return true
}

func isNullLit(e *bexpr) bool { return e.kind == ekConst && e.lit == litNull }

func walk(e *bexpr, fn func(*bexpr) bool) {
	if !fn(e) {
		return
	}
	for _, a := range e.args {
		walk(a, fn)
	}
}

// ---------------------------------------------------------------------------
// Scopes.

type scopeCol struct {
	name   string
	typ    *sqlType
	hidden bool // excluded from SELECT *
}

type scope struct {
	cols   []scopeCol
	alias  string // range variable name
	keyCol int    // index of the _key column, -1 if none
	src    source
}

// lookup resolves a column name: exact match first, then case-insensitive.
func (s *scope) lookup(name string) (int, error) {
	found := -1
	for i, c := range s.cols {
		if c.name == name {
			if found >= 0 {
				return -1, invalidf("Column name %s is ambiguous", name)
			}
			found = i
		}
	}
	if found >= 0 {
		return found, nil
	}
	for i, c := range s.cols {
		if strings.EqualFold(c.name, name) {
			if found >= 0 {
				return -1, invalidf("Column name %s is ambiguous", name)
			}
			found = i
		}
	}
	return found, nil
}

// ---------------------------------------------------------------------------
// Analyzer / binder state.

type paramInfo struct {
	name string
	typ  *sqlType
}

type analyzer struct {
	cat       Catalog
	params    map[string]int
	paramList []paramInfo
	mv        bool // continuous materialized view rules
	viewDepth int
	sawNondet bool
	sawStruct bool
}

func (a *analyzer) lookupParam(name string) (int, bool) {
	if i, ok := a.params[name]; ok {
		return i, true
	}
	for n, i := range a.params {
		if strings.EqualFold(n, name) {
			return i, true
		}
	}
	return 0, false
}

type binder struct {
	a       *analyzer
	scope   *scope
	aggs    *[]*aggSpec // nil: aggregates not allowed
	aggCtx  string      // clause name used in errors when aggregates are not allowed
	inAgg   bool
	aliases func(name string) (*bexpr, bool, error)
}

func (b *binder) bindExpr(x astExpr) (*bexpr, error) {
	switch e := x.(type) {
	case *astLit:
		switch e.kind {
		case litNull:
			return constExpr(nil, typInt64, litNull), nil
		case litBool:
			return constExpr(e.val, typBool, litBool), nil
		case litInt:
			return constExpr(e.val, typInt64, litInt), nil
		case litFloat:
			return constExpr(e.val, typFloat64, litFloat), nil
		case litString:
			return constExpr(e.val, typString, litString), nil
		case litBytes:
			return constExpr(e.val, typBytes, litBytes), nil
		}
	case *astTypedLit:
		switch e.typ {
		case "DATE":
			d, err := parseDateString(e.val)
			if err != nil {
				return nil, invalidf("Invalid DATE literal '%s'", e.val)
			}
			return constExpr(d, typDate, litNone), nil
		case "TIMESTAMP":
			ts, err := parseTimestampString(e.val, utcLoc)
			if err != nil {
				return nil, invalidf("Invalid TIMESTAMP literal '%s'", e.val)
			}
			return constExpr(ts, typTimestamp, litNone), nil
		}
	case *astParam:
		i, ok := b.a.lookupParam(e.name)
		if !ok {
			return nil, invalidf("Query parameter '%s' not found", e.name)
		}
		return &bexpr{kind: ekParam, idx: i, typ: b.a.paramList[i].typ, name: e.name}, nil
	case *astIdent:
		return b.bindIdent(e)
	case *astField:
		return b.bindField(e)
	case *astIndex:
		return b.bindIndex(e)
	case *astUnary:
		return b.bindUnary(e)
	case *astBinary:
		return b.bindBinary(e)
	case *astIs:
		return b.bindIs(e)
	case *astIn:
		return b.bindIn(e)
	case *astBetween:
		return b.bindBetween(e)
	case *astLike:
		return b.bindLike(e)
	case *astCase:
		return b.bindCase(e)
	case *astCall:
		return b.bindCall(e)
	case *astCast:
		return b.bindCast(e)
	case *astExtract:
		return b.bindExtract(e)
	case *astInterval:
		return nil, invalidf("INTERVAL values are only allowed as arguments of date and timestamp functions")
	case *astArray:
		return b.bindArrayLit(e)
	case *astStruct:
		return b.bindStructLit(e)
	case *astUnsupported:
		if b.a.mv && strings.HasPrefix(e.what, "window function") {
			return nil, invalidf("OVER clauses (window functions) are not supported in continuous materialized views")
		}
		if e.message != "" {
			if e.unimpl {
				return nil, unimplementedf("%s", e.message)
			}
			return nil, invalidf("%s", e.message)
		}
		if e.unimpl {
			return nil, unimplementedf("%s is not supported by the emulator", e.what)
		}
		return nil, invalidf("%s is not supported", e.what)
	}
	return nil, internalf("unhandled expression %T", x)
}

func (b *binder) bindIdent(e *astIdent) (*bexpr, error) {
	if b.aliases != nil {
		r, ok, err := b.aliases(e.name)
		if err != nil {
			return nil, err
		}
		if ok {
			return r, nil
		}
	}
	if b.scope != nil {
		i, err := b.scope.lookup(e.name)
		if err != nil {
			return nil, err
		}
		if i >= 0 {
			return &bexpr{kind: ekColumn, idx: i, typ: b.scope.cols[i].typ, name: b.scope.cols[i].name}, nil
		}
	}
	if !e.quoted {
		switch up := strings.ToUpper(e.name); up {
		case "CURRENT_TIMESTAMP", "CURRENT_DATE":
			return b.bindCall(&astCall{name: up, pos: e.pos})
		}
	}
	if b.scope != nil && b.scope.alias != "" && strings.EqualFold(e.name, b.scope.alias) {
		return nil, unimplementedf("using the range variable %s as a value is not supported by the emulator", e.name)
	}
	return nil, invalidf("Unrecognized name: %s", e.name)
}

func (b *binder) bindField(e *astField) (*bexpr, error) {
	// Flatten a.b.c paths rooted at an identifier.
	var names []string
	var root astExpr = e
	for {
		if f, ok := root.(*astField); ok {
			names = append([]string{f.name}, names...)
			root = f.x
			continue
		}
		break
	}
	var base *bexpr
	if id, ok := root.(*astIdent); ok {
		if b.scope != nil && b.scope.alias != "" && strings.EqualFold(id.name, b.scope.alias) {
			// Prefer the range variable unless a column of the same name exists.
			if ci, _ := b.scope.lookup(id.name); ci < 0 {
				i, err := b.scope.lookup(names[0])
				if err != nil {
					return nil, err
				}
				if i < 0 {
					return nil, invalidf("Name %s not found inside %s", names[0], id.name)
				}
				base = &bexpr{kind: ekColumn, idx: i, typ: b.scope.cols[i].typ, name: b.scope.cols[i].name}
				names = names[1:]
			}
		}
		if base == nil {
			x, err := b.bindIdent(id)
			if err != nil {
				return nil, err
			}
			base = x
		}
	} else {
		x, err := b.bindExpr(root)
		if err != nil {
			return nil, err
		}
		base = x
	}
	for _, n := range names {
		x, err := fieldAccess(base, n)
		if err != nil {
			return nil, err
		}
		base = x
	}
	return base, nil
}

func fieldAccess(x *bexpr, name string) (*bexpr, error) {
	if x.typ.kind != tkStruct {
		if x.typ.kind == tkMap {
			return nil, invalidf("Cannot access field %s on a value with type %s; use a subscript such as ['%s'] to access map entries", name, x.typ, name)
		}
		return nil, invalidf("Cannot access field %s on a value with type %s", name, x.typ)
	}
	idx := -1
	for i, f := range x.typ.fields {
		if strings.EqualFold(f.name, name) {
			if idx >= 0 {
				return nil, invalidf("Struct field name %s is ambiguous", name)
			}
			idx = i
		}
	}
	if idx < 0 {
		return nil, invalidf("Field name %s does not exist in %s", name, x.typ)
	}
	i := idx
	r := callExpr("$field", x.typ.fields[i].typ, func(a []any) (any, error) {
		return a[0].(structV)[i], nil
	}, x)
	r.extra = strconv.Itoa(i)
	return r, nil
}

// ---------------------------------------------------------------------------
// Coercion.

// canCoerce reports whether e may be implicitly coerced to t.
func canCoerce(e *bexpr, t *sqlType) bool {
	if typesEqual(e.typ, t) || isNullLit(e) {
		return true
	}
	from := e.typ
	switch {
	case from.kind == tkString && t.kind == tkBytes:
		// GoogleSQL for Bigtable implicitly casts STRING to BYTES.
		return true
	case from.kind == tkInt64 && t.kind == tkFloat64:
		return true
	case from.kind == tkFloat32 && t.kind == tkFloat64:
		return true
	case from.kind == tkInt64 && t.kind == tkFloat32:
		return e.lit == litInt
	case from.kind == tkFloat64 && t.kind == tkFloat32:
		return e.lit == litFloat
	case e.lit == litString && (t.kind == tkDate || t.kind == tkTimestamp):
		return true
	case from.kind == tkArray && t.kind == tkArray:
		if e.ctor == "array" {
			for _, a := range e.args {
				if !canCoerce(a, t.elem) {
					return false
				}
			}
			return true
		}
		return canCoerce(&bexpr{typ: from.elem}, t.elem)
	case from.kind == tkStruct && t.kind == tkStruct && len(from.fields) == len(t.fields):
		for i := range from.fields {
			fe := &bexpr{typ: from.fields[i].typ}
			if e.ctor == "struct" {
				fe = e.args[i]
			}
			if !canCoerce(fe, t.fields[i].typ) {
				return false
			}
		}
		return true
	}
	return false
}

// coerce implicitly converts e to t, failing with InvalidArgument. On
// failure the original expression is returned along with the error so that
// callers can describe it.
func coerce(e *bexpr, t *sqlType) (*bexpr, error) {
	if typesEqual(e.typ, t) {
		return e, nil
	}
	if isNullLit(e) {
		return constExpr(nil, t, litNull), nil
	}
	if !canCoerce(e, t) {
		return e, invalidf("cannot implicitly coerce %s to %s", e.typ, t)
	}
	if e.kind == ekConst {
		v, err := castValue(e.val, e.typ, t, utcLoc)
		if err != nil {
			return e, invalidf("Could not cast literal %s to type %s", literalText(e.val, e.typ), t)
		}
		lit := e.lit
		if t.kind == tkDate || t.kind == tkTimestamp || t.kind == tkBytes {
			lit = litNone
			if t.kind == tkBytes && e.lit == litString {
				lit = litBytes
			}
		}
		return constExpr(v, t, lit), nil
	}
	if e.ctor == "array" && t.kind == tkArray {
		args := make([]*bexpr, len(e.args))
		for i, a := range e.args {
			c, err := coerce(a, t.elem)
			if err != nil {
				return e, err
			}
			args[i] = c
		}
		return makeArrayCtor(t, args), nil
	}
	if e.ctor == "struct" && t.kind == tkStruct {
		args := make([]*bexpr, len(e.args))
		for i, a := range e.args {
			c, err := coerce(a, t.fields[i].typ)
			if err != nil {
				return e, err
			}
			args[i] = c
		}
		return makeStructCtor(t, args), nil
	}
	return implicitCast(e, t), nil
}

func implicitCast(e *bexpr, t *sqlType) *bexpr {
	from := e.typ
	r := callExpr("$cast", t, func(a []any) (any, error) {
		return castValue(a[0], from, t, utcLoc)
	}, e)
	r.extra = t.String()
	return r
}

// mergeTypes computes the supertype of two (type, literal-kind) pairs.
func mergeTypes(a *sqlType, alit litKind, b *sqlType, blit litKind) (*sqlType, litKind, bool) {
	if typesEqual(a, b) {
		if alit == blit {
			return a, alit, true
		}
		return a, litNone, true
	}
	numRank := func(t *sqlType) int {
		switch t.kind {
		case tkInt64:
			return 1
		case tkFloat32:
			return 2
		case tkFloat64:
			return 3
		}
		return 0
	}
	if ra, rb := numRank(a), numRank(b); ra > 0 && rb > 0 {
		switch {
		case ra == 1 && rb == 2 && alit == litInt, rb == 1 && ra == 2 && blit == litInt:
			return typFloat32, litNone, true
		case ra == 3 && rb == 2 && alit == litFloat, rb == 3 && ra == 2 && blit == litFloat:
			return typFloat32, litNone, true
		}
		return typFloat64, litNone, true
	}
	switch {
	case a.kind == tkString && b.kind == tkBytes, a.kind == tkBytes && b.kind == tkString:
		return typBytes, litNone, true
	case (a.kind == tkDate || a.kind == tkTimestamp) && blit == litString:
		return a, litNone, true
	case (b.kind == tkDate || b.kind == tkTimestamp) && alit == litString:
		return b, litNone, true
	case a.kind == tkArray && b.kind == tkArray:
		e, _, ok := mergeTypes(a.elem, litNone, b.elem, litNone)
		if !ok {
			return nil, litNone, false
		}
		return arrayOf(e), litNone, true
	case a.kind == tkStruct && b.kind == tkStruct && len(a.fields) == len(b.fields):
		fs := make([]structField, len(a.fields))
		for i := range a.fields {
			ft, _, ok := mergeTypes(a.fields[i].typ, litNone, b.fields[i].typ, litNone)
			if !ok {
				return nil, litNone, false
			}
			fs[i] = structField{name: a.fields[i].name, typ: ft}
		}
		return structOf(fs...), litNone, true
	}
	return nil, litNone, false
}

// commonType returns the supertype of the given expressions (ignoring NULL
// literals). With only NULLs the result is INT64.
func commonType(exprs []*bexpr) (*sqlType, error) {
	var t *sqlType
	var lit litKind
	for _, e := range exprs {
		if isNullLit(e) {
			continue
		}
		et, elit := e.typ, e.lit
		if e.ctor == "array" && len(e.args) == 0 {
			elit = litNull // untyped empty array literal
		}
		if t == nil {
			t, lit = et, elit
			continue
		}
		if lit == litNull && t.kind == tkArray && et.kind == tkArray {
			t, lit = et, elit
			continue
		}
		if elit == litNull && et.kind == tkArray && t.kind == tkArray {
			continue
		}
		nt, nlit, ok := mergeTypes(t, lit, et, elit)
		if !ok {
			return nil, invalidf("No matching signature: incompatible types %s and %s", t, et)
		}
		t, lit = nt, nlit
	}
	if t == nil {
		return typInt64, nil
	}
	for _, e := range exprs {
		if !canCoerce(e, t) && !(e.ctor == "array" && len(e.args) == 0 && t.kind == tkArray) {
			return nil, invalidf("No matching signature: cannot coerce %s to %s", e.typ, t)
		}
	}
	return t, nil
}

func coerceAll(exprs []*bexpr, t *sqlType) ([]*bexpr, error) {
	out := make([]*bexpr, len(exprs))
	for i, e := range exprs {
		if e.ctor == "array" && len(e.args) == 0 && t.kind == tkArray {
			out[i] = makeArrayCtor(t, nil)
			continue
		}
		c, err := coerce(e, t)
		if err != nil {
			return nil, err
		}
		out[i] = c
	}
	return out, nil
}

func literalText(v any, t *sqlType) string {
	s, err := sqlLiteral(v, t)
	if err != nil {
		return "?"
	}
	return s
}

// ---------------------------------------------------------------------------
// Function calls.

type callCtx struct {
	name string
	ast  *astCall
	args []*bexpr
}

type funcDef struct {
	bind   func(b *binder, c *callCtx) (*bexpr, error)
	raw    bool // receives unbound arguments (date parts, intervals)
	nondet bool
}

var scalarFuncs = map[string]*funcDef{}

func registerFunc(name string, def *funcDef) { scalarFuncs[name] = def }

func argTypesString(args []*bexpr) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if isNullLit(a) {
			parts[i] = "NULL"
		} else {
			parts[i] = a.typ.String()
		}
	}
	return strings.Join(parts, ", ")
}

func noSignature(c *callCtx) error {
	return invalidf("No matching signature for function %s for argument types: %s", c.name, argTypesString(c.args))
}

func (b *binder) bindCall(e *astCall) (*bexpr, error) {
	name := e.name
	if def, ok := aggFuncs[name]; ok {
		return b.bindAggregate(e, def)
	}
	def, ok := scalarFuncs[name]
	if !ok {
		return nil, invalidf("Function not found: %s", name)
	}
	if e.distinct || e.star || len(e.orderBy) > 0 || e.limit != nil || e.ignoreNulls || e.respectNull {
		return nil, invalidf("%s is not an aggregate function; DISTINCT, *, ORDER BY, LIMIT and NULL handling clauses are not allowed", name)
	}
	c := &callCtx{name: name, ast: e}
	if !def.raw {
		for _, ax := range e.args {
			x, err := b.bindExpr(ax)
			if err != nil {
				return nil, err
			}
			c.args = append(c.args, x)
		}
	}
	r, err := def.bind(b, c)
	if err != nil {
		return nil, err
	}
	if def.nondet {
		b.a.sawNondet = true
	}
	if e.safe {
		if r.kind != ekCall && r.kind != ekLazy {
			return r, nil
		}
		cp := *r
		cp.safe = true
		cp.fp = ""
		r = &cp
	}
	return r, nil
}

// sig describes one overload of a scalar function.
type sig struct {
	args     []*sqlType
	minArgs  int // number of required args; 0 means len(args)
	variadic bool
	ret      *sqlType
	fn       func(args []any) (any, error)
	nullOK   bool // fn handles NULL arguments itself
}

func (s sig) matches(args []*bexpr) bool {
	n := len(args)
	req := s.minArgs
	if req == 0 {
		req = len(s.args)
	}
	if n < req || (!s.variadic && n > len(s.args)) {
		return false
	}
	for i, a := range args {
		t := s.args[min(i, len(s.args)-1)]
		if !canCoerce(a, t) {
			return false
		}
	}
	return true
}

func (s sig) build(name string, args []*bexpr) (*bexpr, error) {
	co := make([]*bexpr, len(args))
	for i, a := range args {
		c, err := coerce(a, s.args[min(i, len(s.args)-1)])
		if err != nil {
			return nil, err
		}
		co[i] = c
	}
	r := callExpr(name, s.ret, s.fn, co...)
	r.strict = !s.nullOK
	return r, nil
}

// bySigs creates a function definition that resolves the first matching
// overload. Overloads should be ordered from most to least specific.
func bySigs(ss ...sig) *funcDef {
	return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		for _, s := range ss {
			if s.matches(c.args) {
				return s.build(c.name, c.args)
			}
		}
		return nil, noSignature(c)
	}}
}

func s1(a *sqlType, ret *sqlType, fn func(any) (any, error)) sig {
	return sig{args: []*sqlType{a}, ret: ret, fn: func(x []any) (any, error) { return fn(x[0]) }}
}

func s2(a, b *sqlType, ret *sqlType, fn func(any, any) (any, error)) sig {
	return sig{args: []*sqlType{a, b}, ret: ret, fn: func(x []any) (any, error) { return fn(x[0], x[1]) }}
}

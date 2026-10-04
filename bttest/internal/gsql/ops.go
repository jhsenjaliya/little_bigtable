package gsql

import (
	"math"
	"strings"
)

// ---------------------------------------------------------------------------
// Arithmetic helpers with overflow detection.

func addInt64(a, b int64) (any, error) {
	c := a + b
	if (a^c)&(b^c) < 0 {
		return nil, evalErrorf("int64 overflow: %d + %d", a, b)
	}
	return c, nil
}

func subInt64(a, b int64) (any, error) {
	c := a - b
	if (a^b)&(a^c) < 0 {
		return nil, evalErrorf("int64 overflow: %d - %d", a, b)
	}
	return c, nil
}

func mulInt64(a, b int64) (any, error) {
	if a == 0 || b == 0 {
		return int64(0), nil
	}
	c := a * b
	if c/b != a || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return nil, evalErrorf("int64 overflow: %d * %d", a, b)
	}
	return c, nil
}

func checkFloat(r float64, inputs ...float64) (any, error) {
	if math.IsInf(r, 0) || math.IsNaN(r) {
		for _, in := range inputs {
			if math.IsInf(in, 0) || math.IsNaN(in) {
				return r, nil
			}
		}
		if math.IsNaN(r) {
			return nil, evalErrorf("floating point error: result is NaN")
		}
		return nil, evalErrorf("floating point overflow")
	}
	return r, nil
}

func toF64(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	}
	return 0
}

func arith(op string, l, r *bexpr) (*bexpr, error) {
	if (l.typ.kind == tkInt64 || isNullLit(l)) && (r.typ.kind == tkInt64 || isNullLit(r)) && op != "/" {
		l, _ = coerce(l, typInt64)
		r, _ = coerce(r, typInt64)
		var fn func(a, b int64) (any, error)
		switch op {
		case "+":
			fn = addInt64
		case "-":
			fn = subInt64
		case "*":
			fn = mulInt64
		}
		return callExpr("$"+op, typInt64, func(a []any) (any, error) { return fn(a[0].(int64), a[1].(int64)) }, l, r), nil
	}
	if !(l.typ.isNumeric() || isNullLit(l)) || !(r.typ.isNumeric() || isNullLit(r)) {
		return nil, invalidf("No matching signature for operator %s for argument types: %s, %s", op, argTypesString([]*bexpr{l}), argTypesString([]*bexpr{r}))
	}
	l, err := coerce(l, typFloat64)
	if err != nil {
		return nil, err
	}
	r, err = coerce(r, typFloat64)
	if err != nil {
		return nil, err
	}
	return callExpr("$"+op, typFloat64, func(a []any) (any, error) {
		x, y := a[0].(float64), a[1].(float64)
		switch op {
		case "+":
			return checkFloat(x+y, x, y)
		case "-":
			return checkFloat(x-y, x, y)
		case "*":
			return checkFloat(x*y, x, y)
		default:
			if y == 0 {
				return nil, evalErrorf("division by zero: %v / %v", formatFloat(x, false), formatFloat(y, false))
			}
			return checkFloat(x/y, x, y)
		}
	}, l, r), nil
}

// ---------------------------------------------------------------------------
// Unary / binary operators.

func (b *binder) bindUnary(e *astUnary) (*bexpr, error) {
	x, err := b.bindExpr(e.x)
	if err != nil {
		return nil, err
	}
	switch e.op {
	case "NOT":
		x, err = coerce(x, typBool)
		if err != nil {
			return nil, invalidf("NOT requires a BOOL operand, got %s", x.typ)
		}
		return callExpr("$not", typBool, func(a []any) (any, error) { return !a[0].(bool), nil }, x), nil
	case "+":
		if !x.typ.isNumeric() && !isNullLit(x) {
			return nil, invalidf("No matching signature for unary operator + for argument type %s", x.typ)
		}
		return x, nil
	case "-":
		if x.kind == ekConst && x.val != nil && (x.lit == litInt || x.lit == litFloat) {
			switch v := x.val.(type) {
			case int64:
				if v == math.MinInt64 {
					return nil, invalidf("int64 overflow: -(%d)", v)
				}
				return constExpr(-v, x.typ, x.lit), nil
			case float64:
				return constExpr(-v, x.typ, x.lit), nil
			}
		}
		switch {
		case x.typ.kind == tkInt64 || isNullLit(x):
			x, _ = coerce(x, typInt64)
			return callExpr("$neg", typInt64, func(a []any) (any, error) {
				v := a[0].(int64)
				if v == math.MinInt64 {
					return nil, evalErrorf("int64 overflow: -(%d)", v)
				}
				return -v, nil
			}, x), nil
		case x.typ.isFloat():
			return callExpr("$neg", x.typ, func(a []any) (any, error) { return -a[0].(float64), nil }, x), nil
		}
		return nil, invalidf("No matching signature for unary operator - for argument type %s", x.typ)
	case "~":
		switch {
		case x.typ.kind == tkInt64 || isNullLit(x):
			x, _ = coerce(x, typInt64)
			return callExpr("$bitnot", typInt64, func(a []any) (any, error) { return ^a[0].(int64), nil }, x), nil
		case x.typ.kind == tkBytes:
			return callExpr("$bitnot", typBytes, func(a []any) (any, error) {
				in := a[0].([]byte)
				out := make([]byte, len(in))
				for i, c := range in {
					out[i] = ^c
				}
				return out, nil
			}, x), nil
		}
		return nil, invalidf("No matching signature for operator ~ for argument type %s", x.typ)
	}
	return nil, internalf("unknown unary operator %s", e.op)
}

func (b *binder) bindBinary(e *astBinary) (*bexpr, error) {
	if e.op == "+" || e.op == "-" {
		if iv, ok := e.r.(*astInterval); ok {
			base, err := b.bindExpr(e.l)
			if err != nil {
				return nil, err
			}
			return b.bindIntervalArith(e.op, base, iv)
		}
		if iv, ok := e.l.(*astInterval); ok && e.op == "+" {
			base, err := b.bindExpr(e.r)
			if err != nil {
				return nil, err
			}
			return b.bindIntervalArith(e.op, base, iv)
		}
	}
	l, err := b.bindExpr(e.l)
	if err != nil {
		return nil, err
	}
	r, err := b.bindExpr(e.r)
	if err != nil {
		return nil, err
	}
	switch e.op {
	case "AND", "OR":
		l, err1 := coerce(l, typBool)
		r, err2 := coerce(r, typBool)
		if err1 != nil || err2 != nil {
			return nil, invalidf("No matching signature for operator %s for argument types: %s, %s", e.op, argTypesString([]*bexpr{l}), argTypesString([]*bexpr{r}))
		}
		if e.op == "AND" {
			return lazyExpr("$and", typBool, evalAnd, l, r), nil
		}
		return lazyExpr("$or", typBool, evalOr, l, r), nil
	case "=", "!=", "<", "<=", ">", ">=":
		return bindCompare(e.op, l, r)
	case "+", "-":
		if l.typ.kind == tkDate && (r.typ.kind == tkInt64 || isNullLit(r)) {
			return dateAddDays(e.op, l, r)
		}
		if e.op == "+" && r.typ.kind == tkDate && l.typ.kind == tkInt64 {
			return dateAddDays(e.op, r, l)
		}
		if l.typ.kind == tkDate || l.typ.kind == tkTimestamp || r.typ.kind == tkDate || r.typ.kind == tkTimestamp {
			return nil, unimplementedf("operator %s on %s and %s (INTERVAL results) is not supported by the emulator", e.op, l.typ, r.typ)
		}
		return arith(e.op, l, r)
	case "*", "/":
		return arith(e.op, l, r)
	case "||":
		return bindConcatOp(l, r)
	case "&", "|", "^":
		return bindBitwise(e.op, l, r)
	case "<<", ">>":
		return bindShift(e.op, l, r)
	}
	return nil, internalf("unknown operator %s", e.op)
}

func evalAnd(ec *evalCtx, args []*bexpr) (any, error) {
	l, err := args[0].eval(ec)
	if err != nil {
		return nil, err
	}
	if l == false {
		return false, nil
	}
	r, err := args[1].eval(ec)
	if err != nil {
		return nil, err
	}
	if r == false {
		return false, nil
	}
	if l == nil || r == nil {
		return nil, nil
	}
	return true, nil
}

func evalOr(ec *evalCtx, args []*bexpr) (any, error) {
	l, err := args[0].eval(ec)
	if err != nil {
		return nil, err
	}
	if l == true {
		return true, nil
	}
	r, err := args[1].eval(ec)
	if err != nil {
		return nil, err
	}
	if r == true {
		return true, nil
	}
	if l == nil || r == nil {
		return nil, nil
	}
	return false, nil
}

func isNaNValue(v any) bool {
	f, ok := v.(float64)
	return ok && math.IsNaN(f)
}

func bindCompare(op string, l, r *bexpr) (*bexpr, error) {
	t, err := commonType([]*bexpr{l, r})
	if err != nil {
		return nil, invalidf("No matching signature for operator %s for argument types: %s, %s", op, argTypesString([]*bexpr{l}), argTypesString([]*bexpr{r}))
	}
	if op == "=" || op == "!=" {
		if !t.equalityComparable() {
			return nil, invalidf("Equality is not defined for arguments of type %s", t)
		}
	} else if t.kind == tkArray || !t.orderable() {
		return nil, invalidf("Less than is not defined for arguments of type %s", t)
	}
	if l, err = coerce(l, t); err != nil {
		return nil, err
	}
	if r, err = coerce(r, t); err != nil {
		return nil, err
	}
	var fn func(a []any) (any, error)
	switch op {
	case "=":
		fn = func(a []any) (any, error) { return sqlEquals(a[0], a[1]), nil }
	case "!=":
		fn = func(a []any) (any, error) {
			v := sqlEquals(a[0], a[1])
			if v == nil {
				return nil, nil
			}
			return !v.(bool), nil
		}
	default:
		fn = func(a []any) (any, error) {
			if isNaNValue(a[0]) || isNaNValue(a[1]) {
				return false, nil
			}
			c := compareValues(a[0], a[1])
			switch op {
			case "<":
				return c < 0, nil
			case "<=":
				return c <= 0, nil
			case ">":
				return c > 0, nil
			}
			return c >= 0, nil
		}
	}
	name := map[string]string{"=": "$eq", "!=": "$ne", "<": "$lt", "<=": "$le", ">": "$gt", ">=": "$ge"}[op]
	return callExpr(name, typBool, fn, l, r), nil
}

func bindConcatOp(l, r *bexpr) (*bexpr, error) {
	if l.typ.kind == tkArray || r.typ.kind == tkArray {
		return bindArrayConcat("$concat_array", []*bexpr{l, r})
	}
	c := &callCtx{name: "||", args: []*bexpr{l, r}}
	for _, t := range []*sqlType{typString, typBytes} {
		if canCoerce(l, t) && canCoerce(r, t) {
			s := sig{args: []*sqlType{t}, variadic: true, ret: t, fn: concatFn(t)}
			return s.build("$concat", c.args)
		}
	}
	return nil, invalidf("No matching signature for operator || for argument types: %s", argTypesString(c.args))
}

func concatFn(t *sqlType) func([]any) (any, error) {
	if t.kind == tkString {
		return func(a []any) (any, error) {
			var sb strings.Builder
			for _, v := range a {
				sb.WriteString(v.(string))
			}
			return sb.String(), nil
		}
	}
	return func(a []any) (any, error) {
		var out []byte
		for _, v := range a {
			out = append(out, v.([]byte)...)
		}
		if out == nil {
			out = []byte{}
		}
		return out, nil
	}
}

func bindBitwise(op string, l, r *bexpr) (*bexpr, error) {
	if (l.typ.kind == tkInt64 || isNullLit(l)) && (r.typ.kind == tkInt64 || isNullLit(r)) {
		l, _ = coerce(l, typInt64)
		r, _ = coerce(r, typInt64)
		return callExpr("$bit"+op, typInt64, func(a []any) (any, error) {
			x, y := a[0].(int64), a[1].(int64)
			switch op {
			case "&":
				return x & y, nil
			case "|":
				return x | y, nil
			}
			return x ^ y, nil
		}, l, r), nil
	}
	if canCoerce(l, typBytes) && canCoerce(r, typBytes) && (l.typ.kind == tkBytes || r.typ.kind == tkBytes) {
		l, _ = coerce(l, typBytes)
		r, _ = coerce(r, typBytes)
		return callExpr("$bit"+op, typBytes, func(a []any) (any, error) {
			x, y := a[0].([]byte), a[1].([]byte)
			if len(x) != len(y) {
				return nil, evalErrorf("bitwise operator %s requires BYTES arguments of equal length (%d vs %d)", op, len(x), len(y))
			}
			out := make([]byte, len(x))
			for i := range x {
				switch op {
				case "&":
					out[i] = x[i] & y[i]
				case "|":
					out[i] = x[i] | y[i]
				default:
					out[i] = x[i] ^ y[i]
				}
			}
			return out, nil
		}, l, r), nil
	}
	return nil, invalidf("No matching signature for operator %s for argument types: %s, %s", op, argTypesString([]*bexpr{l}), argTypesString([]*bexpr{r}))
}

func bindShift(op string, l, r *bexpr) (*bexpr, error) {
	r, err := coerce(r, typInt64)
	if err != nil {
		return nil, invalidf("No matching signature for operator %s: shift amount must be INT64", op)
	}
	switch {
	case l.typ.kind == tkInt64 || isNullLit(l):
		l, _ = coerce(l, typInt64)
		return callExpr("$shift"+op, typInt64, func(a []any) (any, error) {
			x, n := a[0].(int64), a[1].(int64)
			if n < 0 {
				return nil, evalErrorf("bit shift by a negative amount: %d", n)
			}
			if n >= 64 {
				return int64(0), nil
			}
			if op == "<<" {
				return int64(uint64(x) << uint(n)), nil
			}
			return int64(uint64(x) >> uint(n)), nil
		}, l, r), nil
	case l.typ.kind == tkBytes:
		return callExpr("$shift"+op, typBytes, func(a []any) (any, error) {
			x, n := a[0].([]byte), a[1].(int64)
			if n < 0 {
				return nil, evalErrorf("bit shift by a negative amount: %d", n)
			}
			out := make([]byte, len(x))
			total := int64(len(x)) * 8
			for bit := int64(0); bit < total; bit++ {
				var src int64
				if op == "<<" {
					src = bit + n
				} else {
					src = bit - n
				}
				if src < 0 || src >= total {
					continue
				}
				if x[src/8]&(0x80>>(src%8)) != 0 {
					out[bit/8] |= 0x80 >> (bit % 8)
				}
			}
			return out, nil
		}, l, r), nil
	}
	return nil, invalidf("No matching signature for operator %s for argument type %s", op, l.typ)
}

// ---------------------------------------------------------------------------
// IS / IN / BETWEEN / LIKE / CASE.

func (b *binder) bindIs(e *astIs) (*bexpr, error) {
	x, err := b.bindExpr(e.x)
	if err != nil {
		return nil, err
	}
	not := e.not
	switch e.what {
	case "NULL":
		r := callExpr("$is_null", typBool, func(a []any) (any, error) { return (a[0] == nil) != not, nil }, x)
		r.strict = false
		if not {
			r.name = "$is_not_null"
		}
		return r, nil
	case "TRUE", "FALSE":
		x, err = coerce(x, typBool)
		if err != nil {
			return nil, invalidf("IS %s requires a BOOL operand", e.what)
		}
		want := e.what == "TRUE"
		r := callExpr("$is_"+strings.ToLower(e.what), typBool, func(a []any) (any, error) {
			return (a[0] == want) != not, nil
		}, x)
		r.strict = false
		if not {
			r.name = "$is_not_" + strings.ToLower(e.what)
		}
		return r, nil
	}
	return nil, internalf("bad IS")
}

func (b *binder) bindIn(e *astIn) (*bexpr, error) {
	x, err := b.bindExpr(e.x)
	if err != nil {
		return nil, err
	}
	not := e.not
	if e.unnest != nil {
		arr, err := b.bindExpr(e.unnest)
		if err != nil {
			return nil, err
		}
		if arr.typ.kind != tkArray && !isNullLit(arr) {
			return nil, invalidf("UNNEST requires an ARRAY argument, got %s", arr.typ)
		}
		if isNullLit(arr) {
			arr = constExpr(nil, arrayOf(x.typ), litNull)
		}
		t, err := commonType([]*bexpr{x, {typ: arr.typ.elem}})
		if err != nil {
			return nil, err
		}
		if !t.equalityComparable() {
			return nil, invalidf("IN is not defined for arguments of type %s", t)
		}
		if x, err = coerce(x, t); err != nil {
			return nil, err
		}
		if arr, err = coerce(arr, arrayOf(t)); err != nil {
			return nil, err
		}
		name := "$in_unnest"
		if not {
			name = "$not_in_unnest"
		}
		r := lazyExpr(name, typBool, func(ec *evalCtx, args []*bexpr) (any, error) {
			xv, err := args[0].eval(ec)
			if err != nil {
				return nil, err
			}
			av, err := args[1].eval(ec)
			if err != nil {
				return nil, err
			}
			if av == nil || len(av.(arrayV)) == 0 {
				return not, nil
			}
			res := inResult(xv, av.(arrayV))
			if res == nil {
				return nil, nil
			}
			return res.(bool) != not, nil
		}, x, arr)
		return r, nil
	}
	all := []*bexpr{x}
	for _, it := range e.list {
		v, err := b.bindExpr(it)
		if err != nil {
			return nil, err
		}
		all = append(all, v)
	}
	t, err := commonType(all)
	if err != nil {
		return nil, err
	}
	if !t.equalityComparable() {
		return nil, invalidf("IN is not defined for arguments of type %s", t)
	}
	if all, err = coerceAll(all, t); err != nil {
		return nil, err
	}
	name := "$in"
	if not {
		name = "$not_in"
	}
	return lazyExpr(name, typBool, func(ec *evalCtx, args []*bexpr) (any, error) {
		xv, err := args[0].eval(ec)
		if err != nil {
			return nil, err
		}
		vals := make(arrayV, len(args)-1)
		for i, a := range args[1:] {
			v, err := a.eval(ec)
			if err != nil {
				return nil, err
			}
			vals[i] = v
		}
		res := inResult(xv, vals)
		if res == nil {
			return nil, nil
		}
		return res.(bool) != not, nil
	}, all...), nil
}

func inResult(x any, vals arrayV) any {
	if x == nil {
		return nil
	}
	sawNull := false
	for _, v := range vals {
		switch sqlEquals(x, v) {
		case true:
			return true
		case nil:
			sawNull = true
		}
	}
	if sawNull {
		return nil
	}
	return false
}

func (b *binder) bindBetween(e *astBetween) (*bexpr, error) {
	x, err := b.bindExpr(e.x)
	if err != nil {
		return nil, err
	}
	lo, err := b.bindExpr(e.lo)
	if err != nil {
		return nil, err
	}
	hi, err := b.bindExpr(e.hi)
	if err != nil {
		return nil, err
	}
	all := []*bexpr{x, lo, hi}
	t, err := commonType(all)
	if err != nil {
		return nil, err
	}
	if t.kind == tkArray || !t.orderable() {
		return nil, invalidf("BETWEEN is not defined for arguments of type %s", t)
	}
	if all, err = coerceAll(all, t); err != nil {
		return nil, err
	}
	not := e.not
	name := "$between"
	if not {
		name = "$not_between"
	}
	return lazyExpr(name, typBool, func(ec *evalCtx, args []*bexpr) (any, error) {
		vals := [3]any{}
		for i, a := range args {
			v, err := a.eval(ec)
			if err != nil {
				return nil, err
			}
			vals[i] = v
		}
		cmp := func(a, b any, wantLE bool) any {
			if a == nil || b == nil {
				return nil
			}
			if isNaNValue(a) || isNaNValue(b) {
				return false
			}
			return compareValues(a, b) <= 0
		}
		ge := cmp(vals[1], vals[0], true)
		le := cmp(vals[0], vals[2], true)
		var res any
		switch {
		case ge == false || le == false:
			res = false
		case ge == nil || le == nil:
			res = nil
		default:
			res = true
		}
		if res == nil {
			return nil, nil
		}
		return res.(bool) != not, nil
	}, all...), nil
}

func (b *binder) bindLike(e *astLike) (*bexpr, error) {
	x, err := b.bindExpr(e.x)
	if err != nil {
		return nil, err
	}
	p, err := b.bindExpr(e.pattern)
	if err != nil {
		return nil, err
	}
	var t *sqlType
	switch {
	case canCoerce(x, typString) && canCoerce(p, typString):
		t = typString
	case canCoerce(x, typBytes) && canCoerce(p, typBytes):
		t = typBytes
	default:
		return nil, invalidf("No matching signature for operator LIKE for argument types: %s, %s", argTypesString([]*bexpr{x}), argTypesString([]*bexpr{p}))
	}
	x, _ = coerce(x, t)
	p, _ = coerce(p, t)
	not := e.not
	var cached *likePattern
	if p.kind == ekConst && p.val != nil {
		lp, err := compileLike(p.val, t.kind == tkString)
		if err != nil {
			return nil, invalidf("%v", err)
		}
		cached = lp
	}
	isStr := t.kind == tkString
	name := "$like"
	if not {
		name = "$not_like"
	}
	return callExpr(name, typBool, func(a []any) (any, error) {
		lp := cached
		if lp == nil {
			var err error
			if lp, err = compileLike(a[1], isStr); err != nil {
				return nil, err
			}
		}
		return lp.match(a[0]) != not, nil
	}, x, p), nil
}

// likePattern is a compiled LIKE pattern over runes (STRING) or bytes (BYTES).
type likePattern struct {
	isStr bool
	toks  []likeTok
}

type likeTok struct {
	kind byte // 'l' literal, '_' any single, '%' any sequence
	r    rune
}

func compileLike(pat any, isStr bool) (*likePattern, error) {
	var elems []rune
	if isStr {
		elems = []rune(pat.(string))
	} else {
		for _, c := range pat.([]byte) {
			elems = append(elems, rune(c))
		}
	}
	lp := &likePattern{isStr: isStr}
	for i := 0; i < len(elems); i++ {
		c := elems[i]
		switch c {
		case '\\':
			if i+1 >= len(elems) {
				return nil, evalErrorf("LIKE pattern ends with a backslash")
			}
			i++
			lp.toks = append(lp.toks, likeTok{kind: 'l', r: elems[i]})
		case '%':
			if n := len(lp.toks); n > 0 && lp.toks[n-1].kind == '%' {
				continue
			}
			lp.toks = append(lp.toks, likeTok{kind: '%'})
		case '_':
			lp.toks = append(lp.toks, likeTok{kind: '_'})
		default:
			lp.toks = append(lp.toks, likeTok{kind: 'l', r: c})
		}
	}
	return lp, nil
}

func (lp *likePattern) match(v any) bool {
	var s []rune
	if lp.isStr {
		s = []rune(v.(string))
	} else {
		for _, c := range v.([]byte) {
			s = append(s, rune(c))
		}
	}
	// Iterative wildcard matching with backtracking to the last '%'.
	si, pi := 0, 0
	starP, starS := -1, 0
	for si < len(s) {
		if pi < len(lp.toks) {
			t := lp.toks[pi]
			switch {
			case t.kind == '%':
				starP, starS = pi, si
				pi++
				continue
			case t.kind == '_' || (t.kind == 'l' && t.r == s[si]):
				si++
				pi++
				continue
			}
		}
		if starP >= 0 {
			pi = starP + 1
			starS++
			si = starS
			continue
		}
		return false
	}
	for pi < len(lp.toks) && lp.toks[pi].kind == '%' {
		pi++
	}
	return pi == len(lp.toks)
}

// likePrefix returns the literal prefix of a LIKE pattern (used for key range
// pushdown) and whether the pattern is exactly that literal.
func likePrefix(pat []byte) (prefix []byte, exact bool) {
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch c {
		case '\\':
			if i+1 < len(pat) {
				i++
				prefix = append(prefix, pat[i])
				continue
			}
			return prefix, false
		case '%', '_':
			return prefix, false
		}
		prefix = append(prefix, c)
	}
	return prefix, true
}

func (b *binder) bindCase(e *astCase) (*bexpr, error) {
	var results []*bexpr
	var conds []*bexpr
	var operand *bexpr
	if e.operand != nil {
		op, err := b.bindExpr(e.operand)
		if err != nil {
			return nil, err
		}
		operand = op
	}
	for _, w := range e.whens {
		c, err := b.bindExpr(w.cond)
		if err != nil {
			return nil, err
		}
		r, err := b.bindExpr(w.result)
		if err != nil {
			return nil, err
		}
		conds = append(conds, c)
		results = append(results, r)
	}
	if e.els != nil {
		r, err := b.bindExpr(e.els)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	} else {
		results = append(results, constExpr(nil, typInt64, litNull))
	}
	rt, err := commonType(results)
	if err != nil {
		return nil, invalidf("CASE results have incompatible types: %v", err)
	}
	if results, err = coerceAll(results, rt); err != nil {
		return nil, err
	}
	n := len(conds)
	if operand != nil {
		all := append([]*bexpr{operand}, conds...)
		ct, err := commonType(all)
		if err != nil {
			return nil, err
		}
		if !ct.equalityComparable() {
			return nil, invalidf("CASE operand of type %s cannot be compared", ct)
		}
		if all, err = coerceAll(all, ct); err != nil {
			return nil, err
		}
		args := append(all, results...)
		// args: operand, conds[n], results[n], else
		return lazyExpr("$case_value", rt, func(ec *evalCtx, a []*bexpr) (any, error) {
			ov, err := a[0].eval(ec)
			if err != nil {
				return nil, err
			}
			for i := 0; i < n; i++ {
				cv, err := a[1+i].eval(ec)
				if err != nil {
					return nil, err
				}
				if sqlEquals(ov, cv) == true {
					return a[1+n+i].eval(ec)
				}
			}
			return a[1+2*n].eval(ec)
		}, args...), nil
	}
	for i, c := range conds {
		cc, err := coerce(c, typBool)
		if err != nil {
			return nil, invalidf("CASE WHEN condition must be BOOL, got %s", c.typ)
		}
		conds[i] = cc
	}
	args := append(conds, results...)
	return lazyExpr("$case", rt, func(ec *evalCtx, a []*bexpr) (any, error) {
		for i := 0; i < n; i++ {
			cv, err := a[i].eval(ec)
			if err != nil {
				return nil, err
			}
			if cv == true {
				return a[n+i].eval(ec)
			}
		}
		return a[2*n].eval(ec)
	}, args...), nil
}

// ---------------------------------------------------------------------------
// Subscripts and constructors.

func (b *binder) bindIndex(e *astIndex) (*bexpr, error) {
	x, err := b.bindExpr(e.x)
	if err != nil {
		return nil, err
	}
	idx, err := b.bindExpr(e.idx)
	if err != nil {
		return nil, err
	}
	switch x.typ.kind {
	case tkArray:
		mode := e.mode
		if mode == "" {
			mode = "OFFSET"
		}
		if mode == "KEY" || mode == "SAFE_KEY" {
			return nil, invalidf("%s cannot be used to access an ARRAY", mode)
		}
		idx, err = coerce(idx, typInt64)
		if err != nil {
			return nil, invalidf("Array position in [] must be coercible to INT64, got %s", idx.typ)
		}
		safe := strings.HasPrefix(mode, "SAFE_")
		ordinal := strings.HasSuffix(mode, "ORDINAL")
		r := callExpr("$array_at", x.typ.elem, func(a []any) (any, error) {
			arr, i := a[0].(arrayV), a[1].(int64)
			if ordinal {
				i--
			}
			if i < 0 || i >= int64(len(arr)) {
				if safe {
					return nil, nil
				}
				if ordinal {
					return nil, evalErrorf("Array index %d is out of bounds", i+1)
				}
				return nil, evalErrorf("Array index %d is out of bounds", i)
			}
			return arr[i], nil
		}, x, idx)
		r.extra = mode
		return r, nil
	case tkMap:
		mode := e.mode
		switch mode {
		case "", "KEY", "SAFE_KEY":
		default:
			return nil, invalidf("%s cannot be used to access a MAP; use a key subscript", mode)
		}
		idx, err = coerce(idx, x.typ.key)
		if err != nil {
			return nil, invalidf("Map subscript must be coercible to %s, got %s", x.typ.key, idx.typ)
		}
		r := callExpr("$map_at", x.typ.val, func(a []any) (any, error) {
			v, ok := a[0].(*mapV).lookup(a[1])
			if !ok && mode == "KEY" {
				return nil, evalErrorf("Key not found in map")
			}
			return v, nil
		}, x, idx)
		r.extra = mode
		return r, nil
	}
	if isNullLit(x) {
		return constExpr(nil, typInt64, litNull), nil
	}
	return nil, invalidf("Subscript access is not supported for values of type %s", x.typ)
}

func makeArrayCtor(t *sqlType, args []*bexpr) *bexpr {
	r := callExpr("$array", t, func(a []any) (any, error) {
		out := make(arrayV, len(a))
		copy(out, a)
		return out, nil
	}, args...)
	r.strict = false
	r.ctor = "array"
	r.extra = t.String()
	return r
}

func makeStructCtor(t *sqlType, args []*bexpr) *bexpr {
	r := callExpr("$struct", t, func(a []any) (any, error) {
		out := make(structV, len(a))
		copy(out, a)
		return out, nil
	}, args...)
	r.strict = false
	r.ctor = "struct"
	r.extra = t.String()
	return r
}

func (b *binder) bindArrayLit(e *astArray) (*bexpr, error) {
	var elems []*bexpr
	for _, x := range e.elems {
		v, err := b.bindExpr(x)
		if err != nil {
			return nil, err
		}
		elems = append(elems, v)
	}
	var et *sqlType
	if e.elemType != nil {
		t, err := resolveType(e.elemType)
		if err != nil {
			return nil, err
		}
		et = t
	} else {
		if len(elems) == 0 {
			// Untyped empty array; coerced to the required type by context.
			return makeArrayCtor(arrayOf(typInt64), nil), nil
		}
		t, err := commonType(elems)
		if err != nil {
			return nil, invalidf("Array elements of types {%s} do not have a common supertype", argTypesString(elems))
		}
		et = t
	}
	if et.kind == tkArray {
		return nil, invalidf("Arrays of arrays are not supported")
	}
	co, err := coerceAll(elems, et)
	if err != nil {
		return nil, err
	}
	r := makeArrayCtor(arrayOf(et), co)
	if e.elemType != nil && len(co) == 0 {
		r.ctor = "typed_array"
	}
	return r, nil
}

func (b *binder) bindStructLit(e *astStruct) (*bexpr, error) {
	b.a.sawStruct = true
	var fields []*bexpr
	for _, x := range e.fields {
		v, err := b.bindExpr(x)
		if err != nil {
			return nil, err
		}
		fields = append(fields, v)
	}
	if e.typ != nil {
		t, err := resolveType(e.typ)
		if err != nil {
			return nil, err
		}
		if len(t.fields) != len(fields) {
			return nil, invalidf("STRUCT type has %d fields but %d values were provided", len(t.fields), len(fields))
		}
		for i := range fields {
			c, err := coerce(fields[i], t.fields[i].typ)
			if err != nil {
				return nil, err
			}
			fields[i] = c
		}
		return makeStructCtor(t, fields), nil
	}
	fs := make([]structField, len(fields))
	for i, f := range fields {
		fs[i] = structField{name: e.names[i], typ: f.typ}
	}
	return makeStructCtor(structOf(fs...), fields), nil
}

// resolveType converts a parsed type to an internal type.
func resolveType(t *astType) (*sqlType, error) {
	switch t.name {
	case "INT64", "INT", "INTEGER", "BIGINT", "SMALLINT", "TINYINT", "BYTEINT":
		return typInt64, nil
	case "FLOAT64", "DOUBLE":
		return typFloat64, nil
	case "FLOAT32":
		return typFloat32, nil
	case "BOOL", "BOOLEAN":
		return typBool, nil
	case "STRING":
		return typString, nil
	case "BYTES":
		return typBytes, nil
	case "DATE":
		return typDate, nil
	case "TIMESTAMP":
		return typTimestamp, nil
	case "ARRAY":
		e, err := resolveType(t.elem)
		if err != nil {
			return nil, err
		}
		if e.kind == tkArray {
			return nil, invalidf("Arrays of arrays are not supported")
		}
		return arrayOf(e), nil
	case "MAP":
		k, err := resolveType(t.key)
		if err != nil {
			return nil, err
		}
		v, err := resolveType(t.val)
		if err != nil {
			return nil, err
		}
		return mapOf(k, v), nil
	case "STRUCT":
		fs := make([]structField, len(t.fields))
		for i, f := range t.fields {
			ft, err := resolveType(f.typ)
			if err != nil {
				return nil, err
			}
			fs[i] = structField{name: f.name, typ: ft}
		}
		return structOf(fs...), nil
	case "NUMERIC", "BIGNUMERIC", "DECIMAL", "BIGDECIMAL", "JSON", "DATETIME", "TIME", "INTERVAL", "GEOGRAPHY", "RANGE":
		return nil, unimplementedf("type %s is not supported by the emulator", t.name)
	}
	return nil, invalidf("Type not found: %s", t.name)
}

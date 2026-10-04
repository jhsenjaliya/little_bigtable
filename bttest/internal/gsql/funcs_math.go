package gsql

import (
	"math"
	"math/rand/v2"
)

func f1(fn func(float64) (any, error)) sig {
	return sig{args: []*sqlType{typFloat64}, ret: typFloat64, fn: func(a []any) (any, error) { return fn(a[0].(float64)) }}
}

func f2(fn func(x, y float64) (any, error)) sig {
	return sig{args: []*sqlType{typFloat64, typFloat64}, ret: typFloat64, fn: func(a []any) (any, error) {
		return fn(a[0].(float64), a[1].(float64))
	}}
}

// mathFn wraps a float function with GoogleSQL error semantics: finite inputs
// producing non-finite outputs are errors.
func mathFn(name string, fn func(float64) float64) sig {
	return f1(func(x float64) (any, error) {
		r := fn(x)
		if math.IsNaN(r) && !math.IsNaN(x) {
			return nil, evalErrorf("Argument %s to %s is out of the function domain", formatFloat(x, false), name)
		}
		return checkFloat(r, x)
	})
}

func roundHalfAway(x float64, digits int64) (any, error) {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x, nil
	}
	if digits > 330 {
		return x, nil
	}
	if digits < -330 {
		return 0.0, nil
	}
	p := math.Pow(10, float64(digits))
	var r float64
	if digits >= 0 {
		r = math.Round(x*p) / p
		if math.IsInf(x*p, 0) {
			r = x
		}
	} else {
		r = math.Round(x/math.Pow(10, float64(-digits))) * math.Pow(10, float64(-digits))
	}
	return checkFloat(r, x)
}

func truncDigits(x float64, digits int64) (any, error) {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x, nil
	}
	if digits > 330 {
		return x, nil
	}
	if digits < -330 {
		return 0.0, nil
	}
	if digits >= 0 {
		p := math.Pow(10, float64(digits))
		if math.IsInf(x*p, 0) {
			return x, nil
		}
		return math.Trunc(x*p) / p, nil
	}
	p := math.Pow(10, float64(-digits))
	return math.Trunc(x/p) * p, nil
}

func init() {
	I, F, F32 := typInt64, typFloat64, typFloat32

	registerFunc("ABS", bySigs(
		s1(I, I, func(v any) (any, error) {
			x := v.(int64)
			if x == math.MinInt64 {
				return nil, evalErrorf("int64 overflow: ABS(%d)", x)
			}
			if x < 0 {
				return -x, nil
			}
			return x, nil
		}),
		s1(F32, F32, func(v any) (any, error) { return math.Abs(v.(float64)), nil }),
		s1(F, F, func(v any) (any, error) { return math.Abs(v.(float64)), nil }),
	))
	signF := func(v any) (any, error) {
		x := v.(float64)
		switch {
		case math.IsNaN(x):
			return x, nil
		case x > 0:
			return 1.0, nil
		case x < 0:
			return -1.0, nil
		}
		return 0.0, nil
	}
	registerFunc("SIGN", bySigs(
		s1(I, I, func(v any) (any, error) {
			x := v.(int64)
			switch {
			case x > 0:
				return int64(1), nil
			case x < 0:
				return int64(-1), nil
			}
			return int64(0), nil
		}),
		s1(F32, F32, signF),
		s1(F, F, signF),
	))
	registerFunc("MOD", bySigs(s2(I, I, I, func(a, b any) (any, error) {
		x, y := a.(int64), b.(int64)
		if y == 0 {
			return nil, evalErrorf("division by zero: MOD(%d, %d)", x, y)
		}
		if y == -1 {
			return int64(0), nil
		}
		return x % y, nil
	})))
	registerFunc("DIV", bySigs(s2(I, I, I, func(a, b any) (any, error) {
		x, y := a.(int64), b.(int64)
		if y == 0 {
			return nil, evalErrorf("division by zero: DIV(%d, %d)", x, y)
		}
		if x == math.MinInt64 && y == -1 {
			return nil, evalErrorf("int64 overflow: DIV(%d, %d)", x, y)
		}
		return x / y, nil
	})))
	registerFunc("ROUND", bySigs(sig{args: []*sqlType{F, I}, minArgs: 1, ret: F, fn: func(a []any) (any, error) {
		d := int64(0)
		if len(a) > 1 {
			d = a[1].(int64)
		}
		return roundHalfAway(a[0].(float64), d)
	}}))
	registerFunc("TRUNC", bySigs(sig{args: []*sqlType{F, I}, minArgs: 1, ret: F, fn: func(a []any) (any, error) {
		d := int64(0)
		if len(a) > 1 {
			d = a[1].(int64)
		}
		return truncDigits(a[0].(float64), d)
	}}))
	ceil := bySigs(f1(func(x float64) (any, error) { return math.Ceil(x), nil }))
	registerFunc("CEIL", ceil)
	registerFunc("CEILING", ceil)
	registerFunc("FLOOR", bySigs(f1(func(x float64) (any, error) { return math.Floor(x), nil })))
	registerFunc("SQRT", bySigs(f1(func(x float64) (any, error) {
		if x < 0 {
			return nil, evalErrorf("Argument to SQRT cannot be negative: %s", formatFloat(x, false))
		}
		return math.Sqrt(x), nil
	})))
	pow := bySigs(f2(func(x, y float64) (any, error) {
		if x == 0 && y < 0 {
			return nil, evalErrorf("Division by zero: POW(%s, %s)", formatFloat(x, false), formatFloat(y, false))
		}
		r := math.Pow(x, y)
		if math.IsNaN(r) && !math.IsNaN(x) && !math.IsNaN(y) {
			return nil, evalErrorf("Domain error: POW(%s, %s)", formatFloat(x, false), formatFloat(y, false))
		}
		return checkFloat(r, x, y)
	}))
	registerFunc("POW", pow)
	registerFunc("POWER", pow)
	registerFunc("EXP", bySigs(mathFn("EXP", math.Exp)))
	logDomain := func(name string, fn func(float64) float64) sig {
		return f1(func(x float64) (any, error) {
			if x <= 0 {
				return nil, evalErrorf("Argument to %s must be positive: %s", name, formatFloat(x, false))
			}
			return checkFloat(fn(x), x)
		})
	}
	registerFunc("LN", bySigs(logDomain("LN", math.Log)))
	registerFunc("LOG10", bySigs(logDomain("LOG10", math.Log10)))
	registerFunc("LOG", bySigs(
		logDomain("LOG", math.Log),
		f2(func(x, base float64) (any, error) {
			if x <= 0 || base <= 0 || base == 1 {
				return nil, evalErrorf("Domain error: LOG(%s, %s)", formatFloat(x, false), formatFloat(base, false))
			}
			return checkFloat(math.Log(x)/math.Log(base), x, base)
		}),
	))
	registerFunc("IEEE_DIVIDE", bySigs(f2(func(x, y float64) (any, error) { return x / y, nil })))
	registerFunc("IS_INF", bySigs(s1(F, typBool, func(v any) (any, error) { return math.IsInf(v.(float64), 0), nil })))
	registerFunc("IS_NAN", bySigs(s1(F, typBool, func(v any) (any, error) { return math.IsNaN(v.(float64)), nil })))

	safeBin := func(op string) *funcDef {
		return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
			if len(c.args) != 2 {
				return nil, noSignature(c)
			}
			r, err := arith(op, c.args[0], c.args[1])
			if err != nil {
				return nil, noSignature(c)
			}
			r.name = c.name
			r.safe = true
			return r, nil
		}}
	}
	registerFunc("SAFE_ADD", safeBin("+"))
	registerFunc("SAFE_SUBTRACT", safeBin("-"))
	registerFunc("SAFE_MULTIPLY", safeBin("*"))
	registerFunc("SAFE_DIVIDE", safeBin("/"))
	registerFunc("SAFE_NEGATE", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if len(c.args) != 1 {
			return nil, noSignature(c)
		}
		r, err := b.negate(c.args[0])
		if err != nil {
			return nil, noSignature(c)
		}
		r.safe = true
		return r, nil
	}})

	for name, fn := range map[string]func(float64) float64{
		"SIN": math.Sin, "COS": math.Cos, "TAN": math.Tan, "ASIN": math.Asin, "ACOS": math.Acos,
		"ATAN": math.Atan, "SINH": math.Sinh, "COSH": math.Cosh, "TANH": math.Tanh,
		"ASINH": math.Asinh, "ACOSH": math.Acosh, "ATANH": math.Atanh,
		"COT":  func(x float64) float64 { return 1 / math.Tan(x) },
		"COTH": func(x float64) float64 { return 1 / math.Tanh(x) },
		"CSC":  func(x float64) float64 { return 1 / math.Sin(x) },
		"CSCH": func(x float64) float64 { return 1 / math.Sinh(x) },
		"SEC":  func(x float64) float64 { return 1 / math.Cos(x) },
		"SECH": func(x float64) float64 { return 1 / math.Cosh(x) },
	} {
		registerFunc(name, bySigs(mathFn(name, fn)))
	}
	registerFunc("ATAN2", bySigs(f2(func(y, x float64) (any, error) { return math.Atan2(y, x), nil })))
	registerFunc("PI", bySigs(sig{args: []*sqlType{}, ret: F, fn: func(a []any) (any, error) { return math.Pi, nil }}))
	registerFunc("RAND", &funcDef{nondet: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if len(c.args) != 0 {
			return nil, noSignature(c)
		}
		r := callExpr("RAND", F, func(a []any) (any, error) { return rand.Float64(), nil })
		r.extra = "nondet"
		return r, nil
	}})

	greatest := func(name string, want int) *funcDef {
		return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
			if len(c.args) == 0 {
				return nil, noSignature(c)
			}
			t, err := commonType(c.args)
			if err != nil {
				return nil, noSignature(c)
			}
			if t.kind == tkArray || !t.orderable() {
				return nil, invalidf("%s is not defined for arguments of type %s", name, t)
			}
			args, err := coerceAll(c.args, t)
			if err != nil {
				return nil, err
			}
			return callExpr(name, t, func(a []any) (any, error) {
				best := a[0]
				for _, v := range a {
					if isNaNValue(v) {
						return v, nil
					}
					if compareValues(v, best)*want > 0 {
						best = v
					}
				}
				return best, nil
			}, args...), nil
		}}
	}
	registerFunc("GREATEST", greatest("GREATEST", 1))
	registerFunc("LEAST", greatest("LEAST", -1))
}

func (b *binder) negate(x *bexpr) (*bexpr, error) {
	return b.bindUnaryExpr("-", x)
}

// bindUnaryExpr applies unary minus to an already bound expression.
func (b *binder) bindUnaryExpr(op string, x *bexpr) (*bexpr, error) {
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
	return nil, invalidf("No matching signature for unary operator %s for argument type %s", op, x.typ)
}

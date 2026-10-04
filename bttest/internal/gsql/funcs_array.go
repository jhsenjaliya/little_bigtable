package gsql

import (
	"bytes"
	"math"
)

func arrayArg(c *callCtx, i int) (*sqlType, error) {
	if i >= len(c.args) {
		return nil, noSignature(c)
	}
	a := c.args[i]
	if isNullLit(a) {
		c.args[i] = constExpr(nil, arrayOf(typInt64), litNull)
		return c.args[i].typ, nil
	}
	if a.typ.kind != tkArray {
		return nil, noSignature(c)
	}
	return a.typ, nil
}

func mapArg(c *callCtx, i int) (*sqlType, error) {
	if i >= len(c.args) {
		return nil, noSignature(c)
	}
	a := c.args[i]
	if a.typ.kind != tkMap {
		return nil, noSignature(c)
	}
	return a.typ, nil
}

func exactArgs(c *callCtx, lo, hi int) error {
	if len(c.args) < lo || len(c.args) > hi {
		return noSignature(c)
	}
	return nil
}

func bindArrayConcat(name string, args []*bexpr) (*bexpr, error) {
	for i, a := range args {
		if isNullLit(a) {
			continue
		}
		if a.typ.kind != tkArray {
			return nil, invalidf("No matching signature for function %s: argument %d is %s, expected ARRAY", name, i+1, a.typ)
		}
	}
	t, err := commonType(args)
	if err != nil {
		return nil, err
	}
	if t.kind != tkArray {
		t = arrayOf(typInt64)
	}
	co, err := coerceAll(args, t)
	if err != nil {
		return nil, err
	}
	return callExpr(name, t, func(a []any) (any, error) {
		out := arrayV{}
		for _, v := range a {
			out = append(out, v.(arrayV)...)
		}
		return out, nil
	}, co...), nil
}

func init() {
	I := typInt64

	registerFunc("ARRAY_LENGTH", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 1, 1); err != nil {
			return nil, err
		}
		if _, err := arrayArg(c, 0); err != nil {
			return nil, err
		}
		return callExpr(c.name, I, func(a []any) (any, error) { return int64(len(a[0].(arrayV))), nil }, c.args...), nil
	}})
	registerFunc("ARRAY_CONCAT", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if len(c.args) == 0 {
			return nil, noSignature(c)
		}
		return bindArrayConcat(c.name, c.args)
	}})
	registerFunc("ARRAY_REVERSE", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 1, 1); err != nil {
			return nil, err
		}
		t, err := arrayArg(c, 0)
		if err != nil {
			return nil, err
		}
		return callExpr(c.name, t, func(a []any) (any, error) {
			in := a[0].(arrayV)
			out := make(arrayV, len(in))
			for i, v := range in {
				out[len(in)-1-i] = v
			}
			return out, nil
		}, c.args...), nil
	}})
	registerFunc("ARRAY_TO_STRING", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 2, 3); err != nil {
			return nil, err
		}
		t, err := arrayArg(c, 0)
		if err != nil {
			return nil, err
		}
		et := t.elem
		if et.kind != tkString && et.kind != tkBytes {
			return nil, noSignature(c)
		}
		args := []*bexpr{c.args[0]}
		for _, x := range c.args[1:] {
			cx, err := coerce(x, et)
			if err != nil {
				return nil, noSignature(c)
			}
			args = append(args, cx)
		}
		isStr := et.kind == tkString
		r := callExpr(c.name, et, func(a []any) (any, error) {
			if a[0] == nil || a[1] == nil || (len(a) > 2 && a[2] == nil) {
				return nil, nil
			}
			var parts [][]byte
			for _, v := range a[0].(arrayV) {
				if v == nil {
					if len(a) > 2 {
						parts = append(parts, asBytes(a[2]))
					}
					continue
				}
				parts = append(parts, asBytes(v))
			}
			return asStringOrBytes(bytes.Join(parts, asBytes(a[1])), isStr), nil
		}, args...)
		return r, nil
	}})
	firstLast := func(last bool) *funcDef {
		return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
			if err := exactArgs(c, 1, 1); err != nil {
				return nil, err
			}
			t, err := arrayArg(c, 0)
			if err != nil {
				return nil, err
			}
			return callExpr(c.name, t.elem, func(a []any) (any, error) {
				arr := a[0].(arrayV)
				if len(arr) == 0 {
					return nil, evalErrorf("%s cannot get an element from an empty array", c.name)
				}
				if last {
					return arr[len(arr)-1], nil
				}
				return arr[0], nil
			}, c.args...), nil
		}}
	}
	registerFunc("ARRAY_FIRST", firstLast(false))
	registerFunc("ARRAY_LAST", firstLast(true))
	firstLastN := func(last bool) *funcDef {
		return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
			if err := exactArgs(c, 2, 2); err != nil {
				return nil, err
			}
			t, err := arrayArg(c, 0)
			if err != nil {
				return nil, err
			}
			n, err := coerce(c.args[1], I)
			if err != nil {
				return nil, noSignature(c)
			}
			return callExpr(c.name, t, func(a []any) (any, error) {
				arr, k := a[0].(arrayV), a[1].(int64)
				if k < 0 {
					return nil, evalErrorf("%s requires a non-negative length", c.name)
				}
				if k > int64(len(arr)) {
					k = int64(len(arr))
				}
				if last {
					return append(arrayV{}, arr[int64(len(arr))-k:]...), nil
				}
				return append(arrayV{}, arr[:k]...), nil
			}, c.args[0], n), nil
		}}
	}
	registerFunc("ARRAY_FIRST_N", firstLastN(false))
	registerFunc("ARRAY_LAST_N", firstLastN(true))
	registerFunc("ARRAY_SLICE", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 3, 3); err != nil {
			return nil, err
		}
		t, err := arrayArg(c, 0)
		if err != nil {
			return nil, err
		}
		s, err1 := coerce(c.args[1], I)
		e, err2 := coerce(c.args[2], I)
		if err1 != nil || err2 != nil {
			return nil, noSignature(c)
		}
		return callExpr(c.name, t, func(a []any) (any, error) {
			arr := a[0].(arrayV)
			n := int64(len(arr))
			norm := func(i int64) int64 {
				if i < 0 {
					i += n
				}
				return i
			}
			lo, hi := norm(a[1].(int64)), norm(a[2].(int64))
			if lo < 0 {
				lo = 0
			}
			if hi >= n {
				hi = n - 1
			}
			if lo > hi || lo >= n {
				return arrayV{}, nil
			}
			return append(arrayV{}, arr[lo:hi+1]...), nil
		}, c.args[0], s, e), nil
	}})
	registerFunc("ARRAY_INCLUDES", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 2, 2); err != nil {
			return nil, err
		}
		t, err := arrayArg(c, 0)
		if err != nil {
			return nil, err
		}
		v, err := coerce(c.args[1], t.elem)
		if err != nil {
			return nil, noSignature(c)
		}
		if !t.elem.equalityComparable() {
			return nil, invalidf("ARRAY_INCLUDES is not defined for elements of type %s", t.elem)
		}
		return callExpr(c.name, typBool, func(a []any) (any, error) {
			for _, e := range a[0].(arrayV) {
				if sqlEquals(e, a[1]) == true {
					return true, nil
				}
			}
			return false, nil
		}, c.args[0], v), nil
	}})
	includesSet := func(all bool) *funcDef {
		return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
			if err := exactArgs(c, 2, 2); err != nil {
				return nil, err
			}
			if _, err := arrayArg(c, 0); err != nil {
				return nil, err
			}
			if _, err := arrayArg(c, 1); err != nil {
				return nil, err
			}
			t, err := commonType(c.args)
			if err != nil {
				return nil, noSignature(c)
			}
			args, err := coerceAll(c.args, t)
			if err != nil {
				return nil, err
			}
			return callExpr(c.name, typBool, func(a []any) (any, error) {
				arr, search := a[0].(arrayV), a[1].(arrayV)
				for _, s := range search {
					if s == nil {
						return nil, nil
					}
					found := false
					for _, e := range arr {
						if sqlEquals(e, s) == true {
							found = true
							break
						}
					}
					if all && !found {
						return false, nil
					}
					if !all && found {
						return true, nil
					}
				}
				return all, nil
			}, args...), nil
		}}
	}
	registerFunc("ARRAY_INCLUDES_ALL", includesSet(true))
	registerFunc("ARRAY_INCLUDES_ANY", includesSet(false))
	registerFunc("ARRAY_IS_DISTINCT", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 1, 1); err != nil {
			return nil, err
		}
		if _, err := arrayArg(c, 0); err != nil {
			return nil, err
		}
		return callExpr(c.name, typBool, func(a []any) (any, error) {
			seen := map[string]bool{}
			for _, e := range a[0].(arrayV) {
				k := valueKey(e)
				if seen[k] {
					return false, nil
				}
				seen[k] = true
			}
			return true, nil
		}, c.args...), nil
	}})
	offsets := func(all bool) *funcDef {
		return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
			if err := exactArgs(c, 2, 2); err != nil {
				return nil, err
			}
			t, err := arrayArg(c, 0)
			if err != nil {
				return nil, err
			}
			v, err := coerce(c.args[1], t.elem)
			if err != nil {
				return nil, noSignature(c)
			}
			rt := I
			if all {
				rt = arrayOf(I)
			}
			return callExpr(c.name, rt, func(a []any) (any, error) {
				out := arrayV{}
				for i, e := range a[0].(arrayV) {
					if sqlEquals(e, a[1]) == true {
						if !all {
							return int64(i), nil
						}
						out = append(out, int64(i))
					}
				}
				if !all {
					return nil, nil
				}
				return out, nil
			}, c.args[0], v), nil
		}}
	}
	registerFunc("ARRAY_OFFSET", offsets(false))
	registerFunc("ARRAY_OFFSETS", offsets(true))
	registerFunc("GENERATE_ARRAY", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 2, 3); err != nil {
			return nil, err
		}
		t, err := commonType(c.args)
		if err != nil || !t.isNumeric() {
			return nil, noSignature(c)
		}
		if t.kind == tkFloat32 {
			t = typFloat64
		}
		args, err := coerceAll(c.args, t)
		if err != nil {
			return nil, err
		}
		if t.kind == tkInt64 {
			return callExpr(c.name, arrayOf(I), func(a []any) (any, error) {
				s, e, step := a[0].(int64), a[1].(int64), int64(1)
				if len(a) > 2 {
					step = a[2].(int64)
				}
				if step == 0 {
					return nil, evalErrorf("Sequence step cannot be 0.")
				}
				out := arrayV{}
				for cur := s; (step > 0 && cur <= e) || (step < 0 && cur >= e); {
					out = append(out, cur)
					if len(out) > 1e6 {
						return nil, evalErrorf("GENERATE_ARRAY produced too many elements")
					}
					next := cur + step
					if (step > 0 && next < cur) || (step < 0 && next > cur) {
						break
					}
					cur = next
				}
				return out, nil
			}, args...), nil
		}
		return callExpr(c.name, arrayOf(typFloat64), func(a []any) (any, error) {
			s, e, step := a[0].(float64), a[1].(float64), 1.0
			if len(a) > 2 {
				step = a[2].(float64)
			}
			if step == 0 || math.IsNaN(step) || math.IsNaN(s) || math.IsNaN(e) || math.IsInf(step, 0) {
				return nil, evalErrorf("Sequence step cannot be 0, NaN or infinite")
			}
			out := arrayV{}
			for i := 0; ; i++ {
				cur := s + float64(i)*step
				if (step > 0 && cur > e) || (step < 0 && cur < e) {
					break
				}
				out = append(out, cur)
				if len(out) > 1e6 {
					return nil, evalErrorf("GENERATE_ARRAY produced too many elements")
				}
			}
			return out, nil
		}, args...), nil
	}})

	// Maps.
	registerFunc("MAP_KEYS", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 1, 1); err != nil {
			return nil, err
		}
		t, err := mapArg(c, 0)
		if err != nil {
			return nil, err
		}
		return callExpr(c.name, arrayOf(t.key), func(a []any) (any, error) {
			return append(arrayV{}, a[0].(*mapV).keys...), nil
		}, c.args...), nil
	}})
	registerFunc("MAP_VALUES", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 1, 1); err != nil {
			return nil, err
		}
		t, err := mapArg(c, 0)
		if err != nil {
			return nil, err
		}
		if t.val.kind == tkArray {
			return nil, invalidf("MAP_VALUES cannot return an array of arrays (value type %s)", t.val)
		}
		return callExpr(c.name, arrayOf(t.val), func(a []any) (any, error) {
			return append(arrayV{}, a[0].(*mapV).vals...), nil
		}, c.args...), nil
	}})
	registerFunc("MAP_ENTRIES", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 1, 1); err != nil {
			return nil, err
		}
		t, err := mapArg(c, 0)
		if err != nil {
			return nil, err
		}
		et := structOf(structField{"key", t.key}, structField{"value", t.val})
		return callExpr(c.name, arrayOf(et), func(a []any) (any, error) {
			m := a[0].(*mapV)
			out := make(arrayV, len(m.keys))
			for i := range m.keys {
				out[i] = structV{m.keys[i], m.vals[i]}
			}
			return out, nil
		}, c.args...), nil
	}})
	registerFunc("MAP_CONTAINS_KEY", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 2, 2); err != nil {
			return nil, err
		}
		t, err := mapArg(c, 0)
		if err != nil {
			return nil, err
		}
		k, err := coerce(c.args[1], t.key)
		if err != nil {
			return nil, noSignature(c)
		}
		return callExpr(c.name, typBool, func(a []any) (any, error) {
			_, ok := a[0].(*mapV).lookup(a[1])
			return ok, nil
		}, c.args[0], k), nil
	}})
	registerFunc("MAP_EMPTY", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 1, 1); err != nil {
			return nil, err
		}
		if _, err := mapArg(c, 0); err != nil {
			return nil, err
		}
		return callExpr(c.name, typBool, func(a []any) (any, error) { return len(a[0].(*mapV).keys) == 0, nil }, c.args...), nil
	}})

	// Vector distances.
	dist := func(name string, fn func(x, y []float64) (any, error)) *funcDef {
		return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
			if err := exactArgs(c, 2, 2); err != nil {
				return nil, err
			}
			args := make([]*bexpr, 2)
			for i := range c.args {
				x, err := coerce(c.args[i], arrayOf(typFloat64))
				if err != nil {
					if c.args[i].typ.kind == tkArray && c.args[i].typ.elem.kind == tkFloat32 {
						x = implicitCast(c.args[i], arrayOf(typFloat64))
					} else {
						return nil, noSignature(c)
					}
				}
				args[i] = x
			}
			return callExpr(name, typFloat64, func(a []any) (any, error) {
				xs, ys := a[0].(arrayV), a[1].(arrayV)
				if len(xs) != len(ys) {
					return nil, evalErrorf("%s requires vectors of equal length", name)
				}
				x, y := make([]float64, len(xs)), make([]float64, len(ys))
				for i := range xs {
					if xs[i] == nil || ys[i] == nil {
						return nil, evalErrorf("%s does not accept NULL vector elements", name)
					}
					x[i], y[i] = xs[i].(float64), ys[i].(float64)
				}
				return fn(x, y)
			}, args...), nil
		}}
	}
	registerFunc("EUCLIDEAN_DISTANCE", dist("EUCLIDEAN_DISTANCE", func(x, y []float64) (any, error) {
		s := 0.0
		for i := range x {
			d := x[i] - y[i]
			s += d * d
		}
		return math.Sqrt(s), nil
	}))
	registerFunc("COSINE_DISTANCE", dist("COSINE_DISTANCE", func(x, y []float64) (any, error) {
		var dot, nx, ny float64
		for i := range x {
			dot += x[i] * y[i]
			nx += x[i] * x[i]
			ny += y[i] * y[i]
		}
		if nx == 0 || ny == 0 {
			return nil, evalErrorf("COSINE_DISTANCE is undefined for zero-length vectors")
		}
		return 1 - dot/(math.Sqrt(nx)*math.Sqrt(ny)), nil
	}))
}

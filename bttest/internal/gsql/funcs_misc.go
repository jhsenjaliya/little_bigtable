package gsql

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

func init() {
	S := typString

	registerFunc("IF", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 3, 3); err != nil {
			return nil, err
		}
		cond, err := coerce(c.args[0], typBool)
		if err != nil {
			return nil, noSignature(c)
		}
		t, err := commonType(c.args[1:])
		if err != nil {
			return nil, noSignature(c)
		}
		res, err := coerceAll(c.args[1:], t)
		if err != nil {
			return nil, err
		}
		return lazyExpr("IF", t, func(ec *evalCtx, a []*bexpr) (any, error) {
			v, err := a[0].eval(ec)
			if err != nil {
				return nil, err
			}
			if v == true {
				return a[1].eval(ec)
			}
			return a[2].eval(ec)
		}, cond, res[0], res[1]), nil
	}})
	coalesce := func(name string, lo, hi int) *funcDef {
		return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
			if len(c.args) < lo || (hi > 0 && len(c.args) > hi) {
				return nil, noSignature(c)
			}
			t, err := commonType(c.args)
			if err != nil {
				return nil, noSignature(c)
			}
			args, err := coerceAll(c.args, t)
			if err != nil {
				return nil, err
			}
			return lazyExpr(name, t, func(ec *evalCtx, a []*bexpr) (any, error) {
				for _, x := range a {
					v, err := x.eval(ec)
					if err != nil {
						return nil, err
					}
					if v != nil {
						return v, nil
					}
				}
				return nil, nil
			}, args...), nil
		}}
	}
	registerFunc("COALESCE", coalesce("COALESCE", 1, 0))
	registerFunc("IFNULL", coalesce("IFNULL", 2, 2))
	registerFunc("NULLIF", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 2, 2); err != nil {
			return nil, err
		}
		t, err := commonType(c.args)
		if err != nil || !t.equalityComparable() {
			return nil, noSignature(c)
		}
		args, err := coerceAll(c.args, t)
		if err != nil {
			return nil, err
		}
		r := callExpr("NULLIF", t, func(a []any) (any, error) {
			if a[0] == nil {
				return nil, nil
			}
			if sqlEquals(a[0], a[1]) == true {
				return nil, nil
			}
			return a[0], nil
		}, args...)
		r.strict = false
		return r, nil
	}})
	registerFunc("IFERROR", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 2, 2); err != nil {
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
		return lazyExpr("IFERROR", t, func(ec *evalCtx, a []*bexpr) (any, error) {
			v, err := a[0].eval(ec)
			if err != nil {
				if !isEvalError(err) {
					return nil, err
				}
				return a[1].eval(ec)
			}
			return v, nil
		}, args...), nil
	}})
	registerFunc("ISERROR", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 1, 1); err != nil {
			return nil, err
		}
		return lazyExpr("ISERROR", typBool, func(ec *evalCtx, a []*bexpr) (any, error) {
			_, err := a[0].eval(ec)
			if err != nil && !isEvalError(err) {
				return nil, err
			}
			return err != nil, nil
		}, c.args...), nil
	}})
	registerFunc("NULLIFERROR", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 1, 1); err != nil {
			return nil, err
		}
		return lazyExpr("NULLIFERROR", c.args[0].typ, func(ec *evalCtx, a []*bexpr) (any, error) {
			v, err := a[0].eval(ec)
			if err != nil {
				if !isEvalError(err) {
					return nil, err
				}
				return nil, nil
			}
			return v, nil
		}, c.args...), nil
	}})
	distinctFrom := func(neg bool) *funcDef {
		return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
			t, err := commonType(c.args)
			if err != nil || !t.equalityComparable() {
				return nil, noSignature(c)
			}
			args, err := coerceAll(c.args, t)
			if err != nil {
				return nil, err
			}
			r := callExpr(c.name, typBool, func(a []any) (any, error) {
				var same bool
				switch {
				case a[0] == nil || a[1] == nil:
					same = a[0] == nil && a[1] == nil
				default:
					same = valueKey(a[0]) == valueKey(a[1]) || sqlEquals(a[0], a[1]) == true
				}
				return same == neg, nil
			}, args...)
			r.strict = false
			return r, nil
		}}
	}
	registerFunc("$is_distinct_from", distinctFrom(false))
	registerFunc("$is_not_distinct_from", distinctFrom(true))

	// JSON.
	jsonFn := func(name string, ret *sqlType, defaultPath bool, fn func(doc *jsonVal) (any, error)) *funcDef {
		return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
			lo := 2
			if defaultPath {
				lo = 1
			}
			if err := exactArgs(c, lo, 2); err != nil {
				return nil, err
			}
			doc := c.args[0]
			if !canCoerce(doc, S) {
				if doc.typ.kind != tkBytes {
					return nil, noSignature(c)
				}
			} else {
				doc, _ = coerce(doc, S)
			}
			args := []*bexpr{doc}
			if len(c.args) > 1 {
				p, err := coerce(c.args[1], S)
				if err != nil {
					return nil, noSignature(c)
				}
				args = append(args, p)
			}
			legacy := strings.HasPrefix(name, "JSON_EXTRACT")
			return callExpr(name, ret, func(a []any) (any, error) {
				path := "$"
				if len(a) > 1 {
					path = a[1].(string)
				}
				steps, err := parseJSONPath(path, legacy)
				if err != nil {
					return nil, err
				}
				root, ok := parseJSON(asBytes(a[0]))
				if !ok {
					return nil, nil
				}
				v := root.navigate(steps)
				if v == nil {
					return nil, nil
				}
				return fn(v)
			}, args...), nil
		}}
	}
	scalar := func(v *jsonVal) (any, error) {
		switch v.kind {
		case 's', 'n':
			return v.str, nil
		case 't':
			return "true", nil
		case 'f':
			return "false", nil
		}
		return nil, nil
	}
	query := func(v *jsonVal) (any, error) { return v.serialize(), nil }
	queryArray := func(v *jsonVal) (any, error) {
		if v.kind != 'a' {
			return nil, nil
		}
		out := arrayV{}
		for _, e := range v.vals {
			out = append(out, e.serialize())
		}
		return out, nil
	}
	valueArray := func(v *jsonVal) (any, error) {
		if v.kind != 'a' {
			return nil, nil
		}
		out := arrayV{}
		for _, e := range v.vals {
			s, _ := scalar(e)
			if e.kind == 'o' || e.kind == 'a' {
				return nil, nil
			}
			out = append(out, s)
		}
		return out, nil
	}
	registerFunc("JSON_VALUE", jsonFn("JSON_VALUE", S, true, scalar))
	registerFunc("JSON_EXTRACT_SCALAR", jsonFn("JSON_EXTRACT_SCALAR", S, true, scalar))
	registerFunc("JSON_QUERY", jsonFn("JSON_QUERY", S, false, query))
	registerFunc("JSON_EXTRACT", jsonFn("JSON_EXTRACT", S, false, query))
	registerFunc("JSON_QUERY_ARRAY", jsonFn("JSON_QUERY_ARRAY", arrayOf(S), true, queryArray))
	registerFunc("JSON_EXTRACT_ARRAY", jsonFn("JSON_EXTRACT_ARRAY", arrayOf(S), true, queryArray))
	registerFunc("JSON_VALUE_ARRAY", jsonFn("JSON_VALUE_ARRAY", arrayOf(S), true, valueArray))
	registerFunc("JSON_EXTRACT_STRING_ARRAY", jsonFn("JSON_EXTRACT_STRING_ARRAY", arrayOf(S), true, valueArray))
	registerFunc("TO_JSON_STRING", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 1, 2); err != nil {
			return nil, err
		}
		args := []*bexpr{c.args[0]}
		if len(c.args) > 1 {
			p, err := coerce(c.args[1], typBool)
			if err != nil {
				return nil, noSignature(c)
			}
			args = append(args, p)
		}
		t := c.args[0].typ
		r := callExpr(c.name, S, func(a []any) (any, error) {
			pretty := len(a) > 1 && a[1] == true
			s := toJSON(a[0], t)
			if pretty {
				var buf bytes.Buffer
				if err := json.Indent(&buf, []byte(s), "", "  "); err == nil {
					return buf.String(), nil
				}
			}
			return s, nil
		}, args...)
		r.strict = false
		return r, nil
	}})

	registerFunc("HLL_COUNT.EXTRACT", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := exactArgs(c, 1, 1); err != nil {
			return nil, err
		}
		x, err := coerce(c.args[0], typBytes)
		if err != nil {
			return nil, noSignature(c)
		}
		r := callExpr(c.name, typInt64, func(a []any) (any, error) {
			if a[0] == nil {
				return int64(0), nil
			}
			h, err := DecodeHLL(a[0].([]byte))
			if err != nil {
				return nil, evalErrorf("HLL_COUNT.EXTRACT: invalid sketch: %v", err)
			}
			return h.Estimate(), nil
		}, x)
		r.strict = false
		return r, nil
	}})
}

// ---------------------------------------------------------------------------
// Minimal order-preserving JSON model.

type jsonVal struct {
	kind byte // 'o' object, 'a' array, 's' string, 'n' number, 't' true, 'f' false, 'z' null
	str  string
	keys []string
	vals []*jsonVal
}

func parseJSON(b []byte) (*jsonVal, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := readJSON(dec)
	if err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

func readJSON(dec *json.Decoder) (*jsonVal, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v := &jsonVal{kind: 'o'}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, io.ErrUnexpectedEOF
				}
				e, err := readJSON(dec)
				if err != nil {
					return nil, err
				}
				v.keys = append(v.keys, k)
				v.vals = append(v.vals, e)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return v, nil
		case '[':
			v := &jsonVal{kind: 'a'}
			for dec.More() {
				e, err := readJSON(dec)
				if err != nil {
					return nil, err
				}
				v.vals = append(v.vals, e)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return v, nil
		}
	case string:
		return &jsonVal{kind: 's', str: t}, nil
	case json.Number:
		return &jsonVal{kind: 'n', str: t.String()}, nil
	case bool:
		if t {
			return &jsonVal{kind: 't'}, nil
		}
		return &jsonVal{kind: 'f'}, nil
	case nil:
		return &jsonVal{kind: 'z'}, nil
	}
	return nil, io.ErrUnexpectedEOF
}

type jsonStep struct {
	key   string
	index int
	isIdx bool
}

func parseJSONPath(p string, legacy bool) ([]jsonStep, error) {
	bad := func() ([]jsonStep, error) { return nil, evalErrorf("Invalid JSONPath: %s", p) }
	if !strings.HasPrefix(p, "$") {
		return bad()
	}
	var steps []jsonStep
	i := 1
	for i < len(p) {
		switch p[i] {
		case '.':
			i++
			if i < len(p) && p[i] == '"' {
				j := strings.IndexByte(p[i+1:], '"')
				if j < 0 {
					return bad()
				}
				steps = append(steps, jsonStep{key: p[i+1 : i+1+j]})
				i += j + 2
				continue
			}
			j := i
			for j < len(p) && p[j] != '.' && p[j] != '[' {
				j++
			}
			if j == i || p[i:j] == "*" {
				return bad()
			}
			steps = append(steps, jsonStep{key: p[i:j]})
			i = j
		case '[':
			j := strings.IndexByte(p[i:], ']')
			if j < 0 {
				return bad()
			}
			inner := strings.TrimSpace(p[i+1 : i+j])
			i += j + 1
			if len(inner) >= 2 && (inner[0] == '\'' || inner[0] == '"') && inner[len(inner)-1] == inner[0] {
				steps = append(steps, jsonStep{key: inner[1 : len(inner)-1]})
				continue
			}
			n, err := strconv.Atoi(inner)
			if err != nil || n < 0 {
				return bad()
			}
			steps = append(steps, jsonStep{index: n, isIdx: true})
		default:
			return bad()
		}
	}
	_ = legacy
	return steps, nil
}

func (v *jsonVal) navigate(steps []jsonStep) *jsonVal {
	cur := v
	for _, s := range steps {
		switch {
		case s.isIdx:
			if cur.kind != 'a' || s.index >= len(cur.vals) {
				return nil
			}
			cur = cur.vals[s.index]
		default:
			if cur.kind != 'o' {
				return nil
			}
			var next *jsonVal
			for i, k := range cur.keys {
				if k == s.key {
					next = cur.vals[i]
					break
				}
			}
			if next == nil {
				return nil
			}
			cur = next
		}
	}
	return cur
}

func (v *jsonVal) serialize() string {
	var sb strings.Builder
	v.write(&sb)
	return sb.String()
}

func (v *jsonVal) write(sb *strings.Builder) {
	switch v.kind {
	case 'o':
		sb.WriteByte('{')
		for i, k := range v.keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(jsonQuote(k))
			sb.WriteByte(':')
			v.vals[i].write(sb)
		}
		sb.WriteByte('}')
	case 'a':
		sb.WriteByte('[')
		for i, e := range v.vals {
			if i > 0 {
				sb.WriteByte(',')
			}
			e.write(sb)
		}
		sb.WriteByte(']')
	case 's':
		sb.WriteString(jsonQuote(v.str))
	case 'n':
		sb.WriteString(v.str)
	case 't':
		sb.WriteString("true")
	case 'f':
		sb.WriteString("false")
	default:
		sb.WriteString("null")
	}
}

func jsonQuote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		default:
			if r < 0x20 || r == utf8.RuneError {
				sb.WriteString(`\u` + leftPad(strconv.FormatInt(int64(r), 16), 4))
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// toJSON renders a SQL value as JSON (TO_JSON_STRING).
func toJSON(v any, t *sqlType) string {
	if v == nil {
		return "null"
	}
	switch t.kind {
	case tkBool:
		return strconv.FormatBool(v.(bool))
	case tkInt64:
		n := v.(int64)
		if n > 1<<53 || n < -(1<<53) {
			return `"` + strconv.FormatInt(n, 10) + `"`
		}
		return strconv.FormatInt(n, 10)
	case tkFloat32, tkFloat64:
		f := v.(float64)
		switch {
		case math.IsNaN(f):
			return `"NaN"`
		case math.IsInf(f, 1):
			return `"Infinity"`
		case math.IsInf(f, -1):
			return `"-Infinity"`
		}
		return formatFloat(f, t.kind == tkFloat32)
	case tkString:
		return jsonQuote(v.(string))
	case tkBytes:
		return `"` + base64.StdEncoding.EncodeToString(v.([]byte)) + `"`
	case tkDate:
		return `"` + formatDate(v.(dateVal)) + `"`
	case tkTimestamp:
		ts := v.(tsVal)
		s := ts.time().Format("2006-01-02T15:04:05")
		if us := floorMod(int64(ts), 1e6); us != 0 {
			s += "." + strings.TrimRight(leftPad(strconv.FormatInt(us, 10), 6), "0")
		}
		return `"` + s + `Z"`
	case tkArray:
		parts := []string{}
		for _, e := range v.(arrayV) {
			parts = append(parts, toJSON(e, t.elem))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case tkStruct:
		parts := []string{}
		for i, e := range v.(structV) {
			parts = append(parts, jsonQuote(t.fields[i].name)+":"+toJSON(e, t.fields[i].typ))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case tkMap:
		m := v.(*mapV)
		parts := []string{}
		for i := range m.keys {
			var k string
			switch kv := m.keys[i].(type) {
			case []byte:
				k = base64.StdEncoding.EncodeToString(kv)
			case string:
				k = kv
			default:
				k = textValue(kv, t.key)
			}
			parts = append(parts, jsonQuote(k)+":"+toJSON(m.vals[i], t.val))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return "null"
}

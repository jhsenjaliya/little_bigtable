package gsql

import (
	"bytes"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// seq abstracts over STRING (code points) and BYTES (bytes).
type seq []rune

func toSeq(v any) seq {
	switch x := v.(type) {
	case string:
		return seq([]rune(x))
	case []byte:
		s := make(seq, len(x))
		for i, c := range x {
			s[i] = rune(c)
		}
		return s
	}
	return nil
}

func fromSeq(s seq, isStr bool) any {
	if isStr {
		return string(s)
	}
	b := make([]byte, len(s))
	for i, r := range s {
		b[i] = byte(r)
	}
	return b
}

func seqIndex(s, sub seq, from int) int {
	for i := from; i+len(sub) <= len(s); i++ {
		match := true
		for j := range sub {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// strBytes registers a function with STRING and BYTES overloads sharing one
// implementation; argument types listed as nil take the overload's type.
func strBytes(argTypes []*sqlType, minArgs int, retSame bool, ret *sqlType, fn func(isStr bool, a []any) (any, error)) *funcDef {
	mk := func(t *sqlType) sig {
		args := make([]*sqlType, len(argTypes))
		for i, at := range argTypes {
			if at == nil {
				args[i] = t
			} else {
				args[i] = at
			}
		}
		r := ret
		if retSame {
			r = t
		}
		isStr := t.kind == tkString
		return sig{args: args, minArgs: minArgs, ret: r, fn: func(a []any) (any, error) { return fn(isStr, a) }}
	}
	return bySigs(mk(typString), mk(typBytes))
}

var regexCache sync.Map

func compileRegex(pat string) (*regexp.Regexp, error) {
	if r, ok := regexCache.Load(pat); ok {
		return r.(*regexp.Regexp), nil
	}
	r, err := regexp.Compile(pat)
	if err != nil {
		return nil, evalErrorf("Cannot parse regular expression: %v", err)
	}
	regexCache.Store(pat, r)
	return r, nil
}

func asBytes(v any) []byte {
	switch x := v.(type) {
	case string:
		return []byte(x)
	case []byte:
		return x
	}
	return nil
}

func asStringOrBytes(b []byte, isStr bool) any {
	if isStr {
		return string(b)
	}
	if b == nil {
		return []byte{}
	}
	return b
}

func init() {
	S, B, I := typString, typBytes, typInt64

	concat := &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if len(c.args) == 0 {
			return nil, noSignature(c)
		}
		for _, t := range []*sqlType{typString, typBytes} {
			s := sig{args: []*sqlType{t}, variadic: true, ret: t, fn: concatFn(t)}
			if s.matches(c.args) {
				return s.build(c.name, c.args)
			}
		}
		return nil, noSignature(c)
	}}
	registerFunc("CONCAT", concat)

	registerFunc("LENGTH", strBytes([]*sqlType{nil}, 0, false, I, func(isStr bool, a []any) (any, error) {
		if isStr {
			return int64(utf8.RuneCountInString(a[0].(string))), nil
		}
		return int64(len(a[0].([]byte))), nil
	}))
	charLen := bySigs(s1(S, I, func(v any) (any, error) { return int64(utf8.RuneCountInString(v.(string))), nil }))
	registerFunc("CHAR_LENGTH", charLen)
	registerFunc("CHARACTER_LENGTH", charLen)
	byteLen := strBytes([]*sqlType{nil}, 0, false, I, func(isStr bool, a []any) (any, error) {
		return int64(len(asBytes(a[0]))), nil
	})
	registerFunc("BYTE_LENGTH", byteLen)
	registerFunc("OCTET_LENGTH", byteLen)

	registerFunc("LOWER", strBytes([]*sqlType{nil}, 0, true, nil, func(isStr bool, a []any) (any, error) {
		if isStr {
			return strings.ToLower(a[0].(string)), nil
		}
		return asciiMap(a[0].([]byte), unicode.ToLower), nil
	}))
	registerFunc("UPPER", strBytes([]*sqlType{nil}, 0, true, nil, func(isStr bool, a []any) (any, error) {
		if isStr {
			return strings.ToUpper(a[0].(string)), nil
		}
		return asciiMap(a[0].([]byte), unicode.ToUpper), nil
	}))

	substr := strBytes([]*sqlType{nil, I, I}, 2, true, nil, func(isStr bool, a []any) (any, error) {
		s := toSeq(a[0])
		n := int64(len(s))
		pos := a[1].(int64)
		var start int64
		switch {
		case pos > 0:
			start = pos - 1
		case pos == 0:
			start = 0
		default:
			start = n + pos
			if start < 0 {
				start = 0
			}
		}
		if start > n {
			start = n
		}
		end := n
		if len(a) > 2 {
			l := a[2].(int64)
			if l < 0 {
				return nil, evalErrorf("Third argument in SUBSTR() cannot be negative")
			}
			if l < n-start {
				end = start + l
			}
		}
		return fromSeq(s[start:end], isStr), nil
	})
	registerFunc("SUBSTR", substr)
	registerFunc("SUBSTRING", substr)

	registerFunc("STARTS_WITH", strBytes([]*sqlType{nil, nil}, 0, false, typBool, func(isStr bool, a []any) (any, error) {
		return bytes.HasPrefix(asBytes(a[0]), asBytes(a[1])), nil
	}))
	registerFunc("ENDS_WITH", strBytes([]*sqlType{nil, nil}, 0, false, typBool, func(isStr bool, a []any) (any, error) {
		return bytes.HasSuffix(asBytes(a[0]), asBytes(a[1])), nil
	}))
	registerFunc("STRPOS", strBytes([]*sqlType{nil, nil}, 0, false, I, func(isStr bool, a []any) (any, error) {
		return int64(seqIndex(toSeq(a[0]), toSeq(a[1]), 0) + 1), nil
	}))
	registerFunc("INSTR", strBytes([]*sqlType{nil, nil, I, I}, 2, false, I, func(isStr bool, a []any) (any, error) {
		s, sub := toSeq(a[0]), toSeq(a[1])
		pos, occ := int64(1), int64(1)
		if len(a) > 2 {
			pos = a[2].(int64)
		}
		if len(a) > 3 {
			occ = a[3].(int64)
		}
		if pos == 0 {
			return nil, evalErrorf("INSTR position must not be 0")
		}
		if occ <= 0 {
			return nil, evalErrorf("INSTR occurrence must be positive")
		}
		n := int64(len(s))
		if pos > 0 {
			i := pos - 1
			for ; i <= n-int64(len(sub)); i++ {
				j := seqIndex(s, sub, int(i))
				if j < 0 {
					return int64(0), nil
				}
				occ--
				if occ == 0 {
					return int64(j + 1), nil
				}
				i = int64(j)
			}
			return int64(0), nil
		}
		start := n + pos
		if start > n-int64(len(sub)) {
			start = n - int64(len(sub))
		}
		for i := start; i >= 0; i-- {
			if seqIndex(s[:min(int64(len(s)), i+int64(len(sub)))], sub, int(i)) == int(i) {
				occ--
				if occ == 0 {
					return i + 1, nil
				}
			}
		}
		return int64(0), nil
	}))
	registerFunc("LEFT", strBytes([]*sqlType{nil, I}, 0, true, nil, func(isStr bool, a []any) (any, error) {
		s, n := toSeq(a[0]), a[1].(int64)
		if n < 0 {
			return nil, evalErrorf("Second argument in LEFT() cannot be negative")
		}
		if n > int64(len(s)) {
			n = int64(len(s))
		}
		return fromSeq(s[:n], isStr), nil
	}))
	registerFunc("RIGHT", strBytes([]*sqlType{nil, I}, 0, true, nil, func(isStr bool, a []any) (any, error) {
		s, n := toSeq(a[0]), a[1].(int64)
		if n < 0 {
			return nil, evalErrorf("Second argument in RIGHT() cannot be negative")
		}
		if n > int64(len(s)) {
			n = int64(len(s))
		}
		return fromSeq(s[int64(len(s))-n:], isStr), nil
	}))
	registerFunc("REPLACE", strBytes([]*sqlType{nil, nil, nil}, 0, true, nil, func(isStr bool, a []any) (any, error) {
		from := asBytes(a[1])
		if len(from) == 0 {
			return a[0], nil
		}
		return asStringOrBytes(bytes.ReplaceAll(asBytes(a[0]), from, asBytes(a[2])), isStr), nil
	}))
	registerFunc("REPEAT", strBytes([]*sqlType{nil, I}, 0, true, nil, func(isStr bool, a []any) (any, error) {
		n := a[1].(int64)
		if n < 0 {
			return nil, evalErrorf("Second argument in REPEAT() cannot be negative")
		}
		b := asBytes(a[0])
		if int64(len(b))*n > 64<<20 {
			return nil, evalErrorf("REPEAT result is too large")
		}
		return asStringOrBytes(bytes.Repeat(b, int(n)), isStr), nil
	}))
	registerFunc("REVERSE", strBytes([]*sqlType{nil}, 0, true, nil, func(isStr bool, a []any) (any, error) {
		s := toSeq(a[0])
		for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
			s[i], s[j] = s[j], s[i]
		}
		return fromSeq(s, isStr), nil
	}))
	trim := func(left, right bool) *funcDef {
		return &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
			ss := []sig{
				{args: []*sqlType{S, S}, minArgs: 1, ret: S},
				{args: []*sqlType{B, B}, minArgs: 2, ret: B},
			}
			for _, s := range ss {
				isStr := s.ret.kind == tkString
				s.fn = func(a []any) (any, error) {
					v := toSeq(a[0])
					in := func(r rune) bool { return unicode.IsSpace(r) }
					if len(a) > 1 {
						set := toSeq(a[1])
						in = func(r rune) bool {
							for _, x := range set {
								if x == r {
									return true
								}
							}
							return false
						}
					}
					i, j := 0, len(v)
					if left {
						for i < j && in(v[i]) {
							i++
						}
					}
					if right {
						for j > i && in(v[j-1]) {
							j--
						}
					}
					return fromSeq(v[i:j], isStr), nil
				}
				if s.matches(c.args) {
					return s.build(c.name, c.args)
				}
			}
			return nil, noSignature(c)
		}}
	}
	registerFunc("TRIM", trim(true, true))
	registerFunc("LTRIM", trim(true, false))
	registerFunc("RTRIM", trim(false, true))

	pad := func(left bool) *funcDef {
		return strBytes([]*sqlType{nil, I, nil}, 2, true, nil, func(isStr bool, a []any) (any, error) {
			v, n := toSeq(a[0]), a[1].(int64)
			p := seq{' '}
			if len(a) > 2 {
				p = toSeq(a[2])
			}
			if n < 0 {
				return nil, evalErrorf("return_length must not be negative")
			}
			if n > 64<<20 {
				return nil, evalErrorf("return_length is too large")
			}
			if len(p) == 0 {
				return nil, evalErrorf("pattern must not be empty")
			}
			if n <= int64(len(v)) {
				return fromSeq(v[:n], isStr), nil
			}
			fill := make(seq, 0, n-int64(len(v)))
			for int64(len(fill)) < n-int64(len(v)) {
				fill = append(fill, p[len(fill)%len(p)])
			}
			if left {
				return fromSeq(append(fill, v...), isStr), nil
			}
			return fromSeq(append(v, fill...), isStr), nil
		})
	}
	registerFunc("LPAD", pad(true))
	registerFunc("RPAD", pad(false))

	registerFunc("SPLIT", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		ss := []sig{
			{args: []*sqlType{S, S}, minArgs: 1, ret: arrayOf(S)},
			{args: []*sqlType{B, B}, minArgs: 2, ret: arrayOf(B)},
		}
		for _, s := range ss {
			isStr := s.ret.elem.kind == tkString
			s.fn = func(a []any) (any, error) {
				var delim []byte
				if len(a) > 1 {
					delim = asBytes(a[1])
				} else {
					delim = []byte(",")
				}
				val := asBytes(a[0])
				var parts [][]byte
				if len(delim) == 0 {
					if isStr {
						for _, r := range string(val) {
							parts = append(parts, []byte(string(r)))
						}
					} else {
						for _, x := range val {
							parts = append(parts, []byte{x})
						}
					}
				} else {
					parts = bytes.Split(val, delim)
				}
				out := make(arrayV, len(parts))
				for i, p := range parts {
					out[i] = asStringOrBytes(append([]byte(nil), p...), isStr)
				}
				return out, nil
			}
			if s.matches(c.args) {
				return s.build(c.name, c.args)
			}
		}
		return nil, noSignature(c)
	}})

	registerFunc("ASCII", strBytes([]*sqlType{nil}, 0, false, I, func(isStr bool, a []any) (any, error) {
		if isStr {
			s := a[0].(string)
			if s == "" {
				return int64(0), nil
			}
			r, _ := utf8.DecodeRuneInString(s)
			if r > 127 {
				return nil, evalErrorf("ASCII() requires the first character to be ASCII")
			}
			return int64(r), nil
		}
		b := a[0].([]byte)
		if len(b) == 0 {
			return int64(0), nil
		}
		return int64(b[0]), nil
	}))
	registerFunc("UNICODE", bySigs(s1(S, I, func(v any) (any, error) {
		s := v.(string)
		if s == "" {
			return int64(0), nil
		}
		r, _ := utf8.DecodeRuneInString(s)
		return int64(r), nil
	})))
	registerFunc("CHR", bySigs(s1(I, S, func(v any) (any, error) {
		n := v.(int64)
		if n == 0 {
			return "", nil
		}
		if n < 0 || n > unicode.MaxRune || (n >= 0xD800 && n <= 0xDFFF) {
			return nil, evalErrorf("Invalid code point %d", n)
		}
		return string(rune(n)), nil
	})))
	registerFunc("INITCAP", bySigs(sig{args: []*sqlType{S, S}, minArgs: 1, ret: S, fn: func(a []any) (any, error) {
		delims := " \t\n\r\f\v[](){}/|\\<>!?@\"^#$&~_,.:;*%+-"
		if len(a) > 1 {
			delims = a[1].(string)
		}
		var sb strings.Builder
		newWord := true
		for _, r := range a[0].(string) {
			if strings.ContainsRune(delims, r) {
				newWord = true
				sb.WriteRune(r)
				continue
			}
			if newWord {
				sb.WriteRune(unicode.ToUpper(r))
			} else {
				sb.WriteRune(unicode.ToLower(r))
			}
			newWord = false
		}
		return sb.String(), nil
	}}))
	registerFunc("TRANSLATE", strBytes([]*sqlType{nil, nil, nil}, 0, true, nil, func(isStr bool, a []any) (any, error) {
		src, from, to := toSeq(a[0]), toSeq(a[1]), toSeq(a[2])
		m := map[rune]int{}
		for i, r := range from {
			if _, dup := m[r]; dup {
				return nil, evalErrorf("Duplicate character in TRANSLATE source characters")
			}
			m[r] = i
		}
		out := make(seq, 0, len(src))
		for _, r := range src {
			if i, ok := m[r]; ok {
				if i < len(to) {
					out = append(out, to[i])
				}
				continue
			}
			out = append(out, r)
		}
		return fromSeq(out, isStr), nil
	}))
	registerFunc("SOUNDEX", bySigs(s1(S, S, func(v any) (any, error) { return soundex(v.(string)), nil })))

	// Regular expressions.
	registerFunc("REGEXP_CONTAINS", strBytes([]*sqlType{nil, nil}, 0, false, typBool, func(isStr bool, a []any) (any, error) {
		re, err := compileRegex(string(asBytes(a[1])))
		if err != nil {
			return nil, err
		}
		return re.Match(asBytes(a[0])), nil
	}))
	extract := strBytes([]*sqlType{nil, nil, I, I}, 2, true, nil, func(isStr bool, a []any) (any, error) {
		re, err := compileRegex(string(asBytes(a[1])))
		if err != nil {
			return nil, err
		}
		if re.NumSubexp() > 1 {
			return nil, evalErrorf("Regular expressions passed into extraction functions must not have more than 1 capturing group")
		}
		val := asBytes(a[0])
		pos, occ := int64(1), int64(1)
		if len(a) > 2 {
			pos = a[2].(int64)
		}
		if len(a) > 3 {
			occ = a[3].(int64)
		}
		if pos <= 0 || occ <= 0 {
			return nil, evalErrorf("REGEXP_EXTRACT position and occurrence must be positive")
		}
		off := regexOffset(val, pos, isStr)
		if off < 0 {
			return nil, nil
		}
		ms := re.FindAllSubmatchIndex(val[off:], -1)
		if int64(len(ms)) < occ {
			return nil, nil
		}
		m := ms[occ-1]
		if re.NumSubexp() == 1 {
			if m[2] < 0 {
				return nil, nil
			}
			return asStringOrBytes(append([]byte(nil), val[off+m[2]:off+m[3]]...), isStr), nil
		}
		return asStringOrBytes(append([]byte(nil), val[off+m[0]:off+m[1]]...), isStr), nil
	})
	registerFunc("REGEXP_EXTRACT", extract)
	registerFunc("REGEXP_SUBSTR", extract)
	registerFunc("REGEXP_EXTRACT_ALL", &funcDef{bind: func(b *binder, c *callCtx) (*bexpr, error) {
		for _, t := range []*sqlType{S, B} {
			isStr := t.kind == tkString
			s := sig{args: []*sqlType{t, t}, ret: arrayOf(t), fn: func(a []any) (any, error) {
				re, err := compileRegex(string(asBytes(a[1])))
				if err != nil {
					return nil, err
				}
				if re.NumSubexp() > 1 {
					return nil, evalErrorf("Regular expressions passed into extraction functions must not have more than 1 capturing group")
				}
				val := asBytes(a[0])
				out := arrayV{}
				for _, m := range re.FindAllSubmatchIndex(val, -1) {
					lo, hi := m[0], m[1]
					if re.NumSubexp() == 1 {
						lo, hi = m[2], m[3]
					}
					if lo < 0 {
						out = append(out, nil)
						continue
					}
					out = append(out, asStringOrBytes(append([]byte(nil), val[lo:hi]...), isStr))
				}
				return out, nil
			}}
			if s.matches(c.args) {
				return s.build(c.name, c.args)
			}
		}
		return nil, noSignature(c)
	}})
	registerFunc("REGEXP_REPLACE", strBytes([]*sqlType{nil, nil, nil}, 0, true, nil, func(isStr bool, a []any) (any, error) {
		re, err := compileRegex(string(asBytes(a[1])))
		if err != nil {
			return nil, err
		}
		tmpl, err := regexReplacement(asBytes(a[2]), re.NumSubexp())
		if err != nil {
			return nil, err
		}
		return asStringOrBytes(re.ReplaceAll(asBytes(a[0]), tmpl), isStr), nil
	}))
	registerFunc("REGEXP_INSTR", strBytes([]*sqlType{nil, nil, I, I, I}, 2, false, I, func(isStr bool, a []any) (any, error) {
		re, err := compileRegex(string(asBytes(a[1])))
		if err != nil {
			return nil, err
		}
		if re.NumSubexp() > 1 {
			return nil, evalErrorf("REGEXP_INSTR regular expressions must not have more than 1 capturing group")
		}
		val := asBytes(a[0])
		pos, occ, occPos := int64(1), int64(1), int64(0)
		if len(a) > 2 {
			pos = a[2].(int64)
		}
		if len(a) > 3 {
			occ = a[3].(int64)
		}
		if len(a) > 4 {
			occPos = a[4].(int64)
		}
		if pos <= 0 || occ <= 0 || (occPos != 0 && occPos != 1) {
			return nil, evalErrorf("invalid REGEXP_INSTR arguments")
		}
		off := regexOffset(val, pos, isStr)
		if off < 0 {
			return int64(0), nil
		}
		ms := re.FindAllSubmatchIndex(val[off:], -1)
		if int64(len(ms)) < occ {
			return int64(0), nil
		}
		m := ms[occ-1]
		lo, hi := m[0], m[1]
		if re.NumSubexp() == 1 {
			lo, hi = m[2], m[3]
		}
		at := off + lo
		if occPos == 1 {
			at = off + hi
		}
		if isStr {
			return int64(utf8.RuneCount(val[:at]) + 1), nil
		}
		return int64(at + 1), nil
	}))

	// Encodings.
	registerFunc("TO_HEX", bySigs(s1(B, S, func(v any) (any, error) { return hex.EncodeToString(v.([]byte)), nil })))
	registerFunc("FROM_HEX", bySigs(s1(S, B, func(v any) (any, error) {
		s := v.(string)
		if len(s)%2 == 1 {
			s = "0" + s
		}
		b, err := hex.DecodeString(s)
		if err != nil {
			return nil, evalErrorf("Failed to decode invalid hexadecimal string: %q", v)
		}
		return b, nil
	})))
	registerFunc("TO_BASE64", bySigs(s1(B, S, func(v any) (any, error) { return base64.StdEncoding.EncodeToString(v.([]byte)), nil })))
	registerFunc("FROM_BASE64", bySigs(s1(S, B, func(v any) (any, error) {
		s := strings.TrimSpace(v.(string))
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
		}
		if err != nil {
			return nil, evalErrorf("Failed to decode invalid base64 string")
		}
		return b, nil
	})))
	registerFunc("TO_BASE32", bySigs(s1(B, S, func(v any) (any, error) { return base32.StdEncoding.EncodeToString(v.([]byte)), nil })))
	registerFunc("FROM_BASE32", bySigs(s1(S, B, func(v any) (any, error) {
		b, err := base32.StdEncoding.DecodeString(strings.TrimSpace(v.(string)))
		if err != nil {
			return nil, evalErrorf("Failed to decode invalid base32 string")
		}
		return b, nil
	})))
	registerFunc("SAFE_CONVERT_BYTES_TO_STRING", bySigs(s1(B, S, func(v any) (any, error) {
		b := v.([]byte)
		if utf8.Valid(b) {
			return string(b), nil
		}
		var sb strings.Builder
		for len(b) > 0 {
			r, n := utf8.DecodeRune(b)
			sb.WriteRune(r)
			b = b[n:]
		}
		return sb.String(), nil
	})))
	registerFunc("TO_CODE_POINTS", strBytes([]*sqlType{nil}, 0, false, arrayOf(I), func(isStr bool, a []any) (any, error) {
		s := toSeq(a[0])
		out := make(arrayV, len(s))
		for i, r := range s {
			out[i] = int64(r)
		}
		return out, nil
	}))
	registerFunc("CODE_POINTS_TO_STRING", bySigs(s1(arrayOf(I), S, func(v any) (any, error) {
		var sb strings.Builder
		for _, e := range v.(arrayV) {
			if e == nil {
				return nil, nil
			}
			n := e.(int64)
			if n < 0 || n > unicode.MaxRune || (n >= 0xD800 && n <= 0xDFFF) {
				return nil, evalErrorf("Invalid code point %d", n)
			}
			sb.WriteRune(rune(n))
		}
		return sb.String(), nil
	})))
	registerFunc("CODE_POINTS_TO_BYTES", bySigs(s1(arrayOf(I), B, func(v any) (any, error) {
		out := []byte{}
		for _, e := range v.(arrayV) {
			if e == nil {
				return nil, nil
			}
			n := e.(int64)
			if n < 0 || n > 255 {
				return nil, evalErrorf("Invalid ASCII code point %d", n)
			}
			out = append(out, byte(n))
		}
		return out, nil
	})))

	// Bigtable byte decoders.
	registerFunc("TO_INT64", bySigs(s1(B, I, func(v any) (any, error) {
		b := v.([]byte)
		if len(b) != 8 {
			return nil, evalErrorf("TO_INT64 requires exactly 8 bytes, got %d", len(b))
		}
		return int64(binary.BigEndian.Uint64(b)), nil
	})))
	registerFunc("TO_FLOAT64", bySigs(s1(B, typFloat64, func(v any) (any, error) {
		b := v.([]byte)
		if len(b) != 8 {
			return nil, evalErrorf("TO_FLOAT64 requires exactly 8 bytes, got %d", len(b))
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	})))
	registerFunc("TO_FLOAT32", bySigs(s1(B, typFloat32, func(v any) (any, error) {
		b := v.([]byte)
		if len(b) != 4 {
			return nil, evalErrorf("TO_FLOAT32 requires exactly 4 bytes, got %d", len(b))
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	})))
	registerFunc("TO_VECTOR32", bySigs(s1(B, arrayOf(typFloat32), func(v any) (any, error) {
		b := v.([]byte)
		if len(b)%4 != 0 {
			return nil, evalErrorf("TO_VECTOR32 requires a multiple of 4 bytes, got %d", len(b))
		}
		out := make(arrayV, len(b)/4)
		for i := range out {
			out[i] = float64(math.Float32frombits(binary.BigEndian.Uint32(b[i*4:])))
		}
		return out, nil
	})))
	registerFunc("TO_VECTOR64", bySigs(s1(B, arrayOf(typFloat64), func(v any) (any, error) {
		b := v.([]byte)
		if len(b)%8 != 0 {
			return nil, evalErrorf("TO_VECTOR64 requires a multiple of 8 bytes, got %d", len(b))
		}
		out := make(arrayV, len(b)/8)
		for i := range out {
			out[i] = math.Float64frombits(binary.BigEndian.Uint64(b[i*8:]))
		}
		return out, nil
	})))

	registerFunc("FORMAT", &funcDef{bind: bindFormat})
}

func asciiMap(b []byte, f func(rune) rune) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c < 0x80 {
			out[i] = byte(f(rune(c)))
		} else {
			out[i] = c
		}
	}
	return out
}

// regexOffset converts a 1-based character (STRING) or byte (BYTES) position
// into a byte offset; -1 if past the end.
func regexOffset(val []byte, pos int64, isStr bool) int {
	if !isStr {
		if pos-1 > int64(len(val)) {
			return -1
		}
		return int(pos - 1)
	}
	off := 0
	for i := int64(1); i < pos; i++ {
		if off >= len(val) {
			return -1
		}
		_, n := utf8.DecodeRune(val[off:])
		off += n
	}
	if off > len(val) {
		return -1
	}
	return off
}

// regexReplacement converts a GoogleSQL replacement string (\1 style) to a Go
// regexp template.
func regexReplacement(r []byte, groups int) ([]byte, error) {
	var out []byte
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == '$':
			out = append(out, '$', '$')
		case c == '\\':
			if i+1 >= len(r) {
				return nil, evalErrorf("REGEXP_REPLACE replacement ends with a backslash")
			}
			i++
			n := r[i]
			switch {
			case n >= '0' && n <= '9':
				if int(n-'0') > groups {
					return nil, evalErrorf("REGEXP_REPLACE replacement references a nonexistent group \\%c", n)
				}
				out = append(out, []byte("${"+string(n)+"}")...)
			case n == '\\':
				out = append(out, '\\')
			default:
				return nil, evalErrorf("Invalid REGEXP_REPLACE pattern escape \\%c", n)
			}
		default:
			out = append(out, c)
		}
	}
	return out, nil
}

func soundex(s string) string {
	code := func(r rune) byte {
		switch unicode.ToUpper(r) {
		case 'B', 'F', 'P', 'V':
			return '1'
		case 'C', 'G', 'J', 'K', 'Q', 'S', 'X', 'Z':
			return '2'
		case 'D', 'T':
			return '3'
		case 'L':
			return '4'
		case 'M', 'N':
			return '5'
		case 'R':
			return '6'
		case 'H', 'W':
			return 'h'
		}
		return 0
	}
	var out []byte
	var last byte
	for _, r := range s {
		if !(r < 128 && unicode.IsLetter(r)) {
			continue
		}
		c := code(r)
		if len(out) == 0 {
			out = append(out, byte(unicode.ToUpper(r)))
			last = c
			continue
		}
		if c == 'h' {
			continue
		}
		if c != 0 && c != last {
			out = append(out, c)
		}
		last = c
		if len(out) == 4 {
			break
		}
	}
	if len(out) == 0 {
		return ""
	}
	for len(out) < 4 {
		out = append(out, '0')
	}
	return string(out)
}

// bindFormat implements FORMAT(format_string, ...).
func bindFormat(b *binder, c *callCtx) (*bexpr, error) {
	if len(c.args) == 0 {
		return nil, noSignature(c)
	}
	f, err := coerce(c.args[0], typString)
	if err != nil {
		return nil, noSignature(c)
	}
	args := append([]*bexpr{f}, c.args[1:]...)
	types := make([]*sqlType, len(args))
	for i, a := range args {
		types[i] = a.typ
	}
	r := callExpr("FORMAT", typString, func(a []any) (any, error) {
		if a[0] == nil {
			return nil, nil
		}
		return formatSQL(a[0].(string), a[1:], types[1:])
	}, args...)
	r.strict = false
	return r, nil
}

func formatSQL(f string, args []any, types []*sqlType) (any, error) {
	var sb strings.Builder
	ai := 0
	for i := 0; i < len(f); i++ {
		c := f[i]
		if c != '%' {
			sb.WriteByte(c)
			continue
		}
		j := i + 1
		for j < len(f) && strings.IndexByte("-+ #0'", f[j]) >= 0 {
			j++
		}
		for j < len(f) && isDigit(f[j]) {
			j++
		}
		if j < len(f) && f[j] == '.' {
			j++
			for j < len(f) && isDigit(f[j]) {
				j++
			}
		}
		if j >= len(f) {
			return nil, evalErrorf("Invalid format string: %q", f)
		}
		verb := f[j]
		spec := strings.ReplaceAll(f[i:j], "'", "")
		i = j
		if verb == '%' {
			sb.WriteByte('%')
			continue
		}
		if ai >= len(args) {
			return nil, evalErrorf("Too few arguments to FORMAT for format string %q", f)
		}
		v, t := args[ai], types[ai]
		ai++
		if v == nil {
			if verb == 't' || verb == 'T' {
				sb.WriteString(fmtPad(spec, "NULL"))
				continue
			}
			return nil, nil
		}
		switch verb {
		case 'd', 'i':
			n, ok := v.(int64)
			if !ok {
				return nil, evalErrorf("FORMAT %%%c requires an INT64 argument, got %s", verb, t)
			}
			sb.WriteString(sprintf(spec+"d", n))
		case 'o', 'x', 'X':
			n, ok := v.(int64)
			if !ok {
				return nil, evalErrorf("FORMAT %%%c requires an INT64 argument, got %s", verb, t)
			}
			neg := n < 0
			u := uint64(n)
			if neg {
				u = uint64(-n)
			}
			s := sprintf(strings.ReplaceAll(spec, "+", "")+string(verb), u)
			if neg {
				s = "-" + strings.TrimLeft(s, " ")
			}
			sb.WriteString(s)
		case 'f', 'F', 'e', 'E', 'g', 'G':
			var x float64
			switch n := v.(type) {
			case float64:
				x = n
			case int64:
				x = float64(n)
			default:
				return nil, evalErrorf("FORMAT %%%c requires a numeric argument, got %s", verb, t)
			}
			if math.IsInf(x, 0) || math.IsNaN(x) {
				sb.WriteString(fmtPad(spec, formatFloat(x, false)))
				continue
			}
			sb.WriteString(sprintf(spec+string(verb), x))
		case 's':
			if t.kind != tkString {
				return nil, evalErrorf("FORMAT %%s requires a STRING argument, got %s", t)
			}
			sb.WriteString(sprintf(spec+"s", v.(string)))
		case 't':
			sb.WriteString(sprintf(spec+"s", textValue(v, t)))
		case 'T':
			s, err := sqlLiteral(v, t)
			if err != nil {
				return nil, err
			}
			sb.WriteString(sprintf(spec+"s", s))
		default:
			return nil, evalErrorf("Invalid format specifier %%%c", verb)
		}
	}
	if ai < len(args) {
		return nil, evalErrorf("Too many arguments to FORMAT for format string %q", f)
	}
	return sb.String(), nil
}

func fmtPad(spec, s string) string { return sprintf(strings.TrimRight(spec, ".0123456789")+"s", s) }

func sprintf(format string, v any) string { return fmt.Sprintf(format, v) }

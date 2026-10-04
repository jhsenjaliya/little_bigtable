package gsql

import (
	"encoding/base64"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // deterministic time zone database for AT TIME ZONE / tz arguments
	"unicode/utf8"
)

var utcLoc = time.UTC

// castAllowed reports whether CAST(from AS to) is a valid conversion.
func castAllowed(from, to *sqlType) bool {
	if typesEqual(from, to) {
		return true
	}
	f, t := from.kind, to.kind
	switch t {
	case tkBool:
		return f == tkInt64 || f == tkString
	case tkInt64:
		return f == tkBool || f == tkFloat32 || f == tkFloat64 || f == tkString
	case tkFloat32, tkFloat64:
		return f == tkInt64 || f == tkFloat32 || f == tkFloat64 || f == tkString
	case tkString:
		switch f {
		case tkBool, tkInt64, tkFloat32, tkFloat64, tkBytes, tkDate, tkTimestamp:
			return true
		}
	case tkBytes:
		return f == tkString
	case tkDate:
		return f == tkString || f == tkTimestamp
	case tkTimestamp:
		return f == tkString || f == tkDate
	case tkArray:
		return f == tkArray && castAllowed(from.elem, to.elem)
	case tkStruct:
		if f != tkStruct || len(from.fields) != len(to.fields) {
			return false
		}
		for i := range from.fields {
			if !castAllowed(from.fields[i].typ, to.fields[i].typ) {
				return false
			}
		}
		return true
	}
	return false
}

func (b *binder) bindCast(e *astCast) (*bexpr, error) {
	x, err := b.bindExpr(e.x)
	if err != nil {
		return nil, err
	}
	to, err := resolveType(e.typ)
	if err != nil {
		return nil, err
	}
	if to.kind == tkMap {
		return nil, invalidf("Casting to MAP is not supported")
	}
	if isNullLit(x) {
		return constExpr(nil, to, litNull), nil
	}
	if x.ctor == "array" && len(x.args) == 0 && to.kind == tkArray {
		return makeArrayCtor(to, nil), nil
	}
	if !castAllowed(x.typ, to) {
		return nil, invalidf("Invalid cast from %s to %s", x.typ, to)
	}
	from := x.typ
	safe := e.safe
	name := "CAST"
	if safe {
		name = "SAFE_CAST"
	}
	r := callExpr(name, to, func(a []any) (any, error) {
		v, err := castValue(a[0], from, to, utcLoc)
		if err != nil && safe {
			return nil, nil
		}
		return v, err
	}, x)
	r.extra = to.String()
	return r, nil
}

// castValue converts v from one type to another following GoogleSQL
// conversion rules. loc is used for DATE/TIMESTAMP/STRING conversions.
func castValue(v any, from, to *sqlType, loc *time.Location) (any, error) {
	if v == nil {
		return nil, nil
	}
	if typesEqual(from, to) {
		return v, nil
	}
	bad := func() (any, error) {
		return nil, evalErrorf("Bad %s value: %s", to, literalTextShort(v, from))
	}
	switch to.kind {
	case tkBool:
		switch x := v.(type) {
		case int64:
			return x != 0, nil
		case string:
			switch strings.ToLower(x) {
			case "true":
				return true, nil
			case "false":
				return false, nil
			}
			return bad()
		}
	case tkInt64:
		switch x := v.(type) {
		case bool:
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		case float64:
			return floatToInt64(x)
		case string:
			n, err := parseInt64String(x)
			if err != nil {
				return bad()
			}
			return n, nil
		}
	case tkFloat64, tkFloat32:
		var f float64
		switch x := v.(type) {
		case int64:
			f = float64(x)
		case float64:
			f = x
		case string:
			p, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			if err != nil {
				if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
					return bad()
				}
				return nil, evalErrorf("Value out of range for %s: %s", to, x)
			}
			f = p
		default:
			return bad()
		}
		if to.kind == tkFloat32 {
			f32 := float32(f)
			if math.IsInf(float64(f32), 0) && !math.IsInf(f, 0) {
				return nil, evalErrorf("Value out of range for FLOAT32: %v", f)
			}
			return float64(f32), nil
		}
		return f, nil
	case tkString:
		switch x := v.(type) {
		case bool:
			return strconv.FormatBool(x), nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		case float64:
			return formatFloat(x, from.kind == tkFloat32), nil
		case []byte:
			if !utf8.Valid(x) {
				return nil, evalErrorf("Invalid UTF-8 bytes in cast from BYTES to STRING")
			}
			return string(x), nil
		case dateVal:
			return formatDate(x), nil
		case tsVal:
			return formatTimestamp(x, loc), nil
		}
	case tkBytes:
		if s, ok := v.(string); ok {
			return []byte(s), nil
		}
	case tkDate:
		switch x := v.(type) {
		case string:
			d, err := parseDateString(x)
			if err != nil {
				return bad()
			}
			return d, nil
		case tsVal:
			return dateFromTime(x.time().In(loc))
		}
	case tkTimestamp:
		switch x := v.(type) {
		case string:
			ts, err := parseTimestampString(x, loc)
			if err != nil {
				return bad()
			}
			return ts, nil
		case dateVal:
			y, m, d := x.civil()
			return checkTimestamp(civilToTime(y, m, d, 0, 0, 0, 0, loc).UnixMicro())
		}
	case tkArray:
		a, ok := v.(arrayV)
		if !ok {
			break
		}
		out := make(arrayV, len(a))
		for i, e := range a {
			c, err := castValue(e, from.elem, to.elem, loc)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case tkStruct:
		s, ok := v.(structV)
		if !ok || len(s) != len(to.fields) {
			break
		}
		out := make(structV, len(s))
		for i, e := range s {
			c, err := castValue(e, from.fields[i].typ, to.fields[i].typ, loc)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	return nil, evalErrorf("Invalid cast from %s to %s", from, to)
}

func literalTextShort(v any, t *sqlType) string {
	s := literalText(v, t)
	if len(s) > 64 {
		s = s[:61] + "..."
	}
	return s
}

func floatToInt64(f float64) (any, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, evalErrorf("Illegal conversion of non-finite floating point number to an integer: %v", formatFloat(f, false))
	}
	r := math.Round(f) // half away from zero
	if r < -9223372036854775808.0 || r >= 9223372036854775808.0 {
		return nil, evalErrorf("int64 out of range: %v", formatFloat(f, false))
	}
	return int64(r), nil
}

func parseInt64String(s string) (int64, error) {
	s = strings.TrimSpace(s)
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg = true
		s = s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	var u uint64
	var err error
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		u, err = strconv.ParseUint(s[2:], 16, 64)
	} else {
		if s == "" || !isAllDigits(s) {
			return 0, invalidf("bad int64")
		}
		u, err = strconv.ParseUint(s, 10, 64)
	}
	if err != nil {
		return 0, err
	}
	if neg {
		if u > 1<<63 {
			return 0, invalidf("int64 out of range")
		}
		return int64(-u), nil
	}
	if u > math.MaxInt64 {
		return 0, invalidf("int64 out of range")
	}
	return int64(u), nil
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

// ---------------------------------------------------------------------------
// Date and timestamp parsing.

// scanInt reads 1..maxDigits digits from s starting at i.
func scanInt(s string, i, minDigits, maxDigits int) (int, int, bool) {
	j := i
	for j < len(s) && j-i < maxDigits && isDigit(s[j]) {
		j++
	}
	if j-i < minDigits {
		return 0, i, false
	}
	n, _ := strconv.Atoi(s[i:j])
	return n, j, true
}

// parseCivilDate parses YYYY-[M]M-[D]D at the start of s.
func parseCivilDate(s string) (y, m, d, next int, ok bool) {
	i := 0
	if y, i, ok = scanInt(s, i, 1, 4); !ok || i >= len(s) || s[i] != '-' {
		return 0, 0, 0, 0, false
	}
	if m, i, ok = scanInt(s, i+1, 1, 2); !ok || i >= len(s) || s[i] != '-' {
		return 0, 0, 0, 0, false
	}
	if d, i, ok = scanInt(s, i+1, 1, 2); !ok {
		return 0, 0, 0, 0, false
	}
	if m < 1 || m > 12 || d < 1 || d > daysIn(time.Month(m), y) || y < 1 {
		return 0, 0, 0, 0, false
	}
	return y, m, d, i, true
}

func daysIn(m time.Month, y int) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func parseDateString(s string) (dateVal, error) {
	s = strings.TrimSpace(s)
	y, m, d, n, ok := parseCivilDate(s)
	if !ok || n != len(s) {
		return 0, invalidf("invalid date %q", s)
	}
	return dateFromYMD(y, time.Month(m), d)
}

// parseTimestampString parses the canonical timestamp format with optional
// time and time zone; loc is the default time zone.
func parseTimestampString(s string, loc *time.Location) (tsVal, error) {
	s = strings.TrimSpace(s)
	y, mo, d, i, ok := parseCivilDate(s)
	if !ok {
		return 0, invalidf("invalid timestamp %q", s)
	}
	h, mi, sec, us := 0, 0, 0, 0
	if i < len(s) && (s[i] == ' ' || s[i] == 'T' || s[i] == 't') && i+1 < len(s) && isDigit(s[i+1]) {
		if h, i, ok = scanInt(s, i+1, 1, 2); !ok || i >= len(s) || s[i] != ':' {
			return 0, invalidf("invalid timestamp %q", s)
		}
		if mi, i, ok = scanInt(s, i+1, 1, 2); !ok {
			return 0, invalidf("invalid timestamp %q", s)
		}
		if i < len(s) && s[i] == ':' {
			if sec, i, ok = scanInt(s, i+1, 1, 2); !ok {
				return 0, invalidf("invalid timestamp %q", s)
			}
			if i < len(s) && s[i] == '.' {
				j := i + 1
				for j < len(s) && isDigit(s[j]) {
					j++
				}
				frac := s[i+1 : j]
				if len(frac) == 0 || len(frac) > 9 {
					return 0, invalidf("invalid timestamp %q", s)
				}
				frac = (frac + "000000")[:6]
				us, _ = strconv.Atoi(frac)
				i = j
			}
		}
		if h > 23 || mi > 59 || sec > 60 {
			return 0, invalidf("invalid timestamp %q", s)
		}
	}
	rest := s[i:]
	tz := loc
	if rest != "" {
		trimmed := strings.TrimSpace(rest)
		switch {
		case trimmed == "":
		case rest == "Z" || rest == "z":
			tz = time.UTC
		case rest[0] == '+' || rest[0] == '-':
			l, err := parseOffsetZone(rest)
			if err != nil {
				return 0, err
			}
			tz = l
		case rest[0] == ' ':
			l, err := loadZone(trimmed)
			if err != nil {
				return 0, err
			}
			tz = l
		default:
			return 0, invalidf("invalid timestamp %q", s)
		}
	}
	leap := 0
	if sec == 60 {
		sec, leap = 59, 1
	}
	t := civilToTime(y, time.Month(mo), d, h, mi, sec, us*1000, tz)
	return checkTimestamp(t.UnixMicro() + int64(leap)*1e6)
}

// civilToTime converts a civil (wall clock) time in loc to an instant using
// GoogleSQL's daylight saving rules: a civil time inside a skipped hour is
// treated as if it were written an hour later (i.e. interpreted with the
// offset in effect before the transition), and an ambiguous civil time
// resolves to the earlier instant.
func civilToTime(y int, mo time.Month, d, h, mi, s, ns int, loc *time.Location) time.Time {
	if loc == time.UTC {
		return time.Date(y, mo, d, h, mi, s, ns, loc)
	}
	naive := time.Date(y, mo, d, h, mi, s, ns, time.UTC)
	offsetAt := func(t time.Time) time.Duration {
		_, off := t.In(loc).Zone()
		return time.Duration(off) * time.Second
	}
	before := offsetAt(naive.Add(-24 * time.Hour))
	after := offsetAt(naive.Add(24 * time.Hour))
	var best time.Time
	found := false
	for _, off := range []time.Duration{before, after} {
		c := naive.Add(-off)
		w := c.In(loc)
		if w.Year() == y && w.Month() == mo && w.Day() == d && w.Hour() == h && w.Minute() == mi && w.Second() == s {
			if !found || c.Before(best) {
				best, found = c, true
			}
		}
	}
	if found {
		return best
	}
	if before == after {
		return time.Date(y, mo, d, h, mi, s, ns, loc)
	}
	return naive.Add(-before)
}

var zoneCache sync.Map

// loadZone resolves a GoogleSQL time zone: a tz database name, UTC, or an
// offset such as +05:30, -8, UTC+3.
func loadZone(name string) (*time.Location, error) {
	if l, ok := zoneCache.Load(name); ok {
		return l.(*time.Location), nil
	}
	var loc *time.Location
	var err error
	switch {
	case name == "UTC" || name == "Etc/UTC" || name == "utc":
		loc = time.UTC
	case strings.HasPrefix(name, "+") || strings.HasPrefix(name, "-"):
		loc, err = parseOffsetZone(name)
	case strings.HasPrefix(strings.ToUpper(name), "UTC") && len(name) > 3 && (name[3] == '+' || name[3] == '-'):
		loc, err = parseOffsetZone(name[3:])
	default:
		loc, err = time.LoadLocation(name)
		if err != nil {
			err = evalErrorf("Invalid time zone: %s", name)
		}
	}
	if err != nil {
		return nil, err
	}
	zoneCache.Store(name, loc)
	return loc, nil
}

func parseOffsetZone(s string) (*time.Location, error) {
	sign := 1
	switch s[0] {
	case '-':
		sign = -1
	case '+':
	default:
		return nil, evalErrorf("Invalid time zone offset: %s", s)
	}
	h, i, ok := scanInt(s, 1, 1, 2)
	if !ok {
		return nil, evalErrorf("Invalid time zone offset: %s", s)
	}
	m := 0
	if i < len(s) && s[i] == ':' {
		if m, i, ok = scanInt(s, i+1, 1, 2); !ok {
			return nil, evalErrorf("Invalid time zone offset: %s", s)
		}
	} else if i+2 == len(s) && isDigit(s[i]) {
		m, i, _ = scanInt(s, i, 2, 2)
	}
	if i != len(s) || h > 14 || m > 59 {
		return nil, evalErrorf("Invalid time zone offset: %s", s)
	}
	off := sign * (h*3600 + m*60)
	name := "UTC" + s
	return time.FixedZone(name, off), nil
}

// ---------------------------------------------------------------------------
// SQL literal rendering (FORMAT %T, error messages).

func quoteSQLString(s string) string {
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
		default:
			if r < 0x20 {
				sb.WriteString(`\x` + leftPad(strconv.FormatInt(int64(r), 16), 2))
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

func quoteSQLBytes(b []byte) string {
	var sb strings.Builder
	sb.WriteString(`b"`)
	for _, c := range b {
		switch {
		case c == '"':
			sb.WriteString(`\"`)
		case c == '\\':
			sb.WriteString(`\\`)
		case c >= 0x20 && c < 0x7f:
			sb.WriteByte(c)
		default:
			sb.WriteString(`\x` + leftPad(strconv.FormatInt(int64(c), 16), 2))
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// sqlLiteral renders v as a GoogleSQL literal of type t.
func sqlLiteral(v any, t *sqlType) (string, error) {
	if v == nil {
		return "NULL", nil
	}
	switch t.kind {
	case tkBool:
		return strconv.FormatBool(v.(bool)), nil
	case tkInt64:
		return strconv.FormatInt(v.(int64), 10), nil
	case tkFloat32, tkFloat64:
		f := v.(float64)
		switch {
		case math.IsNaN(f):
			return `CAST("nan" AS ` + t.String() + `)`, nil
		case math.IsInf(f, 1):
			return `CAST("inf" AS ` + t.String() + `)`, nil
		case math.IsInf(f, -1):
			return `CAST("-inf" AS ` + t.String() + `)`, nil
		}
		s := formatFloat(f, t.kind == tkFloat32)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s, nil
	case tkString:
		return quoteSQLString(v.(string)), nil
	case tkBytes:
		return quoteSQLBytes(v.([]byte)), nil
	case tkDate:
		return `DATE "` + formatDate(v.(dateVal)) + `"`, nil
	case tkTimestamp:
		return `TIMESTAMP "` + formatTimestamp(v.(tsVal), time.UTC) + `"`, nil
	case tkArray:
		parts := []string{}
		for _, e := range v.(arrayV) {
			s, err := sqlLiteral(e, t.elem)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case tkStruct:
		parts := []string{}
		for i, e := range v.(structV) {
			s, err := sqlLiteral(e, t.fields[i].typ)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "(" + strings.Join(parts, ", ") + ")", nil
	case tkMap:
		m := v.(*mapV)
		parts := []string{}
		for i := range m.keys {
			ks, _ := sqlLiteral(m.keys[i], t.key)
			vs, _ := sqlLiteral(m.vals[i], t.val)
			parts = append(parts, ks+": "+vs)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	}
	return "", evalErrorf("cannot render value of type %s", t)
}

// textValue renders v like CAST(v AS STRING), used by FORMAT %t and
// ARRAY_TO_STRING; composite values use a literal-like syntax.
func textValue(v any, t *sqlType) string {
	if v == nil {
		return "NULL"
	}
	switch t.kind {
	case tkBytes:
		return string(v.([]byte))
	case tkArray:
		parts := []string{}
		for _, e := range v.(arrayV) {
			parts = append(parts, textValue(e, t.elem))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case tkStruct:
		parts := []string{}
		for i, e := range v.(structV) {
			parts = append(parts, textValue(e, t.fields[i].typ))
		}
		return "(" + strings.Join(parts, ", ") + ")"
	case tkMap:
		m := v.(*mapV)
		parts := []string{}
		for i := range m.keys {
			parts = append(parts, textValue(m.keys[i], t.key)+": "+textValue(m.vals[i], t.val))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	s, err := castValue(v, t, typString, time.UTC)
	if err != nil {
		return base64.StdEncoding.EncodeToString([]byte(literalText(v, t)))
	}
	return s.(string)
}

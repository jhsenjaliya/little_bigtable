package gsql

import (
	"math"
	"strings"
	"time"
)

type datePart struct {
	name    string
	weekday time.Weekday
}

func (d datePart) String() string {
	if d.name == "WEEK" && d.weekday != time.Sunday {
		return "WEEK(" + strings.ToUpper(weekdayNames[d.weekday]) + ")"
	}
	return d.name
}

var unitMicros = map[string]int64{
	"MICROSECOND": 1, "MILLISECOND": 1000, "SECOND": 1e6, "MINUTE": 60e6, "HOUR": 3600e6, "DAY": 86400e6,
}

var (
	tsAddParts    = setOf("MICROSECOND", "MILLISECOND", "SECOND", "MINUTE", "HOUR", "DAY")
	tsTruncParts  = setOf("MICROSECOND", "MILLISECOND", "SECOND", "MINUTE", "HOUR", "DAY", "WEEK", "ISOWEEK", "MONTH", "QUARTER", "YEAR", "ISOYEAR")
	dateAddParts  = setOf("DAY", "WEEK", "MONTH", "QUARTER", "YEAR")
	dateDiffParts = setOf("DAY", "WEEK", "ISOWEEK", "MONTH", "QUARTER", "YEAR", "ISOYEAR")
	lastDayParts  = setOf("WEEK", "ISOWEEK", "MONTH", "QUARTER", "YEAR", "ISOYEAR")
	extractTS     = setOf("MICROSECOND", "MILLISECOND", "SECOND", "MINUTE", "HOUR", "DAYOFWEEK", "DAY", "DAYOFYEAR", "WEEK", "ISOWEEK", "MONTH", "QUARTER", "YEAR", "ISOYEAR", "DATE")
	extractDate   = setOf("DAYOFWEEK", "DAY", "DAYOFYEAR", "WEEK", "ISOWEEK", "MONTH", "QUARTER", "YEAR", "ISOYEAR")
)

func setOf(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func datePartFromAST(x astExpr, allowed map[string]bool, fn string) (datePart, error) {
	var dp datePart
	switch e := x.(type) {
	case *astIdent:
		dp.name = strings.ToUpper(e.name)
	case *astCall:
		if e.name == "WEEK" && len(e.args) == 1 {
			if id, ok := e.args[0].(*astIdent); ok {
				for i, n := range weekdayNames {
					if strings.EqualFold(n, id.name) {
						dp = datePart{name: "WEEK", weekday: time.Weekday(i)}
						if allowed["WEEK"] {
							return dp, nil
						}
					}
				}
			}
		}
		return dp, invalidf("A valid date part name is required in %s", fn)
	default:
		return dp, invalidf("A valid date part name is required in %s", fn)
	}
	switch dp.name {
	case "TIME", "DATETIME", "NANOSECOND":
		if allowed != nil {
			return dp, unimplementedf("date part %s is not supported by the emulator", dp.name)
		}
	}
	if !allowed[dp.name] {
		return dp, invalidf("%s does not support the %s date part", fn, dp.name)
	}
	return dp, nil
}

// ---------------------------------------------------------------------------
// Date/time arithmetic helpers.

func addTimestamp(ts tsVal, n int64, unit string) (any, error) {
	d, err := mulInt64(n, unitMicros[unit])
	if err != nil {
		return nil, evalErrorf("TIMESTAMP arithmetic overflow")
	}
	s, err := addInt64(int64(ts), d.(int64))
	if err != nil {
		return nil, evalErrorf("TIMESTAMP arithmetic overflow")
	}
	return checkTimestamp(s.(int64))
}

func addDate(d dateVal, n int64, part string) (any, error) {
	switch part {
	case "DAY":
		return checkDateI(int64(d), n)
	case "WEEK":
		if n > 1e7 || n < -1e7 {
			return nil, evalErrorf("DATE arithmetic overflow")
		}
		return checkDateI(int64(d), n*7)
	case "MONTH", "QUARTER", "YEAR":
		mult := map[string]int64{"MONTH": 1, "QUARTER": 3, "YEAR": 12}[part]
		if n > 1e6 || n < -1e6 {
			return nil, evalErrorf("DATE arithmetic overflow")
		}
		y, m, day := d.civil()
		total := int64(y)*12 + int64(m-1) + n*mult
		ny, nm := floorDiv(total, 12), floorMod(total, 12)+1
		if ny < 1 || ny > 9999 {
			return nil, evalErrorf("DATE value out of range")
		}
		day = min(day, daysIn(time.Month(nm), int(ny)))
		return dateFromYMD(int(ny), time.Month(nm), day)
	}
	return nil, evalErrorf("unsupported date part %s", part)
}

func checkDateI(d, n int64) (any, error) {
	if n > 1e8 || n < -1e8 {
		return nil, evalErrorf("DATE value out of range")
	}
	return checkDate(d + n)
}

// truncDate truncates a civil date to the start of the given part.
func truncDate(d dateVal, dp datePart) dateVal {
	y, m, day := d.civil()
	t := time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
	switch dp.name {
	case "WEEK":
		back := (int(t.Weekday()) - int(dp.weekday) + 7) % 7
		return d - dateVal(back)
	case "ISOWEEK":
		back := (int(t.Weekday()) + 6) % 7
		return d - dateVal(back)
	case "MONTH":
		r, _ := dateFromYMD(y, m, 1)
		return r
	case "QUARTER":
		r, _ := dateFromYMD(y, time.Month((int(m)-1)/3*3+1), 1)
		return r
	case "YEAR":
		r, _ := dateFromYMD(y, 1, 1)
		return r
	case "ISOYEAR":
		iy, _ := t.ISOWeek()
		return isoYearStart(iy)
	}
	return d
}

func isoYearStart(iy int) dateVal {
	jan4 := time.Date(iy, 1, 4, 0, 0, 0, 0, time.UTC)
	back := (int(jan4.Weekday()) + 6) % 7
	d, _ := dateFromTime(jan4.AddDate(0, 0, -back))
	return d
}

func truncTimestamp(ts tsVal, dp datePart, loc *time.Location) (any, error) {
	if u, ok := unitMicros[dp.name]; ok && dp.name != "DAY" {
		if dp.name == "HOUR" || dp.name == "MINUTE" {
			// Truncate in local time to honor offsets that are not whole hours.
			t := ts.time().In(loc)
			sub := int64(t.Minute())*60e6 + int64(t.Second())*1e6 + int64(t.Nanosecond()/1000)
			if dp.name == "MINUTE" {
				sub = int64(t.Second())*1e6 + int64(t.Nanosecond()/1000)
			}
			return checkTimestamp(int64(ts) - sub)
		}
		return checkTimestamp(floorDiv(int64(ts), u) * u)
	}
	t := ts.time().In(loc)
	d, err := dateFromTime(t)
	if err != nil {
		return nil, err
	}
	td := truncDate(d, dp)
	y, m, day := td.civil()
	return checkTimestamp(civilToTime(y, m, day, 0, 0, 0, 0, loc).UnixMicro())
}

func isoYear(t time.Time) int {
	y, _ := t.ISOWeek()
	return y
}

func dateDiff(a, b dateVal, dp datePart) int64 {
	ay, am, ad := a.civil()
	by, bm, bd := b.civil()
	switch dp.name {
	case "DAY":
		return int64(a) - int64(b)
	case "WEEK", "ISOWEEK":
		return (int64(truncDate(a, dp)) - int64(truncDate(b, dp))) / 7
	case "MONTH":
		return int64(ay*12+int(am)) - int64(by*12+int(bm))
	case "QUARTER":
		return int64(ay*4+(int(am)-1)/3) - int64(by*4+(int(bm)-1)/3)
	case "YEAR":
		return int64(ay - by)
	case "ISOYEAR":
		return int64(isoYear(time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)) - isoYear(time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC)))
	}
	return 0
}

func extractPart(t time.Time, us int64, dp datePart) any {
	switch dp.name {
	case "MICROSECOND":
		return us
	case "MILLISECOND":
		return us / 1000
	case "SECOND":
		return int64(t.Second())
	case "MINUTE":
		return int64(t.Minute())
	case "HOUR":
		return int64(t.Hour())
	case "DAYOFWEEK":
		return int64(t.Weekday()) + 1
	case "DAY":
		return int64(t.Day())
	case "DAYOFYEAR":
		return int64(t.YearDay())
	case "WEEK":
		return int64(weekNumber(t, dp.weekday))
	case "ISOWEEK":
		_, w := t.ISOWeek()
		return int64(w)
	case "MONTH":
		return int64(t.Month())
	case "QUARTER":
		return int64((int(t.Month())-1)/3 + 1)
	case "YEAR":
		return int64(t.Year())
	case "ISOYEAR":
		return int64(isoYear(t))
	}
	return nil
}

func zoneArg(v any) (*time.Location, error) {
	if v == nil {
		return time.UTC, nil
	}
	return loadZone(v.(string))
}

func dateTime(d dateVal) time.Time {
	y, m, day := d.civil()
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// Binding helpers.

func (b *binder) bindIntervalArg(x astExpr, allowed map[string]bool, fn string) (*bexpr, string, error) {
	iv, ok := x.(*astInterval)
	if !ok {
		return nil, "", invalidf("%s requires an INTERVAL argument", fn)
	}
	n, err := b.bindExpr(iv.x)
	if err != nil {
		return nil, "", err
	}
	n, err = coerce(n, typInt64)
	if err != nil {
		return nil, "", invalidf("INTERVAL value must be INT64 in %s", fn)
	}
	if !allowed[iv.unit] {
		return nil, "", invalidf("%s does not support the %s date part", fn, iv.unit)
	}
	return n, iv.unit, nil
}

func (b *binder) bindIntervalArith(op string, base *bexpr, iv *astInterval) (*bexpr, error) {
	switch base.typ.kind {
	case tkTimestamp:
		n, unit, err := b.bindIntervalArg(iv, tsAddParts, "TIMESTAMP "+op+" INTERVAL")
		if err != nil {
			return nil, err
		}
		return tsAddExpr(op, base, n, unit), nil
	case tkDate:
		n, unit, err := b.bindIntervalArg(iv, dateAddParts, "DATE "+op+" INTERVAL")
		if err != nil {
			return nil, err
		}
		return dateAddExpr(op, base, n, unit), nil
	}
	return nil, invalidf("operator %s INTERVAL requires a DATE or TIMESTAMP operand, got %s", op, base.typ)
}

func tsAddExpr(op string, ts, n *bexpr, unit string) *bexpr {
	r := callExpr("$ts_add", typTimestamp, func(a []any) (any, error) {
		k := a[1].(int64)
		if op == "-" {
			if k == math.MinInt64 {
				return nil, evalErrorf("TIMESTAMP arithmetic overflow")
			}
			k = -k
		}
		return addTimestamp(a[0].(tsVal), k, unit)
	}, ts, n)
	r.extra = op + unit
	return r
}

func dateAddExpr(op string, d, n *bexpr, unit string) *bexpr {
	r := callExpr("$date_add", typDate, func(a []any) (any, error) {
		k := a[1].(int64)
		if op == "-" {
			if k == math.MinInt64 {
				return nil, evalErrorf("DATE arithmetic overflow")
			}
			k = -k
		}
		return addDate(a[0].(dateVal), k, unit)
	}, d, n)
	r.extra = op + unit
	return r
}

func dateAddDays(op string, d, n *bexpr) (*bexpr, error) {
	n, err := coerce(n, typInt64)
	if err != nil {
		return nil, err
	}
	return dateAddExpr(op, d, n, "DAY"), nil
}

func (b *binder) bindExtract(e *astExtract) (*bexpr, error) {
	x, err := b.bindExpr(e.x)
	if err != nil {
		return nil, err
	}
	var tz *bexpr
	if e.tz != nil {
		if tz, err = b.bindExpr(e.tz); err != nil {
			return nil, err
		}
		if tz, err = coerce(tz, typString); err != nil {
			return nil, invalidf("AT TIME ZONE requires a STRING time zone")
		}
	}
	if x.lit == litString {
		x, _ = coerce(x, typTimestamp)
	}
	switch x.typ.kind {
	case tkTimestamp:
		dp, err := datePartFromAST(e.part, extractTS, "EXTRACT")
		if err != nil {
			return nil, err
		}
		args := []*bexpr{x}
		if tz != nil {
			args = append(args, tz)
		}
		rt := typInt64
		if dp.name == "DATE" {
			rt = typDate
		}
		r := callExpr("EXTRACT", rt, func(a []any) (any, error) {
			loc := time.UTC
			if len(a) > 1 {
				l, err := zoneArg(a[1])
				if err != nil {
					return nil, err
				}
				loc = l
			}
			ts := a[0].(tsVal)
			t := ts.time().In(loc)
			if dp.name == "DATE" {
				return dateFromTime(t)
			}
			return extractPart(t, floorMod(int64(ts), 1e6), dp), nil
		}, args...)
		r.extra = dp.String()
		return r, nil
	case tkDate:
		if tz != nil {
			return nil, invalidf("EXTRACT from DATE does not support AT TIME ZONE")
		}
		dp, err := datePartFromAST(e.part, extractDate, "EXTRACT from DATE")
		if err != nil {
			return nil, err
		}
		r := callExpr("EXTRACT", typInt64, func(a []any) (any, error) {
			return extractPart(dateTime(a[0].(dateVal)), 0, dp), nil
		}, x)
		r.extra = dp.String()
		return r, nil
	}
	if isNullLit(x) {
		return constExpr(nil, typInt64, litNull), nil
	}
	return nil, invalidf("EXTRACT requires a DATE or TIMESTAMP argument, got %s", x.typ)
}

func rawArgCount(c *callCtx, lo, hi int) error {
	if n := len(c.ast.args); n < lo || n > hi {
		return invalidf("Wrong number of arguments to %s: expected %d to %d, got %d", c.name, lo, hi, n)
	}
	return nil
}

func (b *binder) bindCoerced(x astExpr, t *sqlType, fn string) (*bexpr, error) {
	e, err := b.bindExpr(x)
	if err != nil {
		return nil, err
	}
	c, err := coerce(e, t)
	if err != nil {
		return nil, invalidf("No matching signature for function %s: expected %s argument, got %s", fn, t, e.typ)
	}
	return c, nil
}

func init() {
	S, I, TS, D := typString, typInt64, typTimestamp, typDate

	registerFunc("CURRENT_TIMESTAMP", &funcDef{nondet: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if len(c.args) != 0 {
			return nil, noSignature(c)
		}
		return lazyExpr("CURRENT_TIMESTAMP", TS, func(ec *evalCtx, _ []*bexpr) (any, error) { return ec.now, nil }), nil
	}})
	registerFunc("CURRENT_DATE", &funcDef{nondet: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if len(c.args) > 1 {
			return nil, noSignature(c)
		}
		args := c.args
		if len(args) == 1 {
			tz, err := coerce(args[0], S)
			if err != nil {
				return nil, noSignature(c)
			}
			args = []*bexpr{tz}
		}
		return lazyExpr("CURRENT_DATE", D, func(ec *evalCtx, a []*bexpr) (any, error) {
			loc := time.UTC
			if len(a) == 1 {
				v, err := a[0].eval(ec)
				if err != nil {
					return nil, err
				}
				if v == nil {
					return nil, nil
				}
				if loc, err = zoneArg(v); err != nil {
					return nil, err
				}
			}
			return dateFromTime(ec.now.time().In(loc))
		}, args...), nil
	}})

	registerFunc("TIMESTAMP", bySigs(
		sig{args: []*sqlType{S, S}, minArgs: 1, ret: TS, fn: func(a []any) (any, error) {
			loc := time.UTC
			if len(a) > 1 {
				l, err := zoneArg(a[1])
				if err != nil {
					return nil, err
				}
				loc = l
			}
			ts, err := parseTimestampString(a[0].(string), loc)
			if err != nil {
				return nil, evalErrorf("Invalid timestamp: %q", a[0])
			}
			return ts, nil
		}},
		sig{args: []*sqlType{D, S}, minArgs: 1, ret: TS, fn: func(a []any) (any, error) {
			loc := time.UTC
			if len(a) > 1 {
				l, err := zoneArg(a[1])
				if err != nil {
					return nil, err
				}
				loc = l
			}
			return castValue(a[0], D, TS, loc)
		}},
	))
	registerFunc("DATE", bySigs(
		sig{args: []*sqlType{I, I, I}, ret: D, fn: func(a []any) (any, error) {
			y, m, d := a[0].(int64), a[1].(int64), a[2].(int64)
			if m < 1 || m > 12 || d < 1 || y < 1 || y > 9999 || d > int64(daysIn(time.Month(m), int(y))) {
				return nil, evalErrorf("Invalid date: DATE(%d, %d, %d)", y, m, d)
			}
			return dateFromYMD(int(y), time.Month(m), int(d))
		}},
		sig{args: []*sqlType{TS, S}, minArgs: 1, ret: D, fn: func(a []any) (any, error) {
			loc := time.UTC
			if len(a) > 1 {
				l, err := zoneArg(a[1])
				if err != nil {
					return nil, err
				}
				loc = l
			}
			return dateFromTime(a[0].(tsVal).time().In(loc))
		}},
		s1(D, D, func(v any) (any, error) { return v, nil }),
		s1(S, D, func(v any) (any, error) {
			d, err := parseDateString(v.(string))
			if err != nil {
				return nil, evalErrorf("Invalid date: %q", v)
			}
			return d, nil
		}),
	))
	registerFunc("TIMESTAMP_MICROS", bySigs(s1(I, TS, func(v any) (any, error) { return checkTimestamp(v.(int64)) })))
	registerFunc("TIMESTAMP_MILLIS", bySigs(s1(I, TS, func(v any) (any, error) {
		r, err := mulInt64(v.(int64), 1000)
		if err != nil {
			return nil, evalErrorf("TIMESTAMP out of range")
		}
		return checkTimestamp(r.(int64))
	})))
	registerFunc("TIMESTAMP_SECONDS", bySigs(s1(I, TS, func(v any) (any, error) {
		r, err := mulInt64(v.(int64), 1e6)
		if err != nil {
			return nil, evalErrorf("TIMESTAMP out of range")
		}
		return checkTimestamp(r.(int64))
	})))
	for name, mult := range map[string]int64{"TIMESTAMP_FROM_UNIX_MICROS": 1, "TIMESTAMP_FROM_UNIX_MILLIS": 1000, "TIMESTAMP_FROM_UNIX_SECONDS": 1e6} {
		m := mult
		registerFunc(name, bySigs(
			s1(I, TS, func(v any) (any, error) {
				r, err := mulInt64(v.(int64), m)
				if err != nil {
					return nil, evalErrorf("TIMESTAMP out of range")
				}
				return checkTimestamp(r.(int64))
			}),
			s1(TS, TS, func(v any) (any, error) { return v, nil }),
		))
	}
	for name, div := range map[string]int64{"UNIX_MICROS": 1, "UNIX_MILLIS": 1000, "UNIX_SECONDS": 1e6} {
		d := div
		registerFunc(name, bySigs(s1(TS, I, func(v any) (any, error) { return floorDiv(int64(v.(tsVal)), d), nil })))
	}
	registerFunc("UNIX_DATE", bySigs(s1(D, I, func(v any) (any, error) { return int64(v.(dateVal)), nil })))
	registerFunc("DATE_FROM_UNIX_DATE", bySigs(s1(I, D, func(v any) (any, error) { return checkDate(v.(int64)) })))
	registerFunc("STRING", bySigs(sig{args: []*sqlType{TS, S}, minArgs: 1, ret: S, fn: func(a []any) (any, error) {
		loc := time.UTC
		if len(a) > 1 {
			l, err := zoneArg(a[1])
			if err != nil {
				return nil, err
			}
			loc = l
		}
		return formatTimestamp(a[0].(tsVal), loc), nil
	}}))
	registerFunc("FORMAT_TIMESTAMP", bySigs(sig{args: []*sqlType{S, TS, S}, minArgs: 2, ret: S, fn: func(a []any) (any, error) {
		loc := time.UTC
		if len(a) > 2 {
			l, err := zoneArg(a[2])
			if err != nil {
				return nil, err
			}
			loc = l
		}
		ts := a[1].(tsVal)
		return formatTime(a[0].(string), ts.time().In(loc), floorMod(int64(ts), 1e6))
	}}))
	registerFunc("FORMAT_DATE", bySigs(s2(S, D, S, func(f, d any) (any, error) {
		return formatTime(f.(string), dateTime(d.(dateVal)), 0)
	})))
	registerFunc("PARSE_TIMESTAMP", bySigs(sig{args: []*sqlType{S, S, S}, minArgs: 2, ret: TS, fn: func(a []any) (any, error) {
		loc := time.UTC
		if len(a) > 2 {
			l, err := zoneArg(a[2])
			if err != nil {
				return nil, err
			}
			loc = l
		}
		t, _, err := parseTimeFormat(a[0].(string), a[1].(string), loc)
		if err != nil {
			return nil, err
		}
		return checkTimestamp(t.UnixMicro())
	}}))
	registerFunc("PARSE_DATE", bySigs(s2(S, S, D, func(f, s any) (any, error) {
		t, _, err := parseTimeFormat(f.(string), s.(string), time.UTC)
		if err != nil {
			return nil, err
		}
		return dateFromTime(t)
	})))

	// Functions with date-part / INTERVAL arguments.
	tsAddSub := func(op string) *funcDef {
		return &funcDef{raw: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
			if err := rawArgCount(c, 2, 2); err != nil {
				return nil, err
			}
			ts, err := b.bindCoerced(c.ast.args[0], TS, c.name)
			if err != nil {
				return nil, err
			}
			n, unit, err := b.bindIntervalArg(c.ast.args[1], tsAddParts, c.name)
			if err != nil {
				return nil, err
			}
			return tsAddExpr(op, ts, n, unit), nil
		}}
	}
	registerFunc("TIMESTAMP_ADD", tsAddSub("+"))
	registerFunc("TIMESTAMP_SUB", tsAddSub("-"))
	dateAddSub := func(op string) *funcDef {
		return &funcDef{raw: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
			if err := rawArgCount(c, 2, 2); err != nil {
				return nil, err
			}
			d, err := b.bindCoerced(c.ast.args[0], D, c.name)
			if err != nil {
				return nil, err
			}
			n, unit, err := b.bindIntervalArg(c.ast.args[1], dateAddParts, c.name)
			if err != nil {
				return nil, err
			}
			return dateAddExpr(op, d, n, unit), nil
		}}
	}
	registerFunc("DATE_ADD", dateAddSub("+"))
	registerFunc("DATE_SUB", dateAddSub("-"))
	registerFunc("TIMESTAMP_DIFF", &funcDef{raw: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := rawArgCount(c, 3, 3); err != nil {
			return nil, err
		}
		x, err := b.bindCoerced(c.ast.args[0], TS, c.name)
		if err != nil {
			return nil, err
		}
		y, err := b.bindCoerced(c.ast.args[1], TS, c.name)
		if err != nil {
			return nil, err
		}
		dp, err := datePartFromAST(c.ast.args[2], tsAddParts, c.name)
		if err != nil {
			return nil, err
		}
		u := unitMicros[dp.name]
		r := callExpr("TIMESTAMP_DIFF", I, func(a []any) (any, error) {
			return (int64(a[0].(tsVal)) - int64(a[1].(tsVal))) / u, nil
		}, x, y)
		r.extra = dp.String()
		return r, nil
	}})
	registerFunc("DATE_DIFF", &funcDef{raw: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := rawArgCount(c, 3, 3); err != nil {
			return nil, err
		}
		x, err := b.bindCoerced(c.ast.args[0], D, c.name)
		if err != nil {
			return nil, err
		}
		y, err := b.bindCoerced(c.ast.args[1], D, c.name)
		if err != nil {
			return nil, err
		}
		dp, err := datePartFromAST(c.ast.args[2], dateDiffParts, c.name)
		if err != nil {
			return nil, err
		}
		r := callExpr("DATE_DIFF", I, func(a []any) (any, error) {
			return dateDiff(a[0].(dateVal), a[1].(dateVal), dp), nil
		}, x, y)
		r.extra = dp.String()
		return r, nil
	}})
	tsTrunc := func(b *binder, c *callCtx, x *bexpr) (*bexpr, error) {
		dp, err := datePartFromAST(c.ast.args[1], tsTruncParts, c.name)
		if err != nil {
			return nil, err
		}
		args := []*bexpr{x}
		if len(c.ast.args) > 2 {
			tz, err := b.bindCoerced(c.ast.args[2], S, c.name)
			if err != nil {
				return nil, err
			}
			args = append(args, tz)
		}
		r := callExpr("TIMESTAMP_TRUNC", TS, func(a []any) (any, error) {
			loc := time.UTC
			if len(a) > 1 {
				l, err := zoneArg(a[1])
				if err != nil {
					return nil, err
				}
				loc = l
			}
			return truncTimestamp(a[0].(tsVal), dp, loc)
		}, args...)
		r.extra = dp.String()
		return r, nil
	}
	registerFunc("TIMESTAMP_TRUNC", &funcDef{raw: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := rawArgCount(c, 2, 3); err != nil {
			return nil, err
		}
		x, err := b.bindCoerced(c.ast.args[0], TS, c.name)
		if err != nil {
			return nil, err
		}
		return tsTrunc(b, c, x)
	}})
	registerFunc("DATE_TRUNC", &funcDef{raw: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := rawArgCount(c, 2, 3); err != nil {
			return nil, err
		}
		x, err := b.bindExpr(c.ast.args[0])
		if err != nil {
			return nil, err
		}
		if x.typ.kind == tkTimestamp {
			return tsTrunc(b, c, x)
		}
		if x, err = coerce(x, D); err != nil {
			return nil, invalidf("No matching signature for function DATE_TRUNC for argument type %s", x.typ)
		}
		if len(c.ast.args) > 2 {
			return nil, invalidf("DATE_TRUNC with a DATE argument does not accept a time zone")
		}
		dp, err := datePartFromAST(c.ast.args[1], dateDiffParts, c.name)
		if err != nil {
			return nil, err
		}
		r := callExpr("DATE_TRUNC", D, func(a []any) (any, error) { return truncDate(a[0].(dateVal), dp), nil }, x)
		r.extra = dp.String()
		return r, nil
	}})
	registerFunc("LAST_DAY", &funcDef{raw: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := rawArgCount(c, 1, 2); err != nil {
			return nil, err
		}
		x, err := b.bindCoerced(c.ast.args[0], D, c.name)
		if err != nil {
			return nil, err
		}
		dp := datePart{name: "MONTH"}
		if len(c.ast.args) > 1 {
			if dp, err = datePartFromAST(c.ast.args[1], lastDayParts, c.name); err != nil {
				return nil, err
			}
		}
		r := callExpr("LAST_DAY", D, func(a []any) (any, error) {
			start := truncDate(a[0].(dateVal), dp)
			switch dp.name {
			case "WEEK", "ISOWEEK":
				return checkDate(int64(start) + 6)
			case "MONTH":
				n, err := addDate(start, 1, "MONTH")
				if err != nil {
					return nil, err
				}
				return n.(dateVal) - 1, nil
			case "QUARTER":
				n, err := addDate(start, 3, "MONTH")
				if err != nil {
					return nil, err
				}
				return n.(dateVal) - 1, nil
			case "YEAR":
				y, _, _ := start.civil()
				return dateFromYMD(y, 12, 31)
			}
			// ISOYEAR: the day before the next ISO year starts.
			return isoYearStart(isoYear(dateTime(start))+1) - 1, nil
		}, x)
		r.extra = dp.String()
		return r, nil
	}})
	registerFunc("GENERATE_DATE_ARRAY", &funcDef{raw: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := rawArgCount(c, 2, 3); err != nil {
			return nil, err
		}
		start, err := b.bindCoerced(c.ast.args[0], D, c.name)
		if err != nil {
			return nil, err
		}
		end, err := b.bindCoerced(c.ast.args[1], D, c.name)
		if err != nil {
			return nil, err
		}
		args := []*bexpr{start, end}
		unit := "DAY"
		if len(c.ast.args) > 2 {
			n, u, err := b.bindIntervalArg(c.ast.args[2], dateAddParts, c.name)
			if err != nil {
				return nil, err
			}
			args = append(args, n)
			unit = u
		}
		r := callExpr("GENERATE_DATE_ARRAY", arrayOf(D), func(a []any) (any, error) {
			step := int64(1)
			if len(a) > 2 {
				step = a[2].(int64)
			}
			if step == 0 {
				return nil, evalErrorf("GENERATE_DATE_ARRAY step cannot be 0")
			}
			s, e := a[0].(dateVal), a[1].(dateVal)
			out := arrayV{}
			for i := int64(0); ; i++ {
				v, err := addDate(s, step*i, unit)
				if err != nil {
					break
				}
				d := v.(dateVal)
				if (step > 0 && d > e) || (step < 0 && d < e) {
					break
				}
				out = append(out, d)
				if len(out) > 1e6 {
					return nil, evalErrorf("GENERATE_DATE_ARRAY produced too many elements")
				}
			}
			return out, nil
		}, args...)
		r.extra = unit
		return r, nil
	}})
	registerFunc("GENERATE_TIMESTAMP_ARRAY", &funcDef{raw: true, bind: func(b *binder, c *callCtx) (*bexpr, error) {
		if err := rawArgCount(c, 3, 3); err != nil {
			return nil, err
		}
		start, err := b.bindCoerced(c.ast.args[0], TS, c.name)
		if err != nil {
			return nil, err
		}
		end, err := b.bindCoerced(c.ast.args[1], TS, c.name)
		if err != nil {
			return nil, err
		}
		n, unit, err := b.bindIntervalArg(c.ast.args[2], tsAddParts, c.name)
		if err != nil {
			return nil, err
		}
		r := callExpr("GENERATE_TIMESTAMP_ARRAY", arrayOf(TS), func(a []any) (any, error) {
			step := a[2].(int64)
			if step == 0 {
				return nil, evalErrorf("GENERATE_TIMESTAMP_ARRAY step cannot be 0")
			}
			s, e := a[0].(tsVal), a[1].(tsVal)
			out := arrayV{}
			for cur := s; (step > 0 && cur <= e) || (step < 0 && cur >= e); {
				out = append(out, cur)
				if len(out) > 1e6 {
					return nil, evalErrorf("GENERATE_TIMESTAMP_ARRAY produced too many elements")
				}
				nv, err := addTimestamp(cur, step, unit)
				if err != nil {
					break
				}
				cur = nv.(tsVal)
			}
			return out, nil
		}, start, end, n)
		r.extra = unit
		return r, nil
	}})
}

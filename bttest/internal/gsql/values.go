package gsql

import (
	"bytes"
	"encoding/binary"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Runtime value representation. The static type of every expression is known
// after analysis, so values carry only the Go representation:
//
//	NULL       -> untyped nil
//	BOOL       -> bool
//	INT64      -> int64
//	FLOAT32/64 -> float64 (FLOAT32 values are kept rounded to float32 precision)
//	STRING     -> string
//	BYTES      -> []byte
//	TIMESTAMP  -> tsVal (microseconds since the Unix epoch, UTC)
//	DATE       -> dateVal (days since 1970-01-01)
//	ARRAY      -> arrayV
//	STRUCT     -> structV
//	MAP        -> *mapV (entries sorted by key, unique keys)
type (
	tsVal   int64
	dateVal int32
	arrayV  []any
	structV []any
	mapV    struct {
		keys []any
		vals []any
	}
)

const (
	minTimestampMicros = int64(-62135596800) * 1e6
	maxTimestampMicros = int64(253402300799999999)
	minDateDays        = -719162
	maxDateDays        = 2932896
)

func checkTimestamp(us int64) (tsVal, error) {
	if us < minTimestampMicros || us > maxTimestampMicros {
		return 0, evalErrorf("timestamp value is out of the supported range")
	}
	return tsVal(us), nil
}

func checkDate(days int64) (dateVal, error) {
	if days < minDateDays || days > maxDateDays {
		return 0, evalErrorf("date value is out of the supported range")
	}
	return dateVal(days), nil
}

func (t tsVal) time() time.Time { return time.UnixMicro(int64(t)).UTC() }

func (d dateVal) civil() (int, time.Month, int) {
	t := time.Unix(int64(d)*86400, 0).UTC()
	return t.Date()
}

func dateFromYMD(y int, m time.Month, d int) (dateVal, error) {
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return checkDate(floorDiv(t.Unix(), 86400))
}

func dateFromTime(t time.Time) (dateVal, error) {
	y, m, d := t.Date()
	return dateFromYMD(y, m, d)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func floorMod(a, b int64) int64 { return a - floorDiv(a, b)*b }

func newMap(keys, vals []any) *mapV {
	m := &mapV{keys: keys, vals: vals}
	if !sort.IsSorted(mapSorter{m}) {
		sort.Stable(mapSorter{m})
	}
	// Deduplicate keys: the last value wins, matching Bigtable map semantics.
	if len(m.keys) > 1 {
		ok, ov := m.keys[:1], m.vals[:1]
		for i := 1; i < len(m.keys); i++ {
			if compareValues(m.keys[i], ok[len(ok)-1]) == 0 {
				ov[len(ov)-1] = m.vals[i]
				continue
			}
			ok = append(ok, m.keys[i])
			ov = append(ov, m.vals[i])
		}
		m.keys, m.vals = ok, ov
	}
	return m
}

type mapSorter struct{ m *mapV }

func (s mapSorter) Len() int           { return len(s.m.keys) }
func (s mapSorter) Less(i, j int) bool { return compareValues(s.m.keys[i], s.m.keys[j]) < 0 }
func (s mapSorter) Swap(i, j int) {
	s.m.keys[i], s.m.keys[j] = s.m.keys[j], s.m.keys[i]
	s.m.vals[i], s.m.vals[j] = s.m.vals[j], s.m.vals[i]
}

// lookup returns the value for key k (NULL if absent).
func (m *mapV) lookup(k any) (any, bool) {
	i := sort.Search(len(m.keys), func(i int) bool { return compareValues(m.keys[i], k) >= 0 })
	if i < len(m.keys) && compareValues(m.keys[i], k) == 0 {
		return m.vals[i], true
	}
	return nil, false
}

// compareValues defines a total order used for sorting, grouping and
// MIN/MAX: NULL first, NaN before all other floats, -0 == 0.
func compareValues(a, b any) int {
	if a == nil || b == nil {
		switch {
		case a == nil && b == nil:
			return 0
		case a == nil:
			return -1
		default:
			return 1
		}
	}
	switch x := a.(type) {
	case bool:
		y := b.(bool)
		switch {
		case x == y:
			return 0
		case !x:
			return -1
		default:
			return 1
		}
	case int64:
		switch y := b.(type) {
		case int64:
			return cmpInt(x, y)
		case float64:
			return cmpFloat(float64(x), y)
		}
	case float64:
		switch y := b.(type) {
		case float64:
			return cmpFloat(x, y)
		case int64:
			return cmpFloat(x, float64(y))
		}
	case string:
		return strings.Compare(x, b.(string))
	case []byte:
		return bytes.Compare(x, b.([]byte))
	case tsVal:
		return cmpInt(int64(x), int64(b.(tsVal)))
	case dateVal:
		return cmpInt(int64(x), int64(b.(dateVal)))
	case arrayV:
		y := b.(arrayV)
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := compareValues(x[i], y[i]); c != 0 {
				return c
			}
		}
		return cmpInt(int64(len(x)), int64(len(y)))
	case structV:
		y := b.(structV)
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := compareValues(x[i], y[i]); c != 0 {
				return c
			}
		}
		return cmpInt(int64(len(x)), int64(len(y)))
	case *mapV:
		y := b.(*mapV)
		for i := 0; i < len(x.keys) && i < len(y.keys); i++ {
			if c := compareValues(x.keys[i], y.keys[i]); c != 0 {
				return c
			}
			if c := compareValues(x.vals[i], y.vals[i]); c != 0 {
				return c
			}
		}
		return cmpInt(int64(len(x.keys)), int64(len(y.keys)))
	}
	return 0
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpFloat(a, b float64) int {
	an, bn := math.IsNaN(a), math.IsNaN(b)
	switch {
	case an && bn:
		return 0
	case an:
		return -1
	case bn:
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// sqlEquals implements SQL '=' with three-valued logic. The returned value is
// nil (NULL), true or false.
func sqlEquals(a, b any) any {
	if a == nil || b == nil {
		return nil
	}
	switch x := a.(type) {
	case float64:
		switch y := b.(type) {
		case float64:
			return x == y
		case int64:
			return x == float64(y)
		}
	case int64:
		if y, ok := b.(float64); ok {
			return float64(x) == y
		}
	case structV:
		y := b.(structV)
		if len(x) != len(y) {
			return false
		}
		var res any = true
		for i := range x {
			switch sqlEquals(x[i], y[i]) {
			case false:
				return false
			case nil:
				res = nil
			}
		}
		return res
	case arrayV:
		y := b.(arrayV)
		if len(x) != len(y) {
			return false
		}
		var res any = true
		for i := range x {
			switch sqlEquals(x[i], y[i]) {
			case false:
				return false
			case nil:
				res = nil
			}
		}
		return res
	case *mapV:
		y := b.(*mapV)
		if len(x.keys) != len(y.keys) {
			return false
		}
		var res any = true
		for i := range x.keys {
			if compareValues(x.keys[i], y.keys[i]) != 0 {
				return false
			}
			switch sqlEquals(x.vals[i], y.vals[i]) {
			case false:
				return false
			case nil:
				res = nil
			}
		}
		return res
	}
	return compareValues(a, b) == 0
}

// valueKey returns a canonical encoding of v suitable for hashing in GROUP BY,
// DISTINCT and set operations. NaNs are equal to each other and -0 == 0.
func valueKey(v any) string {
	return string(appendValueKey(nil, v))
}

func appendValueKey(buf []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(buf, 'N')
	case bool:
		if x {
			return append(buf, 'b', 1)
		}
		return append(buf, 'b', 0)
	case int64:
		buf = append(buf, 'i')
		return binary.BigEndian.AppendUint64(buf, uint64(x))
	case float64:
		buf = append(buf, 'f')
		switch {
		case math.IsNaN(x):
			x = math.NaN()
		case x == 0:
			x = 0
		}
		return binary.BigEndian.AppendUint64(buf, math.Float64bits(x))
	case string:
		buf = append(buf, 's')
		buf = binary.AppendUvarint(buf, uint64(len(x)))
		return append(buf, x...)
	case []byte:
		buf = append(buf, 'y')
		buf = binary.AppendUvarint(buf, uint64(len(x)))
		return append(buf, x...)
	case tsVal:
		buf = append(buf, 't')
		return binary.BigEndian.AppendUint64(buf, uint64(x))
	case dateVal:
		buf = append(buf, 'd')
		return binary.BigEndian.AppendUint32(buf, uint32(x))
	case arrayV:
		buf = append(buf, 'a')
		buf = binary.AppendUvarint(buf, uint64(len(x)))
		for _, e := range x {
			buf = appendValueKey(buf, e)
		}
		return buf
	case structV:
		buf = append(buf, 'r')
		buf = binary.AppendUvarint(buf, uint64(len(x)))
		for _, e := range x {
			buf = appendValueKey(buf, e)
		}
		return buf
	case *mapV:
		buf = append(buf, 'm')
		buf = binary.AppendUvarint(buf, uint64(len(x.keys)))
		for i := range x.keys {
			buf = appendValueKey(buf, x.keys[i])
			buf = appendValueKey(buf, x.vals[i])
		}
		return buf
	}
	return append(buf, '?')
}

// ---------------------------------------------------------------------------
// Wire conversion.

// toProtoValue converts a runtime value of type t into a btpb.Value. NULL is
// a Value with no kind set. Nested values never carry a type.
func toProtoValue(v any, t *sqlType) *btpb.Value {
	if v == nil {
		return &btpb.Value{}
	}
	switch t.kind {
	case tkBool:
		return &btpb.Value{Kind: &btpb.Value_BoolValue{BoolValue: v.(bool)}}
	case tkInt64:
		return &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: v.(int64)}}
	case tkFloat32, tkFloat64:
		return &btpb.Value{Kind: &btpb.Value_FloatValue{FloatValue: v.(float64)}}
	case tkString:
		return &btpb.Value{Kind: &btpb.Value_StringValue{StringValue: v.(string)}}
	case tkBytes:
		b := v.([]byte)
		if b == nil {
			b = []byte{}
		}
		return &btpb.Value{Kind: &btpb.Value_BytesValue{BytesValue: b}}
	case tkTimestamp:
		us := int64(v.(tsVal))
		return &btpb.Value{Kind: &btpb.Value_TimestampValue{TimestampValue: &timestamppb.Timestamp{
			Seconds: floorDiv(us, 1e6), Nanos: int32(floorMod(us, 1e6) * 1000)}}}
	case tkDate:
		y, m, d := v.(dateVal).civil()
		return &btpb.Value{Kind: &btpb.Value_DateValue{DateValue: &date.Date{Year: int32(y), Month: int32(m), Day: int32(d)}}}
	case tkArray:
		a := v.(arrayV)
		vals := make([]*btpb.Value, len(a))
		for i, e := range a {
			vals[i] = toProtoValue(e, t.elem)
		}
		return &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{Values: vals}}}
	case tkStruct:
		s := v.(structV)
		vals := make([]*btpb.Value, len(s))
		for i, e := range s {
			vals[i] = toProtoValue(e, t.fields[i].typ)
		}
		return &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{Values: vals}}}
	case tkMap:
		m := v.(*mapV)
		vals := make([]*btpb.Value, len(m.keys))
		for i := range m.keys {
			vals[i] = &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{Values: []*btpb.Value{
				toProtoValue(m.keys[i], t.key), toProtoValue(m.vals[i], t.val)}}}}
		}
		return &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{Values: vals}}}
	}
	return &btpb.Value{}
}

// fromProtoValue converts a wire value to a runtime value of type t,
// validating that the value kind matches the type.
func fromProtoValue(pv *btpb.Value, t *sqlType, nested bool) (any, error) {
	if pv == nil {
		return nil, nil
	}
	if nested && pv.GetType() != nil {
		return nil, invalidf("nested values must not specify a type")
	}
	if pv.Kind == nil {
		return nil, nil
	}
	mismatch := func() error {
		return invalidf("value of kind %T does not match type %s", pv.Kind, t)
	}
	switch t.kind {
	case tkBool:
		if k, ok := pv.Kind.(*btpb.Value_BoolValue); ok {
			return k.BoolValue, nil
		}
	case tkInt64:
		if k, ok := pv.Kind.(*btpb.Value_IntValue); ok {
			return k.IntValue, nil
		}
	case tkFloat32:
		if k, ok := pv.Kind.(*btpb.Value_FloatValue); ok {
			return float64(float32(k.FloatValue)), nil
		}
	case tkFloat64:
		if k, ok := pv.Kind.(*btpb.Value_FloatValue); ok {
			return k.FloatValue, nil
		}
	case tkString:
		if k, ok := pv.Kind.(*btpb.Value_StringValue); ok {
			return k.StringValue, nil
		}
	case tkBytes:
		if k, ok := pv.Kind.(*btpb.Value_BytesValue); ok {
			if k.BytesValue == nil {
				return []byte{}, nil
			}
			return k.BytesValue, nil
		}
	case tkTimestamp:
		if k, ok := pv.Kind.(*btpb.Value_TimestampValue); ok {
			if k.TimestampValue == nil {
				return nil, nil
			}
			if err := k.TimestampValue.CheckValid(); err != nil {
				return nil, invalidf("invalid timestamp value: %v", err)
			}
			us := k.TimestampValue.GetSeconds()*1e6 + int64(k.TimestampValue.GetNanos()/1000)
			ts, err := checkTimestamp(us)
			if err != nil {
				return nil, invalidf("timestamp value is out of range")
			}
			return ts, nil
		}
	case tkDate:
		if k, ok := pv.Kind.(*btpb.Value_DateValue); ok {
			if k.DateValue == nil {
				return nil, nil
			}
			d := k.DateValue
			if d.Month < 1 || d.Month > 12 || d.Day < 1 || d.Day > 31 {
				return nil, invalidf("invalid date value %v", d)
			}
			dv, err := dateFromYMD(int(d.Year), time.Month(d.Month), int(d.Day))
			if err != nil {
				return nil, invalidf("date value is out of range")
			}
			if y, m, dd := dv.civil(); y != int(d.Year) || int32(m) != d.Month || int32(dd) != d.Day {
				return nil, invalidf("invalid date value %d-%d-%d", d.Year, d.Month, d.Day)
			}
			return dv, nil
		}
	case tkArray:
		if k, ok := pv.Kind.(*btpb.Value_ArrayValue); ok {
			out := make(arrayV, len(k.ArrayValue.GetValues()))
			for i, e := range k.ArrayValue.GetValues() {
				v, err := fromProtoValue(e, t.elem, true)
				if err != nil {
					return nil, err
				}
				out[i] = v
			}
			return out, nil
		}
	case tkStruct:
		if k, ok := pv.Kind.(*btpb.Value_ArrayValue); ok {
			vals := k.ArrayValue.GetValues()
			if len(vals) != len(t.fields) {
				return nil, invalidf("struct value has %d fields, expected %d", len(vals), len(t.fields))
			}
			out := make(structV, len(vals))
			for i, e := range vals {
				v, err := fromProtoValue(e, t.fields[i].typ, true)
				if err != nil {
					return nil, err
				}
				out[i] = v
			}
			return out, nil
		}
	case tkMap:
		if k, ok := pv.Kind.(*btpb.Value_ArrayValue); ok {
			var keys, vals []any
			for _, e := range k.ArrayValue.GetValues() {
				pair, ok := e.Kind.(*btpb.Value_ArrayValue)
				if !ok || len(pair.ArrayValue.GetValues()) != 2 {
					return nil, invalidf("invalid map entry")
				}
				kv, err := fromProtoValue(pair.ArrayValue.Values[0], t.key, true)
				if err != nil {
					return nil, err
				}
				if kv == nil {
					return nil, invalidf("map keys must not be NULL")
				}
				vv, err := fromProtoValue(pair.ArrayValue.Values[1], t.val, true)
				if err != nil {
					return nil, err
				}
				keys = append(keys, kv)
				vals = append(vals, vv)
			}
			return newMap(keys, vals), nil
		}
	}
	return nil, mismatch()
}

// ---------------------------------------------------------------------------
// Text formatting (CAST AS STRING and friends).

func formatFloat(f float64, is32 bool) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	if is32 {
		s := strconv.FormatFloat(f, 'g', 6, 32)
		if p, err := strconv.ParseFloat(s, 32); err != nil || float32(p) != float32(f) {
			s = strconv.FormatFloat(f, 'g', 9, 32)
		}
		return s
	}
	s := strconv.FormatFloat(f, 'g', 15, 64)
	if p, err := strconv.ParseFloat(s, 64); err != nil || p != f {
		s = strconv.FormatFloat(f, 'g', 17, 64)
	}
	return s
}

// formatTimestamp renders a timestamp in the canonical GoogleSQL format, e.g.
// "2008-12-25 15:30:00.123+00", in the given location.
func formatTimestamp(ts tsVal, loc *time.Location) string {
	t := ts.time().In(loc)
	var sb strings.Builder
	sb.WriteString(t.Format("2006-01-02 15:04:05"))
	us := floorMod(int64(ts), 1e6)
	switch {
	case us == 0:
	case us%1000 == 0:
		sb.WriteString("." + leftPad(strconv.FormatInt(us/1000, 10), 3))
	default:
		sb.WriteString("." + leftPad(strconv.FormatInt(us, 10), 6))
	}
	sb.WriteString(formatOffset(t, false))
	return sb.String()
}

// formatOffset renders a UTC offset as +HH or +HH:MM (compact) or +HH:MM.
func formatOffset(t time.Time, full bool) string {
	_, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	h, m := off/3600, (off%3600)/60
	if full || m != 0 {
		return sign + leftPad(strconv.Itoa(h), 2) + ":" + leftPad(strconv.Itoa(m), 2)
	}
	return sign + leftPad(strconv.Itoa(h), 2)
}

func leftPad(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	return s
}

func formatDate(d dateVal) string {
	y, m, dd := d.civil()
	return leftPad(strconv.Itoa(y), 4) + "-" + leftPad(strconv.Itoa(int(m)), 2) + "-" + leftPad(strconv.Itoa(dd), 2)
}

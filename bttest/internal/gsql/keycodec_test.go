package gsql

import (
	"bytes"
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func strV(s string) *btpb.Value { return &btpb.Value{Kind: &btpb.Value_StringValue{StringValue: s}} }
func bytesV(b string) *btpb.Value {
	return &btpb.Value{Kind: &btpb.Value_BytesValue{BytesValue: []byte(b)}}
}
func intV(i int64) *btpb.Value { return &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: i}} }

func structSchema(enc *btpb.Type_Struct_Encoding, fields ...*btpb.Type) *btpb.Type_Struct {
	s := &btpb.Type_Struct{Encoding: enc}
	for i, f := range fields {
		s.Fields = append(s.Fields, &btpb.Type_Struct_Field{FieldName: string(rune('a' + i)), Type: f})
	}
	return s
}

var (
	orderedEnc = &btpb.Type_Struct_Encoding{Encoding: &btpb.Type_Struct_Encoding_OrderedCodeBytes_{OrderedCodeBytes: &btpb.Type_Struct_Encoding_OrderedCodeBytes{}}}
	hashEnc    = &btpb.Type_Struct_Encoding{Encoding: &btpb.Type_Struct_Encoding_DelimitedBytes_{DelimitedBytes: &btpb.Type_Struct_Encoding_DelimitedBytes{Delimiter: []byte("#")}}}
	singleEnc  = &btpb.Type_Struct_Encoding{Encoding: &btpb.Type_Struct_Encoding_Singleton_{Singleton: &btpb.Type_Struct_Encoding_Singleton{}}}
	beInt      = &btpb.Type{Kind: &btpb.Type_Int64Type{Int64Type: &btpb.Type_Int64{Encoding: &btpb.Type_Int64_Encoding{
		Encoding: &btpb.Type_Int64_Encoding_BigEndianBytes_{BigEndianBytes: &btpb.Type_Int64_Encoding_BigEndianBytes{}}}}}}
)

func TestOrderedCodeInt64(t *testing.T) {
	vectors := map[int64]string{
		0:   "\x80",
		1:   "\x81",
		-1:  "\x7f",
		63:  "\xbf",
		-64: "\x40",
		64:  "\xc0\x40",
		-65: "\x3f\xbf",
	}
	for v, want := range vectors {
		require.Equal(t, []byte(want), appendOrderedInt64(nil, v), "%d", v)
	}
	lengths := map[int64]int{0: 1, 63: 1, 64: 2, -64: 1, -65: 2, 8191: 2, 8192: 3, math.MaxInt64: 10, math.MinInt64: 10, 1 << 55: 9, 1<<55 - 1: 8}
	for v, n := range lengths {
		require.Len(t, appendOrderedInt64(nil, v), n, "%d", v)
	}
	vals := []int64{math.MinInt64, math.MinInt64 + 1, -1 << 40, -65, -64, -1, 0, 1, 63, 64, 1 << 20, 1 << 62, math.MaxInt64 - 1, math.MaxInt64}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		vals = append(vals, int64(r.Uint64())>>uint(r.Intn(64)))
	}
	for _, v := range vals {
		enc := appendOrderedInt64(nil, v)
		got, n, err := decodeOrderedInt64(append(enc, 0xAB))
		require.NoError(t, err)
		require.Equal(t, v, got)
		require.Equal(t, len(enc), n)
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	for i := 1; i < len(vals); i++ {
		a, b := appendOrderedInt64(nil, vals[i-1]), appendOrderedInt64(nil, vals[i])
		if vals[i-1] == vals[i] {
			require.Equal(t, a, b)
		} else {
			require.Equal(t, -1, bytes.Compare(a, b), "%d < %d", vals[i-1], vals[i])
		}
	}
	_, _, err := decodeOrderedInt64(nil)
	require.Error(t, err)
	_, _, err = decodeOrderedInt64([]byte{0xc0})
	require.Error(t, err, "truncated")
	_, _, err = decodeOrderedInt64([]byte{0xc0, 0x01})
	require.Error(t, err, "non-canonical")
}

func TestOrderedCodeStructExamples(t *testing.T) {
	s := keyFieldProto(typString)
	for _, c := range []struct {
		vals []string
		want string
	}{
		{[]string{"", ""}, "\x00\x00"},
		{[]string{"", "B"}, "\x00\x00\x00\x01B"},
		{[]string{"A", ""}, "A"},
		{[]string{"", "B", ""}, "\x00\x00\x00\x01B"},
		{[]string{"A", "", "C"}, "A\x00\x01\x00\x00\x00\x01C"},
		{[]string{"A\x00B", "C"}, "A\x00\xffB\x00\x01C"},
	} {
		types := make([]*btpb.Type, len(c.vals))
		vals := make([]*btpb.Value, len(c.vals))
		for i, v := range c.vals {
			types[i] = s
			vals[i] = strV(v)
		}
		schema := structSchema(orderedEnc, types...)
		key, err := EncodeKey(schema, vals)
		require.NoError(t, err)
		require.Equal(t, []byte(c.want), key, "%q", c.vals)
		dec, err := DecodeKey(schema, key)
		require.NoError(t, err)
		for i := range c.vals {
			require.Equal(t, c.vals[i], dec[i].GetStringValue())
		}
	}
}

type tuple struct {
	s string
	n int64
	b []byte
}

func TestOrderedCodeStructPreservesOrder(t *testing.T) {
	schema := structSchema(orderedEnc, keyFieldProto(typString), keyFieldProto(typInt64), keyFieldProto(typBytes))
	r := rand.New(rand.NewSource(7))
	alphabet := []string{"", "a", "ab", "b", "\x00", "a\x00", "\x01", "\xff"}
	ints := []int64{math.MinInt64, -1000, -65, -64, -1, 0, 1, 63, 64, 1000, math.MaxInt64}
	var tuples []tuple
	for i := 0; i < 400; i++ {
		s := alphabet[r.Intn(len(alphabet))]
		if s == "\xff" { // not valid UTF-8 for STRING
			s = "z"
		}
		tuples = append(tuples, tuple{
			s: s + alphabet[r.Intn(4)],
			n: ints[r.Intn(len(ints))],
			b: []byte(alphabet[r.Intn(len(alphabet))] + alphabet[r.Intn(len(alphabet))]),
		})
	}
	encode := func(tp tuple) []byte {
		k, err := EncodeKey(schema, []*btpb.Value{strV(tp.s), intV(tp.n), {Kind: &btpb.Value_BytesValue{BytesValue: tp.b}}})
		require.NoError(t, err)
		dec, err := DecodeKey(schema, k)
		require.NoError(t, err)
		require.Equal(t, tp.s, dec[0].GetStringValue())
		require.Equal(t, tp.n, dec[1].GetIntValue())
		require.Equal(t, string(tp.b), string(dec[2].GetBytesValue()))
		return k
	}
	cmpTuple := func(a, b tuple) int {
		if c := bytes.Compare([]byte(a.s), []byte(b.s)); c != 0 {
			return c
		}
		if a.n != b.n {
			if a.n < b.n {
				return -1
			}
			return 1
		}
		return bytes.Compare(a.b, b.b)
	}
	for i := 0; i < len(tuples); i++ {
		for j := 0; j < len(tuples); j += 7 {
			ka, kb := encode(tuples[i]), encode(tuples[j])
			require.Equal(t, cmpTuple(tuples[i], tuples[j]), bytes.Compare(ka, kb), "%#v vs %#v", tuples[i], tuples[j])
		}
	}
}

func TestDelimitedAndSingletonKeys(t *testing.T) {
	schema := structSchema(hashEnc, keyFieldProto(typString), keyFieldProto(typBytes), beInt)
	key, err := EncodeKey(schema, []*btpb.Value{strV("a"), bytesV("b"), intV(0x23)})
	require.NoError(t, err)
	require.Equal(t, []byte("a#b#\x00\x00\x00\x00\x00\x00\x00\x23"), key)
	dec, err := DecodeKey(schema, key)
	require.NoError(t, err, "fixed-width fields may contain the delimiter byte")
	require.Equal(t, "a", dec[0].GetStringValue())
	require.Equal(t, []byte("b"), dec[1].GetBytesValue())
	require.Equal(t, int64(0x23), dec[2].GetIntValue())

	_, err = EncodeKey(schema, []*btpb.Value{strV("a#x"), bytesV("b"), intV(1)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	for _, bad := range []string{"a#b", "a#b#\x00", "a#b#\x00\x00\x00\x00\x00\x00\x00\x01#", "ab"} {
		_, err = DecodeKey(schema, []byte(bad))
		require.Error(t, err, "%q", bad)
	}
	// Ordered-code INT64 inside delimited keys is self-delimiting.
	s2 := structSchema(hashEnc, keyFieldProto(typInt64), keyFieldProto(typString))
	k2, err := EncodeKey(s2, []*btpb.Value{intV(-1000), strV("tail")})
	require.NoError(t, err)
	d2, err := DecodeKey(s2, k2)
	require.NoError(t, err)
	require.Equal(t, int64(-1000), d2[0].GetIntValue())
	require.Equal(t, "tail", d2[1].GetStringValue())

	single := structSchema(singleEnc, beInt)
	k3, err := EncodeKey(single, []*btpb.Value{intV(-2)})
	require.NoError(t, err)
	require.Equal(t, []byte("\xff\xff\xff\xff\xff\xff\xff\xfe"), k3)
	d3, err := DecodeKey(single, k3)
	require.NoError(t, err)
	require.Equal(t, int64(-2), d3[0].GetIntValue())

	// Timestamp fields use unix_micros_int64.
	tsSchema := structSchema(orderedEnc, keyFieldProto(typTimestamp), keyFieldProto(typString))
	when := time.Date(2024, 5, 1, 12, 0, 0, 123000, time.UTC)
	k4, err := EncodeKey(tsSchema, []*btpb.Value{{Kind: &btpb.Value_TimestampValue{TimestampValue: timestamppb.New(when)}}, strV("x")})
	require.NoError(t, err)
	d4, err := DecodeKey(tsSchema, k4)
	require.NoError(t, err)
	require.True(t, when.Equal(d4[0].GetTimestampValue().AsTime()))
}

func TestKeyNullEscapes(t *testing.T) {
	esc := &btpb.Type{Kind: &btpb.Type_BytesType{BytesType: &btpb.Type_Bytes{Encoding: &btpb.Type_Bytes_Encoding{
		Encoding: &btpb.Type_Bytes_Encoding_Raw_{Raw: &btpb.Type_Bytes_Encoding_Raw{EscapeNulls: true}}}}}}
	schema := structSchema(singleEnc, esc)
	for _, c := range []struct {
		in   *btpb.Value
		want string
	}{
		{&btpb.Value{}, ""},
		{bytesV(""), "\x00"},
		{bytesV("\x00"), "\x00\x00"},
		{bytesV("a"), "a"},
	} {
		k, err := EncodeKey(schema, []*btpb.Value{c.in})
		require.NoError(t, err)
		require.Equal(t, []byte(c.want), k)
		d, err := DecodeKey(schema, k)
		require.NoError(t, err)
		require.Equal(t, c.in.GetKind() == nil, d[0].GetKind() == nil)
		require.Equal(t, c.in.GetBytesValue(), d[0].GetBytesValue())
	}
	strEsc := &btpb.Type{Kind: &btpb.Type_StringType{StringType: &btpb.Type_String{Encoding: &btpb.Type_String_Encoding{
		Encoding: &btpb.Type_String_Encoding_Utf8Bytes_{Utf8Bytes: &btpb.Type_String_Encoding_Utf8Bytes{NullEscapeChar: "\x00"}}}}}}
	sSchema := structSchema(singleEnc, strEsc)
	k, err := EncodeKey(sSchema, []*btpb.Value{strV("")})
	require.NoError(t, err)
	require.Equal(t, []byte("\x00"), k)
	d, err := DecodeKey(sSchema, []byte(""))
	require.NoError(t, err)
	require.Nil(t, d[0].GetKind())

	// NULLs are rejected without escapes.
	_, err = EncodeKey(structSchema(orderedEnc, keyFieldProto(typString)), []*btpb.Value{{}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestInvalidKeySchemas(t *testing.T) {
	for _, s := range []*btpb.Type_Struct{
		nil,
		structSchema(nil, keyFieldProto(typString)),
		structSchema(singleEnc, keyFieldProto(typString), keyFieldProto(typString)),
		structSchema(orderedEnc, typFloat64.toProto()),
		structSchema(&btpb.Type_Struct_Encoding{Encoding: &btpb.Type_Struct_Encoding_DelimitedBytes_{DelimitedBytes: &btpb.Type_Struct_Encoding_DelimitedBytes{}}}, keyFieldProto(typString)),
	} {
		_, err := EncodeKey(s, []*btpb.Value{strV("a")})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		_, err = DecodeKey(s, []byte("a"))
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	_, err := EncodeKey(structSchema(orderedEnc, keyFieldProto(typString)), []*btpb.Value{intV(1)})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "value kind must match the field type")
	_, err = EncodeKey(structSchema(orderedEnc, keyFieldProto(typString)), nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = DecodeKey(structSchema(orderedEnc, keyFieldProto(typString)), []byte("a\x00\x01b"))
	require.Error(t, err, "too many fields")
	_, err = DecodeKey(structSchema(orderedEnc, keyFieldProto(typString), keyFieldProto(typString)), []byte("a\x00\x05"))
	require.Error(t, err, "bad escape")
}

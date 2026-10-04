package gsql

import (
	"math"
	"testing"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// checkNoEncodings asserts that a metadata type carries no encodings and
// only uses kinds understood by the Go client's pbTypeToSQLType.
func checkNoEncodings(t *testing.T, ty *btpb.Type) {
	t.Helper()
	switch k := ty.GetKind().(type) {
	case *btpb.Type_BytesType:
		require.Nil(t, k.BytesType.GetEncoding())
	case *btpb.Type_StringType:
		require.Nil(t, k.StringType.GetEncoding())
	case *btpb.Type_Int64Type:
		require.Nil(t, k.Int64Type.GetEncoding())
	case *btpb.Type_TimestampType:
		require.Nil(t, k.TimestampType.GetEncoding())
	case *btpb.Type_Float32Type, *btpb.Type_Float64Type, *btpb.Type_BoolType, *btpb.Type_DateType:
	case *btpb.Type_ArrayType:
		checkNoEncodings(t, k.ArrayType.GetElementType())
	case *btpb.Type_MapType:
		checkNoEncodings(t, k.MapType.GetKeyType())
		checkNoEncodings(t, k.MapType.GetValueType())
	case *btpb.Type_StructType:
		require.Nil(t, k.StructType.GetEncoding())
		for _, f := range k.StructType.GetFields() {
			checkNoEncodings(t, f.GetType())
		}
	default:
		t.Fatalf("unexpected type kind %T", k)
	}
}

func TestWireValueShapes(t *testing.T) {
	cat, _ := stdCatalog()
	p, rows, err := execQuery(cat, "SELECT _key, 's', 1, 1.5, CAST(2.5 AS FLOAT32), TRUE, TIMESTAMP '2020-01-02 03:04:05.123456+00', "+
		"DATE '2020-01-02', [1, NULL], ARRAY<STRING>[], STRUCT(1 AS a, 'x' AS b), cf1, NULL, CAST(NULL AS STRING), cf2 "+
		"FROM t WHERE _key = 'a#01'", qopt{})
	require.NoError(t, err)
	for _, c := range p.Metadata().GetProtoSchema().GetColumns() {
		checkNoEncodings(t, c.GetType())
	}
	r := rows[0]
	require.Equal(t, []byte("a#01"), r[0].GetBytesValue())
	require.Equal(t, "s", r[1].GetStringValue())
	require.Equal(t, int64(1), r[2].GetIntValue())
	require.Equal(t, 1.5, r[3].GetFloatValue())
	require.Equal(t, 2.5, r[4].GetFloatValue())
	require.NotNil(t, p.Metadata().GetProtoSchema().GetColumns()[4].GetType().GetFloat32Type())
	require.Equal(t, true, r[5].GetBoolValue())
	require.Equal(t, int64(1577934245), r[6].GetTimestampValue().GetSeconds())
	require.Equal(t, int32(123456000), r[6].GetTimestampValue().GetNanos())
	require.Equal(t, int32(2020), r[7].GetDateValue().GetYear())
	require.Equal(t, int32(1), r[7].GetDateValue().GetMonth())
	require.Equal(t, int32(2), r[7].GetDateValue().GetDay())
	arr := r[8].GetArrayValue().GetValues()
	require.Len(t, arr, 2)
	require.Equal(t, int64(1), arr[0].GetIntValue())
	require.Nil(t, arr[1].GetKind(), "NULL element has no kind")
	require.Nil(t, arr[0].GetType(), "nested values never carry a type")
	require.NotNil(t, r[9].GetArrayValue(), "empty arrays are non-nil ArrayValues")
	require.Empty(t, r[9].GetArrayValue().GetValues())
	st := r[10].GetArrayValue().GetValues()
	require.Equal(t, int64(1), st[0].GetIntValue())
	require.Equal(t, "x", st[1].GetStringValue())
	stType := p.Metadata().GetProtoSchema().GetColumns()[10].GetType().GetStructType()
	require.Equal(t, "a", stType.GetFields()[0].GetFieldName())
	m := r[11].GetArrayValue().GetValues()
	require.Len(t, m, 2)
	require.Equal(t, []byte("c1"), m[0].GetArrayValue().GetValues()[0].GetBytesValue())
	require.Equal(t, []byte("xyz"), m[0].GetArrayValue().GetValues()[1].GetBytesValue())
	require.Equal(t, []byte("c2"), m[1].GetArrayValue().GetValues()[0].GetBytesValue())
	require.Nil(t, r[12].GetKind())
	require.Nil(t, r[13].GetKind())
	require.NotNil(t, p.Metadata().GetProtoSchema().GetColumns()[13].GetType().GetStringType())
	require.Equal(t, []byte("5"), r[14].GetArrayValue().GetValues()[0].GetArrayValue().GetValues()[1].GetBytesValue())
	for _, v := range r {
		require.Nil(t, v.GetType())
	}

	// with_history metadata: MAP<BYTES, ARRAY<STRUCT<timestamp TIMESTAMP, value BYTES>>>.
	ph := prep(t, cat, "SELECT cf1 FROM t(with_history => TRUE)")
	mt := ph.Metadata().GetProtoSchema().GetColumns()[0].GetType().GetMapType()
	require.NotNil(t, mt.GetKeyType().GetBytesType())
	fs := mt.GetValueType().GetArrayType().GetElementType().GetStructType().GetFields()
	require.Equal(t, "timestamp", fs[0].GetFieldName())
	require.NotNil(t, fs[0].GetType().GetTimestampType())
	require.Equal(t, "value", fs[1].GetFieldName())
	require.NotNil(t, fs[1].GetType().GetBytesType())
	checkNoEncodings(t, ph.Metadata().GetProtoSchema().GetColumns()[0].GetType())

	// Metadata is a defensive copy.
	md := p.Metadata()
	md.GetProtoSchema().Columns[0].Name = "mutated"
	require.Equal(t, "_key", p.Metadata().GetProtoSchema().GetColumns()[0].GetName())

	// Negative timestamps use floor semantics for seconds/nanos.
	_, rows, err = execQuery(cat, "SELECT TIMESTAMP_MICROS(-1)", qopt{})
	require.NoError(t, err)
	require.Equal(t, int64(-1), rows[0][0].GetTimestampValue().GetSeconds())
	require.Equal(t, int32(999999000), rows[0][0].GetTimestampValue().GetNanos())
}

func TestEncodeCellValue(t *testing.T) {
	for _, c := range []struct {
		typ  *sqlType
		val  *btpb.Value
		want []byte
	}{
		{typBool, &btpb.Value{Kind: &btpb.Value_BoolValue{BoolValue: true}}, []byte{1}},
		{typBool, &btpb.Value{Kind: &btpb.Value_BoolValue{BoolValue: false}}, []byte{0}},
		{typInt64, intV(-2), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe}},
		{typFloat64, &btpb.Value{Kind: &btpb.Value_FloatValue{FloatValue: 1.5}}, []byte{0x3f, 0xf8, 0, 0, 0, 0, 0, 0}},
		{typFloat32, &btpb.Value{Kind: &btpb.Value_FloatValue{FloatValue: 1}}, []byte{0x3f, 0x80, 0, 0}},
		{typString, strV("hé"), []byte("hé")},
		{typString, strV(""), []byte{}},
		{typBytes, bytesV("\x00b"), []byte("\x00b")},
		{typTimestamp, &btpb.Value{Kind: &btpb.Value_TimestampValue{TimestampValue: toProtoValue(tsVal(1500), typTimestamp).GetTimestampValue()}}, []byte{0, 0, 0, 0, 0, 0, 0x05, 0xdc}},
		{typDate, toProtoValue(dateVal(1), typDate), []byte{0, 0, 0, 0, 0, 0, 0, 1}},
	} {
		got, err := EncodeCellValue(c.typ.toProto(), c.val)
		require.NoError(t, err, c.typ.String())
		require.NotNil(t, got)
		require.Equal(t, c.want, got, c.typ.String())
		back, err := DecodeCellValue(c.typ.toProto(), got)
		require.NoError(t, err)
		require.True(t, proto.Equal(c.val, back), "%s: %v vs %v", c.typ, c.val, back)
	}
	got, err := EncodeCellValue(typInt64.toProto(), &btpb.Value{})
	require.NoError(t, err)
	require.Nil(t, got, "NULL encodes to nil")
	_, err = EncodeCellValue(mapOf(typBytes, typBytes).toProto(), &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{}}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = EncodeCellValue(typInt64.toProto(), strV("x"))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = DecodeCellValue(typInt64.toProto(), []byte{1})
	require.Error(t, err)
	nan, err := EncodeCellValue(typFloat64.toProto(), &btpb.Value{Kind: &btpb.Value_FloatValue{FloatValue: math.Inf(1)}})
	require.NoError(t, err)
	require.Len(t, nan, 8)
}

func TestTypeConversions(t *testing.T) {
	for _, ty := range []*sqlType{
		typBool, typInt64, typFloat32, typFloat64, typString, typBytes, typTimestamp, typDate,
		arrayOf(typString), mapOf(typBytes, arrayOf(historyCellType(typInt64))),
		structOf(structField{"a", typInt64}, structField{"", typString}),
	} {
		back, err := typeFromProto(ty.toProto())
		require.NoError(t, err)
		require.Equal(t, ty.String(), back.String())
	}
	_, err := typeFromProto(&btpb.Type{Kind: &btpb.Type_ProtoType{ProtoType: &btpb.Type_Proto{}}})
	require.Equal(t, codes.Unimplemented, status.Code(err))
	_, err = typeFromProto(arrayOf(arrayOf(typInt64)).toProto())
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

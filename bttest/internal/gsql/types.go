package gsql

import (
	"strings"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// typeKind enumerates the SQL types supported by the engine.
type typeKind uint8

const (
	tkInvalid typeKind = iota
	tkBool
	tkInt64
	tkFloat32
	tkFloat64
	tkString
	tkBytes
	tkTimestamp
	tkDate
	tkArray
	tkStruct
	tkMap
)

// sqlType is the engine's internal type representation.
type sqlType struct {
	kind   typeKind
	elem   *sqlType // ARRAY element
	key    *sqlType // MAP key
	val    *sqlType // MAP value
	fields []structField
}

type structField struct {
	name string
	typ  *sqlType
}

var (
	typBool      = &sqlType{kind: tkBool}
	typInt64     = &sqlType{kind: tkInt64}
	typFloat32   = &sqlType{kind: tkFloat32}
	typFloat64   = &sqlType{kind: tkFloat64}
	typString    = &sqlType{kind: tkString}
	typBytes     = &sqlType{kind: tkBytes}
	typTimestamp = &sqlType{kind: tkTimestamp}
	typDate      = &sqlType{kind: tkDate}
)

func arrayOf(t *sqlType) *sqlType        { return &sqlType{kind: tkArray, elem: t} }
func mapOf(k, v *sqlType) *sqlType       { return &sqlType{kind: tkMap, key: k, val: v} }
func structOf(f ...structField) *sqlType { return &sqlType{kind: tkStruct, fields: f} }

// historyCellType is the element type of with_history family columns.
func historyCellType(v *sqlType) *sqlType {
	return structOf(structField{"timestamp", typTimestamp}, structField{"value", v})
}

func (t *sqlType) String() string {
	if t == nil {
		return "<nil>"
	}
	switch t.kind {
	case tkBool:
		return "BOOL"
	case tkInt64:
		return "INT64"
	case tkFloat32:
		return "FLOAT32"
	case tkFloat64:
		return "FLOAT64"
	case tkString:
		return "STRING"
	case tkBytes:
		return "BYTES"
	case tkTimestamp:
		return "TIMESTAMP"
	case tkDate:
		return "DATE"
	case tkArray:
		return "ARRAY<" + t.elem.String() + ">"
	case tkMap:
		return "MAP<" + t.key.String() + ", " + t.val.String() + ">"
	case tkStruct:
		var sb strings.Builder
		sb.WriteString("STRUCT<")
		for i, f := range t.fields {
			if i > 0 {
				sb.WriteString(", ")
			}
			if f.name != "" {
				sb.WriteString(f.name)
				sb.WriteByte(' ')
			}
			sb.WriteString(f.typ.String())
		}
		sb.WriteString(">")
		return sb.String()
	}
	return "INVALID"
}

// typesEqual compares types structurally, ignoring struct field names.
func typesEqual(a, b *sqlType) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil || a.kind != b.kind {
		return false
	}
	switch a.kind {
	case tkArray:
		return typesEqual(a.elem, b.elem)
	case tkMap:
		return typesEqual(a.key, b.key) && typesEqual(a.val, b.val)
	case tkStruct:
		if len(a.fields) != len(b.fields) {
			return false
		}
		for i := range a.fields {
			if !typesEqual(a.fields[i].typ, b.fields[i].typ) {
				return false
			}
		}
	}
	return true
}

func (t *sqlType) isNumeric() bool {
	return t.kind == tkInt64 || t.kind == tkFloat32 || t.kind == tkFloat64
}

func (t *sqlType) isFloat() bool { return t.kind == tkFloat32 || t.kind == tkFloat64 }

// orderable: everything except STRUCT and MAP (arrays if element orderable).
func (t *sqlType) orderable() bool {
	switch t.kind {
	case tkStruct, tkMap:
		return false
	case tkArray:
		return t.elem.orderable()
	}
	return true
}

// groupable: everything except ARRAY and STRUCT (per the Bigtable data type docs).
func (t *sqlType) groupable() bool {
	return t.kind != tkArray && t.kind != tkStruct
}

// comparable for equality: everything except ARRAY.
func (t *sqlType) equalityComparable() bool {
	switch t.kind {
	case tkArray:
		return false
	case tkStruct:
		for _, f := range t.fields {
			if !f.typ.equalityComparable() {
				return false
			}
		}
	case tkMap:
		return t.val.equalityComparable()
	}
	return true
}

// toProto converts the type to the wire representation without encodings.
func (t *sqlType) toProto() *btpb.Type {
	switch t.kind {
	case tkBool:
		return &btpb.Type{Kind: &btpb.Type_BoolType{BoolType: &btpb.Type_Bool{}}}
	case tkInt64:
		return &btpb.Type{Kind: &btpb.Type_Int64Type{Int64Type: &btpb.Type_Int64{}}}
	case tkFloat32:
		return &btpb.Type{Kind: &btpb.Type_Float32Type{Float32Type: &btpb.Type_Float32{}}}
	case tkFloat64:
		return &btpb.Type{Kind: &btpb.Type_Float64Type{Float64Type: &btpb.Type_Float64{}}}
	case tkString:
		return &btpb.Type{Kind: &btpb.Type_StringType{StringType: &btpb.Type_String{}}}
	case tkBytes:
		return &btpb.Type{Kind: &btpb.Type_BytesType{BytesType: &btpb.Type_Bytes{}}}
	case tkTimestamp:
		return &btpb.Type{Kind: &btpb.Type_TimestampType{TimestampType: &btpb.Type_Timestamp{}}}
	case tkDate:
		return &btpb.Type{Kind: &btpb.Type_DateType{DateType: &btpb.Type_Date{}}}
	case tkArray:
		return &btpb.Type{Kind: &btpb.Type_ArrayType{ArrayType: &btpb.Type_Array{ElementType: t.elem.toProto()}}}
	case tkMap:
		return &btpb.Type{Kind: &btpb.Type_MapType{MapType: &btpb.Type_Map{KeyType: t.key.toProto(), ValueType: t.val.toProto()}}}
	case tkStruct:
		fs := make([]*btpb.Type_Struct_Field, len(t.fields))
		for i, f := range t.fields {
			fs[i] = &btpb.Type_Struct_Field{FieldName: f.name, Type: f.typ.toProto()}
		}
		return &btpb.Type{Kind: &btpb.Type_StructType{StructType: &btpb.Type_Struct{Fields: fs}}}
	}
	return &btpb.Type{}
}

// typeFromProto converts a wire type (encodings are ignored) to an internal type.
func typeFromProto(pt *btpb.Type) (*sqlType, error) {
	if pt == nil {
		return nil, invalidf("type is not specified")
	}
	switch k := pt.Kind.(type) {
	case *btpb.Type_BoolType:
		return typBool, nil
	case *btpb.Type_Int64Type:
		return typInt64, nil
	case *btpb.Type_Float32Type:
		return typFloat32, nil
	case *btpb.Type_Float64Type:
		return typFloat64, nil
	case *btpb.Type_StringType:
		return typString, nil
	case *btpb.Type_BytesType:
		return typBytes, nil
	case *btpb.Type_TimestampType:
		return typTimestamp, nil
	case *btpb.Type_DateType:
		return typDate, nil
	case *btpb.Type_ArrayType:
		e, err := typeFromProto(k.ArrayType.GetElementType())
		if err != nil {
			return nil, err
		}
		if e.kind == tkArray {
			return nil, invalidf("arrays of arrays are not supported")
		}
		return arrayOf(e), nil
	case *btpb.Type_MapType:
		kt, err := typeFromProto(k.MapType.GetKeyType())
		if err != nil {
			return nil, err
		}
		vt, err := typeFromProto(k.MapType.GetValueType())
		if err != nil {
			return nil, err
		}
		return mapOf(kt, vt), nil
	case *btpb.Type_StructType:
		fs := make([]structField, len(k.StructType.GetFields()))
		for i, f := range k.StructType.GetFields() {
			ft, err := typeFromProto(f.GetType())
			if err != nil {
				return nil, err
			}
			fs[i] = structField{name: f.GetFieldName(), typ: ft}
		}
		return structOf(fs...), nil
	case *btpb.Type_ProtoType, *btpb.Type_EnumType:
		return nil, unimplementedf("PROTO and ENUM types are not supported by the emulator")
	case *btpb.Type_AggregateType:
		return nil, invalidf("aggregate types cannot be used as SQL types")
	}
	return nil, invalidf("unsupported or unset type %v", pt)
}

package gsql

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/bits"
	"strings"
	"unicode/utf8"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// EncodeKey encodes one value per schema field into a row key, following
// the encodings defined in google/bigtable/v2/types.proto:
//
//   - Struct: Singleton, DelimitedBytes and OrderedCodeBytes.
//   - Bytes: Raw (with optional escape_nulls).
//   - String: Utf8Bytes / Utf8Raw (with optional null_escape_char).
//   - Int64: BigEndianBytes (the default) and OrderedCodeBytes.
//   - Timestamp: UnixMicrosInt64 with either Int64 encoding (default
//     BigEndianBytes).
func EncodeKey(schema *btpb.Type_Struct, values []*btpb.Value) ([]byte, error) {
	types, err := keySchemaTypes(schema)
	if err != nil {
		return nil, err
	}
	if len(values) != len(types) {
		return nil, invalidf("row key schema has %d fields but %d values were provided", len(types), len(values))
	}
	vals := make([]any, len(values))
	for i, v := range values {
		x, err := fromProtoValue(v, types[i], false)
		if err != nil {
			return nil, err
		}
		vals[i] = x
	}
	return encodeKeyInternal(schema, vals)
}

// DecodeKey decodes a row key into one value per schema field.
func DecodeKey(schema *btpb.Type_Struct, key []byte) ([]*btpb.Value, error) {
	types, err := keySchemaTypes(schema)
	if err != nil {
		return nil, err
	}
	vals, err := decodeKeyInternal(schema, key)
	if err != nil {
		return nil, err
	}
	out := make([]*btpb.Value, len(vals))
	for i, v := range vals {
		out[i] = toProtoValue(v, types[i])
	}
	return out, nil
}

// keySchemaTypes validates a row key schema and returns the SQL field types.
func keySchemaTypes(schema *btpb.Type_Struct) ([]*sqlType, error) {
	if schema == nil {
		return nil, invalidf("row key schema is not set")
	}
	fields := schema.GetFields()
	types := make([]*sqlType, len(fields))
	for i, f := range fields {
		switch f.GetType().GetKind().(type) {
		case *btpb.Type_BytesType:
			types[i] = typBytes
		case *btpb.Type_StringType:
			types[i] = typString
		case *btpb.Type_Int64Type:
			types[i] = typInt64
		case *btpb.Type_TimestampType:
			types[i] = typTimestamp
		default:
			return nil, invalidf("row key schema field %q has unsupported type %v", f.GetFieldName(), f.GetType())
		}
	}
	switch enc := schema.GetEncoding().GetEncoding().(type) {
	case *btpb.Type_Struct_Encoding_Singleton_:
		if len(fields) != 1 {
			return nil, invalidf("singleton row key encoding requires exactly one field, got %d", len(fields))
		}
	case *btpb.Type_Struct_Encoding_DelimitedBytes_:
		d := enc.DelimitedBytes.GetDelimiter()
		if len(d) == 0 || len(d) > 50 {
			return nil, invalidf("row key delimiter must contain 1 to 50 bytes")
		}
	case *btpb.Type_Struct_Encoding_OrderedCodeBytes_:
	default:
		return nil, invalidf("row key schema must specify a struct encoding")
	}
	return types, nil
}

func encodeKeyInternal(schema *btpb.Type_Struct, vals []any) ([]byte, error) {
	fields := schema.GetFields()
	encs := make([][]byte, len(fields))
	for i, f := range fields {
		b, err := encodeKeyField(f.GetType(), vals[i], f.GetFieldName())
		if err != nil {
			return nil, err
		}
		encs[i] = b
	}
	switch enc := schema.GetEncoding().GetEncoding().(type) {
	case *btpb.Type_Struct_Encoding_Singleton_:
		return encs[0], nil
	case *btpb.Type_Struct_Encoding_DelimitedBytes_:
		d := enc.DelimitedBytes.GetDelimiter()
		if len(encs) == 0 {
			return append([]byte(nil), d...), nil
		}
		for i, f := range fields {
			if fixedKeyWidth(f.GetType()) == 0 && bytes.Contains(encs[i], d) {
				return nil, invalidf("row key field %q value contains the delimiter %q", f.GetFieldName(), d)
			}
		}
		return bytes.Join(encs, d), nil
	case *btpb.Type_Struct_Encoding_OrderedCodeBytes_:
		return orderedCodeJoin(encs), nil
	}
	return nil, invalidf("row key schema must specify a struct encoding")
}

func orderedCodeJoin(encs [][]byte) []byte {
	last := -1
	for i, e := range encs {
		if len(e) > 0 {
			last = i
		}
	}
	if last < 0 {
		return []byte{0, 0}
	}
	var out []byte
	for i := 0; i <= last; i++ {
		if i > 0 {
			out = append(out, 0, 1)
		}
		if len(encs[i]) == 0 {
			out = append(out, 0, 0)
			continue
		}
		for _, c := range encs[i] {
			if c == 0 {
				out = append(out, 0, 0xFF)
			} else {
				out = append(out, c)
			}
		}
	}
	return out
}

func orderedCodeSplit(key []byte, n int) ([][]byte, error) {
	if len(key) == 0 {
		return make([][]byte, n), nil
	}
	var segs [][]byte
	cur := []byte{}
	for i := 0; i < len(key); {
		c := key[i]
		if c != 0 {
			cur = append(cur, c)
			i++
			continue
		}
		if i+1 >= len(key) {
			return nil, invalidf("invalid ordered-code row key: dangling escape byte")
		}
		switch key[i+1] {
		case 0xFF:
			cur = append(cur, 0)
			i += 2
		case 0x01:
			segs = append(segs, cur)
			cur = []byte{}
			i += 2
		case 0x00:
			if len(cur) != 0 {
				return nil, invalidf("invalid ordered-code row key: misplaced empty-field marker")
			}
			i += 2
			if i < len(key) && !(i+1 < len(key) && key[i] == 0 && key[i+1] == 1) {
				return nil, invalidf("invalid ordered-code row key: empty-field marker not followed by a separator")
			}
		default:
			return nil, invalidf("invalid ordered-code row key: bad escape sequence 0x00 0x%02x", key[i+1])
		}
	}
	segs = append(segs, cur)
	if len(segs) > n {
		return nil, invalidf("ordered-code row key has %d fields, schema has %d", len(segs), n)
	}
	for len(segs) < n {
		segs = append(segs, []byte{})
	}
	return segs, nil
}

// fixedKeyWidth returns the encoded width of fixed-size fields, -1 for
// self-delimiting fields and 0 for variable-length fields.
func fixedKeyWidth(t *btpb.Type) int {
	var ie *btpb.Type_Int64_Encoding
	switch k := t.GetKind().(type) {
	case *btpb.Type_Int64Type:
		ie = k.Int64Type.GetEncoding()
	case *btpb.Type_TimestampType:
		ie = k.TimestampType.GetEncoding().GetUnixMicrosInt64()
	default:
		return 0
	}
	if ie.GetOrderedCodeBytes() != nil {
		return -1
	}
	return 8
}

func decodeKeyInternal(schema *btpb.Type_Struct, key []byte) ([]any, error) {
	fields := schema.GetFields()
	out := make([]any, len(fields))
	switch enc := schema.GetEncoding().GetEncoding().(type) {
	case *btpb.Type_Struct_Encoding_Singleton_:
		v, err := decodeKeyField(fields[0].GetType(), key)
		if err != nil {
			return nil, err
		}
		out[0] = v
		return out, nil
	case *btpb.Type_Struct_Encoding_DelimitedBytes_:
		d := enc.DelimitedBytes.GetDelimiter()
		rest := key
		for i, f := range fields {
			if i > 0 {
				if !bytes.HasPrefix(rest, d) {
					return nil, invalidf("row key %q does not conform to the row key schema: missing delimiter before field %q", key, f.GetFieldName())
				}
				rest = rest[len(d):]
			}
			var fb []byte
			switch w := fixedKeyWidth(f.GetType()); {
			case w > 0:
				if len(rest) < w {
					return nil, invalidf("row key %q does not conform to the row key schema: field %q is truncated", key, f.GetFieldName())
				}
				fb, rest = rest[:w], rest[w:]
			case w < 0:
				_, n, err := decodeOrderedInt64(rest)
				if err != nil {
					return nil, invalidf("row key %q does not conform to the row key schema: field %q: %v", key, f.GetFieldName(), err)
				}
				fb, rest = rest[:n], rest[n:]
			default:
				if i == len(fields)-1 {
					if bytes.Contains(rest, d) {
						return nil, invalidf("row key %q does not conform to the row key schema: too many fields", key)
					}
					fb, rest = rest, nil
				} else {
					j := bytes.Index(rest, d)
					if j < 0 {
						return nil, invalidf("row key %q does not conform to the row key schema: expected %d fields", key, len(fields))
					}
					fb, rest = rest[:j], rest[j:]
				}
			}
			v, err := decodeKeyField(f.GetType(), fb)
			if err != nil {
				return nil, invalidf("row key %q does not conform to the row key schema: field %q: %v", key, f.GetFieldName(), err)
			}
			out[i] = v
		}
		if len(rest) != 0 {
			return nil, invalidf("row key %q does not conform to the row key schema: trailing bytes", key)
		}
		return out, nil
	case *btpb.Type_Struct_Encoding_OrderedCodeBytes_:
		segs, err := orderedCodeSplit(key, len(fields))
		if err != nil {
			return nil, err
		}
		for i, f := range fields {
			v, err := decodeKeyField(f.GetType(), segs[i])
			if err != nil {
				return nil, invalidf("row key %q does not conform to the row key schema: field %q: %v", key, f.GetFieldName(), err)
			}
			out[i] = v
		}
		return out, nil
	}
	return nil, invalidf("row key schema must specify a struct encoding")
}

func encodeKeyField(t *btpb.Type, v any, name string) ([]byte, error) {
	switch k := t.GetKind().(type) {
	case *btpb.Type_BytesType:
		escape := k.BytesType.GetEncoding().GetRaw().GetEscapeNulls()
		if v == nil {
			if escape {
				return []byte{}, nil
			}
			return nil, invalidf("row key field %q must not be NULL", name)
		}
		b := v.([]byte)
		if escape && allBytesEqual(b, 0) {
			return append(append([]byte(nil), b...), 0), nil
		}
		return append([]byte(nil), b...), nil
	case *btpb.Type_StringType:
		esc := k.StringType.GetEncoding().GetUtf8Bytes().GetNullEscapeChar()
		if v == nil {
			if esc != "" {
				return []byte{}, nil
			}
			return nil, invalidf("row key field %q must not be NULL", name)
		}
		s := v.(string)
		if esc != "" && strings.Trim(s, esc) == "" {
			s += esc
		}
		return []byte(s), nil
	case *btpb.Type_Int64Type:
		if v == nil {
			return nil, invalidf("row key field %q must not be NULL", name)
		}
		return encodeInt64(k.Int64Type.GetEncoding(), v.(int64)), nil
	case *btpb.Type_TimestampType:
		if v == nil {
			return nil, invalidf("row key field %q must not be NULL", name)
		}
		return encodeInt64(k.TimestampType.GetEncoding().GetUnixMicrosInt64(), int64(v.(tsVal))), nil
	}
	return nil, invalidf("row key field %q has an unsupported type", name)
}

func decodeKeyField(t *btpb.Type, b []byte) (any, error) {
	switch k := t.GetKind().(type) {
	case *btpb.Type_BytesType:
		if k.BytesType.GetEncoding().GetRaw().GetEscapeNulls() {
			if len(b) == 0 {
				return nil, nil
			}
			if allBytesEqual(b, 0) {
				return append([]byte(nil), b[:len(b)-1]...), nil
			}
		}
		return append([]byte(nil), b...), nil
	case *btpb.Type_StringType:
		s := string(b)
		if esc := k.StringType.GetEncoding().GetUtf8Bytes().GetNullEscapeChar(); esc != "" {
			if s == "" {
				return nil, nil
			}
			if strings.Trim(s, esc) == "" {
				s = strings.TrimSuffix(s, esc)
			}
		}
		if !isValidUTF8(b) {
			return nil, invalidf("invalid UTF-8 in STRING key field")
		}
		return s, nil
	case *btpb.Type_Int64Type:
		return decodeInt64(k.Int64Type.GetEncoding(), b)
	case *btpb.Type_TimestampType:
		n, err := decodeInt64(k.TimestampType.GetEncoding().GetUnixMicrosInt64(), b)
		if err != nil {
			return nil, err
		}
		return checkTimestamp(n.(int64))
	}
	return nil, invalidf("unsupported key field type")
}

func allBytesEqual(b []byte, c byte) bool {
	for _, x := range b {
		if x != c {
			return false
		}
	}
	return true
}

func encodeInt64(enc *btpb.Type_Int64_Encoding, v int64) []byte {
	if enc.GetOrderedCodeBytes() != nil {
		return appendOrderedInt64(nil, v)
	}
	return binary.BigEndian.AppendUint64(nil, uint64(v))
}

func decodeInt64(enc *btpb.Type_Int64_Encoding, b []byte) (any, error) {
	if enc.GetOrderedCodeBytes() != nil {
		v, n, err := decodeOrderedInt64(b)
		if err != nil {
			return nil, err
		}
		if n != len(b) {
			return nil, invalidf("trailing bytes after ordered-code INT64")
		}
		return v, nil
	}
	if len(b) != 8 {
		return nil, invalidf("big-endian INT64 requires 8 bytes, got %d", len(b))
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

// appendOrderedInt64 implements the OrderedCode signed integer encoding
// ("WriteSignedNumIncreasing"): a variable-length (1..10 byte), order
// preserving encoding where values closer to zero use fewer bytes. The first
// n bits of an n-byte encoding of a non-negative value are 1s followed by a
// 0; negative values are encoded as the bitwise complement of the encoding of
// ^v.
func appendOrderedInt64(dst []byte, v int64) []byte {
	x := uint64(v)
	neg := v < 0
	if neg {
		x = ^x
	}
	n := 1
	for bits.Len64(x) > 7*n-1 {
		n++
	}
	var buf [10]byte
	for i := 0; i < 8; i++ {
		buf[9-i] = byte(x >> (8 * i))
	}
	enc := buf[10-n:]
	for i := 0; i < n; i++ {
		enc[i/8] |= 0x80 >> (i % 8)
	}
	if neg {
		for i := range enc {
			enc[i] = ^enc[i]
		}
	}
	return append(dst, enc...)
}

// decodeOrderedInt64 decodes an OrderedCode signed integer from the start of
// b and returns the value and the number of bytes consumed.
func decodeOrderedInt64(b []byte) (int64, int, error) {
	if len(b) == 0 {
		return 0, 0, invalidf("empty ordered-code INT64")
	}
	neg := b[0]&0x80 == 0
	get := func(i int) byte {
		if neg {
			return ^b[i]
		}
		return b[i]
	}
	n := 0
	for {
		if n/8 >= len(b) {
			return 0, 0, invalidf("truncated ordered-code INT64")
		}
		if get(n/8)&(0x80>>(n%8)) == 0 {
			break
		}
		n++
		if n > 10 {
			return 0, 0, invalidf("invalid ordered-code INT64 length")
		}
	}
	if n < 1 || len(b) < n {
		return 0, 0, invalidf("truncated ordered-code INT64")
	}
	enc := make([]byte, n)
	for i := 0; i < n; i++ {
		enc[i] = get(i)
	}
	for i := 0; i < n; i++ {
		enc[i/8] &^= 0x80 >> (i % 8)
	}
	for i := 0; i < n-8; i++ {
		if enc[i] != 0 {
			return 0, 0, invalidf("ordered-code INT64 overflow")
		}
	}
	var x uint64
	for i := max(0, n-8); i < n; i++ {
		x = x<<8 | uint64(enc[i])
	}
	if x > math.MaxInt64 {
		return 0, 0, invalidf("ordered-code INT64 overflow")
	}
	v := int64(x)
	if neg {
		v = ^v
	}
	if len(appendOrderedInt64(nil, v)) != n {
		return 0, 0, invalidf("non-canonical ordered-code INT64")
	}
	return v, n, nil
}

func isValidUTF8(b []byte) bool { return utf8.Valid(b) }

package gsql

import (
	"encoding/binary"
	"math"
	"unicode/utf8"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// EncodeCellValue encodes a typed value as a materialized-view cell value,
// following the encodings documented for continuous materialized views:
//
//	BOOL      1 byte (1 = true, 0 = false)
//	INT64     8-byte big-endian two's complement
//	FLOAT64   8-byte IEEE 754 big-endian
//	FLOAT32   4-byte IEEE 754 big-endian
//	STRING    UTF-8
//	BYTES     raw
//	TIMESTAMP INT64 microseconds since the Unix epoch, 8-byte big-endian
//	DATE      INT64 days since 1970-01-01, 8-byte big-endian (emulator choice:
//	          production views reject DATE output columns, so no format is
//	          documented)
//
// A NULL value encodes to a nil slice with a nil error (callers should not
// write a cell); non-NULL values always encode to a non-nil slice. ARRAY,
// STRUCT and MAP values cannot be encoded as a single cell.
func EncodeCellValue(t *btpb.Type, v *btpb.Value) ([]byte, error) {
	st, err := typeFromProto(t)
	if err != nil {
		return nil, err
	}
	x, err := fromProtoValue(v, st, false)
	if err != nil {
		return nil, err
	}
	return encodeCell(st, x)
}

func encodeCell(t *sqlType, v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	switch t.kind {
	case tkBool:
		if v.(bool) {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case tkInt64:
		return binary.BigEndian.AppendUint64(make([]byte, 0, 8), uint64(v.(int64))), nil
	case tkFloat64:
		return binary.BigEndian.AppendUint64(make([]byte, 0, 8), math.Float64bits(v.(float64))), nil
	case tkFloat32:
		return binary.BigEndian.AppendUint32(make([]byte, 0, 4), math.Float32bits(float32(v.(float64)))), nil
	case tkString:
		return append([]byte{}, v.(string)...), nil
	case tkBytes:
		return append([]byte{}, v.([]byte)...), nil
	case tkTimestamp:
		return binary.BigEndian.AppendUint64(make([]byte, 0, 8), uint64(v.(tsVal))), nil
	case tkDate:
		return binary.BigEndian.AppendUint64(make([]byte, 0, 8), uint64(int64(v.(dateVal)))), nil
	}
	return nil, invalidf("values of type %s cannot be encoded as a single cell", t)
}

// DecodeCellValue is the inverse of EncodeCellValue.
func DecodeCellValue(t *btpb.Type, b []byte) (*btpb.Value, error) {
	st, err := typeFromProto(t)
	if err != nil {
		return nil, err
	}
	v, err := decodeCell(st, b)
	if err != nil {
		return nil, err
	}
	return toProtoValue(v, st), nil
}

func decodeCell(t *sqlType, b []byte) (any, error) {
	need := func(n int) error {
		if len(b) != n {
			return invalidf("%s cell value must be %d bytes, got %d", t, n, len(b))
		}
		return nil
	}
	switch t.kind {
	case tkBool:
		if err := need(1); err != nil {
			return nil, err
		}
		return b[0] != 0, nil
	case tkInt64:
		if err := need(8); err != nil {
			return nil, err
		}
		return int64(binary.BigEndian.Uint64(b)), nil
	case tkFloat64:
		if err := need(8); err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	case tkFloat32:
		if err := need(4); err != nil {
			return nil, err
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	case tkString:
		if !utf8.Valid(b) {
			return nil, invalidf("STRING cell value is not valid UTF-8")
		}
		return string(b), nil
	case tkBytes:
		return append([]byte{}, b...), nil
	case tkTimestamp:
		if err := need(8); err != nil {
			return nil, err
		}
		return checkTimestamp(int64(binary.BigEndian.Uint64(b)))
	case tkDate:
		if err := need(8); err != nil {
			return nil, err
		}
		return checkDate(int64(binary.BigEndian.Uint64(b)))
	}
	return nil, invalidf("values of type %s cannot be decoded from a single cell", t)
}

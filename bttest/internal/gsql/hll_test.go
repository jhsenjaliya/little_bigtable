package gsql

import (
	"encoding/binary"
	"math"
	"strconv"
	"testing"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/require"
)

func hllWith(from, to int) *HLL {
	h := NewHLL()
	for i := from; i < to; i++ {
		h.Add([]byte("item-" + strconv.Itoa(i)))
	}
	return h
}

func TestHLLExactForSmallCardinalities(t *testing.T) {
	h := NewHLL()
	require.Equal(t, int64(0), h.Estimate())
	for i := 0; i < 1000; i++ {
		h.Add([]byte("item-" + strconv.Itoa(i)))
		h.Add([]byte("item-" + strconv.Itoa(i))) // duplicates do not count
	}
	require.Equal(t, int64(1000), h.Estimate())
	h.Add(nil)
	require.Equal(t, int64(1001), h.Estimate())
}

func TestHLLAccuracy(t *testing.T) {
	for _, n := range []int{3000, 20000, 100000} {
		est := float64(hllWith(0, n).Estimate())
		require.InDelta(t, float64(n), est, 0.02*float64(n), "n=%d est=%v", n, est)
	}
}

func TestHLLMerge(t *testing.T) {
	// sparse + sparse
	a, b := hllWith(0, 600), hllWith(300, 900)
	a.Merge(b)
	require.Equal(t, int64(900), a.Estimate())
	// dense + sparse, sparse + dense, dense + dense
	d1, s1 := hllWith(0, 50000), hllWith(49000, 50500)
	d1.Merge(s1)
	require.InDelta(t, 50500, float64(d1.Estimate()), 0.02*50500)
	s2, d2 := hllWith(0, 100), hllWith(0, 30000)
	s2.Merge(d2)
	require.InDelta(t, 30000, float64(s2.Estimate()), 0.02*30000)
	x, y := hllWith(0, 40000), hllWith(20000, 60000)
	x.Merge(y)
	require.InDelta(t, 60000, float64(x.Estimate()), 0.02*60000)
	x.Merge(nil)

	// Different precisions merge at the lower precision.
	lo := newHLLPrecision(12)
	for i := 0; i < 20000; i++ {
		lo.Add([]byte("item-" + strconv.Itoa(i)))
	}
	hi := hllWith(10000, 30000)
	hi.Merge(lo)
	require.Equal(t, uint8(12), hi.p)
	require.InDelta(t, 30000, float64(hi.Estimate()), 0.05*30000)
	lo2 := newHLLPrecision(12)
	lo2.Merge(hllWith(0, 40000))
	require.Equal(t, uint8(12), lo2.p)
	require.InDelta(t, 40000, float64(lo2.Estimate()), 0.05*40000)
}

func TestHLLEncoding(t *testing.T) {
	for _, n := range []int{0, 10, 2000, 5000, 50000} {
		h := hllWith(0, n)
		enc := h.Encode()
		require.Equal(t, byte(1), enc[0], "version byte")
		dec, err := DecodeHLL(enc)
		require.NoError(t, err)
		require.Equal(t, h.Estimate(), dec.Estimate())
		require.Equal(t, enc, dec.Encode(), "encoding is deterministic")
	}
	require.Equal(t, hllWith(0, 5000).Encode(), hllWith(0, 5000).Encode())
	_, err := DecodeHLL(nil)
	require.Error(t, err)
	_, err = DecodeHLL([]byte{})
	require.Error(t, err)
	for _, bad := range [][]byte{{2, 14, 0, 0}, {1, 40, 0, 0}, {1, 14, 3}, {1, 14, 1, 0}, {1, 14, 0, 5, 1}, append([]byte{1, 14, 0}, binary.AppendUvarint(nil, 1)...)} {
		_, err := DecodeHLL(bad)
		require.Error(t, err, "%v", bad)
	}
	// A sketch built here round trips through the SQL functions.
	cat := newMemCatalog()
	tb := cat.addTable(&Table{Name: "h", Families: []Family{{Name: "s", Kind: FamilyHLLAggregate}}})
	tb.set("r", "s", "q", 1, hllWith(0, 1234).Encode())
	require.Equal(t, []string{`1234`}, q(t, cat, "SELECT HLL_COUNT.EXTRACT(s['q']) FROM h"))
}

func TestHLLInputBytes(t *testing.T) {
	b, ok := HLLInputBytes(&btpb.Value{Kind: &btpb.Value_IntValue{IntValue: 1}})
	require.True(t, ok)
	require.Equal(t, []byte{0, 0, 0, 0, 0, 0, 0, 1}, b)
	b, ok = HLLInputBytes(&btpb.Value{Kind: &btpb.Value_StringValue{StringValue: "x"}})
	require.True(t, ok)
	require.Equal(t, []byte("x"), b)
	b, ok = HLLInputBytes(&btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte("r")}})
	require.True(t, ok)
	require.Equal(t, []byte("r"), b)
	_, ok = HLLInputBytes(&btpb.Value{})
	require.False(t, ok)
	_, ok = HLLInputBytes(nil)
	require.False(t, ok)

	// HLL_COUNT.INIT on INT64 hashes the same bytes as HLLInputBytes.
	h := NewHLL()
	for i := int64(0); i < 10; i++ {
		ib, _ := HLLInputBytes(&btpb.Value{Kind: &btpb.Value_IntValue{IntValue: i}})
		h.Add(ib)
	}
	require.Equal(t, int64(10), h.Estimate())
	require.False(t, math.IsNaN(hllSigma(0.5)))
}

package gsql

import (
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
	"sort"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// HLL is a HyperLogLog++ cardinality sketch shared by HLL aggregate column
// families and the HLL_COUNT.* SQL functions.
//
// IMPORTANT: the binary encoding produced by Encode is emulator-local. It is
// NOT byte-compatible with ZetaSketch / BigQuery / production Bigtable HLL++
// sketches and must not be exchanged with them. Sketches are only meaningful
// to this package.
//
// Design:
//   - Items are hashed with 64-bit FNV-1a followed by the MurmurHash3 fmix64
//     finalizer. The hash is unseeded, so results are deterministic across
//     processes.
//   - Small sketches use an exact sparse representation (the set of distinct
//     64-bit hashes), so estimates are exact up to 2^p/8 distinct items
//     (2048 at the default precision 14).
//   - Larger sketches use 2^p dense 8-bit registers and Ertl's improved raw
//     estimator ("New cardinality estimation algorithms for HyperLogLog
//     sketches", 2017), which is unbiased over the full range without the
//     empirical bias tables of the original HLL++ paper; the standard error
//     is about 1.04/sqrt(2^p) (0.8% at p=14).
//   - Merging sketches of different precisions downgrades to the lower one.
//
// Encoding (version 1):
//
//	byte 0: version (1)
//	byte 1: precision p
//	byte 2: representation (0 = sparse, 1 = dense)
//	sparse: uvarint count, then the sorted hashes as uvarint deltas
//	dense:  2^p register bytes
type HLL struct {
	p      uint8
	sparse map[uint64]struct{} // non-nil while sparse
	regs   []uint8             // dense registers
}

const (
	hllVersion          = 1
	hllDefaultPrecision = 14
	hllInitPrecision    = 15 // GoogleSQL default precision for HLL_COUNT.INIT
	hllMinPrecision     = 10
	hllMaxPrecision     = 24
)

// NewHLL returns an empty sketch with precision 14.
func NewHLL() *HLL { return newHLLPrecision(hllDefaultPrecision) }

func newHLLPrecision(p int) *HLL {
	return &HLL{p: uint8(p), sparse: map[uint64]struct{}{}}
}

func (h *HLL) sparseLimit() int { return (1 << h.p) / 8 }

func hllHash(b []byte) uint64 {
	x := uint64(14695981039346656037)
	for _, c := range b {
		x ^= uint64(c)
		x *= 1099511628211
	}
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

// Add adds an item to the sketch.
func (h *HLL) Add(item []byte) { h.addHash(hllHash(item)) }

func (h *HLL) addHash(x uint64) {
	if h.sparse != nil {
		h.sparse[x] = struct{}{}
		if len(h.sparse) > h.sparseLimit() {
			h.densify()
		}
		return
	}
	h.setRegister(x)
}

func (h *HLL) setRegister(x uint64) {
	p := uint(h.p)
	idx := x >> (64 - p)
	w := x<<p | 1<<(p-1)
	rho := uint8(bits.LeadingZeros64(w) + 1)
	if rho > h.regs[idx] {
		h.regs[idx] = rho
	}
}

func (h *HLL) densify() {
	h.regs = make([]uint8, 1<<h.p)
	for x := range h.sparse {
		h.setRegister(x)
	}
	h.sparse = nil
}

// downgrade reduces the precision of h to p.
func (h *HLL) downgrade(p uint8) {
	if p >= h.p {
		return
	}
	if h.sparse != nil {
		h.p = p
		if len(h.sparse) > h.sparseLimit() {
			h.densify()
		}
		return
	}
	d := uint(h.p - p)
	regs := make([]uint8, 1<<p)
	for j, r := range h.regs {
		if r == 0 {
			continue
		}
		dropped := uint64(j) & (1<<d - 1)
		var rho uint8
		if dropped != 0 {
			rho = uint8(int(d)-bits.Len64(dropped)) + 1
		} else {
			rho = uint8(d) + r
		}
		ni := j >> d
		if rho > regs[ni] {
			regs[ni] = rho
		}
	}
	h.p, h.regs = p, regs
}

func (h *HLL) clone() *HLL {
	c := &HLL{p: h.p}
	if h.sparse != nil {
		c.sparse = make(map[uint64]struct{}, len(h.sparse))
		for k := range h.sparse {
			c.sparse[k] = struct{}{}
		}
	} else {
		c.regs = append([]uint8(nil), h.regs...)
	}
	return c
}

// Merge merges o into h. If the precisions differ, the result uses the lower
// precision.
func (h *HLL) Merge(o *HLL) {
	if o == nil {
		return
	}
	if o.p > h.p {
		o = o.clone()
		o.downgrade(h.p)
	} else if o.p < h.p {
		h.downgrade(o.p)
	}
	switch {
	case o.sparse != nil:
		for x := range o.sparse {
			h.addHash(x)
		}
	default:
		if h.sparse != nil {
			h.densify()
		}
		for i, r := range o.regs {
			if r > h.regs[i] {
				h.regs[i] = r
			}
		}
	}
}

// Estimate returns the estimated number of distinct items.
func (h *HLL) Estimate() int64 {
	if h.sparse != nil {
		return int64(len(h.sparse))
	}
	m := float64(len(h.regs))
	q := 64 - int(h.p)
	counts := make([]float64, q+2)
	for _, r := range h.regs {
		counts[r]++
	}
	z := m * hllTau(1-counts[q+1]/m)
	for k := q; k >= 1; k-- {
		z = 0.5 * (z + counts[k])
	}
	z += m * hllSigma(counts[0]/m)
	est := m * m / (2 * math.Ln2) / z
	return int64(math.Round(est))
}

func hllSigma(x float64) float64 {
	if x == 1 {
		return math.Inf(1)
	}
	y, z := 1.0, x
	for {
		x *= x
		zp := z
		z += x * y
		y += y
		if z == zp {
			return z
		}
	}
}

func hllTau(x float64) float64 {
	if x == 0 || x == 1 {
		return 0
	}
	y, z := 1.0, 1-x
	for {
		x = math.Sqrt(x)
		zp := z
		y *= 0.5
		z -= (1 - x) * (1 - x) * y
		if z == zp {
			return z / 3
		}
	}
}

// Encode serializes the sketch (see the HLL type documentation).
func (h *HLL) Encode() []byte {
	out := []byte{hllVersion, h.p}
	if h.sparse != nil {
		out = append(out, 0)
		hs := make([]uint64, 0, len(h.sparse))
		for x := range h.sparse {
			hs = append(hs, x)
		}
		sort.Slice(hs, func(i, j int) bool { return hs[i] < hs[j] })
		out = binary.AppendUvarint(out, uint64(len(hs)))
		prev := uint64(0)
		for _, x := range hs {
			out = binary.AppendUvarint(out, x-prev)
			prev = x
		}
		return out
	}
	out = append(out, 1)
	return append(out, h.regs...)
}

// DecodeHLL parses a sketch produced by Encode. Empty input is an error.
func DecodeHLL(b []byte) (*HLL, error) {
	if len(b) < 3 {
		return nil, errors.New("HLL sketch is empty or truncated")
	}
	if b[0] != hllVersion {
		return nil, errors.New("unsupported HLL sketch version")
	}
	p := b[1]
	if p < 4 || p > hllMaxPrecision {
		return nil, errors.New("invalid HLL sketch precision")
	}
	h := &HLL{p: p}
	switch b[2] {
	case 0:
		rest := b[3:]
		n, k := binary.Uvarint(rest)
		if k <= 0 || n > uint64(1)<<p {
			return nil, errors.New("invalid sparse HLL sketch")
		}
		rest = rest[k:]
		h.sparse = make(map[uint64]struct{}, n)
		prev := uint64(0)
		for i := uint64(0); i < n; i++ {
			d, k := binary.Uvarint(rest)
			if k <= 0 {
				return nil, errors.New("truncated sparse HLL sketch")
			}
			rest = rest[k:]
			prev += d
			h.sparse[prev] = struct{}{}
		}
		if len(rest) != 0 {
			return nil, errors.New("trailing bytes in sparse HLL sketch")
		}
		if len(h.sparse) > h.sparseLimit() {
			h.densify()
		}
	case 1:
		if len(b)-3 != 1<<p {
			return nil, errors.New("invalid dense HLL sketch length")
		}
		h.regs = append([]uint8(nil), b[3:]...)
		for _, r := range h.regs {
			if int(r) > 64-int(p)+1 {
				return nil, errors.New("invalid dense HLL register value")
			}
		}
	default:
		return nil, errors.New("invalid HLL sketch representation")
	}
	return h, nil
}

// hllInput returns the canonical item bytes for INT64 (8-byte big-endian),
// STRING (UTF-8) and BYTES (raw) runtime values.
func hllInput(v any) ([]byte, bool) {
	switch x := v.(type) {
	case int64:
		return binary.BigEndian.AppendUint64(nil, uint64(x)), true
	case string:
		return []byte(x), true
	case []byte:
		return x, true
	}
	return nil, false
}

// HLLInputBytes returns the canonical bytes that HLL_COUNT.INIT hashes for a
// wire value, so that HLL aggregate families fed through AddToCell agree with
// SQL-built sketches: INT64 as 8-byte big-endian, STRING as UTF-8, BYTES and
// raw values as-is. It reports false for NULL or unsupported kinds.
func HLLInputBytes(v *btpb.Value) ([]byte, bool) {
	if v == nil {
		return nil, false
	}
	switch k := v.Kind.(type) {
	case *btpb.Value_IntValue:
		return hllInput(k.IntValue)
	case *btpb.Value_StringValue:
		return hllInput(k.StringValue)
	case *btpb.Value_BytesValue:
		return hllInput(k.BytesValue)
	case *btpb.Value_RawValue:
		return k.RawValue, true
	}
	return nil, false
}

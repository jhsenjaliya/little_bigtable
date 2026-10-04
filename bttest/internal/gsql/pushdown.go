package gsql

import (
	"bytes"
	"sort"
)

// keyRange is a half-open row key range [start, end). A nil start is the
// beginning of the table and a nil end is unbounded.
type keyRange struct {
	start []byte
	end   []byte
}

func keySuccessor(k []byte) []byte { return append(append([]byte{}, k...), 0) }

// prefixSuccessor returns the smallest key greater than every key with the
// given prefix, or nil if there is none.
func prefixSuccessor(p []byte) []byte {
	out := append([]byte{}, p...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xff {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}

func (r keyRange) empty() bool {
	return r.end != nil && bytes.Compare(r.start, r.end) >= 0
}

func cmpEnd(a, b []byte) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return bytes.Compare(a, b)
}

func normalizeRanges(rs []keyRange) []keyRange {
	var out []keyRange
	for _, r := range rs {
		if !r.empty() {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].start, out[j].start) < 0 })
	var merged []keyRange
	for _, r := range out {
		if n := len(merged); n > 0 {
			last := &merged[n-1]
			if last.end == nil || bytes.Compare(r.start, last.end) <= 0 {
				if cmpEnd(r.end, last.end) > 0 {
					last.end = r.end
				}
				continue
			}
		}
		merged = append(merged, r)
	}
	return merged
}

func intersectRanges(a, b []keyRange) []keyRange {
	var out []keyRange
	for _, x := range a {
		for _, y := range b {
			r := keyRange{start: x.start, end: x.end}
			if bytes.Compare(y.start, r.start) > 0 {
				r.start = y.start
			}
			if cmpEnd(y.end, r.end) < 0 {
				r.end = y.end
			}
			if !r.empty() {
				out = append(out, r)
			}
		}
	}
	return normalizeRanges(out)
}

// keyRanges computes the row ranges to scan for the plan's WHERE clause.
// The result is always a superset of the matching rows; the full WHERE
// clause is still evaluated for every scanned row.
func (p *queryPlan) keyRanges(env *execEnv, ec *evalCtx) []keyRange {
	full := []keyRange{{}}
	if env.noPushdown || p.where == nil {
		return full
	}
	rs, ok := p.rangesFor(p.where, ec)
	if !ok {
		return full
	}
	return normalizeRanges(rs)
}

func (p *queryPlan) isBaseKey(e *bexpr) bool {
	return e.kind == ekColumn && e.idx == 0 && p.scope != nil && p.scope.keyCol == 0
}

// constBytes evaluates a constant BYTES expression.
func constBytes(e *bexpr, ec *evalCtx) (val []byte, isNull, ok bool) {
	if !isConst(e) || e.typ.kind != tkBytes {
		return nil, false, false
	}
	v, err := e.eval(&evalCtx{params: ec.params, now: ec.now})
	if err != nil {
		return nil, false, false
	}
	if v == nil {
		return nil, true, true
	}
	return v.([]byte), false, true
}

// rangesFor returns ranges that contain every row for which e may be TRUE.
// ok is false when e does not constrain the key.
func (p *queryPlan) rangesFor(e *bexpr, ec *evalCtx) ([]keyRange, bool) {
	switch e.name {
	case "$and":
		l, lok := p.rangesFor(e.args[0], ec)
		r, rok := p.rangesFor(e.args[1], ec)
		switch {
		case lok && rok:
			return intersectRanges(normalizeRanges(l), normalizeRanges(r)), true
		case lok:
			return l, true
		case rok:
			return r, true
		}
		return nil, false
	case "$or":
		l, lok := p.rangesFor(e.args[0], ec)
		r, rok := p.rangesFor(e.args[1], ec)
		if !lok || !rok {
			return nil, false
		}
		return normalizeRanges(append(append([]keyRange{}, l...), r...)), true
	case "$eq", "$lt", "$le", "$gt", "$ge":
		op := e.name
		k, c := e.args[0], e.args[1]
		if !p.isBaseKey(k) {
			k, c = c, k
			op = map[string]string{"$eq": "$eq", "$lt": "$gt", "$le": "$ge", "$gt": "$lt", "$ge": "$le"}[op]
		}
		if !p.isBaseKey(k) {
			return nil, false
		}
		v, isNull, ok := constBytes(c, ec)
		if !ok {
			return nil, false
		}
		if isNull {
			return []keyRange{}, true
		}
		switch op {
		case "$eq":
			return []keyRange{{start: v, end: keySuccessor(v)}}, true
		case "$lt":
			return []keyRange{{end: v}}, true
		case "$le":
			return []keyRange{{end: keySuccessor(v)}}, true
		case "$gt":
			return []keyRange{{start: keySuccessor(v)}}, true
		default:
			return []keyRange{{start: v}}, true
		}
	case "$in":
		if !p.isBaseKey(e.args[0]) {
			return nil, false
		}
		var out []keyRange
		for _, x := range e.args[1:] {
			v, isNull, ok := constBytes(x, ec)
			if !ok {
				return nil, false
			}
			if !isNull {
				out = append(out, keyRange{start: v, end: keySuccessor(v)})
			}
		}
		return out, true
	case "$in_unnest":
		if !p.isBaseKey(e.args[0]) || !isConst(e.args[1]) {
			return nil, false
		}
		v, err := e.args[1].eval(&evalCtx{params: ec.params, now: ec.now})
		if err != nil {
			return nil, false
		}
		var out []keyRange
		if v != nil {
			for _, x := range v.(arrayV) {
				if b, ok := x.([]byte); ok {
					out = append(out, keyRange{start: b, end: keySuccessor(b)})
				}
			}
		}
		return out, true
	case "$between":
		if !p.isBaseKey(e.args[0]) {
			return nil, false
		}
		lo, ln, ok1 := constBytes(e.args[1], ec)
		hi, hn, ok2 := constBytes(e.args[2], ec)
		if !ok1 || !ok2 {
			return nil, false
		}
		if ln || hn {
			return []keyRange{}, true
		}
		return []keyRange{{start: lo, end: keySuccessor(hi)}}, true
	case "STARTS_WITH":
		if len(e.args) != 2 || !p.isBaseKey(e.args[0]) {
			return nil, false
		}
		pre, isNull, ok := constBytes(e.args[1], ec)
		if !ok {
			return nil, false
		}
		if isNull {
			return []keyRange{}, true
		}
		return []keyRange{{start: pre, end: prefixSuccessor(pre)}}, true
	case "$like":
		if !p.isBaseKey(e.args[0]) {
			return nil, false
		}
		pat, isNull, ok := constBytes(e.args[1], ec)
		if !ok {
			return nil, false
		}
		if isNull {
			return []keyRange{}, true
		}
		pre, exact := likePrefix(pat)
		if exact {
			return []keyRange{{start: pre, end: keySuccessor(pre)}}, true
		}
		return []keyRange{{start: pre, end: prefixSuccessor(pre)}}, true
	}
	return nil, false
}

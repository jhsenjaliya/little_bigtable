package bttest

import (
	"bytes"
	"log"
	"math/rand"
	"regexp"
	"sort"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"rsc.io/binaryregexp"
)

// Bigtable labels are 1-15 characters of [a-z0-9-].
var validLabelTransformer = regexp.MustCompile(`^[a-z0-9\-]{1,15}$`)

// maxFilterDepth bounds recursion on hostile or malformed filter trees.
const maxFilterDepth = 20

var randFloat = rand.Float64

// validateFilter checks filter arguments that do not depend on row data, so a
// malformed filter fails before any row is read.
func validateFilter(f *btpb.RowFilter) error {
	return validateFilterAt(f, 0, false)
}

func validateFilterAt(f *btpb.RowFilter, depth int, inCondition bool) error {
	if f == nil {
		return nil
	}
	if depth > maxFilterDepth {
		return status.Errorf(codes.InvalidArgument, "filter nesting exceeds %d levels", maxFilterDepth)
	}
	switch f := f.Filter.(type) {
	case nil:
		return nil
	case *btpb.RowFilter_Chain_:
		if len(f.Chain.Filters) < 2 {
			return status.Errorf(codes.InvalidArgument, "Chain must contain at least two RowFilters")
		}
		labelled := 0
		for _, sub := range f.Chain.Filters {
			if err := validateFilterAt(sub, depth+1, inCondition); err != nil {
				return err
			}
			if containsLabelTransformer(sub) {
				labelled++
			}
		}
		if labelled > 1 {
			return status.Errorf(codes.InvalidArgument, "a Chain may contain at most one sub-filter with apply_label_transformer")
		}
	case *btpb.RowFilter_Interleave_:
		if len(f.Interleave.Filters) < 2 {
			return status.Errorf(codes.InvalidArgument, "Interleave must contain at least two RowFilters")
		}
		for _, sub := range f.Interleave.Filters {
			if err := validateFilterAt(sub, depth+1, inCondition); err != nil {
				return err
			}
		}
	case *btpb.RowFilter_Condition_:
		if f.Condition == nil {
			return status.Errorf(codes.InvalidArgument, "condition filter is empty")
		}
		for _, sub := range []*btpb.RowFilter{f.Condition.PredicateFilter, f.Condition.TrueFilter, f.Condition.FalseFilter} {
			if err := validateFilterAt(sub, depth+1, true); err != nil {
				return err
			}
		}
	case *btpb.RowFilter_Sink:
		if !f.Sink {
			return status.Errorf(codes.InvalidArgument, "sink must be true if set")
		}
		if inCondition {
			return status.Errorf(codes.InvalidArgument, "sink cannot be used within a Condition filter")
		}
	case *btpb.RowFilter_PassAllFilter:
		if !f.PassAllFilter {
			return status.Errorf(codes.InvalidArgument, "pass_all_filter must be true if set")
		}
	case *btpb.RowFilter_BlockAllFilter:
		if !f.BlockAllFilter {
			return status.Errorf(codes.InvalidArgument, "block_all_filter must be true if set")
		}
	case *btpb.RowFilter_RowKeyRegexFilter:
		if _, err := newRegexp(f.RowKeyRegexFilter); err != nil {
			return status.Errorf(codes.InvalidArgument, "Error in field 'rowkey_regex_filter' : %v", err)
		}
	case *btpb.RowFilter_RowSampleFilter:
		if f.RowSampleFilter <= 0.0 || f.RowSampleFilter >= 1.0 {
			return status.Error(codes.InvalidArgument, "row_sample_filter argument must be between 0.0 and 1.0")
		}
	case *btpb.RowFilter_FamilyNameRegexFilter:
		if _, err := newRegexp([]byte(f.FamilyNameRegexFilter)); err != nil {
			return status.Errorf(codes.InvalidArgument, "Error in field 'family_name_regex_filter' : %v", err)
		}
	case *btpb.RowFilter_ColumnQualifierRegexFilter:
		if _, err := newRegexp(f.ColumnQualifierRegexFilter); err != nil {
			return status.Errorf(codes.InvalidArgument, "Error in field 'column_qualifier_regex_filter' : %v", err)
		}
	case *btpb.RowFilter_ValueRegexFilter:
		if _, err := newRegexp(f.ValueRegexFilter); err != nil {
			return status.Errorf(codes.InvalidArgument, "Error in field 'value_regex_filter' : %v", err)
		}
	case *btpb.RowFilter_ColumnRangeFilter:
		if f.ColumnRangeFilter == nil || f.ColumnRangeFilter.FamilyName == "" {
			return status.Errorf(codes.InvalidArgument, "column_range_filter requires a family_name")
		}
	case *btpb.RowFilter_TimestampRangeFilter:
		tr := f.TimestampRangeFilter
		if tr == nil {
			return nil
		}
		if tr.StartTimestampMicros%1000 != 0 || tr.EndTimestampMicros%1000 != 0 {
			return status.Errorf(codes.InvalidArgument, "Error in field 'timestamp_range_filter'. Maximum precision allowed in filter is millisecond.\nGot:\nStart: %v\nEnd: %v", tr.StartTimestampMicros, tr.EndTimestampMicros)
		}
		if tr.StartTimestampMicros < 0 || tr.EndTimestampMicros < 0 {
			return status.Errorf(codes.InvalidArgument, "timestamp_range_filter bounds must not be negative")
		}
	case *btpb.RowFilter_CellsPerRowOffsetFilter:
		if f.CellsPerRowOffsetFilter < 0 {
			return status.Errorf(codes.InvalidArgument, "cells_per_row_offset_filter must not be negative")
		}
	case *btpb.RowFilter_CellsPerRowLimitFilter:
		if f.CellsPerRowLimitFilter < 0 {
			return status.Errorf(codes.InvalidArgument, "cells_per_row_limit_filter must not be negative")
		}
	case *btpb.RowFilter_CellsPerColumnLimitFilter:
		if f.CellsPerColumnLimitFilter < 0 {
			return status.Errorf(codes.InvalidArgument, "cells_per_column_limit_filter must not be negative")
		}
	case *btpb.RowFilter_ApplyLabelTransformer:
		if !validLabelTransformer.MatchString(f.ApplyLabelTransformer) {
			return status.Errorf(codes.InvalidArgument,
				`apply_label_transformer must match RE2([a-z0-9\-]+) and be at most 15 characters, but found %v`, f.ApplyLabelTransformer)
		}
	case *btpb.RowFilter_StripValueTransformer:
		if !f.StripValueTransformer {
			return status.Errorf(codes.InvalidArgument, "strip_value_transformer must be true if set")
		}
	case *btpb.RowFilter_ValueBitmaskFilter:
		if len(f.ValueBitmaskFilter.GetMask()) == 0 {
			return status.Errorf(codes.InvalidArgument, "value_bitmask_filter requires a non-empty mask")
		}
	case *btpb.RowFilter_ValueRangeFilter:
	default:
		return status.Errorf(codes.Unimplemented, "unsupported filter type %T", f)
	}
	return nil
}

func containsLabelTransformer(f *btpb.RowFilter) bool {
	if f == nil {
		return false
	}
	switch f := f.Filter.(type) {
	case *btpb.RowFilter_ApplyLabelTransformer:
		return true
	case *btpb.RowFilter_Chain_:
		for _, sub := range f.Chain.Filters {
			if containsLabelTransformer(sub) {
				return true
			}
		}
	case *btpb.RowFilter_Interleave_:
		for _, sub := range f.Interleave.Filters {
			if containsLabelTransformer(sub) {
				return true
			}
		}
	case *btpb.RowFilter_Condition_:
		return containsLabelTransformer(f.Condition.GetTrueFilter()) || containsLabelTransformer(f.Condition.GetFalseFilter())
	}
	return false
}

// filterRow replaces r's cells with the cells selected by f, including any
// cells emitted through a Sink. It returns whether at least one cell remains.
// Stored rows must never be passed directly: callers filter a copy.
func filterRow(f *btpb.RowFilter, r *row) (bool, error) {
	if f == nil {
		return !r.isEmpty(), nil
	}
	if err := validateFilter(f); err != nil {
		return false, err
	}
	sink := newRow(r.key)
	out, err := evalFilter(f, r, sink)
	if err != nil {
		return false, err
	}
	mergeCells(out, sink)
	out.normalize()
	r.families = out.families
	return !r.isEmpty(), nil
}

// evalFilter returns a new row containing the cells f passes to its parent.
// Cells reaching a Sink are appended to sink instead. The input row is not
// modified.
func evalFilter(f *btpb.RowFilter, in *row, sink *row) (*row, error) {
	if f == nil {
		return in.copy(), nil
	}
	switch f := f.Filter.(type) {
	case nil:
		return in.copy(), nil
	case *btpb.RowFilter_PassAllFilter:
		return in.copy(), nil
	case *btpb.RowFilter_BlockAllFilter:
		return newRow(in.key), nil
	case *btpb.RowFilter_Sink:
		mergeCells(sink, in)
		return newRow(in.key), nil
	case *btpb.RowFilter_Chain_:
		cur := in
		for _, sub := range f.Chain.Filters {
			next, err := evalFilter(sub, cur, sink)
			if err != nil {
				return nil, err
			}
			cur = next
		}
		if cur == in {
			return in.copy(), nil
		}
		return cur, nil
	case *btpb.RowFilter_Interleave_:
		out := newRow(in.key)
		for _, sub := range f.Interleave.Filters {
			branch, err := evalFilter(sub, in, sink)
			if err != nil {
				return nil, err
			}
			mergeCells(out, branch)
		}
		out.normalize()
		return out, nil
	case *btpb.RowFilter_Condition_:
		predicate, err := evalFilter(f.Condition.PredicateFilter, in, newRow(in.key))
		if err != nil {
			return nil, err
		}
		next := f.Condition.FalseFilter
		if !predicate.isEmpty() {
			next = f.Condition.TrueFilter
		}
		if next == nil {
			return newRow(in.key), nil
		}
		return evalFilter(next, in, sink)
	case *btpb.RowFilter_RowKeyRegexFilter:
		rx, err := newRegexp(f.RowKeyRegexFilter)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Error in field 'rowkey_regex_filter' : %v", err)
		}
		if !rx.MatchString(in.key) {
			return newRow(in.key), nil
		}
		return in.copy(), nil
	case *btpb.RowFilter_RowSampleFilter:
		if randFloat() < f.RowSampleFilter {
			return in.copy(), nil
		}
		return newRow(in.key), nil
	case *btpb.RowFilter_CellsPerColumnLimitFilter:
		out := in.copy()
		lim := int(f.CellsPerColumnLimitFilter)
		for _, fam := range out.families {
			for col, cs := range fam.Cells {
				if len(cs) > lim {
					fam.Cells[col] = cs[:lim]
				}
			}
		}
		out.normalize()
		return out, nil
	case *btpb.RowFilter_CellsPerRowLimitFilter:
		out := in.copy()
		remaining := int(f.CellsPerRowLimitFilter)
		for _, fam := range out.sortedFamilies() {
			for _, col := range fam.ColNames {
				cs := fam.Cells[col]
				if len(cs) > remaining {
					cs = cs[:remaining]
				}
				fam.Cells[col] = cs
				remaining -= len(cs)
			}
		}
		out.normalize()
		return out, nil
	case *btpb.RowFilter_CellsPerRowOffsetFilter:
		out := in.copy()
		skip := int(f.CellsPerRowOffsetFilter)
		for _, fam := range out.sortedFamilies() {
			for _, col := range fam.ColNames {
				cs := fam.Cells[col]
				if skip >= len(cs) {
					skip -= len(cs)
					fam.Cells[col] = nil
					continue
				}
				fam.Cells[col] = cs[skip:]
				skip = 0
			}
		}
		out.normalize()
		return out, nil
	}

	// Per-cell filters and transformers.
	out := newRow(in.key)
	for _, fam := range in.families {
		for col, cs := range fam.Cells {
			for _, c := range cs {
				include, err := includeCell(f, fam.Name, col, c)
				if err != nil {
					return nil, err
				}
				if !include {
					continue
				}
				c, err = modifyCell(f, c)
				if err != nil {
					return nil, err
				}
				of := out.getOrCreateFamily(fam.Name, fam.Order)
				of.Cells[col] = append(of.Cells[col], c)
			}
		}
	}
	out.normalize()
	return out, nil
}

// mergeCells appends every cell of src to dst, preserving duplicates.
func mergeCells(dst, src *row) {
	for _, fam := range src.families {
		df := dst.getOrCreateFamily(fam.Name, fam.Order)
		for col, cs := range fam.Cells {
			df.Cells[col] = append(df.Cells[col], cs...)
		}
	}
	dst.normalize()
}

func modifyCell(f *btpb.RowFilter, c cell) (cell, error) {
	switch filter := f.Filter.(type) {
	case *btpb.RowFilter_StripValueTransformer:
		return cell{Ts: c.Ts, Labels: c.Labels}, nil
	case *btpb.RowFilter_ApplyLabelTransformer:
		if !validLabelTransformer.MatchString(filter.ApplyLabelTransformer) {
			return cell{}, status.Errorf(codes.InvalidArgument,
				`apply_label_transformer must match RE2([a-z0-9\-]+) and be at most 15 characters, but found %v`, filter.ApplyLabelTransformer)
		}
		return cell{Ts: c.Ts, Value: c.Value, Labels: []string{filter.ApplyLabelTransformer}}, nil
	default:
		return c, nil
	}
}

func includeCell(f *btpb.RowFilter, fam, col string, c cell) (bool, error) {
	switch f := f.Filter.(type) {
	case *btpb.RowFilter_StripValueTransformer, *btpb.RowFilter_ApplyLabelTransformer:
		return true, nil
	case *btpb.RowFilter_FamilyNameRegexFilter:
		rx, err := newRegexp([]byte(f.FamilyNameRegexFilter))
		if err != nil {
			return false, status.Errorf(codes.InvalidArgument, "Error in field 'family_name_regex_filter' : %v", err)
		}
		return rx.MatchString(fam), nil
	case *btpb.RowFilter_ColumnQualifierRegexFilter:
		rx, err := newRegexp(f.ColumnQualifierRegexFilter)
		if err != nil {
			return false, status.Errorf(codes.InvalidArgument, "Error in field 'column_qualifier_regex_filter' : %v", err)
		}
		return rx.MatchString(col), nil
	case *btpb.RowFilter_ValueRegexFilter:
		rx, err := newRegexp(f.ValueRegexFilter)
		if err != nil {
			return false, status.Errorf(codes.InvalidArgument, "Error in field 'value_regex_filter' : %v", err)
		}
		return rx.Match(c.Value), nil
	case *btpb.RowFilter_ColumnRangeFilter:
		if fam != f.ColumnRangeFilter.FamilyName {
			return false, nil
		}
		switch sq := f.ColumnRangeFilter.StartQualifier.(type) {
		case *btpb.ColumnRange_StartQualifierOpen:
			if col <= string(sq.StartQualifierOpen) {
				return false, nil
			}
		case *btpb.ColumnRange_StartQualifierClosed:
			if col < string(sq.StartQualifierClosed) {
				return false, nil
			}
		}
		switch eq := f.ColumnRangeFilter.EndQualifier.(type) {
		case *btpb.ColumnRange_EndQualifierClosed:
			return col <= string(eq.EndQualifierClosed), nil
		case *btpb.ColumnRange_EndQualifierOpen:
			return col < string(eq.EndQualifierOpen), nil
		}
		return true, nil
	case *btpb.RowFilter_TimestampRangeFilter:
		tr := f.TimestampRangeFilter
		if tr.StartTimestampMicros%int64(time.Millisecond/time.Microsecond) != 0 || tr.EndTimestampMicros%int64(time.Millisecond/time.Microsecond) != 0 {
			return false, status.Errorf(codes.InvalidArgument, "Error in field 'timestamp_range_filter'. Maximum precision allowed in filter is millisecond.\nGot:\nStart: %v\nEnd: %v", tr.StartTimestampMicros, tr.EndTimestampMicros)
		}
		// Lower bound is inclusive and defaults to 0; upper bound is exclusive and defaults to infinity.
		return c.Ts >= tr.StartTimestampMicros && (tr.EndTimestampMicros == 0 || c.Ts < tr.EndTimestampMicros), nil
	case *btpb.RowFilter_ValueBitmaskFilter:
		// A cell matches when (value & mask) == mask; a length mismatch never matches.
		mask := f.ValueBitmaskFilter.GetMask()
		if len(mask) != len(c.Value) {
			return false, nil
		}
		for i := range mask {
			if c.Value[i]&mask[i] != mask[i] {
				return false, nil
			}
		}
		return true, nil
	case *btpb.RowFilter_ValueRangeFilter:
		v := c.Value
		switch sv := f.ValueRangeFilter.StartValue.(type) {
		case *btpb.ValueRange_StartValueOpen:
			if bytes.Compare(v, sv.StartValueOpen) <= 0 {
				return false, nil
			}
		case *btpb.ValueRange_StartValueClosed:
			if bytes.Compare(v, sv.StartValueClosed) < 0 {
				return false, nil
			}
		}
		switch ev := f.ValueRangeFilter.EndValue.(type) {
		case *btpb.ValueRange_EndValueClosed:
			return bytes.Compare(v, ev.EndValueClosed) <= 0, nil
		case *btpb.ValueRange_EndValueOpen:
			return bytes.Compare(v, ev.EndValueOpen) < 0, nil
		}
		return true, nil
	default:
		return false, status.Errorf(codes.Unimplemented, "unsupported filter type %T", f)
	}
}

// escapeUTF is used to escape non-ASCII characters in pattern strings passed
// to binaryregexp. This makes regexp column and row key matching work more
// closely to what's seen with the real BigTable.
func escapeUTF(in []byte) []byte {
	var toEsc int
	for _, c := range in {
		if c > 127 {
			toEsc++
		}
	}
	if toEsc == 0 {
		return in
	}
	// Each escaped byte becomes 4 bytes (byte a1 becomes \xA1)
	out := make([]byte, 0, len(in)+3*toEsc)
	for _, c := range in {
		if c > 127 {
			h, l := c>>4, c&0xF
			const conv = "0123456789ABCDEF"
			out = append(out, '\\', 'x', conv[h], conv[l])
		} else {
			out = append(out, c)
		}
	}
	return out
}

func newRegexp(pat []byte) (*binaryregexp.Regexp, error) {
	re, err := binaryregexp.Compile("^(?:" + string(escapeUTF(pat)) + ")$") // match entire target
	if err != nil {
		log.Printf("Bad pattern %q: %v", pat, err)
	}
	return re, err
}

// normalize sorts columns and cells and drops empty columns and families.
// Duplicate cells (from Interleave or Sink) are kept in stable order.
func (r *row) normalize() {
	for name, fam := range r.families {
		cols := fam.ColNames[:0:0]
		for col, cs := range fam.Cells {
			if len(cs) == 0 {
				delete(fam.Cells, col)
				continue
			}
			sort.Stable(byDescTS(cs))
			cols = append(cols, col)
		}
		if len(cols) == 0 {
			delete(r.families, name)
			continue
		}
		sort.Strings(cols)
		fam.ColNames = cols
	}
}

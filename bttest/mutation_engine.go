package bttest

import (
	"context"
	"encoding/binary"
	"math"
	"sort"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// maxMutationsPerRequest is Bigtable's documented per-request mutation cap.
	maxMutationsPerRequest = 100000
	// maxRowKeyBytes is Bigtable's documented row-key size limit (4 KiB).
	maxRowKeyBytes = 4 * 1024

	// MilliSeconds field of the minimum valid Timestamp.
	minValidMilliSeconds = 0
	// MilliSeconds field of the max valid Timestamp. Must match the max value of
	// TimestampMicros truncated to millisecond granularity.
	maxValidMilliSeconds = math.MaxInt64 - math.MaxInt64%1000
)

// newTimestamp returns the server time in microseconds, truncated to the
// millisecond granularity every local table uses.
func newTimestamp() int64 {
	ts := time.Now().UnixNano() / 1e3
	return ts - ts%1000
}

func validTimestamp(ts int64) bool {
	return ts >= minValidMilliSeconds && ts <= maxValidMilliSeconds && ts%1000 == 0
}

// granularity describes which timestamps a table accepts.
type granularity struct{ micros bool }

func tableGranularity(t *table) granularity {
	return granularity{micros: t.granularity == btapb.Table_MICROS}
}

func (g granularity) valid(ts int64) bool {
	if g.micros {
		return ts >= 0
	}
	return validTimestamp(ts)
}

// resolve returns the timestamp a mutation stores. Client auto-generated
// timestamps are truncated to the table granularity; user-specified ones
// must already match it.
func (g granularity) resolve(ts int64, origin btpb.Mutation_TimestampOrigin, now int64) (int64, bool) {
	if ts == -1 {
		return now, true
	}
	if origin == btpb.Mutation_CLIENT_AUTO_GENERATED && !g.micros && ts >= 0 {
		ts -= ts % 1000
	}
	return ts, g.valid(ts)
}

func validateRowKey(key []byte) error {
	if len(key) == 0 {
		return status.Error(codes.InvalidArgument, "row keys must be non-empty")
	}
	if len(key) > maxRowKeyBytes {
		return status.Errorf(codes.InvalidArgument, "row key is %d bytes; the maximum is %d", len(key), maxRowKeyBytes)
	}
	return nil
}

func unknownFamilyError(family string) error {
	return status.Errorf(codes.NotFound, "Requested column family not found: %q", family)
}

// aggregateKind is the aggregate behavior of a column family's value type.
type aggregateKind int

const (
	aggregateNone aggregateKind = iota
	aggregateSum
	aggregateMin
	aggregateMax
	aggregateHLL
)

func familyAggregate(cf *columnFamily) aggregateKind {
	if cf == nil || cf.ValueType == nil {
		return aggregateNone
	}
	agg := cf.ValueType.GetAggregateType()
	if agg == nil {
		return aggregateNone
	}
	switch agg.GetAggregator().(type) {
	case *btapb.Type_Aggregate_Sum_:
		return aggregateSum
	case *btapb.Type_Aggregate_Min_:
		return aggregateMin
	case *btapb.Type_Aggregate_Max_:
		return aggregateMax
	case *btapb.Type_Aggregate_HllppUniqueCount:
		return aggregateHLL
	}
	return aggregateNone
}

// validateMutations checks a complete mutation list before anything is
// applied, so an invalid later mutation can never leave an earlier one
// committed.
func validateMutations(muts []*btpb.Mutation, fams map[string]*columnFamily, g granularity) error {
	if len(muts) == 0 {
		return status.Error(codes.InvalidArgument, "No mutations provided")
	}
	if len(muts) > maxMutationsPerRequest {
		return status.Errorf(codes.InvalidArgument, "too many mutations: %d (maximum %d)", len(muts), maxMutationsPerRequest)
	}
	for i, mut := range muts {
		if err := validateMutation(mut, fams, g); err != nil {
			st, _ := status.FromError(err)
			return status.Errorf(st.Code(), "mutation %d: %s", i, st.Message())
		}
	}
	return nil
}

func validateMutation(mut *btpb.Mutation, fams map[string]*columnFamily, g granularity) error {
	switch m := mut.GetMutation().(type) {
	case *btpb.Mutation_SetCell_:
		cf, ok := fams[m.SetCell.GetFamilyName()]
		if !ok {
			return unknownFamilyError(m.SetCell.GetFamilyName())
		}
		if familyAggregate(cf) != aggregateNone {
			return status.Errorf(codes.InvalidArgument, "SetCell is not supported on aggregate column family %q; use AddToCell or MergeToCell", m.SetCell.GetFamilyName())
		}
		if _, ok := g.resolve(m.SetCell.GetTimestampMicros(), mut.GetTimestampOrigin(), 0); !ok {
			return status.Errorf(codes.InvalidArgument, "invalid timestamp %d: timestamps must be non-negative and match the table granularity", m.SetCell.GetTimestampMicros())
		}
	case *btpb.Mutation_DeleteFromColumn_:
		del := m.DeleteFromColumn
		if _, ok := fams[del.GetFamilyName()]; !ok {
			return unknownFamilyError(del.GetFamilyName())
		}
		if tr := del.GetTimeRange(); tr != nil {
			if !g.valid(tr.StartTimestampMicros) {
				return status.Errorf(codes.InvalidArgument, "invalid timestamp %d", tr.StartTimestampMicros)
			}
			if tr.EndTimestampMicros != 0 && !g.valid(tr.EndTimestampMicros) {
				return status.Errorf(codes.InvalidArgument, "invalid timestamp %d", tr.EndTimestampMicros)
			}
			if tr.EndTimestampMicros != 0 && tr.StartTimestampMicros >= tr.EndTimestampMicros {
				return status.Errorf(codes.InvalidArgument, "inverted or invalid timestamp range [%d, %d]", tr.StartTimestampMicros, tr.EndTimestampMicros)
			}
		}
	case *btpb.Mutation_DeleteFromFamily_:
		if _, ok := fams[m.DeleteFromFamily.GetFamilyName()]; !ok {
			return unknownFamilyError(m.DeleteFromFamily.GetFamilyName())
		}
	case *btpb.Mutation_DeleteFromRow_:
	case *btpb.Mutation_AddToCell_:
		return validateAggregateMutation(m.AddToCell.GetFamilyName(), m.AddToCell.GetColumnQualifier(), m.AddToCell.GetTimestamp(), m.AddToCell.GetInput(), fams, g, mut.GetTimestampOrigin(), false)
	case *btpb.Mutation_MergeToCell_:
		return validateAggregateMutation(m.MergeToCell.GetFamilyName(), m.MergeToCell.GetColumnQualifier(), m.MergeToCell.GetTimestamp(), m.MergeToCell.GetInput(), fams, g, mut.GetTimestampOrigin(), true)
	case nil:
		return status.Error(codes.InvalidArgument, "mutation is empty")
	default:
		return status.Errorf(codes.Unimplemented, "unsupported mutation type %T", m)
	}
	return nil
}

func validateAggregateMutation(family string, qualifier, timestamp, input *btpb.Value, fams map[string]*columnFamily, g granularity, origin btpb.Mutation_TimestampOrigin, merge bool) error {
	cf, ok := fams[family]
	if !ok {
		return unknownFamilyError(family)
	}
	op := "AddToCell"
	if merge {
		op = "MergeToCell"
	}
	kind := familyAggregate(cf)
	if kind == aggregateNone {
		return status.Errorf(codes.InvalidArgument, "%s requires an aggregate column family; %q has no aggregate value_type", op, family)
	}
	if _, ok := valueQualifier(qualifier); !ok {
		return status.Errorf(codes.InvalidArgument, "%s column_qualifier must be a raw, bytes or string value", op)
	}
	ts, ok := valueTimestamp(timestamp)
	if ok {
		_, ok = g.resolve(ts, origin, 0)
	}
	if !ok || ts == -1 {
		return status.Errorf(codes.InvalidArgument, "%s timestamp must be non-negative and match the table granularity", op)
	}
	if kind == aggregateHLL {
		if merge {
			if _, err := decodeHLLSketch(rawBytesValue(input)); err != nil {
				return status.Errorf(codes.InvalidArgument, "MergeToCell input is not a valid HLL++ sketch: %v", err)
			}
			return nil
		}
		if _, ok := hllInputBytes(input); !ok {
			return status.Errorf(codes.InvalidArgument, "AddToCell input for an HLL++ family must be an int, bytes, string or raw value")
		}
		return nil
	}
	if _, ok := int64Input(input); !ok {
		return status.Errorf(codes.InvalidArgument, "%s input for family %q must be an INT64 value", op, family)
	}
	return nil
}

func valueQualifier(v *btpb.Value) (string, bool) {
	switch k := v.GetKind().(type) {
	case *btpb.Value_RawValue:
		return string(k.RawValue), true
	case *btpb.Value_BytesValue:
		return string(k.BytesValue), true
	case *btpb.Value_StringValue:
		return k.StringValue, true
	}
	return "", false
}

func valueTimestamp(v *btpb.Value) (int64, bool) {
	switch k := v.GetKind().(type) {
	case *btpb.Value_RawTimestampMicros:
		return k.RawTimestampMicros, true
	case *btpb.Value_IntValue:
		return k.IntValue, true
	case *btpb.Value_TimestampValue:
		t := k.TimestampValue.AsTime()
		return t.UnixMicro(), true
	}
	return 0, false
}

func int64Input(v *btpb.Value) (int64, bool) {
	switch k := v.GetKind().(type) {
	case *btpb.Value_IntValue:
		return k.IntValue, true
	case *btpb.Value_RawValue:
		if len(k.RawValue) == 8 {
			return int64(binary.BigEndian.Uint64(k.RawValue)), true
		}
	case *btpb.Value_BytesValue:
		if len(k.BytesValue) == 8 {
			return int64(binary.BigEndian.Uint64(k.BytesValue)), true
		}
	}
	return 0, false
}

func rawBytesValue(v *btpb.Value) []byte {
	switch k := v.GetKind().(type) {
	case *btpb.Value_RawValue:
		return k.RawValue
	case *btpb.Value_BytesValue:
		return k.BytesValue
	case *btpb.Value_StringValue:
		return []byte(k.StringValue)
	}
	return nil
}

func encodeInt64(v int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return b[:]
}

// applyMutations applies already validated mutations to r in request order
// and returns the normalized changes a change stream records. now resolves
// server-assigned timestamps.
func applyMutations(r *row, muts []*btpb.Mutation, fams map[string]*columnFamily, g granularity, now int64) ([]*btpb.Mutation, error) {
	var changes []*btpb.Mutation
	for _, mut := range muts {
		switch m := mut.Mutation.(type) {
		case *btpb.Mutation_SetCell_:
			set := m.SetCell
			ts, _ := g.resolve(set.TimestampMicros, mut.GetTimestampOrigin(), now)
			col := string(set.ColumnQualifier)
			f := r.getOrCreateFamily(set.FamilyName, fams[set.FamilyName].Order)
			f.Cells[col] = appendOrReplaceCell(f.cellsByColumn(col), cell{Ts: ts, Value: set.Value})
			changes = append(changes, &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
				FamilyName: set.FamilyName, ColumnQualifier: set.ColumnQualifier, TimestampMicros: ts, Value: set.Value,
			}}})
		case *btpb.Mutation_DeleteFromColumn_:
			del := m.DeleteFromColumn
			deleteFromColumn(r, del.FamilyName, string(del.ColumnQualifier), del.TimeRange)
			changes = append(changes, mut)
		case *btpb.Mutation_DeleteFromFamily_:
			delete(r.families, m.DeleteFromFamily.FamilyName)
			changes = append(changes, mut)
		case *btpb.Mutation_DeleteFromRow_:
			// Change streams express a row deletion as one family deletion per
			// column family that holds data.
			for _, fam := range r.sortedFamilies() {
				changes = append(changes, &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromFamily_{
					DeleteFromFamily: &btpb.Mutation_DeleteFromFamily{FamilyName: fam.Name},
				}})
			}
			r.families = make(map[string]*family)
		case *btpb.Mutation_AddToCell_:
			add := m.AddToCell
			if err := applyAggregate(r, fams[add.FamilyName], add.FamilyName, add.ColumnQualifier, add.Timestamp, add.Input, g, mut.GetTimestampOrigin(), false); err != nil {
				return nil, err
			}
			changes = append(changes, mut)
		case *btpb.Mutation_MergeToCell_:
			merge := m.MergeToCell
			if err := applyAggregate(r, fams[merge.FamilyName], merge.FamilyName, merge.ColumnQualifier, merge.Timestamp, merge.Input, g, mut.GetTimestampOrigin(), true); err != nil {
				return nil, err
			}
			changes = append(changes, mut)
		default:
			return nil, status.Errorf(codes.Unimplemented, "unsupported mutation type %T", m)
		}
	}
	return changes, nil
}

func deleteFromColumn(r *row, famName, col string, tr *btpb.TimestampRange) {
	fam, ok := r.families[famName]
	if !ok {
		return
	}
	cs := fam.Cells[col]
	if tr != nil {
		kept := cs[:0:0]
		for _, c := range cs {
			if c.Ts >= tr.StartTimestampMicros && (tr.EndTimestampMicros == 0 || c.Ts < tr.EndTimestampMicros) {
				continue
			}
			kept = append(kept, c)
		}
		cs = kept
	} else {
		cs = nil
	}
	if len(cs) > 0 {
		fam.Cells[col] = cs
		return
	}
	delete(fam.Cells, col)
	for i, name := range fam.ColNames {
		if name == col {
			fam.ColNames = append(fam.ColNames[:i:i], fam.ColNames[i+1:]...)
			break
		}
	}
	if len(fam.Cells) == 0 {
		delete(r.families, famName)
	}
}

func applyAggregate(r *row, cf *columnFamily, famName string, qualifier, timestamp, input *btpb.Value, g granularity, origin btpb.Mutation_TimestampOrigin, merge bool) error {
	col, _ := valueQualifier(qualifier)
	ts, _ := valueTimestamp(timestamp)
	ts, _ = g.resolve(ts, origin, 0)
	f := r.getOrCreateFamily(famName, cf.Order)
	cs := f.cellsByColumn(col)
	idx := -1
	for i, c := range cs {
		if c.Ts == ts {
			idx = i
			break
		}
	}
	kind := familyAggregate(cf)
	if kind == aggregateHLL {
		var sketch *hllSketch
		if idx >= 0 {
			existing, err := decodeHLLSketch(cs[idx].Value)
			if err != nil {
				return status.Errorf(codes.FailedPrecondition, "stored HLL++ cell is corrupt: %v", err)
			}
			sketch = existing
		} else {
			sketch = newHLLSketch()
		}
		if merge {
			other, err := decodeHLLSketch(rawBytesValue(input))
			if err != nil {
				return status.Errorf(codes.InvalidArgument, "MergeToCell input is not a valid HLL++ sketch: %v", err)
			}
			sketch.merge(other)
		} else {
			data, _ := hllInputBytes(input)
			sketch.add(data)
		}
		value := sketch.encode()
		if idx >= 0 {
			cs[idx].Value = value
		} else {
			f.Cells[col] = appendOrReplaceCell(cs, cell{Ts: ts, Value: value})
		}
		return nil
	}

	in, _ := int64Input(input)
	if idx < 0 {
		f.Cells[col] = appendOrReplaceCell(cs, cell{Ts: ts, Value: encodeInt64(in)})
		return nil
	}
	existing := int64(0)
	if len(cs[idx].Value) == 8 {
		existing = int64(binary.BigEndian.Uint64(cs[idx].Value))
	}
	switch kind {
	case aggregateSum:
		existing += in
	case aggregateMin:
		if in < existing {
			existing = in
		}
	case aggregateMax:
		if in > existing {
			existing = in
		}
	}
	cs[idx].Value = encodeInt64(existing)
	return nil
}

func appendOrReplaceCell(cs []cell, newCell cell) []cell {
	for i, c := range cs {
		if c.Ts == newCell.Ts {
			cs[i] = newCell
			return cs
		}
	}
	cs = append(cs, newCell)
	sort.Sort(byDescTS(cs))
	return cs
}

// rowCommit is a fully validated next state for one row.
type rowCommit struct {
	key         string
	before      *row // stored row, nil when absent
	after       *row
	userChanges []*btpb.Mutation
	gcChanges   []*btpb.Mutation
}

// stage loads (or reuses a staged candidate for) key, applies mutate to a
// private copy, applies the table's GC policy, and drops cells of deleted
// families. Nothing is persisted. tbl.mu must be held.
func (s *server) stage(ctx context.Context, tbl *table, staged map[string]*rowCommit, key string,
	mutate func(r *row) ([]*btpb.Mutation, error)) (*rowCommit, error) {
	var base *row
	var before *row
	if prev, ok := staged[key]; ok {
		base, before = prev.after, prev.before
	} else {
		stored, err := tbl.rows.load(ctx, nil, key)
		if err != nil {
			return nil, storageErr(err)
		}
		before = stored
		base = stored
	}
	candidate := newRow(key)
	if base != nil {
		candidate = base.copy()
	}
	changes, err := mutate(candidate)
	if err != nil {
		return nil, err
	}
	fams := tbl.families
	for name := range candidate.families {
		if _, ok := fams[name]; !ok {
			delete(candidate.families, name)
		}
	}
	gcChanges := candidate.gcWithChanges(tbl.gcRulesNoLock())
	candidate.normalize()

	commit := &rowCommit{key: key, before: before, after: candidate}
	if prev, ok := staged[key]; ok {
		commit.userChanges = append(append([]*btpb.Mutation(nil), prev.userChanges...), changes...)
		commit.gcChanges = append(append([]*btpb.Mutation(nil), prev.gcChanges...), gcChanges...)
	} else {
		commit.userChanges = changes
		commit.gcChanges = gcChanges
	}
	staged[key] = commit
	return commit, nil
}

// persist writes staged rows and their change records in one transaction.
// tbl.mu must be held. On error nothing is written.
func (s *server) persist(ctx context.Context, tbl *table, commits []*rowCommit) error {
	return s.persistWith(ctx, tbl, commits, nil)
}

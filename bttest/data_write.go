package bttest

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	statpb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// idempotencyWindow is how long Bigtable deduplicates retried aggregate
// mutations that carry the same Idempotency token.
const idempotencyWindow = 15 * time.Minute

func validateIdempotency(idem *btpb.Idempotency) error {
	if idem == nil || len(idem.GetToken()) == 0 {
		return nil
	}
	if len(idem.GetToken()) < 8 {
		return status.Error(codes.InvalidArgument, "idempotency token must be at least 8 bytes long")
	}
	if st := idem.GetStartTime(); st != nil && (st.GetSeconds() != 0 || st.GetNanos() != 0) {
		if time.Since(st.AsTime()) > idempotencyWindow {
			return status.Errorf(codes.FailedPrecondition, "idempotency protection for this mutation (first attempted %v) has expired", st.AsTime())
		}
	}
	return nil
}

// idempotencySeen reports whether a mutation with this token was already
// applied to the row inside the deduplication window.
func (s *server) idempotencySeen(ctx context.Context, tableName string, key []byte, idem *btpb.Idempotency) (bool, error) {
	if len(idem.GetToken()) == 0 {
		return false, nil
	}
	var expire int64
	err := s.db.QueryRowContext(ctx,
		bind("SELECT expire_micros FROM idempotency_t WHERE table_name = ? AND row_key = ? AND token = ?"),
		tableName, key, idem.GetToken()).Scan(&expire)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, status.Errorf(codes.Internal, "read idempotency token: %v", err)
	}
	return expire > time.Now().UnixMicro(), nil
}

func recordIdempotency(ctx context.Context, q sqlExecutor, tableName string, key []byte, idem *btpb.Idempotency) error {
	if len(idem.GetToken()) == 0 {
		return nil
	}
	expire := time.Now().Add(idempotencyWindow).UnixMicro()
	_, err := q.ExecContext(ctx,
		bind("INSERT INTO idempotency_t (table_name, row_key, token, expire_micros) VALUES (?, ?, ?, ?) ON CONFLICT (table_name, row_key, token) DO UPDATE SET expire_micros = ?"),
		tableName, key, idem.GetToken(), expire, expire)
	return err
}

func (s *server) MutateRow(ctx context.Context, req *btpb.MutateRowRequest) (*btpb.MutateRowResponse, error) {
	if len(req.GetMutations()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "No mutations provided")
	}
	target, err := s.resolveWriteTarget(req.GetTableName(), req.GetAuthorizedViewName(), req.GetAppProfileId(), false)
	if err != nil {
		return nil, err
	}
	if err := validateRowKey(req.GetRowKey()); err != nil {
		return nil, err
	}
	if err := validateIdempotency(req.GetIdempotency()); err != nil {
		return nil, err
	}
	if target.view != nil {
		if err := target.view.checkMutations(string(req.RowKey), req.Mutations); err != nil {
			return nil, err
		}
	}
	tbl := target.tbl
	tbl.mu.Lock()
	if seen, err := s.idempotencySeen(ctx, tbl.name(), req.RowKey, req.GetIdempotency()); err != nil || seen {
		tbl.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &btpb.MutateRowResponse{}, nil
	}
	fams, g := tbl.families, tableGranularity(tbl)
	if err := validateMutations(req.Mutations, fams, g); err != nil {
		tbl.mu.Unlock()
		return nil, err
	}
	now := newTimestamp()
	staged := map[string]*rowCommit{}
	commit, err := s.stage(ctx, tbl, staged, string(req.RowKey), func(r *row) ([]*btpb.Mutation, error) {
		return applyMutations(r, req.Mutations, fams, g, now)
	})
	if err == nil {
		err = s.persistWith(ctx, tbl, []*rowCommit{commit}, func(q sqlExecutor) error {
			return recordIdempotency(ctx, q, tbl.name(), req.RowKey, req.GetIdempotency())
		})
	}
	tbl.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.afterCommit(ctx, tbl, []*rowCommit{commit})
	return &btpb.MutateRowResponse{}, nil
}

// MutateRows applies each entry atomically; entries are independent. Entry
// failures are reported with their own status codes in one response.
func (s *server) MutateRows(req *btpb.MutateRowsRequest, stream btpb.Bigtable_MutateRowsServer) error {
	ctx := stream.Context()
	nMutations := 0
	for _, entry := range req.Entries {
		nMutations += len(entry.Mutations)
	}
	if nMutations == 0 {
		return status.Error(codes.InvalidArgument, "No mutations provided")
	}
	if nMutations > maxMutationsPerRequest {
		return status.Errorf(codes.InvalidArgument, "too many mutations: %d (maximum %d)", nMutations, maxMutationsPerRequest)
	}
	target, err := s.resolveWriteTarget(req.GetTableName(), req.GetAuthorizedViewName(), req.GetAppProfileId(), false)
	if err != nil {
		return err
	}
	tbl := target.tbl
	res := &btpb.MutateRowsResponse{Entries: make([]*btpb.MutateRowsResponse_Entry, len(req.Entries))}
	setStatus := func(i int, err error) {
		st := status.Convert(err)
		res.Entries[i] = &btpb.MutateRowsResponse_Entry{Index: int64(i), Status: &statpb.Status{Code: int32(st.Code()), Message: st.Message()}}
	}

	tbl.mu.Lock()
	fams, g := tbl.families, tableGranularity(tbl)
	now := newTimestamp()
	staged := map[string]*rowCommit{}
	var order []*rowCommit
	entryCommit := make([]*rowCommit, len(req.Entries))
	batchTokens := map[string]bool{}
	for i, entry := range req.Entries {
		err := validateRowKey(entry.RowKey)
		if err == nil {
			err = validateIdempotency(entry.GetIdempotency())
		}
		if err == nil && target.view != nil {
			err = target.view.checkMutations(string(entry.RowKey), entry.Mutations)
		}
		if err == nil {
			err = validateMutations(entry.Mutations, fams, g)
		}
		if err == nil {
			var seen bool
			batchKey := string(entry.RowKey) + "\x00" + string(entry.GetIdempotency().GetToken())
			if len(entry.GetIdempotency().GetToken()) > 0 && batchTokens[batchKey] {
				setStatus(i, nil) // a retry of an entry earlier in this batch
				continue
			}
			if seen, err = s.idempotencySeen(ctx, tbl.name(), entry.RowKey, entry.GetIdempotency()); err == nil && seen {
				setStatus(i, nil)
				continue
			}
			if len(entry.GetIdempotency().GetToken()) > 0 {
				batchTokens[batchKey] = true
			}
		}
		if err != nil {
			setStatus(i, err)
			continue
		}
		muts := entry.Mutations
		_, existed := staged[string(entry.RowKey)]
		c, err := s.stage(ctx, tbl, staged, string(entry.RowKey), func(r *row) ([]*btpb.Mutation, error) {
			return applyMutations(r, muts, fams, g, now)
		})
		if err != nil {
			setStatus(i, err)
			continue
		}
		if !existed {
			order = append(order, c)
		}
		entryCommit[i] = c
		setStatus(i, nil)
	}
	// Persist the final staged state of each row once.
	final := make([]*rowCommit, 0, len(order))
	for _, c := range order {
		final = append(final, staged[c.key])
	}
	err = s.persistWith(ctx, tbl, final, func(q sqlExecutor) error {
		for i, entry := range req.Entries {
			if entryCommit[i] == nil {
				continue
			}
			if err := recordIdempotency(ctx, q, tbl.name(), entry.RowKey, entry.GetIdempotency()); err != nil {
				return err
			}
		}
		return nil
	})
	tbl.mu.Unlock()
	if err != nil {
		// Nothing was written; every entry that would have been applied failed.
		for i := range req.Entries {
			if entryCommit[i] != nil {
				setStatus(i, err)
			}
		}
	} else {
		s.afterCommit(ctx, tbl, final)
	}
	return stream.Send(res)
}

func (s *server) CheckAndMutateRow(ctx context.Context, req *btpb.CheckAndMutateRowRequest) (*btpb.CheckAndMutateRowResponse, error) {
	target, err := s.resolveWriteTarget(req.GetTableName(), req.GetAuthorizedViewName(), req.GetAppProfileId(), true)
	if err != nil {
		return nil, err
	}
	if err := validateRowKey(req.GetRowKey()); err != nil {
		return nil, err
	}
	if len(req.TrueMutations) == 0 && len(req.FalseMutations) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one of true_mutations or false_mutations must be provided")
	}
	if err := validateFilter(req.PredicateFilter); err != nil {
		return nil, err
	}
	if target.view != nil {
		for _, muts := range [][]*btpb.Mutation{req.TrueMutations, req.FalseMutations} {
			if len(muts) == 0 {
				continue
			}
			if err := target.view.checkMutations(string(req.RowKey), muts); err != nil {
				return nil, err
			}
		}
	}
	tbl := target.tbl
	tbl.mu.Lock()
	fams, g := tbl.families, tableGranularity(tbl)
	now := newTimestamp()
	matched := false
	staged := map[string]*rowCommit{}
	commit, err := s.stage(ctx, tbl, staged, string(req.RowKey), func(r *row) ([]*btpb.Mutation, error) {
		visible := r
		if target.view != nil {
			visible = target.view.restrict(r)
		}
		if req.PredicateFilter == nil {
			matched = !visible.isEmpty()
		} else {
			candidate := visible.copy()
			candidate.gc(tbl.gcRulesNoLock())
			ok, err := filterRow(req.PredicateFilter, candidate)
			if err != nil {
				return nil, err
			}
			matched = ok
		}
		muts := req.FalseMutations
		if matched {
			muts = req.TrueMutations
		}
		if len(muts) == 0 {
			return nil, nil
		}
		// Only the selected branch is validated and applied.
		if err := validateMutations(muts, fams, g); err != nil {
			return nil, err
		}
		return applyMutations(r, muts, fams, g, now)
	})
	if err == nil && len(commit.userChanges) > 0 {
		err = s.persist(ctx, tbl, []*rowCommit{commit})
	}
	tbl.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(commit.userChanges) > 0 {
		s.afterCommit(ctx, tbl, []*rowCommit{commit})
	}
	return &btpb.CheckAndMutateRowResponse{PredicateMatched: matched}, nil
}

func (s *server) ReadModifyWriteRow(ctx context.Context, req *btpb.ReadModifyWriteRowRequest) (*btpb.ReadModifyWriteRowResponse, error) {
	target, err := s.resolveWriteTarget(req.GetTableName(), req.GetAuthorizedViewName(), req.GetAppProfileId(), true)
	if err != nil {
		return nil, err
	}
	if err := validateRowKey(req.GetRowKey()); err != nil {
		return nil, err
	}
	if len(req.Rules) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one rule must be provided")
	}
	if len(req.Rules) > maxMutationsPerRequest {
		return nil, status.Errorf(codes.InvalidArgument, "too many rules: %d (maximum %d)", len(req.Rules), maxMutationsPerRequest)
	}
	if target.view != nil {
		if err := target.view.checkRules(string(req.RowKey), req.Rules); err != nil {
			return nil, err
		}
	}
	tbl := target.tbl
	tbl.mu.Lock()
	fams := tbl.families
	for i, rule := range req.Rules {
		cf, ok := fams[rule.GetFamilyName()]
		if !ok {
			tbl.mu.Unlock()
			return nil, unknownFamilyError(rule.GetFamilyName())
		}
		if familyAggregate(cf) != aggregateNone {
			tbl.mu.Unlock()
			return nil, status.Errorf(codes.InvalidArgument, "rule %d: ReadModifyWrite is not supported on aggregate column family %q", i, rule.GetFamilyName())
		}
		switch rule.GetRule().(type) {
		case *btpb.ReadModifyWriteRule_AppendValue, *btpb.ReadModifyWriteRule_IncrementAmount:
		default:
			tbl.mu.Unlock()
			return nil, status.Errorf(codes.InvalidArgument, "rule %d: rule must set append_value or increment_amount", i)
		}
	}

	result := newRow(string(req.RowKey))
	staged := map[string]*rowCommit{}
	commit, err := s.stage(ctx, tbl, staged, string(req.RowKey), func(r *row) ([]*btpb.Mutation, error) {
		// Rules read the cells as visible after GC, then apply in order; a
		// later rule observes the result of an earlier rule on the same cell.
		// Read from a GC'd copy; stage() garbage-collects r itself so the
		// removed cells are reported to change streams.
		visible := r.copy()
		visible.gc(tbl.gcRulesNoLock())
		var changes []*btpb.Mutation
		for i, rule := range req.Rules {
			fam, col := rule.FamilyName, string(rule.ColumnQualifier)
			f := r.getOrCreateFamily(fam, fams[fam].Order)
			vf := visible.getOrCreateFamily(fam, fams[fam].Order)
			cs := vf.cellsByColumn(col)
			ts := newTimestamp()
			var prev cell
			if len(cs) > 0 {
				prev = cs[0]
				ts = max(ts, prev.Ts)
			}
			var value []byte
			switch rule := rule.Rule.(type) {
			case *btpb.ReadModifyWriteRule_AppendValue:
				value = append(append([]byte(nil), prev.Value...), rule.AppendValue...)
			case *btpb.ReadModifyWriteRule_IncrementAmount:
				var v int64
				if len(cs) > 0 {
					if len(prev.Value) != 8 {
						return nil, status.Errorf(codes.FailedPrecondition, "rule %d: cannot increment %s:%s because its value is %d bytes, not a 64-bit big-endian integer", i, fam, col, len(prev.Value))
					}
					v = int64(binary.BigEndian.Uint64(prev.Value))
				}
				value = encodeInt64(v + rule.IncrementAmount)
			}
			newCell := cell{Ts: ts, Value: value}
			f.Cells[col] = appendOrReplaceCell(f.cellsByColumn(col), newCell)
			vf.Cells[col] = appendOrReplaceCell(cs, newCell)
			rf := result.getOrCreateFamily(fam, fams[fam].Order)
			rf.cellsByColumn(col)
			rf.Cells[col] = []cell{newCell}
			changes = append(changes, &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
				FamilyName: fam, ColumnQualifier: []byte(col), TimestampMicros: ts, Value: value,
			}}})
		}
		return changes, nil
	})
	if err == nil {
		err = s.persist(ctx, tbl, []*rowCommit{commit})
	}
	tbl.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.afterCommit(ctx, tbl, []*rowCommit{commit})

	res := &btpb.Row{Key: req.RowKey}
	for _, family := range result.sortedFamilies() {
		f := &btpb.Family{Name: family.Name}
		for _, colName := range family.ColNames {
			c := family.Cells[colName][0]
			f.Columns = append(f.Columns, &btpb.Column{
				Qualifier: []byte(colName),
				Cells:     []*btpb.Cell{{TimestampMicros: c.Ts, Value: c.Value}},
			})
		}
		res.Families = append(res.Families, f)
	}
	return &btpb.ReadModifyWriteRowResponse{Row: res}, nil
}

// persistWith persists commits and runs extra in the same transaction.
func (s *server) persistWith(ctx context.Context, tbl *table, commits []*rowCommit, extra func(q sqlExecutor) error) error {
	if len(commits) == 0 {
		return nil
	}
	commitMicros := time.Now().UnixMicro()
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := s.writeCommits(ctx, tx, tbl, commits, commitMicros); err != nil {
			return err
		}
		if extra != nil {
			return extra(tx)
		}
		return nil
	})
	if err != nil {
		return storageErr(fmt.Errorf("persist rows: %w", err))
	}
	return nil
}

func (s *server) writeCommits(ctx context.Context, q sqlExecutor, tbl *table, commits []*rowCommit, commitMicros int64) error {
	for _, c := range commits {
		if err := tbl.rows.save(ctx, q, c.after); err != nil {
			return err
		}
		if !tbl.changeStreamEnabled() {
			continue
		}
		if len(c.userChanges) > 0 {
			if err := s.changeLog.append(ctx, q, tbl.name(), c.key, btpb.ReadChangeStreamResponse_DataChange_USER, commitMicros, c.userChanges); err != nil {
				return err
			}
		}
		if len(c.gcChanges) > 0 {
			if err := s.changeLog.append(ctx, q, tbl.name(), c.key, btpb.ReadChangeStreamResponse_DataChange_GARBAGE_COLLECTION, commitMicros, c.gcChanges); err != nil {
				return err
			}
		}
	}
	return nil
}

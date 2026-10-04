package gsql

import (
	"context"
	"sort"
)

type execEnv struct {
	ctx        context.Context
	cat        Catalog
	params     []any
	now        tsVal
	noPushdown bool
}

func (p *queryPlan) limits(ec *evalCtx) (limit, offset int64, err error) {
	limit = -1
	if p.limit != nil {
		v, err := p.limit.eval(ec)
		if err != nil {
			return 0, 0, err
		}
		if v == nil || v.(int64) < 0 {
			return 0, 0, invalidf("LIMIT must be a non-negative, non-NULL INT64")
		}
		limit = v.(int64)
	}
	if p.offset != nil {
		v, err := p.offset.eval(ec)
		if err != nil {
			return 0, 0, err
		}
		if v == nil || v.(int64) < 0 {
			return 0, 0, invalidf("OFFSET must be a non-negative, non-NULL INT64")
		}
		offset = v.(int64)
	}
	return limit, offset, nil
}

// run executes the plan, calling emit for each output row in order.
func (p *queryPlan) run(env *execEnv, emit func([]any) error) error {
	ec := &evalCtx{params: env.params, now: env.now}
	limit, offset, err := p.limits(ec)
	if err != nil {
		return err
	}
	out := &sink{p: p, limit: limit, offset: offset, emit: emit}
	out.sorting = (len(p.order) > 0 && !p.orderNoop) || p.mv != nil
	if p.distinct {
		out.seen = map[string]bool{}
	}
	if limit == 0 {
		return nil
	}
	skipErr := func(err error) bool { return p.mv != nil && isEvalError(err) }

	if !p.aggregated {
		err = p.scanSource(env, ec, func(r rowAccessor) (bool, error) {
			ec.row = r
			if p.where != nil {
				v, err := p.where.eval(ec)
				if err != nil {
					if skipErr(err) {
						return true, nil
					}
					return false, err
				}
				if v != true {
					return true, nil
				}
			}
			vals, keys, err := p.project(ec)
			if err != nil {
				if skipErr(err) {
					return true, nil
				}
				return false, err
			}
			return out.add(vals, keys)
		})
		if err != nil {
			return err
		}
		return out.finish()
	}

	groups := map[string]*group{}
	var order []*group
	aggLimits := make([]int64, len(p.aggs))
	for i, s := range p.aggs {
		aggLimits[i] = -1
		if s.limit != nil {
			v, err := s.limit.eval(ec)
			if err != nil {
				return err
			}
			if v == nil || v.(int64) < 0 {
				return invalidf("LIMIT in aggregate function %s must be a non-negative INT64", s.name)
			}
			aggLimits[i] = v.(int64)
		}
	}
	newGroup := func(keys []any) *group {
		g := &group{keys: keys, runners: make([]*aggRunner, len(p.aggs))}
		for i, s := range p.aggs {
			g.runners[i] = newAggRunner(s, aggLimits[i])
		}
		return g
	}
	argBuf := make([][]any, len(p.aggs))
	keyBuf := make([][]any, len(p.aggs))
	err = p.scanSource(env, ec, func(r rowAccessor) (bool, error) {
		ec.row = r
		if p.where != nil {
			v, err := p.where.eval(ec)
			if err != nil {
				if skipErr(err) {
					return true, nil
				}
				return false, err
			}
			if v != true {
				return true, nil
			}
		}
		keys := make([]any, len(p.groupKeys))
		for i, k := range p.groupKeys {
			v, err := k.eval(ec)
			if err != nil {
				if skipErr(err) {
					return true, nil
				}
				return false, err
			}
			keys[i] = v
		}
		for i, s := range p.aggs {
			args := make([]any, len(s.args))
			for j, a := range s.args {
				v, err := a.eval(ec)
				if err != nil {
					if skipErr(err) {
						return true, nil
					}
					return false, err
				}
				args[j] = v
			}
			var okeys []any
			if len(s.orderBy) > 0 {
				okeys = make([]any, len(s.orderBy))
				for j, o := range s.orderBy {
					v, err := o.expr.eval(ec)
					if err != nil {
						if skipErr(err) {
							return true, nil
						}
						return false, err
					}
					okeys[j] = v
				}
			}
			argBuf[i], keyBuf[i] = args, okeys
		}
		var kb []byte
		for _, k := range keys {
			kb = appendValueKey(kb, k)
		}
		g, ok := groups[string(kb)]
		if !ok {
			g = newGroup(keys)
			groups[string(kb)] = g
			order = append(order, g)
		}
		for i, rn := range g.runners {
			if err := rn.add(argBuf[i], keyBuf[i]); err != nil {
				if skipErr(err) {
					continue
				}
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if len(order) == 0 && len(p.groupKeys) == 0 {
		order = append(order, newGroup(nil))
	}
	ec.row = nil
	for _, g := range order {
		ec.group = g.keys
		ec.aggs = make([]any, len(g.runners))
		failed := false
		for i, rn := range g.runners {
			v, err := rn.result()
			if err != nil {
				if skipErr(err) {
					failed = true
					break
				}
				return err
			}
			ec.aggs[i] = v
		}
		if failed {
			continue
		}
		if p.having != nil {
			v, err := p.having.eval(ec)
			if err != nil {
				if skipErr(err) {
					continue
				}
				return err
			}
			if v != true {
				continue
			}
		}
		vals, keys, err := p.project(ec)
		if err != nil {
			if skipErr(err) {
				continue
			}
			return err
		}
		more, err := out.add(vals, keys)
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	return out.finish()
}

type group struct {
	keys    []any
	runners []*aggRunner
}

func (p *queryPlan) project(ec *evalCtx) ([]any, []any, error) {
	vals := make([]any, len(p.outExprs))
	for i, e := range p.outExprs {
		v, err := e.eval(ec)
		if err != nil {
			return nil, nil, err
		}
		vals[i] = v
	}
	var keys []any
	if len(p.order) > 0 && !p.orderNoop {
		keys = make([]any, len(p.order))
		for i, o := range p.order {
			v, err := o.expr.eval(ec)
			if err != nil {
				return nil, nil, err
			}
			keys[i] = v
		}
	}
	return vals, keys, nil
}

func (p *queryPlan) scanSource(env *execEnv, ec *evalCtx, fn func(rowAccessor) (bool, error)) error {
	switch src := p.src.(type) {
	case nil:
		_, err := fn(emptyRow{})
		return err
	case *tableSource:
		return src.scan(env, ec, p.keyRanges(env, ec), fn)
	default:
		return src.scan(env, ec, nil, fn)
	}
}

// sink applies DISTINCT, ORDER BY, OFFSET and LIMIT to output rows.
type sink struct {
	p       *queryPlan
	limit   int64
	offset  int64
	emit    func([]any) error
	seen    map[string]bool
	sorting bool
	rows    []sinkRow
	skipped int64
	emitted int64
}

type sinkRow struct {
	vals []any
	keys []any
}

func (s *sink) add(vals, keys []any) (bool, error) {
	if s.p.mv != nil {
		if !s.p.mv.accept(vals) {
			return true, nil
		}
	}
	if s.seen != nil {
		var kb []byte
		for _, v := range vals {
			kb = appendValueKey(kb, v)
		}
		if s.seen[string(kb)] {
			return true, nil
		}
		s.seen[string(kb)] = true
	}
	if s.sorting {
		s.rows = append(s.rows, sinkRow{vals: vals, keys: keys})
		return true, nil
	}
	return s.push(vals)
}

func (s *sink) push(vals []any) (bool, error) {
	if s.skipped < s.offset {
		s.skipped++
		return true, nil
	}
	if s.limit >= 0 && s.emitted >= s.limit {
		return false, nil
	}
	if err := s.emit(vals); err != nil {
		return false, err
	}
	s.emitted++
	return s.limit < 0 || s.emitted < s.limit, nil
}

func (s *sink) finish() error {
	if !s.sorting {
		return nil
	}
	if s.p.mv != nil {
		mv := s.p.mv
		sort.SliceStable(s.rows, func(i, j int) bool { return mv.less(s.rows[i].vals, s.rows[j].vals) })
	} else {
		specs := s.p.order
		sort.SliceStable(s.rows, func(i, j int) bool { return compareOrder(specs, s.rows[i].keys, s.rows[j].keys) < 0 })
	}
	for _, r := range s.rows {
		more, err := s.push(r.vals)
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	s.rows = nil
	return nil
}

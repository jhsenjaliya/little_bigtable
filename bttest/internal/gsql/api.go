// Package gsql is a self-contained GoogleSQL-for-Bigtable query engine used
// by the Bigtable emulator. It parses, type-checks and executes SELECT
// queries (including continuous materialized view definitions) against rows
// provided by a Catalog, and produces results in the Bigtable Data API wire
// format (btpb.Value / btpb.ResultSetMetadata).
//
// All errors returned by Prepare, Execute and PrepareMaterializedView are
// gRPC status errors:
//
//   - InvalidArgument for syntax, type and semantic errors, bad parameter
//     values, and constructs GoogleSQL for Bigtable itself does not support
//     (JOIN, UNION, subqueries, CTEs, DML/DDL).
//   - NotFound for unknown tables and views.
//   - Unimplemented for valid GoogleSQL for Bigtable that the emulator does
//     not implement (window functions, geography, approximate quantiles, ...).
//   - OutOfRange for runtime evaluation errors (division by zero, overflow,
//     invalid casts, array index out of bounds), as in ZetaSQL.
package gsql

import (
	"context"
	"strings"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/protobuf/proto"
)

// Cell is one stored cell. Families hold cells newest first.
type Cell struct {
	TimestampMicros int64
	Value           []byte
}

// Row is one Bigtable row: family -> qualifier -> cells (descending timestamp).
type Row struct {
	Key      []byte
	Families map[string]map[string][]Cell
}

// FamilyKind describes how a column family's cells are typed in SQL.
type FamilyKind int

const (
	// FamilyBytes holds plain bytes cells (MAP<BYTES, BYTES>).
	FamilyBytes FamilyKind = iota
	// FamilyInt64Aggregate is a sum/min/max aggregate family whose cell values
	// are 8-byte big-endian int64 (MAP<BYTES, INT64>).
	FamilyInt64Aggregate
	// FamilyHLLAggregate is an HLL++ aggregate family whose cell values are
	// sketches encoded by this package (MAP<BYTES, BYTES>; see HLL).
	FamilyHLLAggregate
)

// Family is a column family of a table.
type Family struct {
	Name string
	Kind FamilyKind
}

// Table describes a queryable table.
type Table struct {
	Name         string            // the ID used in FROM
	Families     []Family          // stable order; SELECT * returns _key, key schema fields, then families in this order
	RowKeySchema *btpb.Type_Struct // optional structured row key; nil means none
}

// Catalog resolves names and reads rows. Implemented by the emulator.
type Catalog interface {
	LookupTable(name string) (*Table, bool)
	// LookupView returns the SQL text of a logical view or materialized view by ID.
	LookupView(name string) (query string, ok bool)
	// Scan visits rows of table with keys in [start, end) in ascending key
	// order; nil/empty bounds are unbounded. fn returns false to stop.
	Scan(ctx context.Context, table string, start, end []byte, fn func(Row) bool) error
}

// MaterializedViewCatalog may optionally be implemented by a Catalog to tell
// materialized views apart from logical views. Queries over a materialized
// view see the view's rows in key order with the view's row exclusion rules
// (NULL key columns, all-NULL values) and a hidden _key column. Without this
// interface, a view whose SQL is only valid as a materialized-view definition
// (e.g. a secondary index ORDER BY non-key columns) is treated as one.
type MaterializedViewCatalog interface {
	Catalog
	IsMaterializedView(name string) bool
}

// Prepared is a parsed, type-checked query.
type Prepared struct {
	query      string
	plan       *queryPlan
	params     []paramInfo
	paramTypes map[string]*btpb.Type
	meta       *btpb.ResultSetMetadata
	types      []*sqlType
	noPushdown bool // testing: disable key-range pushdown
}

func newPrepared(query string, plan *queryPlan, params []paramInfo, paramTypes map[string]*btpb.Type) *Prepared {
	p := &Prepared{query: query, plan: plan, params: params, paramTypes: paramTypes}
	cols := make([]*btpb.ColumnMetadata, len(plan.outCols))
	p.types = make([]*sqlType, len(plan.outCols))
	for i, c := range plan.outCols {
		cols[i] = &btpb.ColumnMetadata{Name: c.name, Type: c.typ.toProto()}
		p.types[i] = c.typ
	}
	p.meta = &btpb.ResultSetMetadata{Schema: &btpb.ResultSetMetadata_ProtoSchema{ProtoSchema: &btpb.ProtoSchema{Columns: cols}}}
	return p
}

// Prepare parses and type-checks a query. paramTypes declares the types of
// @parameters (encodings are ignored).
func Prepare(query string, paramTypes map[string]*btpb.Type, cat Catalog) (*Prepared, error) {
	if cat == nil {
		return nil, internalf("nil catalog")
	}
	q, hints, err := parseStatement(query)
	if err != nil {
		return nil, err
	}
	for _, h := range hints {
		if !strings.EqualFold(h.name, "allow_incomplete_view") {
			return nil, unimplementedf("statement hint %s is not supported by the emulator", h.name)
		}
	}
	a := &analyzer{cat: cat, params: map[string]int{}}
	names := make([]string, 0, len(paramTypes))
	for n := range paramTypes {
		names = append(names, n)
	}
	sortStrings(names)
	for _, n := range names {
		t, err := typeFromProto(paramTypes[n])
		if err != nil {
			return nil, invalidf("invalid type for parameter %s: %s", n, statusMessage(err))
		}
		a.params[n] = len(a.paramList)
		a.paramList = append(a.paramList, paramInfo{name: n, typ: t})
	}
	plan, err := a.analyzeQuery(q)
	if err != nil {
		return nil, err
	}
	pt := make(map[string]*btpb.Type, len(paramTypes))
	for n, t := range paramTypes {
		pt[n] = proto.Clone(t).(*btpb.Type)
	}
	return newPrepared(query, plan, a.paramList, pt), nil
}

// Metadata returns the result schema. Types never carry encodings.
func (p *Prepared) Metadata() *btpb.ResultSetMetadata {
	return proto.Clone(p.meta).(*btpb.ResultSetMetadata)
}

// ParamTypes returns the declared parameter types.
func (p *Prepared) ParamTypes() map[string]*btpb.Type {
	out := make(map[string]*btpb.Type, len(p.paramTypes))
	for n, t := range p.paramTypes {
		out[n] = proto.Clone(t).(*btpb.Type)
	}
	return out
}

func (p *Prepared) bindParams(params map[string]*btpb.Value) ([]any, error) {
	vals := make([]any, len(p.params))
	for name := range params {
		found := false
		for _, pi := range p.params {
			if pi.name == name {
				found = true
				break
			}
		}
		if !found {
			return nil, invalidf("parameter %s was not declared when the query was prepared", name)
		}
	}
	for i, pi := range p.params {
		pv, ok := params[pi.name]
		if !ok {
			return nil, invalidf("parameter %s is not bound", pi.name)
		}
		if pv.GetType() != nil {
			t, err := typeFromProto(pv.GetType())
			if err != nil {
				return nil, invalidf("invalid type for parameter %s: %s", pi.name, statusMessage(err))
			}
			if !typesEqual(t, pi.typ) {
				return nil, invalidf("parameter %s has type %s but was declared as %s", pi.name, t, pi.typ)
			}
		}
		v, err := fromProtoValue(pv, pi.typ, false)
		if err != nil {
			return nil, invalidf("invalid value for parameter %s: %s", pi.name, statusMessage(err))
		}
		vals[i] = v
	}
	return vals, nil
}

// Execute runs the query. emit receives one row (one value per column) at a
// time, in result order; an error returned by emit stops execution and is
// returned unchanged.
func (p *Prepared) Execute(ctx context.Context, cat Catalog, params map[string]*btpb.Value, emit func(row []*btpb.Value) error) error {
	if cat == nil {
		return internalf("nil catalog")
	}
	vals, err := p.bindParams(params)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	env := &execEnv{ctx: ctx, cat: cat, params: vals, now: tsVal(time.Now().UnixMicro()), noPushdown: p.noPushdown}
	return p.plan.run(env, func(row []any) error {
		out := make([]*btpb.Value, len(row))
		for i, v := range row {
			out[i] = toProtoValue(v, p.types[i])
		}
		return emit(out)
	})
}

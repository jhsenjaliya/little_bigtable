package bttest

// Conformance tests for authorized views: Data API restrictions
// (https://cloud.google.com/bigtable/docs/authorized-views, writes.txt
// "Authorized view definition limitations": writes outside the view are
// PERMISSION_DENIED) and the admin surface (ResponseView defaults, etags,
// pagination, deletion protection) from google/bigtable/admin/v2.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type ctgAVEnv struct {
	*testEnv
	tableAdmin    btapb.BigtableTableAdminClient
	instanceAdmin btapb.BigtableInstanceAdminClient
	table         string // table ID
	tableName     string // full table resource name
}

func (e *ctgAVEnv) viewName(id string) string { return e.tableName + "/authorizedViews/" + id }

// ctgNewAVEnv creates table "av" (families cf1, cf2, cf3) holding:
//
//	user#1: cf1:a=1 cf1:b=2 cf1:pfx-x=3 cf2:c=4 cf3:z=5
//	user#2: cf1:a=6
//	user#9: cf3:z=9
//	other#1: cf1:a=7
//
// and authorized view "restricted" covering row prefix "user#", cf1 qualifier
// "a" plus qualifier prefix "pfx-", and all of cf2.
func ctgNewAVEnv(t *testing.T) *ctgAVEnv {
	t.Helper()
	env := setupTestEnv(t)
	t.Cleanup(env.cancel)
	conn := ctgDial(t, env.ctx)
	e := &ctgAVEnv{
		testEnv:       env,
		tableAdmin:    btapb.NewBigtableTableAdminClient(conn),
		instanceAdmin: btapb.NewBigtableInstanceAdminClient(conn),
		table:         "av",
		tableName:     fmt.Sprintf("projects/%s/instances/%s/tables/av", env.projectID, env.instanceID),
	}
	tbl := env.createTable(t, e.table, "cf1", "cf2", "cf3")
	cells := map[string][][3]string{
		"user#1":  {{"cf1", "a", "1"}, {"cf1", "b", "2"}, {"cf1", "pfx-x", "3"}, {"cf2", "c", "4"}, {"cf3", "z", "5"}},
		"user#2":  {{"cf1", "a", "6"}},
		"user#9":  {{"cf3", "z", "9"}},
		"other#1": {{"cf1", "a", "7"}},
	}
	for key, cs := range cells {
		m := bigtable.NewMutation()
		for _, c := range cs {
			m.Set(c[0], c[1], 1000, []byte(c[2]))
		}
		require.NoError(t, tbl.Apply(env.ctx, key, m))
	}
	require.NoError(t, env.admin.CreateAuthorizedView(env.ctx, &bigtable.AuthorizedViewConf{
		TableID: e.table, AuthorizedViewID: "restricted",
		AuthorizedView: &bigtable.SubsetViewConf{
			RowPrefixes: [][]byte{[]byte("user#")},
			FamilySubsets: map[string]bigtable.FamilySubset{
				"cf1": {Qualifiers: [][]byte{[]byte("a")}, QualifierPrefixes: [][]byte{[]byte("pfx-")}},
				"cf2": {QualifierPrefixes: [][]byte{[]byte("")}},
			},
		},
	}))
	return e
}

// ctgDump reads rs through tbl and renders "key fam:col=value" lines.
func ctgDump(t *testing.T, ctx context.Context, tbl bigtable.TableAPI, rs bigtable.RowSet, opts ...bigtable.ReadOption) []string {
	t.Helper()
	var out []string
	require.NoError(t, tbl.ReadRows(ctx, rs, func(r bigtable.Row) bool {
		for _, items := range r {
			for _, it := range items {
				out = append(out, fmt.Sprintf("%s %s=%s", r.Key(), it.Column, it.Value))
			}
		}
		return true
	}, opts...))
	sort.Strings(out)
	return out
}

func TestConformanceAuthorizedViewReadRestriction(t *testing.T) {
	e := ctgNewAVEnv(t)
	ctx := e.ctx
	av := e.client.OpenAuthorizedView(e.table, "restricted")

	// Only rows under "user#" and only cells in the family subsets; rows with
	// no visible cells (user#9) are omitted.
	require.Equal(t, []string{
		"user#1 cf1:a=1", "user#1 cf1:pfx-x=3", "user#1 cf2:c=4", "user#2 cf1:a=6",
	}, ctgDump(t, ctx, av, bigtable.InfiniteRange("")))

	// Point reads outside the view return nothing.
	row, err := av.ReadRow(ctx, "other#1")
	require.NoError(t, err)
	require.Empty(t, row)
	row, err = av.ReadRow(ctx, "user#9")
	require.NoError(t, err)
	require.Empty(t, row)
	require.Equal(t, []string{"user#2 cf1:a=6"}, ctgDump(t, ctx, av, bigtable.RowList{"other#1", "user#2"}))

	// Filters only see view data: cf1:b and cf3 stay invisible.
	require.Empty(t, ctgDump(t, ctx, av, bigtable.InfiniteRange(""), bigtable.RowFilter(bigtable.ColumnFilter("b|z"))))
	require.Equal(t, []string{"user#1 cf1:pfx-x=3"},
		ctgDump(t, ctx, av, bigtable.InfiniteRange(""), bigtable.RowFilter(bigtable.ColumnFilter("pfx-.*"))))

	// An empty row_prefixes list selects no rows at all, while "" selects all.
	require.NoError(t, e.admin.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{
		TableID: e.table, AuthorizedViewID: "norows",
		AuthorizedView: &bigtable.SubsetViewConf{
			FamilySubsets: map[string]bigtable.FamilySubset{"cf1": {QualifierPrefixes: [][]byte{[]byte("")}}},
		},
	}))
	norows := e.client.OpenAuthorizedView(e.table, "norows")
	require.Empty(t, ctgDump(t, ctx, norows, bigtable.InfiniteRange("")))
	row, err = norows.ReadRow(ctx, "user#1")
	require.NoError(t, err)
	require.Empty(t, row)
	require.Equal(t, codes.PermissionDenied, status.Code(norows.Apply(ctx, "user#1", ctgSet("cf1", "a", "x"))))

	require.NoError(t, e.admin.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{
		TableID: e.table, AuthorizedViewID: "cf3all",
		AuthorizedView: &bigtable.SubsetViewConf{
			RowPrefixes:   [][]byte{[]byte("")},
			FamilySubsets: map[string]bigtable.FamilySubset{"cf3": {Qualifiers: [][]byte{[]byte("z")}}},
		},
	}))
	require.Equal(t, []string{"user#1 cf3:z=5", "user#9 cf3:z=9"},
		ctgDump(t, ctx, e.client.OpenAuthorizedView(e.table, "cf3all"), bigtable.InfiniteRange("")))

	// SampleRowKeys and reads against a missing view fail with NotFound.
	_, err = av.SampleRowKeys(ctx)
	require.NoError(t, err)
	_, err = e.client.OpenAuthorizedView(e.table, "missing").ReadRow(ctx, "user#1")
	require.Equal(t, codes.NotFound, status.Code(err))
}

func ctgSet(fam, col, value string) *bigtable.Mutation {
	m := bigtable.NewMutation()
	m.Set(fam, col, 2000, []byte(value))
	return m
}

func TestConformanceAuthorizedViewWriteRestriction(t *testing.T) {
	e := ctgNewAVEnv(t)
	ctx := e.ctx
	av := e.client.OpenAuthorizedView(e.table, "restricted")
	tbl := e.client.OpenTable(e.table)
	before := ctgDump(t, ctx, tbl, bigtable.InfiniteRange(""))

	denied := map[string]struct {
		key string
		mut *bigtable.Mutation
	}{
		"row outside prefixes":     {"other#2", ctgSet("cf1", "a", "x")},
		"qualifier outside view":   {"user#1", ctgSet("cf1", "b", "x")},
		"family outside view":      {"user#1", ctgSet("cf3", "z", "x")},
		"DeleteFromRow":            {"user#1", func() *bigtable.Mutation { m := bigtable.NewMutation(); m.DeleteRow(); return m }()},
		"DeleteFromFamily partial": {"user#1", func() *bigtable.Mutation { m := bigtable.NewMutation(); m.DeleteCellsInFamily("cf1"); return m }()},
		"DeleteFromColumn outside": {"user#1", func() *bigtable.Mutation { m := bigtable.NewMutation(); m.DeleteCellsInColumn("cf1", "b"); return m }()},
		"DeleteFromFamily outside": {"user#1", func() *bigtable.Mutation { m := bigtable.NewMutation(); m.DeleteCellsInFamily("cf3"); return m }()},
		"valid then invalid in row": {"user#1", func() *bigtable.Mutation {
			m := ctgSet("cf1", "a", "new")
			m.Set("cf1", "b", 2000, []byte("x"))
			return m
		}()},
	}
	for name, tc := range denied {
		err := av.Apply(ctx, tc.key, tc.mut)
		require.Equal(t, codes.PermissionDenied, status.Code(err), "%s: %v", name, err)
	}
	require.Equal(t, before, ctgDump(t, ctx, tbl, bigtable.InfiniteRange("")), "denied writes must not change the table")

	// Writes inside the view succeed.
	require.NoError(t, av.Apply(ctx, "user#3", ctgSet("cf1", "a", "new")))
	require.NoError(t, av.Apply(ctx, "user#3", ctgSet("cf1", "pfx-y", "p")))
	m := bigtable.NewMutation()
	m.DeleteCellsInColumn("cf1", "pfx-y")
	require.NoError(t, av.Apply(ctx, "user#3", m))
	// DeleteFromFamily is allowed when the family subset has the "" prefix,
	// and only touches that family.
	m = bigtable.NewMutation()
	m.DeleteCellsInFamily("cf2")
	require.NoError(t, av.Apply(ctx, "user#1", m))

	// MutateRows reports per-entry PermissionDenied and applies the rest.
	errs, err := av.ApplyBulk(ctx, []string{"user#4", "other#4"}, []*bigtable.Mutation{ctgSet("cf1", "a", "ok"), ctgSet("cf1", "a", "no")})
	require.NoError(t, err)
	require.Len(t, errs, 2)
	require.NoError(t, errs[0])
	require.Equal(t, codes.PermissionDenied, status.Code(errs[1]))

	require.Equal(t, []string{
		"other#1 cf1:a=7",
		"user#1 cf1:a=1", "user#1 cf1:b=2", "user#1 cf1:pfx-x=3", "user#1 cf3:z=5",
		"user#2 cf1:a=6",
		"user#3 cf1:a=new",
		"user#4 cf1:a=ok",
		"user#9 cf3:z=9",
	}, ctgDump(t, ctx, tbl, bigtable.InfiniteRange("")))
}

func TestConformanceAuthorizedViewReadModifyWrite(t *testing.T) {
	e := ctgNewAVEnv(t)
	ctx := e.ctx
	av := e.client.OpenAuthorizedView(e.table, "restricted")

	rmw := func(fam, col string) *bigtable.ReadModifyWrite {
		r := bigtable.NewReadModifyWrite()
		r.AppendValue(fam, col, []byte("+"))
		return r
	}
	for name, tc := range map[string]struct{ key, fam, col string }{
		"qualifier outside view": {"user#1", "cf1", "b"},
		"family outside view":    {"user#1", "cf3", "z"},
		"row outside view":       {"other#1", "cf1", "a"},
	} {
		_, err := av.ApplyReadModifyWrite(ctx, tc.key, rmw(tc.fam, tc.col))
		require.Equal(t, codes.PermissionDenied, status.Code(err), name)
	}
	row, err := av.ApplyReadModifyWrite(ctx, "user#1", rmw("cf1", "a"))
	require.NoError(t, err)
	require.Equal(t, "1+", string(row["cf1"][0].Value))

	require.Equal(t, []string{
		"other#1 cf1:a=7", "user#1 cf1:a=1+", "user#1 cf1:b=2", "user#1 cf3:z=5",
	}, ctgDump(t, ctx, e.client.OpenTable(e.table), bigtable.RowList{"other#1", "user#1"},
		bigtable.RowFilter(bigtable.ChainFilters(bigtable.LatestNFilter(1), bigtable.ColumnFilter("a|b|z")))))
}

// The predicate of a conditional write through an authorized view is
// evaluated only against data inside the view.
func TestConformanceAuthorizedViewCheckAndMutatePredicate(t *testing.T) {
	e := ctgNewAVEnv(t)
	ctx := e.ctx
	av := e.client.OpenAuthorizedView(e.table, "restricted")
	tbl := e.client.OpenTable(e.table)

	// user#1 has cf1:b (outside the view). Through the table the predicate
	// matches; through the view it does not.
	var matched bool
	cond := bigtable.NewCondMutation(bigtable.ColumnFilter("b"), ctgSet("cf1", "a", "true-branch"), ctgSet("cf1", "a", "false-branch"))
	require.NoError(t, av.Apply(ctx, "user#1", cond, bigtable.GetCondMutationResult(&matched)))
	require.False(t, matched)
	row, err := av.ReadRow(ctx, "user#1", bigtable.RowFilter(bigtable.ChainFilters(bigtable.ColumnFilter("a"), bigtable.LatestNFilter(1))))
	require.NoError(t, err)
	require.Equal(t, "false-branch", string(row["cf1"][0].Value))

	require.NoError(t, tbl.Apply(ctx, "user#1", bigtable.NewCondMutation(bigtable.ColumnFilter("b"), ctgSet("cf1", "a", "t"), nil),
		bigtable.GetCondMutationResult(&matched)))
	require.True(t, matched)

	// user#9 only has cf3 data: a nil predicate ("row has any cell") does not
	// match through the view.
	require.NoError(t, av.Apply(ctx, "user#9", bigtable.NewCondMutation(nil, nil, ctgSet("cf1", "a", "empty-in-view")),
		bigtable.GetCondMutationResult(&matched)))
	require.False(t, matched)
	row, err = av.ReadRow(ctx, "user#9")
	require.NoError(t, err)
	require.Equal(t, "empty-in-view", string(row["cf1"][0].Value))

	// Mutations of either branch must stay inside the view.
	err = av.Apply(ctx, "user#1", bigtable.NewCondMutation(nil, ctgSet("cf1", "a", "x"), ctgSet("cf3", "z", "x")))
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	err = av.Apply(ctx, "other#1", bigtable.NewCondMutation(nil, ctgSet("cf1", "a", "x"), nil))
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestConformanceAuthorizedViewResponseViewsAndPagination(t *testing.T) {
	e := ctgNewAVEnv(t)
	ctx := e.ctx
	for _, id := range []string{"v-a", "v-b", "v-c"} {
		require.NoError(t, e.admin.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{
			TableID: e.table, AuthorizedViewID: id, DeletionProtection: bigtable.Unprotected,
			AuthorizedView: &bigtable.SubsetViewConf{RowPrefixes: [][]byte{[]byte(id)}},
		}))
	}
	err := e.admin.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{
		TableID: e.table, AuthorizedViewID: "v-a", AuthorizedView: &bigtable.SubsetViewConf{},
	})
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	// GetAuthorizedView defaults to BASIC: name, deletion_protection, etag.
	basic, err := e.tableAdmin.GetAuthorizedView(ctx, &btapb.GetAuthorizedViewRequest{Name: e.viewName("restricted")})
	require.NoError(t, err)
	require.Equal(t, e.viewName("restricted"), basic.GetName())
	require.NotEmpty(t, basic.GetEtag())
	require.Nil(t, basic.GetSubsetView())
	nameOnly, err := e.tableAdmin.GetAuthorizedView(ctx, &btapb.GetAuthorizedViewRequest{Name: e.viewName("restricted"), View: btapb.AuthorizedView_NAME_ONLY})
	require.NoError(t, err)
	require.Equal(t, e.viewName("restricted"), nameOnly.GetName())
	require.Empty(t, nameOnly.GetEtag())
	require.Nil(t, nameOnly.GetSubsetView())
	full, err := e.tableAdmin.GetAuthorizedView(ctx, &btapb.GetAuthorizedViewRequest{Name: e.viewName("restricted"), View: btapb.AuthorizedView_FULL})
	require.NoError(t, err)
	require.Equal(t, basic.GetEtag(), full.GetEtag())
	require.Equal(t, [][]byte{[]byte("user#")}, full.GetSubsetView().GetRowPrefixes())
	require.Equal(t, [][]byte{[]byte("a")}, full.GetSubsetView().GetFamilySubsets()["cf1"].GetQualifiers())
	_, err = e.tableAdmin.GetAuthorizedView(ctx, &btapb.GetAuthorizedViewRequest{Name: e.viewName("missing")})
	require.Equal(t, codes.NotFound, status.Code(err))

	// The Go client's list uses NAME_ONLY.
	names, err := e.admin.AuthorizedViews(ctx, e.table)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"restricted", "v-a", "v-b", "v-c"}, names)

	// ListAuthorizedViews defaults to NAME_ONLY.
	list, err := e.tableAdmin.ListAuthorizedViews(ctx, &btapb.ListAuthorizedViewsRequest{Parent: e.tableName})
	require.NoError(t, err)
	require.Len(t, list.GetAuthorizedViews(), 4)
	for _, v := range list.GetAuthorizedViews() {
		require.Empty(t, v.GetEtag())
		require.Nil(t, v.GetSubsetView())
	}
	list, err = e.tableAdmin.ListAuthorizedViews(ctx, &btapb.ListAuthorizedViewsRequest{Parent: e.tableName, View: btapb.AuthorizedView_BASIC})
	require.NoError(t, err)
	for _, v := range list.GetAuthorizedViews() {
		require.NotEmpty(t, v.GetEtag())
		require.Nil(t, v.GetSubsetView())
	}
	list, err = e.tableAdmin.ListAuthorizedViews(ctx, &btapb.ListAuthorizedViewsRequest{Parent: e.tableName, View: btapb.AuthorizedView_FULL})
	require.NoError(t, err)
	for _, v := range list.GetAuthorizedViews() {
		require.NotNil(t, v.GetSubsetView(), v.GetName())
	}

	// Pagination returns every view exactly once, in a stable order.
	var paged []string
	token := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 3)
		resp, err := e.tableAdmin.ListAuthorizedViews(ctx, &btapb.ListAuthorizedViewsRequest{Parent: e.tableName, PageSize: 3, PageToken: token})
		require.NoError(t, err)
		require.LessOrEqual(t, len(resp.GetAuthorizedViews()), 3)
		for _, v := range resp.GetAuthorizedViews() {
			paged = append(paged, strings.TrimPrefix(v.GetName(), e.tableName+"/authorizedViews/"))
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	require.Equal(t, []string{"restricted", "v-a", "v-b", "v-c"}, paged)
	_, err = e.tableAdmin.ListAuthorizedViews(ctx, &btapb.ListAuthorizedViewsRequest{Parent: e.tableName, PageToken: "%%%not-a-token"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = e.tableAdmin.ListAuthorizedViews(ctx, &btapb.ListAuthorizedViewsRequest{Parent: e.tableName, PageSize: -1})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = e.tableAdmin.ListAuthorizedViews(ctx, &btapb.ListAuthorizedViewsRequest{Parent: e.tableName + "-missing"})
	require.Equal(t, codes.NotFound, status.Code(err))

	// At most 10 distinct qualifier prefixes per view.
	var prefixes [][]byte
	for i := 0; i < 11; i++ {
		prefixes = append(prefixes, []byte(fmt.Sprintf("p%d", i)))
	}
	err = e.admin.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{
		TableID: e.table, AuthorizedViewID: "too-many",
		AuthorizedView: &bigtable.SubsetViewConf{
			RowPrefixes:   [][]byte{[]byte("")},
			FamilySubsets: map[string]bigtable.FamilySubset{"cf1": {QualifierPrefixes: prefixes}},
		},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestConformanceAuthorizedViewEtagAndUpdateMask(t *testing.T) {
	e := ctgNewAVEnv(t)
	ctx := e.ctx
	name := e.viewName("restricted")
	get := func() *btapb.AuthorizedView {
		v, err := e.tableAdmin.GetAuthorizedView(ctx, &btapb.GetAuthorizedViewRequest{Name: name, View: btapb.AuthorizedView_FULL})
		require.NoError(t, err)
		return v
	}
	cur := get()

	// A stale etag aborts updates and deletes.
	_, err := e.tableAdmin.UpdateAuthorizedView(ctx, &btapb.UpdateAuthorizedViewRequest{
		AuthorizedView: &btapb.AuthorizedView{Name: name, Etag: "stale", DeletionProtection: true},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
	})
	require.Equal(t, codes.Aborted, status.Code(err))
	_, err = e.tableAdmin.DeleteAuthorizedView(ctx, &btapb.DeleteAuthorizedViewRequest{Name: name, Etag: "stale"})
	require.Equal(t, codes.Aborted, status.Code(err))
	require.Equal(t, cur.GetEtag(), get().GetEtag())

	// The current etag is accepted; the update changes the etag and only the
	// masked field.
	op, err := e.tableAdmin.UpdateAuthorizedView(ctx, &btapb.UpdateAuthorizedViewRequest{
		AuthorizedView: &btapb.AuthorizedView{Name: name, Etag: cur.GetEtag(), DeletionProtection: true},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
	})
	require.NoError(t, err)
	require.True(t, op.GetDone())
	next := get()
	require.True(t, next.GetDeletionProtection())
	require.NotEqual(t, cur.GetEtag(), next.GetEtag())
	require.Equal(t, cur.GetSubsetView().GetRowPrefixes(), next.GetSubsetView().GetRowPrefixes())
	_, err = e.tableAdmin.UpdateAuthorizedView(ctx, &btapb.UpdateAuthorizedViewRequest{
		AuthorizedView: &btapb.AuthorizedView{Name: name, Etag: cur.GetEtag()},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
	})
	require.Equal(t, codes.Aborted, status.Code(err))

	// An empty mask overwrites only the fields set in the request: setting a
	// new subset_view keeps deletion_protection.
	_, err = e.tableAdmin.UpdateAuthorizedView(ctx, &btapb.UpdateAuthorizedViewRequest{
		AuthorizedView: &btapb.AuthorizedView{Name: name, AuthorizedView: &btapb.AuthorizedView_SubsetView_{SubsetView: &btapb.AuthorizedView_SubsetView{
			RowPrefixes:   [][]byte{[]byte("other#")},
			FamilySubsets: map[string]*btapb.AuthorizedView_FamilySubsets{"cf1": {Qualifiers: [][]byte{[]byte("a")}}},
		}}},
	})
	require.NoError(t, err)
	next = get()
	require.True(t, next.GetDeletionProtection())
	require.Equal(t, [][]byte{[]byte("other#")}, next.GetSubsetView().GetRowPrefixes())
	// The new definition takes effect for data requests immediately.
	require.Equal(t, []string{"other#1 cf1:a=7"},
		ctgDump(t, ctx, e.client.OpenAuthorizedView(e.table, "restricted"), bigtable.InfiniteRange("")))

	_, err = e.tableAdmin.UpdateAuthorizedView(ctx, &btapb.UpdateAuthorizedViewRequest{
		AuthorizedView: &btapb.AuthorizedView{Name: name},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"etag"}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = e.tableAdmin.UpdateAuthorizedView(ctx, &btapb.UpdateAuthorizedViewRequest{
		AuthorizedView: &btapb.AuthorizedView{Name: e.viewName("missing")},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
	})
	require.Equal(t, codes.NotFound, status.Code(err))

	// "*" overwrites every field, including ones not set in the request.
	_, err = e.tableAdmin.UpdateAuthorizedView(ctx, &btapb.UpdateAuthorizedViewRequest{
		AuthorizedView: &btapb.AuthorizedView{Name: name, AuthorizedView: &btapb.AuthorizedView_SubsetView_{SubsetView: &btapb.AuthorizedView_SubsetView{
			RowPrefixes: [][]byte{[]byte("user#")},
		}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"*"}},
	})
	require.NoError(t, err)
	next = get()
	require.False(t, next.GetDeletionProtection())

	// Deleting with the current etag succeeds.
	_, err = e.tableAdmin.DeleteAuthorizedView(ctx, &btapb.DeleteAuthorizedViewRequest{Name: name, Etag: next.GetEtag()})
	require.NoError(t, err)
	_, err = e.tableAdmin.GetAuthorizedView(ctx, &btapb.GetAuthorizedViewRequest{Name: name})
	require.Equal(t, codes.NotFound, status.Code(err))
}

// Deletion protection on an authorized view blocks deleting the view, its
// table and its instance.
func TestConformanceAuthorizedViewDeletionProtection(t *testing.T) {
	e := ctgNewAVEnv(t)
	ctx := e.ctx
	require.NoError(t, e.admin.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{
		TableID: e.table, AuthorizedViewID: "protected", DeletionProtection: bigtable.Protected,
		AuthorizedView: &bigtable.SubsetViewConf{
			RowPrefixes: [][]byte{[]byte("")},
			FamilySubsets: map[string]bigtable.FamilySubset{
				"cf1": {QualifierPrefixes: [][]byte{[]byte("")}},
				"cf2": {QualifierPrefixes: [][]byte{[]byte("")}},
				"cf3": {QualifierPrefixes: [][]byte{[]byte("")}},
			},
		},
	}))
	info, err := e.admin.AuthorizedViewInfo(ctx, e.table, "protected")
	require.NoError(t, err)
	require.Equal(t, bigtable.Protected, info.DeletionProtection)

	require.Equal(t, codes.FailedPrecondition, status.Code(e.admin.DeleteAuthorizedView(ctx, e.table, "protected")))
	require.Equal(t, codes.FailedPrecondition, status.Code(e.admin.DeleteTable(ctx, e.table)))
	instance := fmt.Sprintf("projects/%s/instances/%s", e.projectID, e.instanceID)
	_, err = e.instanceAdmin.DeleteInstance(ctx, &btapb.DeleteInstanceRequest{Name: instance})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	// Everything is still there and readable.
	_, err = e.instanceAdmin.GetInstance(ctx, &btapb.GetInstanceRequest{Name: instance})
	require.NoError(t, err)
	require.Len(t, ctgDump(t, ctx, e.client.OpenAuthorizedView(e.table, "protected"), bigtable.InfiniteRange("")), 8)

	// Disabling protection unblocks the deletes.
	require.NoError(t, e.admin.UpdateAuthorizedView(ctx, bigtable.UpdateAuthorizedViewConf{
		AuthorizedViewConf: bigtable.AuthorizedViewConf{TableID: e.table, AuthorizedViewID: "protected", DeletionProtection: bigtable.Unprotected},
	}))
	require.NoError(t, e.admin.DeleteAuthorizedView(ctx, e.table, "protected"))
	require.NoError(t, e.admin.DeleteTable(ctx, e.table))
	// Deleting a table also deletes all of its authorized views; a table
	// re-created with the same ID starts without views.
	_, err = e.tableAdmin.GetAuthorizedView(ctx, &btapb.GetAuthorizedViewRequest{Name: e.viewName("restricted")})
	require.Equal(t, codes.NotFound, status.Code(err))
	require.NoError(t, e.admin.CreateTable(ctx, e.table))
	names, err := e.admin.AuthorizedViews(ctx, e.table)
	require.NoError(t, err)
	require.Empty(t, names)
	_, err = e.client.OpenAuthorizedView(e.table, "restricted").ReadRow(ctx, "user#1")
	require.Equal(t, codes.NotFound, status.Code(err))
}

package bttest

import (
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// cadIAMResources creates one of every IAM-bearing resource under
// cadInstance and returns their names.
func (e *cadEnv) cadIAMResources() (table, av, backup, bundle string) {
	e.t.Helper()
	table = e.createTable("iam-table", "cf")
	_, err := e.ta.CreateAuthorizedView(e.ctx, &btapb.CreateAuthorizedViewRequest{
		Parent: table, AuthorizedViewId: "iam-av",
		AuthorizedView: &btapb.AuthorizedView{AuthorizedView: &btapb.AuthorizedView_SubsetView_{
			SubsetView: &btapb.AuthorizedView_SubsetView{RowPrefixes: [][]byte{[]byte("")}},
		}},
	})
	require.NoError(e.t, err)
	_, err = e.ta.CreateBackup(e.ctx, &btapb.CreateBackupRequest{
		Parent: cadCluster, BackupId: "iam-backup",
		Backup: &btapb.Backup{SourceTable: table, ExpireTime: timestamppb.New(time.Now().Add(48 * time.Hour))},
	})
	require.NoError(e.t, err)
	_, err = e.ta.CreateSchemaBundle(e.ctx, &btapb.CreateSchemaBundleRequest{
		Parent: table, SchemaBundleId: "iam-sb",
		SchemaBundle: &btapb.SchemaBundle{Type: &btapb.SchemaBundle_ProtoSchema{ProtoSchema: &btapb.ProtoSchema{ProtoDescriptors: cadDescriptorSet(e.t)}}},
	})
	require.NoError(e.t, err)
	return table, table + "/authorizedViews/iam-av", cadCluster + "/backups/iam-backup", table + "/schemaBundles/iam-sb"
}

func cadReaderPolicy(member string) *iampb.Policy {
	return &iampb.Policy{Version: 1, Bindings: []*iampb.Binding{{Role: "roles/bigtable.reader", Members: []string{member}}}}
}

// TestConformanceIAMMissingResourceNotFound: IAM methods require the
// resource to exist.
func TestConformanceIAMMissingResourceNotFound(t *testing.T) {
	e := cadNewEnv(t)
	e.createInstance()
	table := e.createTable("iam-table", "cf")
	missing := []string{
		cadInstance + "/tables/missing",
		table + "/authorizedViews/missing",
		table + "/schemaBundles/missing",
		cadCluster + "/backups/missing",
		cadInstance + "/logicalViews/missing",
		cadInstance + "/materializedViews/missing",
		cadProject + "/instances/missing-instance",
	}
	for _, resource := range missing {
		_, err := e.ta.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{Resource: resource})
		require.Equal(t, codes.NotFound, status.Code(err), "GetIamPolicy %s", resource)
		_, err = e.ta.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: cadReaderPolicy("user:a@example.com")})
		require.Equal(t, codes.NotFound, status.Code(err), "SetIamPolicy %s", resource)
		_, err = e.ta.TestIamPermissions(e.ctx, &iampb.TestIamPermissionsRequest{Resource: resource, Permissions: []string{"bigtable.tables.get"}})
		require.Equal(t, codes.NotFound, status.Code(err), "TestIamPermissions %s", resource)
	}
	_, err := e.ia.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "resource is required")
	_, err = e.ta.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{Resource: table})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "policy is required")

	// The official client surfaces the same status.
	admin, err := bigtable.NewAdminClient(e.ctx, cadProjectID, cadInstanceID, option.WithGRPCConn(e.conn))
	require.NoError(t, err)
	defer admin.Close()
	_, err = admin.TableIAM("missing").Policy(e.ctx)
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestConformanceIAMOfficialClientHandles round-trips policies through the
// official client's iam.Handle for instances, tables, backups and
// authorized views, and checks TestIamPermissions.
func TestConformanceIAMOfficialClientHandles(t *testing.T) {
	e := cadNewEnv(t)
	e.createInstance()
	e.cadIAMResources()
	admin, err := bigtable.NewAdminClient(e.ctx, cadProjectID, cadInstanceID, option.WithGRPCConn(e.conn))
	require.NoError(t, err)
	defer admin.Close()
	iac, err := bigtable.NewInstanceAdminClient(e.ctx, cadProjectID, option.WithGRPCConn(e.conn))
	require.NoError(t, err)
	defer iac.Close()

	handles := map[string]*iam.Handle{
		"instance":       iac.InstanceIAM(cadInstanceID),
		"table":          admin.TableIAM("iam-table"),
		"backup":         admin.BackupIAM(cadClusterID, "iam-backup"),
		"authorizedView": admin.AuthorizedViewIAM("iam-table", "iam-av"),
	}
	for label, h := range handles {
		p, err := h.Policy(e.ctx)
		require.NoError(t, err, label)
		require.Empty(t, p.Roles(), "%s starts with an empty policy", label)
		member := "user:" + label + "@example.com"
		p.Add(member, "roles/bigtable.reader")
		require.NoError(t, h.SetPolicy(e.ctx, p), label)
		got, err := h.Policy(e.ctx)
		require.NoError(t, err, label)
		require.Equal(t, []string{member}, got.Members("roles/bigtable.reader"), label)

		perms := []string{"bigtable.tables.readRows", "bigtable.tables.mutateRows"}
		granted, err := h.TestPermissions(e.ctx, perms)
		require.NoError(t, err, label)
		require.Equal(t, perms, granted, "%s: the unauthenticated emulator grants every permission", label)
	}
}

// TestConformanceIAMEtagAborted: a policy written with a stale etag is
// rejected with ABORTED so read-modify-write cycles cannot lose updates.
func TestConformanceIAMEtagAborted(t *testing.T) {
	e := cadNewEnv(t)
	e.createInstance()
	table := e.createTable("iam-table", "cf")

	initial, err := e.ta.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{Resource: table})
	require.NoError(t, err)
	require.NotEmpty(t, initial.GetEtag(), "a default policy still carries an etag")

	p := cadReaderPolicy("user:first@example.com")
	p.Etag = initial.GetEtag()
	first, err := e.ta.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{Resource: table, Policy: p})
	require.NoError(t, err)
	require.NotEqual(t, initial.GetEtag(), first.GetEtag(), "the etag changes when the policy changes")

	stale := cadReaderPolicy("user:second@example.com")
	stale.Etag = initial.GetEtag()
	_, err = e.ta.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{Resource: table, Policy: stale})
	require.Equal(t, codes.Aborted, status.Code(err))
	got, err := e.ta.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{Resource: table})
	require.NoError(t, err)
	require.True(t, proto.Equal(first, got), "a rejected write leaves the policy unchanged")

	// The official client sends the etag it read: two concurrent
	// read-modify-write cycles cannot both succeed.
	admin, err := bigtable.NewAdminClient(e.ctx, cadProjectID, cadInstanceID, option.WithGRPCConn(e.conn))
	require.NoError(t, err)
	defer admin.Close()
	h := admin.TableIAM("iam-table")
	p1, err := h.Policy(e.ctx)
	require.NoError(t, err)
	p2, err := h.Policy(e.ctx)
	require.NoError(t, err)
	p1.Add("user:p1@example.com", "roles/bigtable.user")
	require.NoError(t, h.SetPolicy(e.ctx, p1))
	p2.Add("user:p2@example.com", "roles/bigtable.user")
	require.Equal(t, codes.Aborted, status.Code(h.SetPolicy(e.ctx, p2)))

	// Without an etag the write is unconditional.
	_, err = e.ta.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{Resource: table, Policy: cadReaderPolicy("user:blind@example.com")})
	require.NoError(t, err)
}

// TestConformanceIAMUpdateMask: SetIamPolicy only replaces the policy
// fields named by update_mask.
func TestConformanceIAMUpdateMask(t *testing.T) {
	e := cadNewEnv(t)
	e.createInstance()
	table := e.createTable("iam-table", "cf")
	base := cadReaderPolicy("user:base@example.com")
	base.Version = 3
	_, err := e.ta.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{Resource: table, Policy: base})
	require.NoError(t, err)

	other := cadReaderPolicy("user:other@example.com")
	got, err := e.ta.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{
		Resource: table, Policy: other, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"version"}},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, got.GetVersion(), "version is in the mask")
	require.Equal(t, []string{"user:base@example.com"}, got.GetBindings()[0].GetMembers(), "bindings are not in the mask")

	other.Version = 3
	got, err = e.ta.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{
		Resource: table, Policy: other, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bindings", "etag"}},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, got.GetVersion(), "version is not in the mask")
	require.Equal(t, []string{"user:other@example.com"}, got.GetBindings()[0].GetMembers())

	stored, err := e.ta.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{Resource: table})
	require.NoError(t, err)
	require.True(t, proto.Equal(got, stored))

	_, err = e.ta.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{
		Resource: table, Policy: other, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"owner"}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestConformanceIAMPersistsAcrossRestart: policies are durable.
func TestConformanceIAMPersistsAcrossRestart(t *testing.T) {
	e := cadNewEnv(t)
	e.createInstance()
	table, av, backup, bundle := e.cadIAMResources()
	resources := []string{cadInstance, table, av, backup, bundle}
	want := map[string]*iampb.Policy{}
	for _, r := range resources {
		p, err := e.ta.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{Resource: r, Policy: cadReaderPolicy("user:" + r + "@example.com")})
		require.NoError(t, err, r)
		want[r] = p
	}

	e = e.restart()
	for _, r := range resources {
		got, err := e.ia.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{Resource: r})
		require.NoError(t, err, r)
		require.True(t, proto.Equal(want[r], got), "%s policy survives a restart", r)
	}
}

// TestConformanceIAMPoliciesRemovedWithResource: deleting a resource deletes
// its policy (and its children's), so a re-created resource with the same
// name starts with an empty policy.
func TestConformanceIAMPoliciesRemovedWithResource(t *testing.T) {
	e := cadNewEnv(t)
	e.createInstance()
	table, av, backup, bundle := e.cadIAMResources()
	setPolicy := func(r string) {
		t.Helper()
		_, err := e.ta.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{Resource: r, Policy: cadReaderPolicy("user:x@example.com")})
		require.NoError(t, err, r)
	}
	requireEmpty := func(r string) {
		t.Helper()
		p, err := e.ta.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{Resource: r})
		require.NoError(t, err, r)
		require.Empty(t, p.GetBindings(), "%s must not inherit the deleted resource's policy", r)
	}
	subsetAll := &btapb.AuthorizedView{AuthorizedView: &btapb.AuthorizedView_SubsetView_{
		SubsetView: &btapb.AuthorizedView_SubsetView{RowPrefixes: [][]byte{[]byte("")}},
	}}
	schema := &btapb.SchemaBundle{Type: &btapb.SchemaBundle_ProtoSchema{ProtoSchema: &btapb.ProtoSchema{ProtoDescriptors: cadDescriptorSet(t)}}}
	recreateAV := func() {
		t.Helper()
		_, err := e.ta.CreateAuthorizedView(e.ctx, &btapb.CreateAuthorizedViewRequest{Parent: table, AuthorizedViewId: "iam-av", AuthorizedView: subsetAll})
		require.NoError(t, err)
	}
	recreateBundle := func() {
		t.Helper()
		_, err := e.ta.CreateSchemaBundle(e.ctx, &btapb.CreateSchemaBundleRequest{Parent: table, SchemaBundleId: "iam-sb", SchemaBundle: schema})
		require.NoError(t, err)
	}

	// Child resources deleted on their own.
	setPolicy(av)
	_, err := e.ta.DeleteAuthorizedView(e.ctx, &btapb.DeleteAuthorizedViewRequest{Name: av})
	require.NoError(t, err)
	recreateAV()
	requireEmpty(av)

	setPolicy(bundle)
	_, err = e.ta.DeleteSchemaBundle(e.ctx, &btapb.DeleteSchemaBundleRequest{Name: bundle})
	require.NoError(t, err)
	recreateBundle()
	requireEmpty(bundle)

	setPolicy(backup)
	_, err = e.ta.DeleteBackup(e.ctx, &btapb.DeleteBackupRequest{Name: backup})
	require.NoError(t, err)
	_, err = e.ta.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{Resource: backup})
	require.Equal(t, codes.NotFound, status.Code(err), "IAM on a deleted backup")

	// Deleting the table removes its policy and its children's policies.
	setPolicy(table)
	setPolicy(av)
	setPolicy(bundle)
	_, err = e.ta.DeleteTable(e.ctx, &btapb.DeleteTableRequest{Name: table})
	require.NoError(t, err)
	_, err = e.ta.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{Resource: table})
	require.Equal(t, codes.NotFound, status.Code(err), "IAM on a deleted table")
	e.createTable("iam-table", "cf")
	requireEmpty(table)
	recreateAV()
	requireEmpty(av)
	// Schema bundles are kept with the soft-deleted table for UndeleteTable;
	// either way the bundle name must not carry the old policy.
	if _, err := e.ta.GetSchemaBundle(e.ctx, &btapb.GetSchemaBundleRequest{Name: bundle}); status.Code(err) == codes.NotFound {
		recreateBundle()
	}
	requireEmpty(bundle)

	// Deleting the instance removes the instance policy.
	setPolicy(cadInstance)
	_, err = e.ta.DeleteTable(e.ctx, &btapb.DeleteTableRequest{Name: table})
	require.NoError(t, err)
	_, err = e.ia.DeleteInstance(e.ctx, &btapb.DeleteInstanceRequest{Name: cadInstance})
	require.NoError(t, err)
	_, err = e.ia.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{Resource: cadInstance})
	require.Equal(t, codes.NotFound, status.Code(err), "IAM on a deleted instance")
	e.createInstance()
	requireEmpty(cadInstance)
}

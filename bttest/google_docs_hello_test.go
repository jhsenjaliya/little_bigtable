package bttest

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestGoogleDocsHelloWorldGoClientWorkflow(t *testing.T) {
	const (
		projectID        = "local-project"
		instanceID       = "local-instance"
		tableName        = "Hello-Bigtable"
		columnFamilyName = "cf1"
		columnName       = "greeting"
	)
	greetings := []string{"Hello World!", "Hello Cloud Bigtable!", "Hello Go!"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	prevDialect, prevStrict := currentDialect(), isStrictAdmin()
	ConfigureStorage("sqlite3", true)
	t.Cleanup(func() { ConfigureStorage(string(prevDialect), prevStrict) })

	dbFilename := newDBFile(t)
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared", dbFilename))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	require.NoError(t, CreateTables(ctx, db))

	srv, err := NewServer("127.0.0.1:0", db)
	require.NoError(t, err)
	t.Cleanup(srv.Close)
	t.Setenv("BIGTABLE_EMULATOR_HOST", srv.Addr)

	conn, err := grpc.DialContext(ctx, srv.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	instanceAdmin := btapb.NewBigtableInstanceAdminClient(conn)
	op, err := instanceAdmin.CreateInstance(ctx, &btapb.CreateInstanceRequest{
		Parent:     "projects/" + projectID,
		InstanceId: instanceID,
		Instance: &btapb.Instance{
			DisplayName: "Local Bigtable instance",
			Type:        btapb.Instance_PRODUCTION,
		},
		Clusters: map[string]*btapb.Cluster{
			"local-cluster": {},
		},
	})
	require.NoError(t, err)
	require.True(t, op.GetDone())

	adminClient, err := bigtable.NewAdminClient(ctx, projectID, instanceID, option.WithGRPCConn(conn))
	require.NoError(t, err)
	t.Cleanup(func() { _ = adminClient.Close() })

	tables, err := adminClient.Tables(ctx)
	require.NoError(t, err)
	if !stringSliceContains(tables, tableName) {
		require.NoError(t, adminClient.CreateTable(ctx, tableName))
	}

	tblInfo, err := adminClient.TableInfo(ctx, tableName)
	require.NoError(t, err)
	if !stringSliceContains(tblInfo.Families, columnFamilyName) {
		require.NoError(t, adminClient.CreateColumnFamily(ctx, tableName, columnFamilyName))
	}

	dataConn, err := grpc.DialContext(ctx, srv.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dataConn.Close() })

	client, err := bigtable.NewClient(ctx, projectID, instanceID, option.WithGRPCConn(dataConn))
	require.NoError(t, err)

	tbl := client.Open(tableName)
	muts := make([]*bigtable.Mutation, len(greetings))
	rowKeys := make([]string, len(greetings))
	for i, greeting := range greetings {
		muts[i] = bigtable.NewMutation()
		muts[i].Set(columnFamilyName, columnName, bigtable.Now(), []byte(greeting))
		rowKeys[i] = fmt.Sprintf("%s%d", columnName, i)
	}

	rowErrs, err := tbl.ApplyBulk(ctx, rowKeys, muts)
	require.NoError(t, err)
	for _, rowErr := range rowErrs {
		require.NoError(t, rowErr)
	}

	row, err := tbl.ReadRow(ctx, rowKeys[0], bigtable.RowFilter(bigtable.ColumnFilter(columnName)))
	require.NoError(t, err)
	require.Len(t, row[columnFamilyName], 1)
	require.Equal(t, greetings[0], string(row[columnFamilyName][0].Value))

	got := make([]string, 0, len(greetings))
	err = tbl.ReadRows(ctx, bigtable.PrefixRange(columnName), func(row bigtable.Row) bool {
		items := row[columnFamilyName]
		if len(items) > 0 {
			got = append(got, fmt.Sprintf("%s=%s", items[0].Row, string(items[0].Value)))
		}
		return true
	}, bigtable.RowFilter(bigtable.ColumnFilter(columnName)))
	require.NoError(t, err)
	require.NoError(t, client.Close())

	sort.Strings(got)
	require.Equal(t, []string{
		"greeting0=Hello World!",
		"greeting1=Hello Cloud Bigtable!",
		"greeting2=Hello Go!",
	}, got)

	require.NoError(t, adminClient.DeleteTable(ctx, tableName))
}

func stringSliceContains(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

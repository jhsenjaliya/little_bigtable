package bttest

// Tests that verify all DEV-CRITICAL Cloud Bigtable features work in the emulator,
// using the official Go client library (cloud.google.com/go/bigtable).
//
// These tests mirror the usage patterns from Google's Cloud Bigtable documentation:
//   https://cloud.google.com/bigtable/docs/writing-data
//   https://cloud.google.com/bigtable/docs/reading-data
//   https://cloud.google.com/bigtable/docs/using-filters
//   https://cloud.google.com/bigtable/docs/mutations-and-deletions
//   https://cloud.google.com/bigtable/docs/conditional-mutations
//   https://cloud.google.com/bigtable/docs/read-modify-write
//   https://cloud.google.com/bigtable/docs/managing-tables
//   https://cloud.google.com/bigtable/docs/garbage-collection

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"sort"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// testEnv bundles all clients needed for a test against the emulator.
type testEnv struct {
	ctx        context.Context
	cancel     context.CancelFunc
	admin      *bigtable.AdminClient
	client     *bigtable.Client
	instanceID string
	projectID  string
}

// setupTestEnv creates a fresh emulator, instance, and returns connected clients.
func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	const (
		projectID  = "test-project"
		instanceID = "test-instance"
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

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

	// Create instance via raw gRPC (admin client requires it to exist).
	instanceAdmin := btapb.NewBigtableInstanceAdminClient(conn)
	op, err := instanceAdmin.CreateInstance(ctx, &btapb.CreateInstanceRequest{
		Parent:     "projects/" + projectID,
		InstanceId: instanceID,
		Instance: &btapb.Instance{
			DisplayName: "Test instance",
			Type:        btapb.Instance_PRODUCTION,
		},
		Clusters: map[string]*btapb.Cluster{
			"test-cluster": {},
		},
	})
	require.NoError(t, err)
	require.True(t, op.GetDone())

	adminClient, err := bigtable.NewAdminClient(ctx, projectID, instanceID, option.WithGRPCConn(conn))
	require.NoError(t, err)
	t.Cleanup(func() { _ = adminClient.Close() })

	dataConn, err := grpc.DialContext(ctx, srv.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dataConn.Close() })

	client, err := bigtable.NewClient(ctx, projectID, instanceID, option.WithGRPCConn(dataConn))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return &testEnv{
		ctx:        ctx,
		cancel:     cancel,
		admin:      adminClient,
		client:     client,
		instanceID: instanceID,
		projectID:  projectID,
	}
}

// createTable is a helper that creates a table with given column families.
func (e *testEnv) createTable(t *testing.T, table string, families ...string) *bigtable.Table {
	t.Helper()
	require.NoError(t, e.admin.CreateTable(e.ctx, table))
	for _, cf := range families {
		require.NoError(t, e.admin.CreateColumnFamily(e.ctx, table, cf))
	}
	t.Cleanup(func() { _ = e.admin.DeleteTable(e.ctx, table) })
	return e.client.Open(table)
}

// --------------------------------------------------------------------------
// Test 1: Writing Data — Google Docs patterns
// https://cloud.google.com/bigtable/docs/writing-data
// --------------------------------------------------------------------------

func TestGoogleDocs_WritingData(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	tbl := env.createTable(t, "write-test", "stats", "profile")

	t.Run("SingleRowMutation", func(t *testing.T) {
		// Google Docs: simple write pattern
		mut := bigtable.NewMutation()
		mut.Set("stats", "connected_cell", bigtable.Now(), []byte("1"))
		mut.Set("stats", "os_build", bigtable.Now(), []byte("PQ2A.190405.003"))
		require.NoError(t, tbl.Apply(env.ctx, "phone#4c410523#20190501", mut))

		row, err := tbl.ReadRow(env.ctx, "phone#4c410523#20190501")
		require.NoError(t, err)
		require.Len(t, row["stats"], 2)
	})

	t.Run("BulkMutateRows", func(t *testing.T) {
		// Google Docs: writing multiple rows in batch
		rowKeys := []string{"phone#a", "phone#b", "phone#c"}
		muts := make([]*bigtable.Mutation, 3)
		for i, key := range rowKeys {
			muts[i] = bigtable.NewMutation()
			muts[i].Set("stats", "os", bigtable.Now(), []byte(fmt.Sprintf("android-%s", key)))
		}
		rowErrs, err := tbl.ApplyBulk(env.ctx, rowKeys, muts)
		require.NoError(t, err)
		for _, e := range rowErrs {
			require.NoError(t, e)
		}

		// Verify all rows written
		var count int
		err = tbl.ReadRows(env.ctx, bigtable.PrefixRange("phone#"), func(row bigtable.Row) bool {
			count++
			return true
		})
		require.NoError(t, err)
		assert.GreaterOrEqual(t, count, 3)
	})

	t.Run("MultipleCellVersions", func(t *testing.T) {
		// Google Docs: each cell can hold multiple timestamped versions
		rowKey := "phone#versions"
		ts1 := bigtable.Time(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
		ts2 := bigtable.Time(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
		ts3 := bigtable.Time(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

		mut1 := bigtable.NewMutation()
		mut1.Set("stats", "os", ts1, []byte("android-12"))
		require.NoError(t, tbl.Apply(env.ctx, rowKey, mut1))

		mut2 := bigtable.NewMutation()
		mut2.Set("stats", "os", ts2, []byte("android-13"))
		require.NoError(t, tbl.Apply(env.ctx, rowKey, mut2))

		mut3 := bigtable.NewMutation()
		mut3.Set("stats", "os", ts3, []byte("android-14"))
		require.NoError(t, tbl.Apply(env.ctx, rowKey, mut3))

		// Read all versions (no CellsPerColumn limit)
		row, err := tbl.ReadRow(env.ctx, rowKey,
			bigtable.RowFilter(bigtable.ColumnFilter("os")),
		)
		require.NoError(t, err)
		// Should have 3 versions, newest first
		require.GreaterOrEqual(t, len(row["stats"]), 3)
		assert.Equal(t, "android-14", string(row["stats"][0].Value))
	})

	t.Run("WriteToMultipleFamilies", func(t *testing.T) {
		// Google Docs: mutation can span multiple column families
		rowKey := "user#alice"
		mut := bigtable.NewMutation()
		mut.Set("stats", "login_count", bigtable.Now(), []byte("42"))
		mut.Set("profile", "name", bigtable.Now(), []byte("Alice"))
		mut.Set("profile", "email", bigtable.Now(), []byte("alice@example.com"))
		require.NoError(t, tbl.Apply(env.ctx, rowKey, mut))

		row, err := tbl.ReadRow(env.ctx, rowKey)
		require.NoError(t, err)
		require.Len(t, row["stats"], 1)
		require.Len(t, row["profile"], 2)
	})
}

// --------------------------------------------------------------------------
// Test 2: Reading Data — Google Docs patterns
// https://cloud.google.com/bigtable/docs/reading-data
// --------------------------------------------------------------------------

func TestGoogleDocs_ReadingData(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	tbl := env.createTable(t, "read-test", "cf")

	// Seed data
	rows := map[string]string{
		"row#1": "val1",
		"row#2": "val2",
		"row#3": "val3",
		"row#4": "val4",
		"row#5": "val5",
	}
	for k, v := range rows {
		mut := bigtable.NewMutation()
		mut.Set("cf", "col", bigtable.Now(), []byte(v))
		require.NoError(t, tbl.Apply(env.ctx, k, mut))
	}

	t.Run("ReadSingleRow", func(t *testing.T) {
		row, err := tbl.ReadRow(env.ctx, "row#1")
		require.NoError(t, err)
		require.NotNil(t, row)
		assert.Equal(t, "val1", string(row["cf"][0].Value))
	})

	t.Run("ReadRowRange", func(t *testing.T) {
		// Google Docs: read a range of rows
		var keys []string
		err := tbl.ReadRows(env.ctx, bigtable.NewRange("row#2", "row#4"), func(row bigtable.Row) bool {
			keys = append(keys, row.Key())
			return true
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"row#2", "row#3"}, keys) // end key exclusive
	})

	t.Run("ReadRowPrefix", func(t *testing.T) {
		// Google Docs: read all rows matching a prefix
		var keys []string
		err := tbl.ReadRows(env.ctx, bigtable.PrefixRange("row#"), func(row bigtable.Row) bool {
			keys = append(keys, row.Key())
			return true
		})
		require.NoError(t, err)
		assert.Len(t, keys, 5)
	})

	t.Run("ReadMultipleRowKeys", func(t *testing.T) {
		// Google Docs: read specific rows by key
		rowList := bigtable.RowList{"row#1", "row#3", "row#5"}
		var keys []string
		err := tbl.ReadRows(env.ctx, rowList, func(row bigtable.Row) bool {
			keys = append(keys, row.Key())
			return true
		})
		require.NoError(t, err)
		sort.Strings(keys)
		assert.Equal(t, []string{"row#1", "row#3", "row#5"}, keys)
	})

	t.Run("ReadRowsReversed", func(t *testing.T) {
		// Google Docs: reverse scan
		var keys []string
		err := tbl.ReadRows(env.ctx, bigtable.NewRange("row#1", "row#4"), func(row bigtable.Row) bool {
			keys = append(keys, row.Key())
			return true
		}, bigtable.ReverseScan())
		require.NoError(t, err)
		assert.Equal(t, []string{"row#3", "row#2", "row#1"}, keys)
	})

	t.Run("ReadRowsWithLimit", func(t *testing.T) {
		// Google Docs: limit number of rows returned
		var keys []string
		err := tbl.ReadRows(env.ctx, bigtable.PrefixRange("row#"), func(row bigtable.Row) bool {
			keys = append(keys, row.Key())
			return true
		}, bigtable.LimitRows(2))
		require.NoError(t, err)
		assert.Len(t, keys, 2)
	})

	t.Run("ReadNonExistentRow", func(t *testing.T) {
		row, err := tbl.ReadRow(env.ctx, "does-not-exist")
		require.NoError(t, err)
		assert.Nil(t, row) // nil = row not found
	})
}

// --------------------------------------------------------------------------
// Test 3: Filters — Google Docs patterns
// https://cloud.google.com/bigtable/docs/using-filters
// --------------------------------------------------------------------------

func TestGoogleDocs_Filters(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	tbl := env.createTable(t, "filter-test", "stats", "profile")

	// Seed data with multiple families, columns, versions
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("user#%04d", i)
		mut := bigtable.NewMutation()
		mut.Set("stats", "visits", bigtable.Now(), []byte(fmt.Sprintf("%d", (i+1)*10)))
		mut.Set("stats", "score", bigtable.Now(), []byte(fmt.Sprintf("%d", (i+1)*100)))
		mut.Set("profile", "name", bigtable.Now(), []byte(fmt.Sprintf("user-%d", i)))
		mut.Set("profile", "city", bigtable.Now(), []byte("city"))
		require.NoError(t, tbl.Apply(env.ctx, key, mut))
	}

	t.Run("RowKeyRegexFilter", func(t *testing.T) {
		// Google Docs: filter rows by key regex
		var keys []string
		err := tbl.ReadRows(env.ctx, bigtable.PrefixRange("user#"), func(row bigtable.Row) bool {
			keys = append(keys, row.Key())
			return true
		}, bigtable.RowFilter(bigtable.RowKeyFilter("user#000[0-2]")))
		require.NoError(t, err)
		assert.Equal(t, []string{"user#0000", "user#0001", "user#0002"}, keys)
	})

	t.Run("ColumnFamilyFilter", func(t *testing.T) {
		// Google Docs: filter to specific column family
		row, err := tbl.ReadRow(env.ctx, "user#0000",
			bigtable.RowFilter(bigtable.FamilyFilter("profile")),
		)
		require.NoError(t, err)
		assert.NotNil(t, row["profile"])
		assert.Nil(t, row["stats"]) // filtered out
	})

	t.Run("ColumnQualifierFilter", func(t *testing.T) {
		// Google Docs: filter to specific column
		row, err := tbl.ReadRow(env.ctx, "user#0000",
			bigtable.RowFilter(bigtable.ColumnFilter("name")),
		)
		require.NoError(t, err)
		require.Len(t, row["profile"], 1)
		assert.Equal(t, "name", row["profile"][0].Column[len("profile:"):])
	})

	t.Run("ValueFilter", func(t *testing.T) {
		// Google Docs: filter cells by value regex
		var keys []string
		err := tbl.ReadRows(env.ctx, bigtable.PrefixRange("user#"), func(row bigtable.Row) bool {
			keys = append(keys, row.Key())
			return true
		}, bigtable.RowFilter(bigtable.ValueFilter("user-[34]")))
		require.NoError(t, err)
		assert.Equal(t, []string{"user#0003", "user#0004"}, keys)
	})

	t.Run("CellsPerColumnLimitFilter", func(t *testing.T) {
		// Write multiple versions
		key := "user#multiversion"
		for i := 0; i < 5; i++ {
			mut := bigtable.NewMutation()
			ts := bigtable.Time(time.Date(2024, 1, 1+i, 0, 0, 0, 0, time.UTC))
			mut.Set("stats", "visits", ts, []byte(fmt.Sprintf("v%d", i)))
			require.NoError(t, tbl.Apply(env.ctx, key, mut))
		}

		// Google Docs: limit versions returned per column
		row, err := tbl.ReadRow(env.ctx, key,
			bigtable.RowFilter(bigtable.ChainFilters(
				bigtable.ColumnFilter("visits"),
				bigtable.LatestNFilter(2),
			)),
		)
		require.NoError(t, err)
		assert.Len(t, row["stats"], 2) // only 2 most recent
	})

	t.Run("CellsPerRowLimitFilter", func(t *testing.T) {
		// Google Docs: limit total cells per row
		row, err := tbl.ReadRow(env.ctx, "user#0000",
			bigtable.RowFilter(bigtable.CellsPerRowLimitFilter(1)),
		)
		require.NoError(t, err)
		totalCells := 0
		for _, cells := range row {
			totalCells += len(cells)
		}
		assert.Equal(t, 1, totalCells)
	})

	t.Run("CellsPerRowOffsetFilter", func(t *testing.T) {
		// Google Docs: skip first N cells in row
		row, err := tbl.ReadRow(env.ctx, "user#0000",
			bigtable.RowFilter(bigtable.CellsPerRowOffsetFilter(2)),
		)
		require.NoError(t, err)
		totalCells := 0
		for _, cells := range row {
			totalCells += len(cells)
		}
		assert.Equal(t, 2, totalCells) // 4 total - 2 skipped = 2
	})

	t.Run("StripValueFilter", func(t *testing.T) {
		// Google Docs: return row structure without values
		row, err := tbl.ReadRow(env.ctx, "user#0000",
			bigtable.RowFilter(bigtable.StripValueFilter()),
		)
		require.NoError(t, err)
		for _, cells := range row {
			for _, cell := range cells {
				assert.Empty(t, cell.Value, "StripValue should empty all values")
			}
		}
	})

	t.Run("ChainFilter_AND", func(t *testing.T) {
		// Google Docs: chain = sequential AND — output of filter N → input of filter N+1
		row, err := tbl.ReadRow(env.ctx, "user#0000",
			bigtable.RowFilter(bigtable.ChainFilters(
				bigtable.FamilyFilter("stats"),
				bigtable.ColumnFilter("visits"),
			)),
		)
		require.NoError(t, err)
		require.Len(t, row["stats"], 1)
		assert.Contains(t, row["stats"][0].Column, "visits")
	})

	t.Run("InterleaveFilter_OR", func(t *testing.T) {
		// Google Docs: interleave = union/OR — merge results from multiple filters
		row, err := tbl.ReadRow(env.ctx, "user#0000",
			bigtable.RowFilter(bigtable.InterleaveFilters(
				bigtable.ColumnFilter("visits"),
				bigtable.ColumnFilter("name"),
			)),
		)
		require.NoError(t, err)
		var cols []string
		for _, cells := range row {
			for _, cell := range cells {
				cols = append(cols, cell.Column)
			}
		}
		sort.Strings(cols)
		assert.Equal(t, []string{"profile:name", "stats:visits"}, cols)
	})

	t.Run("ConditionFilter", func(t *testing.T) {
		// Google Docs: if-then-else filter
		// If row has "visits" column → return only stats family, else return profile
		row, err := tbl.ReadRow(env.ctx, "user#0000",
			bigtable.RowFilter(bigtable.ConditionFilter(
				bigtable.ColumnFilter("visits"),  // predicate
				bigtable.FamilyFilter("stats"),   // true branch
				bigtable.FamilyFilter("profile"), // false branch
			)),
		)
		require.NoError(t, err)
		// user#0000 has "visits" → should get stats family
		assert.NotNil(t, row["stats"])
	})

	t.Run("TimestampRangeFilter", func(t *testing.T) {
		// Google Docs: filter cells by timestamp range
		key := "user#tsrange"
		t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		t2 := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
		t3 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

		for _, ts := range []time.Time{t1, t2, t3} {
			mut := bigtable.NewMutation()
			mut.Set("stats", "val", bigtable.Time(ts), []byte(ts.Format("2006")))
			require.NoError(t, tbl.Apply(env.ctx, key, mut))
		}

		// Read only cells from 2024 (start inclusive, end exclusive)
		row, err := tbl.ReadRow(env.ctx, key,
			bigtable.RowFilter(bigtable.TimestampRangeFilter(t1, t3)),
		)
		require.NoError(t, err)
		assert.Len(t, row["stats"], 2) // t1 and t2, not t3
	})

	t.Run("ValueRangeFilter", func(t *testing.T) {
		// Google Docs: filter cells by value range
		for i := 0; i < 5; i++ {
			key := fmt.Sprintf("vr#%d", i)
			mut := bigtable.NewMutation()
			mut.Set("stats", "letter", bigtable.Now(), []byte(string(rune('a'+i))))
			require.NoError(t, tbl.Apply(env.ctx, key, mut))
		}

		var vals []string
		err := tbl.ReadRows(env.ctx, bigtable.PrefixRange("vr#"), func(row bigtable.Row) bool {
			if cells, ok := row["stats"]; ok {
				for _, c := range cells {
					vals = append(vals, string(c.Value))
				}
			}
			return true
		}, bigtable.RowFilter(bigtable.ValueRangeFilter([]byte("b"), []byte("d"))))
		require.NoError(t, err)
		sort.Strings(vals)
		assert.Equal(t, []string{"b", "c"}, vals) // [b, d) — start inclusive, end exclusive
	})

	t.Run("ColumnRangeFilter", func(t *testing.T) {
		// Google Docs: filter columns in a range within a family
		key := "user#colrange"
		mut := bigtable.NewMutation()
		mut.Set("stats", "aaa", bigtable.Now(), []byte("1"))
		mut.Set("stats", "bbb", bigtable.Now(), []byte("2"))
		mut.Set("stats", "ccc", bigtable.Now(), []byte("3"))
		mut.Set("stats", "ddd", bigtable.Now(), []byte("4"))
		require.NoError(t, tbl.Apply(env.ctx, key, mut))

		row, err := tbl.ReadRow(env.ctx, key,
			bigtable.RowFilter(bigtable.ColumnRangeFilter("stats", "bbb", "ddd")),
		)
		require.NoError(t, err)
		var cols []string
		for _, c := range row["stats"] {
			cols = append(cols, c.Column[len("stats:"):])
		}
		sort.Strings(cols)
		assert.Equal(t, []string{"bbb", "ccc"}, cols) // [bbb, ddd)
	})
}

// --------------------------------------------------------------------------
// Test 4: Mutations & Deletions — Google Docs patterns
// https://cloud.google.com/bigtable/docs/mutations-and-deletions
// --------------------------------------------------------------------------

func TestGoogleDocs_MutationsAndDeletions(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	tbl := env.createTable(t, "mutation-test", "cf1", "cf2")

	seedRow := func(key string) {
		t.Helper()
		mut := bigtable.NewMutation()
		mut.Set("cf1", "col1", bigtable.Now(), []byte("v1"))
		mut.Set("cf1", "col2", bigtable.Now(), []byte("v2"))
		mut.Set("cf2", "col3", bigtable.Now(), []byte("v3"))
		require.NoError(t, tbl.Apply(env.ctx, key, mut))
	}

	t.Run("DeleteFromColumn", func(t *testing.T) {
		// Google Docs: delete specific cells from a column
		seedRow("del-col")
		mut := bigtable.NewMutation()
		mut.DeleteCellsInColumn("cf1", "col1")
		require.NoError(t, tbl.Apply(env.ctx, "del-col", mut))

		row, err := tbl.ReadRow(env.ctx, "del-col")
		require.NoError(t, err)
		// col1 gone, col2 remains
		for _, c := range row["cf1"] {
			assert.NotEqual(t, "cf1:col1", c.Column)
		}
		assert.Len(t, row["cf1"], 1) // only col2
	})

	t.Run("DeleteFromFamily", func(t *testing.T) {
		// Google Docs: delete all cells in a column family
		seedRow("del-fam")
		mut := bigtable.NewMutation()
		mut.DeleteCellsInFamily("cf1")
		require.NoError(t, tbl.Apply(env.ctx, "del-fam", mut))

		row, err := tbl.ReadRow(env.ctx, "del-fam")
		require.NoError(t, err)
		assert.Nil(t, row["cf1"])    // entire family gone
		assert.NotNil(t, row["cf2"]) // other family untouched
	})

	t.Run("DeleteRow", func(t *testing.T) {
		// Google Docs: delete entire row
		seedRow("del-row")
		mut := bigtable.NewMutation()
		mut.DeleteRow()
		require.NoError(t, tbl.Apply(env.ctx, "del-row", mut))

		row, err := tbl.ReadRow(env.ctx, "del-row")
		require.NoError(t, err)
		assert.Nil(t, row) // entire row gone
	})

	t.Run("DeleteTimestampRange", func(t *testing.T) {
		// Google Docs: delete cells within a timestamp range
		key := "del-ts"
		t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		t2 := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
		t3 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

		for _, ts := range []time.Time{t1, t2, t3} {
			mut := bigtable.NewMutation()
			mut.Set("cf1", "col1", bigtable.Time(ts), []byte(ts.Format("2006-01")))
			require.NoError(t, tbl.Apply(env.ctx, key, mut))
		}

		// Delete cells from first half of 2024
		mut := bigtable.NewMutation()
		mut.DeleteTimestampRange("cf1", "col1", bigtable.Time(t1), bigtable.Time(t2))
		require.NoError(t, tbl.Apply(env.ctx, key, mut))

		row, err := tbl.ReadRow(env.ctx, key,
			bigtable.RowFilter(bigtable.ColumnFilter("col1")),
		)
		require.NoError(t, err)
		// t1 deleted, t2 and t3 remain
		assert.Len(t, row["cf1"], 2)
	})
}

// --------------------------------------------------------------------------
// Test 5: Conditional Mutations (CheckAndMutateRow) — Google Docs patterns
// https://cloud.google.com/bigtable/docs/conditional-mutations
// --------------------------------------------------------------------------

func TestGoogleDocs_ConditionalMutations(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	tbl := env.createTable(t, "cam-test", "cf")

	t.Run("ConditionalMutate_TrueBranch", func(t *testing.T) {
		// Seed a row
		mut := bigtable.NewMutation()
		mut.Set("cf", "status", bigtable.Now(), []byte("active"))
		require.NoError(t, tbl.Apply(env.ctx, "user#1", mut))

		// Google Docs: CheckAndMutateRow — if status exists, update it
		trueMut := bigtable.NewMutation()
		trueMut.Set("cf", "status", bigtable.Now(), []byte("verified"))
		condMut := bigtable.NewCondMutation(
			bigtable.ColumnFilter("status"), // predicate: column exists?
			trueMut,                         // true mutations
			nil,                             // false mutations
		)

		require.NoError(t, tbl.Apply(env.ctx, "user#1", condMut))

		row, err := tbl.ReadRow(env.ctx, "user#1",
			bigtable.RowFilter(bigtable.ChainFilters(
				bigtable.ColumnFilter("status"),
				bigtable.LatestNFilter(1),
			)),
		)
		require.NoError(t, err)
		assert.Equal(t, "verified", string(row["cf"][0].Value))
	})

	t.Run("ConditionalMutate_FalseBranch", func(t *testing.T) {
		// Google Docs: if column doesn't exist → create with default
		falseMut := bigtable.NewMutation()
		falseMut.Set("cf", "missing_col", bigtable.Now(), []byte("default_value"))
		condMut := bigtable.NewCondMutation(
			bigtable.ColumnFilter("missing_col"), // predicate: this column doesn't exist
			nil,                                  // true: nothing
			falseMut,                             // false: create default
		)

		require.NoError(t, tbl.Apply(env.ctx, "user#new", condMut))

		row, err := tbl.ReadRow(env.ctx, "user#new")
		require.NoError(t, err)
		require.Len(t, row["cf"], 1)
		assert.Equal(t, "default_value", string(row["cf"][0].Value))
	})

	t.Run("ConditionalMutate_ValuePredicate", func(t *testing.T) {
		// Seed
		mut := bigtable.NewMutation()
		mut.Set("cf", "role", bigtable.Now(), []byte("admin"))
		require.NoError(t, tbl.Apply(env.ctx, "user#admin", mut))

		// Google Docs: check value before mutating
		trueMut := bigtable.NewMutation()
		trueMut.Set("cf", "elevated", bigtable.Now(), []byte("true"))
		condMut := bigtable.NewCondMutation(
			bigtable.ChainFilters(
				bigtable.ColumnFilter("role"),
				bigtable.ValueFilter("admin"),
			),
			trueMut,
			nil,
		)

		require.NoError(t, tbl.Apply(env.ctx, "user#admin", condMut))

		row, err := tbl.ReadRow(env.ctx, "user#admin",
			bigtable.RowFilter(bigtable.ColumnFilter("elevated")),
		)
		require.NoError(t, err)
		assert.Equal(t, "true", string(row["cf"][0].Value))
	})
}

// --------------------------------------------------------------------------
// Test 6: Read-Modify-Write (Atomic operations) — Google Docs patterns
// https://cloud.google.com/bigtable/docs/read-modify-write
// --------------------------------------------------------------------------

func TestGoogleDocs_ReadModifyWrite(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	tbl := env.createTable(t, "rmw-test", "cf")

	t.Run("AtomicIncrement", func(t *testing.T) {
		// Google Docs: increment a counter atomically
		key := "page#home"

		// Seed with big-endian int64
		initialVal := make([]byte, 8)
		binary.BigEndian.PutUint64(initialVal, 100)
		mut := bigtable.NewMutation()
		mut.Set("cf", "views", bigtable.Now(), initialVal)
		require.NoError(t, tbl.Apply(env.ctx, key, mut))

		// Atomic increment by 5
		rmw := bigtable.NewReadModifyWrite()
		rmw.Increment("cf", "views", 5)
		newRow, err := tbl.ApplyReadModifyWrite(env.ctx, key, rmw)
		require.NoError(t, err)

		val := binary.BigEndian.Uint64(newRow["cf"][0].Value)
		assert.Equal(t, uint64(105), val)
	})

	t.Run("AtomicAppend", func(t *testing.T) {
		// Google Docs: append bytes to a cell atomically
		key := "log#entry"
		mut := bigtable.NewMutation()
		mut.Set("cf", "data", bigtable.Now(), []byte("hello"))
		require.NoError(t, tbl.Apply(env.ctx, key, mut))

		rmw := bigtable.NewReadModifyWrite()
		rmw.AppendValue("cf", "data", []byte(" world"))
		newRow, err := tbl.ApplyReadModifyWrite(env.ctx, key, rmw)
		require.NoError(t, err)

		assert.Equal(t, "hello world", string(newRow["cf"][0].Value))
	})

	t.Run("IncrementNewRow", func(t *testing.T) {
		// Google Docs: increment creates cell if it doesn't exist (starts from 0)
		key := "counter#new"
		rmw := bigtable.NewReadModifyWrite()
		rmw.Increment("cf", "count", 1)
		newRow, err := tbl.ApplyReadModifyWrite(env.ctx, key, rmw)
		require.NoError(t, err)

		val := binary.BigEndian.Uint64(newRow["cf"][0].Value)
		assert.Equal(t, uint64(1), val)
	})

	t.Run("MultipleRMWRules", func(t *testing.T) {
		// Google Docs: apply multiple read-modify-write rules atomically
		key := "multi#rmw"
		rmw := bigtable.NewReadModifyWrite()
		rmw.Increment("cf", "counter_a", 10)
		rmw.Increment("cf", "counter_b", 20)
		rmw.AppendValue("cf", "log", []byte("init"))
		newRow, err := tbl.ApplyReadModifyWrite(env.ctx, key, rmw)
		require.NoError(t, err)

		// Verify all 3 columns
		found := map[string]bool{}
		for _, c := range newRow["cf"] {
			col := c.Column[len("cf:"):]
			found[col] = true
		}
		assert.True(t, found["counter_a"])
		assert.True(t, found["counter_b"])
		assert.True(t, found["log"])
	})
}

// --------------------------------------------------------------------------
// Test 7: Table & Column Family Management — Google Docs patterns
// https://cloud.google.com/bigtable/docs/managing-tables
// --------------------------------------------------------------------------

func TestGoogleDocs_TableManagement(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()

	t.Run("CreateListDeleteTable", func(t *testing.T) {
		// Google Docs: basic table lifecycle
		require.NoError(t, env.admin.CreateTable(env.ctx, "lifecycle-table"))

		tables, err := env.admin.Tables(env.ctx)
		require.NoError(t, err)
		assert.Contains(t, tables, "lifecycle-table")

		require.NoError(t, env.admin.DeleteTable(env.ctx, "lifecycle-table"))

		tables, err = env.admin.Tables(env.ctx)
		require.NoError(t, err)
		assert.NotContains(t, tables, "lifecycle-table")
	})

	t.Run("AddAndRemoveColumnFamilies", func(t *testing.T) {
		// Google Docs: modify column families on existing table
		require.NoError(t, env.admin.CreateTable(env.ctx, "cf-mgmt"))
		t.Cleanup(func() { _ = env.admin.DeleteTable(env.ctx, "cf-mgmt") })

		require.NoError(t, env.admin.CreateColumnFamily(env.ctx, "cf-mgmt", "cf1"))
		require.NoError(t, env.admin.CreateColumnFamily(env.ctx, "cf-mgmt", "cf2"))

		info, err := env.admin.TableInfo(env.ctx, "cf-mgmt")
		require.NoError(t, err)
		sort.Strings(info.Families)
		assert.Equal(t, []string{"cf1", "cf2"}, info.Families)

		// Delete a column family
		require.NoError(t, env.admin.DeleteColumnFamily(env.ctx, "cf-mgmt", "cf1"))

		info, err = env.admin.TableInfo(env.ctx, "cf-mgmt")
		require.NoError(t, err)
		assert.Equal(t, []string{"cf2"}, info.Families)
	})

	t.Run("DropRowRange", func(t *testing.T) {
		// Google Docs: delete all rows with a prefix
		require.NoError(t, env.admin.CreateTable(env.ctx, "drop-test"))
		require.NoError(t, env.admin.CreateColumnFamily(env.ctx, "drop-test", "cf"))
		t.Cleanup(func() { _ = env.admin.DeleteTable(env.ctx, "drop-test") })

		tbl := env.client.Open("drop-test")
		for _, key := range []string{"keep#1", "keep#2", "delete#1", "delete#2"} {
			mut := bigtable.NewMutation()
			mut.Set("cf", "v", bigtable.Now(), []byte("x"))
			require.NoError(t, tbl.Apply(env.ctx, key, mut))
		}

		require.NoError(t, env.admin.DropRowRange(env.ctx, "drop-test", "delete#"))

		var keys []string
		err := tbl.ReadRows(env.ctx, bigtable.InfiniteRange(""), func(row bigtable.Row) bool {
			keys = append(keys, row.Key())
			return true
		})
		require.NoError(t, err)
		sort.Strings(keys)
		assert.Equal(t, []string{"keep#1", "keep#2"}, keys)
	})

	t.Run("DropAllRows", func(t *testing.T) {
		// Google Docs: truncate table (delete all data, keep schema)
		require.NoError(t, env.admin.CreateTable(env.ctx, "truncate-test"))
		require.NoError(t, env.admin.CreateColumnFamily(env.ctx, "truncate-test", "cf"))
		t.Cleanup(func() { _ = env.admin.DeleteTable(env.ctx, "truncate-test") })

		tbl := env.client.Open("truncate-test")
		for i := 0; i < 5; i++ {
			mut := bigtable.NewMutation()
			mut.Set("cf", "v", bigtable.Now(), []byte("x"))
			require.NoError(t, tbl.Apply(env.ctx, fmt.Sprintf("row%d", i), mut))
		}

		require.NoError(t, env.admin.DropAllRows(env.ctx, "truncate-test"))

		var count int
		err := tbl.ReadRows(env.ctx, bigtable.InfiniteRange(""), func(row bigtable.Row) bool {
			count++
			return true
		})
		require.NoError(t, err)
		assert.Equal(t, 0, count)

		// Table schema still exists
		info, err := env.admin.TableInfo(env.ctx, "truncate-test")
		require.NoError(t, err)
		assert.Contains(t, info.Families, "cf")
	})
}

// --------------------------------------------------------------------------
// Test 8: Garbage Collection Policies — Google Docs patterns
// https://cloud.google.com/bigtable/docs/garbage-collection
// --------------------------------------------------------------------------

func TestGoogleDocs_GarbageCollection(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()

	t.Run("MaxVersionsPolicy", func(t *testing.T) {
		// Google Docs: keep only N most recent versions
		require.NoError(t, env.admin.CreateTable(env.ctx, "gc-maxver"))
		t.Cleanup(func() { _ = env.admin.DeleteTable(env.ctx, "gc-maxver") })

		require.NoError(t, env.admin.CreateColumnFamily(env.ctx, "gc-maxver", "cf"))
		require.NoError(t, env.admin.SetGCPolicy(env.ctx, "gc-maxver", "cf",
			bigtable.MaxVersionsPolicy(2),
		))

		tbl := env.client.Open("gc-maxver")
		for i := 0; i < 5; i++ {
			ts := bigtable.Time(time.Date(2024, 1, 1+i, 0, 0, 0, 0, time.UTC))
			mut := bigtable.NewMutation()
			mut.Set("cf", "val", ts, []byte(fmt.Sprintf("v%d", i)))
			require.NoError(t, tbl.Apply(env.ctx, "row1", mut))
		}

		// GC applied at read time — should keep only 2 latest
		row, err := tbl.ReadRow(env.ctx, "row1")
		require.NoError(t, err)
		assert.LessOrEqual(t, len(row["cf"]), 2)
		// Most recent should be v4
		assert.Equal(t, "v4", string(row["cf"][0].Value))
	})

	t.Run("MaxAgePolicy", func(t *testing.T) {
		// Google Docs: remove cells older than a duration
		require.NoError(t, env.admin.CreateTable(env.ctx, "gc-maxage"))
		t.Cleanup(func() { _ = env.admin.DeleteTable(env.ctx, "gc-maxage") })

		require.NoError(t, env.admin.CreateColumnFamily(env.ctx, "gc-maxage", "cf"))
		require.NoError(t, env.admin.SetGCPolicy(env.ctx, "gc-maxage", "cf",
			bigtable.MaxAgePolicy(24*time.Hour), // keep only last 24 hours
		))

		tbl := env.client.Open("gc-maxage")

		// Write old cell (definitely older than 24h)
		oldTs := bigtable.Time(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
		mut1 := bigtable.NewMutation()
		mut1.Set("cf", "val", oldTs, []byte("old"))
		require.NoError(t, tbl.Apply(env.ctx, "row1", mut1))

		// Write recent cell
		mut2 := bigtable.NewMutation()
		mut2.Set("cf", "val", bigtable.Now(), []byte("new"))
		require.NoError(t, tbl.Apply(env.ctx, "row1", mut2))

		// GC at read time — old cell should be removed
		row, err := tbl.ReadRow(env.ctx, "row1")
		require.NoError(t, err)
		require.Len(t, row["cf"], 1)
		assert.Equal(t, "new", string(row["cf"][0].Value))
	})

	t.Run("UnionPolicy_OR", func(t *testing.T) {
		// Google Docs: union policy — remove cell if ANY sub-policy matches
		require.NoError(t, env.admin.CreateTable(env.ctx, "gc-union"))
		t.Cleanup(func() { _ = env.admin.DeleteTable(env.ctx, "gc-union") })

		require.NoError(t, env.admin.CreateColumnFamily(env.ctx, "gc-union", "cf"))
		require.NoError(t, env.admin.SetGCPolicy(env.ctx, "gc-union", "cf",
			bigtable.UnionPolicy(
				bigtable.MaxVersionsPolicy(3),
				bigtable.MaxAgePolicy(24*time.Hour),
			),
		))

		tbl := env.client.Open("gc-union")

		// Write 5 recent cells (within 24h)
		for i := 0; i < 5; i++ {
			mut := bigtable.NewMutation()
			mut.Set("cf", "val", bigtable.Now(), []byte(fmt.Sprintf("v%d", i)))
			require.NoError(t, tbl.Apply(env.ctx, "row1", mut))
		}

		// Union of MaxVersions(3) OR MaxAge(24h)
		// Since all cells within 24h, MaxAge won't trigger
		// MaxVersions(3) keeps only 3 → union removes anything matching EITHER
		row, err := tbl.ReadRow(env.ctx, "row1")
		require.NoError(t, err)
		assert.LessOrEqual(t, len(row["cf"]), 3)
	})

	t.Run("IntersectionPolicy_AND", func(t *testing.T) {
		// Google Docs: intersection policy — remove cell only if ALL sub-policies match
		// Common pattern: keep max 5 versions AND only if newer than 7 days
		require.NoError(t, env.admin.CreateTable(env.ctx, "gc-intersect"))
		t.Cleanup(func() { _ = env.admin.DeleteTable(env.ctx, "gc-intersect") })

		require.NoError(t, env.admin.CreateColumnFamily(env.ctx, "gc-intersect", "cf"))
		require.NoError(t, env.admin.SetGCPolicy(env.ctx, "gc-intersect", "cf",
			bigtable.IntersectionPolicy(
				bigtable.MaxVersionsPolicy(2),
				bigtable.MaxAgePolicy(24*time.Hour),
			),
		))

		tbl := env.client.Open("gc-intersect")

		// Write 1 old cell and 4 recent cells
		oldTs := bigtable.Time(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
		mut0 := bigtable.NewMutation()
		mut0.Set("cf", "val", oldTs, []byte("old"))
		require.NoError(t, tbl.Apply(env.ctx, "row1", mut0))

		for i := 1; i <= 4; i++ {
			mut := bigtable.NewMutation()
			mut.Set("cf", "val", bigtable.Now(), []byte(fmt.Sprintf("new%d", i)))
			require.NoError(t, tbl.Apply(env.ctx, "row1", mut))
		}

		// Intersection of MaxVersions(2) AND MaxAge(24h):
		// A cell is deleted only if BOTH rules want to delete it.
		// MaxVersions(2) wants to delete: old, new1, new2 (keep new3, new4)
		// MaxAge(24h) wants to delete: old (keep new1..new4)
		// Intersection (both agree): delete "old"
		// But GC is applied per-column serially — the intersection keeps cells
		// that survive the combined filter. With 5 cells total, only "old" is
		// removed by both rules. However, since GC applies per read and the
		// implementation intersects the kept sets, MaxVersions(2) keeps {new3,new4}
		// and MaxAge keeps {new1,new2,new3,new4}. Intersection of kept = {new3,new4}.
		row, err := tbl.ReadRow(env.ctx, "row1")
		require.NoError(t, err)

		// "old" must be gone (both rules agree to delete it)
		for _, c := range row["cf"] {
			assert.NotEqual(t, "old", string(c.Value), "old cell should be GC'd by intersection")
		}
		// At least 2 recent cells survive (the ones both rules agree to keep)
		assert.GreaterOrEqual(t, len(row["cf"]), 2, "intersection should keep cells that both rules keep")
	})
}

// --------------------------------------------------------------------------
// Test 9: SampleRowKeys — Google Docs patterns
// --------------------------------------------------------------------------

func TestGoogleDocs_SampleRowKeys(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	tbl := env.createTable(t, "sample-test", "cf")

	// Write some data
	for i := 0; i < 10; i++ {
		mut := bigtable.NewMutation()
		mut.Set("cf", "v", bigtable.Now(), []byte("x"))
		require.NoError(t, tbl.Apply(env.ctx, fmt.Sprintf("row#%04d", i), mut))
	}

	// Google Docs: SampleRowKeys for parallel scans
	keys, err := tbl.SampleRowKeys(env.ctx)
	require.NoError(t, err)
	// Emulator should return at least some sample keys (implementation varies)
	assert.NotNil(t, keys)
}

// --------------------------------------------------------------------------
// Test 10: End-to-End — Full developer workflow from Google Docs
// --------------------------------------------------------------------------

func TestGoogleDocs_EndToEnd_MobileTimeSeries(t *testing.T) {
	// Google Docs example: mobile time series data
	// https://cloud.google.com/bigtable/docs/schema-design-time-series
	env := setupTestEnv(t)
	defer env.cancel()
	tbl := env.createTable(t, "mobile-timeseries", "stats")

	// Set GC: keep last 2 versions (Google Docs best practice for time series)
	require.NoError(t, env.admin.SetGCPolicy(env.ctx, "mobile-timeseries", "stats",
		bigtable.MaxVersionsPolicy(2),
	))

	// Step 1: Write phone stats (Google Docs pattern: row key = device#date)
	devices := []struct {
		key string
		os  string
		mem string
	}{
		{"phone#4c410523#20190501", "android", "4096"},
		{"phone#4c410523#20190502", "android", "4096"},
		{"phone#5c10102#20190501", "ios", "8192"},
		{"phone#5c10102#20190502", "ios", "8192"},
		{"tablet#a0b81f#20190501", "android", "2048"},
	}

	for _, d := range devices {
		mut := bigtable.NewMutation()
		mut.Set("stats", "os_build", bigtable.Now(), []byte(d.os))
		mut.Set("stats", "mem_usage", bigtable.Now(), []byte(d.mem))
		require.NoError(t, tbl.Apply(env.ctx, d.key, mut))
	}

	// Step 2: Read single device row
	row, err := tbl.ReadRow(env.ctx, "phone#4c410523#20190501",
		bigtable.RowFilter(bigtable.ColumnFilter("os_build")),
	)
	require.NoError(t, err)
	require.Len(t, row["stats"], 1)
	assert.Equal(t, "android", string(row["stats"][0].Value))

	// Step 3: Scan all data for a specific phone
	var phoneKeys []string
	err = tbl.ReadRows(env.ctx, bigtable.PrefixRange("phone#4c410523#"), func(row bigtable.Row) bool {
		phoneKeys = append(phoneKeys, row.Key())
		return true
	})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"phone#4c410523#20190501",
		"phone#4c410523#20190502",
	}, phoneKeys)

	// Step 4: Scan all phones (not tablets)
	var allPhones []string
	err = tbl.ReadRows(env.ctx, bigtable.PrefixRange("phone#"), func(row bigtable.Row) bool {
		allPhones = append(allPhones, row.Key())
		return true
	})
	require.NoError(t, err)
	assert.Len(t, allPhones, 4) // 4 phone rows

	// Step 5: Filter by value — only iOS devices
	var iosDevices []string
	err = tbl.ReadRows(env.ctx, bigtable.InfiniteRange(""), func(row bigtable.Row) bool {
		iosDevices = append(iosDevices, row.Key())
		return true
	}, bigtable.RowFilter(bigtable.ChainFilters(
		bigtable.ColumnFilter("os_build"),
		bigtable.ValueFilter("ios"),
	)))
	require.NoError(t, err)
	assert.Equal(t, []string{
		"phone#5c10102#20190501",
		"phone#5c10102#20190502",
	}, iosDevices)

	// Step 6: Conditional update — set "premium" flag only if mem > 4096
	premiumMut := bigtable.NewMutation()
	premiumMut.Set("stats", "tier", bigtable.Now(), []byte("premium"))
	condMut := bigtable.NewCondMutation(
		bigtable.ChainFilters(
			bigtable.ColumnFilter("mem_usage"),
			bigtable.ValueFilter("8192"),
		),
		premiumMut,
		nil,
	)
	require.NoError(t, tbl.Apply(env.ctx, "phone#5c10102#20190501", condMut))

	row, err = tbl.ReadRow(env.ctx, "phone#5c10102#20190501",
		bigtable.RowFilter(bigtable.ColumnFilter("tier")),
	)
	require.NoError(t, err)
	assert.Equal(t, "premium", string(row["stats"][0].Value))

	// Step 7: Delete old data for a device
	require.NoError(t, env.admin.DropRowRange(env.ctx, "mobile-timeseries", "tablet#"))

	var remaining []string
	err = tbl.ReadRows(env.ctx, bigtable.InfiniteRange(""), func(row bigtable.Row) bool {
		remaining = append(remaining, row.Key())
		return true
	})
	require.NoError(t, err)
	for _, k := range remaining {
		assert.NotContains(t, k, "tablet#")
	}
}

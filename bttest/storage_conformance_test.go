package bttest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	_ "github.com/glebarez/go-sqlite"
	_ "github.com/lib/pq"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	storageConformanceBackendEnv  = "LITTLE_BIGTABLE_CONFORMANCE_BACKEND"
	storageConformancePostgresDSN = "LITTLE_BIGTABLE_POSTGRES_DSN"
)

type storageConformanceBackend struct {
	name      string
	dialect   string
	sqlDriver string
	dsn       string
}

type storageConformanceHarness struct {
	db     *sql.DB
	server *Server
	conn   *grpc.ClientConn
	admin  *bigtable.AdminClient
	client *bigtable.Client
}

func TestStorageConformance(t *testing.T) {
	for _, backend := range storageConformanceBackends(t) {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			runStoragePersistenceContract(t, backend)
		})
	}
}

func storageConformanceBackends(t *testing.T) []storageConformanceBackend {
	t.Helper()

	sqliteBackend := storageConformanceBackend{
		name:      "sqlite",
		dialect:   "sqlite3",
		sqlDriver: "sqlite",
		dsn:       "file:" + filepath.Join(t.TempDir(), "conformance.db") + "?cache=shared",
	}
	postgresDSN := os.Getenv(storageConformancePostgresDSN)
	postgresBackend := storageConformanceBackend{
		name:      "postgres",
		dialect:   "postgres",
		sqlDriver: "postgres",
		dsn:       postgresDSN,
	}

	switch requested := os.Getenv(storageConformanceBackendEnv); requested {
	case "", "all":
		backends := []storageConformanceBackend{sqliteBackend}
		if postgresDSN != "" {
			backends = append(backends, postgresBackend)
		}
		return backends
	case "sqlite":
		return []storageConformanceBackend{sqliteBackend}
	case "postgres":
		if postgresDSN == "" {
			t.Fatalf("%s=postgres requires %s", storageConformanceBackendEnv, storageConformancePostgresDSN)
		}
		return []storageConformanceBackend{postgresBackend}
	default:
		t.Fatalf("unsupported %s value %q", storageConformanceBackendEnv, requested)
		return nil
	}
}

func runStoragePersistenceContract(t *testing.T, backend storageConformanceBackend) {
	t.Helper()

	previousDialect, previousStrict := currentDialect(), isStrictAdmin()
	ConfigureStorage(backend.dialect, false)
	t.Cleanup(func() { ConfigureStorage(string(previousDialect), previousStrict) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const (
		projectID  = "conformance-project"
		instanceID = "conformance-instance"
		family     = "cf"
		qualifier  = "value"
		rowKey     = "row-0001"
	)
	tableID := "storage-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	parent := "projects/" + projectID + "/instances/" + instanceID
	t.Cleanup(func() { cleanupStorageConformanceData(t, backend, parent, tableID) })

	first := openStorageConformanceHarness(t, ctx, backend, projectID, instanceID)
	if err := first.admin.CreateTable(ctx, tableID); err != nil {
		first.close()
		t.Fatal(err)
	}
	if err := first.admin.CreateColumnFamily(ctx, tableID, family); err != nil {
		first.close()
		t.Fatal(err)
	}
	mutation := bigtable.NewMutation()
	mutation.Set(family, qualifier, bigtable.Now(), []byte("persisted-value"))
	if err := first.client.Open(tableID).Apply(ctx, rowKey, mutation); err != nil {
		first.close()
		t.Fatal(err)
	}
	first.close()

	second := openStorageConformanceHarness(t, ctx, backend, projectID, instanceID)
	defer second.close()

	tables, err := second.admin.Tables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(tables, tableID) {
		t.Fatalf("persisted table %q not found in %v", tableID, tables)
	}
	info, err := second.admin.TableInfo(ctx, tableID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(info.Families, family) {
		t.Fatalf("persisted family %q not found in %v", family, info.Families)
	}
	storedRow, err := second.client.Open(tableID).ReadRow(ctx, rowKey)
	if err != nil {
		t.Fatal(err)
	}
	cells := storedRow[family]
	if len(cells) != 1 || cells[0].Column != family+":"+qualifier || string(cells[0].Value) != "persisted-value" {
		t.Fatalf("persisted row mismatch: %#v", storedRow)
	}
}

func openStorageConformanceHarness(t *testing.T, ctx context.Context, backend storageConformanceBackend, projectID, instanceID string) *storageConformanceHarness {
	t.Helper()

	db, err := sql.Open(backend.sqlDriver, backend.dsn)
	if err != nil {
		t.Fatal(err)
	}
	if backend.name == "sqlite" {
		db.SetMaxOpenConns(1)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := CreateTables(ctx, db); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}

	srv, err := NewServer("127.0.0.1:0", db)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	conn, err := grpc.DialContext(ctx, srv.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		srv.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	admin, err := bigtable.NewAdminClient(ctx, projectID, instanceID, option.WithGRPCConn(conn))
	if err != nil {
		_ = conn.Close()
		srv.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	client, err := bigtable.NewClient(ctx, projectID, instanceID, option.WithGRPCConn(conn))
	if err != nil {
		_ = admin.Close()
		_ = conn.Close()
		srv.Close()
		_ = db.Close()
		t.Fatal(err)
	}

	return &storageConformanceHarness{db: db, server: srv, conn: conn, admin: admin, client: client}
}

func (h *storageConformanceHarness) close() {
	_ = h.client.Close()
	_ = h.admin.Close()
	_ = h.conn.Close()
	h.server.Close()
	_ = h.db.Close()
}

func cleanupStorageConformanceData(t *testing.T, backend storageConformanceBackend, parent, tableID string) {
	t.Helper()

	db, err := sql.Open(backend.sqlDriver, backend.dsn)
	if err != nil {
		t.Errorf("open storage conformance cleanup database: %v", err)
		return
	}
	defer db.Close()

	placeholder := func(position int) string { return "?" }
	if backend.name == "postgres" {
		placeholder = func(position int) string { return "$" + strconv.Itoa(position) }
	}
	tableName := parent + "/tables/" + tableID
	if _, err := db.Exec("DELETE FROM change_log_t WHERE table_name = "+placeholder(1), tableName); err != nil {
		t.Errorf("clean change_log_t storage conformance data: %v", err)
	}
	for _, table := range []string{"rows_t", "tables_t"} {
		query := fmt.Sprintf("DELETE FROM %s WHERE parent = %s AND table_id = %s", table, placeholder(1), placeholder(2))
		if _, err := db.Exec(query, parent, tableID); err != nil {
			t.Errorf("clean %s storage conformance data: %v", table, err)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

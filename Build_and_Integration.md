# LocalCloud Integration Guide

This document covers how to build the Bigtable emulator from source, integrate
it into Go projects as a library, release new versions, and build Docker images.

## Building from Source

### Prerequisites

- Go 1.27.0
- No C compiler. Both SQL drivers are pure Go: SQLite uses
  `github.com/glebarez/go-sqlite` (registered under the driver name `sqlite3`
  in `little_bigtable.go`) and PostgreSQL uses `github.com/lib/pq`.

### Build the binary

```bash
make
# Output: build/little_bigtable
```

Or directly:

```bash
go build -o little_bigtable .
```

### Build a static binary (for containers)

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o little_bigtable .
```

This is how the release workflow, `dist.sh` and the repository `Dockerfile`
build. The `Dockerfile` cross-compiles from `--platform=$BUILDPLATFORM` with
`CGO_ENABLED=0`. For a multi-platform image:

```bash
make -f Makefile.localcloud docker-buildx            # OCI archive in dist/
make -f Makefile.localcloud docker-buildx PUSH=true  # push to the registry
```

The GoogleSQL engine (`bttest/internal/gsql`) is always built; no build tag is
needed.

### Run tests

```bash
go test ./bttest/ -count=1 -timeout 60s
```

The backend-neutral persistence contract runs on SQLite by default:

```bash
LITTLE_BIGTABLE_CONFORMANCE_BACKEND=sqlite \
  go test ./bttest/ -count=1 -run '^TestStorageConformance$' -v
```

Run the identical contract against a PostgreSQL test database with:

```bash
LITTLE_BIGTABLE_CONFORMANCE_BACKEND=postgres \
LITTLE_BIGTABLE_POSTGRES_DSN='postgres://postgres:postgres@localhost:5432/little_bigtable_test?sslmode=disable' \
  go test ./bttest/ -count=1 -run '^TestStorageConformance$' -v
```

CI pins the standalone `cbt` CLI and requires its subprocess smoke test. To
reproduce that lane locally:

```bash
go install cloud.google.com/go/cbt@v0.0.0-20260810145131-fe593de7bc1a
CBT_BIN="$(go env GOPATH)/bin/cbt" \
CBT_REQUIRED=true \
CBT_EXPECTED_VERSION=v0.0.0-20260810145131-fe593de7bc1a \
  go test ./bttest/ -count=1 -run '^TestCBTClientConformance$' -v
```

### Verify build

```bash
./build/little_bigtable -version
```

## Go Library (Recommended)

The `bttest` package can be imported directly into any Go project — no Docker
image, no separate process, no port management. Single binary ships everything.

### Add the dependency

**From GitHub (published release):**

```bash
# Configure Go for private repos (one-time setup)
export GOPRIVATE=github.com/jhsenjaliya/*
git config --global url."git@github.com:jhsenjaliya/".insteadOf "https://github.com/jhsenjaliya/"

# Add dependency
go get github.com/jhsenjaliya/little_bigtable/bttest@v0.4.0
```

**From local checkout (development):**

Add a `replace` directive in your `go.mod`:

```
require github.com/jhsenjaliya/little_bigtable v0.4.0

replace github.com/jhsenjaliya/little_bigtable => ../local_cloud_dependencies/bigtable-emulator-extended
```

Remove the `replace` directive before committing — it should only be used for
local development. CI/CD should pull from GitHub.

### Build requirements

| Backend | CGO required | C compiler needed | Notes |
|---------|-------------|-------------------|-------|
| SQLite | No | No | `github.com/glebarez/go-sqlite` is pure Go; register it as `sqlite3` |
| PostgreSQL | No | No | `lib/pq` is pure Go |

The `bttest` package does not import a driver. Register one in your program;
for SQLite, call `sqlite.RegisterAsSQLITE3()` so the `sqlite3` driver name used
by `bttest.ConfigureStorage("sqlite3", …)` resolves.

### Import and start

```go
package yourpkg

import (
    "context"
    "database/sql"
    "log"

    sqlite "github.com/glebarez/go-sqlite" // SQLite driver (pure Go)
    "github.com/jhsenjaliya/little_bigtable/bttest"
    // _ "github.com/lib/pq"               // PostgreSQL driver (pure Go)
    "google.golang.org/grpc"
)

func init() { sqlite.RegisterAsSQLITE3() } // register as "sqlite3"

func StartBigtableEmulator(ctx context.Context) (*bttest.Server, error) {
    bttest.ConfigureStorage("sqlite3", true)

    db, err := sql.Open("sqlite3", "file:bigtable.db?cache=shared")
    if err != nil {
        return nil, err
    }
    db.SetMaxOpenConns(1) // required for SQLite

    if err := bttest.CreateTables(ctx, db); err != nil {
        return nil, err
    }

    return bttest.NewServer("localhost:0", db,
        grpc.MaxRecvMsgSize(256<<20),
        grpc.MaxSendMsgSize(256<<20),
    )
}
```

### Connect SDK clients

Set the environment variable (works with all Bigtable SDKs):

```go
os.Setenv("BIGTABLE_EMULATOR_HOST", srv.Addr)
```

Or explicit gRPC connection:

```go
conn, err := grpc.Dial(emulatorAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
client, err := bigtable.NewClient(ctx, "local-project", "local-instance",
    option.WithGRPCConn(conn),
)
```

### Use in tests

```go
func TestWithBigtable(t *testing.T) {
    ctx := context.Background()
    bttest.ConfigureStorage("sqlite3", false)

    db, err := sql.Open("sqlite3", ":memory:")
    require.NoError(t, err)
    defer db.Close()
    db.SetMaxOpenConns(1)
    bttest.CreateTables(ctx, db)

    srv, err := bttest.NewServer("localhost:0", db)
    require.NoError(t, err)
    defer srv.Close()

    t.Setenv("BIGTABLE_EMULATOR_HOST", srv.Addr)

    client, err := bigtable.NewClient(ctx, "proj", "inst")
    require.NoError(t, err)
    defer client.Close()

    // ... test with real Bigtable SDK operations
}
```

### PostgreSQL backend

For shared environments or persistent data across restarts:

```go
bttest.ConfigureStorage("postgres", true)
db, err := sql.Open("postgres", "postgres://user@localhost/bigtable?sslmode=disable")
// No SetMaxOpenConns(1) needed for PostgreSQL.
```

### API summary

| Function | Purpose |
|----------|---------|
| `bttest.ConfigureStorage(driver, strictAdmin)` | Set SQL dialect and admin mode. Call before `NewServer`. |
| `bttest.CreateTables(ctx, db)` | Initialize schema. Safe to call on existing DB. |
| `bttest.NewServer(addr, db, ...grpc.ServerOption)` | Start gRPC server. Returns `*Server` with `.Addr` and `.Close()`. |
| `bttest.CompatibilityLedger()` | Return checked-in RPC/field dispositions with their owning tests (89 RPC entries, all `test_verified`); matches `BIGTABLE_COMPATIBILITY.md` Appendix A. |

## Releasing a New Version

### Prerequisites

- All tests pass: `go test ./bttest/ -count=1`
- `go.sum` is up to date: `go mod tidy`
- Changes committed and pushed to `master`

### Release steps

```bash
# 1. Verify tests pass.
go test ./bttest/ -count=1 -timeout 60s

# 2. Tag the release. Use semver.
#    Bump major for breaking API changes.
#    Bump minor for new features (new RPCs, new fields).
#    Bump patch for bug fixes.
git tag v0.4.0

# 3. Push tag to GitHub.
git push origin master --tags

# 4. Verify the module is fetchable.
GOPRIVATE=github.com/jhsenjaliya/* go list -m github.com/jhsenjaliya/little_bigtable@v0.4.0
```

### Consuming the release

In the consuming project:

```bash
GOPRIVATE=github.com/jhsenjaliya/* go get github.com/jhsenjaliya/little_bigtable/bttest@v0.4.0
```

Go caches the module. No `replace` directive needed when using published tags.

### Version history

| Version | Changes |
|---------|---------|
| `v0.3.0` | PostgreSQL backend, instance/cluster admin, change streams |
| `v0.4.0` | Table deletion protection, IAM stubs, authorized views, backups, logical views, CopyBackup, RestoreTable |
| `v0.5.0` (`dd5f9e7`) | Parity iteration against `cloud.google.com/go/bigtable` v1.58.0 (see [`BIGTABLE_COMPATIBILITY.md`](BIGTABLE_COMPATIBILITY.md)): atomic single-row writes with per-entry `MutateRows` codes and idempotency tokens; full filter set (`Interleave` duplicates, `Sink`, `ValueBitmask`) and corrected GC intersection; authorized-view and app-profile enforcement; production-style `ReadRows` chunking, request stats and `SampleRowKeys` ranges; `UndeleteTable`, initial splits, row key schema, aggregate families, schema bundles; durable LRO store and persisted IAM policies; backup data snapshots; opt-in change streams with retention; session protocol; GoogleSQL `PrepareQuery`/`ExecuteQuery`, executable logical views and GoogleSQL continuous materialized views; pure-Go multi-platform image build. One-way storage migration on first start. Every registered RPC has a named owning test. |

## Docker Image (Standalone)

For non-Go consumers or containerized deployments.

### Build

```bash
make -f Makefile.localcloud docker-build
```

Or directly:

```bash
docker build --pull=false -t bigtable-emulator-extended:latest .
```

Override base images:

```bash
docker build \
  --build-arg GO_BASE_IMAGE=golang:1.27.0-alpine \
  --build-arg RUNTIME_BASE_IMAGE=alpine:3.22 \
  -t bigtable-emulator-extended:latest .
```

Corporate TLS inspection — pass CA bundle:

```bash
docker build --pull=false \
  --secret id=ca_bundle,src="$HOME/ca-bundle.pem" \
  -t bigtable-emulator-extended:latest .
```

### Offline builds

Always run `go mod tidy` first to ensure `go.sum` is complete.

**Vendor bundle (recommended):**

```bash
rm -rf .docker/offline-go/vendor
go mod vendor -o .docker/offline-go/vendor
make -f Makefile.localcloud docker-build-offline
```

**Module cache copy:**

```bash
rm -rf .docker/offline-go/mod
mkdir -p .docker/offline-go/mod
rsync -a "$(go env GOMODCACHE)/" .docker/offline-go/mod/
make -f Makefile.localcloud docker-build-offline
```

**Troubleshooting:**

| Error | Cause | Fix |
|-------|-------|-----|
| `missing go.sum entry for module` | `go.sum` incomplete | Run `go mod tidy` before staging deps |
| `GO_OFFLINE=true requires .../vendor or .../mod` | No deps staged | Run vendor or rsync steps above |
| `cannot find module providing package ...` | Stale vendor/cache | Re-run `go mod vendor` or `rsync` |

### Run standalone

```bash
docker run -p 8087:8087 bigtable-emulator-extended:latest \
  -host 0.0.0.0 \
  -port 8087 \
  -database-driver sqlite3 \
  -db-file /data/bigtable.db
```

```bash
export BIGTABLE_EMULATOR_HOST=localhost:8087
```

### Consume in LocalCloud Dockerfile

```bash
docker build \
  --build-arg BIGTABLE_EMULATOR_IMAGE=bigtable-emulator-extended:latest \
  -t localcloud/localcloud:latest \
  /path/to/localcloud
```

## Feature Coverage

See [BIGTABLE_COMPATIBILITY.md](BIGTABLE_COMPATIBILITY.md) for full feature
parity with production Bigtable.

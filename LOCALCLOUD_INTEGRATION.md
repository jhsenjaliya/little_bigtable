# LocalCloud Integration Guide

This document covers how to integrate the Bigtable emulator into LocalCloud and
other Go projects as a library, how to release new versions, and Docker image
builds for standalone deployment.

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
| SQLite | Yes | Yes (`gcc` or `clang`) | `go-sqlite3` is a CGO wrapper |
| PostgreSQL | No | No | `lib/pq` is pure Go |

For SQLite, ensure a C compiler is available:

```bash
# macOS — included with Xcode CLI tools
xcode-select --install

# Ubuntu/Debian
apt-get install gcc

# Alpine
apk add gcc musl-dev
```

PostgreSQL mode avoids CGO entirely — recommended for CI environments without
C toolchains.

### Import and start

```go
package yourpkg

import (
    "context"
    "database/sql"
    "log"

    "github.com/jhsenjaliya/little_bigtable/bttest"
    _ "github.com/mattn/go-sqlite3"  // SQLite driver (CGO)
    // _ "github.com/lib/pq"         // PostgreSQL driver (pure Go, no CGO)
    "google.golang.org/grpc"
)

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
  --build-arg GO_BASE_IMAGE=golang:1.25-alpine \
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

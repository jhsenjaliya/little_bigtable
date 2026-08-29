# LocalCloud Integration Guide

This document covers how to build, release, and integrate the Bigtable emulator
into Go projects as a library, and Docker image builds for standalone deployment.

All releases are from the `jay-bigtable-extended` branch. Versioning: `v0.0.x`.

## Releasing a New Version

Releases use Go 1.27.0 locally and in GitHub Actions.

### Steps

```bash
# 1. Switch to the release branch.
git checkout jay-bigtable-extended
git pull origin jay-bigtable-extended

# 2. Verify tests pass.
go test ./bttest/ -count=1 -timeout 60s

# 3. Verify build.
go build -o build/little_bigtable .
./build/little_bigtable -version

# 4. Tag the release (increment x in v0.0.x).
git tag -a v0.0.2 -m "v0.0.2 - description of changes"

# 5. Push the tag.
git push origin v0.0.2

# 6. Verify the module is fetchable (from another machine or directory).
GOPRIVATE=github.com/jhsenjaliya/* \
  go list -m github.com/jhsenjaliya/little_bigtable@v0.0.2
```

### Versioning

| Version | Changes |
|---------|---------|
| `v0.0.1` | Initial extended emulator: PostgreSQL persistence, instance/cluster admin, change streams, IAM stubs, authorized views, backups, logical views, deletion protection, AddToCell/MergeToCell |

### Important notes

- Always release from `jay-bigtable-extended` branch, never from `master`
- `master` tracks upstream `bitly/little_bigtable` and should not be tagged
- Tags on any branch work for `go get` — Go modules resolve tags to commits regardless of branch
- Requires Go 1.27.0 locally; GitHub Actions use the same version

## Building from Source

### Prerequisites

- Go 1.27.0
- C compiler (`gcc` or `clang`) for SQLite via `go-sqlite3`

### Build

```bash
make
# Output: build/little_bigtable
```

Or directly:

```bash
go build -o little_bigtable .
```

### Static binary (for containers)

```bash
CGO_ENABLED=1 go build \
  -trimpath \
  -ldflags="-s -w -linkmode external -extldflags -static" \
  -o little_bigtable .
```

### Run tests

```bash
go test ./bttest/ -count=1 -timeout 60s
```

## Go Library (Recommended)

The `bttest` package can be imported directly into any Go project — no Docker
image, no separate process. Single binary ships everything.

### Add the dependency

**From GitHub (published release):**

```bash
export GOPRIVATE=github.com/jhsenjaliya/*
git config --global url."git@github.com:jhsenjaliya/".insteadOf "https://github.com/jhsenjaliya/"

go get github.com/jhsenjaliya/little_bigtable/bttest@v0.0.1
```

**From local checkout (development only):**

```
require github.com/jhsenjaliya/little_bigtable v0.0.1

replace github.com/jhsenjaliya/little_bigtable => ../local_cloud_dependencies/bigtable-emulator-extended
```

Remove the `replace` directive before committing.

### Build requirements

| Backend | CGO required | C compiler needed | Notes |
|---------|-------------|-------------------|-------|
| SQLite | Yes | Yes (`gcc` or `clang`) | `go-sqlite3` is a CGO wrapper |
| PostgreSQL | No | No | `lib/pq` is pure Go |

### Import and start

```go
package yourpkg

import (
    "context"
    "database/sql"
    "log"

    "github.com/jhsenjaliya/little_bigtable/bttest"
    _ "github.com/mattn/go-sqlite3"
    "google.golang.org/grpc"
)

func StartBigtableEmulator(ctx context.Context) (*bttest.Server, error) {
    bttest.ConfigureStorage("sqlite3", true)

    db, err := sql.Open("sqlite3", "file:bigtable.db?cache=shared")
    if err != nil {
        return nil, err
    }
    db.SetMaxOpenConns(1)

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
}
```

### PostgreSQL backend

```go
bttest.ConfigureStorage("postgres", true)
db, err := sql.Open("postgres", "postgres://user@localhost/bigtable?sslmode=disable")
```

### API summary

| Function | Purpose |
|----------|---------|
| `bttest.ConfigureStorage(driver, strictAdmin)` | Set SQL dialect and admin mode. Call before `NewServer`. |
| `bttest.CreateTables(ctx, db)` | Initialize schema. Safe to call on existing DB. |
| `bttest.NewServer(addr, db, ...grpc.ServerOption)` | Start gRPC server. Returns `*Server` with `.Addr` and `.Close()`. |

## Docker Image (Standalone)

For non-Go consumers or containerized deployments.

### Build

```bash
make -f Makefile.localcloud docker-build
```

### Offline builds

```bash
go mod tidy
go mod vendor -o .docker/offline-go/vendor
make -f Makefile.localcloud docker-build-offline
```

### Stage for LocalCloud Docker build

```bash
cd /path/to/localcloud
bash build.sh
```

`build.sh` copies source from `../local_cloud_dependencies/bigtable-emulator-extended/`
into `.build/`, vendors deps, and builds via the localcloud Dockerfile.

## Feature Coverage

See [BIGTABLE_COMPATIBILITY.md](BIGTABLE_COMPATIBILITY.md) for full feature
parity with production Bigtable.

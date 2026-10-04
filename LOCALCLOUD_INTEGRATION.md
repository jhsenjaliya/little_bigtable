# LocalCloud Integration Guide

This document covers how to build, release, and integrate the Bigtable emulator
into Go projects as a library, and Docker image builds for standalone deployment.

All releases are from the `jay-bigtable-extended` branch. Versioning: semantic `vX.Y.Z` tags.

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

# 4. Tag the release (semantic version; examples use v0.0.2).
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
| `v0.5.0` (`dd5f9e7`) | Parity iteration against `cloud.google.com/go/bigtable` v1.58.0; see [`BIGTABLE_COMPATIBILITY.md`](BIGTABLE_COMPATIBILITY.md) and [`docs/superpowers/plans/2026-10-04-bigtable-parity-implementation-plan.md`](docs/superpowers/plans/2026-10-04-bigtable-parity-implementation-plan.md). Atomic writes, full filter set, authorized-view/app-profile enforcement, `ReadRows` chunking and stats, `UndeleteTable`, schema bundles, durable LROs, persisted IAM, backup snapshots, opt-in change streams, session protocol, GoogleSQL query/logical views/continuous materialized views; pure-Go multi-platform image. One-way storage migration on first start. LocalCloud pins `dd5f9e7`; LocalCloud image build and platform tests pending. |

### Important notes

- Always release from `jay-bigtable-extended` branch, never from `master`
- `master` tracks upstream `bitly/little_bigtable` and should not be tagged
- Tags on any branch work for `go get` — Go modules resolve tags to commits regardless of branch
- Requires Go 1.27.0 locally; GitHub Actions use the same version

## Building from Source

### Prerequisites

- Go 1.27.0
- No C compiler: SQLite uses the pure-Go `github.com/glebarez/go-sqlite`
  driver (registered as `sqlite3` in `little_bigtable.go`); PostgreSQL uses the
  pure-Go `github.com/lib/pq`.

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
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o little_bigtable .
```

The repository `Dockerfile` builds the same way (`CGO_ENABLED=0`, cross-compiled
from `--platform=$BUILDPLATFORM`); `make -f Makefile.localcloud docker-buildx`
builds a multi-platform image. The GoogleSQL engine is always built.

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
| SQLite | No | No | `github.com/glebarez/go-sqlite` is pure Go; register it as `sqlite3` |
| PostgreSQL | No | No | `lib/pq` is pure Go |

### Import and start

```go
package yourpkg

import (
    "context"
    "database/sql"
    "log"

    sqlite "github.com/glebarez/go-sqlite"
    "github.com/jhsenjaliya/little_bigtable/bttest"
    "google.golang.org/grpc"
)

func init() { sqlite.RegisterAsSQLITE3() } // register as "sqlite3"

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

The LocalCloud `Dockerfile` stage `bigtable-build` fetches
`github.com/jhsenjaliya/little_bigtable@${LITTLE_BIGTABLE_VERSION}` and builds it;
`build.sh` accepts `LITTLE_BIGTABLE_VERSION` to override the default revision
set in that `Dockerfile`. Publish (push) the revision before building
LocalCloud against it.

## Feature Coverage

See [BIGTABLE_COMPATIBILITY.md](BIGTABLE_COMPATIBILITY.md) for full feature
parity with production Bigtable.

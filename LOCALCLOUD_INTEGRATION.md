# LocalCloud Integration Guide

This document covers how to build, run, and integrate the Bigtable emulator
into LocalCloud — both as a Docker image and as an embedded Go package.

## Docker Image

### Build

```bash
docker build --pull=false -t bigtable-emulator-extended:latest .
```

The Dockerfile defaults to Public ECR's Docker official image mirror for base
images to avoid Docker Hub auth/DNS failures in constrained networks. Override
the base images when needed:

```bash
docker build \
  --build-arg GO_BASE_IMAGE=golang:1.25-alpine \
  --build-arg RUNTIME_BASE_IMAGE=alpine:3.22 \
  -t bigtable-emulator-extended:latest .
```

If Go module downloads fail on corporate TLS inspection, pass the local CA
bundle as a BuildKit secret:

```bash
docker build --pull=false \
  --secret id=ca_bundle,src="$HOME/paypal-ca-bundle.pem" \
  -t bigtable-emulator-extended:latest .
```

### Offline Builds

Offline builds require no network access during `docker build`. Dependencies
must be staged into the build context beforehand (default: `.docker/offline-go/`,
already gitignored).

**Prerequisites** — always run these first to ensure `go.sum` is complete:

```bash
go mod tidy
```

#### Option A: Vendor bundle (recommended)

Copies all dependencies as source into a `vendor/` directory. Most reliable —
no module cache subtleties.

```bash
rm -rf .docker/offline-go/vendor
go mod vendor -o .docker/offline-go/vendor

docker build --pull=false \
  --build-arg GO_OFFLINE=true \
  -t bigtable-emulator-extended:latest .
```

#### Option B: Module cache copy

Copies your local `$GOMODCACHE` into the build context. Uses more disk but
preserves the exact cache layout.

```bash
rm -rf .docker/offline-go/mod
mkdir -p .docker/offline-go/mod
rsync -a "$(go env GOMODCACHE)/" .docker/offline-go/mod/

docker build --pull=false \
  --build-arg GO_OFFLINE=true \
  -t bigtable-emulator-extended:latest .
```

#### Make shortcut

```bash
make -f Makefile.localcloud docker-build-offline
```

#### Troubleshooting offline builds

| Error | Cause | Fix |
|-------|-------|-----|
| `missing go.sum entry for module` | `go.sum` incomplete | Run `go mod tidy` before staging deps |
| `GO_OFFLINE=true requires .docker/offline-go/vendor or .../mod` | No deps staged | Run Option A or B above |
| `cannot find module providing package ...` | Stale vendor/cache | Re-run `go mod vendor` or `rsync` |

### Consume in LocalCloud Dockerfile

LocalCloud's Dockerfile can consume the built image as a named build stage:

```bash
docker build \
  --build-arg BIGTABLE_EMULATOR_IMAGE=bigtable-emulator-extended:latest \
  -t localcloud/localcloud:latest \
  /path/to/localcloud
```

### Run as standalone binary

```bash
bigtable-emulator-extended \
  -host 0.0.0.0 \
  -port 8087 \
  -database-driver postgres \
  -database-url 'postgres://localcloud@localhost/localcloud?sslmode=disable' \
  -strict-admin=true
```

```bash
export BIGTABLE_EMULATOR_HOST=localhost:8087
```

## Go Package (Embedded)

The `bttest` package can be imported directly — no Docker image or separate
process required. This is the recommended approach for Go projects that need
an embedded Bigtable emulator.

### 1. Add the dependency

Use a `replace` directive in your `go.mod` for the local module:

```bash
go get github.com/localcloud/bigtable-emulator-extended/bttest
```

```
replace github.com/localcloud/bigtable-emulator-extended => ../local_cloud_dependencies/bigtable-emulator-extended
```

Adjust the relative path to match your project layout.

### 2. Import CGO drivers

The emulator uses CGO-based SQLite. Your main package (or a blank-import file)
must import the SQL drivers:

```go
import (
    _ "github.com/mattn/go-sqlite3" // SQLite backend
    _ "github.com/lib/pq"           // PostgreSQL backend (optional)
)
```

### 3. Start the embedded emulator

```go
package yourpkg

import (
    "context"
    "database/sql"
    "log"

    "github.com/localcloud/bigtable-emulator-extended/bttest"
    _ "github.com/mattn/go-sqlite3"
    "google.golang.org/grpc"
)

func StartBigtableEmulator(ctx context.Context) (*bttest.Server, error) {
    // Configure backend: "sqlite3" or "postgres".
    // Second arg enables strict instance admin (require CreateInstance before table APIs).
    bttest.ConfigureStorage("sqlite3", true)

    db, err := sql.Open("sqlite3", "file:bigtable.db?cache=shared")
    if err != nil {
        return nil, err
    }
    db.SetMaxOpenConns(1) // required for SQLite

    if err := bttest.CreateTables(ctx, db); err != nil {
        return nil, err
    }

    srv, err := bttest.NewServer("localhost:0", db,
        grpc.MaxRecvMsgSize(256<<20),
        grpc.MaxSendMsgSize(256<<20),
    )
    if err != nil {
        return nil, err
    }

    log.Printf("Bigtable emulator listening on %s", srv.Addr)
    return srv, nil
}
```

### 4. Connect SDK clients

Using the environment variable (works with all SDKs):

```go
os.Setenv("BIGTABLE_EMULATOR_HOST", srv.Addr)
// All Bigtable clients auto-connect to the emulator.
```

Or explicit gRPC connection:

```go
import (
    "cloud.google.com/go/bigtable"
    "google.golang.org/api/option"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
)

conn, err := grpc.Dial(emulatorAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
client, err := bigtable.NewClient(ctx, "local-project", "local-instance",
    option.WithGRPCConn(conn),
)
```

### 5. Use in tests

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

    // Use standard Bigtable SDK — connects to emulator automatically.
    client, err := bigtable.NewClient(ctx, "proj", "inst")
    require.NoError(t, err)
    defer client.Close()

    // ... test with real Bigtable SDK operations
}
```

### 6. PostgreSQL backend

For shared environments or persistent data across restarts:

```go
bttest.ConfigureStorage("postgres", true)
db, err := sql.Open("postgres", "postgres://user@localhost/bigtable?sslmode=disable")
```

### API Summary

| Function | Purpose |
|----------|---------|
| `bttest.ConfigureStorage(driver, strictAdmin)` | Set SQL dialect and admin mode. Call before `NewServer`. |
| `bttest.CreateTables(ctx, db)` | Initialize schema. Safe to call on existing DB. |
| `bttest.NewServer(addr, db, ...grpc.ServerOption)` | Start gRPC server. Returns `*Server` with `.Addr` and `.Close()`. |

## Feature Coverage

See [BIGTABLE_COMPATIBILITY.md](BIGTABLE_COMPATIBILITY.md) for full feature
parity details.

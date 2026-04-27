# LocalCloud Bigtable Emulator

PostgreSQL-backed fork of [`bitly/little_bigtable`](https://github.com/bitly/little_bigtable), which itself is derived from Google's Bigtable `bttest` emulator.

This repo keeps LocalCloud-specific behavior isolated in `bttest/localcloud_*.go` where possible, so upstream `little_bigtable` syncs stay manageable.

## Build

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
make docker-build-offline                  # uses vendor if present, mod cache otherwise
```

#### Troubleshooting offline builds

| Error | Cause | Fix |
|-------|-------|-----|
| `missing go.sum entry for module` | `go.sum` incomplete | Run `go mod tidy` before staging deps |
| `GO_OFFLINE=true requires .docker/offline-go/vendor or .../mod` | No deps staged | Run Option A or B above |
| `cannot find module providing package ...` | Stale vendor/cache | Re-run `go mod vendor` or `rsync` |

LocalCloud's Dockerfile consumes that image as a named build stage:

```bash
docker build \
  --build-arg BIGTABLE_EMULATOR_IMAGE=bigtable-emulator-extended:latest \
  -t localcloud/localcloud:latest \
  /Users/jsenjaliya/src/my/localcloud
```

## Run

```bash
bigtable-emulator-extended \
  -host 0.0.0.0 \
  -port 8087 \
  -database-driver postgres \
  -database-url 'postgres://localcloud@localhost/localcloud?sslmode=disable' \
  -strict-admin=true
```

Then point SDKs at:

```bash
export BIGTABLE_EMULATOR_HOST=localhost:8087
```

## Scope

Supported:

- Bigtable table/data APIs inherited from `little_bigtable`
- PostgreSQL persistence for tables, rows, admin metadata, and changelog records
- Instance, cluster, and app profile metadata APIs for local validation
- Minimal change stream RPCs with one stable full-table partition

Explicitly unsupported:

- IAM enforcement
- CMEK
- backups/restore
- replication behavior
- autoscaling
- Data Boost
- distributed tablet split/merge behavior
- GoogleSQL for Bigtable

## Upstream Sync Policy

Keep upstream-derived files close to `little_bigtable`. New behavior should prefer:

- new `bttest/localcloud_*.go` files
- small delegation hooks in upstream files
- minimal SQL dialect helpers instead of broad rewrites

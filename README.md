# Little Bigtable

![CI Status](https://github.com/jhsenjaliya/little_bigtable/actions/workflows/test.yaml/badge.svg?branch=jay-bigtable-extended)

A local emulator for [Cloud Bigtable](https://cloud.google.com/bigtable) with persistence to a SQLite or PostgreSQL backend.

The Cloud SDK provided `cbtemulator` is in-memory and does not support persistence which limits it's applicability. This project is a fork of `cbtemulator` from [google-cloud-go/bigtable/bttest](https://github.com/googleapis/google-cloud-go/tree/c46c1c395b5f2fb89776a2d0e478e39a2d5572e4/bigtable/bttest)

For the audited feature and conformance contract, see
[`BIGTABLE_COMPATIBILITY.md`](BIGTABLE_COMPATIBILITY.md).

|             | [`cbtemulator`](https://cloud.google.com/bigtable/docs/emulator) | "little" Bigtable       | Bigtable                     |
| ----------- | ---------------------------------------------------------------- | ----------------------- | ---------------------------- |
| **Storage** | In-Memory                                                        | sqlite3 or postgres     | Distributed GFS              |
| **Type**    | Emulator                                                         | Emulator                | Managed Production Datastore |
| **Scaling** | Single process                                                   | Single process          | Scalable multi-node backend  |
| **GC**      | async GC                                                         | per-row GC at read time |                              |

## Features

### Data Plane (gRPC)

- **ReadRows** — table-targeted row keys/ranges, filters, reversed scans, and row limits; production chunking, statistics, and view targets remain partial
- **MutateRow / MutateRows** — table-targeted SetCell and delete success paths; rollback-safe mixed-failure behavior is not yet conformant
- **CheckAndMutateRow** — predicate-selected table mutations on success paths; failed-request rollback is not yet conformant
- **ReadModifyWriteRow** — append and increment success paths; failed-request rollback is not yet conformant
- **SampleRowKeys** — deterministic table sampling; range-restricted and view-target sampling are not yet conformant
- **PingAndWarm** — successful liveness no-op

### Admin (gRPC)

- **Instance CRUD** — Create, Get, List, Update, Delete instances
- **Table CRUD** — Create, List, Get, Delete tables
- **Column Families** — ModifyColumnFamilies (add/drop)
- **Row Ranges** — DropRowRange (by prefix or delete all)
- **Consistency** — GenerateConsistencyToken, CheckConsistency
- **Materialized Views (CMV)** — Create, Get, List, Update, Delete with write-time sync and delete propagation

### Filters

Supported row filters: Chain, Interleave, Condition, PassAll, BlockAll,
RowKeyRegex, RowSample, FamilyNameRegex, ColumnQualifierRegex, ColumnRange,
TimestampRange, ValueRange, ValueRegex, CellsPerColumnLimit, CellsPerRowLimit,
CellsPerRowOffset, StripValueTransformer, ApplyLabelTransformer.

Sink is recognized but currently behaves as a partial no-op rather than
production-equivalent sink suppression.

### Persistence

The following SQL-backed resource state is persisted (to SQLite or PostgreSQL,
per `-database-driver`) and survives emulator restarts:

- `instances_t` / `clusters_t` / `app_profiles_t` — instance, cluster, and app profile metadata
- `tables_t` — table definitions and column families
- `rows_t` — row keys and cell data
- `materialized_views_t` — CMV registrations
- `authorized_views_t` / `logical_views_t` / `backups_t` — authorized views, logical views, and backups
- `change_log_t` — change stream mutation log

IAM policies remain in memory only. Some resource update paths also have
documented persistence limits; see
[`BIGTABLE_COMPATIBILITY.md`](BIGTABLE_COMPATIBILITY.md) for exact contracts.

### Forward Compatibility

Most methods without local support, such as ExecuteQuery, OpenTable, and
snapshot RPCs, return `codes.Unimplemented`. Known false-success gaps in
data-bearing backup and change-stream handlers remain explicitly recorded in
`bttest.CompatibilityLedger()` until their owning conformance phases replace
that behavior. `PingAndWarm` is a deterministic local no-op.

## Usage

```
Usage of ./little_bigtable:
  -database-driver string
      database/sql driver name: postgres or sqlite3 (default "postgres")
  -database-url string
      database/sql connection string
  -db-file string
      legacy sqlite3 data file path (default "little_bigtable.db")
  -host string
      the address to bind to on the local machine (default "localhost")
  -port int
      the port number to bind to on the local machine (default 9000)
  -strict-admin
      require instances to exist before table/data APIs are used (default true)
  -version
      show version
```

With `-database-driver sqlite3`, `-db-file` is used to build the connection string automatically. With `-database-driver postgres` (the default), pass a connection string via `-database-url`.

In the environment for your application, set the `BIGTABLE_EMULATOR_HOST` environment variable to the host and port where `little_bigtable` is running. This environment variable is automatically detected by the Bigtable SDK or the `cbt` CLI. For example:

```bash
export BIGTABLE_EMULATOR_HOST="127.0.0.1:9000"
./run_my_app
```

### Running with Docker (Persistent Storage)

`little_bigtable` stores its SQL-backed resource metadata, tables, column
families, and row data in a single SQLite database file. In-memory state such as
IAM policies is not included. When running in a container, mount a volume for
the database path so persisted state survives container restarts, updates, and
recreations.

> **Note:** Always pass `-host 0.0.0.0` inside a container so the gRPC server binds to all interfaces rather than container loopback (`localhost`/`127.0.0.1`).

#### Using `docker run`

Mount a named volume to `/data` and set `-db-file` accordingly:

```bash
# Create a named volume
docker volume create little_bigtable_data

# Run the container with persistent storage
docker run -d \
  --name little_bigtable \
  -p 9000:9000 \
  -v little_bigtable_data:/data \
  <image-name> \
  -host 0.0.0.0 \
  -port 9000 \
  -database-driver sqlite3 \
  -db-file /data/little_bigtable.db
```

Or using a host directory bind mount:

```bash
mkdir -p ./data
docker run -d \
  --name little_bigtable \
  -p 9000:9000 \
  -v "$(pwd)/data:/data" \
  <image-name> \
  -host 0.0.0.0 \
  -port 9000 \
  -database-driver sqlite3 \
  -db-file /data/little_bigtable.db
```

#### Using `docker-compose.yml`

```yaml
services:
  little_bigtable:
    image: <image-name>
    container_name: little_bigtable
    ports:
      - "9000:9000"
    volumes:
      - little_bigtable_data:/data
    command:
      - "-host"
      - "0.0.0.0"
      - "-port"
      - "9000"
      - "-database-driver"
      - "sqlite3"
      - "-db-file"
      - "/data/little_bigtable.db"
    restart: unless-stopped

volumes:
  little_bigtable_data:
```

### Using with `cbt` CLI

```bash
export BIGTABLE_EMULATOR_HOST=localhost:9000
cbt -access-token emulator -project my-project createinstance my-instance "My Instance" my-cluster us-central1-b 1 SSD
cbt -access-token emulator -project my-project -instance my-instance createtable my-table
cbt -access-token emulator -project my-project -instance my-instance createfamily my-table cf1
cbt -access-token emulator -project my-project -instance my-instance set my-table row1 cf1:col1=value1
cbt -access-token emulator -project my-project -instance my-instance read my-table start=row1 end=row2
cbt -access-token emulator -project my-project -instance my-instance deletetable my-table
cbt -access-token emulator -project my-project deleteinstance my-instance
```

The standalone `cbt` binary currently initializes an authentication source
before its Bigtable client observes emulator mode. The non-secret placeholder
token above prevents a dependency on local gcloud credentials; transport still
uses only `BIGTABLE_EMULATOR_HOST`.

### REST API (localcloud console)

When running under localcloud, row-level operations are available via REST:

```bash
# Browse rows
curl http://localhost:8080/bigtable/admin/v2/projects/my-project/instances/my-instance/tables/my-table/rows?limit=50

# Write cells
curl -X POST http://localhost:8080/bigtable/admin/v2/projects/my-project/instances/my-instance/tables/my-table/rows \
  -H "Content-Type: application/json" \
  -d '{"rowKey": "row1", "cells": {"cf1:col1": "value1"}}'

# Delete a row
curl -X DELETE http://localhost:8080/bigtable/admin/v2/projects/my-project/instances/my-instance/tables/my-table/rows/row1
```

## Releasing

Official releases must come from `jay-bigtable-extended` and are created by the
**Release Go Module** GitHub Actions workflow.

1. Update the `version` constant in `little_bigtable.go` to the release version
   without the `v` prefix.
2. Commit and push that change to `jay-bigtable-extended`, then wait for the
   branch tests to pass.
3. In GitHub, open **Actions → Release Go Module → Run workflow**.
4. Select `jay-bigtable-extended` and enter a tag matching `vX.Y.Z`.

The workflow tests the exact branch tip, creates the annotated tag and GitHub
release, and uploads Linux and macOS binaries for AMD64 and ARM64. Do not create
releases from `master`.

For local packaging only, `./dist.sh` runs the complete test suite and writes
matching `.tar.gz` archives to `dist/`. It does not create tags or publish a
GitHub release.

## Syncing with Upstream

To fetch updates from the upstream repo (`bitly/little_bigtable`) and merge them into your branch:

```bash
# 1. Fetch all updates from upstream
git fetch upstream

# 2. Merge upstream/master into the current branch
git merge upstream/master --no-edit

# 3. Push the merge commit to origin
git push origin <your-branch>
```

## Limitations

- **Non-production features** (snapshots and GoogleSQL queries) return `codes.Unimplemented`.
- **GoogleSQL queries** (ExecuteQuery/PrepareQuery) are not supported.
- **Session protocol** (OpenTable/OpenAuthorizedView/OpenMaterializedView) is not implemented — not needed for correctness with standard SDK usage.
- Cluster resources support metadata CRUD, but real clustering, multi-node capacity, multi-region operation, and replication are not supported.
- Some GC rule types (Intersection) are not fully supported.
- CMV shadow tables do not auto-update when source table column families change after CMV creation.
- Some filters are not implemented or have partial support. See [cbtemulator docs](https://cloud.google.com/bigtable/docs/emulator#filters)

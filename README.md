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
| **GC**      | async GC                                                         | per-row GC at read and write time |                    |

## Features

Status follows the 2026-10-04 parity audit in
[`BIGTABLE_COMPATIBILITY.md`](BIGTABLE_COMPATIBILITY.md), which is the current
contract; it names the owning test for each capability.

### Data plane (gRPC)

- **MutateRow / MutateRows / CheckAndMutateRow / ReadModifyWriteRow** — single-row
  atomicity (all mutations validated before any is applied), per-entry
  `MutateRows` status codes, idempotency tokens, timestamp granularity, and
  aggregate (Sum/Min/Max Int64, HLL++) families.
- **ReadRows** — table, authorized-view and materialized-view targets,
  production-style chunking, `REQUEST_STATS_FULL` request stats.
- **SampleRowKeys** — honors `row_range`, includes initial split keys, samples
  every 512 KiB.
- **Filters** — every `RowFilter` variant, validated before reading, including
  `Interleave` (keeps duplicates), `Sink`, `Condition` and `ValueBitmask`.
- **App profiles** — resolved per request; routing rules enforced for
  transactional RPCs, change streams and Data Boost writes.
- **Change streams** — opt-in per table via `change_stream_config`, 1–7 day
  retention, grouped per-row records including GC and `DropRowRange`, one
  partition, heartbeats and continuation tokens.
- **Session protocol** — `GetClientConfiguration`, `OpenTable`,
  `OpenAuthorizedView`, `OpenMaterializedView`.
- **GoogleSQL** — `PrepareQuery` / `ExecuteQuery` over tables, logical views and
  materialized views (engine in `bttest/internal/gsql`).
- **PingAndWarm** — validates the instance and app profile.

### Admin (gRPC)

- **Tables** — validated create (initial splits, aggregate types, row key
  schema, change stream config), views, `UpdateTable` masks, soft delete and
  `UndeleteTable` (7 days), atomic `ModifyColumnFamilies`, `DropRowRange`,
  consistency tokens.
- **Authorized views** — CRUD with etags and deletion protection; enforced on
  reads and writes.
- **Backups** — data snapshots, copy, restore to a new table, expiry and quota
  rules, filtered listing.
- **Schema bundles** — CRUD with descriptor validation.
- **Logical views** and **continuous materialized views** — GoogleSQL
  definitions (see [`CMV_SUPPORT.md`](CMV_SUPPORT.md)).
- **Instances, clusters, app profiles** — validated metadata CRUD, pagination,
  guarded deletion.
- **Long-running operations** — durable Get/List/Wait/Delete/Cancel.
- **IAM policies** — persisted with etags; not enforced.

### Persistence

The following state is persisted to SQLite or PostgreSQL (per
`-database-driver`) and survives emulator restarts:

- `rows_t` — row data (tables, tombstoned tables, backup snapshots, materialized-view storage)
- `tables_t` — table metadata
- `instances_t` / `clusters_t` / `app_profiles_t` — instance, cluster and app-profile metadata
- `authorized_views_t` / `logical_views_t` / `materialized_views_t` / `schema_bundles_t` — view and schema-bundle definitions
- `backups_t` / `backup_manifests_t` — backups and their schema manifests
- `change_stream_t` — change-stream records
- `iam_policies_t` — IAM policies
- `operations_t` — long-running operations
- `idempotency_t` — mutation idempotency tokens

On first start, a database from an earlier release is migrated one way: table
metadata is rewritten in the new format, `change_log_t` is dropped, and legacy
materialized-view shadow tables are removed. Back up the database before
upgrading if you may need to downgrade.

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

With `-database-driver sqlite3`, `little_bigtable` stores all persisted state
(see [Persistence](#persistence)) in a single SQLite database file. When running
in a container, mount a volume for the database path so persisted state
survives container restarts, updates, and recreations.

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

See [`BIGTABLE_COMPATIBILITY.md`](BIGTABLE_COMPATIBILITY.md) for the full list.

- **Production-only behavior is not emulated:** replication, failover and
  multi-cluster consistency (consistency checks always succeed), autoscaling
  and node capacity, hot tablets (`ListHotTablets` returns `Unimplemented`),
  memory layers (`Unimplemented`), the Locations service (not registered),
  Data Boost compute, CMEK key custody, Cloud Monitoring and Key Visualizer,
  production latency, and Dataflow/BigQuery/Pub/Sub connectors.
- **IAM is not enforced.** The emulator is unauthenticated;
  `TestIamPermissions` grants every requested permission.
- Change streams use a single partition (no split/merge); the low watermark can
  precede an in-flight commit; `start_time` is not checked against the time the
  stream was enabled.
- An empty instance `display_name` is accepted (defaults to the instance ID);
  `DeleteCluster` is not blocked by automated-backup locations.
- `ReadRows` does not emit `last_scanned_row_key`.
- HLL++ sketch bytes are emulator-specific, not ZetaSketch/BigQuery compatible.
- Multi-column materialized-view row keys use an emulator-specific encoding.
- Parameterized logical views (`view_parameters`) are not supported.
- Deprecated table snapshot RPCs return `Unimplemented`; use backups.

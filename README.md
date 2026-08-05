# Little Bigtable

![CI Status](https://github.com/bitly/little_bigtable/actions/workflows/test.yaml/badge.svg?branch=master)

A local emulator for [Cloud Bigtable](https://cloud.google.com/bigtable) with persistence to a sqlite3 backend.

The Cloud SDK provided `cbtemulator` is in-memory and does not support persistence which limits it's applicability. This project is a fork of `cbtemulator` from [google-cloud-go/bigtable/bttest](https://github.com/googleapis/google-cloud-go/tree/c46c1c395b5f2fb89776a2d0e478e39a2d5572e4/bigtable/bttest)

| | [`cbtemulator`](https://cloud.google.com/bigtable/docs/emulator) | "little" Bigtable | Bigtable
| --- | ----- | ---- | ----
| **Storage** | In-Memory | sqlite3 | Distributed GFS
| **Type** | Emulator | Emulator | Managed Production Datastore
| **Scaling**| Single process | Single process | Scalable multi-node backend
| **GC** | async GC | per-row GC at read time |

## Features

### Data Plane (gRPC)

- **ReadRows** — full support with row keys, row ranges, reversed scans, row limits
- **MutateRow / MutateRows** — atomic row mutations (SetCell, DeleteFromColumn, DeleteFromFamily, DeleteFromRow)
- **CheckAndMutateRow** — conditional mutations with predicate filters
- **ReadModifyWriteRow** — atomic append and increment operations
- **SampleRowKeys** — row key sampling for map-reduce partitioning
- **PingAndWarm** — returns Unimplemented (safe, no crash)

### Admin (gRPC)

- **Instance CRUD** — Create, Get, List, Update, Delete instances
- **Table CRUD** — Create, List, Get, Delete tables
- **Column Families** — ModifyColumnFamilies (add/drop)
- **Row Ranges** — DropRowRange (by prefix or delete all)
- **Consistency** — GenerateConsistencyToken, CheckConsistency
- **Materialized Views (CMV)** — Create, Get, List, Update, Delete with write-time sync and delete propagation

### Filters

Supported row filters: Chain, Interleave, Condition, Sink, PassAll, BlockAll,
RowKeyRegex, RowSample, FamilyNameRegex, ColumnQualifierRegex, ColumnRange,
TimestampRange, ValueRange, ValueRegex, CellsPerColumnLimit, CellsPerRowLimit,
CellsPerRowOffset, StripValueTransformer, ApplyLabelTransformer.

### Persistence

All state is persisted to SQLite and survives emulator restarts:

- `instances_t` — instance metadata
- `tables_t` — table definitions and column families
- `rows_t` — row keys and cell data (gob-encoded)
- `materialized_views_t` — CMV registrations

### Forward Compatibility

New gRPC methods added by Google to the Bigtable proto (ExecuteQuery, OpenTable,
ReadChangeStream, etc.) safely return `codes.Unimplemented` without crashing.

## Usage

```
Usage of ./little_bigtable:
  -db-file string
      path to data file (default "little_bigtable.db")
  -host string
      the address to bind to on the local machine (default "localhost")
  -port int
      the port number to bind to on the local machine (default 9000)
  -version
      show version
```

In the environment for your application, set the `BIGTABLE_EMULATOR_HOST` environment variable to the host and port where `little_bigtable` is running. This environment variable is automatically detected by the Bigtable SDK or the `cbt` CLI. For example:

```bash
export BIGTABLE_EMULATOR_HOST="127.0.0.1:9000"
./run_my_app
```

### Using with `cbt` CLI

```bash
cbt -project my-project createinstance my-instance "My Instance" my-cluster
cbt -project my-project -instance my-instance createtable my-table families=cf1
cbt -project my-project -instance my-instance set my-table row1 cf1:col1=value1
cbt -project my-project -instance my-instance read my-table
```

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

- **Non-production features** (snapshots, backups, restores, change streams, GoogleSQL queries, clusters, IAM, app profiles, logical views, authorized views) return `codes.Unimplemented`.
- **GoogleSQL queries** (ExecuteQuery/PrepareQuery) are not supported.
- **Session protocol** (OpenTable/OpenAuthorizedView/OpenMaterializedView) is not implemented — not needed for correctness with standard SDK usage.
- Single-node emulator by design; clusters, multi-region, replication not supported.
- Some GC rule types (Intersection) are not fully supported.
- CMV shadow tables do not auto-update when source table column families change after CMV creation.

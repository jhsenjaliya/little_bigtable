# Bigtable Emulator Compatibility

This emulator should prioritize behavior that application developers need to
validate locally: schema setup, data writes, reads, filters, conditional writes,
atomic counters/appends, and SDK connectivity. Production operations whose value
is mainly managed-service control plane behavior can remain explicit gaps.

References:

- Bigtable product documentation: https://docs.cloud.google.com/bigtable/docs
- RPC Data API: https://docs.cloud.google.com/bigtable/docs/reference/data/rpc/google.bigtable.v2
- RPC Admin API: https://docs.cloud.google.com/bigtable/docs/reference/admin/rpc/google.bigtable.admin.v2
- Go hello world sample: https://docs.cloud.google.com/bigtable/docs/samples-go-hello

## Data API

| RPC | Status | Notes |
| --- | --- | --- |
| `ReadRows` | Supported | Streaming reads with full filter support. Chunking is simplified. |
| `MutateRow` | Supported | Set cell, delete from column/family/row/timestamp range, AddToCell, MergeToCell. |
| `MutateRows` | Supported | Streaming batch mutations with per-entry error reporting. |
| `CheckAndMutateRow` | Supported | Predicate-based conditional mutations. |
| `ReadModifyWriteRow` | Supported | Atomic increment and append operations. |
| `SampleRowKeys` | Supported | Approximate; adequate for local partitioning tests, not production-equivalent split behavior. |
| `PingAndWarm` | Supported | No-op, always succeeds. |
| `PrepareQuery` | Not implemented | GoogleSQL for Bigtable. Returns `Unimplemented`. |
| `ExecuteQuery` | Not implemented | GoogleSQL for Bigtable. Returns `Unimplemented`. |
| Aggregate mutations (`AddToCell`, `MergeToCell`) | Supported | Int64 sum aggregation. HyperLogLog and other aggregate types return raw input value. |

## Read Filters

| Filter | Status |
| --- | --- |
| `Chain` | Supported |
| `Interleave` | Supported |
| `Condition` (if/then/else) | Supported |
| `BlockAllFilter` | Supported |
| `PassAllFilter` | Supported |
| `RowKeyRegexFilter` | Supported |
| `FamilyNameRegexFilter` | Supported |
| `ColumnQualifierRegexFilter` | Supported |
| `ValueRegexFilter` | Supported |
| `ColumnRangeFilter` | Supported |
| `TimestampRangeFilter` | Supported |
| `ValueRangeFilter` | Supported |
| `CellsPerColumnLimitFilter` | Supported |
| `CellsPerRowLimitFilter` | Supported |
| `CellsPerRowOffsetFilter` | Supported |
| `RowSampleFilter` | Supported |
| `StripValueTransformer` | Supported |
| `ApplyLabelTransformer` | Supported |

Unsupported filter types return `Unimplemented` error rather than silently
passing. Interleave filter correctly deduplicates cells appearing in multiple
sub-filter results.

## Table Admin API

| RPC | Status | Notes |
| --- | --- | --- |
| `CreateTable` | Supported | With column families and initial splits. |
| `ListTables` | Supported | |
| `GetTable` | Supported | Returns deletion protection status. |
| `UpdateTable` | Supported | Fields: `automated_backup_policy`, `deletion_protection`. |
| `DeleteTable` | Supported | Respects deletion protection. |
| `ModifyColumnFamilies` | Supported | Create/update/delete column families and GC rules. |
| `DropRowRange` | Supported | |
| `GenerateConsistencyToken` | Supported | |
| `CheckConsistency` | Supported | |
| `CreateAuthorizedView` | Supported | Metadata-only; no access enforcement. |
| `GetAuthorizedView` | Supported | |
| `ListAuthorizedViews` | Supported | |
| `UpdateAuthorizedView` | Supported | Supports `deletion_protection` and `authorized_view` fields. |
| `DeleteAuthorizedView` | Supported | Respects deletion protection. |
| `CreateBackup` | Supported | Metadata-only; no actual data snapshot. |
| `GetBackup` | Supported | |
| `UpdateBackup` | Supported | Field: `expire_time`. |
| `DeleteBackup` | Supported | |
| `ListBackups` | Supported | |
| `CopyBackup` | Supported | Clones backup metadata with source_backup reference. |
| `RestoreTable` | Supported | Creates table with source schema; no data restore. |
| `UndeleteTable` | Not implemented | Returns `Unimplemented`. |
| `CreateTableFromSnapshot` | Not implemented | Returns `Unimplemented`. Deprecated API. |
| `SnapshotTable` | Not implemented | Returns `Unimplemented`. Deprecated API. |
| `GetSnapshot` / `ListSnapshots` / `DeleteSnapshot` | Not implemented | Returns `Unimplemented`. Deprecated API. |

## Instance Admin API

| RPC | Status | Notes |
| --- | --- | --- |
| `CreateInstance` | Supported | Sets type, display name, labels, auto-creates clusters. |
| `GetInstance` | Supported | |
| `ListInstances` | Supported | |
| `UpdateInstance` | Supported | |
| `PartialUpdateInstance` | Supported | Fields: `display_name`, `type`, `labels`. |
| `DeleteInstance` | Supported | Cascades to tables, clusters, and app profiles. |
| `CreateCluster` | Supported | |
| `GetCluster` | Supported | |
| `ListClusters` | Supported | |
| `UpdateCluster` | Supported | |
| `PartialUpdateCluster` | Supported | Field: `serve_nodes`. |
| `DeleteCluster` | Supported | |
| `CreateAppProfile` | Supported | Auto-sets `SingleClusterRouting` and `StandardIsolation` defaults. |
| `GetAppProfile` | Supported | |
| `ListAppProfiles` | Supported | |
| `UpdateAppProfile` | Supported | |
| `DeleteAppProfile` | Supported | |
| `GetIamPolicy` | Supported | Returns stored policy or empty default. Permissive stub. |
| `SetIamPolicy` | Supported | Stores policy in memory. Permissive stub. |
| `TestIamPermissions` | Supported | Returns all requested permissions. Permissive stub. |
| `ListHotTablets` | Not implemented | Returns `Unimplemented`. |

## Change Streams

| RPC | Status | Notes |
| --- | --- | --- |
| `GenerateInitialChangeStreamPartitions` | Supported | Returns one stable full-table partition. |
| `ReadChangeStream` | Supported | Continuation tokens, start-time filtering, heartbeat duration. |

SQL-backed change log (`change_log_t`) records per-mutation type tracking
(set_cell, delete_from_column, delete_from_family, delete_from_row) with
microsecond-precision commit timestamps. Changes are appended automatically on
`MutateRow`, `MutateRows`, and `ReadModifyWriteRow`.

Limitations: single partition only, no partition-level filtering.

## Continuous Materialized Views

LocalCloud-specific partial implementation.

| RPC | Status | Notes |
| --- | --- | --- |
| `CreateMaterializedView` | Supported | SQL query parsing, shadow table registration. |
| `GetMaterializedView` | Supported | |
| `ListMaterializedViews` | Supported | |
| `UpdateMaterializedView` | Supported | Toggle deletion protection (queries are immutable). |
| `DeleteMaterializedView` | Supported | Respects deletion protection. |

Features: composite key mapping with reordering, configurable key separator,
optional source key appending, selective column family inclusion. Shadow tables
are created automatically and synchronized on source table writes/deletes.

## Authorized Views

| RPC | Status | Notes |
| --- | --- | --- |
| `CreateAuthorizedView` | Supported | Metadata CRUD only; no access enforcement on reads/writes. |
| `GetAuthorizedView` | Supported | |
| `ListAuthorizedViews` | Supported | By parent table. |
| `UpdateAuthorizedView` | Supported | Fields: `deletion_protection`, `authorized_view`. |
| `DeleteAuthorizedView` | Supported | Respects deletion protection. |

Authorized views are stored in SQL (`authorized_views_t`). Setup code and
Terraform providers can create/manage views without hitting `Unimplemented`.
Access filtering is not enforced — all data reads return full table data
regardless of view restrictions.

## Logical Views

| RPC | Status | Notes |
| --- | --- | --- |
| `CreateLogicalView` | Supported | Metadata CRUD only; no query execution. |
| `GetLogicalView` | Supported | |
| `ListLogicalViews` | Supported | |
| `UpdateLogicalView` | Supported | Fields: `query`, `deletion_protection`. |
| `DeleteLogicalView` | Supported | Respects deletion protection. |

Logical views are stored in SQL (`logical_views_t`). Query strings are stored
but not executed.

## Backups

| RPC | Status | Notes |
| --- | --- | --- |
| `CreateBackup` | Supported | Metadata-only; records source table, state, timestamps. No data snapshot. |
| `GetBackup` | Supported | |
| `UpdateBackup` | Supported | Field: `expire_time`. |
| `DeleteBackup` | Supported | |
| `ListBackups` | Supported | By parent cluster. |
| `CopyBackup` | Supported | Clones metadata with `source_backup` reference. |
| `RestoreTable` | Supported | Creates new table with source table schema (column families). No data restore. |

Backups are stored in SQL (`backups_t`). State is always `READY` immediately.
Useful for validating backup scheduling code, Terraform, and admin scripts
without requiring actual data snapshots.

## Deletion Protection

| Scope | Status |
| --- | --- |
| Tables | Supported — `UpdateTable` with `deletion_protection` field. Enforced on `DeleteTable`. |
| Materialized views | Supported — stored flag, enforced on delete. |
| Authorized views | Supported — stored flag, enforced on delete. |
| Logical views | Supported — stored flag, enforced on delete. |
| Instances | Not supported — Instance proto does not expose field in current SDK version. |

## IAM

| RPC | Status | Notes |
| --- | --- | --- |
| `GetIamPolicy` | Supported | Returns stored policy or empty `{version: 1}` default. |
| `SetIamPolicy` | Supported | Stores policy in memory keyed by resource name. |
| `TestIamPermissions` | Supported | Always returns all requested permissions (permissive). |

IAM stubs are permissive — no actual access enforcement. Designed so Terraform
providers, admin scripts, and SDK setup code run without `Unimplemented` errors.

## Persistence

| Backend | Status |
| --- | --- |
| SQLite3 | Supported (default). BLOB binary, AUTOINCREMENT IDs. |
| PostgreSQL | Supported. BYTEA binary, BIGSERIAL IDs. |

Persisted entities: rows, tables, instances, clusters, app profiles,
materialized views, change log records, authorized views, backups, logical
views. GC is lazy (applied on reads/writes per column family rules).

## SDK Connectivity

Supported through unauthenticated gRPC and `BIGTABLE_EMULATOR_HOST`. Covered by
`TestGoogleDocsHelloWorldGoClientWorkflow`.

## Production-Only / Explicitly Deferred

| Area | Rationale |
| --- | --- |
| CMEK / encryption | Managed-service infrastructure behavior. |
| Snapshots (deprecated) | Deprecated by Google in favor of backups. Dead API. |
| Replication, failover, multi-cluster consistency | Distributed production behavior; emulator is single-process. |
| Autoscaling, node counts, hot tablets, Key Visualizer | Capacity and observability tied to production infrastructure. |
| Data Boost | Serverless production read compute. |
| GoogleSQL for Bigtable | Large query engine surface. |
| Schema bundles | Very new API, minimal SDK adoption. |
| Aggregate cell types beyond int64 sum | HyperLogLog, min/max. AddToCell/MergeToCell handle int64 sum; other aggregate types store raw input. |
| HBase, Beam, Spark, Flink, Kafka connector parity | External integration stacks. |

## Compatibility Bar

A feature should be considered emulator-required when a developer needs it to
run application code locally without branching away from production Bigtable SDK
usage. The preferred acceptance test is a Google-documented client example or a
minimal variant of it running unchanged except for local emulator setup.

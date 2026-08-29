# Bigtable Emulator Compatibility Audit

**Audit date:** 2026-08-29
**Baseline:** current Google Cloud Bigtable product documentation and v2 Data/Admin RPC references
**Repository scope:** all checked-in Go handlers, SQL persistence code, executable wiring, behavioral tests, and existing product/compatibility documentation

## Method and status vocabulary

This audit combines source inspection with emulator-only runtime validation. No
non-emulator database or Google Cloud resource was contacted.

- **Supported** — the useful local observable contract is implemented and no material mismatch was found in the audited path.
- **Partial** — a useful subset exists, but documented Bigtable behavior is missing or incompatible.
- **Unsupported** — the RPC is explicitly `Unimplemented`, inherited from an unimplemented server, absent, or materially nonfunctional.
- **Not applicable** — managed-service behavior that a single-process local emulator should normally expose only as an explicit boundary.
- **TB (test-backed)** — a focused behavioral test exists and passed in the uncached `bttest` package run.
- **RV (runtime-verified)** — a one-off, uncommitted overlay probe reproduced the stated behavior.
- **SI (source-inferred)** — based on handler/control-flow inspection without a focused behavioral test.
- **EG (explicit gap)** — an explicit `Unimplemented` path or no corresponding registered handler.

Validation:

- `./test.sh` passed under Go 1.27.0, including package tests, race tests,
  `go vet`, and the formatting check.
- One-off overlay probes reproduced the failed-mutation rollback leak, `Interleave`
  deduplication, GC-intersection error, and loss of table policy fields on
  serialization. The probes and their temporary files were removed.
- The checked-in `go.mod`, CI workflows, release workflows, and Docker builder
  now target Go 1.27.0. `go mod verify` passes without an external module file.
- `bttest/compatibility_test.go` verifies that the executable capability ledger
  has unique entries for all 86 RPCs registered by `NewServer`; field-level
  declarations exhaustively cover eight pinned high-risk messages, and every
  `test_verified` declaration names an existing behavioral test.
- `TestStorageConformance` passes the same table/family/row restart contract on
  SQLite and PostgreSQL 17. `TestCBTClientConformance` passes instance, table,
  family, write, bounded-read, table-delete, and instance-delete commands with
  `cloud.google.com/go/cbt` pinned at
  `v0.0.0-20260810145131-fe593de7bc1a`.

## Executive parity matrix

| Feature family | Status | What works locally | Material boundary | Repository evidence |
| --- | --- | --- | --- | --- |
| Data-plane reads and writes | **Partial** | Legacy table-targeted reads, row sets/ranges, successful set/delete/batch/conditional/read-modify-write flows | View targets, routing and read stats are ignored; response streaming is simplified; failed multi-mutation requests are not rollback-safe | TB: `bttest/inmem.go` (`ReadRows`, `MutateRow`, `MutateRows`, `CheckAndMutateRow`, `ReadModifyWriteRow`); `bttest/google_docs_features_test.go` (`TestGoogleDocs_WritingData`, `TestGoogleDocs_ReadingData`, `TestGoogleDocs_MutationsAndDeletions`, `TestGoogleDocs_ConditionalMutations`, `TestGoogleDocs_ReadModifyWrite`) |
| Filters and garbage collection | **Partial** | Most legacy filters and basic max-age/max-version GC paths | `Interleave`, `Sink`, `ValueBitmask`, and GC intersection semantics do not match the documented contracts | TB/SI: `bttest/inmem.go` (`filterRow`, `includeCell`, `modifyCell`, `applyGC`); `bttest/inmem_test.go` (`TestFilters`, `TestFilterRow*`); `bttest/localcloud_new_features_test.go` (`TestInterleaveDedup`) |
| Table administration | **Partial** | Basic table/column-family CRUD, row-range deletion, selected table metadata | Initial splits, aggregate family types, current update fields and schema bundles are missing; table protection/backup policy are not durable; consistency is a stub; undelete is unsupported | TB/SI: `bttest/inmem.go` (`CreateTable` through `CheckConsistency`, `newTable`, `columnFamily`); `bttest/sql_tables.go`; `bttest/inmem_test.go` (`TestCreateTable*`, `TestUpdateTable`, `TestModifyColumnFamilies`, `TestDropRowRange`) |
| Instance administration/control plane | **Partial** for metadata; **Not applicable** for managed semantics | Persistent instance, cluster and app-profile metadata CRUD | No capacity, autoscaling, storage, replication, routing, location, memory-layer, or real LRO behavior | TB/SI: `bttest/localcloud_instance_admin.go`; `bttest/instance_server.go`; `bttest/instance_server_test.go`; `bttest/inmem_test.go: TestInstancePersistence` |
| GoogleSQL/query and Data Boost | **Unsupported** / **Not applicable** | None | `PrepareQuery` and `ExecuteQuery` return `Unimplemented`; no SQL engine, typed query results, or Data Boost compute | EG: `bttest/localcloud_change_stream.go: PrepareQuery`, `ExecuteQuery` |
| Change streams | **Partial** | Source-inferred single SQL log, one full-table partition, continuation IDs and heartbeats | No enablement/retention, partition lifecycle, GC or `DropRowRange` records, atomic record grouping, or routing enforcement; no focused test | SI: `bttest/localcloud_change_stream.go`; mutation hooks in `bttest/inmem.go` |
| Authorized, logical and materialized views | **Partial** | Admin metadata CRUD; a custom synchronous shadow-table subset for materialized views | Authorized-view access is not enforced; logical views are not executable; CMV SQL/consistency/read targeting differs materially from Bigtable | TB/SI: `bttest/localcloud_authorized_views.go`, `localcloud_logical_views.go`, `instance_server.go`, `cmv.go`, `sql_parse.go`; corresponding tests in `localcloud_new_features_test.go`, `cmv_test.go`, `sql_parse_test.go` |
| Backups, copy and restore | **Partial** metadata compatibility | Backup metadata lifecycle and immediate done operations | No schema/data snapshot; copy copies metadata; restore consults the live source table and cannot recover deleted data/schema | TB/SI: `bttest/localcloud_backups.go`; `bttest/localcloud_new_features_test.go` (`TestBackups_CRUD`, `TestCopyBackup`, `TestRestoreTable`) |
| IAM, authentication and encryption | **Partial** stubs / **Not applicable** managed security | In-memory policy round-trip compatibility | All requested permissions are granted; no authorization enforcement or default secure endpoint; no CMEK behavior | TB/SI: `bttest/instance_server.go` (`GetIamPolicy`, `SetIamPolicy`, `TestIamPermissions`); `TestIAMStubs_Permissive`; `bttest/inmem.go: NewServer`; `little_bigtable.go: main` |
| Replication and routing | **Unsupported** behavior / **Partial** metadata | Cluster/app-profile resources can be stored | Data handlers ignore app profiles; there is no replication, failover, consistency model, row affinity, or Data Boost routing | SI: `bttest/localcloud_instance_admin.go`; data handlers in `bttest/inmem.go`; fixed token handlers `GenerateConsistencyToken`, `CheckConsistency` |
| Observability | **Unsupported** / **Not applicable** | Ordinary process logging | No request stats, Cloud Monitoring metrics, tracing, Key Visualizer, or hot-tablet implementation | SI/EG: `bttest/inmem.go: ReadRows`; `bttest/instance_server.go: ListHotTablets`; `little_bigtable.go` disables gRPC tracing |
| Import/export and connectors | **Not applicable** as managed integrations | Generic API writes might be usable by compatible tools | No Dataflow/BigQuery/Pub/Sub integration or import/export implementation is in this repository | EG: repository inventory; only generic Data/Admin gRPC services are registered in `bttest/inmem.go: NewServer` |
| CLI/client compatibility | **Partial** | Production Go client and a pinned `cbt` core workflow are test-backed over insecure gRPC; `cbt` exercises automatic `BIGTABLE_EMULATOR_HOST` discovery | Other languages, HBase/Beam/Spark/connectors, advanced `cbt` commands, gcloud emulator lifecycle, and newer internal session RPCs remain uncovered or unsupported | TB/SI: `bttest/google_docs_hello_test.go: TestGoogleDocsHelloWorldGoClientWorkflow`; `bttest/client_conformance_test.go: TestCBTClientConformance`; `little_bigtable.go`; embedded unimplemented services in `bttest/inmem.go: server` |

The official API and product targets for these families are the [Data RPC
reference][data-rpc], [Admin RPC reference][admin-rpc], [official emulator
contract][emulator], and [tables/views comparison][tables-views].

## 1. Data plane

The public v2 data service defines `ReadRows`, `SampleRowKeys`, `MutateRow`,
`MutateRows`, `CheckAndMutateRow`, `ReadModifyWriteRow`, `PingAndWarm`, SQL, and
change-stream RPCs. It promises row-level atomicity and specific streaming and
target-selection semantics ([Data API][data-rpc], [detailed messages and
methods][data-detail]).

| Capability | Status | Repository evidence and exact caveat |
| --- | --- | --- |
| `ReadRows` on a table | **Partial** | TB: `bttest/inmem.go: ReadRows`, `streamRow`; `bttest/inmem_test.go: TestReadRows`, `TestReadRowsOrder`, `TestReadRowsReversed`; `TestGoogleDocs_ReadingData`. Row keys/ranges, deduped sorted rows, reverse scans and row limits are covered. SI: the handler selects only `req.TableName`; it ignores `authorized_view_name`, `materialized_view_name`, `app_profile_id`, and `request_stats_view`. It emits one response containing the whole row rather than production chunk/resume/progress semantics described by [ReadRows][data-detail]. |
| `MutateRow` set/delete operations | **Partial** | TB: `bttest/inmem.go: MutateRow`, `applyMutations`; `TestGoogleDocs_WritingData`, `TestGoogleDocs_MutationsAndDeletions`. RV/SI: mutations are applied directly to the stored row before all later mutations validate, so an error can leave earlier mutations committed, violating the documented atomic-row contract in the [Data API][data-detail]. Authorized-view and app-profile targets are not honored. |
| `MutateRows` batch | **Partial** | TB: `bttest/inmem.go: MutateRows`; `TestGoogleDocs_WritingData/BulkMutateRows`; empty-batch errors in `TestMutateRowsEmptyMutationErrors`. RV/SI: each entry uses the same non-rollback-safe `applyMutations`; failures are remapped to `Internal`, and one aggregate response is sent. Production requires each entry to be atomic, although the whole batch is not ([Data API][data-detail]). |
| `CheckAndMutateRow` | **Partial** | TB: `bttest/inmem.go: CheckAndMutateRow`; `TestCheckAndMutateRowWithoutPredicate`, `TestCheckAndMutateRowWithPredicate`, `TestGoogleDocs_ConditionalMutations`. Predicate selection works on success paths; the selected mutation list inherits the failed-request rollback defect and ignores authorized-view/routing targets ([Data API][data-detail]). |
| `ReadModifyWriteRow` increment/append | **Partial** | TB: `bttest/inmem.go: ReadModifyWriteRow`; `TestServer_ReadModifyWriteRow`, `TestGoogleDocs_ReadModifyWrite`. SI: rules update the stored row as they are processed, so a later invalid rule can leave earlier rules applied instead of preserving RPC atomicity ([Data API][data-detail]). |
| `AddToCell` / `MergeToCell` narrow simulation | **Partial** | TB: `bttest/inmem.go: applyMutations`, `extractInputValue`; `TestAddToCell_Int64Sum`, `TestMergeToCell_Int64Sum`, and edge tests. `columnFamily` stores only name/order/GC rule, so the required aggregate `value_type` and encoding are neither persisted nor enforced. Both mutations perform same-timestamp big-endian `int64` addition in any family; unsupported or malformed `Value` kinds silently become zero. This is not general aggregate-cell support. Bigtable defines typed sum, min, max and HyperLogLog++ aggregate families and input/state encodings ([Admin `ColumnFamily`/`Type.Aggregate`][admin-detail], [v2 mutations][data-detail]). |
| `SampleRowKeys` | **Partial** | TB: `bttest/inmem.go: SampleRowKeys`; `TestSampleRowKeys`. Samples are random and offsets are local value-size estimates. SI: `row_range` is ignored even though the API requires samples to be restricted to it ([Data API][data-detail]). |
| `PingAndWarm` | **Supported** as a local no-op (SI) | `bttest/localcloud_change_stream.go: PingAndWarm` returns success. There is no distributed metadata to warm locally; no focused test exists. |
| Cross-row transactions | **Not applicable** | Bigtable itself supports transactions only within a single row ([tables and views][tables-views]). The emulator exposes no extra cross-row transaction claim. |

### Filters

Bigtable documents limiting, modifying and composing filters; the official
Google emulator supports all filters except `Sink` ([filters][filters], [official
emulator][emulator]).

| Filter group | Status | Repository evidence and mismatch |
| --- | --- | --- |
| Chain, condition, pass/block all, row-key/family/qualifier/value regex, column/timestamp/value range, row sample, cells-per-column/row limits, row offset, strip value, apply label | **Supported** for audited success paths | TB: `bttest/inmem.go: filterRow`, `filterCells`, `includeCell`, `modifyCell`; `bttest/inmem_test.go: TestFilters`, `TestFilterRow*`, `TestReadRowsWithlabelTransformer`; `bttest/google_docs_features_test.go: TestGoogleDocs_Filters`. Unknown oneofs return `Unimplemented` (`TestUnknownFilter_ReturnsError`). |
| `Interleave` | **Unsupported semantic parity** | TB/RV: `filterRow` deliberately deduplicates identical cells and `TestInterleaveDedup` asserts one copy. Bigtable explicitly allows duplicate copies from matching branches and counts them in limit/offset filters ([filters, Interleave][filters]). The existing test therefore protects incompatible behavior. |
| `Sink` | **Unsupported semantic parity** | SI: `filterRow` accepts `Sink` but returns a no-op result; it does not copy cells directly to final output past parent filters. That is not the documented `Sink` behavior ([filters, Sink][filters]). The official emulator explicitly rejects `Sink` ([emulator][emulator]); this emulator should at least fail explicitly rather than silently misbehave. |
| `ValueBitmask` | **Unsupported** | EG: `includeCell` falls through to `Unimplemented`; the current v2 `RowFilter` includes the bitmask filter ([Data type reference][data-detail]). The official emulator’s current contract says every filter except `Sink` is supported ([emulator][emulator]). |

### Garbage collection

Basic max-age, max-version and union paths are **partial** and lazily applied on
reads/writes (`bttest/inmem.go: row.gc`, `applyGC`; TB:
`TestGoogleDocs_GarbageCollection`). Production GC is asynchronous and can take
days, which is appropriately not reproduced locally ([garbage
collection][garbage-collection]). However, `applyGC` implements intersection by
intersecting the cells *kept* by every child rule. This deletes a cell when any
child deletes it. Bigtable intersection deletes only when **all** criteria match;
union deletes when **any** criterion matches ([garbage collection][garbage-collection]).
Thus intersection/nested policies are **unsupported semantic parity (RV)**.

### Performance realism and scan safety

The emulator accepts unbounded `ReadRows` requests but does not model tablets,
capacity, network cost, or managed-service latency. Do not use local timings to
validate production performance. In production, prefer exact row keys or
explicitly bounded row ranges; prefix scans and especially unbounded full-table
scans can be expensive. If a second latency-sensitive access pattern cannot be
served by the primary row-key design, use an appropriately keyed continuous
materialized view; reserve Data Boost for large analytical or batch reads
([tables and views][tables-views], [Data Boost][data-boost]).

## 2. Table administration

The current Table Admin surface includes table, column-family, authorized-view,
backup, IAM, consistency, undelete, and schema-bundle operations
([Admin API][admin-rpc]).

| Capability | Status | Repository evidence and exact caveat |
| --- | --- | --- |
| Create/get/list/delete table | **Partial** | TB: `bttest/inmem.go: CreateTable`, `GetTable`, `ListTables`, `DeleteTable`; `TestCreateTableResponse`, `TestCreateTableWithFamily`, `TestGoogleDocs_TableManagement`. SI: `newTable` ignores `initial_splits`; `columnFamily` discards `value_type`; only selected `Table` fields are retained/returned. The current resource also includes change-stream config, aggregate family type, row-key schema, tiered storage and stats ([Admin messages][admin-detail]). |
| Update table | **Partial and non-durable** | TB/RV/SI: `bttest/inmem.go: UpdateTable`; `TestUpdateTable`, `TestTableDeletionProtection_BlocksDelete`. Only `automated_backup_policy*` and `deletion_protection` are accepted. Current Bigtable also permits `change_stream_config*` and `row_key_schema` update masks ([UpdateTableRequest][admin-detail]). `UpdateTable` does not call `tableBackend.Save`, and `bttest/sql_tables.go: table.Bytes/Scan` serializes only column families, so backup policy and deletion protection disappear on restart. Automated backup has no scheduler or snapshot. |
| Column-family CRUD and GC rules | **Partial** | TB: `ModifyColumnFamilies`, `TestModifyColumnFamilies`, `TestGoogleDocs_TableManagement`. Create/update/drop and basic rules exist, but aggregate `value_type` is lost and never enforced; intersection/nested GC is incompatible as described above. Bigtable defines immutable aggregate family types and column-family GC contracts ([Admin messages][admin-detail], [garbage collection][garbage-collection]). |
| `DropRowRange` | **Supported** for local data deletion | TB: `bttest/inmem.go: DropRowRange`; `TestDropRowRange`. Whole-table and prefix deletion work. Change-stream parity remains partial because these deletes are not logged ([change streams][change-streams]). |
| Replication consistency tokens | **Partial** stub | SI: `GenerateConsistencyToken` returns `"TokenFor-" + table`; `CheckConsistency` validates that string and always reports consistent. This is not replication catch-up checking promised by the [Admin API][admin-rpc]. |
| `UndeleteTable` | **Unsupported** | EG: `bttest/inmem.go: UndeleteTable` returns `Unimplemented`; it is a current Table Admin operation ([Admin API][admin-rpc]). |
| Schema-bundle CRUD | **Unsupported** | EG: no overrides exist; `server` inherits `UnimplementedBigtableTableAdminServer`. The current API exposes create/get/list/update/delete schema bundles ([Admin API][admin-rpc]). |
| Legacy snapshot RPCs | **Not applicable** | EG: snapshot methods explicitly return `Unimplemented`. They remain in the pinned proto as deprecated/private-alpha legacy paths and are absent from the current public Admin index; backups are the remediation target. |

Long-running operations are also only superficially compatible. Admin handlers
return immediately completed operations (`bttest/localcloud_instance_admin.go:
doneOperation` and analogous handlers), but `bttest/operations.go` implements
only `GetOperation`, and no returned operation is inserted into its map. Lookup is
therefore effectively unusable; `ListOperations` and `WaitOperation` remain
inherited unimplemented even though the current Admin reference lists them
([Admin API][admin-rpc]). Status: **Partial (SI)**.

## 3. Instance administration and managed control plane

Google’s own local emulator deliberately has no instance or cluster Admin APIs
and accepts arbitrary project/instance names ([official emulator][emulator]).
This repository adds persistent metadata APIs for LocalCloud bootstrap flows;
that is useful, but it is not an emulation of the managed control plane.

| Capability | Status | Repository evidence and boundary |
| --- | --- | --- |
| Instance CRUD/partial update | **Partial** metadata | TB/SI: `bttest/localcloud_instance_admin.go: localCreateInstance` through `localDeleteInstance`; `TestDeleteInstance`, `TestInstancePersistence`, and the create path in `TestGoogleDocsHelloWorldGoClientWorkflow`. State is immediately `READY`; only display name/type/labels have meaningful updates. No project quota, edition, capacity or asynchronous lifecycle is modeled ([instances/clusters/nodes][instances]). |
| Cluster CRUD/update | **Partial** metadata | SI: `localCreateCluster` through `localDeleteCluster`; only `serve_nodes` is accepted by partial update. Location/state are synthesized. Autoscaling, node/compute-unit behavior, storage configuration and actual serving are not modeled ([instances/clusters/nodes][instances], [Admin API][admin-rpc]). |
| App-profile CRUD | **Partial** metadata | SI: `localCreateAppProfile` through `localDeleteAppProfile`, `setDefaultAppProfileFieldsLocked`. Defaults are synthesized and persisted, but Data RPCs ignore `app_profile_id`; routing/isolation/priority have no effect ([routing][routing]). |
| Hot tablets | **Unsupported** / managed behavior **Not applicable** | EG: `bttest/instance_server.go: ListHotTablets` returns `Unimplemented`; hot-tablet analysis is a managed observability/capacity feature ([Admin API][admin-rpc]). |
| Memory layer | **Unsupported** / managed behavior **Not applicable** | EG: the current Admin API adds `GetMemoryLayer` and `ListMemoryLayers`; the pinned `cloud.google.com/go/bigtable v1.51.0` interface and this server have no handlers ([Admin API][admin-rpc]; repository pin: `go.mod`). |
| Locations | **Unsupported** / managed behavior **Not applicable** | EG: no `google.cloud.location.Locations` service is registered in `NewServer`; the current Admin index exposes `ListLocations` ([Admin API][admin-rpc]). |

## 4. SQL, views, change streams, and backups

### GoogleSQL and Data Boost

`PrepareQuery` and `ExecuteQuery` explicitly return `Unimplemented`
(`bttest/localcloud_change_stream.go`). There is no parser/executor for Bigtable
GoogleSQL, parameter binding, typed result streaming, resume tokens, structured
row keys, or query stats. Status: **Unsupported (EG)** against [GoogleSQL for
Bigtable][sql] and the [Data API][data-rpc].

Data Boost is managed read-only compute selected through app profiles; it has
routing, freshness, edition and operation restrictions ([Data Boost][data-boost]).
No local compute analogue exists, and app profiles are not consulted by reads.
Status: **Not applicable** as infrastructure and **unsupported** if an application
expects Data Boost behavior.

### Views

Bigtable distinguishes SQL logical/parameterized views, eventually consistent
read-only continuous materialized views, and read/write authorized subsets
([tables and views][tables-views]).

| View family | Status | Repository evidence and mismatch |
| --- | --- | --- |
| Authorized views | **Partial** metadata only | TB: `bttest/localcloud_authorized_views.go`; `TestAuthorizedViews_CRUD`, duplicate/not-found tests. CRUD and deletion protection metadata work. SI: Data handlers never use `authorized_view_name` and do not enforce row-prefix/family/qualifier subsets or view IAM. Parent table/instance deletion also does not honor an authorized view’s protection. Bigtable authorized views control both reads and writes ([authorized views][authorized-views]). |
| Logical and parameterized views | **Partial** metadata; execution **Unsupported** | TB/SI: `bttest/localcloud_logical_views.go`; `TestLogicalViews_CRUD`, `TestLogicalViews_UpdateQuery`. Query text/deletion protection are stored, but SQL is not validated or executed, list ignores parent scoping, and parameterized views have no behavior. Bigtable logical views are SQL virtual tables queried through SQL ([tables and views][tables-views], [parameterized views][parameterized-views]). |
| Continuous materialized views | **Partial**, non-production custom subset | TB: `bttest/instance_server.go` materialized-view CRUD; `cmv.go`; `sql_parse.go`; `TestCMVWriteSync`, `TestCMVDeleteSync`, `TestCMVDropRowRange*`, `TestCreateMaterializedViewRPC`, `TestParseCMVConfigFromSQL`. The emulator synchronously maintains a shadow table for a narrow `SPLIT(_key)...ORDER BY` subset. SI: `GROUP BY` is rejected, `WHERE` is silently ignored, transformation/aggregation coverage is narrow, and standard `materialized_view_name` reads are not handled. Bigtable CMVs are managed, eventually consistent, read-only, support transformations/aggregations, and are read through SQL or the Data API ([continuous materialized views][cmv]). |

### Change streams

Status: **Partial (SI)**; no focused change-stream behavioral test was found.

Implemented in `bttest/localcloud_change_stream.go`:

- one stable full-keyspace partition;
- a persistent SQL mutation log;
- numeric continuation tokens, start time, end time, and heartbeats;
- mutation hooks for `MutateRow`, `MutateRows`, `CheckAndMutateRow`, and `ReadModifyWriteRow` in `bttest/inmem.go`.

Documented Bigtable change streams require explicit table enablement and 1–7 day
retention, partition split/merge handling, per-row/cluster ordering metadata,
resume behavior, and records for GC and `DropRowRange` changes
([change streams][change-streams]). This implementation logs regardless of table
configuration, retains indefinitely, ignores requested partition semantics,
emits each mutation as a separate change instead of preserving an atomic change
group, and omits GC and `DropRowRange` changes. It uses a fixed
`local-cluster` and does not enforce the required routing profile.

### Backups, copy, restore, and automated backups

Status: **Partial metadata compatibility**, not data protection.

TB/SI evidence: `bttest/localcloud_backups.go`; `TestBackups_CRUD`,
`TestCopyBackup`, `TestRestoreTable`, and failure-path tests in
`bttest/localcloud_new_features_test.go`.

- `CreateBackup` immediately stores metadata with `READY`, size zero, and no schema/data snapshot.
- `CopyBackup` clones metadata, not backup contents.
- `RestoreTable` copies schema from the **currently live** source table; after source deletion it creates an empty schema and can never restore source data.
- Standard/hot backup distinctions, retention enforcement, cancellation, incremental storage, cross-location/project copy semantics, and automated execution are absent.

Bigtable backups preserve table schema and data and support Standard/Hot,
on-demand/automated/copy, expiration, and restore-to-new-table workflows
([backups][backups]). The current handlers must not be treated as a backup or
disaster-recovery mechanism.

## 5. IAM, security, routing, and observability

### IAM and security

| Capability | Status | Evidence and boundary |
| --- | --- | --- |
| IAM policy RPC shape | **Partial** compatibility stub | TB: `GetIamPolicy`, `SetIamPolicy`, `TestIamPermissions` in `bttest/instance_server.go`; `TestIAMStubs_Permissive`. Policies are held only in the in-memory `server.iamPolicies`, are lost on restart, and `TestIamPermissions` returns every requested permission. There is no authorization on Data/Admin calls, unlike Bigtable IAM ([access control][iam]). |
| Authentication and transport security | **Not applicable** for a trusted local process; **unsupported** as a security boundary | SI: `NewServer` is documented unauthenticated; the shipped `little_bigtable.go` starts plain gRPC without TLS credentials. Google’s emulator likewise documents no secure connection ([official emulator][emulator]). |
| Encryption/CMEK | **Not applicable** managed infrastructure | No emulator-layer CMEK or key lifecycle exists. Bigtable CMEK is configured on managed resources and controls encryption at rest ([CMEK][cmek]). |

### Replication, consistency, and routing

Cluster and app-profile CRUD are metadata-compatible only. Data handlers look up
the table directly and ignore `app_profile_id`; cluster state, single/multi-cluster
routing, failover, row affinity, transaction restrictions and priorities never
affect a request. Consistency tokens always resolve true. Status:
**Unsupported functional parity (SI)** against Bigtable’s documented
single-cluster, multi-cluster, cluster-group and row-affinity behavior
([routing][routing]). The actual distributed behavior is **Not applicable** to a
single-process emulator; unsupported inputs should nevertheless be rejected or
clearly documented rather than accepted without effect.

### Observability

Status: **Unsupported** for protocol-level stats and **Not applicable** for
managed dashboards.

- `ReadRows` ignores `request_stats_view` and never populates request stats (`bttest/inmem.go: ReadRows`).
- `ListHotTablets` is explicitly unimplemented (`bttest/instance_server.go`).
- `little_bigtable.go` disables gRPC tracing and exposes no metrics exporter; ordinary logs are the only built-in signal.

For production hotspot or latency diagnosis, use **Key Visualizer first** because
it provides the most granular view of access patterns across row keys
([Key Visualizer][key-visualizer]). Follow it with the hot-tablets tool /
`ListHotTablets` and table statistics from `gcloud`, then use
`cbt read` with `include-stats=full` for per-read execution statistics
([Admin API][admin-rpc], [gcloud Bigtable][gcloud-bigtable], [`cbt`
reference][cbt]). None of those signals are emulated here. Cloud Monitoring
server metrics, client-side metrics, and console monitoring are also managed
facilities ([metrics][metrics], [client-side metrics][client-metrics],
[instance monitoring][monitoring]).

## 6. Import/export, CLI, and client compatibility

Official import/export options include BigQuery reverse ETL, Pub/Sub Bigtable
subscriptions (Preview), Dataflow templates, and `cbt` CSV import
([import/export][import-export]). These are companion-service/tool workflows,
not Data API RPCs. No connector/template implementation exists in this
repository: **Not applicable** as managed integrations. Their use against this
emulator is unverified and limited by the data/admin gaps above.

Client/CLI assessment:

- **Go client: Partial, TB.** `TestGoogleDocsHelloWorldGoClientWorkflow` uses the production Go client over an insecure explicit gRPC connection for instance creation, table/family setup, bulk write, filter/read, scan and deletion. `TestGoogleDocs_*` adds broad Go-client success-path coverage.
- **`BIGTABLE_EMULATOR_HOST`: Partial, TB.** `TestCBTClientConformance` configures the pinned CLI transport through this endpoint variable and documented project/instance flags. The standalone CLI currently requires an authentication source before its client observes emulator mode, so the hermetic test runs from an empty directory without inherited home/ADC/gcloud configuration and supplies a non-secret documented `-access-token` placeholder; it supplies no credentials file or endpoint override. The Go hello test still injects an explicit connection, so automatic Go-client discovery is not independently isolated. The environment-variable contract is the official client mechanism ([official emulator][emulator]).
- **Other official client languages/HBase: Unverified.** Google documents supported client libraries ([client libraries][libraries]), but this repository has no behavioral coverage beyond Go.
- **`cbt`: Partial, TB for the core smoke.** `TestCBTClientConformance` runs the pinned standalone CLI as a subprocess and verifies instance/table/family creation, three writes, a bounded read that excludes both neighboring rows, table deletion, and instance deletion. Advanced instance, view, routing, backup, filter, query, and schema commands inherit the gaps in this report.
- **gcloud emulator lifecycle: Unsupported.** This is a separate binary with custom flags, not the `gcloud beta emulators bigtable` component.
- **Newer client session protocol: Unsupported (SI).** The pinned v1.51 generated service includes internal `GetClientConfiguration`, `OpenTable`, `OpenAuthorizedView`, and `OpenMaterializedView` RPCs. `bttest/inmem.go: server` embeds `UnimplementedBigtableServer` and provides no overrides, so a client that opts into that protocol receives `Unimplemented`. The public Data API baseline remains [the documented RPC surface][data-rpc].

SQLite/PostgreSQL persistence is a repository-specific extension, not managed
Bigtable parity. Rows, column families, instance/cluster/app-profile metadata,
view/backup metadata, and the change log are SQL-backed (`bttest/sql_schema.go`,
`sql_rows.go`, `sql_tables.go`, `sql_admin_metadata.go`, and feature-specific SQL
stores). Important exceptions are table automated-backup policy and deletion
protection—`table.Bytes/Scan` serializes only families—and IAM policies, which are
explicitly in-memory. Persistence must not be confused with replication,
durability SLA, backup, or transactional parity. `TestStorageConformance` is the
checked-in backend-neutral contract for table, family, and row persistence across
an emulator restart; CI runs it in separate SQLite and PostgreSQL jobs.

## Appendix A — RPC disposition

This appendix enumerates every method in the current public Data/Admin RPC
indexes, plus the pinned generated service's internal and legacy methods.
The appendix statuses summarize current observed semantics and therefore retain
the audit's **Partial** label where needed. The executable ledger separately
records the intended local disposition plus observed verification as
`test_verified`, `known_nonconformant`, or `declared_unverified`; only the first
state means that disposition is achieved. This keeps Phase 0 honest about
false-success gaps that later phases own without prematurely changing handlers.
`bttest/compatibility.go` declares the four services currently registered by
`NewServer`; `TestCompatibilityLedgerCoversRegisteredRPCs` fails if its RPC
identifiers drift from the runtime surface, and the tracked semantic-field test
fails when a pinned high-risk request gains an undeclared field.

### Data API

| RPC or service | Disposition |
| --- | --- |
| `ReadRows` | **Partial** |
| `SampleRowKeys` | **Partial** |
| `MutateRow` | **Partial** |
| `MutateRows` | **Partial** |
| `CheckAndMutateRow` | **Partial** |
| `ReadModifyWriteRow` | **Partial** |
| `PingAndWarm` | **Supported** as a local no-op |
| `GenerateInitialChangeStreamPartitions`, `ReadChangeStream` | **Partial** |
| `PrepareQuery`, `ExecuteQuery` | **Unsupported** |
| `grpc.lookup.v1.RouteLookupService.RouteLookup` | **Unsupported**; the service is not registered |
| Pinned internal `GetClientConfiguration`, `OpenTable`, `OpenAuthorizedView`, `OpenMaterializedView` | **Unsupported** through the embedded unimplemented server |

### Table Admin API

| RPC group | Disposition |
| --- | --- |
| `CreateTable`, `ListTables`, `GetTable`, `UpdateTable`, `DeleteTable` | **Partial** |
| `ModifyColumnFamilies` | **Partial** |
| `DropRowRange` | **Supported** for local deletion; change-stream side effects are missing |
| `GenerateConsistencyToken`, `CheckConsistency` | **Partial** stubs |
| `CreateAuthorizedView`, `ListAuthorizedViews`, `GetAuthorizedView`, `UpdateAuthorizedView`, `DeleteAuthorizedView` | **Partial** metadata only |
| `CreateBackup`, `GetBackup`, `UpdateBackup`, `DeleteBackup`, `ListBackups`, `RestoreTable`, `CopyBackup` | **Partial** metadata only |
| `GetIamPolicy`, `SetIamPolicy`, `TestIamPermissions` | **Partial** permissive in-memory stubs |
| `UndeleteTable` | **Unsupported** |
| `CreateSchemaBundle`, `UpdateSchemaBundle`, `GetSchemaBundle`, `ListSchemaBundles`, `DeleteSchemaBundle` | **Unsupported** |
| Pinned legacy `CreateTableFromSnapshot`, `SnapshotTable`, `GetSnapshot`, `ListSnapshots`, `DeleteSnapshot` | **Unsupported**; not current public features |

### Instance Admin API

| RPC group | Disposition |
| --- | --- |
| `CreateInstance`, `GetInstance`, `ListInstances`, `UpdateInstance`, `PartialUpdateInstance`, `DeleteInstance` | **Partial** metadata |
| `CreateCluster`, `GetCluster`, `ListClusters`, `UpdateCluster`, `PartialUpdateCluster`, `DeleteCluster` | **Partial** metadata |
| `CreateAppProfile`, `GetAppProfile`, `ListAppProfiles`, `UpdateAppProfile`, `DeleteAppProfile` | **Partial** metadata |
| `GetIamPolicy`, `SetIamPolicy`, `TestIamPermissions` | **Partial** permissive in-memory stubs |
| `ListHotTablets` | **Unsupported** |
| `CreateLogicalView`, `GetLogicalView`, `ListLogicalViews`, `UpdateLogicalView`, `DeleteLogicalView` | **Partial** metadata; execution unsupported |
| `CreateMaterializedView`, `GetMaterializedView`, `ListMaterializedViews`, `UpdateMaterializedView`, `DeleteMaterializedView` | **Partial** custom subset |
| Current `GetMemoryLayer`, `ListMemoryLayers` | **Unsupported** and absent from the pinned interface |

### Auxiliary Admin services

| RPC group | Disposition |
| --- | --- |
| `google.cloud.location.Locations.ListLocations` | **Unsupported**; service not registered |
| `google.longrunning.Operations.GetOperation` | **Partial but unusable**; completed operations are not inserted into its store |
| `google.longrunning.Operations.ListOperations`, `WaitOperation` | **Unsupported** |

## Prioritized remediation

### P0 — prevent false-success and data-corruption semantics

1. **Make every single-row RPC genuinely atomic.** Validate and apply mutations/rules to a private row copy, then commit once; preserve per-entry status codes in `MutateRows`. Add failure-after-successful-first-mutation tests for `MutateRow`, `MutateRows`, `CheckAndMutateRow`, and `ReadModifyWriteRow`.
2. **Correct filter and GC truth tables.** Preserve duplicate `Interleave` cells and replace the incompatible dedup test; explicitly reject or correctly implement `Sink`; implement `ValueBitmask`; fix GC intersection to keep cells retained by *any* child rule and cover nested union/intersection.
3. **Never ignore semantic request targets.** Until implemented, explicitly reject authorized/materialized-view names, unsupported app-profile modes, and requested read stats. Honor `SampleRowKeys.row_range`. Silent acceptance is more dangerous than `Unimplemented`.

### P1 — make existing feature claims functional

4. **Change streams:** honor `Table.change_stream_config`, retention and requested partitions; group atomic mutations; record GC and `DropRowRange`; provide split/merge/close semantics and focused consumer tests.
5. **Backups:** capture immutable schema and row data at creation, copy the snapshot, restore without consulting the live source, enforce expiration/type constraints, and wire operations into a usable LRO store.
6. **Views:** enforce authorized-view subsets for reads and writes. Either execute logical/CMV SQL with documented semantics or narrow the admin APIs and fail unsupported queries/targets explicitly; never ignore `WHERE`.
7. **Current Table Admin:** persist and return current table/family fields, implement or explicitly reject initial splits with a documented local model, support change-stream/row-key-schema update masks, and add schema-bundle CRUD if structured types/SQL are targeted.

### P2 — broaden compatibility only after correctness

8. Extend the pinned core `cbt` smoke to advanced commands where product workflows require them, add at least one non-Go client, and decide whether to implement the pinned internal session protocol.
9. Complete LRO `Get/List/Wait` storage and pagination/etag behavior for metadata resources used by Terraform/bootstrap tools.
10. Implement GoogleSQL only if local query/view development is a product goal; otherwise keep SQL, Data Boost, parameterized views, and query stats explicitly unsupported.

### Explicitly defer as managed-service-only

Replication/failover, autoscaling/capacity, locations, memory layer, Data Boost
compute, CMEK, Cloud Monitoring/Key Visualizer, and Dataflow/BigQuery/Pub/Sub
pipelines should remain **Not applicable** unless a concrete local testing
contract is requested. Metadata stubs must say they do not emulate those
services.

## Official Google sources

- [Cloud Bigtable Data API RPC reference][data-rpc]
- [Detailed `google.bigtable.v2` messages and methods][data-detail]
- [Cloud Bigtable Admin API RPC reference][admin-rpc]
- [Detailed `google.bigtable.admin.v2` messages and methods][admin-detail]
- [Official Bigtable emulator contract and limits][emulator]
- [Filters][filters] and [garbage collection][garbage-collection]
- [Tables and views][tables-views], [authorized views][authorized-views], [parameterized views][parameterized-views], and [continuous materialized views][cmv]
- [GoogleSQL][sql] and [Data Boost][data-boost]
- [Change streams][change-streams] and [backups][backups]
- [IAM access control][iam], [CMEK][cmek], and [routing][routing]
- [Metrics][metrics], [client-side metrics][client-metrics], [instance monitoring][monitoring], [Key Visualizer][key-visualizer], [`gcloud bigtable`][gcloud-bigtable], and [`cbt`][cbt]
- [Import/export][import-export] and [client libraries][libraries]

[data-rpc]: https://docs.cloud.google.com/bigtable/docs/reference/data/rpc
[data-detail]: https://docs.cloud.google.com/bigtable/docs/reference/data/rpc/google.bigtable.v2
[admin-rpc]: https://docs.cloud.google.com/bigtable/docs/reference/admin/rpc
[admin-detail]: https://docs.cloud.google.com/bigtable/docs/reference/admin/rpc/google.bigtable.admin.v2
[emulator]: https://docs.cloud.google.com/bigtable/docs/emulator
[filters]: https://docs.cloud.google.com/bigtable/docs/using-filters
[garbage-collection]: https://docs.cloud.google.com/bigtable/docs/garbage-collection
[tables-views]: https://docs.cloud.google.com/bigtable/docs/tables-and-views
[authorized-views]: https://docs.cloud.google.com/bigtable/docs/authorized-views
[parameterized-views]: https://docs.cloud.google.com/bigtable/docs/parameterized-views-overview
[cmv]: https://docs.cloud.google.com/bigtable/docs/continuous-materialized-views
[sql]: https://docs.cloud.google.com/bigtable/docs/introduction-sql
[data-boost]: https://docs.cloud.google.com/bigtable/docs/data-boost-overview
[change-streams]: https://docs.cloud.google.com/bigtable/docs/change-streams-overview
[backups]: https://docs.cloud.google.com/bigtable/docs/backups
[iam]: https://docs.cloud.google.com/bigtable/docs/access-control
[cmek]: https://docs.cloud.google.com/bigtable/docs/cmek
[routing]: https://docs.cloud.google.com/bigtable/docs/routing
[instances]: https://docs.cloud.google.com/bigtable/docs/instances-clusters-nodes
[metrics]: https://docs.cloud.google.com/bigtable/docs/metrics
[client-metrics]: https://docs.cloud.google.com/bigtable/docs/client-side-metrics
[monitoring]: https://docs.cloud.google.com/bigtable/docs/monitoring-instance
[import-export]: https://docs.cloud.google.com/bigtable/docs/import-export
[libraries]: https://docs.cloud.google.com/bigtable/docs/reference/libraries
[key-visualizer]: https://docs.cloud.google.com/bigtable/docs/keyvis-overview
[gcloud-bigtable]: https://docs.cloud.google.com/sdk/gcloud/reference/bigtable
[cbt]: https://docs.cloud.google.com/bigtable/docs/cbt-reference

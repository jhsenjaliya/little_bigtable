# Bigtable Emulator Compatibility Audit

**Audit date:** 2026-10-04 (supersedes the 2026-08-29 audit)
**Upstream baseline:** Bigtable v2 Data API (`google.bigtable.v2.Bigtable`) and Admin API (`google.bigtable.admin.v2.BigtableTableAdmin`, `google.bigtable.admin.v2.BigtableInstanceAdmin`) as generated in `cloud.google.com/go/bigtable` **v1.58.0**; googleapis `master` protos and Bigtable product documentation fetched 2026-10-04.
**Implementation plan:** [`docs/superpowers/plans/2026-10-04-bigtable-parity-implementation-plan.md`](docs/superpowers/plans/2026-10-04-bigtable-parity-implementation-plan.md)
**Scope rule:** emulate every observable behavior that does not require production hardware or infrastructure. Exclude only the production guarantee itself, and record why.

## Runtime and configuration identity

| Item | Value at audit time |
| --- | --- |
| Repository | `github.com/jhsenjaliya/little_bigtable`, branch `jay-bigtable-extended`, base commit `9137de7` plus the uncommitted parity iteration |
| Generated API | `cloud.google.com/go/bigtable v1.58.0` (upgraded from v1.51.0), `cloud.google.com/go/longrunning v1.2.0`, `cloud.google.com/go/iam v1.12.0`, `google.golang.org/grpc v1.83.2` |
| Toolchain | Go 1.27.0 (`go.mod`) |
| SQL drivers | `github.com/glebarez/go-sqlite` (pure Go, registered as `sqlite3`) and `github.com/lib/pq` |
| Binary version string | `little_bigtable.go` still reports `0.3.0-localcloud` (not yet bumped for v0.5.0) |
| LocalCloud image | LocalCloud `Dockerfile` pins `LITTLE_BIGTABLE_VERSION=9137de7…` — the pre-iteration revision. No image containing this iteration has been built or verified. |
| Registered services (`bttest/inmem.go: NewServer`) | `google.bigtable.v2.Bigtable`, `BigtableTableAdmin`, `BigtableInstanceAdmin`, `google.longrunning.Operations`. `google.cloud.location.Locations` is not registered. |

## Method and status vocabulary

The parity method is the LocalCloud parity audit: each capability records an
ID, upstream reference, expected and observed behavior, implementation owner,
local or production scope, finding and implementation status, verification
tier, test evidence, runtime identity (above) and next action.

**Implementation status**

- **Implemented** — the local contract described here exists in source.
- **Partial** — a useful subset exists; the listed local gaps remain.
- **Build-gated** — implemented only when compiled with `-tags gsqlready` (see finding F-1); the default build returns `Unimplemented`.
- **Explicit rejection** — the RPC returns a documented error by design (normally `Unimplemented` with a reason).
- **Not registered** — the gRPC service is not served; clients receive `Unimplemented` from gRPC.

**Scope:** *Local* (deterministic single-process behavior that must be emulated) or *Production-only* (the defining guarantee needs production hardware or infrastructure).

**Finding labels**

| Finding | Meaning |
| --- | --- |
| Implementation gap | A required local path or semantic is absent |
| Behavioral defect | The path exists but observably violates the expected contract |
| Integration/configuration failure | The scenario does not reach the intended implementation or environment |
| Documentation error | A published claim conflicts with the implementation |
| Unverified | Evidence is absent, incomplete or no longer applicable |
| Production-only guarantee | The defining guarantee requires production infrastructure or hardware |

**Verification tier**

- **TB** — a focused behavioral test exists and passed.
- **SI** — source-inspected only.
- **Pending test evidence** — no owning test is recorded yet. A separate test pass is running; its owner fills the evidence column. Until then no capability in this audit is TB.

Evidence cells carry the marker `<!-- EVIDENCE: fill after test pass -->`.
The 2026-08-29 test names (for example `TestInterleaveDedup`,
`TestIAMStubs_Permissive`, `TestCMVWriteSync`) are not carried forward: they
asserted the previous behavior and several of their source files were removed.

## Audit-level findings

| ID | Finding | Observation | Next action |
| --- | --- | --- | --- |
| F-1 | Integration/configuration failure | `bttest/query_service.go` and `bttest/materialized_views.go` carry `//go:build gsqlready` and import `bttest/internal/gsql`. That package was not present in the working tree at audit time, and no build entry point (`Makefile`, `test.sh`, `dist.sh`, `Dockerfile`, GitHub workflows, LocalCloud `Dockerfile`) sets the tag. The default build compiles `bttest/gsql_stub.go`: `PrepareQuery`, `ExecuteQuery` and all materialized-view RPCs return `Unimplemented` ("GoogleSQL is not available in this build"), logical-view query validation is a no-op, materialized-view targets cannot be read, and HLL++ aggregate mutations are rejected with `InvalidArgument`. | Land `internal/gsql`, then either remove the build tag or set it in every build and test entry point; rerun the GoogleSQL, logical-view, CMV and HLL scenarios. |
| F-2 | Documentation error | `bttest/compatibility.go` (`CompatibilityLedger()`, last changed 2026-08-29) still declares session RPCs, schema bundles, `ListOperations`/`WaitOperation`/`DeleteOperation`/`CancelOperation` and others as `Unimplemented` or Phase-0 dispositions. The v1.58.0 interface also adds the memory-layer RPCs. The ledger drift test was not rerun for this documentation pass. | Update the ledger to this audit's dispositions in the test pass; rerun `TestCompatibilityLedgerCoversRegisteredRPCs`. |
| F-3 | Unverified | No runtime evidence exists for the packaged binary or image of this iteration (version string and LocalCloud pin are pre-iteration). | Commit, bump `version`, update the LocalCloud pin, then run the LocalCloud Bigtable scenarios against the rebuilt image. |

## Executive parity matrix

Verification for every row: **SI; pending test evidence**.

| ID | Area | Status | Local contract (summary) | Production-only exclusions | Remaining local gaps | Evidence |
| --- | --- | --- | --- | --- | --- | --- |
| BT-WRITE | Data-plane writes | Implemented | Validate-all-then-apply single-row atomicity; per-entry `MutateRows` codes; idempotency tokens; aggregate families | Cross-cluster write ordering | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-READ | Data-plane reads | Implemented | Target resolution, production-style chunking, request stats, `SampleRowKeys` row ranges and 512 KiB sampling | Production latency, tablet-based sampling | `last_scanned_row_key` not emitted | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-FILTER | Row filters | Implemented | Up-front validation; all `RowFilter` variants including `Interleave` duplicates, `Sink`, `ValueBitmask` | — | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-GC | Garbage collection | Implemented | Correct nested union/intersection; read-time GC on copies; write-time GC with change records | Asynchronous production GC timing | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TARGET | Targets, app profiles, authorized views | Implemented | Exactly one target; app-profile lookup and routing rules; authorized-view read/write enforcement | Data Boost compute, multi-cluster routing behavior | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TADMIN | Table administration | Implemented | Validated create, views, update masks, soft delete/undelete, atomic family changes, initial splits, row key schema, consistency tokens | Replication catch-up | Deprecated snapshot RPCs rejected | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-AGG | Aggregate (counter) families | Partial | Sum/Min/Max Int64 big-endian; HLL++ sketches | — | HLL++ encoding is emulator-local; HLL requires the `gsqlready` build (F-1) | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-IADMIN | Instance administration | Implemented (metadata) | Validated instance/cluster/app-profile CRUD, masks, pagination, deletion rules, cascading delete | Capacity, autoscaling effects, hot tablets, memory layers, locations inventory | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-LRO | Long-running operations | Implemented | Durable `operations_t` registry; Get/List/Wait/Delete/Cancel | Real asynchronous progress | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-IAM | IAM policies | Partial | Persistent policies with etags and update masks; `NotFound` for missing resources | — | No authorization enforcement (unauthenticated emulator) | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-BACKUP | Backups, copy, restore | Implemented | Immutable schema+row snapshots; expiry, type and quota rules; copy; restore independent of live source | Cross-region placement, CMEK custody | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-CS | Change streams | Partial | Opt-in per table; retention; grouped per-row records; GC and `DropRowRange` records; tokens, heartbeats, end time | Multi-cluster ordering metadata | Single partition; no split/merge | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-SB | Schema bundles | Implemented | CRUD; descriptor validation; compatibility check; limits; etags | — | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-SESSION | Session protocol | Implemented | `GetClientConfiguration`; `OpenTable`/`OpenAuthorizedView`/`OpenMaterializedView` with virtual ReadRow/MutateRow | Server-side load balancing (`session_load` is always 0) | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-SQL | GoogleSQL query | Build-gated | `PrepareQuery`/`ExecuteQuery` with prepared tokens, typed `ProtoRows`, checksums, resume tokens | Data Boost compute | F-1; query stats not recorded | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-LV | Logical views | Partial (validation build-gated) | CRUD, etags, deletion protection; executable through `ExecuteQuery` | — | Parameterized views; F-1 | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-CMV | Continuous materialized views | Build-gated | GoogleSQL definitions; read-only reads by `materialized_view_name`, sessions and SQL; limits; deletion protection | Eventual-consistency lag | Multi-column key encoding is an emulator choice; F-1 | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-REPL | Replication and consistency | Production-only | Single process; consistency tokens are validated and always consistent | Replication, failover, multi-cluster consistency | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-OBS | Observability | Partial | ReadRows request stats | Cloud Monitoring, Key Visualizer, client metrics, hot tablets | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-PERSIST | SQL persistence (repository extension) | Implemented | SQLite/PostgreSQL storage for all resource state, migrated on first start | — | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-CLIENT | Client/CLI compatibility | Unverified | Go client and pinned `cbt` workflows previously test-backed | — | Rerun after this iteration; other languages untested | Pending <!-- EVIDENCE: fill after test pass --> |

## 1. Data plane

Upstream: `google.bigtable.v2.Bigtable` in `google/bigtable/v2/bigtable.proto`
and `data.proto`; [Data API][data-rpc], [writes][writes], [reads][reads].

| ID | Capability | Observed local contract | Owner | Scope | Status / finding | Verification | Evidence | Next action |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| BT-WRITE-1 | `MutateRow` single-row atomicity | All mutations are validated before any is applied (`validateMutations`); the row is cloned, mutated, GC'd and persisted once (`stage`, `persist`). A failure leaves the row unchanged. | `bttest/mutation_engine.go`, `bttest/data_write.go: MutateRow` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | Add failure-after-valid-first-mutation test |
| BT-WRITE-2 | `MutateRows` per-entry status | One SQL transaction for the batch; each entry is atomic and reports its own code (`NotFound` unknown family, `InvalidArgument` invalid timestamp) instead of `Internal`; final per-row state is persisted. | `bttest/data_write.go: MutateRows` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | Mixed-success batch test |
| BT-WRITE-3 | Error contract | Unknown family returns `NotFound` "Requested column family not found". | `bttest/mutation_engine.go: unknownFamilyError` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | — |
| BT-WRITE-4 | Timestamp granularity | `MILLIS`/`MICROS` table granularity; `timestamp_origin` `CLIENT_AUTO_GENERATED` timestamps are truncated. | `bttest/mutation_engine.go: tableGranularity`, `validTimestamp` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | — |
| BT-WRITE-5 | `CheckAndMutateRow` | Predicate is evaluated on the view-restricted row; only the chosen branch is validated and applied. Requires single-cluster routing with `allow_transactional_writes` (`FailedPrecondition`). | `bttest/data_write.go: CheckAndMutateRow`, `bttest/targets.go` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | — |
| BT-WRITE-6 | `ReadModifyWriteRow` | All rules validated first; increment on a non-8-byte cell returns `FailedPrecondition`; rules on an aggregate family return `InvalidArgument`. Same routing rule as BT-WRITE-5. | `bttest/data_write.go: ReadModifyWriteRow` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | — |
| BT-WRITE-7 | Idempotency tokens | Tokens must be at least 8 bytes; replays within a 15-minute window are deduplicated; a `start_time` older than the window returns `FailedPrecondition`. Stored in `idempotency_t`. | `bttest/data_write.go: validateIdempotency`, `idempotencySeen`, `recordIdempotency` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | — |
| BT-READ-1 | `ReadRows` validation and targets | Exactly one of table, authorized view or materialized view; `rows_limit < 0` and Data Boost with `reversed` return `InvalidArgument`; filters validated before reading. | `bttest/data_read.go: ReadRows`, `bttest/targets.go: resolveReadTarget` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | — |
| BT-READ-2 | `ReadRows` chunking | Row key on the first chunk of a row, family/qualifier when changed, timestamp and labels on the first piece; large values split with `value_size`; responses flushed at about 1 MiB (`readChunkValueBytes`, `readResponseBytes`). | `bttest/data_read.go: newRowWriter`, `writeRow`, `flush` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | — |
| BT-READ-3 | Request stats | `request_stats_view = REQUEST_STATS_FULL` adds a final response carrying only `request_stats` (rows/cells seen and returned, frontend latency). | `bttest/data_read.go: ReadRows` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | — |
| BT-READ-4 | Read-time GC | GC policies are applied to read copies; results are not persisted by the read. | `bttest/data_read.go`, `bttest/inmem.go: gc` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | — |
| BT-READ-5 | `last_scanned_row_key` | Not emitted (optional in the upstream contract). | `bttest/data_read.go` | Local | Partial — Implementation gap (low impact) | SI | Pending <!-- EVIDENCE: fill after test pass --> | Emit on long filtered scans if a client depends on it |
| BT-READ-6 | `SampleRowKeys` | Honors `row_range`; includes initial split keys; samples every 512 KiB (`sampleEveryBytes`); the final sample is the range end key, or the empty "end of table" key, with the total offset. | `bttest/data_read.go: SampleRowKeys` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | — |
| BT-READ-7 | `PingAndWarm` | Validates the instance name (strict) and the app profile. | `bttest/localcloud_change_stream.go: PingAndWarm` | Local | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> | — |

### Filters

Upstream: `RowFilter` in `data.proto`; [filters][filters].

| ID | Capability | Observed local contract | Owner | Status | Verification | Evidence |
| --- | --- | --- | --- | --- | --- | --- |
| BT-FILTER-1 | Validation before reading | `Chain`/`Interleave` need at least two filters; label must match `^[a-z0-9-]{1,15}$`; at most one label transformer per `Chain`; `Sink` not allowed inside `Condition`; `value_bitmask` mask required; negative limits/offsets rejected; nesting depth at most 20. | `bttest/filter.go: validateFilter` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-FILTER-2 | `Interleave` | Keeps duplicate cells from matching branches (counted by later limit/offset filters). | `bttest/filter.go: evalFilter`, `mergeCells` | Implemented (behavior change) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-FILTER-3 | `Sink` | Outputs to the final result and nothing to its parent. | `bttest/filter.go: filterRow`, `evalFilter` | Implemented (behavior change) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-FILTER-4 | `Condition` | Predicate evaluated on a copy of the row. | `bttest/filter.go: evalFilter` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-FILTER-5 | `ValueBitmask` | Matches when `(value & mask) == mask`; value and mask lengths must match. | `bttest/filter.go: includeCell` | Implemented (new) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-FILTER-6 | Remaining filters | Row-key/family/qualifier/value regex, ranges, sample, limits, offset, strip value, apply label. Unknown variants return `Unimplemented`. | `bttest/filter.go` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |

### Garbage collection

Upstream: [garbage collection][garbage-collection]. Union deletes a cell when
any child rule deletes it; intersection deletes only when every child deletes
it. Owner: `bttest/inmem.go: applyGC`, `applyGCAt`, `gcDeletes`, `gcWithChanges`.
GC runs on read copies and during the write path (`stage`), and write-time GC
produces `GARBAGE_COLLECTION` change records when the table has a change
stream. **Status:** Implemented (nested union/intersection corrected).
**Verification:** SI; pending test evidence. Evidence: Pending <!-- EVIDENCE: fill after test pass -->.
**Production-only:** asynchronous production GC timing (production can take
days); the emulator applies rules deterministically.

### Targets, app profiles and authorized views

Upstream: `table_name`/`authorized_view_name`/`materialized_view_name` and
`app_profile_id` fields in `bigtable.proto`; [routing][routing],
[authorized views][authorized-views].

| ID | Capability | Observed local contract | Owner | Status | Verification | Evidence |
| --- | --- | --- | --- | --- | --- | --- |
| BT-TARGET-1 | Target selection | Exactly one target per request. | `bttest/targets.go: singleTarget`, `resolveReadTarget`, `resolveWriteTarget` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TARGET-2 | App profiles | An instance without registered metadata accepts any profile ID. Registered instance: `""` means `default`; an unknown profile returns `NotFound`. | `bttest/targets.go: resolveAppProfile` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TARGET-3 | Data Boost profiles | Accepted for reads; writes return `FailedPrecondition`. No separate compute. | `bttest/targets.go: isDataBoostProfile` | Implemented (local rules); compute is Production-only | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TARGET-4 | Authorized-view reads | Compiled `SubsetView`: `row_prefixes` (empty list = no rows, `""` = all rows) and `family_subsets` (`qualifiers`, `qualifier_prefixes`). Rows outside prefixes are skipped; cells outside the subset are filtered. | `bttest/targets.go: compileViewPolicy`, `rowAllowed`, `cellAllowed`, `restrict` | Implemented (behavior change) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TARGET-5 | Authorized-view writes | Writes outside the view return `PermissionDenied`; `DeleteFromRow` through a view returns `PermissionDenied`; `DeleteFromFamily` requires qualifier prefix `""` for that family. | `bttest/targets.go: checkMutations`, `checkRules`, `viewDenied` | Implemented (behavior change) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |

**Production-only exclusions (data plane):** production latency and throughput,
tablet-level sampling, Data Boost compute isolation, multi-cluster routing and
row affinity behavior. Local timings must not be used to validate production
performance.

## 2. Table administration

Upstream: `google/bigtable/admin/v2/bigtable_table_admin.proto`, `table.proto`,
`types.proto`; [Admin API][admin-rpc], [managing tables][managing-tables].

| ID | Capability | Observed local contract | Owner | Status | Verification | Evidence |
| --- | --- | --- | --- | --- | --- | --- |
| BT-TADMIN-1 | `CreateTable` validation | Table and family ID patterns; GC rules validated (including `max_age` at least 1 ms, at most 500 bytes); `value_type` must be Aggregate Sum/Min/Max (Int64 input) or HLL; change stream retention 1–7 days; row key schema validated; initial splits must be non-empty and are persisted. | `bttest/table_admin.go: CreateTable`, `validateGCRule`, `validateValueType`, `validateFamily`, `validateRowKeySchema` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TADMIN-2 | `GetTable` / `ListTables` views | Get defaults to `SCHEMA_VIEW`; `NAME_ONLY`, `REPLICATION_VIEW`/`ENCRYPTION_VIEW` (cluster states `READY`, `GOOGLE_DEFAULT_ENCRYPTION`), `FULL`. List defaults to `NAME_ONLY` with pagination. | `bttest/table_admin.go: GetTable`, `ListTables`, `tableView`, `clusterStatesLocked`, `googleDefaultEncryption` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TADMIN-3 | `UpdateTable` masks | `change_stream_config(.retention_period)`, `deletion_protection`, `row_key_schema` (set, or remove with `ignore_warnings`; modification rejected), `automated_backup_policy(.*)`, `tiered_storage_config`. `column_families` → `Unimplemented` (use `ModifyColumnFamilies`); `*` → `InvalidArgument`. Returns an LRO with `UpdateTableMetadata`. Disabling a change stream purges its records. | `bttest/table_admin.go: UpdateTable`, `mergeBackupPolicy` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TADMIN-4 | `DeleteTable` / `UndeleteTable` | Delete blocked (`FailedPrecondition`) by table or authorized-view deletion protection, or by a CMV that reads the table. Delete is a soft delete: rows and metadata move to `<id>@deleted@<micros>` and table IAM policies are removed. Undelete within 7 days restores the table with deletion protection enabled; `AlreadyExists` if a live table has the ID, `NotFound` otherwise; LRO with `UndeleteTableMetadata`. Expired tombstones are purged by the maintenance loop. | `bttest/table_admin.go: DeleteTable`, `checkTableDeletableLocked`, `UndeleteTable`, `purgeExpiredTombstones` | Implemented (new) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TADMIN-5 | `ModifyColumnFamilies` | Atomic: all modifications validated first; `update_mask` per modification (`gc_rule`; `value_type` immutable); drop physically removes family data; `ignore_warnings` accepted. | `bttest/table_admin.go: ModifyColumnFamilies`, `dropFamilyData` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TADMIN-6 | `DropRowRange` | One transaction; with a change stream, records per-row `DeleteFromFamily` changes (type `USER`). | `bttest/table_admin.go: DropRowRange`, `prefixRange` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TADMIN-7 | Consistency tokens | Token `base64("ct1\|<table>\|<micros>")`, validated against its table; always consistent (single process). | `bttest/table_admin.go: GenerateConsistencyToken`, `CheckConsistency` | Implemented (local); replication catch-up is Production-only | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TADMIN-8 | Snapshot RPCs | `CreateTableFromSnapshot`, `SnapshotTable`, `GetSnapshot`, `ListSnapshots`, `DeleteSnapshot` return `Unimplemented` ("deprecated private-alpha feature; use backups"). | `bttest/table_admin.go` | Explicit rejection — remaining local gap (deprecated API) | SI | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-TADMIN-9 | Metadata persistence | `tables_t` stores `"LBT\x02"` + gob of the proto `Table`, family order, counter, splits, create/delete times and table ID. Legacy gob family maps are migrated on load (one-way rewrite). | `bttest/sql_tables.go: encodeMetadata`, `decodeMetadata` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |

### Aggregate families

Upstream: `Type.Aggregate` in `types.proto`; "Aggregating values at write time"
and "Create and update counters in Bigtable" (docs fetched 2026-10-04).

| ID | Capability | Observed local contract | Owner | Status | Verification | Evidence |
| --- | --- | --- | --- | --- | --- | --- |
| BT-AGG-1 | Sum/Min/Max Int64 | `AddToCell`/`MergeToCell` require an aggregate family (`InvalidArgument` otherwise; previously any family was accepted) and validate qualifier, timestamp and input against the family `value_type`; big-endian int64 state. `ReadModifyWriteRow` on an aggregate family returns `InvalidArgument`. | `bttest/mutation_engine.go: familyAggregate`, `validateAggregateMutation`, `applyAggregate` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-AGG-2 | HLL++ | Sketch input/merge implemented in `query_service.go` (`newHLLSketch`, `decodeHLLSketch`, `hllInputBytes`). Sketch bytes are emulator-local, not ZetaSketch/BigQuery byte-compatible. | `bttest/query_service.go` (gsqlready), `bttest/mutation_engine.go` | Build-gated; Implementation gap for byte compatibility | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |

**Production-only exclusions (table admin):** replication catch-up behind
consistency tokens, tiered-storage placement (configuration is stored only),
table storage statistics derived from production storage.

**Remaining local gaps:** deprecated snapshot RPCs; HLL++ byte compatibility.

## 3. Schema bundles

Upstream: `SchemaBundle` RPCs in `bigtable_table_admin.proto`; "Create and
manage protobuf schemas" (docs fetched 2026-10-04).

| ID | Observed local contract | Owner | Status | Verification | Evidence |
| --- | --- | --- | --- | --- | --- |
| BT-SB-1 | CRUD with pagination, etags and LRO metadata. `proto_schema` must be a valid `FileDescriptorSet`; `avro_schema` must be valid JSON. At most 10 bundles per table (`ResourceExhausted`), 4 MB per bundle. The schema oneof is immutable. An update that removes a message type fails `FailedPrecondition` unless `ignore_warnings`. | `bttest/localcloud_schema_bundles.go` | Implemented (new) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |

## 4. Instance administration, LRO and IAM

Upstream: `bigtable_instance_admin.proto`, `instance.proto`,
`google/longrunning/operations.proto`, `google/iam/v1`; [instances, clusters
and nodes][instances], [access control][iam].

| ID | Capability | Observed local contract | Owner | Status | Verification | Evidence |
| --- | --- | --- | --- | --- | --- | --- |
| BT-IADMIN-1 | Instances | ID 6–33 characters; at least one cluster required; display name 4–30; edition defaults to `ENTERPRISE`, type to `PRODUCTION`; a default app profile (single-cluster to the first cluster, transactional writes) is created. `PartialUpdateInstance` masks `display_name`, `labels`, `type` (`PRODUCTION`→`DEVELOPMENT` rejected), `edition`. `DeleteInstance` is blocked by protected tables, authorized views, logical views, materialized views or backups, and otherwise cascades. | `bttest/localcloud_instance_admin.go` | Implemented (metadata) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-IADMIN-2 | Clusters | Cluster ID rule; `UpdateCluster` `serve_nodes`; `PartialUpdateCluster` `serve_nodes`, `cluster_config` (autoscaling), `node_scaling_factor`. `DeleteCluster` blocked for the last cluster, while backups exist, or while an app profile routes to it. | `bttest/localcloud_instance_admin.go` | Implemented (metadata); capacity effects Production-only | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-IADMIN-3 | App profiles | Routing must reference existing clusters; etags. | `bttest/localcloud_instance_admin.go` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-IADMIN-4 | Pagination | `ListInstances`, `ListClusters`, `ListAppProfiles` paginate; `-` wildcard supported. | `bttest/localcloud_instance_admin.go` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-IADMIN-5 | Hot tablets, memory layers | `ListHotTablets`, `GetMemoryLayer`, `ListMemoryLayers`, `UpdateMemoryLayer` return `Unimplemented` with a reason. | `bttest/localcloud_instance_admin.go` | Explicit rejection — Production-only guarantee | SI | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-IADMIN-6 | Locations | `google.cloud.location.Locations` not registered. | `bttest/inmem.go: NewServer` | Not registered — Production-only guarantee (real location inventory) | SI | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-LRO-1 | Operations | Durable `operations_t`; names `operations/<resource>/locations/local/operations/<n>`; `GetOperation`; `ListOperations` (name prefix, filter `done=true\|false` only, pagination); `WaitOperation`; `DeleteOperation`; `CancelOperation` (no-op for done operations). All admin operations complete before the response returns. | `bttest/operations.go` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-IAM-1 | IAM policy storage | Policies persisted in `iam_policies_t`; etag mismatch returns `ABORTED`; `update_mask` honored; missing resources return `NotFound`; table deletion removes table policies. | `bttest/sql_iam.go`, `bttest/localcloud_instance_admin.go: GetIamPolicy`, `SetIamPolicy` | Implemented (behavior change) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-IAM-2 | IAM enforcement | `TestIamPermissions` grants every requested permission; no Data/Admin call is authorized against policies. | `bttest/sql_iam.go: testIamPermissions` | Partial — Implementation gap (needs authenticated identities; the emulator is unauthenticated by contract) | SI | Pending <!-- EVIDENCE: fill after test pass --> |

**Production-only exclusions:** node capacity and autoscaling effects, tablet
balancing and hot tablets, memory layers, real location inventory, CMEK key
custody (`GOOGLE_DEFAULT_ENCRYPTION` is reported), transport authentication.

## 5. Backups

Upstream: backup RPCs in `bigtable_table_admin.proto`; [backups][backups],
"Manage backups" (docs fetched 2026-10-04).

| ID | Capability | Observed local contract | Owner | Status | Verification | Evidence |
| --- | --- | --- | --- | --- | --- | --- |
| BT-BACKUP-1 | `CreateBackup` | Immutable snapshot: schema manifest in `backup_manifests_t` and rows copied into `rows_t` under the backup name. `expire_time` 6 h–90 d (365 d for Enterprise Plus). `HOT`/`STANDARD`; `hot_to_standard_time` at least 24 h and only for `HOT`. Quotas: 150 standard / 10 hot per table per cluster (`ResourceExhausted`). `size_bytes` reported; state `READY`; `GOOGLE_DEFAULT_ENCRYPTION`. | `bttest/localcloud_backups.go: CreateBackup`, `snapshotRows`, `validateBackupTimes`, `checkBackupQuotaLocked` | Implemented (behavior change) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-BACKUP-2 | `CopyBackup` | Source must be `READY`; copy of a copy rejected; result is always `STANDARD`; expiry at most 30 days after source creation; the snapshot is copied. | `bttest/localcloud_backups.go: CopyBackup` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-BACKUP-3 | `RestoreTable` | New table only; restored from the snapshot, independent of the live source; GC policies, automated backup policy and deletion protection are not inherited; `RestoreInfo` set; `OptimizeRestoredTable` LRO plus `RestoreTableMetadata`. | `bttest/localcloud_backups.go: RestoreTable` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-BACKUP-4 | `ListBackups` | Cluster `-` wildcard; filter terms `name`, `source_table`, `state`, `backup_type`, `source_backup` with `=` or `:` joined by `AND`; `order_by`; pagination. Expired backups are hidden and purged by the maintenance loop. | `bttest/localcloud_backups.go: ListBackups`, `backupMatches`, `orderBackups`, `purgeExpiredBackups` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-BACKUP-5 | Deletion rules | `DeleteCluster`/`DeleteInstance` blocked while backups exist. | `bttest/localcloud_backups.go: backupsUnder`, `bttest/localcloud_instance_admin.go` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |

**Production-only exclusions:** cross-region backup placement and durability
guarantees, CMEK-protected backups, incremental backup storage accounting.
Automated backup policy is stored and validated; no scheduler creates backups
(not recorded in the implementation notes — Unverified).

## 6. Change streams

Upstream: `GenerateInitialChangeStreamPartitions`, `ReadChangeStream` in
`bigtable.proto`; [change streams][change-streams], "Configure change streams".

| ID | Capability | Observed local contract | Owner | Status | Verification | Evidence |
| --- | --- | --- | --- | --- | --- | --- |
| BT-CS-1 | Enablement and retention | Records are written only while `change_stream_config` is set; retention 1–7 days; reading a table without a change stream returns `FailedPrecondition`; expired records purged every minute; disabling purges records. | `bttest/localcloud_change_stream.go: validateChangeStreamConfig`, `purgeExpiredChangeRecords`; `bttest/inmem.go: maintenanceLoop` | Implemented (behavior change) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-CS-2 | Record model | Transactional outbox `change_stream_t`: one record per row commit, type `USER` or `GARBAGE_COLLECTION`, mutations grouped; `DeleteFromRow` recorded as `DeleteFromFamily` per family; `DropRowRange` recorded per row. The previous `change_log_t` is dropped. | `bttest/localcloud_change_stream.go`, `bttest/data_write.go: writeCommits` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-CS-3 | Routing | App profile must use single-cluster routing (`FailedPrecondition` otherwise). | `bttest/localcloud_change_stream.go: ReadChangeStream` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-CS-4 | Stream protocol | One full-keyspace partition; the requested partition range filters records; opaque tokens `base64("cs1:<id>")`; `start_time` must be ≤ now and within retention (`InvalidArgument`); no start time or tokens → from now; heartbeats (default 5 s, must be positive) carry a continuation token and low watermark; `end_time` → `CloseStream` with OK. | `bttest/localcloud_change_stream.go: ReadChangeStream`, `changeStreamStart`, `heartbeatResponse`, `sendCloseStream` | Implemented | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-CS-5 | Partition split/merge | Not produced; a single partition is valid but does not exercise client split handling. | `bttest/localcloud_change_stream.go: fullStreamPartition` | Partial — Implementation gap | SI | Pending <!-- EVIDENCE: fill after test pass --> |

**Production-only exclusions:** multi-cluster ordering metadata and
cross-cluster tiebreaking (single source cluster), Dataflow connector behavior.

## 7. Session protocol

Upstream: `GetClientConfiguration`, `OpenTable`, `OpenAuthorizedView`,
`OpenMaterializedView` in `bigtable.proto` (v1.58.0).

| ID | Observed local contract | Owner | Status | Verification | Evidence |
| --- | --- | --- | --- | --- | --- |
| BT-SESSION-1 | `GetClientConfiguration` returns `session_load` 0 and stops polling. Open RPCs validate the target on open (`NotFound` etc.) and READ/WRITE permissions, then serve virtual ReadRow/MutateRow through the same pipeline as the unary RPCs (authorized-view restrictions apply; materialized views are read-only). Per-request failures are returned as `SessionResponse.error`; the stream stays open. Unsupported session requests return `Unimplemented`. | `bttest/session_streaming.go: openSession`, `sessionPermissions`, `sessionVirtualRPC`, `sessionReadRow` | Implemented (new) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |

## 8. GoogleSQL, logical views and continuous materialized views

Upstream: `PrepareQuery`/`ExecuteQuery` in `bigtable.proto`; logical and
materialized view RPCs in `bigtable_instance_admin.proto`; [GoogleSQL][sql],
[tables and views][tables-views], [continuous materialized views][cmv],
"Continuous materialized view queries" (docs fetched 2026-10-04).
All rows in this section are subject to **F-1**.

| ID | Capability | Observed local contract | Owner | Status | Verification | Evidence |
| --- | --- | --- | --- | --- | --- | --- |
| BT-SQL-1 | `PrepareQuery` | Self-describing `prepared_query` token valid for 1 hour (`valid_until`). | `bttest/query_service.go: PrepareQuery` | Build-gated | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-SQL-2 | `ExecuteQuery` | Accepts `prepared_query` or the deprecated `query` with typed params. `FailedPrecondition` `PREPARED_QUERY_EXPIRED` with `PreconditionFailure` when the schema changed. `ProtoRows` batches with CRC32C checksums and resume tokens (row counts). Metadata sent first only for the deprecated query path. | `bttest/query_service.go: ExecuteQuery`, `preparedQueryExpired` | Build-gated | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-SQL-3 | Query stats | Not recorded in the implementation notes. | — | Unverified | — | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-LV-1 | Logical views | CRUD; query validated by the GoogleSQL engine (`validateViewQuery`); parent-scoped list with pagination; etags; deletion protection; LRO metadata; executable via `ExecuteQuery` (`FROM <view>`). In the default build validation is a no-op and execution is unavailable. | `bttest/localcloud_logical_views.go`, `bttest/query_service.go` | Partial (CRUD implemented; validation and execution build-gated) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-LV-2 | Parameterized views | `view_parameters` not supported (depends on engine support). | — | Implementation gap | SI | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-CMV-1 | CMV definition | `CreateMaterializedView` with a GoogleSQL `GROUP BY` or `ORDER BY` query (`gsql.PrepareMaterializedView`); query immutable (only `deletion_protection` updatable); 50 per instance, 5 per table (`ResourceExhausted`); deletion protection enforced (`FailedPrecondition`). | `bttest/materialized_views.go` | Build-gated | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |
| BT-CMV-2 | CMV storage and reads | Stored under hidden storage `__mv__/<id>`; key = `_key` or an OrderedCodeBytes struct of key columns (emulator choice); value columns in family `default` (qualifier = alias); map columns as their own family; cell timestamp 0 or `_timestamp`. Read via `materialized_view_name` (`ReadRows`, `SampleRowKeys`), sessions and SQL; read-only. Recomputed when stale before a read. Legacy shadow tables removed on load. Details: [`CMV_SUPPORT.md`](CMV_SUPPORT.md). | `bttest/materialized_views.go: newMaterializedView`, `recompute`, `rowKey`, `readable`, `LoadMaterializedViews` | Build-gated (behavior change) | SI; pending test evidence | Pending <!-- EVIDENCE: fill after test pass --> |

**Production-only exclusions:** Data Boost compute isolation for SQL;
eventual-consistency lag of production CMVs (the emulator recomputes before
reads, so reads observe every committed source write); CMV `user_errors`
metrics.

**Remaining local gaps:** F-1; parameterized views; CMV multi-column key
encoding is not documented by production (OrderedCodeBytes is an emulator
choice); `_timestamp` values are truncated to milliseconds instead of
production's rule that non-multiples of 1,000 are invalid rows (Unverified
divergence).

## 9. Replication, routing and observability

| ID | Capability | Observed local contract | Status |
| --- | --- | --- | --- |
| BT-REPL-1 | Replication, failover, multi-cluster consistency | Single process; consistency tokens always consistent; cluster metadata has no serving effect beyond routing validation. | Production-only guarantee |
| BT-REPL-2 | Routing validation | App-profile routing rules enforced for transactional RPCs, change streams and Data Boost writes (section 1). | Implemented (local) |
| BT-OBS-1 | ReadRows request stats | See BT-READ-3. | Implemented |
| BT-OBS-2 | Cloud Monitoring, Key Visualizer, client-side metrics, hot tablets | Not emulated. `little_bigtable.go` disables gRPC tracing. | Production-only guarantee |

Verification for this section: SI; pending test evidence. Evidence: Pending <!-- EVIDENCE: fill after test pass -->.

For production hotspot or latency diagnosis use [Key Visualizer][key-visualizer],
`ListHotTablets` and [metrics][metrics]; none are emulated.

## 10. Persistence (repository extension)

SQL persistence is not a Bigtable parity feature. Persisted state
(`bttest/sql_schema.go: CreateTables`):

| SQL table | Contents |
| --- | --- |
| `rows_t` | Row data for tables, tombstoned tables, backup snapshots and CMV storage (`__mv__/<id>`) |
| `tables_t` | Table metadata (`LBT\x02` format; migrated on first start) |
| `instances_t`, `clusters_t`, `app_profiles_t` | Instance admin metadata |
| `authorized_views_t`, `logical_views_t`, `materialized_views_t`, `schema_bundles_t` | View and schema-bundle definitions |
| `backups_t`, `backup_manifests_t` | Backup metadata and schema manifests |
| `change_stream_t` | Change-stream outbox (replaces `change_log_t`, which is dropped) |
| `iam_policies_t` | IAM policies |
| `operations_t` | Long-running operations |
| `idempotency_t` | Mutation idempotency tokens |

Storage errors are returned to the caller (`bttest/storage.go: storageErr`:
context errors map to `DeadlineExceeded`/`Canceled`, others to `Internal`); no
`log.Fatal` on request paths. The maintenance loop (`bttest/inmem.go:
maintenanceLoop`, every minute) purges expired change records, backups and
table tombstones.

## 11. Client and CLI compatibility

- **Go client and pinned `cbt` smoke:** previously test-backed
  (`TestGoogleDocsHelloWorldGoClientWorkflow`, `TestCBTClientConformance`); the
  client library and server changed in this iteration. Status: **Unverified**
  until the test pass reruns them.
- **`BIGTABLE_EMULATOR_HOST`:** unchanged transport contract ([emulator][emulator]).
- **Other client languages, HBase, Beam/Dataflow, BigQuery and Pub/Sub
  connectors:** not covered. Connectors are Production-only managed integrations.
- **gcloud emulator lifecycle:** not provided; this is a separate binary.

## Behavior changes in this release

| Area | Previous behavior | New behavior |
| --- | --- | --- |
| Mutations | Failed requests could partially apply; `MutateRows` reported `Internal` | No partial application; entries report real codes (`NotFound` unknown family, `InvalidArgument` invalid timestamp) |
| Unknown family | Varied | `NotFound` "Requested column family not found" |
| Aggregate mutations | `AddToCell`/`MergeToCell` performed int64 addition in any family | Require a family with an aggregate `value_type` (`InvalidArgument` otherwise; source-verified, not listed in the implementation notes) |
| Filters and GC | `Interleave` deduplicated; `Sink` partial no-op; `ValueBitmask` unsupported; GC intersection deleted when any child deleted | `Interleave` returns duplicates; `Sink` and `ValueBitmask` implemented; intersection deletes only when all children delete |
| Targets | Authorized/materialized view names and app profiles ignored | Enforced |
| Instances | Created without clusters, any ID | At least one cluster and a 6–33 character ID required |
| IAM | In-memory, any resource name | Persisted; resource must exist |
| `SampleRowKeys` | Random samples, `row_range` ignored | Split points and 512 KiB samples within `row_range`, ending with the end key or the empty end-of-table key |
| Backups | Metadata only; restore read the live table | Data snapshots; `expire_time` validated |
| CMVs | Readable as a regular table named after the view | Read with `materialized_view_name` or SQL; legacy shadow tables removed on startup |
| Change streams | Logged every table; `change_log_t` | Record only when `change_stream_config` is set; `change_log_t` dropped |
| Storage | — | Table metadata format migrated on first start (one-way; an older binary cannot read it) |

## Prior audit findings → resolution

Mapping of the 2026-08-29 "Prioritized remediation" items. "Resolved in
source" means implemented per the notes and source inspection; verification is
pending test evidence for every row.

| Prior item | Resolution | Owner | Remaining |
| --- | --- | --- | --- |
| P0-1 Single-row atomicity, per-entry `MutateRows` codes | Resolved in source | `mutation_engine.go`, `data_write.go` | Test evidence |
| P0-2 `Interleave`, `Sink`, `ValueBitmask`, GC intersection | Resolved in source | `filter.go`, `inmem.go` | Test evidence; replace the old dedup test |
| P0-3 Never ignore targets; read stats; `SampleRowKeys.row_range` | Resolved in source | `targets.go`, `data_read.go` | Test evidence |
| P1-4 Change streams: config, retention, partitions, grouping, GC and `DropRowRange` records | Resolved in source except split/merge | `localcloud_change_stream.go`, `table_admin.go` | Partition split/merge (local gap) |
| P1-5 Backups: snapshots, copy, independent restore, expiry/type, LRO store | Resolved in source | `localcloud_backups.go`, `operations.go` | Test evidence |
| P1-6 Views: enforce authorized views; execute or reject logical/CMV SQL | Authorized views resolved; logical views and CMVs implemented on GoogleSQL | `targets.go`, `localcloud_logical_views.go`, `materialized_views.go` | F-1 build gating; parameterized views |
| P1-7 Current Table Admin fields, initial splits, update masks, schema bundles | Resolved in source | `table_admin.go`, `sql_tables.go`, `localcloud_schema_bundles.go` | Test evidence |
| P2-8 Advanced `cbt`, non-Go client, session protocol | Session protocol implemented; client breadth not addressed | `session_streaming.go` | Non-Go client and advanced `cbt` coverage |
| P2-9 LRO Get/List/Wait, pagination, etags | Resolved in source | `operations.go`, admin handlers | Test evidence |
| P2-10 GoogleSQL | Implemented behind `gsqlready` | `query_service.go`, `internal/gsql` | F-1; parameterized views; query stats unverified |
| Deferred managed-service items | Retained as Production-only exclusions; hot tablets and memory layers return `Unimplemented` with a reason; Locations not registered | `localcloud_instance_admin.go`, `inmem.go` | — |

## Appendix A — RPC disposition

Every RPC of the v1.58.0 generated services plus the auxiliary services.
Verification for every row: **SI; pending test evidence** <!-- EVIDENCE: fill after test pass -->.

### `google.bigtable.v2.Bigtable`

| RPC | Disposition | Owner |
| --- | --- | --- |
| `ReadRows` | Implemented | `data_read.go` |
| `SampleRowKeys` | Implemented | `data_read.go` |
| `MutateRow` | Implemented | `data_write.go` |
| `MutateRows` | Implemented | `data_write.go` |
| `CheckAndMutateRow` | Implemented | `data_write.go` |
| `PingAndWarm` | Implemented (validation only) | `localcloud_change_stream.go` |
| `ReadModifyWriteRow` | Implemented | `data_write.go` |
| `GenerateInitialChangeStreamPartitions` | Implemented (single partition) | `localcloud_change_stream.go` |
| `ReadChangeStream` | Partial (no split/merge) | `localcloud_change_stream.go` |
| `PrepareQuery` | Build-gated | `query_service.go` / `gsql_stub.go` |
| `ExecuteQuery` | Build-gated | `query_service.go` / `gsql_stub.go` |
| `GetClientConfiguration` | Implemented | `session_streaming.go` |
| `OpenTable` | Implemented | `session_streaming.go` |
| `OpenAuthorizedView` | Implemented | `session_streaming.go` |
| `OpenMaterializedView` | Implemented (materialized-view reads build-gated) | `session_streaming.go` |

### `google.bigtable.admin.v2.BigtableTableAdmin`

| RPC | Disposition | Owner |
| --- | --- | --- |
| `CreateTable` | Implemented | `table_admin.go` |
| `CreateTableFromSnapshot` | Explicit rejection (deprecated) | `table_admin.go` |
| `ListTables` | Implemented | `table_admin.go` |
| `GetTable` | Implemented | `table_admin.go` |
| `UpdateTable` | Implemented | `table_admin.go` |
| `DeleteTable` | Implemented (soft delete) | `table_admin.go` |
| `UndeleteTable` | Implemented | `table_admin.go` |
| `CreateAuthorizedView` | Implemented | `localcloud_authorized_views.go` |
| `ListAuthorizedViews` | Implemented | `localcloud_authorized_views.go` |
| `GetAuthorizedView` | Implemented | `localcloud_authorized_views.go` |
| `UpdateAuthorizedView` | Implemented | `localcloud_authorized_views.go` |
| `DeleteAuthorizedView` | Implemented | `localcloud_authorized_views.go` |
| `ModifyColumnFamilies` | Implemented | `table_admin.go` |
| `DropRowRange` | Implemented | `table_admin.go` |
| `GenerateConsistencyToken` | Implemented (local) | `table_admin.go` |
| `CheckConsistency` | Implemented (always consistent) | `table_admin.go` |
| `SnapshotTable` | Explicit rejection (deprecated) | `table_admin.go` |
| `GetSnapshot` | Explicit rejection (deprecated) | `table_admin.go` |
| `ListSnapshots` | Explicit rejection (deprecated) | `table_admin.go` |
| `DeleteSnapshot` | Explicit rejection (deprecated) | `table_admin.go` |
| `CreateBackup` | Implemented | `localcloud_backups.go` |
| `GetBackup` | Implemented | `localcloud_backups.go` |
| `UpdateBackup` | Implemented | `localcloud_backups.go` |
| `DeleteBackup` | Implemented | `localcloud_backups.go` |
| `ListBackups` | Implemented | `localcloud_backups.go` |
| `RestoreTable` | Implemented | `localcloud_backups.go` |
| `CopyBackup` | Implemented | `localcloud_backups.go` |
| `GetIamPolicy` | Implemented (persistent, not enforced) | `localcloud_instance_admin.go`, `sql_iam.go` |
| `SetIamPolicy` | Implemented (persistent, not enforced) | `localcloud_instance_admin.go`, `sql_iam.go` |
| `TestIamPermissions` | Partial (grants all) | `localcloud_instance_admin.go`, `sql_iam.go` |
| `CreateSchemaBundle` | Implemented | `localcloud_schema_bundles.go` |
| `UpdateSchemaBundle` | Implemented | `localcloud_schema_bundles.go` |
| `GetSchemaBundle` | Implemented | `localcloud_schema_bundles.go` |
| `ListSchemaBundles` | Implemented | `localcloud_schema_bundles.go` |
| `DeleteSchemaBundle` | Implemented | `localcloud_schema_bundles.go` |

### `google.bigtable.admin.v2.BigtableInstanceAdmin`

| RPC | Disposition | Owner |
| --- | --- | --- |
| `CreateInstance` | Implemented (metadata) | `localcloud_instance_admin.go` |
| `GetInstance` | Implemented | `localcloud_instance_admin.go` |
| `ListInstances` | Implemented | `localcloud_instance_admin.go` |
| `UpdateInstance` | Implemented | `localcloud_instance_admin.go` |
| `PartialUpdateInstance` | Implemented | `localcloud_instance_admin.go` |
| `DeleteInstance` | Implemented (guarded cascade) | `localcloud_instance_admin.go` |
| `CreateCluster` | Implemented (metadata) | `localcloud_instance_admin.go` |
| `GetCluster` | Implemented | `localcloud_instance_admin.go` |
| `ListClusters` | Implemented | `localcloud_instance_admin.go` |
| `UpdateCluster` | Implemented (metadata) | `localcloud_instance_admin.go` |
| `PartialUpdateCluster` | Implemented (metadata) | `localcloud_instance_admin.go` |
| `DeleteCluster` | Implemented (guarded) | `localcloud_instance_admin.go` |
| `UpdateMemoryLayer` | Explicit rejection — Production-only | `localcloud_instance_admin.go` |
| `ListMemoryLayers` | Explicit rejection — Production-only | `localcloud_instance_admin.go` |
| `GetMemoryLayer` | Explicit rejection — Production-only | `localcloud_instance_admin.go` |
| `CreateAppProfile` | Implemented | `localcloud_instance_admin.go` |
| `GetAppProfile` | Implemented | `localcloud_instance_admin.go` |
| `ListAppProfiles` | Implemented | `localcloud_instance_admin.go` |
| `UpdateAppProfile` | Implemented | `localcloud_instance_admin.go` |
| `DeleteAppProfile` | Implemented | `localcloud_instance_admin.go` |
| `GetIamPolicy` | Implemented (persistent, not enforced) | `localcloud_instance_admin.go`, `sql_iam.go` |
| `SetIamPolicy` | Implemented (persistent, not enforced) | `localcloud_instance_admin.go`, `sql_iam.go` |
| `TestIamPermissions` | Partial (grants all) | `localcloud_instance_admin.go`, `sql_iam.go` |
| `ListHotTablets` | Explicit rejection — Production-only | `localcloud_instance_admin.go` |
| `CreateLogicalView` | Implemented (validation build-gated) | `localcloud_logical_views.go` |
| `GetLogicalView` | Implemented | `localcloud_logical_views.go` |
| `ListLogicalViews` | Implemented | `localcloud_logical_views.go` |
| `UpdateLogicalView` | Implemented (validation build-gated) | `localcloud_logical_views.go` |
| `DeleteLogicalView` | Implemented | `localcloud_logical_views.go` |
| `CreateMaterializedView` | Build-gated | `materialized_views.go` / `gsql_stub.go` |
| `GetMaterializedView` | Build-gated | `materialized_views.go` / `gsql_stub.go` |
| `ListMaterializedViews` | Build-gated | `materialized_views.go` / `gsql_stub.go` |
| `UpdateMaterializedView` | Build-gated | `materialized_views.go` / `gsql_stub.go` |
| `DeleteMaterializedView` | Build-gated | `materialized_views.go` / `gsql_stub.go` |

### `google.longrunning.Operations`

| RPC | Disposition | Owner |
| --- | --- | --- |
| `GetOperation` | Implemented | `operations.go` |
| `ListOperations` | Implemented (`done` filter only) | `operations.go` |
| `WaitOperation` | Implemented (operations are already done) | `operations.go` |
| `DeleteOperation` | Implemented | `operations.go` |
| `CancelOperation` | Implemented (no-op for done operations) | `operations.go` |

### `google.cloud.location.Locations`

| RPC | Disposition | Owner |
| --- | --- | --- |
| `ListLocations` | Not registered — Production-only | `inmem.go: NewServer` |
| `GetLocation` | Not registered — Production-only | `inmem.go: NewServer` |

`grpc.lookup.v1.RouteLookupService.RouteLookup` is also not registered
(client-side routing infrastructure).

## Official sources

Interface: googleapis `google/bigtable/v2/{bigtable,data,types,request_stats,feature_flags}.proto`,
`google/bigtable/admin/v2/{bigtable_table_admin,bigtable_instance_admin,table,instance,types,common}.proto`
(master, fetched 2026-10-04) and the matching generated code in
`cloud.google.com/go/bigtable v1.58.0`. Semantics: the pages below, plus the
fetched pages "Aggregating values at write time", "Manage row key schemas",
"Create and manage protobuf schemas", "Create and manage logical views",
"Create and manage continuous materialized views", "Continuous materialized
view queries", "Configure change streams", "Manage backups", "Get query
stats", "Quotas and limits" and the release notes.

[data-rpc]: https://docs.cloud.google.com/bigtable/docs/reference/data/rpc
[admin-rpc]: https://docs.cloud.google.com/bigtable/docs/reference/admin/rpc
[emulator]: https://docs.cloud.google.com/bigtable/docs/emulator
[reads]: https://docs.cloud.google.com/bigtable/docs/reads
[writes]: https://docs.cloud.google.com/bigtable/docs/writes
[filters]: https://docs.cloud.google.com/bigtable/docs/using-filters
[garbage-collection]: https://docs.cloud.google.com/bigtable/docs/garbage-collection
[managing-tables]: https://docs.cloud.google.com/bigtable/docs/managing-tables
[tables-views]: https://docs.cloud.google.com/bigtable/docs/tables-and-views
[authorized-views]: https://docs.cloud.google.com/bigtable/docs/authorized-views
[cmv]: https://docs.cloud.google.com/bigtable/docs/continuous-materialized-views
[sql]: https://docs.cloud.google.com/bigtable/docs/introduction-sql
[change-streams]: https://docs.cloud.google.com/bigtable/docs/change-streams-overview
[backups]: https://docs.cloud.google.com/bigtable/docs/backups
[iam]: https://docs.cloud.google.com/bigtable/docs/access-control
[routing]: https://docs.cloud.google.com/bigtable/docs/routing
[instances]: https://docs.cloud.google.com/bigtable/docs/instances-clusters-nodes
[metrics]: https://docs.cloud.google.com/bigtable/docs/metrics
[key-visualizer]: https://docs.cloud.google.com/bigtable/docs/keyvis-overview

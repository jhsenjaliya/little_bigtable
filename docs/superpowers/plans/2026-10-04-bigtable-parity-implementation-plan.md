# Bigtable Parity Implementation Plan

**Status:** Phases 1–12 implemented and test-verified at commit `dd5f9e7` (tag `v0.5.0`); Phase 13: LocalCloud pin updated, image build and platform tests pending.

**Date:** 2026-10-04

**Supersedes:** [`2026-08-29-bigtable-conformance-implementation-plan.md`](2026-08-29-bigtable-conformance-implementation-plan.md)
**Audit:** [`../../../BIGTABLE_COMPATIBILITY.md`](../../../BIGTABLE_COMPATIBILITY.md) (2026-10-04, at `dd5f9e7`). Capability IDs (`BT-…`) refer to that audit, which names the owning test for each capability.

## Outcome

Emulate every observable Bigtable v2 Data and Admin API behavior that does not
require production hardware or infrastructure, against the API generated in
`cloud.google.com/go/bigtable v1.58.0`. A request either follows the documented
local contract or fails with an explicit, documented error. Production-only
guarantees are excluded individually, with the reason recorded.

## Rules

- Upstream semantics come from googleapis `master` protos and Bigtable
  documentation fetched 2026-10-04; the interface comes from the v1.58.0
  generated code.
- Validate the whole request before changing state. Storage errors are returned
  to the caller, never `log.Fatal` on a request path.
- One SQL transaction per logical commit; derived state (change records, CMV
  staleness, idempotency) is written in the same commit or after it.
- A phase is **verified** only when its acceptance criteria are covered by
  named, passing tests in `./test.sh`. At `dd5f9e7`, `./test.sh` (test, race,
  vet, gofmt) and `go mod verify` passed: 245 top-level tests (963 including
  subtests) plus the `internal/gsql` suite. Storage conformance ran on SQLite
  locally; the PostgreSQL lane runs in CI. `TestCBTClientConformance` skips
  when `cbt` is not installed.

## Status vocabulary

- **Implemented — test-verified:** shipped in `v0.5.0`; acceptance criteria owned by named tests (see the audit).
- **Pending:** not done.

## Phase summary

| # | Phase | Status | Audit IDs |
| --- | --- | --- | --- |
| 1 | API baseline upgrade | Implemented — test-verified | all |
| 2 | Storage and atomic mutation core | Implemented — test-verified | BT-WRITE, BT-PERSIST |
| 3 | Filters and garbage collection | Implemented — test-verified | BT-FILTER, BT-GC |
| 4 | Targets and authorized views | Implemented — test-verified | BT-TARGET |
| 5 | Read streaming, request stats, sampling | Implemented — test-verified | BT-READ |
| 6 | Table administration | Implemented — test-verified | BT-TADMIN, BT-AGG |
| 7 | Instance admin, LRO, IAM persistence | Implemented — test-verified | BT-IADMIN, BT-LRO, BT-IAM |
| 8 | Backup snapshots | Implemented — test-verified | BT-BACKUP |
| 9 | Change streams | Implemented — test-verified | BT-CS |
| 10 | Schema bundles | Implemented — test-verified | BT-SB |
| 11 | Session protocol | Implemented — test-verified | BT-SESSION |
| 12 | GoogleSQL engine, logical views, continuous materialized views | Implemented — test-verified | BT-SQL, BT-LV, BT-CMV, BT-AGG |
| 13 | LocalCloud integration and multi-platform image | Pin updated; image build and platform tests pending | — |

## Phase 1 — API baseline upgrade

**Scope:** upgrade generated APIs from `cloud.google.com/go/bigtable v1.51.0` to
v1.58.0 (`grpc v1.83.2`, `longrunning v1.2.0`, `iam v1.12.0`); make every new
RPC reachable through an explicit handler or a documented rejection.

**Files:** `go.mod`, `go.sum`; handler files listed in later phases.

**Acceptance criteria:**

- The module builds and `go vet` passes against v1.58.0.
- Every RPC of `Bigtable`, `BigtableTableAdmin`, `BigtableInstanceAdmin` and
  `Operations` has a disposition in `BIGTABLE_COMPATIBILITY.md` Appendix A.
- The executable ledger (`bttest/compatibility.go`) matches the registered
  RPC set (89 entries) and Appendix A.

**Status:** Implemented — test-verified (`TestCompatibilityLedgerCoversRegisteredRPCs`,
`TestCompatibilityLedgerFieldsAreExhaustive`).

**Excluded (production-only):** none.

## Phase 2 — Storage and atomic mutation core

**Scope:** error-returning SQL row store; transaction helper; validate-all
then apply; per-entry `MutateRows` status; idempotency tokens; timestamp
granularity; aggregate family application; write-time GC with change records.

**Files:** `bttest/storage.go` (replaces `sql_rows.go`), `bttest/mutation_engine.go`,
`bttest/data_write.go`, `bttest/sql_schema.go` (`idempotency_t`).

**Acceptance criteria:**

- A request whose second mutation is invalid leaves the row unchanged
  (`MutateRow`, `MutateRows` entry, `CheckAndMutateRow` branch, `ReadModifyWriteRow`).
- `MutateRows` reports `NotFound` for an unknown family and `InvalidArgument`
  for an invalid timestamp per entry; other entries commit.
- Unknown family message is "Requested column family not found".
- Only the chosen `CheckAndMutateRow` branch is validated and applied.
- Increment of a non-8-byte cell → `FailedPrecondition`; RMW on an aggregate
  family → `InvalidArgument`.
- Idempotency: tokens shorter than 8 bytes rejected; replay within 15 minutes
  applied once, including a token repeated within one `MutateRows` batch;
  `start_time` older than the window → `FailedPrecondition`.
- `MILLIS` granularity and `CLIENT_AUTO_GENERATED` truncation.
- Storage failures map to `DeadlineExceeded`/`Canceled`/`Internal`; no process exit.

**Status:** Implemented — test-verified (owning tests per capability in the audit).

**Excluded (production-only):** cross-cluster write ordering.

## Phase 3 — Filters and garbage collection

**Scope:** pure filter evaluation with up-front validation; production truth
tables for `Interleave`, `Sink`, `Condition`, `ValueBitmask`; nested GC union
and intersection.

**Files:** `bttest/filter.go`, `bttest/inmem.go` (`applyGC`, `applyGCAt`,
`gcDeletes`, `gcWithChanges`).

**Acceptance criteria:**

- Validation rejects: `Chain`/`Interleave` with fewer than two filters, labels
  not matching `^[a-z0-9-]{1,15}$`, two label transformers in one `Chain`,
  `Sink` in `Condition`, missing bitmask, negative limits/offsets, depth > 20.
- `Interleave` returns duplicate cells and later limits count them.
- `Sink` emits to the final result and nothing to its parent.
- `ValueBitmask` matches `(v & mask) == mask` with equal lengths.
- GC: union deletes if any child deletes; intersection only if all delete;
  nested rules; read-time GC is not persisted.

**Status:** Implemented — test-verified (`filter_conformance_test.go`).

**Excluded (production-only):** asynchronous GC timing.

## Phase 4 — Targets and authorized views

**Scope:** one target per request; app-profile resolution and routing rules;
Data Boost write rejection; authorized-view CRUD with enforcement on reads and
writes.

**Files:** `bttest/targets.go`, `bttest/localcloud_authorized_views.go`,
`bttest/sql_proto_store.go` (etags).

**Acceptance criteria:**

- Zero or multiple targets → `InvalidArgument`.
- Registered instance: `""` app profile means `default`; unknown → `NotFound`.
- Data Boost profile writes → `FailedPrecondition`; `CheckAndMutateRow`/RMW
  require single-cluster routing with transactional writes.
- Authorized-view reads skip rows outside `row_prefixes` (empty list = none,
  `""` = all) and cells outside `family_subsets`.
- Writes outside the view → `PermissionDenied`; `DeleteFromRow` via a view →
  `PermissionDenied`; `DeleteFromFamily` requires qualifier prefix `""`.
- CRUD: etag mismatch → `ABORTED`; Get default `BASIC`, List default
  `NAME_ONLY`; at most 10 qualifier prefixes; masks `subset_view`,
  `deletion_protection`; typed LRO metadata.

**Status:** Implemented — test-verified (owning tests per capability in the audit).

**Excluded (production-only):** Data Boost compute isolation; multi-cluster
routing and row affinity behavior.

## Phase 5 — Read streaming, request stats, sampling

**Scope:** production-style `ReadRows` chunking; request stats; read
validation; `SampleRowKeys` with row ranges and split keys.

**Files:** `bttest/data_read.go`.

**Acceptance criteria:**

- `rows_limit < 0` and Data Boost with `reversed` → `InvalidArgument`.
- Chunks: row key on the first chunk of a row; family/qualifier only when
  changed; timestamp and labels on the first piece; values above
  `readChunkValueBytes` split with `value_size`; responses flushed near 1 MiB.
- `REQUEST_STATS_FULL` adds a final response with only `request_stats`.
- `SampleRowKeys` honors `row_range`, includes initial splits, samples every
  512 KiB, and ends with the range end key or the empty end-of-table key.

**Status:** Implemented — test-verified (owning tests per capability in the audit).

**Remaining local gap:** `last_scanned_row_key` not emitted.

**Excluded (production-only):** production latency; tablet-based sampling.

## Phase 6 — Table administration

**Scope:** validated `CreateTable`; table views; `UpdateTable` masks; soft
delete and `UndeleteTable`; atomic `ModifyColumnFamilies`; `DropRowRange`
change records; consistency tokens; initial splits; row key schema; aggregate
`value_type`; metadata format migration.

**Files:** `bttest/table_admin.go`, `bttest/sql_tables.go`, `bttest/inmem.go`
(table struct, tombstone purge), `bttest/mutation_engine.go` (aggregates).

**Acceptance criteria:**

- Create validates IDs, GC rules (`max_age` ≥ 1 ms, ≤ 500 bytes), aggregate
  types, change stream retention 1–7 days, row key schema, non-empty splits.
- Get default `SCHEMA_VIEW`; `REPLICATION_VIEW`/`ENCRYPTION_VIEW` report
  `READY` and `GOOGLE_DEFAULT_ENCRYPTION`; List default `NAME_ONLY`, paginated.
- Update masks as in BT-TADMIN-3; `column_families` → `Unimplemented`;
  `*` → `InvalidArgument`; LRO with `UpdateTableMetadata`.
- Delete blocked by table/authorized-view protection or a CMV source; soft
  delete renames storage and moves the table's authorized views and schema
  bundles with the tombstone; undelete within 7 days restores them and enables
  deletion protection (a re-created table starts without them);
  `AlreadyExists`/`NotFound` cases.
- Dropping a family removes its data; `value_type` is immutable.
- Consistency token is per table and always consistent.
- Pre-iteration `tables_t` rows load after migration; policy fields survive
  restart.

**Status:** Implemented — test-verified (owning tests per capability in the audit).

**Remaining local gaps:** deprecated snapshot RPCs return `Unimplemented`;
HLL++ sketch bytes are emulator-local.

**Excluded (production-only):** replication catch-up; tiered-storage placement;
production storage statistics.

## Phase 7 — Instance admin, LRO and IAM persistence

**Scope:** instance/cluster/app-profile validation and deletion rules;
pagination; durable operations; persisted IAM policies.

**Files:** `bttest/localcloud_instance_admin.go` (absorbs the removed
`instance_server.go`), `bttest/operations.go`, `bttest/sql_iam.go`,
`bttest/sql_admin_metadata.go`, `bttest/sql_schema.go` (`operations_t`,
`iam_policies_t`).

**Acceptance criteria:**

- Instance ID 6–33, at least one cluster, display name 4–30, defaults
  `ENTERPRISE`/`PRODUCTION`, default app profile created.
- Partial update masks; `PRODUCTION`→`DEVELOPMENT` rejected.
- `DeleteCluster` blocked for the last cluster, backups, or app-profile routing;
  `DeleteInstance` blocked by protected resources or backups, else cascades.
- List pagination and `-` wildcard.
- Operations named `operations/<resource>/locations/local/operations/<n>`
  survive restart; List prefix, `done` filter and pagination; Wait, Delete,
  Cancel.
- IAM: policy survives restart; etag mismatch → `ABORTED`; `update_mask`;
  missing resource → `NotFound`.
- `ListHotTablets` and memory-layer RPCs return `Unimplemented` with a reason.

**Status:** Implemented — test-verified (owning tests per capability in the audit).

**Remaining local gaps:** IAM enforcement (needs authenticated identities; the
emulator is unauthenticated by contract); an empty instance `display_name` is
accepted (defaults to the ID); `DeleteCluster` is not blocked by
automated-backup locations.

**Excluded (production-only):** capacity and autoscaling effects, hot tablets,
memory layers, Locations service, CMEK key custody.

## Phase 8 — Backup snapshots

**Scope:** immutable schema and row snapshots; expiry, type and quota rules;
copy; restore independent of the live table; filtered and ordered listing;
expiry purge.

**Files:** `bttest/localcloud_backups.go`, `bttest/sql_schema.go`
(`backup_manifests_t`), `bttest/storage.go` (`copyTo`).

**Acceptance criteria:**

- Restore after the source table is modified or deleted returns the snapshot
  rows and schema.
- `expire_time` 6 h–90 d (365 d Enterprise Plus); `hot_to_standard_time` ≥ 24 h
  and only for `HOT`; `HOT` rejected on HDD clusters; 150 standard / 10 hot per
  table per cluster.
- Copy: source `READY`, no copy of a copy, `STANDARD`, expiry ≤ 30 d after
  the copy request.
- Restore: new table only; no inherited GC/automated backup/deletion
  protection; `RestoreInfo`; `OptimizeRestoredTable` LRO.
- `ListBackups` filters, `order_by` (default `start_time desc`), pagination;
  expired backups hidden and purged.
- Cluster/instance deletion blocked while backups exist.

**Status:** Implemented — test-verified (owning tests per capability in the audit).

**Excluded (production-only):** cross-region placement and durability,
CMEK-protected backups, incremental storage accounting.

## Phase 9 — Change streams

**Scope:** transactional outbox; opt-in per table; retention; grouped records;
GC and `DropRowRange` records; routing check; tokens, heartbeats, end time.

**Files:** `bttest/localcloud_change_stream.go`, `bttest/data_write.go`
(`writeCommits`), `bttest/table_admin.go`, `bttest/sql_schema.go`
(`change_stream_t`; `change_log_t` dropped).

**Acceptance criteria:**

- No records without `change_stream_config`; reading such a table →
  `FailedPrecondition`; disabling purges records.
- One record per row commit with grouped mutations; GC records (including GC
  caused by `ReadModifyWriteRow`) typed `GARBAGE_COLLECTION`; `DeleteFromRow` → per-family `DeleteFromFamily`.
- Multi-cluster app profile → `FailedPrecondition`.
- `start_time` in the future or outside retention → `InvalidArgument`; no start
  means from now; continuation tokens resume exactly.
- Heartbeats (default 5 s) carry a token and low watermark; `end_time` closes
  with OK.

**Status:** Implemented — test-verified (owning tests per capability in the audit).

**Remaining local gaps:** single partition (no split/merge); the low watermark
can precede an in-flight commit; `start_time` is not checked against the time
the stream was enabled.

**Excluded (production-only):** multi-cluster ordering metadata; Dataflow
connector.

## Phase 10 — Schema bundles

**Scope:** schema-bundle CRUD with descriptor validation and compatibility
check.

**Files:** `bttest/localcloud_schema_bundles.go`, `bttest/sql_schema.go`
(`schema_bundles_t`).

**Acceptance criteria:**

- Valid `FileDescriptorSet` / Avro JSON required; oneof immutable.
- 10 bundles per table → `ResourceExhausted`; 4 MB limit.
- Removing a message type → `FailedPrecondition` unless `ignore_warnings`.
- Etags, pagination, LRO metadata.

**Status:** Implemented — test-verified (owning tests per capability in the audit).

**Excluded (production-only):** none.

## Phase 11 — Session protocol

**Scope:** `GetClientConfiguration`, `OpenTable`, `OpenAuthorizedView`,
`OpenMaterializedView` with virtual ReadRow/MutateRow.

**Files:** `bttest/session_streaming.go`.

**Acceptance criteria:**

- `GetClientConfiguration` returns `session_load` 0 and stops polling.
- Opening a missing target fails with `NotFound`.
- Virtual RPCs share the unary pipeline: authorized-view restrictions apply;
  materialized views are read-only.
- A failing virtual RPC returns `SessionResponse.error` and the stream stays open.

**Status:** Implemented — test-verified (owning tests per capability in the audit).

**Excluded (production-only):** server-side session load balancing.

## Phase 12 — GoogleSQL engine, logical views, continuous materialized views

**Scope:** GoogleSQL engine (parser, type checker, evaluator, aggregates,
HLL++, key and cell encodings); `PrepareQuery`/`ExecuteQuery`; logical-view
validation and execution; CMVs defined by GoogleSQL and replacing the old
shadow-table implementation.

**Files:** `bttest/internal/gsql` (engine), `bttest/query_service.go`,
`bttest/materialized_views.go`,
`bttest/localcloud_logical_views.go`, `bttest/sql_materialized_views.go`.
Removed: `bttest/cmv.go`, `bttest/cmv_test.go`, `bttest/sql_parse.go`,
`bttest/sql_parse_test.go`. No build tag: the engine is always compiled.

**Acceptance criteria:**

- `PrepareQuery` token valid 1 hour; `ExecuteQuery` with prepared or deprecated
  query; `PREPARED_QUERY_EXPIRED` with `PreconditionFailure` after a schema
  change; `ProtoRows` batches with CRC32C checksums and resume tokens.
- Logical views: query validated on create/update; `SELECT … FROM <view>`
  executes; parent-scoped list; etags; deletion protection.
- CMVs: `GROUP BY` or `ORDER BY` definitions; 50 per instance, 5 per table;
  query immutable; deletion protection; reads via `materialized_view_name`,
  sessions and SQL; writes rejected; `_key`/struct key and `_timestamp` rules
  per [`CMV_SUPPORT.md`](../../../CMV_SUPPORT.md); legacy shadow tables removed
  on startup; a CMV blocks deletion of its source table.
- HLL++ `AddToCell`/`MergeToCell` work.

**Status:** Implemented — test-verified (`query_conformance_test.go`,
`internal/gsql` suite).

**Remaining local gaps:** parameterized views (`view_parameters`); HLL++ sketch
bytes are not ZetaSketch/BigQuery compatible; CMV multi-column key encoding
(OrderedCodeBytes) is an emulator choice.

**Excluded (production-only):** Data Boost compute; CMV eventual-consistency
lag and `user_errors` metrics.

## Phase 13 — LocalCloud integration and multi-platform image

**Scope:** publish this iteration to LocalCloud and build the emulator image
for the supported platforms.

**Files:** `little_bigtable.go` (`version`), `Dockerfile`,
`Makefile.localcloud`, LocalCloud `Dockerfile` (`LITTLE_BIGTABLE_VERSION`).

**Observed state at `dd5f9e7`:** `version` is `0.5.0-localcloud`; the
repository `Dockerfile` cross-compiles with `CGO_ENABLED=0` from
`--platform=$BUILDPLATFORM`; `make -f Makefile.localcloud docker-buildx`
builds a multi-platform image (an OCI archive unless `PUSH=true`); LocalCloud
pins `LITTLE_BIGTABLE_VERSION=dd5f9e7…`.

**Acceptance criteria:**

- Version bumped and tag `v0.5.0` created — done.
- LocalCloud pin updated — done.
- Image builds for `linux/amd64` and `linux/arm64`; the packaged binary
  reports `0.5.0-localcloud` — pending.
- LocalCloud Bigtable scenarios pass against the rebuilt image through the
  supported client and transport — pending.
- Databases from v0.4.x start, migrate (`tables_t`, `change_log_t`, legacy CMV
  shadow tables) and serve in the image — pending (metadata migration is
  covered by `TestConformanceTableAdminLegacyMetadataMigration`).

**Status:** Pin updated; image build and platform tests pending.

**Excluded (production-only):** none.

## Production-only exclusions (whole iteration)

Replication, failover and multi-cluster consistency; autoscaling and node
capacity effects; tablet balancing and hot tablets; memory layers; real
location inventory (Locations service not registered); Data Boost compute;
CMEK key custody; Cloud Monitoring, Key Visualizer and client metrics;
production latency; Dataflow, BigQuery and Pub/Sub connectors.

## Iteration exit criteria

1. Done: GoogleSQL engine always built; executable ledger matches the audit.
2. Done: Phases 1–12 acceptance criteria map to named passing tests (see the
   audit's owning-test columns).
3. Done: `./test.sh` passes; PostgreSQL storage conformance runs in CI.
4. Open: Phase 13 image build and a LocalCloud run against the rebuilt image.

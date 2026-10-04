# Bigtable Emulator Compatibility Audit

**Audit date:** 2026-10-04 (supersedes the 2026-08-29 audit)
**Audited revision:** commit `dd5f9e7`, tag `v0.5.0`, branch `jay-bigtable-extended`
**Upstream baseline:** Bigtable v2 Data API (`google.bigtable.v2.Bigtable`) and Admin API (`google.bigtable.admin.v2.BigtableTableAdmin`, `google.bigtable.admin.v2.BigtableInstanceAdmin`) as generated in `cloud.google.com/go/bigtable` **v1.58.0**; googleapis `master` protos and Bigtable product documentation fetched 2026-10-04.
**Implementation plan:** [`docs/superpowers/plans/2026-10-04-bigtable-parity-implementation-plan.md`](docs/superpowers/plans/2026-10-04-bigtable-parity-implementation-plan.md)
**Scope rule:** emulate every observable behavior that does not require production hardware or infrastructure. Exclude only the production guarantee itself, and record why.

## Runtime and configuration identity

| Item | Value |
| --- | --- |
| Repository | `github.com/jhsenjaliya/little_bigtable` at `dd5f9e7` (`v0.5.0`) |
| Binary version string | `0.5.0-localcloud` (`little_bigtable.go`) |
| Generated API | `cloud.google.com/go/bigtable v1.58.0` (from v1.51.0), `cloud.google.com/go/longrunning v1.2.0`, `cloud.google.com/go/iam v1.12.0`, `google.golang.org/grpc v1.83.2` |
| Toolchain | Go 1.27.0 |
| SQL drivers | `github.com/glebarez/go-sqlite` (pure Go, registered as `sqlite3`) and `github.com/lib/pq`; no CGO |
| GoogleSQL engine | `bttest/internal/gsql`, always built |
| Container image | Repository `Dockerfile` cross-compiles with `CGO_ENABLED=0` from `--platform=$BUILDPLATFORM`; `make -f Makefile.localcloud docker-buildx` builds a multi-platform image (OCI archive unless `PUSH=true`) |
| LocalCloud | LocalCloud `Dockerfile` pins `LITTLE_BIGTABLE_VERSION=dd5f9e7…`; image build and platform tests pending |
| Registered services (`bttest/inmem.go: NewServer`) | `google.bigtable.v2.Bigtable`, `BigtableTableAdmin`, `BigtableInstanceAdmin`, `google.longrunning.Operations`. `google.cloud.location.Locations` is not registered. |

## Evidence summary

- `./test.sh` (`go test`, `-race`, `go vet`, `gofmt`) and `go mod verify` passed at `dd5f9e7`.
- 245 top-level tests (963 including subtests) in `bttest`, plus the `bttest/internal/gsql` package suite.
- `TestCBTClientConformance` skips when the pinned `cbt` binary is not installed (CI installs it).
- `TestStorageConformance` ran on SQLite in this pass; the PostgreSQL lane runs in CI.
- The executable ledger (`bttest/compatibility.go: buildCompatibilityLedger`, exposed as `bttest.CompatibilityLedger()`) has an entry for each of the 89 registered RPCs and for every field of the pinned high-risk messages. Every entry is `test_verified` with a named `OwningTest`; none is `known_nonconformant` or `declared_unverified`. `TestCompatibilityLedgerCoversRegisteredRPCs` and `TestCompatibilityLedgerFieldsAreExhaustive` (`compatibility_test.go`) fail on drift.

## Method and status vocabulary

The parity method is the LocalCloud parity audit: each capability records an
ID, upstream reference, expected and observed behavior, implementation owner,
local or production scope, finding and implementation status, verification
tier, owning test, runtime identity (above) and next action.

**Support level** (ledger values): `supported`; `locally_simplified` (local
contract with a documented single-process simplification); `explicitly_unsupported`
(documented error); `not_applicable` (production hardware or infrastructure;
returns `Unimplemented` with a reason).

**Finding labels:** Implementation gap; Behavioral defect;
Integration/configuration failure; Documentation error; Unverified;
Production-only guarantee.

**Verification tier:** **TB** — a named behavioral test owns the capability and
passed at `dd5f9e7`. **SI** — source-inspected only.

Test files are cited by short name: `write` = `data_write_conformance_test.go`,
`read` = `data_read_conformance_test.go`, `filter`, `targets`, `authz` =
`authorized_view_conformance_test.go`, `tadmin` = `table_admin_conformance_test.go`,
`iadmin` = `instance_admin_conformance_test.go`, `ops` =
`operations_conformance_test.go`, `iam`, `backup`, `cs` =
`change_stream_conformance_test.go`, `sb` = `schema_bundle_conformance_test.go`,
`session`, `query` (each `<name>_conformance_test.go` in `bttest/`).

## Prior findings of this audit (resolved)

| ID | Finding at first pass | Resolution at `dd5f9e7` |
| --- | --- | --- |
| F-1 | GoogleSQL built only under a build tag with a stub | No build tag or stub; `internal/gsql`, `query_service.go` and `materialized_views.go` are always built |
| F-2 | Executable ledger stale | Ledger rewritten: 89 RPC entries plus pinned fields, all `test_verified` |
| F-3 | Version and LocalCloud pin pre-iteration | Version `0.5.0-localcloud`; LocalCloud pins `dd5f9e7` |

## Executive parity matrix

| ID | Area | Support | Local contract (summary) | Production-only exclusions | Remaining local gaps | Verification / owning test |
| --- | --- | --- | --- | --- | --- | --- |
| BT-WRITE | Data-plane writes | supported | Validate-then-apply single-row atomicity; per-entry `MutateRows` codes; idempotency tokens; aggregate families | Cross-cluster write ordering | — | TB: `TestConformanceWriteMutateRowAtomicity` (`write`) |
| BT-READ | Data-plane reads | supported | Target resolution, production chunking, request stats, `SampleRowKeys` ranges and sampling | Production latency; tablet-based sampling | `last_scanned_row_key` not emitted | TB: `TestConformanceReadChunkShape` (`read`) |
| BT-FILTER | Row filters | supported | Up-front validation; all variants incl. `Interleave` duplicates, `Sink`, `ValueBitmask` | — | — | TB: `TestConformanceFilterInterleaveDuplicatesAndLimits` (`filter`) |
| BT-GC | Garbage collection | locally_simplified | Correct nested union/intersection; read-time GC on copies; write-time GC with change records | Asynchronous GC timing | — | TB: `TestConformanceGCRuleTruthTables` (`filter`) |
| BT-TARGET | Targets, app profiles, authorized views | supported | One target; app-profile routing rules; authorized-view enforcement | Data Boost compute; multi-cluster routing | — | TB: `TestConformanceTargetAppProfileResolution` (`targets`) |
| BT-TADMIN | Table administration | supported | Validated create, views, masks, soft delete/undelete, atomic family changes, splits, row key schema, consistency tokens | Replication catch-up | Deprecated snapshot RPCs rejected | TB: `TestConformanceTableAdminCreateTableValidation` (`tadmin`) |
| BT-AGG | Aggregate families | supported | Sum/Min/Max Int64; HLL++ | — | HLL++ bytes are emulator-local | TB: `TestConformanceWriteAggregateSemantics` (`write`) |
| BT-IADMIN | Instance administration | locally_simplified | Validated instance/cluster/app-profile metadata, masks, pagination, guarded deletion | Capacity, autoscaling, hot tablets, memory layers, locations | Empty display name accepted; `DeleteCluster` ignores automated-backup locations | TB: `TestConformanceInstanceAdminCreateRequirements` (`iadmin`) |
| BT-LRO | Long-running operations | locally_simplified | Durable registry; Get/List/Wait/Delete/Cancel | Real asynchronous progress | — | TB: `TestConformanceOperationsDurableAcrossRestart` (`ops`) |
| BT-IAM | IAM policies | locally_simplified | Persistent policies, etags, update masks, `NotFound` for missing resources | — | Not enforced (unauthenticated emulator) | TB: `TestConformanceIAMPersistsAcrossRestart` (`iam`) |
| BT-BACKUP | Backups | supported | Immutable snapshots; expiry, type and quota rules; copy; independent restore | Cross-region placement, CMEK | — | TB: `TestConformanceBackupRestoreAfterSourceDeletedAndRestart` (`backup`) |
| BT-CS | Change streams | locally_simplified | Opt-in; retention; grouped records; GC and `DropRowRange` records; tokens, heartbeats, end time | Multi-cluster ordering | Single partition; watermark and start-time caveats | TB: `TestConformanceChangeStreamMutateRowIsOneDataChange` (`cs`) |
| BT-SB | Schema bundles | supported | CRUD, validation, compatibility check, limits, etags | — | — | TB: `TestConformanceSchemaBundleProtoCRUD` (`sb`) |
| BT-SESSION | Session protocol | supported | `GetClientConfiguration`; Open* with virtual ReadRow/MutateRow | Server-side session load balancing | — | TB: `TestConformanceSessionPermissionsAndErrors` (`session`) |
| BT-SQL | GoogleSQL query | supported | `PrepareQuery`/`ExecuteQuery`, typed `ProtoRows`, checksums, resume tokens | Data Boost compute | — | TB: `TestConformanceSQLQueryThroughClient` (`query`) |
| BT-LV | Logical views | supported | CRUD with validation; executable through SQL | — | Parameterized views | TB: `TestLogicalViews_CRUD` (`localcloud_new_features_test.go`), `TestConformanceLogicalViewQuery` (`query`) |
| BT-CMV | Continuous materialized views | supported | GoogleSQL definitions; read-only reads by `materialized_view_name`, sessions and SQL | Eventual-consistency lag | Multi-column key encoding is an emulator choice | TB: `TestConformanceMaterializedViewReadsAndRefresh` (`query`) |
| BT-REPL | Replication and consistency | locally_simplified | Single process; consistency tokens always consistent | Replication, failover, multi-cluster consistency | — | TB: `TestConformanceTableAdminConsistencyTokens` (`tadmin`) |
| BT-OBS | Observability | supported (request stats only) | ReadRows request stats | Monitoring, Key Visualizer, client metrics, hot tablets | — | TB: `TestConformanceReadRequestStatsFull` (`read`) |
| BT-PERSIST | SQL persistence (extension) | supported | SQLite/PostgreSQL storage for all state; one-way migration on first start | — | — | TB: `TestStorageConformance` (`storage_conformance_test.go`), `TestConformanceTableAdminLegacyMetadataMigration` (`tadmin`) |
| BT-CLIENT | Client/CLI compatibility | supported (Go, `cbt` core) | Official Go client; pinned `cbt` smoke | — | Other languages untested | TB: `TestGoogleDocsHelloWorldGoClientWorkflow` (`google_docs_hello_test.go`); `TestCBTClientConformance` (`client_conformance_test.go`, skips without `cbt`) |

## 1. Data plane

Upstream: `google/bigtable/v2/bigtable.proto`, `data.proto`; [Data API][data-rpc], [writes][writes], [reads][reads].

| ID | Capability | Observed local contract | Owner | Verification / owning test |
| --- | --- | --- | --- | --- |
| BT-WRITE-1 | `MutateRow` atomicity | All mutations validated before any is applied; row cloned, mutated, GC'd and persisted once with its change record in one transaction. | `mutation_engine.go: validateMutations`, `stage`, `persist`; `data_write.go: MutateRow` | TB: `TestConformanceWriteMutateRowAtomicity` (`write`) |
| BT-WRITE-2 | `MutateRows` | One transaction per batch; each entry atomic with its own status (`NotFound` unknown family, `InvalidArgument` invalid timestamp); duplicate keys applied in order. | `data_write.go: MutateRows` | TB: `TestConformanceWriteMutateRowsPerEntryStatus`, `TestConformanceWriteMutateRowsDuplicateKeysInOrder` (`write`) |
| BT-WRITE-3 | Errors | Unknown family → `NotFound` "Requested column family not found"; storage errors map to `DeadlineExceeded`/`Canceled`/`Internal`; request validation. | `mutation_engine.go: unknownFamilyError`; `storage.go: storageErr` | TB: `TestConformanceWriteStorageErrorMapping`, `TestConformanceWriteRequestValidation` (`write`) |
| BT-WRITE-4 | `CheckAndMutateRow` | Predicate on the view-restricted row; only the chosen branch validated and applied; single-cluster transactional routing required. | `data_write.go: CheckAndMutateRow`; `targets.go` | TB: `TestConformanceWriteCheckAndMutateRowAtomicity` (`write`), `TestConformanceAuthorizedViewCheckAndMutatePredicate` (`authz`) |
| BT-WRITE-5 | `ReadModifyWriteRow` | Rules validated first; non-8-byte increment → `FailedPrecondition`; aggregate family → `InvalidArgument`; GC deletions it causes are recorded in change streams. | `data_write.go: ReadModifyWriteRow` | TB: `TestConformanceWriteReadModifyWriteRowAtomicity` (`write`) |
| BT-WRITE-6 | Idempotency, granularity | Tokens ≥ 8 bytes; 15-minute window; stale `start_time` → `FailedPrecondition`; a token repeated within one `MutateRows` batch is applied once. `MILLIS`/`MICROS` granularity; `CLIENT_AUTO_GENERATED` truncation. | `data_write.go: validateIdempotency`; `mutation_engine.go: tableGranularity` | TB: `TestConformanceWriteIdempotency` (`write`) |
| BT-READ-1 | `ReadRows` validation | Exactly one target; `rows_limit < 0` and Data Boost + `reversed` → `InvalidArgument`; filters validated first; mixed keys and ranges; reversed scans with limits. | `data_read.go: ReadRows`; `targets.go: resolveReadTarget` | TB: `TestConformanceReadNegativeRowsLimit`, `TestConformanceReadMixedKeysAndRanges`, `TestConformanceReadReversedWithLimit` (`read`) |
| BT-READ-2 | Chunking | Production chunk shape; large values split with `value_size` and reassembled by the official client; responses flushed near 1 MiB. | `data_read.go: newRowWriter`, `writeRow`, `flush` | TB: `TestConformanceReadChunkShape`, `TestConformanceReadLargeValueSplit`, `TestConformanceReadLargeValueClientReassembly` (`read`) |
| BT-READ-3 | Request stats | `REQUEST_STATS_FULL` adds a final stats-only response. | `data_read.go: ReadRows` | TB: `TestConformanceReadRequestStatsFull`, `TestConformanceReadRequestStatsClient` (`read`) |
| BT-READ-4 | `last_scanned_row_key` | Not emitted (optional upstream). | `data_read.go` | SI — Implementation gap (low impact) |
| BT-READ-5 | `SampleRowKeys` | Honors `row_range`; includes initial splits; 512 KiB sampling; final empty end-of-table key or range end with total offset. | `data_read.go: SampleRowKeys` | TB: `TestConformanceSampleRowKeysEndOfTable`, `TestConformanceSampleRowKeysInitialSplits`, `TestConformanceSampleRowKeysOffsets`, `TestConformanceSampleRowKeysRowRange` (`read`) |
| BT-READ-6 | `PingAndWarm` | Validates instance name and app profile. | `localcloud_change_stream.go: PingAndWarm` | TB: `TestConformanceTargetAppProfileResolution` (`targets`) |

### Filters and garbage collection

Upstream: `RowFilter` in `data.proto`; [filters][filters]; [garbage collection][garbage-collection].

| ID | Observed local contract | Owner | Verification / owning test |
| --- | --- | --- | --- |
| BT-FILTER-1 | Validation before reading: `Chain`/`Interleave` ≥ 2 filters; labels `^[a-z0-9-]{1,15}$`, one label transformer per `Chain`; no `Sink` in `Condition`; bitmask required; no negative limits/offsets; depth ≤ 20. Filters never mutate stored rows. | `filter.go: validateFilter` | TB: `TestConformanceFilterValidationBeforeRead`, `TestConformanceFilterLabelValidation`, `TestConformanceFilterSinkInsideConditionRejected`, `TestConformanceFilterNeverMutatesStoredRows` (`filter`) |
| BT-FILTER-2 | `Interleave` keeps duplicates, counted by later limits. | `filter.go: evalFilter`, `mergeCells` | TB: `TestConformanceFilterInterleaveDuplicatesAndLimits`, `TestConformanceFilterInterleaveDocExample` (`filter`) |
| BT-FILTER-3 | `Sink` outputs to the final result only. | `filter.go` | TB: `TestConformanceFilterSinkDocExample` (`filter`) |
| BT-FILTER-4 | `ValueBitmask`: `(v & mask) == mask`, equal lengths. | `filter.go: includeCell` | TB: `TestConformanceFilterValueBitmask` (`filter`) |
| BT-GC-1 | Union deletes if any child deletes; intersection only if all delete; nested rules; validation; GC on write is persisted, GC on read is not. | `inmem.go: applyGC`, `gcDeletes`, `gcWithChanges` | TB: `TestConformanceGCRuleTruthTables`, `TestConformanceGCRulesEndToEnd`, `TestConformanceGCPersistedOnWrite`, `TestConformanceGCRuleValidation` (`filter`) |

### Targets, app profiles and authorized views

Upstream: target and `app_profile_id` fields in `bigtable.proto`; [routing][routing]; [authorized views][authorized-views].

| ID | Observed local contract | Owner | Verification / owning test |
| --- | --- | --- | --- |
| BT-TARGET-1 | Exactly one target per request. | `targets.go: singleTarget` | TB: `TestConformanceTargetExactlyOneTarget` (`targets`) |
| BT-TARGET-2 | Unregistered instance accepts any profile; registered instance: `""` → `default`, unknown or deleted → `NotFound`; Data Boost writes → `FailedPrecondition`; `CheckAndMutateRow`/RMW require single-cluster transactional routing. | `targets.go: resolveAppProfile` | TB: `TestConformanceTargetAppProfileResolution`, `TestConformanceTargetDeletedAppProfile`, `TestConformanceTargetUnregisteredInstanceLeniency`, `TestConformanceTargetGoClientAppProfiles` (`targets`) |
| BT-TARGET-3 | Authorized-view reads restricted to `row_prefixes` and `family_subsets`. | `targets.go: compileViewPolicy`, `restrict` | TB: `TestConformanceAuthorizedViewReadRestriction` (`authz`) |
| BT-TARGET-4 | Writes outside the view → `PermissionDenied`; `DeleteFromRow` via a view → `PermissionDenied`; `DeleteFromFamily` needs qualifier prefix `""`; RMW restricted. | `targets.go: checkMutations`, `checkRules` | TB: `TestConformanceAuthorizedViewWriteRestriction`, `TestConformanceAuthorizedViewReadModifyWrite` (`authz`) |
| BT-TARGET-5 | Authorized-view CRUD: views `NAME_ONLY`/`BASIC`/`FULL`, pagination, etags (`ABORTED`), masks, deletion protection. | `localcloud_authorized_views.go` | TB: `TestConformanceAuthorizedViewResponseViewsAndPagination`, `TestConformanceAuthorizedViewEtagAndUpdateMask`, `TestConformanceAuthorizedViewDeletionProtection` (`authz`) |

**Production-only exclusions:** production latency and throughput, tablet sampling, Data Boost compute isolation, multi-cluster routing and row affinity.

## 2. Table administration and schema bundles

Upstream: `bigtable_table_admin.proto`, `table.proto`, `types.proto`; [Admin API][admin-rpc]; [managing tables][managing-tables].

| ID | Observed local contract | Owner | Verification / owning test |
| --- | --- | --- | --- |
| BT-TADMIN-1 | `CreateTable` validates IDs, GC rules, aggregate `value_type`, change stream retention 1–7 d, row key schema, non-empty initial splits (persisted). | `table_admin.go: CreateTable` | TB: `TestConformanceTableAdminCreateTableValidation`, `TestConformanceTableAdminInitialSplitsPersisted`, `TestConformanceTableAdminRowKeySchemaRules` (`tadmin`) |
| BT-TADMIN-2 | `GetTable` views (default `SCHEMA_VIEW`; replication/encryption views report `READY`, `GOOGLE_DEFAULT_ENCRYPTION`); `ListTables` default `NAME_ONLY`, paginated. | `table_admin.go: GetTable`, `ListTables` | TB: `TestConformanceTableAdminGetTableViews`, `TestConformanceTableAdminListTablesViewsAndPagination` (`tadmin`) |
| BT-TADMIN-3 | `UpdateTable` masks `change_stream_config`, `deletion_protection`, `row_key_schema`, `automated_backup_policy`, `tiered_storage_config`; `column_families` → `Unimplemented`; `*` → `InvalidArgument`; LRO; persisted across restart. | `table_admin.go: UpdateTable` | TB: `TestConformanceTableAdminUpdateTableMasks`, `TestConformanceTableAdminUpdateTablePersistsAcrossRestart` (`tadmin`) |
| BT-TADMIN-4 | `DeleteTable` blocked by table/view deletion protection or a CMV source; soft delete to `<id>@deleted@<micros>`; the table's authorized views and schema bundles move with the tombstone; IAM policies removed. `UndeleteTable` within 7 days restores the table, its authorized views and schema bundles, with deletion protection on; a re-created table with the same ID starts without them. | `table_admin.go: DeleteTable`, `UndeleteTable`, `moveTableChildren` | TB: `TestConformanceTableAdminDeleteUndelete`, `TestConformanceTableAdminDeletionProtectionBlocksDelete` (`tadmin`) |
| BT-TADMIN-5 | `ModifyColumnFamilies` atomic; per-modification `update_mask`; `value_type` immutable; dropped family data removed and not resurrected. | `table_admin.go: ModifyColumnFamilies` | TB: `TestConformanceTableAdminModifyColumnFamiliesAtomic`, `TestConformanceTableAdminValueTypeImmutable`, `TestConformanceTableAdminDroppedFamilyDataNotResurrected` (`tadmin`) |
| BT-TADMIN-6 | `DropRowRange` in one transaction with change records. | `table_admin.go: DropRowRange` | TB: `TestConformanceTableAdminDropRowRange` (`tadmin`) |
| BT-TADMIN-7 | Consistency tokens per table; always consistent. | `table_admin.go` | TB: `TestConformanceTableAdminConsistencyTokens` (`tadmin`) |
| BT-TADMIN-8 | Snapshot RPCs → `Unimplemented` (deprecated private alpha). | `table_admin.go` | TB: `TestConformanceTableAdminSnapshotsUnimplemented` (`tadmin`) — explicitly_unsupported |
| BT-TADMIN-9 | Metadata format `LBT\x02`; legacy rows migrated on load. | `sql_tables.go` | TB: `TestConformanceTableAdminLegacyMetadataMigration` (`tadmin`) |
| BT-AGG-1 | Sum/Min/Max Int64 and HLL++ families; `AddToCell`/`MergeToCell` require an aggregate family and type-checked input. HLL++ sketch bytes are emulator-local. | `mutation_engine.go`; `internal/gsql/hll.go` | TB: `TestConformanceWriteAggregateSemantics`, `TestConformanceWriteAggregateFamilyTypeChecks` (`write`); `TestHLLEncoding`, `TestHLLMerge` (`internal/gsql/hll_test.go`) |
| BT-SB-1 | Schema bundles: proto `FileDescriptorSet` and Avro JSON validated; 10 per table, 4 MB; oneof immutable; removing a message type → `FailedPrecondition` unless `ignore_warnings`; etags; pagination. | `localcloud_schema_bundles.go` | TB: `TestConformanceSchemaBundleProtoCRUD`, `TestConformanceSchemaBundleAvroCRUD`, `TestConformanceSchemaBundleInvalidSchemas`, `TestConformanceSchemaBundleLimitPerTable`, `TestConformanceSchemaBundleUpdateRules`, `TestConformanceSchemaBundlePagination` (`sb`) |

**Production-only exclusions:** replication catch-up, tiered-storage placement, production storage statistics.

## 3. Instance administration, LRO and IAM

Upstream: `bigtable_instance_admin.proto`, `instance.proto`, `google/longrunning/operations.proto`, `google/iam/v1`; [instances][instances]; [access control][iam].

| ID | Observed local contract | Owner | Verification / owning test |
| --- | --- | --- | --- |
| BT-IADMIN-1 | Instance ID 6–33; ≥ 1 cluster; edition default `ENTERPRISE`, type `PRODUCTION`; default app profile. An empty display name is accepted and defaults to the instance ID. | `localcloud_instance_admin.go` | TB: `TestConformanceInstanceAdminInstanceIDValidation`, `TestConformanceInstanceAdminCreateRequirements` (`iadmin`) |
| BT-IADMIN-2 | `UpdateInstance` scope; `PartialUpdateInstance` masks (`PRODUCTION`→`DEVELOPMENT` rejected); `DeleteInstance` guarded cascade. | `localcloud_instance_admin.go` | TB: `TestConformanceInstanceAdminUpdateInstanceScope`, `TestConformanceInstanceAdminPartialUpdateMasks`, `TestConformanceInstanceAdminOfficialClient` (`iadmin`) |
| BT-IADMIN-3 | Cluster ID rule; `serve_nodes`, autoscaling, `node_scaling_factor` updates; `DeleteCluster` blocked for the last cluster, backups, or app-profile routing (not by automated-backup locations). | `localcloud_instance_admin.go` | TB: `TestConformanceInstanceAdminClusterIDValidation`, `TestConformanceInstanceAdminClusterUpdates`, `TestConformanceInstanceAdminDeleteClusterRules` (`iadmin`) |
| BT-IADMIN-4 | App profiles: routing must reference existing clusters; etags; list pagination and `-` wildcard. | `localcloud_instance_admin.go` | TB: `TestConformanceInstanceAdminAppProfileRouting`, `TestConformanceInstanceAdminAppProfileEtag`, `TestConformanceInstanceAdminListPaginationAndWildcard` (`iadmin`) |
| BT-IADMIN-5 | `ListHotTablets`, `GetMemoryLayer`, `ListMemoryLayers`, `UpdateMemoryLayer` → `Unimplemented` (production hardware). | `localcloud_instance_admin.go` | TB: `TestConformanceInstanceAdminProductionHardwareUnimplemented` (`iadmin`) — not_applicable |
| BT-IADMIN-6 | `google.cloud.location.Locations` not registered. | `inmem.go: NewServer` | SI — Production-only guarantee |
| BT-LRO-1 | Durable `operations_t`; typed metadata; List prefix, `done` filter, pagination; Wait/Delete/Cancel; survives restart. | `operations.go` | TB: `TestConformanceOperationsMetadataTypes`, `TestConformanceOperationsListFilterPagination`, `TestConformanceOperationsWaitDeleteCancel`, `TestConformanceOperationsDurableAcrossRestart` (`ops`) |
| BT-IAM-1 | Persisted policies; etag → `ABORTED`; `update_mask`; missing resource → `NotFound`; removed with the resource. | `sql_iam.go` | TB: `TestConformanceIAMPersistsAcrossRestart`, `TestConformanceIAMEtagAborted`, `TestConformanceIAMUpdateMask`, `TestConformanceIAMMissingResourceNotFound`, `TestConformanceIAMPoliciesRemovedWithResource`, `TestConformanceIAMOfficialClientHandles` (`iam`) |
| BT-IAM-2 | No enforcement: `TestIamPermissions` grants every permission. | `sql_iam.go: testIamPermissions` | Implementation gap — needs authenticated identities; the emulator is unauthenticated by contract |

**Production-only exclusions:** capacity and autoscaling effects, hot tablets, memory layers, location inventory, CMEK key custody, transport authentication.

## 4. Backups

Upstream: backup RPCs in `bigtable_table_admin.proto`; [backups][backups].

| ID | Observed local contract | Owner | Verification / owning test |
| --- | --- | --- | --- |
| BT-BACKUP-1 | Immutable schema + row snapshot; `expire_time` 6 h–90 d (365 d Enterprise Plus); typed operation metadata. | `localcloud_backups.go: CreateBackup` | TB: `TestConformanceBackupExpireTimeAndEdition`, `TestConformanceBackupOperationMetadata` (`backup`) |
| BT-BACKUP-2 | `HOT`/`STANDARD`: `hot_to_standard_time` ≥ 24 h, `HOT` only; HOT rejected on HDD clusters; 150 standard / 10 hot per table per cluster. | `localcloud_backups.go` | TB: `TestConformanceBackupHotBackupRules` (`backup`) |
| BT-BACKUP-3 | `CopyBackup`: source `READY`; no copy of a copy; result `STANDARD`; expiry at most 30 days after the copy request. | `localcloud_backups.go: CopyBackup` | TB: `TestConformanceBackupCopyRules` (`backup`) |
| BT-BACKUP-4 | `RestoreTable` from the snapshot, independent of the source (also after source deletion and restart); no inherited policies; `RestoreInfo`; optimize LRO. | `localcloud_backups.go: RestoreTable` | TB: `TestConformanceBackupRestoreAfterSourceDeletedAndRestart` (`backup`) |
| BT-BACKUP-5 | `ListBackups` filters, `order_by` (default `start_time desc`), pagination, `-` cluster; expired backups hidden and purged. | `localcloud_backups.go: ListBackups` | TB: `TestConformanceBackupListFilterOrderPagination`, `TestConformanceBackupExpiredHiddenAndPurged` (`backup`) |
| BT-BACKUP-6 | Cluster and instance deletion blocked while backups exist. | `localcloud_backups.go: backupsUnder` | TB: `TestConformanceBackupBlocksClusterAndInstanceDeletion` (`backup`) |

**Production-only exclusions:** cross-region placement and durability, CMEK-protected backups, incremental storage accounting. Automated backup policies are stored and validated; no scheduler creates backups (SI).

## 5. Change streams

Upstream: `GenerateInitialChangeStreamPartitions`, `ReadChangeStream`; [change streams][change-streams].

| ID | Observed local contract | Owner | Verification / owning test |
| --- | --- | --- | --- |
| BT-CS-1 | Records only while `change_stream_config` is set (enable via `UpdateTable`); disabled table → `FailedPrecondition`; disabling purges; retention purge. | `localcloud_change_stream.go`; `inmem.go: maintenanceLoop` | TB: `TestConformanceChangeStreamDisabledTableAndEnableViaUpdateTable`, `TestConformanceChangeStreamDisablePurgesRecords`, `TestConformanceChangeStreamRetentionPurge` (`cs`) |
| BT-CS-2 | One grouped record per row commit for every write RPC; `DeleteFromRow` → per-family `DeleteFromFamily`; GC (including RMW-triggered) and `DropRowRange` records. | `localcloud_change_stream.go`; `data_write.go: writeCommits` | TB: `TestConformanceChangeStreamMutateRowIsOneDataChange`, `TestConformanceChangeStreamOtherWriteRPCsRecordOneChangePerRow`, `TestConformanceChangeStreamDeleteFromRowIsDeleteFromFamilyPerFamily`, `TestConformanceChangeStreamGarbageCollectionRecords`, `TestConformanceChangeStreamDropRowRangeRecords` (`cs`) |
| BT-CS-3 | Tokens resume exactly; `start_time` ≤ now and within retention; heartbeats with token and low watermark; `end_time` → OK close; single-cluster routing; one full-keyspace partition. | `localcloud_change_stream.go: ReadChangeStream` | TB: `TestConformanceChangeStreamResumeWithContinuationToken`, `TestConformanceChangeStreamStartTimeValidation`, `TestConformanceChangeStreamHeartbeats`, `TestConformanceChangeStreamEndTimeClosesStreamOK`, `TestConformanceChangeStreamAppProfileRouting`, `TestConformanceChangeStreamPartitions` (`cs`) |

**Remaining local gaps:** single partition (no split/merge); the low watermark can precede an in-flight commit; `start_time` is not checked against the time the stream was enabled.
**Production-only exclusions:** multi-cluster ordering metadata; Dataflow connector.

## 6. Session protocol

| ID | Observed local contract | Owner | Verification / owning test |
| --- | --- | --- | --- |
| BT-SESSION-1 | `GetClientConfiguration` (`session_load` 0, stop polling); Open* validate the target and READ/WRITE permissions; virtual ReadRow/MutateRow share the unary pipeline (authorized-view restrictions; materialized views read-only); per-request errors in `SessionResponse.error`, stream stays open. | `session_streaming.go` | TB: `TestConformanceSessionGetClientConfiguration`, `TestConformanceSessionOpenValidatesTarget`, `TestConformanceSessionPermissionsAndErrors`, `TestConformanceSessionAuthorizedViewRestrictions` (`session`); `TestConformanceMaterializedViewSession` (`query`) |

## 7. GoogleSQL, logical views and continuous materialized views

Upstream: `PrepareQuery`/`ExecuteQuery`; logical and materialized view RPCs; [GoogleSQL][sql]; [tables and views][tables-views]; [continuous materialized views][cmv].

| ID | Observed local contract | Owner | Verification / owning test |
| --- | --- | --- | --- |
| BT-SQL-1 | `PrepareQuery` token valid 1 hour; `ExecuteQuery` (prepared or deprecated query with params); `PREPARED_QUERY_EXPIRED` after schema change; `ProtoRows` batches with CRC32C and resume tokens. | `query_service.go`; `internal/gsql` | TB: `TestConformanceSQLQueryThroughClient`, `TestConformanceSQLPreparedQueryRefreshesAfterSchemaChange` (`query`) |
| BT-LV-1 | Logical views: CRUD with GoogleSQL validation, etags, deletion protection; queryable with `ExecuteQuery`. | `localcloud_logical_views.go` | TB: `TestLogicalViews_CRUD` (`localcloud_new_features_test.go`), `TestConformanceLogicalViewQuery` (`query`) |
| BT-LV-2 | Parameterized views (`view_parameters`) not supported. | — | Implementation gap |
| BT-CMV-1 | CMVs from GoogleSQL `GROUP BY`/`ORDER BY`; limits 50/instance, 5/table; query immutable; deletion protection; survive restart. | `materialized_views.go`; `internal/gsql/mv.go` | TB: `TestConformanceMaterializedViewSurvivesRestart` (`query`); `TestMaterializedViewAggregations`, `TestMaterializedViewSecondaryIndex`, `TestMaterializedViewRejections` (`internal/gsql/mv_test.go`) |
| BT-CMV-2 | Storage `__mv__/<id>`; key `_key` or OrderedCodeBytes struct (emulator choice); family `default`; map columns as families; timestamp 0 or `_timestamp`; read-only reads via `materialized_view_name`, sessions and SQL; recomputed when stale. See [`CMV_SUPPORT.md`](CMV_SUPPORT.md). | `materialized_views.go`; `internal/gsql/keycodec.go` | TB: `TestConformanceMaterializedViewReadsAndRefresh` (`query`); `TestOrderedCodeStructExamples`, `TestOrderedCodeStructPreservesOrder` (`internal/gsql/keycodec_test.go`) |

**Production-only exclusions:** Data Boost compute; CMV eventual-consistency lag and `user_errors` metrics.
**Remaining local gaps:** parameterized views; CMV multi-column key encoding is not documented by production; `_timestamp` is truncated to milliseconds rather than treated as invalid when not a multiple of 1,000 (SI).

## 8. Replication, routing and observability

| ID | Contract | Status |
| --- | --- | --- |
| BT-REPL-1 | Replication, failover, multi-cluster consistency: single process; tokens always consistent. | Production-only guarantee |
| BT-OBS-1 | ReadRows request stats (BT-READ-3). | supported, TB |
| BT-OBS-2 | Cloud Monitoring, Key Visualizer, client-side metrics, hot tablets: not emulated. | Production-only guarantee |

## 9. Persistence (repository extension)

| SQL table | Contents |
| --- | --- |
| `rows_t` | Rows for tables, tombstones, backup snapshots and CMV storage |
| `tables_t` | Table metadata (`LBT\x02`; migrated on first start) |
| `instances_t`, `clusters_t`, `app_profiles_t` | Instance admin metadata |
| `authorized_views_t`, `logical_views_t`, `materialized_views_t`, `schema_bundles_t` | View and schema-bundle definitions |
| `backups_t`, `backup_manifests_t` | Backups and schema manifests |
| `change_stream_t` | Change-stream records (`change_log_t` is dropped) |
| `iam_policies_t`, `operations_t`, `idempotency_t` | IAM policies, LROs, idempotency tokens |

Owning tests: `TestStorageConformance` (`storage_conformance_test.go`; SQLite here, PostgreSQL in CI) and the restart tests cited above.

## Behavior changes in this release

| Area | Previous behavior | New behavior |
| --- | --- | --- |
| Mutations | Failed requests could partially apply; `MutateRows` reported `Internal` | No partial application; per-entry codes (`NotFound` unknown family, `InvalidArgument` invalid timestamp) |
| Unknown family | Varied | `NotFound` "Requested column family not found" |
| Aggregates | `AddToCell`/`MergeToCell` added int64 in any family | Require an aggregate family; Sum/Min/Max/HLL++ |
| Filters and GC | `Interleave` deduplicated; `Sink` partial; `ValueBitmask` unsupported; intersection over-deleted | `Interleave` keeps duplicates; `Sink`, `ValueBitmask` implemented; intersection deletes only when all children delete |
| Targets | View names and app profiles ignored | Enforced |
| Instances | Created without clusters, any ID | ≥ 1 cluster and 6–33 character ID required |
| IAM | In memory, any resource name | Persisted; resource must exist |
| `SampleRowKeys` | Random samples; `row_range` ignored | Split points and 512 KiB samples within `row_range`; empty end-of-table key |
| Backups | Metadata only | Data snapshots; `expire_time` validated |
| Table deletion | Hard delete | Soft delete with `UndeleteTable`; authorized views and schema bundles follow the tombstone |
| CMVs | Readable as a table named after the view | Read with `materialized_view_name` or SQL; legacy shadow tables removed on startup |
| Change streams | Logged every table in `change_log_t` | Only with `change_stream_config`; `change_log_t` dropped |
| GoogleSQL | `Unimplemented` | Implemented |
| Storage | — | One-way metadata migration on first start |

## Prior audit findings → resolution

From the 2026-08-29 "Prioritized remediation". All rows are resolved and test-backed unless noted.

| Prior item | Resolution | Owning tests |
| --- | --- | --- |
| P0-1 Single-row atomicity, per-entry codes | Resolved | `TestConformanceWriteMutateRowAtomicity`, `TestConformanceWriteMutateRowsPerEntryStatus` |
| P0-2 `Interleave`, `Sink`, `ValueBitmask`, GC intersection | Resolved | `TestConformanceFilterInterleaveDuplicatesAndLimits`, `TestConformanceFilterSinkDocExample`, `TestConformanceFilterValueBitmask`, `TestConformanceGCRuleTruthTables` |
| P0-3 Never ignore targets; read stats; `row_range` | Resolved | `TestConformanceTargetExactlyOneTarget`, `TestConformanceReadRequestStatsFull`, `TestConformanceSampleRowKeysRowRange` |
| P1-4 Change streams | Resolved except partition split/merge (local gap) | `TestConformanceChangeStreamMutateRowIsOneDataChange`, `TestConformanceChangeStreamRetentionPurge` |
| P1-5 Backups | Resolved | `TestConformanceBackupRestoreAfterSourceDeletedAndRestart`, `TestConformanceBackupCopyRules` |
| P1-6 Views | Resolved; parameterized views remain a gap | `TestConformanceAuthorizedViewReadRestriction`, `TestConformanceLogicalViewQuery`, `TestConformanceMaterializedViewReadsAndRefresh` |
| P1-7 Current Table Admin, splits, masks, schema bundles | Resolved | `TestConformanceTableAdminUpdateTableMasks`, `TestConformanceTableAdminInitialSplitsPersisted`, `TestConformanceSchemaBundleProtoCRUD` |
| P2-8 Advanced `cbt`, non-Go client, session protocol | Session protocol resolved; client breadth open | `TestConformanceSessionPermissionsAndErrors` |
| P2-9 LRO, pagination, etags | Resolved | `TestConformanceOperationsListFilterPagination`, `TestConformanceOperationsDurableAcrossRestart` |
| P2-10 GoogleSQL | Resolved | `TestConformanceSQLQueryThroughClient` |
| Deferred managed-service items | Production-only exclusions; hot tablets and memory layers return `Unimplemented`; Locations not registered | `TestConformanceInstanceAdminProductionHardwareUnimplemented` |

## Remaining local gaps

| Gap | Finding |
| --- | --- |
| IAM not enforced (unauthenticated emulator) | Implementation gap (design: needs identities) |
| HLL++ sketch bytes are emulator-local (not ZetaSketch/BigQuery compatible) | Implementation gap |
| CMV multi-column row-key encoding is an emulator choice | Unverified against production |
| Single change-stream partition; no split/merge | Implementation gap |
| Change-stream low watermark can precede an in-flight commit | Behavioral defect |
| `start_time` not checked against the stream enablement time | Behavioral defect |
| Empty instance `display_name` accepted (defaults to the ID) | Behavioral defect |
| `DeleteCluster` not blocked by automated-backup locations | Behavioral defect |
| `last_scanned_row_key` not emitted | Implementation gap (optional field) |
| Parameterized logical views | Implementation gap |
| Deprecated snapshot RPCs | explicitly_unsupported by design |
| Non-Go clients and advanced `cbt` commands | Unverified |

## Appendix A — RPC disposition

Support level and owning test from `bttest/compatibility.go`. Every listed entry is `test_verified` (TB).

### `google.bigtable.v2.Bigtable`

| RPC | Support | Owning test |
| --- | --- | --- |
| `ReadRows` | supported | `TestConformanceReadChunkShape` |
| `SampleRowKeys` | locally_simplified | `TestConformanceSampleRowKeysEndOfTable` |
| `MutateRow` | supported | `TestConformanceWriteMutateRowAtomicity` |
| `MutateRows` | supported | `TestConformanceWriteMutateRowsPerEntryStatus` |
| `CheckAndMutateRow` | supported | `TestConformanceWriteCheckAndMutateRowAtomicity` |
| `PingAndWarm` | locally_simplified | `TestConformanceTargetAppProfileResolution` |
| `ReadModifyWriteRow` | supported | `TestConformanceWriteReadModifyWriteRowAtomicity` |
| `GenerateInitialChangeStreamPartitions` | locally_simplified | `TestConformanceChangeStreamPartitions` |
| `ReadChangeStream` | locally_simplified | `TestConformanceChangeStreamMutateRowIsOneDataChange` |
| `PrepareQuery` | supported | `TestConformanceSQLQueryThroughClient` |
| `ExecuteQuery` | supported | `TestConformanceSQLQueryThroughClient` |
| `GetClientConfiguration` | locally_simplified | `TestConformanceSessionGetClientConfiguration` |
| `OpenTable` | supported | `TestConformanceSessionPermissionsAndErrors` |
| `OpenAuthorizedView` | supported | `TestConformanceSessionAuthorizedViewRestrictions` |
| `OpenMaterializedView` | supported | `TestConformanceMaterializedViewSession` |

### `google.bigtable.admin.v2.BigtableTableAdmin`

| RPC | Support | Owning test |
| --- | --- | --- |
| `CreateTable` | supported | `TestConformanceTableAdminCreateTableValidation` |
| `CreateTableFromSnapshot` | explicitly_unsupported | `TestConformanceTableAdminSnapshotsUnimplemented` |
| `ListTables` | supported | `TestConformanceTableAdminListTablesViewsAndPagination` |
| `GetTable` | supported | `TestConformanceTableAdminGetTableViews` |
| `UpdateTable` | supported | `TestConformanceTableAdminUpdateTableMasks` |
| `DeleteTable` | supported | `TestConformanceTableAdminDeleteUndelete` |
| `UndeleteTable` | supported | `TestConformanceTableAdminDeleteUndelete` |
| `CreateAuthorizedView` | supported | `TestConformanceAuthorizedViewResponseViewsAndPagination` |
| `ListAuthorizedViews` | supported | `TestConformanceAuthorizedViewResponseViewsAndPagination` |
| `GetAuthorizedView` | supported | `TestConformanceAuthorizedViewResponseViewsAndPagination` |
| `UpdateAuthorizedView` | supported | `TestConformanceAuthorizedViewEtagAndUpdateMask` |
| `DeleteAuthorizedView` | supported | `TestConformanceAuthorizedViewEtagAndUpdateMask` |
| `ModifyColumnFamilies` | supported | `TestConformanceTableAdminModifyColumnFamiliesAtomic` |
| `DropRowRange` | supported | `TestConformanceTableAdminDropRowRange` |
| `GenerateConsistencyToken` | locally_simplified | `TestConformanceTableAdminConsistencyTokens` |
| `CheckConsistency` | locally_simplified | `TestConformanceTableAdminConsistencyTokens` |
| `SnapshotTable` | explicitly_unsupported | `TestConformanceTableAdminSnapshotsUnimplemented` |
| `GetSnapshot` | explicitly_unsupported | `TestConformanceTableAdminSnapshotsUnimplemented` |
| `ListSnapshots` | explicitly_unsupported | `TestConformanceTableAdminSnapshotsUnimplemented` |
| `DeleteSnapshot` | explicitly_unsupported | `TestConformanceTableAdminSnapshotsUnimplemented` |
| `CreateBackup` | supported | `TestConformanceBackupExpireTimeAndEdition` |
| `GetBackup` | supported | `TestConformanceBackupExpiredHiddenAndPurged` |
| `UpdateBackup` | supported | `TestConformanceBackupHotBackupRules` |
| `DeleteBackup` | supported | `TestConformanceBackupCopyRules` |
| `ListBackups` | supported | `TestConformanceBackupListFilterOrderPagination` |
| `RestoreTable` | supported | `TestConformanceBackupRestoreAfterSourceDeletedAndRestart` |
| `CopyBackup` | supported | `TestConformanceBackupCopyRules` |
| `GetIamPolicy` | locally_simplified | `TestConformanceIAMOfficialClientHandles` |
| `SetIamPolicy` | locally_simplified | `TestConformanceIAMOfficialClientHandles` |
| `TestIamPermissions` | locally_simplified | `TestConformanceIAMOfficialClientHandles` |
| `CreateSchemaBundle` | supported | `TestConformanceSchemaBundleProtoCRUD` |
| `UpdateSchemaBundle` | supported | `TestConformanceSchemaBundleProtoCRUD` |
| `GetSchemaBundle` | supported | `TestConformanceSchemaBundleProtoCRUD` |
| `ListSchemaBundles` | supported | `TestConformanceSchemaBundlePagination` |
| `DeleteSchemaBundle` | supported | `TestConformanceSchemaBundleProtoCRUD` |

### `google.bigtable.admin.v2.BigtableInstanceAdmin`

| RPC | Support | Owning test |
| --- | --- | --- |
| `CreateInstance` | locally_simplified | `TestConformanceInstanceAdminCreateRequirements` |
| `GetInstance` | locally_simplified | `TestConformanceInstanceAdminCreateRequirements` |
| `ListInstances` | locally_simplified | `TestConformanceInstanceAdminListPaginationAndWildcard` |
| `UpdateInstance` | locally_simplified | `TestConformanceInstanceAdminUpdateInstanceScope` |
| `PartialUpdateInstance` | locally_simplified | `TestConformanceInstanceAdminPartialUpdateMasks` |
| `DeleteInstance` | locally_simplified | `TestConformanceInstanceAdminOfficialClient` |
| `CreateCluster` | locally_simplified | `TestConformanceInstanceAdminClusterIDValidation` |
| `GetCluster` | locally_simplified | `TestConformanceInstanceAdminClusterIDValidation` |
| `ListClusters` | locally_simplified | `TestConformanceInstanceAdminListPaginationAndWildcard` |
| `UpdateCluster` | locally_simplified | `TestConformanceInstanceAdminClusterUpdates` |
| `PartialUpdateCluster` | locally_simplified | `TestConformanceInstanceAdminClusterUpdates` |
| `DeleteCluster` | locally_simplified | `TestConformanceInstanceAdminDeleteClusterRules` |
| `UpdateMemoryLayer` | not_applicable | `TestConformanceInstanceAdminProductionHardwareUnimplemented` |
| `ListMemoryLayers` | not_applicable | `TestConformanceInstanceAdminProductionHardwareUnimplemented` |
| `GetMemoryLayer` | not_applicable | `TestConformanceInstanceAdminProductionHardwareUnimplemented` |
| `CreateAppProfile` | locally_simplified | `TestConformanceInstanceAdminAppProfileRouting` |
| `GetAppProfile` | locally_simplified | `TestConformanceInstanceAdminAppProfileEtag` |
| `ListAppProfiles` | locally_simplified | `TestConformanceInstanceAdminListPaginationAndWildcard` |
| `UpdateAppProfile` | locally_simplified | `TestConformanceInstanceAdminAppProfileEtag` |
| `DeleteAppProfile` | locally_simplified | `TestConformanceInstanceAdminAppProfileRouting` |
| `GetIamPolicy` | locally_simplified | `TestConformanceIAMOfficialClientHandles` |
| `SetIamPolicy` | locally_simplified | `TestConformanceIAMOfficialClientHandles` |
| `TestIamPermissions` | locally_simplified | `TestConformanceIAMOfficialClientHandles` |
| `ListHotTablets` | not_applicable | `TestConformanceInstanceAdminProductionHardwareUnimplemented` |
| `CreateLogicalView` | supported | `TestLogicalViews_CRUD` |
| `GetLogicalView` | supported | `TestLogicalViews_CRUD` |
| `ListLogicalViews` | supported | `TestLogicalViews_CRUD` |
| `UpdateLogicalView` | supported | `TestLogicalViews_CRUD` |
| `DeleteLogicalView` | supported | `TestLogicalViews_CRUD` |
| `CreateMaterializedView` | supported | `TestConformanceMaterializedViewSurvivesRestart` |
| `GetMaterializedView` | supported | `TestConformanceMaterializedViewSurvivesRestart` |
| `ListMaterializedViews` | supported | `TestConformanceMaterializedViewSurvivesRestart` |
| `UpdateMaterializedView` | supported | `TestConformanceMaterializedViewSurvivesRestart` |
| `DeleteMaterializedView` | supported | `TestConformanceMaterializedViewSurvivesRestart` |

### `google.longrunning.Operations`

| RPC | Support | Owning test |
| --- | --- | --- |
| `GetOperation` | locally_simplified | `TestConformanceOperationsMetadataTypes` |
| `ListOperations` | locally_simplified | `TestConformanceOperationsListFilterPagination` |
| `WaitOperation` | locally_simplified | `TestConformanceOperationsWaitDeleteCancel` |
| `DeleteOperation` | locally_simplified | `TestConformanceOperationsWaitDeleteCancel` |
| `CancelOperation` | locally_simplified | `TestConformanceOperationsWaitDeleteCancel` |

### `google.cloud.location.Locations`

| RPC | Disposition |
| --- | --- |
| `ListLocations` | Not registered — Production-only (no ledger entry; not a registered RPC) |
| `GetLocation` | Not registered — Production-only (no ledger entry; not a registered RPC) |

`grpc.lookup.v1.RouteLookupService.RouteLookup` is also not registered.

## Official sources

Interface: googleapis `google/bigtable/v2/*.proto` and `google/bigtable/admin/v2/*.proto` (master, fetched 2026-10-04) and the generated code in `cloud.google.com/go/bigtable v1.58.0`. Semantics: the pages below plus the fetched pages "Aggregating values at write time", "Manage row key schemas", "Create and manage protobuf schemas", "Create and manage logical views", "Create and manage continuous materialized views", "Continuous materialized view queries", "Configure change streams", "Manage backups", "Quotas and limits" and the release notes.

[data-rpc]: https://docs.cloud.google.com/bigtable/docs/reference/data/rpc
[admin-rpc]: https://docs.cloud.google.com/bigtable/docs/reference/admin/rpc
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

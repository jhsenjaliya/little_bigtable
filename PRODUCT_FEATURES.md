# Product Features Added by `jay-bigtable-extended`

> **Current contract:** [`BIGTABLE_COMPATIBILITY.md`](BIGTABLE_COMPATIBILITY.md)
> (2026-10-04 parity audit) supersedes the status statements below where they
> differ. This document describes the branch delta as of 2026-08-29; statements
> made wrong by the v0.5.0 (unreleased) parity iteration are corrected inline
> and marked "v0.5.0". Test evidence for v0.5.0 is pending.

## Purpose and comparison scope

This document explains the product behavior and operational improvements that
`jay-bigtable-extended` adds to `master`. It is written for product users,
maintainers, and AI agents that need to understand what the branch provides,
why a capability matters, how complete it is, and where its implementation
lives.

The comparison snapshot used for this document is:

- Baseline: `master` at `735e8fd`
- Extended branch before the conformance implementation session:
  `jay-bigtable-extended` at `bcaef10`
- Documentation snapshot: Phase 0 conformance work completed and verified on
  2026-08-29
- Comparison: the net source difference in `master..jay-bigtable-extended`
  plus the Phase 0 implementation recorded in
  [`docs/superpowers/plans/2026-08-29-bigtable-conformance-implementation-plan.md`](docs/superpowers/plans/2026-08-29-bigtable-conformance-implementation-plan.md)

`master` is an ancestor of the extended branch. Therefore, this document
focuses on the branch's net additions and deliberately does not present
features already available on `master` as new work.

For a method-by-method compatibility table, see
[`BIGTABLE_COMPATIBILITY.md`](BIGTABLE_COMPATIBILITY.md). This document is the
higher-level product explanation of the branch delta.
When this document conflicts with older integration notes, prefer current source
and workflows. `Build_and_Integration.md` still describes a `master`-based
manual release process; its driver and CGO statements were corrected for v0.5.0.

## Capability status vocabulary

The descriptions use the following status terms:

- **Functional** — implements the local behavior needed by normal SDK or admin
  workflows and persists state where stated.
- **Metadata-compatible** — accepts and returns the production API's resource
  metadata, but does not reproduce the managed service behind that metadata.
- **Partial emulation** — implements a useful local subset with explicit limits.
- **Compatibility stub** — prevents client setup from failing, but intentionally
  does not enforce the production behavior.
- **Fix** — corrects behavior or operational reliability without introducing a
  new API.

## Executive summary

Compared with `master`, the extended branch changes the emulator from a
SQLite-only data-plane emulator into a broader LocalCloud-compatible Bigtable
emulator. The main additions are:

1. PostgreSQL support alongside SQLite, including binary-safe row keys and
   dialect-specific SQL.
2. Optional strict validation of the Bigtable resource hierarchy.
3. Persistent instance, cluster, and app-profile administration.
4. Table deletion protection.
5. Metadata-compatible authorized views, logical views, backups, copy-backup,
   and restore-table operations.
6. A persistent, single-partition change-stream implementation.
7. Permissive IAM compatibility stubs.
8. Additional mutation, filter, garbage-collection, and error-handling parity.
9. Pure-Go SQLite support and safer cross-platform builds.
10. An executable compatibility ledger, identical SQLite/PostgreSQL restart
    conformance, and a pinned hermetic `cbt` client smoke.
11. Docker, constrained-network dependency, release, distribution, and test
    automation for the extended branch.

The branch is still a single-process development emulator. It does not attempt
to reproduce production Bigtable's distributed storage, access enforcement,
capacity management, replication, or query engines.

## Feature matrix

| Area | Branch addition | Status | Primary user value |
| --- | --- | --- | --- |
| Storage | PostgreSQL backend plus retained SQLite backend | Functional | Run the emulator against the same shared SQL service used by LocalCloud environments. |
| SQLite | Pure-Go SQLite driver | Functional | Build and run without a C compiler or CGO dependency. |
| Resource hierarchy | Strict instance checks | Functional, configurable | Catch setup code that creates or accesses tables before creating the instance. |
| Instance administration | Instance CRUD and partial updates | Functional local metadata | Run SDK, Terraform, and bootstrap flows locally. |
| Cluster administration | Cluster CRUD and `serve_nodes` updates | Metadata-compatible | Exercise cluster configuration without provisioning capacity. |
| App profiles | App-profile CRUD with local defaults | Metadata-compatible | Allow standard SDK routing setup to complete. |
| Persistence | Admin resources and view/backup metadata survive restarts | Functional | Preserve local environment state between emulator runs. |
| Table safety | Table deletion protection | Functional | Test protected-resource workflows. v0.5.0: persisted across restart; soft delete with `UndeleteTable`. |
| IAM | Get, set, and test-permissions methods | Compatibility stub | Prevent local setup from failing on IAM calls. v0.5.0: policies persisted with etags; still not enforced. |
| Authorized views | Persistent CRUD and deletion protection | Functional (v0.5.0) | v0.5.0: row/family/qualifier subsets enforced on reads and writes. |
| Logical views | Persistent CRUD, query storage, deletion protection | Partial emulation (v0.5.0) | v0.5.0: query validated and executable through `ExecuteQuery` in the `gsqlready` build. |
| Backups | CRUD, copy, and restore-table operations | Functional (v0.5.0) | v0.5.0: backups snapshot schema and rows; restore is independent of the live table. |
| Change streams | Persistent mutation log and streaming RPCs | Partial emulation | Test consumers, continuation tokens, and heartbeats. v0.5.0: opt-in per table, retention, grouped records; single partition. |
| Aggregate mutations | `AddToCell` and `MergeToCell` | Partial emulation | v0.5.0: typed Sum/Min/Max Int64 families; HLL++ in the `gsqlready` build. |
| Filters | Explicit unsupported errors | Fix | Avoid silent false-positive test results. v0.5.0: `Interleave` keeps duplicates as in production; `Sink` and `ValueBitmask` implemented. |
| GC | Intersection-rule implementation | Functional (v0.5.0) | v0.5.0: intersection deletes only when every child rule deletes. |
| Protocol evolution | Unimplemented-server embedding | Fix | New proto RPCs fail safely with `Unimplemented` instead of panicking. |
| Liveness RPC | `PingAndWarm` | Compatibility stub | Let clients probe the local endpoint. v0.5.0: validates the instance and app profile. |
| Conformance baseline | Executable RPC/field ledger, backend-neutral restart contract, and pinned `cbt` smoke | Functional | Make compatibility claims reviewable, detect generated-API drift, and prove core SDK/CLI workflows on both SQL backends. |
| Container builds | Online and staged Go-dependency, custom-CA, multi-stage Docker build | Functional with image/network caveats | Build in constrained or corporate networks. |
| Releases | Branch-restricted tagging and binary publication | Functional | Ensure releases are built from the tested extended-branch commit. |
| Local distribution | Four-platform archive generation | Functional | Produce local Linux/macOS AMD64/ARM64 artifacts without publishing. |

## 1. Dual SQL storage backends

### What the feature does

The branch introduces process-wide dialect configuration through
`bttest.ConfigureStorage(driver, strictAdmin)`:

- `sqlite3` selects SQLite.
- `postgres`, `postgresql`, and `pq` select PostgreSQL SQL syntax.

Those aliases configure the dialect layer, not `database/sql` driver
registration. The shipped binary registers `sqlite3` and `postgres`; its
`--database-driver` value is also passed directly to `sql.Open`, so CLI users
must use `postgres`, not the `postgresql` or `pq` dialect aliases.

The standalone binary exposes this choice through:

- `--database-driver`
- `--database-url`
- `--db-file` for legacy SQLite file configuration
- `--strict-admin`

The branch's standalone binary defaults to PostgreSQL. A PostgreSQL run must
provide `--database-url`. A caller that wants the old SQLite behavior must
select `--database-driver sqlite3`; the binary then derives the SQLite DSN from
`--db-file` and limits the database to one open connection.

### How PostgreSQL compatibility works

The branch adds a small SQL dialect layer rather than maintaining two separate
storage implementations. It provides:

- Placeholder conversion from SQLite-style `?` parameters to PostgreSQL `$1`,
  `$2`, and so on.
- Dialect-specific schema types such as SQLite `BLOB` versus PostgreSQL
  `BYTEA`, and SQLite autoincrement IDs versus PostgreSQL `BIGSERIAL`.
- Conflict-safe upserts for persisted table and metadata records.
- Binary row-key parameters and scans, preserving arbitrary Bigtable row-key
  bytes instead of treating row keys as SQL text.

### Persisted entities

The branch schema can persist:

- Rows and table metadata.
- Instances, clusters, and app profiles.
- Materialized-view registrations.
- Change-stream records.
- Authorized views.
- Backup metadata.
- Logical views.

v0.5.0: IAM policies, long-running operations, schema bundles, backup
manifests and idempotency tokens are also persisted; the change log moved to
`change_stream_t`.

### User value

PostgreSQL mode allows one persistent emulator service to retain state across
restarts while serving multiple LocalCloud components. SQLite remains useful
for isolated tests and single-process development.

### Important constraint

`ConfigureStorage` is global to the process. One emulator process should use
one selected dialect and strict-admin mode. Tests that start multiple servers
with different configurations must serialize or restore the global setting.

## 2. Pure-Go SQLite

### What changed

The branch replaces `mattn/go-sqlite3` with `glebarez/go-sqlite` and registers
the pure-Go driver under the existing `sqlite3` name.

### User value

- No C compiler is needed for normal SQLite builds.
- `CGO_ENABLED=0` release binaries can still use SQLite.
- Linux and macOS cross-compilation is simpler.
- Existing standalone-binary users can retain the `sqlite3` driver name.

The standalone binary registers the pure-Go driver alias. An application that
embeds only the `bttest` package must import and register a compatible SQL
driver itself; importing `bttest` does not register SQLite globally.

This is a clean driver cutover rather than a second SQLite mode.

## 3. Strict administrative hierarchy

### What the feature does

When strict admin mode is enabled, table creation and table listing require the
parent instance to exist. Missing parents return `NotFound` instead of allowing
an implicit hierarchy.

The standalone binary enables strict mode by default. Library users can disable
it with:

```go
bttest.ConfigureStorage("sqlite3", false)
```

### User value

Strict mode catches production setup errors in local tests. Permissive mode
retains compatibility with older tests that create tables without first
creating an instance.

### Scope

This validates local resource existence; it does not reproduce production
project authorization, quotas, locations, or provisioning.

## 4. Instance, cluster, and app-profile administration

### Instances

The branch implements create, get, list, update, partial update, and delete
operations for instances.

Instance creation:

- Assigns the fully qualified resource name.
- Defaults the display name to the instance ID.
- Defaults an unspecified type to `PRODUCTION`.
- Marks the instance `READY` immediately.
- Records a creation timestamp.
- Creates any clusters supplied in the create request.
- Ensures a default app profile exists.
- Returns an already-completed long-running operation.
- v0.5.0: requires at least one cluster and a 6–33 character instance ID;
  display name 4–30 characters; edition defaults to `ENTERPRISE`.

Partial instance updates support:

- `display_name`
- `type` (v0.5.0: `PRODUCTION` to `DEVELOPMENT` is rejected)
- `labels`
- `edition` (v0.5.0)

Deleting an instance removes its local tables and row data, clusters, app
profiles, and materialized-view registrations. v0.5.0: deletion is refused
while protected tables, views or backups exist. It also removes the persisted
instance, cluster, and app-profile metadata.

### Clusters

The branch implements cluster create, get, list, update, partial update, and
delete operations.

Local cluster behavior includes:

- Parent-instance validation.
- A default local location when none is supplied.
- Immediate `READY` state.
- Persistent metadata.
- Partial updates of `serve_nodes` (v0.5.0: also `cluster_config` autoscaling
  and `node_scaling_factor`; deleting the last cluster, a cluster with backups,
  or a cluster used by app-profile routing is refused).

`serve_nodes` is metadata only. It does not change emulator capacity or create
worker nodes.

### App profiles

The branch implements app-profile create, get, list, update, and delete
operations. When routing or isolation is omitted, the emulator supplies local
defaults:

- Single-cluster routing to an available cluster, or `local-cluster`
  (v0.5.0: routing must reference existing clusters, and data RPCs enforce the
  profile's routing rules).
- Transactional writes enabled.
- Standard isolation with high priority.

These settings let SDK bootstrap and configuration flows complete. They do not
create real traffic-routing or isolation behavior in the single-process
emulator.

## 5. Persistent administrative metadata

On startup, the server reloads instances, clusters, and app profiles from SQL.
This differs from `master`, where much of the administrative hierarchy was
unimplemented or memory-only.

### User value

A LocalCloud environment can restart the emulator without forcing every client
to recreate its administrative hierarchy. This is especially useful for:

- Integration tests spanning process restarts.
- Terraform/provider development.
- Local application stacks with independent bootstrap phases.
- Repeated SDK sessions against the same emulator database.

## 6. Table deletion protection

### What the feature does

`UpdateTable` accepts the `deletion_protection` update-mask path. The value is:

- Returned by `GetTable` for the current server process.
- Enforced by `DeleteTable`.

Deleting a protected table returns `FailedPrecondition`.

v0.5.0: `UpdateTable` persists the table record, so the protection flag
survives restart. Deletion is also blocked by authorized-view protection and
by continuous materialized views that read the table.

### User value

Applications and infrastructure tooling can test the same protect-update-delete
lifecycle they use with production resources, including expected error handling.

The existing automated-backup-policy update behavior comes from `master`; the
new branch behavior is the additional deletion-protection field and enforcement.

## 7. IAM compatibility stubs

The branch implements:

- `GetIamPolicy`
- `SetIamPolicy`
- `TestIamPermissions`

Behavior is deliberately permissive:

- `GetIamPolicy` returns the stored policy or an empty version-1 policy.
- `SetIamPolicy` stores the supplied policy (v0.5.0: persisted in
  `iam_policies_t`, etag checked, `update_mask` honored; the resource must exist).
- `TestIamPermissions` returns every requested permission as granted.

### User value

SDK setup, Terraform, and admin scripts no longer fail with `Unimplemented` when
they configure IAM during local development.

### Non-goal

There is no authentication or authorization enforcement. Policies do not limit
data or admin operations.

## 8. Authorized views

### What the feature does

The branch implements persistent authorized-view create, get, list, update, and
delete operations.

Behavior includes:

- Requiring the parent table to exist at creation time.
- Duplicate detection.
- Listing views by parent table.
- Updating the authorized-view definition and deletion-protection flag.
- Blocking deletion while deletion protection is enabled.

### User value

Users can validate configuration and lifecycle code that manages authorized
views through the standard Bigtable Admin API.

### Fidelity limit

v0.5.0: reads through an authorized view are filtered by its subset
definition and writes outside it return `PermissionDenied`. IAM is still not
enforced, so the emulator does not test who may use a view.

## 9. Logical views

### What the feature does

The branch implements persistent logical-view create, get, list, update, and
delete operations. Updates can change:

- `query`
- `deletion_protection`

Deletion protection is enforced.

### User value

Admin and infrastructure code can manage logical-view resources locally using
the production API shape.

### Fidelity limit

v0.5.0: in the `gsqlready` build the query is validated by the GoogleSQL
engine and the view can be queried with `ExecuteQuery`. In the default build
the query is stored without validation. Parameterized views are not supported.

## 10. Backups, backup copies, and table restore

### Backup lifecycle

The branch implements create, get, list, update, and delete operations for
backup resources.

A newly created backup:

- Requires its referenced source table to exist.
- Becomes `READY` immediately.
- Receives local start and end timestamps.
- Persists its metadata in SQL.
- v0.5.0: snapshots the table schema and rows, reports `size_bytes`, and
  validates expiry, backup type and per-table quotas.

`UpdateBackup` currently supports `expire_time`.

### Copy backup

`CopyBackup` records the source-backup relationship. v0.5.0: it copies the
snapshot data.

### Restore table

`RestoreTable` creates a new table resource. v0.5.0: schema and rows come from
the backup snapshot, independent of the live source table.

### User value

These operations let backup schedulers, Terraform providers, admin clients, and
error-handling logic run locally without branching around unsupported RPCs.

### Fidelity limit

v0.5.0: backups are point-in-time snapshots in the local database. They do not
model production durability, cross-region placement or CMEK.

## 11. Change streams

### What the feature does

The branch adds a persistent SQL change log and implements:

- `GenerateInitialChangeStreamPartitions`
- `ReadChangeStream`

The emulator records changes from:

- `MutateRow`
- `MutateRows`
- `CheckAndMutateRow`
- `ReadModifyWriteRow`

Recorded mutation categories include set-cell and delete operations. Each record
has an ordered ID and a microsecond commit timestamp.

### Stream behavior

- Initial partition generation returns one stable partition covering the full
  table.
- Continuation tokens encode the last processed change-log ID.
- Start-time and end-time fields are accepted, with the fidelity limits below.
- Reads emit records in change-log order.
- Configurable heartbeats keep an idle stream alive.
- Missing tables return `NotFound`.

### User value

Developers can test change-stream consumers, checkpoint storage, restart from a
continuation token, heartbeat handling, and basic time-bounded calls without
connecting to production Bigtable, subject to the limits below.

### Fidelity limits

- Only one full-table partition is produced; no split/merge.
- The emulator does not reproduce production partition topology or
  distributed ordering behavior.
- v0.5.0 corrected the earlier limits: records are written only when
  `change_stream_config` is set and expire after the retention period; one
  record groups a row commit; GC and `DropRowRange` changes are recorded;
  start/end times and partition ranges are honored.

## 12. Aggregate mutation support

The branch adds handling for Bigtable's `AddToCell` and `MergeToCell` mutation
variants.

### Implemented behavior

- The target column family must exist.
- A missing timestamp receives a new local timestamp.
- Values at the same timestamp are combined.
- Eight-byte values use big-endian signed int64 sum semantics.
- A missing compatible value starts from zero.

### User value

Applications using aggregate mutation protos can exercise int64 counter-like
behavior without receiving an unsupported-mutation response.

### Fidelity limits

v0.5.0: the family's aggregate `value_type` is persisted and enforced
(Sum/Min/Max over Int64; HLL++ in the `gsqlready` build, with emulator-specific
sketch bytes). `AddToCell`/`MergeToCell` on a non-aggregate family return
`InvalidArgument`.

## 13. Read-filter correctness improvements

### Interleave deduplication

v0.5.0: reverted. Production `Interleave` returns a copy of a cell from every
matching branch, and limit/offset filters count those copies; the emulator now
does the same.

### Explicit unsupported-filter errors

Unknown filter variants previously logged a warning and passed cells through.
That behavior could make a local test succeed while production would behave
differently. The branch returns `Unimplemented` instead.

**User impact:** unsupported behavior fails visibly and cannot create a silent
false-positive test.

### Sink filter

v0.5.0: `Sink` sends its input to the final result and nothing to its parent;
`Sink` inside `Condition` is rejected. `ValueBitmask` is also implemented.

## 14. Read-modify-write change-log integration

`master` already applies read-modify-write rules to the most recent cell,
maintains a nondecreasing timestamp, and synchronizes materialized-view shadow
tables (v0.5.0 replaced shadow tables; see `CMV_SUPPORT.md`). The extended branch preserves that behavior and adds one runtime
capability: resulting mutations are appended to the new local change log.

The branch also adds regression coverage for the inherited latest-version and
timestamp behavior.

**User impact:** append and increment workflows now participate in local change
streams without presenting inherited read-modify-write semantics as a new
branch feature.

## 15. Garbage-collection intersection rules

v0.5.0: corrected. Intersection deletes a cell only when every child rule
deletes it; union deletes when any child does; nested rules compose. The
earlier kept-set regression coverage asserted the old behavior.

## 16. Forward-compatible gRPC behavior

The server now embeds the generated `UnimplementedBigtableServer`,
`UnimplementedBigtableTableAdminServer`, and
`UnimplementedBigtableInstanceAdminServer` implementations instead of bare
service interfaces.

### Fix provided

When Google adds a new RPC to the generated service interface, an unimplemented
call safely returns `codes.Unimplemented` instead of reaching a nil embedded
method and potentially panicking the emulator.

### User value

The emulator is safer to use with newer client libraries, even before every new
Bigtable API has a local implementation.

This does not mean every generated RPC is supported. v0.5.0 examples still
explicitly unimplemented: snapshot APIs, hot-tablet listing and memory layers;
GoogleSQL returns `Unimplemented` only in builds without `gsqlready`.

### `PingAndWarm` compatibility

The branch adds an explicit `PingAndWarm` handler that returns an empty
successful response and performs no warming or capacity work. v0.5.0: it
validates the instance name and app profile first.

## 17. Fork module, library configuration, and CLI identity

The module path changes from the upstream repository to:

```text
github.com/jhsenjaliya/little_bigtable
```

The branch adds `bttest.ConfigureStorage` so embedded callers can select the SQL
dialect and strict-admin behavior. `bttest.CreateTables`, `bttest.NewServer`,
and the ability to run the emulator in process are inherited from `master`, not
new branch features.

The standalone binary also changes its observable version identity. Its default
build version is `0.3.0-localcloud`, and `-version` reports the selected version
plus the Go runtime used to build it:

```text
bigtable-emulator-extended v0.3.0-localcloud (built w/<Go runtime version>)
```

Release packaging currently uses the version compiled from
`little_bigtable.go`; it does not inject the Git tag. Maintainers must update
the version constant before releasing if the binary identity should match the
tag. The fork-specific module and binary identity let dependency and
version-checking code distinguish this extended distribution from upstream.

## 18. Container and constrained-network builds

The branch adds a multi-stage Dockerfile and LocalCloud-specific make targets.
Supported build scenarios include:

- Custom Go and runtime base images.
- Public ECR base images to avoid Docker Hub availability or authentication
  issues.
- BuildKit cache mounts for modules and compilation.
- An optional corporate CA-bundle secret.
- Retried online module downloads.
- Offline Go-module resolution from a staged vendor tree or module cache.
- A small runtime image containing the emulator binary.

### Current caveats

The Dockerfile builder and `go.mod` both pin Go 1.27.0. Offline module mode
therefore uses the builder's bundled toolchain and does not depend on an
automatic toolchain download.

`GO_OFFLINE=true` prevents Go module downloads; it does not make the complete
Docker build air-gapped. The builder still runs Alpine `apk add` for `gcc` and
`musl-dev`. A network-isolated build therefore also needs access to a mirrored
APK repository or a custom builder image that already contains those packages.

## 19. Release automation

The extended branch adds a dedicated Go-module release workflow.

### Manual release path

A manually supplied `vX.Y.Z` version causes the workflow to:

1. Resolve the exact tip of `jay-bigtable-extended`.
2. Reject an invalid or existing tag.
3. Test that exact commit.
4. Create an annotated tag on that commit.
5. Re-verify the remote tag target before publishing.
6. Create the GitHub release.
7. Build Linux and macOS binaries for AMD64 and ARM64 with `CGO_ENABLED=0`.
8. Upload the binaries and verify the module can be fetched.

### Pushed-tag path

A pushed `v*` tag is accepted only when its commit is reachable from
`jay-bigtable-extended`. The workflow tests and builds the validated commit,
not an unrelated branch tip.

### User value

The release cannot be redirected to `master` through workflow inputs, and build
artifacts correspond to the commit that passed release validation.

## 20. Test, distribution, and maintainer tooling

### `test.sh`

The local test entry point now:

- Lets the selected Go binary locate its own matching standard library instead
  of inheriting a stale `GOROOT`.
- Runs the complete normal test suite.
- Runs race tests on supported host configurations.
- Runs `go vet`.
- Fails when Go source is not formatted.
- Uses enough package-timeout headroom for the concurrency tests.

### CI workflow

Push and pull-request CI targets `jay-bigtable-extended` and uses Go 1.27.0.
The main job builds all packages, installs the pinned standalone `cbt` binary,
and runs the complete `bttest` package with the CLI smoke required. Two
independent storage jobs run the same restart-persistence contract against
SQLite and a PostgreSQL 17 service. A failure in either backend is reported by
its own job.

Unlike the local `test.sh`, CI does not currently run race tests, `go vet`, the
formatting check, or tests outside `bttest`; those checks are
local/release-script coverage rather than branch CI coverage.

### `dist.sh`

The local distribution script now:

- Runs the complete test entry point first.
- Builds Linux and macOS binaries for AMD64 and ARM64.
- Uses `CGO_ENABLED=0`.
- Produces versioned `.tar.gz` archives.
- Avoids `sudo` and root-owned files.
- Suppresses macOS AppleDouble metadata in archives.
- Stages the complete artifact set before replacing `dist/`, so a failed target
  does not leave a partial distribution presented as complete.

`dist.sh` is local packaging only. It does not create tags or publish a GitHub
release.

### Added compatibility suites

The branch adds official-client hello-world coverage, broader
documentation-derived data/admin scenarios, and focused LocalCloud tests for
the new control-plane, aggregate, filter, and persistence behavior. Much of the
documentation-derived suite protects inherited behavior; it is regression
coverage rather than evidence that every exercised API was added by this
branch.

#### Executable capability ledger

`bttest.CompatibilityLedger()` is the checked-in compatibility source of truth
for every RPC registered by `NewServer` and every field in eight pinned,
high-risk request messages. Each entry records:

- a stable RPC or field identifier;
- the intended local support disposition;
- observed verification as `test_verified`, `known_nonconformant`, or
  `declared_unverified`;
- the precise local contract and observed behavior; and
- the behavioral test that owns a verified claim.

The ledger does not generate runtime behavior. Its tests instead prevent drift:
they reject duplicate or malformed entries, compare RPC declarations with live
gRPC registration, compare tracked field declarations with protobuf
descriptors, and verify that every `test_verified` owner names a real test.
Known false-success behavior remains visible as `known_nonconformant` until its
owning implementation phase changes the handler.

#### Backend-neutral persistence conformance

`TestStorageConformance` runs one production-Go-client scenario unchanged
against SQLite and PostgreSQL. It creates a table and column family, writes a
row, closes the complete client/server/database harness, starts a new harness,
and verifies that the table, family, and row survived. The test cleans the row,
table, and change-log records it creates, so a reused PostgreSQL test database
does not accumulate conformance artifacts.

This is evidence for local SQL persistence across an emulator restart. It is
not a claim of replication, multi-process coordination, backup durability, or a
managed-service SLA.

#### Pinned `cbt` client conformance

`TestCBTClientConformance` executes the standalone `cbt` binary as an external
process. CI pins `cloud.google.com/go/cbt` at
`v0.0.0-20260810145131-fe593de7bc1a` and verifies the module path and version
from the binary's Go build metadata. The smoke creates an instance, table, and
family; writes three rows; verifies a bounded read includes only the expected
row; then deletes the table and instance.

The subprocess discovers the server through `BIGTABLE_EMULATOR_HOST`, runs from
an empty directory without inherited home, ADC, or gcloud configuration, and
uses a non-secret `-access-token` placeholder required by the pinned CLI's
startup path. It does not use a credentials file, a direct endpoint override,
private test helpers, or SQL access. The test proves this core CLI workflow,
not advanced `cbt` commands or other language clients.

### Maintainer upstream synchronization

The README adds a maintainer procedure for fetching `bitly/little_bigtable` and
merging `upstream/master` into the active branch before pushing the merge to the
fork. This is an operational workflow, not an emulator runtime capability.

## 21. Reliability and regression fixes

### Binary-safe row keys

SQL persistence now reads and writes row keys as bytes. This avoids text
encoding assumptions and keeps arbitrary Bigtable row keys ordered and
addressable across SQLite and PostgreSQL.

### Concurrency-test goroutine cleanup

A pre-existing concurrency test could time out, return, close its database, and
leave worker goroutines issuing SQL calls. Those workers could then panic and
crash unrelated later tests. The branch:

- Reports unexpected worker errors without panicking.
- Ignores expected errors after context cancellation.
- Gives the workers more time to drain.
- Always waits for every worker before test cleanup closes the database.

This fix became important after the pure-Go SQLite driver made the latent timing
window easier to hit on loaded CI runners.

### Sample-row-key concurrency headroom

The concurrent SampleRowKeys tests receive a larger watchdog, while script and
workflow package timeouts are raised to preserve headroom for the rest of the
suite.

### Release tag integrity

The release workflow checks that a tag still points to the tested SHA immediately
before release creation and pins fallback tag creation to that SHA. This avoids
publishing an untested branch tip if a tag is deleted or changed during a run.

### CI branch correction and scope change

Push and pull-request tests target `jay-bigtable-extended`, matching the
repository's active development and release branch rather than `master`. The
workflow's narrower validation scope relative to `master` is documented in
section 20 and should not be mistaken for an increase in CI coverage.

## Capabilities inherited from `master`, not added by this branch

The following important capabilities are present on the extended branch but
should not be attributed to its net delta:

- Core row reads and range reads, including reverse scans.
- Set-cell and delete mutations.
- Batch and conditional mutations.
- Atomic append and increment through `ReadModifyWriteRow`.
- Sample row keys.
- Core table and column-family administration.
- Automated backup-policy metadata on tables.
- The original continuous-materialized-view feature: SQL parsing, key
  transformation, shadow tables, write-time synchronization, and delete
  propagation.
- Most standard row filters.
- SQLite persistence for rows and tables.

The extended branch integrates several inherited capabilities with its new
PostgreSQL dialect, admin hierarchy, change log, and lifecycle cleanup, but the
underlying capability already exists on `master`.

## Explicit limitations and non-goals

Users and AI agents should preserve these boundaries when designing tests or
adding features:

- The emulator is single-process and single-node.
- There is no replication, failover, multi-cluster consistency, or tablet
  topology.
- Cluster capacity and autoscaling fields are metadata only.
- IAM and authorized views do not enforce access.
- Logical-view and GoogleSQL queries are not executed.
- Backups contain no data snapshot and restore no rows.
- Change streams use one full-table partition.
- Change-stream start-time misses can replay history, end times do not filter
  records by commit timestamp, and logs survive table-name reuse.
- Table deletion-protection changes made through `UpdateTable` are process-local.
- GC intersection currently deletes more aggressively than production AND
  semantics.
- Snapshot APIs remain unimplemented.
- Hot-tablet, Key Visualizer, Data Boost, CMEK, and managed-service operations
  are out of scope.
- Aggregate-cell behavior is limited primarily to int64 sums.
- Continuous materialized views remain a partial implementation; unusual SQL,
  backfill, and later column-family/GC-policy propagation have known limits.
- The emulator is intended for application compatibility and local workflow
  validation, not production correctness, durability, security, or performance
  benchmarking.

## Implementation and evidence map

| Capability | Primary implementation | Primary validation or reference |
| --- | --- | --- |
| Storage selection and strict mode | `bttest/dialect.go`, `little_bigtable.go` | `bttest/google_docs_hello_test.go` |
| PostgreSQL and binary SQL persistence | `bttest/sql_schema.go`, `bttest/storage.go` (v0.5.0; replaced `sql_rows.go`), `bttest/sql_tables.go` | `BIGTABLE_COMPATIBILITY.md` |
| Backend-neutral restart persistence | `bttest/storage_conformance_test.go` | `TestStorageConformance` on SQLite and PostgreSQL 17; separate CI jobs |
| Instance, cluster, app-profile admin | `bttest/localcloud_instance_admin.go` (v0.5.0; `instance_server.go` removed) | `bttest/instance_server_test.go`, `bttest/google_docs_hello_test.go` |
| Admin persistence | `bttest/sql_admin_metadata.go`, `bttest/inmem.go` | `TestInstancePersistence` in `bttest/inmem_test.go` |
| Table deletion protection | `bttest/table_admin.go` (v0.5.0) | v0.5.0 evidence pending |
| IAM stubs | `bttest/sql_iam.go`, `bttest/localcloud_instance_admin.go` (v0.5.0) | v0.5.0 evidence pending |
| Authorized views | `bttest/localcloud_authorized_views.go` | Authorized-view tests in `bttest/localcloud_new_features_test.go` |
| Logical views | `bttest/localcloud_logical_views.go` | Logical-view tests in `bttest/localcloud_new_features_test.go` |
| Backups and restore | `bttest/localcloud_backups.go` | Backup/copy/restore tests in `bttest/localcloud_new_features_test.go` |
| Change streams | `bttest/localcloud_change_stream.go`, `bttest/data_write.go` (v0.5.0) | v0.5.0 evidence pending |
| Aggregate mutations | `bttest/mutation_engine.go` (v0.5.0) | Add/Merge tests in `bttest/localcloud_new_features_test.go` |
| Filter and GC behavior | `bttest/filter.go`; `applyGC` in `bttest/inmem.go` (v0.5.0) | v0.5.0 evidence pending; earlier Interleave/GC tests asserted the old behavior |
| Liveness and protocol fallback | `bttest/localcloud_change_stream.go`, server embeddings in `bttest/inmem.go` | Implementation inspection |
| v0.5.0 parity iteration (all areas) | See `BIGTABLE_COMPATIBILITY.md` | Evidence pending |
| Executable compatibility ledger | `bttest/compatibility.go` | `bttest/compatibility_test.go`; live RPC and protobuf descriptor drift checks |
| Pinned `cbt` client workflow | `bttest/client_conformance_test.go` | `TestCBTClientConformance`; pinned binary build metadata and subprocess assertions |
| Fork module and CLI identity | `go.mod`, `little_bigtable.go` | Release workflow build configuration |
| Pure-Go SQLite | `little_bigtable.go`, `go.mod` | Cross-platform release and distribution builds |
| Docker/constrained-network builds | `Dockerfile`, `Makefile.localcloud` | Source inspection with the image/network caveats above |
| Release safety | `.github/workflows/release-module.yaml` | Workflow validation and branch/tag checks in the workflow itself |
| Local and CI quality tooling | `test.sh`, `dist.sh`, `.github/workflows/test.yaml` | Full local test/race/vet/format gate plus required `cbt`, SQLite, and PostgreSQL CI lanes |
| Added compatibility suites | `bttest/google_docs_features_test.go`, `bttest/google_docs_hello_test.go`, `bttest/localcloud_new_features_test.go`, `bttest/compatibility_test.go`, `bttest/storage_conformance_test.go`, `bttest/client_conformance_test.go` | The test scenarios and drift assertions themselves |

## Guidance for future changes

When evaluating a new branch change, classify it against this document:

1. Is it a net addition over `master`, or an inherited capability?
2. Is the behavior functional, metadata-compatible, partial, a stub, or a fix?
3. What observable user workflow does it enable?
4. What state persists across restart?
5. What production behavior is intentionally absent?
6. Which implementation and behavioral test prove the claim?

Do not promote a capability from partial or metadata-compatible to functional
without adding a behavioral test for the production contract being claimed.

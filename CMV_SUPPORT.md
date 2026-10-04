# Continuous Materialized View (CMV) Support

Status as of `v0.5.0` (commit `dd5f9e7`) and the 2026-10-04 parity audit
([`BIGTABLE_COMPATIBILITY.md`](BIGTABLE_COMPATIBILITY.md), capabilities
BT-CMV-1 and BT-CMV-2). CMVs are implemented on the GoogleSQL engine in
`bttest/internal/gsql`, which is always built.

Owning tests: `TestConformanceMaterializedViewReadsAndRefresh`,
`TestConformanceMaterializedViewSurvivesRestart`,
`TestConformanceMaterializedViewSession` (`bttest/query_conformance_test.go`);
`TestMaterializedViewAggregations`, `TestMaterializedViewSecondaryIndex`,
`TestMaterializedViewRejections` (`bttest/internal/gsql/mv_test.go`);
`TestOrderedCodeStructExamples`, `TestOrderedCodeStructPreservesOrder`
(`bttest/internal/gsql/keycodec_test.go`).

## Overview

A continuous materialized view is a read-only, pre-computed result of a
GoogleSQL query over one source table. Production Bigtable maintains it
asynchronously. The emulator defines views through the standard
`CreateMaterializedView` Admin RPC and serves them through the same read paths
as production.

Owner: `bttest/materialized_views.go` (definition, storage, recomputation,
CRUD), `bttest/sql_materialized_views.go` (persisted definitions in
`materialized_views_t`), `bttest/targets.go` and `bttest/session_streaming.go`
(read targets).

## Creating a view

```go
iac, err := bigtable.NewInstanceAdminClient(ctx, project)
err = iac.CreateMaterializedView(ctx, instanceID, &bigtable.MaterializedViewInfo{
    MaterializedViewID: "clicks_by_account",
    Query: "SELECT SPLIT(_key, '#')[SAFE_OFFSET(1)] AS account, " +
        "COUNT(*) AS clicks " +
        "FROM `events` " +
        "GROUP BY account",
})
```

The query is parsed, type-checked against the source table and prepared by the
GoogleSQL engine (`gsql.PrepareMaterializedView`). An invalid query fails the
create call.

### Query rules (upstream)

From "Continuous materialized view queries" (docs fetched 2026-10-04):

- The statement is a `SELECT` with either a `GROUP BY` clause (aggregation) or,
  for an asynchronous secondary index, an `ORDER BY` clause — not both.
- With `GROUP BY`, every unaggregated output column must be grouped; aggregated
  columns use supported aggregation functions.
- With `ORDER BY`, the ordered columns become the row key, in clause order.
- Optional `_key` column: must be `BYTES`; the query must group by `_key` and
  nothing else except, optionally, `_timestamp`.
- Optional `_timestamp` column of type `TIMESTAMP` sets the cell timestamp.
- `LIMIT`/`OFFSET` and nested `GROUP BY`/`ORDER BY` are not allowed.

The engine rejects invalid definitions at create time; rejected shapes are
covered by `TestMaterializedViewRejections`.

## Storage layout

| Aspect | Emulator behavior |
| --- | --- |
| Storage location | Hidden storage ID `__mv__/<view id>` in `rows_t`; not visible as a table |
| Row key, `_key` only | The `_key` bytes are used directly |
| Row key, other key columns | `GROUP BY`/`ORDER BY` key columns encoded as an OrderedCodeBytes struct key (`gsql.EncodeKey`). **Emulator choice:** production does not document its multi-column key encoding, so do not depend on these bytes matching production. |
| Value columns | Family `default`; qualifier = column alias |
| Map-typed columns (for example a whole source family selected as `cf1 AS cf1`) | Their own column family named by the alias; one cell per map key |
| Cell timestamp | 0 (1970-01-01T00:00:00Z) unless the query outputs `_timestamp`; `_timestamp` is truncated to milliseconds |
| NULL values | Value columns with NULL produce no cell |

## Reading a view

Views are read-only. Use any of:

```go
// Data API with materialized_view_name (ReadRows, SampleRowKeys).
mv := client.OpenMaterializedView("clicks_by_account")
row, err := mv.ReadRow(ctx, rowKey)

// GoogleSQL (PrepareQuery / ExecuteQuery).
ps, err := client.PrepareStatement(ctx, "SELECT * FROM `clicks_by_account`", nil)
```

- `ReadRows` and `SampleRowKeys` with `materialized_view_name`.
- Session protocol: `OpenMaterializedView` (virtual ReadRow; writes rejected).
- SQL: `ExecuteQuery` over the view.
- There is no write path: the Data API mutation RPCs cannot name a
  materialized view, and session writes to a view are rejected.

The view is marked stale by each committed source write and recomputed from
the source table before the next read (`readable`, `recompute`). Reads
therefore observe every committed source write. Production CMVs are eventually
consistent; the emulator does not reproduce that lag.

## Administration

| Operation | Behavior |
| --- | --- |
| `CreateMaterializedView` | Validates the query; at most 50 views per instance and 5 per table (`ResourceExhausted`) |
| `GetMaterializedView`, `ListMaterializedViews` | Return the stored definition with an etag |
| `UpdateMaterializedView` | Only `deletion_protection`; the query is immutable |
| `DeleteMaterializedView` | `FailedPrecondition` while deletion protection is enabled; otherwise removes definition and storage |
| `DeleteTable` on a source table | `FailedPrecondition` while a CMV reads the table |
| `DeleteInstance` | Blocked by protected views; otherwise cascades |

## Migration from the shadow-table implementation (v0.4.x and earlier)

The previous implementation (`bttest/cmv.go`, `bttest/sql_parse.go`, removed)
kept a regular table named after the view, maintained by a regex parser for a
narrow `SPLIT(_key)…ORDER BY` subset. On startup, `LoadMaterializedViews`
re-creates each stored view on the GoogleSQL engine and **removes the legacy
shadow table** named after the view.

- Code that read the view with `client.Open("<view id>")` must switch to
  `client.OpenMaterializedView("<view id>")`, `materialized_view_name`, or SQL.
- Shadow-table rows are not migrated; the view is recomputed from its source.
- Re-keying behavior differs: multi-column keys use the struct encoding above
  instead of `#`-joined strings. Define `_key` explicitly if an application
  needs a specific byte layout.

## Known limitations

- Multi-column key encoding is emulator-specific.
- `_timestamp` is truncated to milliseconds; production treats non-multiples of
  1,000 as invalid rows (counted in `materialized_view/user_errors`).
- Each recomputation rebuilds the whole view from the source table; cost grows
  with source size.
- No CMV metrics (`user_errors`, lag) are emulated.

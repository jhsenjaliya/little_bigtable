package gsql

import (
	"bytes"
	"context"
	"testing"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const splitMV = "SELECT SPLIT(_key, '#')[SAFE_OFFSET(3)] AS region, SPLIT(_key, '#')[SAFE_OFFSET(4)] AS account_id, " +
	"SPLIT(_key, '#')[SAFE_OFFSET(1)] AS ts, SPLIT(_key, '#')[SAFE_OFFSET(2)] AS typ, SPLIT(_key, '#')[SAFE_OFFSET(0)] AS item_id, " +
	"_key AS src_key, cf1 AS cf1 FROM `events` ORDER BY region, account_id, ts, typ, item_id, src_key"

func mvTestCatalog() *memCatalog {
	c, _ := stdCatalog()
	ev := c.addTable(&Table{Name: "events", Families: []Family{{Name: "cf1"}}})
	ev.setStr("i1#2024-01-01#click#us#acc1", "cf1", "q", 1, "v1")
	ev.setStr("i2#2024-01-02#view#eu#acc2", "cf1", "q", 1, "v2")
	ev.setStr("i3#2024-01-01#click#us#acc0", "cf1", "q", 1, "v3")
	ev.setStr("short#1", "cf1", "q", 1, "v4")
	ev.setStr("i4#2024-01-03#buy#eu#acc2", "cf1", "q", 1, "v5")
	sr := c.addTable(&Table{Name: "sensor_readings", Families: []Family{{Name: "data"}}})
	sr.setStr("dev1#100#us#u1", "data", "temp", 1, "20")
	sr.setStr("dev2#200#eu#u2", "data", "temp", 1, "21")
	ads := c.addTable(&Table{Name: "ads", Families: []Family{{Name: "data"}}})
	ads.setStr("adv1#us-east#ad1", "data", "spend_usd", 1, "100")
	ads.setStr("adv1#us-west#ad2", "data", "spend_usd", 1, "150")
	ads.setStr("adv2#us-east#ad3", "data", "spend_usd", 1, "200")
	return c
}

func runPrepared(t *testing.T, cat Catalog, p *Prepared) []string {
	t.Helper()
	var rows [][]*btpb.Value
	require.NoError(t, p.Execute(context.Background(), cat, nil, func(r []*btpb.Value) error {
		rows = append(rows, r)
		return nil
	}))
	return render(t, p.Metadata(), rows)
}

func rawRows(t *testing.T, cat Catalog, p *Prepared) [][]*btpb.Value {
	t.Helper()
	var rows [][]*btpb.Value
	require.NoError(t, p.Execute(context.Background(), cat, nil, func(r []*btpb.Value) error {
		rows = append(rows, r)
		return nil
	}))
	return rows
}

func TestMaterializedViewSecondaryIndex(t *testing.T) {
	cat := mvTestCatalog()
	p, def, err := PrepareMaterializedView(splitMV, cat)
	require.NoError(t, err)
	require.Equal(t, "events", def.Source)
	require.Equal(t, []string{"region", "account_id", "ts", "typ", "item_id", "src_key"}, def.KeyColumns)
	require.Equal(t, []string{"cf1"}, def.ValueColumns)
	require.Equal(t, "", def.TimestampColumn)
	require.False(t, def.Aggregated)
	require.NotNil(t, def.KeySchema)
	require.NotNil(t, def.KeySchema.GetEncoding().GetOrderedCodeBytes())
	require.Len(t, def.KeySchema.GetFields(), 6)
	for i, f := range def.KeySchema.GetFields() {
		require.Equal(t, def.KeyColumns[i], f.GetFieldName())
		require.NotNil(t, f.GetType().GetBytesType().GetEncoding().GetRaw(), "SPLIT on BYTES yields BYTES key fields")
	}
	require.Equal(t, []string{"region", "account_id", "ts", "typ", "item_id", "src_key", "cf1"}, colNames(p))
	require.Equal(t, []string{"BYTES", "BYTES", "BYTES", "BYTES", "BYTES", "BYTES", "MAP<BYTES, BYTES>"}, colTypes(t, p))

	require.Equal(t, []string{
		`b"eu", b"acc2", b"2024-01-02", b"view", b"i2", b"i2#2024-01-02#view#eu#acc2", {b"q": b"v2"}`,
		`b"eu", b"acc2", b"2024-01-03", b"buy", b"i4", b"i4#2024-01-03#buy#eu#acc2", {b"q": b"v5"}`,
		`b"us", b"acc0", b"2024-01-01", b"click", b"i3", b"i3#2024-01-01#click#us#acc0", {b"q": b"v3"}`,
		`b"us", b"acc1", b"2024-01-01", b"click", b"i1", b"i1#2024-01-01#click#us#acc1", {b"q": b"v1"}`,
	}, runPrepared(t, cat, p), "rows with NULL key columns (short#1) are excluded; rows are in key order")

	// The view's encoded row keys sort like the rows and round-trip.
	var prev []byte
	for _, r := range rawRows(t, cat, p) {
		key, err := EncodeKey(def.KeySchema, r[:6])
		require.NoError(t, err)
		require.True(t, prev == nil || bytes.Compare(prev, key) < 0)
		prev = key
		dec, err := DecodeKey(def.KeySchema, key)
		require.NoError(t, err)
		for i := range dec {
			require.Equal(t, r[i].GetBytesValue(), dec[i].GetBytesValue())
		}
	}

	// The prior emulator's secondary-index example still works.
	_, def2, err := PrepareMaterializedView("SELECT\n  SPLIT(_key, '#')[SAFE_OFFSET(2)] AS region,\n  SPLIT(_key, '#')[SAFE_OFFSET(3)] AS user_id,\n"+
		"  SPLIT(_key, '#')[SAFE_OFFSET(1)] AS ts,\n  SPLIT(_key, '#')[SAFE_OFFSET(0)] AS device_id,\n  _key AS src_key,\n  data AS data\n"+
		"FROM `sensor_readings`\nORDER BY region, user_id, ts, device_id, src_key", cat)
	require.NoError(t, err)
	require.Equal(t, []string{"region", "user_id", "ts", "device_id", "src_key"}, def2.KeyColumns)
	require.Equal(t, []string{"data"}, def2.ValueColumns)

	// Simple single-column index with no value columns.
	p3, def3, err := PrepareMaterializedView("SELECT SPLIT(_key, '#')[SAFE_OFFSET(0)] AS a FROM `events` ORDER BY a", cat)
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, def3.KeyColumns)
	require.Empty(t, def3.ValueColumns)
	require.Equal(t, []string{`b"i1"`, `b"i2"`, `b"i3"`, `b"i4"`, `b"short"`}, runPrepared(t, cat, p3))
}

func TestMaterializedViewAggregations(t *testing.T) {
	cat := mvTestCatalog()
	p, def, err := PrepareMaterializedView(`SELECT
 SPLIT(_key, "#")[SAFE_OFFSET(0)] AS advertiser_id,
 count(*) AS count,
 sum(cast(cast(data['spend_usd'] as string) as int64)) as sum_spend
 FROM ads
 GROUP BY advertiser_id`, cat)
	require.NoError(t, err)
	require.True(t, def.Aggregated)
	require.Equal(t, "ads", def.Source)
	require.Equal(t, []string{"advertiser_id"}, def.KeyColumns)
	require.Equal(t, []string{"count", "sum_spend"}, def.ValueColumns)
	require.Len(t, def.KeySchema.GetFields(), 1)
	require.Equal(t, []string{`b"adv1", 2, 250`, `b"adv2", 1, 200`}, runPrepared(t, cat, p))

	p, def, err = PrepareMaterializedView("SELECT '*' AS _key, COUNT(*) AS count FROM ads GROUP BY _key", cat)
	require.NoError(t, err)
	require.Nil(t, def.KeySchema)
	require.Equal(t, []string{"_key"}, def.KeyColumns)
	require.Equal(t, []string{"BYTES", "INT64"}, colTypes(t, p))
	require.Equal(t, []string{`b"*", 3`}, runPrepared(t, cat, p))

	p, def, err = PrepareMaterializedView("SELECT _key, TIMESTAMP_TRUNC(_timestamp, DAY) AS _timestamp, SUM(agg['sum']) AS sum_column "+
		"FROM UNPACK(SELECT * FROM t(with_history => TRUE)) GROUP BY 1, 2", cat)
	require.NoError(t, err)
	require.Equal(t, "t", def.Source)
	require.Equal(t, "_timestamp", def.TimestampColumn)
	require.Equal(t, []string{"_key"}, def.KeyColumns)
	require.Equal(t, []string{"sum_column"}, def.ValueColumns)
	require.Nil(t, def.KeySchema)
	require.Equal(t, []string{`b"b#01", TIMESTAMP "1970-01-01 00:00:00+00", 42`}, runPrepared(t, cat, p),
		"groups whose value columns are all NULL are omitted")

	p, def, err = PrepareMaterializedView("SELECT CAST(cf2['n'] AS STRING) AS n, LENGTH(_key) AS len, COUNT(*) AS c, MAX(_key) AS mk, "+
		"AVG(LENGTH(_key)) AS av, BIT_OR(LENGTH(_key)) AS bo, ANY_VALUE(cf1['c1']) AS av2, MIN(cf1['c1']) AS mn "+
		"FROM t WHERE _key != 'a#02' GROUP BY n, len", cat)
	require.NoError(t, err)
	require.Equal(t, []string{"n", "len"}, def.KeyColumns)
	f := def.KeySchema.GetFields()
	require.NotNil(t, f[0].GetType().GetStringType().GetEncoding().GetUtf8Bytes())
	require.NotNil(t, f[1].GetType().GetInt64Type().GetEncoding().GetOrderedCodeBytes())
	require.Equal(t, []string{
		`"11", 4, 1, b"b#01", 4.0, 4, b"jkl", b"jkl"`,
		`"3", 4, 1, b"c#03", 4.0, 4, NULL, NULL`,
		`"5", 4, 1, b"a#01", 4.0, 4, b"xyz", b"xyz"`,
	}, runPrepared(t, cat, p), "WHERE is honored, NULL keys are excluded, rows are in key order")

	// Row-level evaluation errors exclude the source row instead of failing.
	p, _, err = PrepareMaterializedView("SELECT CAST(CAST(cf1['c1'] AS STRING) AS INT64) AS v, COUNT(*) AS c FROM t GROUP BY v", cat)
	require.NoError(t, err)
	require.Empty(t, runPrepared(t, cat, p))

	p, _, err = PrepareMaterializedView("SELECT CAST(cf2['n'] AS STRING) AS n, HLL_COUNT.INIT(_key) AS h, "+
		"BIT_AND(1) AS ba, BIT_XOR(2) AS bx, MIN(LENGTH(_key)) AS mn FROM t GROUP BY n", cat)
	require.NoError(t, err)
	require.Len(t, runPrepared(t, cat, p), 4)
}

func TestMaterializedViewRejections(t *testing.T) {
	cat := mvTestCatalog()
	cat.views["lv"] = "SELECT _key FROM t"
	for _, c := range []struct {
		sql  string
		code codes.Code
		msg  string
	}{
		{"SELECT _key AS k FROM t", codes.InvalidArgument, "group by clause"},
		{"SELECT _key AS k, COUNT(*) AS c FROM t GROUP BY k ORDER BY k", codes.InvalidArgument, "not both"},
		{"SELECT * FROM t ORDER BY _key", codes.InvalidArgument, "select *"},
		{"SELECT _key AS k FROM t ORDER BY k LIMIT 5", codes.InvalidArgument, "limit"},
		{"SELECT _key AS k FROM t ORDER BY k DESC", codes.InvalidArgument, "desc"},
		{"SELECT _key AS k FROM t ORDER BY k NULLS LAST", codes.InvalidArgument, "nulls"},
		{"SELECT DISTINCT _key AS k FROM t ORDER BY k", codes.InvalidArgument, "distinct"},
		{"SELECT _key AS k, SUM(DISTINCT agg['sum']) AS s FROM t GROUP BY k", codes.InvalidArgument, "distinct"},
		{"SELECT _key AS k, ARRAY_AGG(cf1['c1']) AS a FROM t GROUP BY k", codes.InvalidArgument, "array_agg"},
		{"SELECT _key AS k, COUNTIF(TRUE) AS a FROM t GROUP BY k", codes.InvalidArgument, "countif"},
		{"SELECT _key AS k, STRING_AGG(cf1['c1']) AS a FROM t GROUP BY k", codes.InvalidArgument, "string_agg"},
		{"SELECT _key AS k, STRUCT(1 AS a) AS s FROM t ORDER BY k", codes.InvalidArgument, "struct"},
		{"SELECT _key AS k, COUNT(*) OVER () AS c FROM t ORDER BY k", codes.InvalidArgument, "over"},
		{"SELECT _key AS k, DATE '2020-01-01' AS d FROM t ORDER BY k", codes.InvalidArgument, "date"},
		{"SELECT _key AS k, SPLIT(_key, '#') AS parts FROM t ORDER BY k", codes.InvalidArgument, "array"},
		{"SELECT CAST(_key AS STRING) AS k, cf1['c1'] AS v, COUNT(*) AS c FROM t GROUP BY k", codes.InvalidArgument, "neither grouped nor aggregated"},
		{"SELECT COUNT(*) AS c FROM t GROUP BY _key", codes.InvalidArgument, "select list"},
		{"SELECT _key AS k, CURRENT_TIMESTAMP() AS ts FROM t ORDER BY k", codes.InvalidArgument, "non-deterministic"},
		{"SELECT _key AS k, RAND() AS r FROM t ORDER BY k", codes.InvalidArgument, "non-deterministic"},
		{"SELECT CAST(_key AS STRING) AS _key, COUNT(*) AS c FROM t GROUP BY _key", codes.InvalidArgument, "bytes"},
		{"SELECT _key, cf1['c1'] AS c1, COUNT(*) AS n FROM t GROUP BY _key, c1", codes.InvalidArgument, "_key"},
		{"SELECT cf1['c1'] AS k, _key FROM t ORDER BY k", codes.InvalidArgument, "_key must be a key column"},
		{"SELECT _key AS k, ANY_VALUE(cf1) AS m FROM t GROUP BY k", codes.InvalidArgument, "map columns"},
		{"SELECT CAST(1.5 AS FLOAT64) AS f, COUNT(*) AS c FROM t GROUP BY f", codes.InvalidArgument, "row key"},
		{"SELECT CAST(_key AS STRING) = 'x' AS b FROM t ORDER BY b", codes.InvalidArgument, "row key"},
		{"SELECT _key AS k FROM lv ORDER BY k", codes.InvalidArgument, "views"},
		{"SELECT _key AS k FROM nowhere ORDER BY k", codes.NotFound, "nowhere"},
		{"SELECT _key, COUNT(*) FROM t GROUP BY _key", codes.InvalidArgument, "alias"},
		{"SELECT _key AS k, COUNT(*) AS c FROM t GROUP BY k HAVING c > 1", codes.InvalidArgument, "having"},
		{"SELECT _key AS k, cf1['c1'] AS v, cf1['c2'] AS v FROM t ORDER BY k", codes.InvalidArgument, "duplicate"},
		{"SELECT _key AS k FROM t ORDER BY cf1['c1']", codes.InvalidArgument, "select list"},
		{"SELECT _key AS k, TIMESTAMP_SECONDS(1) AS _timestamp FROM t ORDER BY k, _timestamp", codes.InvalidArgument, "_timestamp"},
		{"SELECT _key AS k, 1 AS _timestamp FROM t ORDER BY k", codes.InvalidArgument, "_timestamp"},
		{"SELECT @p AS k FROM t ORDER BY k", codes.InvalidArgument, "query parameter"},
		{"SELECT _key AS k, COUNT(*) AS c FROM t ORDER BY k", codes.InvalidArgument, "neither grouped nor aggregated"},
		{"SELECT 1", codes.InvalidArgument, "group by"},
		{"SELECT _key AS k FROM t ORDER BY k UNION ALL SELECT 1", codes.InvalidArgument, "unexpected"},
		{"@{allow_incomplete_view=true} SELECT _key AS k FROM t ORDER BY k", codes.InvalidArgument, "hints"},
	} {
		t.Run(c.sql, func(t *testing.T) {
			_, _, err := PrepareMaterializedView(c.sql, cat)
			require.Error(t, err)
			st := status.Convert(err)
			require.Equal(t, c.code, st.Code(), err.Error())
			require.Contains(t, toLower(st.Message()), toLower(c.msg))
		})
	}
}

func TestQueryMaterializedViewThroughCatalog(t *testing.T) {
	base := mvTestCatalog()
	base.views["by_region"] = splitMV
	base.views["by_adv"] = "SELECT SPLIT(_key, '#')[SAFE_OFFSET(0)] AS advertiser_id, COUNT(*) AS n FROM ads GROUP BY advertiser_id"
	base.mvs = map[string]bool{"by_region": true, "by_adv": true}
	for name, cat := range map[string]Catalog{"with-kind": mvCatalog{base}, "heuristic": base} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, []string{`b"eu", b"i2"`, `b"eu", b"i4"`, `b"us", b"i3"`, `b"us", b"i1"`}, q(t, cat, "SELECT region, item_id FROM by_region"))
			require.Equal(t, []string{"region", "account_id", "ts", "typ", "item_id", "src_key", "cf1"}, colNames(prep(t, cat, "SELECT * FROM by_region")))
			require.Equal(t, []string{`4`}, q(t, cat, "SELECT COUNT(*) FROM by_region"))
			require.Equal(t, []string{`b"us"`}, q(t, cat, "SELECT region FROM by_region WHERE item_id = b'i1'"))
			keys := q(t, cat, "SELECT _key FROM by_region ORDER BY _key")
			require.Len(t, keys, 4)
		})
	}
	cat := mvCatalog{base}
	require.Equal(t, []string{`b"adv1", 2`, `b"adv2", 1`}, q(t, cat, "SELECT advertiser_id, n FROM by_adv"))
	_, rows, err := execQuery(cat, "SELECT _key, advertiser_id FROM by_adv", qopt{})
	require.NoError(t, err)
	_, def, err := PrepareMaterializedView(base.views["by_adv"], base)
	require.NoError(t, err)
	dec, err := DecodeKey(def.KeySchema, rows[0][0].GetBytesValue())
	require.NoError(t, err)
	require.Equal(t, []byte("adv1"), dec[0].GetBytesValue())
}

func toLower(s string) string { return string(bytes.ToLower([]byte(s))) }

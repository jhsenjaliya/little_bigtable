package gsql

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func pushdownCatalog() *memCatalog {
	c := newMemCatalog()
	tb := c.addTable(&Table{Name: "p", Families: []Family{{Name: "cf"}}})
	for i := 0; i < 200; i++ {
		v := "y"
		if i%7 == 0 {
			v = "x"
		}
		tb.setStr(fmt.Sprintf("k%03d", i), "cf", "v", 1, v)
	}
	for _, k := range []string{"", "\x00", "a", "k01", "k01\x00", "k01%", "\xff", "\xff\xff", "\xff\x00", "z\xff"} {
		if k == "" {
			continue
		}
		tb.setStr(k, "cf", "v", 1, "x")
	}
	return c
}

func TestPushdownPreservesResults(t *testing.T) {
	cat := pushdownCatalog()
	params := map[string]any{"p": "k010", "pre": "k1", "b": []byte("\xff"), "keys": []string{"k003", "k001", "zz"}}
	for _, where := range []string{
		"_key = 'k010'",
		"_key = b'k010'",
		"_key = @p",
		"_key IN ('k001', 'k150', 'zzz', NULL)",
		"_key IN UNNEST(@keys)",
		"_key NOT IN UNNEST(@keys)",
		"_key IN UNNEST(['k001', 'k002'])",
		"_key IN UNNEST(ARRAY<BYTES>[])",
		"_key NOT IN ('k001', 'k150')",
		"_key < 'k005'",
		"_key <= 'k005'",
		"_key > 'k195'",
		"_key >= 'k195'",
		"'k050' > _key",
		"'k050' <= _key AND _key < 'k060'",
		"_key BETWEEN 'k010' AND 'k020'",
		"_key BETWEEN 'k020' AND 'k010'",
		"_key NOT BETWEEN 'k010' AND 'k190'",
		"STARTS_WITH(_key, 'k01')",
		"STARTS_WITH(_key, @pre)",
		"STARTS_WITH(_key, @b)",
		"STARTS_WITH(_key, b'\\xff')",
		"STARTS_WITH(_key, '')",
		"_key LIKE 'k01%'",
		"_key LIKE 'k01_'",
		"_key LIKE 'k010'",
		"_key LIKE 'k01\\\\%'",
		"_key LIKE '%5'",
		"_key NOT LIKE 'k0%'",
		"_key = 'k010' OR _key = 'k020'",
		"(_key > 'k100' AND _key < 'k110') OR STARTS_WITH(_key, 'k19')",
		"_key >= 'k100' AND cf['v'] = 'x'",
		"_key > 'k100' OR cf['v'] = 'x'",
		"NOT (_key = 'k010')",
		"_key = NULL",
		"_key IN (NULL)",
		"_key = CONCAT('k', '010')",
		"_key > 'k150' AND _key > 'k100' AND _key < 'k160'",
		"_key = 'k010' AND _key = 'k011'",
		"(_key = 'k010' OR _key = 'k011') AND (_key = 'k011' OR _key = 'k012')",
		"_key < '' ",
		"_key >= b'\\xff' ",
		"_key > b'\\xff\\xff'",
		"_key = 'k010' OR TRUE",
		"_key = 'k010' AND FALSE",
		"IFNULL(_key = 'k010', FALSE)",
		"SAFE_CAST(_key AS STRING) = 'k010'",
	} {
		for _, suffix := range []string{"", " LIMIT 3", " ORDER BY _key LIMIT 2 OFFSET 1"} {
			sql := "SELECT _key, cf['v'] FROM p WHERE " + where + suffix
			_, withRows, err1 := execQuery(cat, sql, qopt{params: params})
			_, fullRows, err2 := execQuery(cat, sql, qopt{params: params, noPushdown: true})
			require.NoError(t, err1, sql)
			require.NoError(t, err2, sql)
			require.Equal(t, len(fullRows), len(withRows), sql)
			for i := range fullRows {
				require.Equal(t, fullRows[i][0].GetBytesValue(), withRows[i][0].GetBytesValue(), sql)
			}
		}
	}
}

func TestPushdownNarrowsScans(t *testing.T) {
	cat := pushdownCatalog()
	scans := func(where string, params map[string]any) []keyRange {
		cat.scans = nil
		_, _, err := execQuery(cat, "SELECT _key FROM p WHERE "+where, qopt{params: params})
		require.NoError(t, err)
		return cat.scans
	}
	require.Equal(t, []keyRange{{start: []byte("k010"), end: []byte("k010\x00")}}, scans("_key = 'k010'", nil))
	require.Equal(t, []keyRange{{start: []byte("k010"), end: []byte("k010\x00")}}, scans("_key = @p", map[string]any{"p": []byte("k010")}))
	require.Equal(t, []keyRange{{start: []byte("k01"), end: []byte("k02")}}, scans("STARTS_WITH(_key, 'k01')", nil))
	require.Equal(t, []keyRange{{start: []byte("k01"), end: []byte("k02")}}, scans("_key LIKE 'k01%' AND cf['v'] = 'x'", nil))
	require.Equal(t, []keyRange{{start: []byte("\xff")}}, scans("STARTS_WITH(_key, b'\\xff')", nil))
	require.Equal(t, []keyRange{{end: []byte("k005")}}, scans("_key < 'k005'", nil))
	require.Equal(t, []keyRange{{start: []byte("k195\x00")}}, scans("_key > 'k195'", nil))
	require.Equal(t, []keyRange{
		{start: []byte("k001"), end: []byte("k001\x00")},
		{start: []byte("k150"), end: []byte("k150\x00")},
	}, scans("_key IN ('k150', 'k001', NULL)", nil))
	require.Equal(t, []keyRange{{start: []byte("k010"), end: []byte("k020\x00")}}, scans("_key BETWEEN 'k010' AND 'k020'", nil))
	require.Equal(t, []keyRange{{start: []byte("k101"), end: []byte("k110")}}, scans("_key > 'k100' AND _key < 'k110' AND _key >= 'k101'", nil))
	require.Equal(t, []keyRange{
		{start: []byte("k001"), end: []byte("k001\x00")},
		{start: []byte("k003"), end: []byte("k003\x00")},
	}, scans("_key IN UNNEST(@keys)", map[string]any{"keys": []string{"k003", "k001"}}))
	require.Equal(t, []keyRange{{}}, scans("_key NOT IN UNNEST(@keys)", map[string]any{"keys": []string{"k003"}}))
	require.Empty(t, scans("_key = NULL", nil))
	require.Empty(t, scans("_key = 'a' AND _key = 'b'", nil))
	require.Equal(t, []keyRange{{}}, scans("_key > 'k100' OR cf['v'] = 'x'", nil))
	require.Equal(t, []keyRange{{}}, scans("NOT (_key = 'k010')", nil))
	require.Equal(t, []keyRange{{}}, scans("cf['v'] = 'x'", nil))
}

func TestRangeHelpers(t *testing.T) {
	require.Equal(t, []byte("ab"), prefixSuccessor([]byte("aa\xff")))
	require.Nil(t, prefixSuccessor([]byte("\xff\xff")))
	require.Nil(t, prefixSuccessor(nil))
	got := normalizeRanges([]keyRange{
		{start: []byte("c"), end: []byte("d")},
		{start: []byte("a"), end: []byte("b")},
		{start: []byte("b"), end: []byte("c")},
		{start: []byte("x"), end: []byte("x")},
		{start: []byte("y")},
		{start: []byte("z"), end: []byte("zz")},
	})
	require.Equal(t, []keyRange{{start: []byte("a"), end: []byte("d")}, {start: []byte("y")}}, got)
	require.Equal(t, []keyRange{{start: []byte("b"), end: []byte("c")}},
		intersectRanges([]keyRange{{start: []byte("a"), end: []byte("c")}}, []keyRange{{start: []byte("b")}}))
}

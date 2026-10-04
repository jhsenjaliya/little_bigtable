package gsql

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// salesCatalog: table sales with family cf (region, amount, flag) and an
// INT64 aggregate family cnt.
func salesCatalog() *memCatalog {
	c := newMemCatalog()
	s := c.addTable(&Table{Name: "sales", Families: []Family{{Name: "cf"}, {Name: "cnt", Kind: FamilyInt64Aggregate}}})
	data := []struct {
		key, region, amount, flag string
	}{
		{"k1", "us", "10", "true"},
		{"k2", "us", "20", "false"},
		{"k3", "eu", "5", "true"},
		{"k4", "eu", "", "true"},
		{"k5", "apac", "7", ""},
		{"k6", "", "3", "false"},
	}
	for i, d := range data {
		if d.region != "" {
			s.setStr(d.key, "cf", "region", 1, d.region)
		}
		if d.amount != "" {
			s.setStr(d.key, "cf", "amount", 1, d.amount)
		}
		if d.flag != "" {
			s.setStr(d.key, "cf", "flag", 1, d.flag)
		}
		s.setInt(d.key, "cnt", "n", 1, int64(i+1))
	}
	n := c.addTable(&Table{Name: "nums", Families: []Family{{Name: "v", Kind: FamilyInt64Aggregate}}})
	for i, x := range []int64{2, 4, 4, 4, 5, 5, 7, 9} {
		n.setInt("n"+strconv.Itoa(i), "v", "x", 1, x)
		n.setInt("n"+strconv.Itoa(i), "v", "y", 1, 2*x+1)
	}
	return c
}

const amt = "CAST(CAST(cf['amount'] AS STRING) AS INT64)"
const region = "CAST(cf['region'] AS STRING)"

func TestAggregatesWholeTable(t *testing.T) {
	cat := salesCatalog()
	require.Equal(t, []string{`6, 5, 45, 9.0, 3, 20, 5`},
		q(t, cat, "SELECT COUNT(*), COUNT(cf['amount']), SUM("+amt+"), AVG("+amt+"), MIN("+amt+"), MAX("+amt+"), COUNT(DISTINCT cf['amount']) FROM sales"))
	require.Equal(t, []string{`0, NULL, NULL, NULL, 0`},
		q(t, cat, "SELECT COUNT(*), SUM("+amt+"), AVG("+amt+"), MAX("+amt+"), COUNTIF(TRUE) FROM sales WHERE FALSE"))
	require.Empty(t, q(t, cat, "SELECT COUNT(*) FROM sales WHERE FALSE GROUP BY _key"))
	require.Equal(t, []string{`3, 3`}, q(t, cat, "SELECT COUNT(DISTINCT cf['region']), COUNTIF("+amt+" > 6) FROM sales"))
	require.Equal(t, []string{`false, true, true`},
		q(t, cat, "SELECT LOGICAL_AND(CAST(CAST(cf['flag'] AS STRING) AS BOOL)), LOGICAL_OR(CAST(CAST(cf['flag'] AS STRING) AS BOOL)), ANY_VALUE(cf['region']) IS NOT NULL FROM sales"))
	require.Equal(t, []string{`0, 7, 7, 21`},
		q(t, cat, "SELECT BIT_AND(cnt['n']), BIT_OR(cnt['n']), BIT_XOR(cnt['n']), SUM(cnt['n']) FROM sales"))
	require.Equal(t, []string{`[20, 10], [10, 20, 5, 7, 3], [10, 20, 5, NULL, 7, 3]`},
		q(t, cat, "SELECT ARRAY_AGG("+amt+" ORDER BY "+amt+" DESC LIMIT 2), ARRAY_AGG("+amt+" IGNORE NULLS), ARRAY_AGG("+amt+") FROM sales"))
	require.Equal(t, []string{`[3, 5, 7, 10, 20], [7, 5, 10, 20, 3]`},
		q(t, cat, "SELECT ARRAY_AGG(DISTINCT "+amt+" IGNORE NULLS ORDER BY "+amt+"), ARRAY_AGG(DISTINCT "+amt+" ORDER BY "+region+" NULLS LAST) FROM sales WHERE cf['amount'] IS NOT NULL"))
	require.Equal(t, []string{`"apac|eu|eu|us|us", "us,us,eu,eu,apac", b"apac-eu"`},
		q(t, cat, "SELECT STRING_AGG("+region+", '|' ORDER BY "+region+"), STRING_AGG("+region+"), STRING_AGG(DISTINCT cf['region'], b'-' ORDER BY cf['region'] LIMIT 2) FROM sales"))
	require.Equal(t, []string{`[1, 10, 2, 20]`}, q(t, cat, "SELECT ARRAY_CONCAT_AGG([cnt['n'], cnt['n'] * 10] ORDER BY _key LIMIT 2) FROM sales"))
	require.Equal(t, []string{`3, 3`}, q(t, cat, "SELECT APPROX_COUNT_DISTINCT(cf['region']), HLL_COUNT.EXTRACT(HLL_COUNT.INIT(cf['region'])) FROM sales"))
	_, rows, err := execQuery(cat, "SELECT VAR_POP(v['x']), STDDEV_POP(v['x']), VAR_SAMP(v['x']), STDDEV(v['x']), VARIANCE(v['x']), CORR(v['x'], v['y']), COVAR_POP(v['x'], v['y']), COVAR_SAMP(v['x'], v['y']), STDDEV_SAMP(v['x']) FROM nums", qopt{})
	require.NoError(t, err)
	want := []float64{4, 2, 32.0 / 7, 2.138089935299395, 32.0 / 7, 1, 8, 64.0 / 7, 2.138089935299395}
	for i, w := range want {
		require.InDelta(t, w, rows[0][i].GetFloatValue(), 1e-9, "column %d", i)
	}
	require.Equal(t, []string{`NULL, NULL`}, q(t, cat, "SELECT VAR_SAMP(v['x']), CORR(v['x'], v['y']) FROM nums WHERE _key = 'n0'"))
	require.Equal(t, []string{`"apac", "us", b"apac"`}, q(t, cat, "SELECT MIN("+region+"), MAX("+region+"), MIN(cf['region']) FROM sales"))
	require.Equal(t, []string{`[10, 20, 5, 7, 3]`}, q(t, cat, "SELECT ARRAY_AGG("+amt+" IGNORE NULLS ORDER BY _key) FROM sales"))
}

func TestGroupByHaving(t *testing.T) {
	cat := salesCatalog()
	require.ElementsMatch(t, []string{`"us", 2, 30`, `"eu", 2, 5`, `"apac", 1, 7`, `NULL, 1, 3`},
		q(t, cat, "SELECT "+region+" AS region, COUNT(*) AS n, SUM("+amt+") AS total FROM sales GROUP BY region"))
	require.ElementsMatch(t, []string{`"us", 2`, `"eu", 2`},
		q(t, cat, "SELECT "+region+" AS region, COUNT(*) AS n FROM sales GROUP BY region HAVING COUNT(*) > 1"))
	require.ElementsMatch(t, []string{`"us", 2`, `"eu", 2`},
		q(t, cat, "SELECT "+region+" AS region, COUNT(*) AS n FROM sales GROUP BY 1 HAVING n > 1"))
	require.ElementsMatch(t, []string{`"us", 2`, `"eu", 2`, `"apac", 1`, `NULL, 1`},
		q(t, cat, "SELECT "+region+", COUNT(*) FROM sales GROUP BY $col1"))
	require.ElementsMatch(t, []string{`"US", 30`, `"EU", 5`, `"APAC", 7`, `NULL, 3`},
		q(t, cat, "SELECT UPPER("+region+"), SUM("+amt+") FROM sales GROUP BY "+region))
	require.ElementsMatch(t, []string{`b"us", true`, `b"eu", true`, `b"apac", false`, `NULL, false`},
		q(t, cat, "SELECT cf['region'], COUNT(*) > 1 FROM sales GROUP BY cf['region']"))
	require.ElementsMatch(t, []string{`"u", 2`, `"e", 2`, `"a", 1`, `NULL, 1`},
		q(t, cat, "SELECT SUBSTR("+region+", 0, 1) AS first_letter, COUNT(*) AS count FROM sales GROUP BY first_letter"))
	require.Equal(t, []string{`6`}, q(t, cat, "SELECT COUNT(*) AS c FROM sales HAVING c > 1"))
	require.Empty(t, q(t, cat, "SELECT COUNT(*) AS c FROM sales HAVING c > 100"))
	require.Equal(t, []string{`b"k1", 1`, `b"k2", 1`}, q(t, cat, "SELECT _key, COUNT(*) FROM sales GROUP BY _key ORDER BY _key LIMIT 2"))
	require.ElementsMatch(t, []string{`"us"`, `"eu"`, `"apac"`, `NULL`},
		q(t, cat, "SELECT DISTINCT "+region+" FROM sales"))

	qErr(t, cat, "SELECT "+region+", "+amt+" FROM sales GROUP BY 1", codes.InvalidArgument, "neither grouped nor aggregated")
	qErr(t, cat, "SELECT _key FROM sales HAVING _key > 'a'", codes.InvalidArgument, "having clause requires group by")
	qErr(t, cat, "SELECT COUNT(*) FROM sales GROUP BY COUNT(*)", codes.InvalidArgument, "group by clause")
	qErr(t, cat, "SELECT COUNT(*) AS c FROM sales GROUP BY c", codes.InvalidArgument, "aggregate")
	qErr(t, cat, "SELECT COUNT(*) FROM sales GROUP BY [1]", codes.InvalidArgument, "grouping by expressions of type array")
	qErr(t, cat, "SELECT COUNT(*) FROM sales GROUP BY 5", codes.InvalidArgument, "out of range")
	qErr(t, cat, "SELECT COUNT(DISTINCT [1]) FROM sales", codes.InvalidArgument, "distinct")
	qErr(t, cat, "SELECT SUM('a') FROM sales", codes.InvalidArgument, "no matching signature")
	qErr(t, cat, "SELECT ARRAY_AGG([1]) FROM sales", codes.InvalidArgument, "array of arrays")
	qErr(t, cat, "SELECT SUM(x ORDER BY x) FROM (SELECT 1 AS x)", codes.InvalidArgument, "")
	qErr(t, cat, "SELECT SUM(cnt['n'] ORDER BY _key) FROM sales", codes.InvalidArgument, "order by")
	qErr(t, cat, "SELECT SUM(9223372036854775807) FROM sales", codes.OutOfRange, "overflow")
	qErr(t, cat, "SELECT HLL_COUNT.INIT(cf['region'], 30) FROM sales", codes.InvalidArgument, "precision")
	qErr(t, cat, "SELECT HLL_COUNT.MERGE(cf['region']) FROM sales", codes.OutOfRange, "sketch")
}

func TestHLLFunctionsSQL(t *testing.T) {
	c := newMemCatalog()
	tb := c.addTable(&Table{Name: "u", Families: []Family{{Name: "cf"}, {Name: "h", Kind: FamilyHLLAggregate}}})
	for i := 0; i < 50; i++ {
		day := "d1"
		if i >= 30 {
			day = "d2"
		}
		key := "e" + strconv.Itoa(i)
		tb.setStr(key, "cf", "day", 1, day)
		tb.setStr(key, "cf", "user", 1, "user"+strconv.Itoa(i%40))
	}
	h := NewHLL()
	for i := 0; i < 25; i++ {
		h.Add([]byte("x" + strconv.Itoa(i)))
	}
	tb.set("sketch", "h", "users", 1, h.Encode())
	require.Equal(t, []string{`40`}, q(t, c, "SELECT HLL_COUNT.EXTRACT(HLL_COUNT.INIT(cf['user'])) FROM u"))
	require.Equal(t, []string{`40`}, q(t, c, "SELECT HLL_COUNT.EXTRACT(HLL_COUNT.INIT(CAST(cf['user'] AS STRING), 12)) FROM u"))
	require.Equal(t, []string{`25`}, q(t, c, "SELECT HLL_COUNT.EXTRACT(h['users']) FROM u WHERE _key = 'sketch'"))
	require.Equal(t, []string{`25, 25`}, q(t, c, "SELECT HLL_COUNT.MERGE(h['users']), HLL_COUNT.EXTRACT(HLL_COUNT.MERGE_PARTIAL(h['users'])) FROM u"))
	require.ElementsMatch(t, []string{`b"d1", 30`, `b"d2", 20`},
		q(t, c, "SELECT cf['day'], HLL_COUNT.EXTRACT(HLL_COUNT.INIT(cf['user'])) FROM u WHERE cf['day'] IS NOT NULL GROUP BY cf['day']"))
	require.Equal(t, []string{`NULL, 0`}, q(t, c, "SELECT HLL_COUNT.INIT(cf['user']), HLL_COUNT.MERGE(h['users']) FROM u WHERE FALSE"))
}

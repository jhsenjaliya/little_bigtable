package gsql

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

const nExpr = "SAFE_CAST(CAST(cf2['n'] AS STRING) AS INT64)"

func TestOrderingAcrossTypes(t *testing.T) {
	cat, _ := stdCatalog()
	require.Equal(t, []string{`DATE "1970-01-04", DATE "1970-01-12", TIMESTAMP "1970-01-01 00:00:03+00", TIMESTAMP "1970-01-01 00:00:11+00", false, true, 1.5, 5.5, "11", "7"`},
		q(t, cat, "SELECT MIN(DATE_FROM_UNIX_DATE("+nExpr+")), MAX(DATE_FROM_UNIX_DATE("+nExpr+")), "+
			"MIN(TIMESTAMP_SECONDS("+nExpr+")), MAX(TIMESTAMP_SECONDS("+nExpr+")), MIN("+nExpr+" > 5), MAX("+nExpr+" > 5), "+
			"MIN(CAST("+nExpr+" AS FLOAT64) / 2), MAX(CAST("+nExpr+" AS FLOAT64) / 2), "+
			"MIN(CAST(cf2['n'] AS STRING)), MAX(CAST(cf2['n'] AS STRING)) FROM t"))
	require.ElementsMatch(t, []string{`true, 2`, `false, 2`, `NULL, 2`}, q(t, cat, "SELECT "+nExpr+" > 5 AS big, COUNT(*) FROM t GROUP BY big"))
	require.ElementsMatch(t, []string{`DATE "1970-01-06", 1`, `DATE "1970-01-08", 1`, `DATE "1970-01-12", 1`, `DATE "1970-01-04", 1`, `NULL, 2`},
		q(t, cat, "SELECT DATE_FROM_UNIX_DATE("+nExpr+") AS d, COUNT(*) FROM t GROUP BY d"))
	require.Equal(t, []string{`5`}, q(t, cat, "SELECT COUNT(DISTINCT TIMESTAMP_SECONDS(IFNULL("+nExpr+", 0))) FROM t"))
	require.Equal(t, []string{`CAST("nan" AS FLOAT64), 6`}, q(t, cat, "SELECT IEEE_DIVIDE(0, 0) AS x, COUNT(*) FROM t GROUP BY x"))
	require.Equal(t, []string{`2`}, q(t, cat, "SELECT COUNT(DISTINCT IF(_key < 'b', 0.0, -0.0)) + 1 FROM t"), "0.0 and -0.0 are one distinct value")
	require.Len(t, q(t, cat, "SELECT cf2, COUNT(*) FROM t GROUP BY cf2"), 5)
	require.Len(t, q(t, cat, "SELECT DISTINCT cf2 FROM t"), 5)
	require.Equal(t, []string{`true`, `true`, `true`, `true`, `true`, `NULL`}, q(t, cat, "SELECT cf1 = cf1 FROM t"))
	require.Equal(t, []string{`false`}, q(t, cat, "SELECT cf1 = cf2 FROM t WHERE _key = 'a#01'"))
	require.Equal(t, []string{`true, false`}, q(t, cat, "SELECT ARRAY_INCLUDES([STRUCT(1 AS a, 'x' AS b)], STRUCT(1, 'x')), ARRAY_INCLUDES([STRUCT(1 AS a, 'x' AS b)], STRUCT(2, 'x'))"))
	require.Equal(t, []string{`[(b"c1", b"xyz"), (b"c2", b"nop")]`}, q(t, cat, "SELECT MAP_ENTRIES(cf1) FROM t WHERE _key = 'a#01'"))
	require.Equal(t, []string{`"{\"YzE=\":\"eHl6\",\"YzI=\":\"bm9w\"}", "{c1: xyz, c2: nop}"`},
		q(t, cat, "SELECT TO_JSON_STRING(cf1), FORMAT('%t', cf1) FROM t WHERE _key = 'a#01'"))
}

func TestUnaryAndTypeNames(t *testing.T) {
	runExprCases(t, []exprCase{
		{`-CAST(1.5 AS FLOAT32)`, `-1.5`},
		{`+5`, `5`},
		{`-NULL`, `NULL`},
		{`~NULL`, `NULL`},
		{`NOT CAST(NULL AS BOOL)`, `NULL`},
		{`-(1)`, `-1`},
		{`- -1`, `1`},
		{`-[1, 2][OFFSET(0)]`, `-1`},
		{`CAST(STRUCT(1, ['a']) AS STRUCT<a INT64, b ARRAY<STRING>>).b`, `["a"]`},
		{`CAST(1 AS BOOLEAN)`, `true`},
		{`CAST('1' AS BIGINT)`, `1`},
		{`CAST(1 AS DOUBLE)`, `1.0`},
		{`CAST([] AS ARRAY<STRING>)`, `[]`},
		{`ARRAY<STRUCT<x INT64>>[STRUCT(1), STRUCT(2)]`, `[(1), (2)]`},
		{`ARRAY<STRUCT<a INT64, b STRING>>[(1, 'x')][OFFSET(0)].b`, `"x"`},
	})
	cat := newMemCatalog()
	qErr(t, cat, "SELECT -'a'", codes.InvalidArgument, "unary operator -")
	qErr(t, cat, "SELECT MAX([1])", codes.InvalidArgument, "array")
	qErr(t, cat, "SELECT ARRAY<STRUCT<x INT64>>[(1)]", codes.InvalidArgument, "coerce")
	qErr(t, cat, "SELECT ~1.5", codes.InvalidArgument, "~")
	qErr(t, cat, "SELECT +'a'", codes.InvalidArgument, "unary operator +")
	qErr(t, cat, "SELECT CAST(NULL AS MAP<STRING, INT64>)", codes.InvalidArgument, "map")
	qErr(t, cat, "SELECT CAST(1 AS STRING(10))", codes.Unimplemented, "parameterized")
	qErr(t, cat, "SELECT CAST(1 AS FOO)", codes.InvalidArgument, "type not found")
	qErr(t, cat, "SELECT CAST(1 AS ARRAY<ARRAY<INT64>>)", codes.InvalidArgument, "arrays of arrays")
	qErr(t, cat, "SELECT STRUCT<a INT64>(1, 2)", codes.InvalidArgument, "fields")
	qErr(t, cat, "SELECT STRUCT<a INT64>(1 AS a)", codes.InvalidArgument, "as aliases")
	qErr(t, cat, "SELECT [1, 'a']", codes.InvalidArgument, "supertype")
	qErr(t, cat, "SELECT 1 << 'a'", codes.InvalidArgument, "shift")
	qErr(t, cat, "SELECT 1 & 'a'", codes.InvalidArgument, "operator &")
	qErr(t, cat, "SELECT b'a' & b'ab'", codes.OutOfRange, "equal length")
	qErr(t, cat, "SELECT 1 << -1", codes.OutOfRange, "negative")
	qErr(t, cat, "SELECT DATE '2020-01-01' - DATE '2020-01-01'", codes.Unimplemented, "interval")
	qErr(t, cat, "SELECT CASE 1 WHEN 'a' THEN 1 END", codes.InvalidArgument, "")
	qErr(t, cat, "SELECT CASE WHEN 1 THEN 1 END", codes.InvalidArgument, "bool")
	qErr(t, cat, "SELECT IF(1, 2, 3)", codes.InvalidArgument, "no matching signature")
	qErr(t, cat, "SELECT COALESCE(1, 'a')", codes.InvalidArgument, "no matching signature")
	qErr(t, cat, "SELECT [1][KEY(1)]", codes.InvalidArgument, "key")
	qErr(t, cat, "SELECT EXTRACT(HOUR FROM DATE '2020-01-01')", codes.InvalidArgument, "date part")
	qErr(t, cat, "SELECT EXTRACT(DAY FROM 1)", codes.InvalidArgument, "extract")
	qErr(t, cat, "SELECT EXTRACT(TIME FROM TIMESTAMP '2020-01-01 00:00:00+00')", codes.Unimplemented, "time")
}

package gsql

import (
	"context"
	"testing"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func colNames(p *Prepared) []string {
	var out []string
	for _, c := range p.Metadata().GetProtoSchema().GetColumns() {
		out = append(out, c.GetName())
	}
	return out
}

func colTypes(t *testing.T, p *Prepared) []string {
	var out []string
	for _, c := range p.Metadata().GetProtoSchema().GetColumns() {
		typ, err := typeFromProto(c.GetType())
		require.NoError(t, err)
		out = append(out, typ.String())
	}
	return out
}

func prep(t *testing.T, cat Catalog, sql string) *Prepared {
	t.Helper()
	p, err := Prepare(sql, nil, cat)
	require.NoError(t, err, sql)
	return p
}

func TestSelectStar(t *testing.T) {
	cat, _ := stdCatalog()
	p := prep(t, cat, "SELECT * FROM t")
	require.Equal(t, []string{"_key", "cf1", "cf2", "agg", "hll"}, colNames(p))
	require.Equal(t, []string{"BYTES", "MAP<BYTES, BYTES>", "MAP<BYTES, BYTES>", "MAP<BYTES, INT64>", "MAP<BYTES, BYTES>"}, colTypes(t, p))
	rows := q(t, cat, "SELECT * FROM t")
	require.Equal(t, []string{
		`b"a#01", {b"c1": b"xyz", b"c2": b"nop"}, {b"n": b"5"}, NULL, NULL`,
		`b"a#02", {b"c1": b"zyx"}, {b"n": b"7"}, NULL, NULL`,
		`b"a#03", {b"c1": b"abc", b"c2": b"def"}, NULL, NULL, NULL`,
		`b"b#01", {b"c1": b"jkl"}, {b"n": b"11"}, {b"sum": 42}, NULL`,
		`b"b#02", {b"c1": b"howdy"}, NULL, NULL, NULL`,
		`b"c#03", NULL, {b"n": b"3"}, NULL, NULL`,
	}, rows)
	require.Equal(t, []string{`b"a#01", {b"c1": b"xyz", b"c2": b"nop"}`},
		q(t, cat, "SELECT * EXCEPT (cf2, agg, hll) FROM t WHERE _key = 'a#01'"))
	require.Equal(t, []string{`b"a#01", {b"c1": b"xyz", b"c2": b"nop"}, b"5"`},
		q(t, cat, "SELECT * EXCEPT (agg, hll) REPLACE (cf2['n'] AS cf2) FROM t WHERE _key = 'a#01'"))
	require.Equal(t, rows, q(t, cat, "SELECT t.* FROM t"))
	require.Equal(t, rows, q(t, cat, "SELECT x.* FROM t AS x"))
	require.Equal(t, rows, q(t, cat, "SELECT x.* FROM t x"))
	require.Equal(t, []string{`1, "x"`}, q(t, cat, "SELECT STRUCT(1 AS a, 'x' AS b).*"))
	qErr(t, cat, "SELECT * EXCEPT (nope) FROM t", codes.InvalidArgument, "does not exist")
	qErr(t, cat, "SELECT * REPLACE (1 AS nope) FROM t", codes.InvalidArgument, "does not exist")
	qErr(t, cat, "SELECT *", codes.InvalidArgument, "from clause")
	qErr(t, cat, "SELECT * FROM missing", codes.NotFound, "missing")
}

func TestColumnsAndAliases(t *testing.T) {
	cat, _ := stdCatalog()
	p := prep(t, cat, "SELECT _key, cf1['c1'], cf1['c1'] AS c, t.cf2, UPPER('x'), STRUCT(1 AS f).f FROM t")
	require.Equal(t, []string{"_key", "", "c", "cf2", "", "f"}, colNames(p))
	require.Equal(t, []string{"BYTES", "BYTES", "BYTES", "MAP<BYTES, BYTES>", "STRING", "INT64"}, colTypes(t, p))
	qErr(t, cat, "SELECT k FROM t AS k", codes.Unimplemented, "range variable")
	require.Equal(t, []string{`b"xyz"`}, q(t, cat, "SELECT CF1['c1'] FROM t WHERE _KEY = 'a#01'"))
	require.Equal(t, []string{`b"xyz"`}, q(t, cat, "SELECT x.cf1['c1'] FROM t AS x WHERE x._key = 'a#01'"))
	require.Equal(t, []string{`1, 1`}, q(t, cat, "SELECT 1 AS a, 1 AS a"))
	qErr(t, cat, "SELECT nope FROM t", codes.InvalidArgument, "unrecognized name: nope")
	qErr(t, cat, "SELECT t.nope FROM t", codes.InvalidArgument, "nope")
	qErr(t, cat, "SELECT cf1.c1 FROM t", codes.InvalidArgument, "subscript")
}

func TestWhereAndNullSemantics(t *testing.T) {
	cat, _ := stdCatalog()
	require.Equal(t, []string{`b"a#03"`, `b"b#02"`}, q(t, cat, "SELECT _key FROM t WHERE cf2['n'] IS NULL"))
	require.Equal(t, []string{`b"c#03"`}, q(t, cat, "SELECT _key FROM t WHERE cf1 IS NULL"))
	require.Equal(t, []string{`b"a#01"`}, q(t, cat, "SELECT _key FROM t WHERE cf1['c2'] = 'nop'"))
	require.Equal(t, []string{`b"a#03"`}, q(t, cat, "SELECT _key FROM t WHERE NOT (cf1['c2'] = 'nop')"))
	require.Equal(t, []string{`b"a#02", "7"`, `b"b#01", "11"`},
		q(t, cat, "SELECT _key, CAST(cf2['n'] AS STRING) FROM t WHERE SAFE_CAST(CAST(cf2['n'] AS STRING) AS INT64) > 5"))
	require.Equal(t, []string{`[b"c1", b"c2"], [b"xyz", b"nop"], true, false, 2, b"c1", b"nop"`},
		q(t, cat, "SELECT MAP_KEYS(cf1), MAP_VALUES(cf1), MAP_CONTAINS_KEY(cf1, 'c2'), MAP_EMPTY(cf1), ARRAY_LENGTH(MAP_ENTRIES(cf1)), MAP_ENTRIES(cf1)[OFFSET(0)].key, MAP_ENTRIES(cf1)[OFFSET(1)].value FROM t WHERE _key = 'a#01'"))
	require.Equal(t, []string{`NULL`}, q(t, cat, "SELECT cf1['missing'] FROM t WHERE _key = 'a#01'"))
	require.Equal(t, []string{`42, 43`}, q(t, cat, "SELECT agg['sum'], agg['sum'] + 1 FROM t WHERE _key = 'b#01'"))
	require.Equal(t, []string{`b"b#01"`}, q(t, cat, "SELECT _key FROM t WHERE agg['sum'] > 40"))
	require.Equal(t, []string{`"xyz"`}, q(t, cat, "SELECT CAST(cf1['c1'] AS STRING) FROM t WHERE _key = 'a#01' AND cf1['c1'] LIKE 'x%'"))
	require.Equal(t, []string{`b"a#01"`, `b"a#02"`},
		q(t, cat, "SELECT _key FROM t WHERE STARTS_WITH(_key, 'a') AND cf2 IS NOT NULL"))
	qErr(t, cat, "SELECT _key FROM t WHERE 1", codes.InvalidArgument, "bool")
	qErr(t, cat, "SELECT _key FROM t WHERE 1/0 = 1", codes.OutOfRange, "division by zero")
}

func TestOrderLimitDistinct(t *testing.T) {
	cat, _ := stdCatalog()
	require.Equal(t, []string{`b"a#01"`, `b"a#02"`}, q(t, cat, "SELECT _key FROM t ORDER BY _key LIMIT 2"))
	require.Equal(t, []string{`b"b#01"`, `b"b#02"`}, q(t, cat, "SELECT _key FROM t ORDER BY _key ASC LIMIT 2 OFFSET 3"))
	require.Empty(t, q(t, cat, "SELECT _key FROM t LIMIT 0"))
	require.Equal(t, []string{`b"a#02"`}, q(t, cat, "SELECT _key FROM t LIMIT @n OFFSET @o", map[string]any{"n": int64(1), "o": int64(1)}))
	require.Equal(t, []string{`b"a#01"`}, q(t, cat, "SELECT _key FROM t ORDER BY 1 LIMIT 1"))
	require.Equal(t, []string{`b"a#01"`}, q(t, cat, "SELECT _key AS k FROM t ORDER BY k LIMIT 1"))
	require.Equal(t, []string{`b"a#01"`}, q(t, cat, "SELECT _key FROM t ORDER BY t._key LIMIT 1"))
	require.Equal(t, []string{`b"a#01"`}, q(t, cat, "(SELECT _key FROM t) ORDER BY _key LIMIT 1"))
	require.Equal(t, []string{`b"a#02"`}, q(t, cat, "(SELECT _key FROM t LIMIT 3) ORDER BY _key LIMIT 1 OFFSET 1"))
	require.Equal(t, []string{`b"a#01", 1`, `b"a#02", 1`}, q(t, cat, "SELECT _key, COUNT(*) FROM t GROUP BY _key ORDER BY _key LIMIT 2"))
	require.Equal(t, []string{`"a"`, `"b"`, `"c"`}, q(t, cat, "SELECT DISTINCT SUBSTR(CAST(_key AS STRING), 1, 1) FROM t"))
	require.Equal(t, []string{`"a"`, `"b"`}, q(t, cat, "SELECT DISTINCT SUBSTR(CAST(_key AS STRING), 1, 1) FROM t LIMIT 2"))
	qErr(t, cat, "SELECT _key FROM t ORDER BY _key DESC", codes.InvalidArgument, "order by _key")
	qErr(t, cat, "SELECT _key FROM t ORDER BY cf1['c1']", codes.InvalidArgument, "order by _key")
	qErr(t, cat, "SELECT _key FROM t ORDER BY _key, _key", codes.InvalidArgument, "order by _key")
	qErr(t, cat, "SELECT _key FROM t ORDER BY _key NULLS LAST", codes.InvalidArgument, "nulls")
	qErr(t, cat, "SELECT _key FROM t LIMIT -1", codes.InvalidArgument, "non-negative")
	qErr(t, cat, "SELECT _key FROM t LIMIT 'a'", codes.InvalidArgument, "limit")
	qErr(t, cat, "SELECT _key FROM t LIMIT NULL", codes.InvalidArgument, "non-null")
	qErr(t, cat, "SELECT _key FROM t LIMIT cf1['x']", codes.InvalidArgument, "unrecognized name")
	qErr(t, cat, "SELECT _key FROM t LIMIT @n", codes.InvalidArgument, "non-negative", map[string]any{"n": int64(-2)})
	qErr(t, cat, "SELECT DISTINCT [1] FROM t", codes.InvalidArgument, "select distinct")
	qErr(t, cat, "SELECT DISTINCT cf1['c1'] FROM t ORDER BY _key", codes.InvalidArgument, "distinct")

	// LIMIT stops the scan early when no sort is needed.
	cat.visited = 0
	_, rows, err := execQuery(cat, "SELECT _key FROM t ORDER BY _key LIMIT 2", qopt{})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, 2, cat.visited)
}

func TestParameters(t *testing.T) {
	cat, _ := stdCatalog()
	require.Equal(t, []string{`b"a#02"`}, q(t, cat, "SELECT _key FROM t WHERE _key = @k", map[string]any{"k": []byte("a#02")}))
	require.Equal(t, []string{`b"a#02"`}, q(t, cat, "SELECT _key FROM t WHERE _key = @k", map[string]any{"k": "a#02"}))
	require.Equal(t, []string{`b"zyx"`}, q(t, cat, "SELECT cf1[@q] FROM t WHERE _key = @k", map[string]any{"k": "a#02", "q": "c1"}))
	now := time.Date(2020, 1, 2, 3, 4, 5, 6000, time.UTC)
	require.Equal(t, []string{`2, "s", b"b", true, 1.5, TIMESTAMP "2020-01-02 03:04:05.000006+00"`},
		q(t, cat, "SELECT @i + 1, @s, @b, @bo, @f, @ts", map[string]any{
			"i": int64(1), "s": "s", "b": []byte("b"), "bo": true, "f": 1.5, "ts": now}))
	require.Equal(t, []string{`b"a#01"`, `b"b#02"`},
		q(t, cat, "SELECT _key FROM t WHERE CAST(_key AS STRING) IN UNNEST(@keys)", map[string]any{"keys": []string{"a#01", "b#02", "zz"}}))
	require.Equal(t, []string{`b"a#02"`}, q(t, cat, "SELECT _key FROM t WHERE _key = @K", map[string]any{"k": "a#02"}))
	qErr(t, cat, "SELECT @nope", codes.InvalidArgument, "query parameter 'nope' not found")

	p, err := Prepare("SELECT @i IS NULL, @i", map[string]*btpb.Type{"i": typInt64.toProto()}, cat)
	require.NoError(t, err)
	require.Len(t, p.ParamTypes(), 1)
	run := func(params map[string]*btpb.Value) ([][]*btpb.Value, error) {
		var rows [][]*btpb.Value
		err := p.Execute(context.Background(), cat, params, func(r []*btpb.Value) error {
			rows = append(rows, r)
			return nil
		})
		return rows, err
	}
	rows, err := run(map[string]*btpb.Value{"i": {}})
	require.NoError(t, err)
	require.Equal(t, []string{`true, NULL`}, render(t, p.Metadata(), rows))
	_, err = run(nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "not bound")
	_, err = run(map[string]*btpb.Value{"i": {Kind: &btpb.Value_StringValue{StringValue: "x"}}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = run(map[string]*btpb.Value{"i": {Type: typString.toProto(), Kind: &btpb.Value_IntValue{IntValue: 1}}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = run(map[string]*btpb.Value{"i": {Kind: &btpb.Value_IntValue{IntValue: 1}}, "extra": {}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	rows, err = run(map[string]*btpb.Value{"i": {Type: typInt64.toProto(), Kind: &btpb.Value_IntValue{IntValue: 7}}})
	require.NoError(t, err)
	require.Equal(t, []string{`false, 7`}, render(t, p.Metadata(), rows))

	// Array parameter elements must not carry types.
	pa, err := Prepare("SELECT ARRAY_LENGTH(@a)", map[string]*btpb.Type{"a": arrayOf(typInt64).toProto()}, cat)
	require.NoError(t, err)
	err = pa.Execute(context.Background(), cat, map[string]*btpb.Value{"a": {Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{
		Values: []*btpb.Value{{Type: typInt64.toProto(), Kind: &btpb.Value_IntValue{IntValue: 1}}}}}}}, func([]*btpb.Value) error { return nil })
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = Prepare("SELECT 1", map[string]*btpb.Type{"p": {}}, cat)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestTemporalArguments(t *testing.T) {
	cat, _ := stdCatalog()
	require.Equal(t, []string{`b"howdy"`}, q(t, cat, "SELECT cf1['c1'] FROM t WHERE _key = 'b#02'"))
	require.Equal(t, []string{`b"hello"`}, q(t, cat, "SELECT cf1['c1'] FROM t(as_of => TIMESTAMP_SECONDS(250)) WHERE _key = 'b#02'"))
	require.Equal(t, []string{`b"hello"`}, q(t, cat, "SELECT cf1['c1'] FROM t(as_of => @ts) WHERE _key = 'b#02'",
		map[string]any{"ts": time.Unix(200, 0)}))
	require.Empty(t, q(t, cat, "SELECT _key FROM t(as_of => TIMESTAMP_SECONDS(50))"))
	require.Equal(t, []string{`[(TIMESTAMP "1970-01-01 00:05:00+00", b"howdy"), (TIMESTAMP "1970-01-01 00:03:20+00", b"hello"), (TIMESTAMP "1970-01-01 00:01:40+00", b"hi")]`},
		q(t, cat, "SELECT cf1['c1'] FROM t(with_history => TRUE) WHERE _key = 'b#02'"))
	require.Equal(t, []string{`[(TIMESTAMP "1970-01-01 00:05:00+00", b"howdy"), (TIMESTAMP "1970-01-01 00:03:20+00", b"hello")]`},
		q(t, cat, "SELECT cf1['c1'] FROM t(with_history => TRUE, latest_n => 2) WHERE _key = 'b#02'"))
	require.Equal(t, []string{`[(TIMESTAMP "1970-01-01 00:03:20+00", b"hello")]`},
		q(t, cat, "SELECT cf1['c1'] FROM t(with_history => TRUE, after => TIMESTAMP_SECONDS(100), before => TIMESTAMP_SECONDS(300)) WHERE _key = 'b#02'"))
	require.Equal(t, []string{`[(TIMESTAMP "1970-01-01 00:05:00+00", b"howdy"), (TIMESTAMP "1970-01-01 00:03:20+00", b"hello")]`},
		q(t, cat, "SELECT cf1['c1'] FROM t(with_history => TRUE, after_or_equal => TIMESTAMP_SECONDS(200)) WHERE _key = 'b#02'"))
	require.Equal(t, []string{`[(TIMESTAMP "1970-01-01 00:03:20+00", b"hello")]`},
		q(t, cat, "SELECT cf1['c1'] FROM t(with_history => TRUE, as_of => TIMESTAMP_SECONDS(250), latest_n => 1) WHERE _key = 'b#02'"))
	require.Equal(t, []string{`b"howdy", "00:05:00", 3`},
		q(t, cat, "SELECT cf1['c1'][OFFSET(0)].value, FORMAT_TIMESTAMP('%T', cf1['c1'][0].timestamp), ARRAY_LENGTH(cf1['c1']) FROM t(with_history => true) WHERE _key = 'b#02'"))
	require.Equal(t, []string{`{b"sum": [(TIMESTAMP "1970-01-01 00:01:40+00", 42)]}`},
		q(t, cat, "SELECT agg FROM t(with_history => TRUE) WHERE _key = 'b#01'"))
	require.Equal(t, []string{`b"a#01", 2`}, q(t, cat, "SELECT _key, ARRAY_LENGTH(MAP_ENTRIES(cf1)) AS version_count FROM t(with_history => TRUE) WHERE _key = 'a#01'"))
	p := prep(t, cat, "SELECT cf1, agg FROM t(with_history => TRUE)")
	require.Equal(t, []string{"MAP<BYTES, ARRAY<STRUCT<timestamp TIMESTAMP, value BYTES>>>", "MAP<BYTES, ARRAY<STRUCT<timestamp TIMESTAMP, value INT64>>>"}, colTypes(t, p))
	require.Equal(t, []string{"MAP<BYTES, BYTES>"}, colTypes(t, prep(t, cat, "SELECT cf1 FROM t(with_history => FALSE)")))

	for _, c := range []struct{ from, msg string }{
		{"t(latest_n => 2)", "requires with_history"},
		{"t(after => TIMESTAMP_SECONDS(1))", "requires with_history"},
		{"t(with_history => TRUE, latest_n => 0)", "latest_n"},
		{"t(foo => 1)", "unknown argument"},
		{"t(with_history => @p)", "bool literal"},
		{"t(as_of => 'not a ts')", "timestamp"},
		{"t(with_history => TRUE, after => TIMESTAMP_SECONDS(1), after_or_equal => TIMESTAMP_SECONDS(1))", "cannot both"},
		{"t(with_history => TRUE, with_history => FALSE)", "duplicate"},
		{"t(as_of => cf1)", "unrecognized name"},
		{"t(TRUE)", "must be named"},
	} {
		qErr(t, cat, "SELECT _key FROM "+c.from, codes.InvalidArgument, c.msg, map[string]any{"p": true})
	}
	qErr(t, cat, "SELECT _key FROM t(with_history => TRUE, latest_n => @n)", codes.InvalidArgument, "latest_n", map[string]any{"n": int64(0)})
}

func keySchemaCatalog(t *testing.T) *memCatalog {
	c := newMemCatalog()
	delim := &btpb.Type_Struct{
		Fields: []*btpb.Type_Struct_Field{
			{FieldName: "device", Type: keyFieldProto(typString)},
			{FieldName: "country", Type: keyFieldProto(typString)},
			{FieldName: "serial", Type: keyFieldProto(typBytes)},
		},
		Encoding: &btpb.Type_Struct_Encoding{Encoding: &btpb.Type_Struct_Encoding_DelimitedBytes_{
			DelimitedBytes: &btpb.Type_Struct_Encoding_DelimitedBytes{Delimiter: []byte("#")}}},
	}
	u := c.addTable(&Table{Name: "users", Families: []Family{{Name: "info"}}, RowKeySchema: delim})
	u.setStr("phone#india#8923695", "info", "n", 1, "a")
	u.setStr("tablet#us#123", "info", "n", 1, "b")

	oc := keySchemaForView([]string{"region", "id"}, []*sqlType{typString, typInt64})
	m := c.addTable(&Table{Name: "metrics", Families: []Family{{Name: "m"}}, RowKeySchema: oc})
	for _, k := range []struct {
		r  string
		id int64
	}{{"us", -5}, {"us", 10}, {"eu", 300}} {
		key, err := EncodeKey(oc, []*btpb.Value{
			{Kind: &btpb.Value_StringValue{StringValue: k.r}},
			{Kind: &btpb.Value_IntValue{IntValue: k.id}},
		})
		require.NoError(t, err)
		m.setStr(string(key), "m", "v", 1, k.r)
	}
	return c
}

func TestRowKeySchemaColumns(t *testing.T) {
	cat := keySchemaCatalog(t)
	p := prep(t, cat, "SELECT * FROM users")
	require.Equal(t, []string{"_key", "device", "country", "serial", "info"}, colNames(p))
	require.Equal(t, []string{"BYTES", "STRING", "STRING", "BYTES", "MAP<BYTES, BYTES>"}, colTypes(t, p))
	for _, c := range p.Metadata().GetProtoSchema().GetColumns() {
		require.Nil(t, c.GetType().GetStringType().GetEncoding(), "metadata types must not carry encodings")
		require.Nil(t, c.GetType().GetBytesType().GetEncoding())
	}
	require.Equal(t, []string{`"phone", "india", b"8923695"`, `"tablet", "us", b"123"`},
		q(t, cat, "SELECT device, country, serial FROM users ORDER BY _key"))
	require.Equal(t, []string{`b"tablet#us#123"`}, q(t, cat, "SELECT _key FROM users WHERE country = 'us'"))
	require.Equal(t, []string{`"eu", 300`, `"us", -5`, `"us", 10`}, q(t, cat, "SELECT region, id FROM metrics"))
	require.Equal(t, []string{`"us", 10`}, q(t, cat, "SELECT region, id FROM metrics WHERE id > 0 AND region = 'us'"))

	// Rows that do not conform to the schema fail only when key columns are used.
	cat.tables["users"].setStr("bad-key", "info", "n", 1, "c")
	require.Len(t, q(t, cat, "SELECT _key FROM users"), 3)
	qErr(t, cat, "SELECT device FROM users", codes.InvalidArgument, "does not conform")
}

func TestViews(t *testing.T) {
	cat, _ := stdCatalog()
	cat.views["v"] = "SELECT _key, cf1['c1'] AS c1 FROM t WHERE STARTS_WITH(_key, 'a')"
	require.Equal(t, []string{"_key", "c1"}, colNames(prep(t, cat, "SELECT * FROM v")))
	require.Equal(t, []string{`b"a#01", b"xyz"`, `b"a#02", b"zyx"`, `b"a#03", b"abc"`}, q(t, cat, "SELECT * FROM v"))
	require.Equal(t, []string{`b"a#02"`}, q(t, cat, "SELECT _key FROM v WHERE c1 = b'zyx'"))
	require.Equal(t, []string{`3`}, q(t, cat, "SELECT COUNT(*) FROM v"))
	require.Equal(t, []string{`b"a#01"`}, q(t, cat, "SELECT _key FROM v ORDER BY _key LIMIT 1"))
	require.Equal(t, []string{`b"xyz"`}, q(t, cat, "SELECT x.c1 FROM v AS x WHERE x._key = 'a#01'"))

	cat.views["v2"] = "SELECT c1 FROM v WHERE c1 > b'b'"
	require.Equal(t, []string{`b"xyz"`, `b"zyx"`}, q(t, cat, "SELECT * FROM v2"))
	qErr(t, cat, "SELECT * FROM v2 ORDER BY _key", codes.InvalidArgument, "unrecognized name: _key")

	cat.views["bad"] = "SELECT * FROM nowhere"
	qErr(t, cat, "SELECT * FROM bad", codes.NotFound, "invalid view bad")
	cat.views["withparam"] = "SELECT @p"
	qErr(t, cat, "SELECT * FROM withparam", codes.InvalidArgument, "query parameter")
	cat.views["self"] = "SELECT * FROM self"
	qErr(t, cat, "SELECT * FROM self", codes.InvalidArgument, "nested too deeply")
	qErr(t, cat, "SELECT * FROM v(with_history => TRUE)", codes.InvalidArgument, "only supported on tables")
}

func TestUnpack(t *testing.T) {
	cat, _ := stdCatalog()
	require.Equal(t, []string{
		`b"b#02", b"howdy", TIMESTAMP "1970-01-01 00:05:00+00"`,
		`b"b#02", b"hello", TIMESTAMP "1970-01-01 00:03:20+00"`,
		`b"b#02", b"hi", TIMESTAMP "1970-01-01 00:01:40+00"`,
	}, q(t, cat, "SELECT _key, c, _timestamp FROM UNPACK((SELECT _key, cf1['c1'] AS c FROM t(with_history => TRUE) WHERE _key = 'b#02'))"))
	require.Equal(t, []string{`{b"c1": b"xyz", b"c2": b"nop"}, TIMESTAMP "1970-01-01 00:01:40+00"`},
		q(t, cat, "SELECT cf1, _timestamp FROM UNPACK(SELECT _key, cf1 FROM t(with_history => TRUE) WHERE _key = 'a#01')"))
	p := prep(t, cat, "SELECT * FROM UNPACK(SELECT _key, cf1, agg FROM t(with_history => TRUE))")
	require.Equal(t, []string{"_key", "cf1", "agg", "_timestamp"}, colNames(p))
	require.Equal(t, []string{"BYTES", "MAP<BYTES, BYTES>", "MAP<BYTES, INT64>", "TIMESTAMP"}, colTypes(t, p))
	require.Equal(t, []string{`8`}, q(t, cat, "SELECT COUNT(*) FROM UNPACK(SELECT * FROM t(with_history => TRUE))"))
	require.Equal(t, []string{`b"b#02", 3`}, q(t, cat, "SELECT _key, COUNT(*) FROM UNPACK(SELECT * FROM t(with_history => TRUE) WHERE _key = 'b#02') GROUP BY _key"))
	qErr(t, cat, "SELECT * FROM UNPACK(SELECT * FROM t)", codes.InvalidArgument, "with_history")
}

func TestExecuteContextAndEmitErrors(t *testing.T) {
	cat, _ := stdCatalog()
	p := prep(t, cat, "SELECT _key FROM t")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.Execute(ctx, cat, nil, func([]*btpb.Value) error { return nil })
	require.Equal(t, codes.Canceled, status.Code(err))
	stop := status.Error(codes.Aborted, "stop")
	n := 0
	err = p.Execute(context.Background(), cat, nil, func([]*btpb.Value) error {
		n++
		return stop
	})
	require.Equal(t, stop, err)
	require.Equal(t, 1, n)
}

package gsql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSmoke(t *testing.T) {
	cat, _ := stdCatalog()
	require.Equal(t, []string{`1`}, q(t, cat, "SELECT 1"))
	require.Equal(t, []string{`b"a#01", b"xyz"`, `b"a#02", b"zyx"`}, q(t, cat, "SELECT _key, cf1['c1'] FROM t WHERE STARTS_WITH(_key, 'a#0') AND _key < 'a#03'"))
	rows := q(t, cat, "SELECT * FROM t WHERE _key = 'a#01'")
	require.Equal(t, []string{`b"a#01", {b"c1": b"xyz", b"c2": b"nop"}, {b"n": b"5"}, NULL, NULL`}, rows)
	require.Equal(t, []string{`6`}, q(t, cat, "SELECT COUNT(*) FROM t"))
}

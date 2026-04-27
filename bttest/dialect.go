package bttest

import (
	"strings"
	"sync"
)

type sqlDialect string

const (
	dialectSQLite   sqlDialect = "sqlite3"
	dialectPostgres sqlDialect = "postgres"
)

var storageConfig = struct {
	sync.RWMutex
	dialect     sqlDialect
	strictAdmin bool
}{
	dialect: dialectSQLite,
}

// ConfigureStorage selects SQL dialect behavior for the emulator.
func ConfigureStorage(driver string, strictAdmin bool) {
	storageConfig.Lock()
	defer storageConfig.Unlock()

	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pq":
		storageConfig.dialect = dialectPostgres
	default:
		storageConfig.dialect = dialectSQLite
	}
	storageConfig.strictAdmin = strictAdmin
}

func currentDialect() sqlDialect {
	storageConfig.RLock()
	defer storageConfig.RUnlock()
	return storageConfig.dialect
}

func isStrictAdmin() bool {
	storageConfig.RLock()
	defer storageConfig.RUnlock()
	return storageConfig.strictAdmin
}

func bind(sql string) string {
	if currentDialect() != dialectPostgres {
		return sql
	}
	var b strings.Builder
	arg := 1
	for _, r := range sql {
		if r == '?' {
			b.WriteByte('$')
			b.WriteString(intString(arg))
			arg++
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func intString(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

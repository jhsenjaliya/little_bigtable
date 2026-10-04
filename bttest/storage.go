package bttest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// storageErr converts a storage failure into a gRPC status: cancelled or
// expired request contexts keep their own codes, anything else is Internal.
func storageErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Errorf(codes.Internal, "%v", err)
}

// sqlExecutor is the narrow query surface shared by *sql.DB and *sql.Tx, so
// storage helpers can run inside or outside a transaction.
type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// withTx runs fn inside one SQL transaction. The transaction commits only when
// fn returns nil. Callers must not use the *sql.DB while fn runs: SQLite is
// configured with a single connection, so a nested non-transactional query
// would wait for the transaction forever.
func withTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// SqlRows stores one table's rows in rows_t. Every method returns storage
// errors to the caller; no request path terminates the process.
type SqlRows struct {
	parent  string // projects/{project}/instances/{instance}
	tableId string
	db      *sql.DB
}

// NewSqlRows returns a SqlRows for a specific table within the given parent instance path.
func NewSqlRows(db *sql.DB, parent, tableId string) *SqlRows {
	return &SqlRows{parent: parent, tableId: tableId, db: db}
}

// keyRange is a half-open row-key interval [start, end). An empty start or
// end is unbounded. Open starts and closed ends are normalized by appending a
// zero byte, which yields the immediate lexicographic successor.
type keyRange struct {
	start string
	end   string
}

func (r keyRange) contains(key string) bool {
	return key >= r.start && (r.end == "" || key < r.end)
}

func (r keyRange) empty() bool {
	return r.end != "" && r.start >= r.end
}

func (r *row) Scan(src interface{}) error {
	switch src := src.(type) {
	case nil:
		return nil
	case []byte:
		return gob.NewDecoder(bytes.NewBuffer(src)).Decode(&r.families)
	default:
		return fmt.Errorf("unknown type %T", src)
	}
}

func (r *row) Bytes() ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	b := new(bytes.Buffer)
	err := gob.NewEncoder(b).Encode(r.families)
	return b.Bytes(), err
}

func (db *SqlRows) executor(q sqlExecutor) sqlExecutor {
	if q != nil {
		return q
	}
	return db.db
}

// load returns the stored row, or nil when the row does not exist.
func (db *SqlRows) load(ctx context.Context, q sqlExecutor, key string) (*row, error) {
	r := newRow(key)
	err := db.executor(q).QueryRowContext(ctx,
		bind("SELECT families FROM rows_t WHERE parent = ? AND table_id = ? AND row_key = ?"),
		db.parent, db.tableId, []byte(key)).Scan(r)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load row %q: %w", key, err)
	}
	if r.families == nil {
		r.families = make(map[string]*family)
	}
	return r, nil
}

// save upserts a row. A row without cells is deleted, because Bigtable has no
// empty rows.
func (db *SqlRows) save(ctx context.Context, q sqlExecutor, r *row) error {
	if r.isEmpty() {
		return db.remove(ctx, q, r.key)
	}
	families, err := r.Bytes()
	if err != nil {
		return fmt.Errorf("encode row %q: %w", r.key, err)
	}
	_, err = db.executor(q).ExecContext(ctx,
		bind("INSERT INTO rows_t (parent, table_id, row_key, families) VALUES (?, ?, ?, ?) ON CONFLICT (parent, table_id, row_key) DO UPDATE SET families = ?"),
		db.parent, db.tableId, []byte(r.key), families, families)
	if err != nil {
		return fmt.Errorf("save row %q: %w", r.key, err)
	}
	return nil
}

func (db *SqlRows) remove(ctx context.Context, q sqlExecutor, key string) error {
	_, err := db.executor(q).ExecContext(ctx,
		bind("DELETE FROM rows_t WHERE parent = ? AND table_id = ? AND row_key = ?"),
		db.parent, db.tableId, []byte(key))
	if err != nil {
		return fmt.Errorf("delete row %q: %w", key, err)
	}
	return nil
}

func (db *SqlRows) clear(ctx context.Context, q sqlExecutor) error {
	_, err := db.executor(q).ExecContext(ctx,
		bind("DELETE FROM rows_t WHERE parent = ? AND table_id = ?"), db.parent, db.tableId)
	if err != nil {
		return fmt.Errorf("clear table %s/tables/%s: %w", db.parent, db.tableId, err)
	}
	return nil
}

// scan visits rows inside rng in key order. fn returns false to stop early.
// Rows are fully read before fn is called, so fn may issue further queries
// through the same executor.
func (db *SqlRows) scan(ctx context.Context, q sqlExecutor, rng keyRange, reversed bool, fn func(*row) (bool, error)) error {
	if rng.empty() {
		return nil
	}
	query := "SELECT row_key, families FROM rows_t WHERE parent = ? AND table_id = ?"
	args := []any{db.parent, db.tableId}
	if rng.start != "" {
		query += " AND row_key >= ?"
		args = append(args, []byte(rng.start))
	}
	if rng.end != "" {
		query += " AND row_key < ?"
		args = append(args, []byte(rng.end))
	}
	if reversed {
		query += " ORDER BY row_key DESC"
	} else {
		query += " ORDER BY row_key ASC"
	}
	rows, err := db.executor(q).QueryContext(ctx, bind(query), args...)
	if err != nil {
		return fmt.Errorf("scan rows: %w", err)
	}
	var loaded []*row
	for rows.Next() {
		var key []byte
		r := &row{}
		if err := rows.Scan(&key, r); err != nil {
			rows.Close()
			return fmt.Errorf("scan rows: %w", err)
		}
		r.key = string(key)
		if r.families == nil {
			r.families = make(map[string]*family)
		}
		loaded = append(loaded, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("scan rows: %w", err)
	}
	rows.Close()
	for _, r := range loaded {
		more, err := fn(r)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return nil
}

func (db *SqlRows) count(ctx context.Context, q sqlExecutor) (int, error) {
	var n int
	err := db.executor(q).QueryRowContext(ctx,
		bind("SELECT count(*) FROM rows_t WHERE parent = ? AND table_id = ?"), db.parent, db.tableId).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count rows: %w", err)
	}
	return n, nil
}

// copyTo copies every row of this table to dst inside the executor's scope.
func (db *SqlRows) copyTo(ctx context.Context, q sqlExecutor, dst *SqlRows) error {
	_, err := db.executor(q).ExecContext(ctx,
		bind("INSERT INTO rows_t (parent, table_id, row_key, families) SELECT ?, ?, row_key, families FROM rows_t WHERE parent = ? AND table_id = ?"),
		dst.parent, dst.tableId, db.parent, db.tableId)
	if err != nil {
		return fmt.Errorf("copy rows: %w", err)
	}
	return nil
}

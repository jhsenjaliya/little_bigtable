package bttest

import (
	"context"
	"database/sql"
	"fmt"
)

// SqlMaterializedViews persists materialized view definitions to
// materialized_views_t.
type SqlMaterializedViews struct {
	db *sql.DB
}

// NewSqlMaterializedViews returns a SqlMaterializedViews backed by the given DB.
func NewSqlMaterializedViews(db *sql.DB) *SqlMaterializedViews {
	return &SqlMaterializedViews{db: db}
}

type storedMaterializedView struct {
	name               string
	query              string
	deletionProtection bool
}

// getAll returns all persisted materialized views, used to restore state on startup.
func (m *SqlMaterializedViews) getAll(ctx context.Context) ([]storedMaterializedView, error) {
	rows, err := m.db.QueryContext(ctx, "SELECT name, query, deletion_protection FROM materialized_views_t")
	if err != nil {
		return nil, fmt.Errorf("load materialized views: %w", err)
	}
	defer rows.Close()
	var result []storedMaterializedView
	for rows.Next() {
		var v storedMaterializedView
		var dp int
		if err := rows.Scan(&v.name, &v.query, &dp); err != nil {
			return nil, fmt.Errorf("load materialized views: %w", err)
		}
		v.deletionProtection = dp != 0
		result = append(result, v)
	}
	return result, rows.Err()
}

func (m *SqlMaterializedViews) save(ctx context.Context, q sqlExecutor, name, query string, deletionProtection bool) error {
	if q == nil {
		q = m.db
	}
	dp := 0
	if deletionProtection {
		dp = 1
	}
	_, err := q.ExecContext(ctx,
		bind("INSERT INTO materialized_views_t (name, query, deletion_protection) VALUES (?, ?, ?) ON CONFLICT (name) DO UPDATE SET query = ?, deletion_protection = ?"),
		name, query, dp, query, dp)
	if err != nil {
		return fmt.Errorf("save materialized view %q: %w", name, err)
	}
	return nil
}

func (m *SqlMaterializedViews) remove(ctx context.Context, q sqlExecutor, name string) error {
	if q == nil {
		q = m.db
	}
	if _, err := q.ExecContext(ctx, bind("DELETE FROM materialized_views_t WHERE name = ?"), name); err != nil {
		return fmt.Errorf("delete materialized view %q: %w", name, err)
	}
	return nil
}

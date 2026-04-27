package bttest

import (
	"context"
	"database/sql"
)

// CreateTables initializes the SQL schema for the emulator, creating all
// required tables if they do not already exist. Safe to call on an existing DB.
func CreateTables(ctx context.Context, db *sql.DB) error {
	for _, query := range schemaStatements() {
		if _, err := db.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return nil
}

func schemaStatements() []string {
	if currentDialect() == dialectPostgres {
		return []string{
			`CREATE TABLE IF NOT EXISTS rows_t (
				parent TEXT NOT NULL,
				table_id TEXT NOT NULL,
				row_key BYTEA NOT NULL,
				families BYTEA NOT NULL,
				PRIMARY KEY (parent, table_id, row_key)
			)`,
			`CREATE TABLE IF NOT EXISTS tables_t (
				parent TEXT NOT NULL,
				table_id TEXT NOT NULL,
				metadata BYTEA NOT NULL,
				PRIMARY KEY (parent, table_id)
			)`,
			`CREATE TABLE IF NOT EXISTS instances_t (
				name TEXT PRIMARY KEY,
				metadata BYTEA NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS clusters_t (
				name TEXT PRIMARY KEY,
				parent TEXT NOT NULL,
				metadata BYTEA NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_clusters_parent ON clusters_t(parent)`,
			`CREATE TABLE IF NOT EXISTS app_profiles_t (
				name TEXT PRIMARY KEY,
				parent TEXT NOT NULL,
				metadata BYTEA NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_app_profiles_parent ON app_profiles_t(parent)`,
			`CREATE TABLE IF NOT EXISTS materialized_views_t (
				name TEXT PRIMARY KEY,
				query TEXT NOT NULL,
				deletion_protection INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE TABLE IF NOT EXISTS change_log_t (
				id BIGSERIAL PRIMARY KEY,
				table_name TEXT NOT NULL,
				row_key BYTEA NOT NULL,
				mutation_bytes BYTEA NOT NULL,
				mutation_type TEXT NOT NULL,
				commit_micros BIGINT NOT NULL,
				created_at TIMESTAMPTZ NOT NULL DEFAULT now()
			)`,
			`CREATE INDEX IF NOT EXISTS idx_change_log_table_id ON change_log_t(table_name, id)`,
			`CREATE INDEX IF NOT EXISTS idx_change_log_table_commit ON change_log_t(table_name, commit_micros)`,
			`CREATE TABLE IF NOT EXISTS authorized_views_t (
				name TEXT PRIMARY KEY,
				table_name TEXT NOT NULL,
				metadata BYTEA NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_authorized_views_table ON authorized_views_t(table_name)`,
			`CREATE TABLE IF NOT EXISTS backups_t (
				name TEXT PRIMARY KEY,
				metadata BYTEA NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS logical_views_t (
				name TEXT PRIMARY KEY,
				metadata BYTEA NOT NULL
			)`,
		}
	}

	return []string{
		`CREATE TABLE IF NOT EXISTS rows_t (
			parent TEXT NOT NULL,
			table_id TEXT NOT NULL,
			row_key BLOB NOT NULL,
			families BLOB NOT NULL,
			PRIMARY KEY (parent, table_id, row_key)
		)`,
		`CREATE TABLE IF NOT EXISTS tables_t (
			parent TEXT NOT NULL,
			table_id TEXT NOT NULL,
			metadata BLOB NOT NULL,
			PRIMARY KEY (parent, table_id)
		)`,
		`CREATE TABLE IF NOT EXISTS instances_t (
			name TEXT PRIMARY KEY,
			metadata BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS clusters_t (
			name TEXT PRIMARY KEY,
			parent TEXT NOT NULL,
			metadata BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_clusters_parent ON clusters_t(parent)`,
		`CREATE TABLE IF NOT EXISTS app_profiles_t (
			name TEXT PRIMARY KEY,
			parent TEXT NOT NULL,
			metadata BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_app_profiles_parent ON app_profiles_t(parent)`,
		`CREATE TABLE IF NOT EXISTS materialized_views_t (
			name TEXT PRIMARY KEY,
			query TEXT NOT NULL,
			deletion_protection INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS change_log_t (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			table_name TEXT NOT NULL,
			row_key BLOB NOT NULL,
			mutation_bytes BLOB NOT NULL,
			mutation_type TEXT NOT NULL,
			commit_micros INTEGER NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_change_log_table_id ON change_log_t(table_name, id)`,
		`CREATE INDEX IF NOT EXISTS idx_change_log_table_commit ON change_log_t(table_name, commit_micros)`,
		`CREATE TABLE IF NOT EXISTS authorized_views_t (
			name TEXT PRIMARY KEY,
			table_name TEXT NOT NULL,
			metadata BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_authorized_views_table ON authorized_views_t(table_name)`,
		`CREATE TABLE IF NOT EXISTS backups_t (
			name TEXT PRIMARY KEY,
			metadata BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS logical_views_t (
			name TEXT PRIMARY KEY,
			metadata BLOB NOT NULL
		)`,
	}
}

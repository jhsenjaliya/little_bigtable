package bttest

import (
	"context"
	"database/sql"
	"strings"
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

// schemaStatements returns dialect-specific DDL. The schema is written once
// in SQLite form; PostgreSQL substitutes BYTEA for BLOB and BIGSERIAL for the
// autoincrement key.
func schemaStatements() []string {
	statements := []string{
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
		// change_log_t recorded every mutation regardless of table configuration
		// and is superseded by change_stream_t.
		`DROP TABLE IF EXISTS change_log_t`,
		`CREATE TABLE IF NOT EXISTS change_stream_t (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			table_name TEXT NOT NULL,
			row_key BLOB NOT NULL,
			change_type INTEGER NOT NULL,
			commit_micros INTEGER NOT NULL,
			mutations BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_change_stream_table_id ON change_stream_t(table_name, id)`,
		`CREATE INDEX IF NOT EXISTS idx_change_stream_table_commit ON change_stream_t(table_name, commit_micros)`,
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
		`CREATE TABLE IF NOT EXISTS backup_manifests_t (
			name TEXT PRIMARY KEY,
			metadata BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS logical_views_t (
			name TEXT PRIMARY KEY,
			metadata BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS schema_bundles_t (
			name TEXT PRIMARY KEY,
			table_name TEXT NOT NULL,
			metadata BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_schema_bundles_table ON schema_bundles_t(table_name)`,
		`CREATE TABLE IF NOT EXISTS iam_policies_t (
			resource TEXT PRIMARY KEY,
			policy BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS operations_t (
			name TEXT PRIMARY KEY,
			data BLOB NOT NULL,
			create_micros INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS idempotency_t (
			table_name TEXT NOT NULL,
			row_key BLOB NOT NULL,
			token BLOB NOT NULL,
			expire_micros INTEGER NOT NULL,
			PRIMARY KEY (table_name, row_key, token)
		)`,
	}
	if currentDialect() != dialectPostgres {
		return statements
	}
	pg := make([]string, len(statements))
	for i, stmt := range statements {
		stmt = strings.ReplaceAll(stmt, "INTEGER PRIMARY KEY AUTOINCREMENT", "BIGSERIAL PRIMARY KEY")
		stmt = strings.ReplaceAll(stmt, "BLOB", "BYTEA")
		stmt = strings.ReplaceAll(stmt, "INTEGER NOT NULL", "BIGINT NOT NULL")
		pg[i] = stmt
	}
	return pg
}

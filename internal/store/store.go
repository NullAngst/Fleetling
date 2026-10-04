// Package store holds Fleetling's settings in SQLite.
//
// The database only ever holds settings and history. Stack state lives on
// disk under the stack root and in the engines, never here.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Store wraps the settings database.
type Store struct {
	db *sql.DB
}

// migrations run in order. Each entry moves the schema up one version,
// tracked with PRAGMA user_version. Never edit a shipped entry; append.
var migrations = []string{
	`CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE actions (
		id        INTEGER PRIMARY KEY,
		started   INTEGER NOT NULL,          -- unix milliseconds
		finished  INTEGER,                   -- NULL while running
		target    TEXT    NOT NULL,          -- stack folder or object name
		engine    TEXT    NOT NULL DEFAULT '',
		command   TEXT    NOT NULL,          -- one line per command run
		exit_code INTEGER,                   -- NULL while running
		output    TEXT    NOT NULL DEFAULT '' -- last 200 lines
	)`,
	`CREATE INDEX actions_started ON actions (started)`,
}

// Open opens (or creates) the database at path and brings the schema up to
// date. The parent folder is created with mode 700 and the file is forced to
// mode 600, since it holds the password hash and the session secret.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data folder: %w", err)
	}
	db, err := sql.Open(driverName, path)
	if err != nil {
		return nil, err
	}
	// One connection serializes every query. This is a single-user app
	// writing a handful of rows, so a pool buys nothing, and it means the
	// PRAGMAs below apply to every query.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	for _, p := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.ExecContext(ctx, p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("chmod database: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema is version %d but this build only knows %d; it was written by a newer Fleetling", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		// PRAGMA does not take bound parameters, so the number is formatted in.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

// Get returns a setting. ok is false when the key has never been set.
func (s *Store) Get(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// Set writes one setting.
func (s *Store) Set(ctx context.Context, key, value string) error {
	return s.SetMany(ctx, map[string]string{key: value})
}

// SetMany writes several settings in one transaction, so a form save either
// lands completely or not at all.
func (s *Store) SetMany(ctx context.Context, kv map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
			k, v); err != nil {
			return fmt.Errorf("set %s: %w", k, err)
		}
	}
	return tx.Commit()
}

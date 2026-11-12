package store

import (
	"context"
	"database/sql"
	"time"
)

// Update results.
const (
	UpdateOK      = "ok"
	UpdateFailed  = "failed"
	UpdateSkipped = "skipped" // files changed outside Fleetling; nothing ran
)

// Schedule is one stack's auto-update setting and its last result.
type Schedule struct {
	Folder     string
	Cron       string // empty when only manual runs have happened
	Enabled    bool
	LastRun    time.Time // zero before the first run
	LastResult string    // UpdateOK, UpdateFailed, UpdateSkipped or ""
	LastMsg    string
}

// GetSchedule returns a stack's schedule. ok is false when it has none.
func (s *Store) GetSchedule(ctx context.Context, folder string) (Schedule, bool, error) {
	rows, err := s.listSchedules(ctx, "WHERE folder = ?", folder)
	if err != nil || len(rows) == 0 {
		return Schedule{}, false, err
	}
	return rows[0], true, nil
}

// ListSchedules returns every schedule, by folder.
func (s *Store) ListSchedules(ctx context.Context) ([]Schedule, error) {
	return s.listSchedules(ctx, "ORDER BY folder")
}

func (s *Store) listSchedules(ctx context.Context, where string, args ...any) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT folder, cron, enabled, last_run, last_result, last_msg FROM update_schedules "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var sc Schedule
		var enabled int
		var lastRun sql.NullInt64
		if err := rows.Scan(&sc.Folder, &sc.Cron, &enabled, &lastRun, &sc.LastResult, &sc.LastMsg); err != nil {
			return nil, err
		}
		sc.Enabled = enabled == 1
		if lastRun.Valid {
			sc.LastRun = time.UnixMilli(lastRun.Int64)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// SetSchedule writes a stack's cron expression and on/off switch, keeping
// its last result.
func (s *Store) SetSchedule(ctx context.Context, folder, cron string, enabled bool) error {
	on := 0
	if enabled {
		on = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO update_schedules (folder, cron, enabled) VALUES (?, ?, ?)
		 ON CONFLICT(folder) DO UPDATE SET cron = excluded.cron, enabled = excluded.enabled`, folder, cron, on)
	return err
}

// RecordUpdate stores the result of an update run. A stack with no
// schedule (a manual run) gets a disabled row, so its badge still shows.
func (s *Store) RecordUpdate(ctx context.Context, folder string, at time.Time, result, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO update_schedules (folder, cron, enabled, last_run, last_result, last_msg) VALUES (?, '', 0, ?, ?, ?)
		 ON CONFLICT(folder) DO UPDATE SET last_run = excluded.last_run, last_result = excluded.last_result, last_msg = excluded.last_msg`,
		folder, at.UnixMilli(), result, msg)
	return err
}

// DeleteSchedule forgets a stack's schedule and results, once it's removed.
func (s *Store) DeleteSchedule(ctx context.Context, folder string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM update_schedules WHERE folder = ?", folder)
	return err
}

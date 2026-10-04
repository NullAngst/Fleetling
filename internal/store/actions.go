package store

import (
	"context"
	"database/sql"
	"time"
)

// ActionRetention is how long the action log keeps entries.
const ActionRetention = 90 * 24 * time.Hour

// Action is one entry in the action log.
type Action struct {
	ID       int64
	Started  time.Time
	Finished time.Time // zero while running
	Target   string
	Engine   string
	Command  string
	ExitCode *int // nil while running, or if the server died mid-action
	Output   string
}

// StartAction records an action as it begins, so a crash mid-run still
// leaves a trace with no exit code.
func (s *Store) StartAction(ctx context.Context, a Action) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO actions (started, target, engine, command) VALUES (?, ?, ?, ?)",
		a.Started.UnixMilli(), a.Target, a.Engine, a.Command)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishAction stores the exit code and the tail of the output.
func (s *Store) FinishAction(ctx context.Context, id int64, finished time.Time, exit int, output string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE actions SET finished = ?, exit_code = ?, output = ? WHERE id = ?",
		finished.UnixMilli(), exit, output, id)
	return err
}

// ListActions returns entries newest first. target filters when non-empty.
func (s *Store) ListActions(ctx context.Context, target string, limit, offset int) ([]Action, error) {
	q := "SELECT id, started, finished, target, engine, command, exit_code, output FROM actions"
	args := []any{}
	if target != "" {
		q += " WHERE target = ?"
		args = append(args, target)
	}
	q += " ORDER BY started DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var a Action
		var started int64
		var finished, exit sql.NullInt64
		if err := rows.Scan(&a.ID, &started, &finished, &a.Target, &a.Engine, &a.Command, &exit, &a.Output); err != nil {
			return nil, err
		}
		a.Started = time.UnixMilli(started)
		if finished.Valid {
			a.Finished = time.UnixMilli(finished.Int64)
		}
		if exit.Valid {
			e := int(exit.Int64)
			a.ExitCode = &e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PruneActions deletes entries that started before cutoff.
func (s *Store) PruneActions(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM actions WHERE started < ?", cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

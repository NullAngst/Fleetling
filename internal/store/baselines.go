package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Baseline is the approved state of one stack's deployment files.
type Baseline struct {
	Folder   string
	Recorded time.Time
	Source   string
	Snapshot string // JSON, owned by the compose package
}

// GetBaseline returns a stack's baseline. ok is false when none is recorded.
func (s *Store) GetBaseline(ctx context.Context, folder string) (b Baseline, ok bool, err error) {
	var rec int64
	err = s.db.QueryRowContext(ctx,
		"SELECT folder, recorded, source, snapshot FROM stack_baselines WHERE folder = ?", folder).
		Scan(&b.Folder, &rec, &b.Source, &b.Snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return Baseline{}, false, nil
	}
	if err != nil {
		return Baseline{}, false, err
	}
	b.Recorded = time.UnixMilli(rec)
	return b, true, nil
}

// SetBaseline records or replaces a stack's baseline.
func (s *Store) SetBaseline(ctx context.Context, b Baseline) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO stack_baselines (folder, recorded, source, snapshot) VALUES (?, ?, ?, ?)
		 ON CONFLICT(folder) DO UPDATE SET recorded = excluded.recorded, source = excluded.source, snapshot = excluded.snapshot`,
		b.Folder, b.Recorded.UnixMilli(), b.Source, b.Snapshot)
	return err
}

// DeleteBaseline forgets a stack's baseline, once its folder is gone. A
// stack made later in a folder with the same name starts unrecorded
// instead of being compared with files that no longer exist.
func (s *Store) DeleteBaseline(ctx context.Context, folder string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM stack_baselines WHERE folder = ?", folder)
	return err
}

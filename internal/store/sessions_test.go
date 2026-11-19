package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Revoked sessions survive a reopen, and expired ones drop out.
func TestRevokedSessions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleetling.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	if err := s.SetRevokedSessions(ctx, map[string]time.Time{"live": now.Add(time.Hour), "old": now.Add(-time.Second)}, now); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rv, err := s.RevokedSessions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rv) != 1 || !rv["live"].Equal(now.Add(time.Hour)) {
		t.Fatalf("revoked = %v", rv)
	}
	if rv, _ := s.RevokedSessions(ctx, now.Add(2*time.Hour)); len(rv) != 0 {
		t.Fatalf("expired entry still returned: %v", rv)
	}
}

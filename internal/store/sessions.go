package store

import (
	"context"
	"encoding/json"
	"time"
)

// KeyRevokedSessions holds the sessions ended with Log out before they
// expired, as JSON: hex session ID to expiry in unix seconds. Sessions
// are signed cookies, so this list is what stops a copy of the cookie
// working after the browser that held it logged out. It is a setting
// rather than its own table so that no schema change comes with it, and
// an update that rolls back to the previous image can still open the
// database.
const KeyRevokedSessions = "revoked_sessions"

// RevokedSessions returns every revoked session that hasn't expired by
// now, with its expiry.
func (s *Store) RevokedSessions(ctx context.Context, now time.Time) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	v, ok, err := s.Get(ctx, KeyRevokedSessions)
	if err != nil || !ok {
		return out, err
	}
	var raw map[string]int64
	if json.Unmarshal([]byte(v), &raw) != nil {
		return out, nil // unreadable: nothing to honor, and the next write replaces it
	}
	for id, exp := range raw {
		if t := time.Unix(exp, 0); t.After(now) {
			out[id] = t
		}
	}
	return out, nil
}

// SetRevokedSessions replaces the revoked list. Expired entries are
// dropped on the way.
func (s *Store) SetRevokedSessions(ctx context.Context, rv map[string]time.Time, now time.Time) error {
	raw := map[string]int64{}
	for id, t := range rv {
		if t.After(now) {
			raw[id] = t.Unix()
		}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return s.Set(ctx, KeyRevokedSessions, string(b))
}

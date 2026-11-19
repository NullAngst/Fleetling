package auth

import (
	"sync"
	"time"
)

// Limiter locks out a client address after too many failed logins.
// Defaults per the spec: 5 failures in 15 minutes locks that address for
// 15 minutes.
//
// A try is counted when it starts, not when the password check finishes.
// The check is slow on purpose, so counting only at the end would let a
// burst of parallel requests all pass the limiter before the first
// failure is recorded. With tries counted up front, failures plus tries in
// flight can never pass the limit.
type Limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	lockout time.Duration
	now     func() time.Time
	ips     map[string]*ipState
}

type ipState struct {
	fails       []time.Time
	inflight    int
	lockedUntil time.Time
}

// Outcome is how a try that Take let through ended.
type Outcome int

const (
	// Success: the password was right. The address starts over.
	Success Outcome = iota
	// Failure: the password was wrong. It counts toward a lockout.
	Failure
	// Abandoned: nothing was checked, because the server was busy or the
	// browser went away. It doesn't count either way.
	Abandoned
)

// NewLimiter builds a limiter. max failures inside window locks the
// address for lockout.
func NewLimiter(max int, window, lockout time.Duration) *Limiter {
	return &Limiter{max: max, window: window, lockout: lockout, now: time.Now, ips: map[string]*ipState{}}
}

// Take reserves one try for key, before the password is checked. ok is
// false while key is locked out, or while its recent failures plus the
// tries already being checked reach the limit; retry is how long to wait.
// Every Take that returns ok must be followed by exactly one Done.
func (l *Limiter) Take(key string) (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	st := l.ips[key]
	if st == nil {
		st = &ipState{}
		l.ips[key] = st
	}
	if now.Before(st.lockedUntil) {
		return false, st.lockedUntil.Sub(now)
	}
	st.fails = pruneBefore(st.fails, now.Add(-l.window))
	if len(st.fails)+st.inflight >= l.max {
		// Full only because of tries still being checked: those finish in
		// well under a minute, one way or the other.
		retry = time.Minute
		if len(st.fails) >= l.max {
			retry = st.fails[0].Add(l.window).Sub(now)
		}
		return false, retry
	}
	st.inflight++
	return true, 0
}

// Done finishes a try that Take let through. It returns true when this
// failure started a lockout.
func (l *Limiter) Done(key string, o Outcome) (locked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.ips[key]
	if st == nil {
		return false
	}
	if st.inflight > 0 {
		st.inflight--
	}
	switch o {
	case Success:
		if st.inflight == 0 {
			delete(l.ips, key)
		} else {
			st.fails, st.lockedUntil = nil, time.Time{}
		}
	case Failure:
		now := l.now()
		st.fails = append(pruneBefore(st.fails, now.Add(-l.window)), now)
		if len(st.fails) >= l.max {
			st.lockedUntil = now.Add(l.lockout)
			st.fails = nil
			return true
		}
	}
	return false
}

// sweep drops idle entries so a scan from many addresses can't grow the map
// forever. Only runs once the map gets large, so the normal case is free.
func (l *Limiter) sweep(now time.Time) {
	if len(l.ips) < 1024 {
		return
	}
	for ip, st := range l.ips {
		st.fails = pruneBefore(st.fails, now.Add(-l.window))
		if len(st.fails) == 0 && st.inflight == 0 && !now.Before(st.lockedUntil) {
			delete(l.ips, ip)
		}
	}
}

func pruneBefore(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}

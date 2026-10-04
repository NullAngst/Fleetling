package auth

import (
	"sync"
	"time"
)

// Limiter locks out an IP after too many failed logins.
// Defaults per the spec: 5 failures in 15 minutes locks that IP for 15 minutes.
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
	lockedUntil time.Time
}

// NewLimiter builds a limiter. max failures inside window locks the IP for lockout.
func NewLimiter(max int, window, lockout time.Duration) *Limiter {
	return &Limiter{max: max, window: window, lockout: lockout, now: time.Now, ips: map[string]*ipState{}}
}

// Allowed reports whether ip may try a login now. When it may not, retry is
// how long until the lockout ends.
func (l *Limiter) Allowed(ip string) (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.ips[ip]
	if st == nil {
		return true, 0
	}
	now := l.now()
	if now.Before(st.lockedUntil) {
		return false, st.lockedUntil.Sub(now)
	}
	return true, 0
}

// Fail records a failed login. It returns true when this failure triggered a lockout.
func (l *Limiter) Fail(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	st := l.ips[ip]
	if st == nil {
		st = &ipState{}
		l.ips[ip] = st
	}
	st.fails = append(pruneBefore(st.fails, now.Add(-l.window)), now)
	if len(st.fails) >= l.max {
		st.lockedUntil = now.Add(l.lockout)
		st.fails = nil
		return true
	}
	return false
}

// Reset clears an IP after a successful login.
func (l *Limiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ips, ip)
}

// sweep drops idle entries so a scan from many addresses can't grow the map
// forever. Only runs once the map gets large, so the normal case is free.
func (l *Limiter) sweep(now time.Time) {
	if len(l.ips) < 1024 {
		return
	}
	for ip, st := range l.ips {
		st.fails = pruneBefore(st.fails, now.Add(-l.window))
		if len(st.fails) == 0 && !now.Before(st.lockedUntil) {
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

package server

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Live websockets: shells, log streams and job output. A request is
// checked for a session once, when the socket opens, and a shell can stay
// open for days. So every socket is tied to the session that opened it and
// closed when that session ends: on Log out, on Log out everywhere or a
// password change (both replace the secret), and when the cookie expires.

type liveSocket struct {
	session string // hex session ID
	timer   *time.Timer
}

type socketSet struct {
	mu sync.Mutex
	m  map[*websocket.Conn]liveSocket
}

// sessionExpires reads the expiry out of a cookie value that
// verifySession already accepted.
func sessionExpires(value string) time.Time {
	raw, err := b64.DecodeString(value)
	if err != nil || len(raw) < 8 {
		return time.Time{}
	}
	return time.Unix(int64(binary.BigEndian.Uint64(raw[:8])), 0)
}

// trackSocket registers c for the session on r until the handler returns.
func (s *Server) trackSocket(r *http.Request, c *websocket.Conn) {
	ck, err := r.Cookie(sessionCookie)
	id, ok := s.sessionID(r)
	if err != nil || !ok {
		// requireAuth ran first, so this only happens when the session
		// ended in between. Nothing to keep open.
		endSocket(c)
		return
	}
	ls := liveSocket{session: hex.EncodeToString(id)}
	if left := sessionExpires(ck.Value).Sub(s.now()); left > 0 {
		ls.timer = time.AfterFunc(left, func() { s.closeSockets(func(x liveSocket) bool { return x.session == ls.session }) })
	}
	s.socks.mu.Lock()
	if s.socks.m == nil {
		s.socks.m = map[*websocket.Conn]liveSocket{}
	}
	s.socks.m[c] = ls
	s.socks.mu.Unlock()
	// The request context ends when the handler returns, hijacked or not.
	go func() {
		<-r.Context().Done()
		s.socks.mu.Lock()
		if x, ok := s.socks.m[c]; ok {
			if x.timer != nil {
				x.timer.Stop()
			}
			delete(s.socks.m, c)
		}
		s.socks.mu.Unlock()
	}()
}

// closeSockets closes every live socket match picks.
func (s *Server) closeSockets(match func(liveSocket) bool) {
	s.socks.mu.Lock()
	var gone []*websocket.Conn
	for c, ls := range s.socks.m {
		if match(ls) {
			if ls.timer != nil {
				ls.timer.Stop()
			}
			delete(s.socks.m, c)
			gone = append(gone, c)
		}
	}
	s.socks.mu.Unlock()
	for _, c := range gone {
		endSocket(c)
	}
}

// endSocket closes c with a reason the browser can show. Close waits for
// the other side's answer, so it runs off the caller's path; CloseNow
// follows in case the browser never answers.
func endSocket(c *websocket.Conn) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() { c.Close(websocket.StatusPolicyViolation, "session ended, log in again"); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			c.CloseNow()
		}
	}()
}

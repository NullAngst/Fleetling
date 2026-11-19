package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// openSocket dials a websocket that stays open until the server closes it,
// the way a shell does, using cookie for the session.
func openSocket(t *testing.T, h *harness, cookie string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(h.s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		c, err := h.s.acceptWS(w, r)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	hdr := http.Header{}
	hdr.Add("Cookie", sessionCookie+"="+cookie)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

// closedBy waits for the server to close c and returns the close status.
func closedBy(t *testing.T, c *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("socket still open")
	}
	return websocket.CloseStatus(err)
}

// Log out everywhere ends open shells and log streams too, not just
// cookies.
func TestLogoutAllClosesSockets(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	c := openSocket(t, h, b.cookies[sessionCookie].Value)
	if w := b.do("POST", "/settings/logout-all", url.Values{"csrf": {b.token("/settings")}}, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("logout-all = %d", w.Code)
	}
	if code := closedBy(t, c); code != websocket.StatusPolicyViolation {
		t.Fatalf("close status %v", code)
	}
}

// Log out ends that one session: a copy of its cookie stops working, its
// sockets close, and other sessions carry on.
func TestLogoutRevokesTheSession(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	stolen := b.cookies[sessionCookie].Value
	other := h.browser()
	other.ip = "192.168.1.51:40000"
	if w := other.do("POST", "/login", url.Values{"csrf": {other.token("/login")}, "password": {goodPassword}}, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("second login = %d", w.Code)
	}
	mine := openSocket(t, h, stolen)
	theirs := openSocket(t, h, other.cookies[sessionCookie].Value)

	if w := b.do("POST", "/logout", url.Values{"csrf": {b.token("/")}}, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d", w.Code)
	}
	if code := closedBy(t, mine); code != websocket.StatusPolicyViolation {
		t.Fatalf("close status %v", code)
	}
	thief := h.browser()
	thief.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: stolen}
	if w := thief.do("GET", "/", nil, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("logged-out cookie still works: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := other.do("GET", "/", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("the other session was logged out too: %d", w.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := theirs.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the other session's socket closed: %v", err)
	}

	// A restart keeps the revocation: it lives in the database.
	rv, err := h.s.store.RevokedSessions(context.Background(), h.s.now())
	if err != nil || len(rv) != 1 {
		t.Fatalf("revoked after restart: %v %v", rv, err)
	}
}

// A socket closes when its session's cookie runs out, even if the browser
// keeps it open.
func TestSocketClosesAtSessionExpiry(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	cookie := b.cookies[sessionCookie].Value
	exp := sessionExpires(cookie)
	h.s.now = func() time.Time { return exp.Add(-300 * time.Millisecond) }
	c := openSocket(t, h, cookie)
	if code := closedBy(t, c); code != websocket.StatusPolicyViolation {
		t.Fatalf("close status %v", code)
	}
}

// A logged-in post can't be made arbitrarily large.
func TestAuthedBodyIsCapped(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	big := strings.Repeat("x", maxAuthedBody+1)
	w := b.do("POST", "/settings", url.Values{"csrf": {b.token("/settings")}, "pad": {big}}, nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized post = %d, want 413", w.Code)
	}
}

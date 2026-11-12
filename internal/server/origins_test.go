package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/NullAngst/Fleetling/internal/compose"
)

func TestParseOrigins(t *testing.T) {
	got, err := ParseOrigins(" https://Fleet.Example.com , http://server.lan:8420/ ")
	if err != nil || strings.Join(got, ",") != "https://fleet.example.com,http://server.lan:8420" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"fleet.example.com", "ftp://x", "https://u:p@x", "https://x/path", "https://x?q=1", "https://"} {
		if _, err := ParseOrigins(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// Behind a reverse proxy that rewrites Host, the browser's Origin is the
// proxy's. FLEETLING_ORIGINS lets exactly that origin in, for websockets
// and form posts alike.
func TestTrustedOrigins(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "ws")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {a: {image: x}}\n"), 0o644)
	compose.WriteMeta(dir, &compose.Meta{Project: "ws", Engine: "docker", Deployed: ptr(time.Now())}, compose.Owner{})
	h.approve(dir)
	w := b.do("POST", "/stacks/ws/action/down", url.Values{"csrf": {b.token("/stacks/ws")}}, nil)
	id := regexp.MustCompile(`job=([0-9a-f]+)`).FindStringSubmatch(w.Header().Get("Location"))[1]

	h.s.cfg.Origins = []string{"https://fleet.example.com"}
	h.s.handler = h.s.routes()
	srv := httptest.NewServer(h.s)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial := func(origin string) error {
		hdr := http.Header{}
		hdr.Add("Cookie", sessionCookie+"="+b.cookies[sessionCookie].Value)
		hdr.Set("Origin", origin)
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/jobs/"+id+"/ws", &websocket.DialOptions{HTTPHeader: hdr})
		if err == nil {
			c.CloseNow()
		}
		return err
	}
	if err := dial("https://fleet.example.com"); err != nil {
		t.Errorf("listed origin refused: %v", err)
	}
	for _, bad := range []string{"http://fleet.example.com", "https://fleet.example.com.evil.net", "https://evil.example"} {
		if err := dial(bad); err == nil {
			t.Errorf("origin %s accepted", bad)
		}
	}
	// A form post from the proxy's origin passes the cross-origin check;
	// another origin doesn't.
	post := func(origin string) int {
		return b.do("POST", "/stacks/ws/action/down", url.Values{"csrf": {b.token("/stacks/ws")}},
			map[string]string{"Origin": origin, "Sec-Fetch-Site": "cross-site"}).Code
	}
	if code := post("https://fleet.example.com"); code == http.StatusForbidden {
		t.Error("listed origin's post refused")
	}
	if code := post("https://evil.example"); code != http.StatusForbidden {
		t.Errorf("cross-site post: %d", code)
	}
}

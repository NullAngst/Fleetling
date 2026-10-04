package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/NullAngst/Fleetling/internal/auth"
	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
)

func init() {
	// Full-cost argon2 under -race makes the suite slow for no benefit.
	auth.DefaultParams = auth.Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
}

type fakeEngine struct {
	info       engine.Info
	containers []compose.Container
	mounts     []compose.Mount
	labels     map[string]string
	err        error
}

func (f *fakeEngine) Identify(context.Context) (engine.Info, error) { return f.info, f.err }
func (f *fakeEngine) ComposeContainers(_ context.Context, slot string) ([]compose.Container, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]compose.Container, len(f.containers))
	for i, c := range f.containers {
		c.Engine = slot
		out[i] = c
	}
	return out, nil
}
func (f *fakeEngine) ContainerMounts(context.Context, string) ([]compose.Mount, error) {
	return f.mounts, f.err
}
func (f *fakeEngine) ContainerLabels(context.Context, string) (map[string]string, error) {
	return f.labels, f.err
}
func (f *fakeEngine) Close() error { return nil }

type harness struct {
	t         *testing.T
	s         *Server
	root      string
	engines   map[string]*fakeEngine
	inCtr     bool
	dockerLog string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	root := filepath.Join(dir, "opt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "data", "fleetling.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := New(ctx, Config{DefaultRoot: root, DataDir: filepath.Join(dir, "data"), Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, st)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, s: s, root: root, engines: map[string]*fakeEngine{
		store.DefaultDockerHost: {info: engine.Info{Kind: engine.KindDocker, Version: "29.8.2"}},
	}}
	s.newEngine = func(ep string) (Engine, error) {
		if e, ok := h.engines[ep]; ok {
			return e, nil
		}
		return &fakeEngine{err: engine.ErrSocketMissing}, nil
	}
	s.inContainer = func() bool { return h.inCtr }
	h.dockerLog = filepath.Join(dir, "docker.log")
	script := filepath.Join(dir, "docker")
	if err := os.WriteFile(script, []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	s.runner = compose.Runner{Docker: script}
	t.Setenv("FAKE_DOCKER_LOG", h.dockerLog)
	s.selfIDs = func() []string { return []string{"abc123"} }
	return h
}

// browser keeps cookies between requests, like a real one would.
type browser struct {
	h       *harness
	cookies map[string]*http.Cookie
	ip      string
}

func (h *harness) browser() *browser {
	return &browser{h: h, cookies: map[string]*http.Cookie{}, ip: "192.168.1.50:40000"}
}

func (b *browser) do(method, path string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = b.ip
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.h.s.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return w
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// token loads a page and pulls the CSRF token out of its form.
func (b *browser) token(path string) string {
	b.h.t.Helper()
	w := b.do("GET", path, nil, nil)
	m := csrfRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		b.h.t.Fatalf("no csrf token on %s (status %d): %s", path, w.Code, w.Body.String())
	}
	return m[1]
}

const goodPassword = "a long enough password"

func (h *harness) setUp(b *browser) {
	h.t.Helper()
	w := b.do("POST", "/setup", url.Values{
		"csrf": {b.token("/setup")}, "token": {h.s.SetupToken()},
		"password": {goodPassword}, "confirm": {goodPassword}, "root": {h.root},
	}, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		h.t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
}

func TestEverythingRedirectsToSetupFirst(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	for _, p := range []string{"/", "/settings", "/login"} {
		if w := b.do("GET", p, nil, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
			t.Errorf("GET %s = %d %s", p, w.Code, w.Header().Get("Location"))
		}
	}
	if w := b.do("GET", "/healthz", nil, nil); w.Code != http.StatusOK {
		t.Errorf("healthz = %d", w.Code)
	}
	if w := b.do("GET", "/static/app.css", nil, nil); w.Code != http.StatusOK {
		t.Errorf("static = %d", w.Code)
	}
	if h.s.SetupToken() == "" {
		t.Error("no setup token on a fresh install")
	}
}

func TestSetupNeedsTokenAndCSRF(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	tok := b.token("/setup")
	base := url.Values{"token": {h.s.SetupToken()}, "password": {goodPassword}, "confirm": {goodPassword}, "root": {h.root}}

	if w := b.do("POST", "/setup", base, nil); w.Code != http.StatusForbidden {
		t.Errorf("no csrf: %d", w.Code)
	}
	wrong := url.Values{"csrf": {tok}, "token": {"nope"}, "password": {goodPassword}, "confirm": {goodPassword}, "root": {h.root}}
	if w := b.do("POST", "/setup", wrong, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", w.Code)
	}
	short := url.Values{"csrf": {tok}, "token": {h.s.SetupToken()}, "password": {"short"}, "confirm": {"short"}, "root": {h.root}}
	if w := b.do("POST", "/setup", short, nil); w.Code != http.StatusBadRequest {
		t.Errorf("short password: %d", w.Code)
	}
	badRoot := url.Values{"csrf": {tok}, "token": {h.s.SetupToken()}, "password": {goodPassword}, "confirm": {goodPassword}, "root": {"/"}}
	if w := b.do("POST", "/setup", badRoot, nil); w.Code != http.StatusBadRequest {
		t.Errorf("root /: %d", w.Code)
	}

	h.setUp(b)
	if h.s.SetupToken() != "" {
		t.Error("setup token still set after setup")
	}
	if w := b.do("GET", "/", nil, nil); w.Code != http.StatusOK {
		t.Errorf("after setup GET / = %d", w.Code)
	}
	if w := b.do("GET", "/setup", nil, nil); w.Code != http.StatusSeeOther {
		t.Errorf("setup page still open: %d", w.Code)
	}
}

func TestSetupRefusesMismatchedRootMount(t *testing.T) {
	h := newHarness(t)
	h.inCtr = true
	h.engines[store.DefaultDockerHost].mounts = []compose.Mount{{Type: "bind", Source: "/mnt/elsewhere", Destination: h.root}}
	b := h.browser()
	w := b.do("POST", "/setup", url.Values{
		"csrf": {b.token("/setup")}, "token": {h.s.SetupToken()},
		"password": {goodPassword}, "confirm": {goodPassword}, "root": {h.root},
	}, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "- "+h.root+":"+h.root) {
		t.Fatalf("mismatched mount: %d %s", w.Code, w.Body.String())
	}
	h.engines[store.DefaultDockerHost].mounts = []compose.Mount{{Type: "bind", Source: h.root, Destination: h.root}}
	h.setUp(b)
}

func TestLoginLockout(t *testing.T) {
	h := newHarness(t)
	h.setUp(h.browser())

	b := h.browser()
	for i := range 5 {
		w := b.do("POST", "/login", url.Values{"csrf": {b.token("/login")}, "password": {"wrong password!"}}, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i+1, w.Code)
		}
	}
	w := b.do("POST", "/login", url.Values{"csrf": {b.token("/login")}, "password": {goodPassword}}, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("right password during lockout: %d", w.Code)
	}

	other := h.browser()
	other.ip = "192.168.1.51:40000"
	w = other.do("POST", "/login", url.Values{"csrf": {other.token("/login")}, "password": {goodPassword}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("other IP locked out too: %d", w.Code)
	}
}

func TestAuthedPostsNeedCSRF(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	form := url.Values{"slot": {"docker"}, "docker_host": {store.DefaultDockerHost}}
	if w := b.do("POST", "/settings/test", form, nil); w.Code != http.StatusForbidden {
		t.Errorf("no token: %d", w.Code)
	}
	tok := b.token("/settings")
	w := b.do("POST", "/settings/test", form, map[string]string{csrfHeader: tok, "HX-Request": "true"})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Docker 29.8.2") {
		t.Errorf("with token: %d %s", w.Code, w.Body.String())
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	tok := b.token("/setup")
	w := b.do("POST", "/setup", url.Values{"csrf": {tok}}, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site POST = %d", w.Code)
	}
}

func TestLogoutEverywhere(t *testing.T) {
	h := newHarness(t)
	a := h.browser()
	h.setUp(a)
	other := h.browser()
	other.cookies[sessionCookie] = a.cookies[sessionCookie] // same session, second device

	w := a.do("POST", "/settings/logout-all", url.Values{"csrf": {a.token("/settings")}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("logout-all = %d", w.Code)
	}
	if w := other.do("GET", "/", nil, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("old session still works: %d", w.Code)
	}
}

func TestStacksPage(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	os.MkdirAll(filepath.Join(h.root, "copyparty"), 0o755)
	os.WriteFile(filepath.Join(h.root, "copyparty", "compose.yaml"), []byte("services:\n  copyparty:\n    image: x\n"), 0o644)
	h.engines[store.DefaultDockerHost].containers = []compose.Container{
		{Project: "copyparty", Service: "copyparty", WorkingDir: filepath.Join(h.root, "copyparty"), Running: true},
		{Project: "plex", Service: "plex", WorkingDir: "/data/compose/9", Running: true},
	}
	w := b.do("GET", "/", nil, nil)
	body := w.Body.String()
	for _, want := range []string{"copyparty", "On disk", "plex", "External", "/data/compose/9", "1/1", "no socket at"} {
		if !strings.Contains(body, want) {
			t.Errorf("stacks page missing %q", want)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestRootChangeNeedsConfirm(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	os.MkdirAll(filepath.Join(h.root, "gitea"), 0o755)
	os.WriteFile(filepath.Join(h.root, "gitea", "compose.yaml"), []byte("services: {gitea: {image: x}}\n"), 0o644)
	newRoot := filepath.Join(filepath.Dir(h.root), "containers")
	os.MkdirAll(newRoot, 0o755)

	form := url.Values{"csrf": {b.token("/settings")}, "root": {newRoot}, "docker_host": {store.DefaultDockerHost}, "podman_host": {""}, "ignore": {"containerd"}}
	w := b.do("POST", "/settings", form, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), filepath.Join(h.root, "gitea")) {
		t.Fatalf("confirm page: %d %s", w.Code, w.Body.String())
	}
	st, _ := h.s.store.LoadSettings(context.Background(), "")
	if st.Root != h.root {
		t.Fatal("root changed before confirming")
	}
	form.Set("confirm_root", "yes")
	if w := b.do("POST", "/settings", form, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("confirmed save: %d %s", w.Code, w.Body.String())
	}
	st, _ = h.s.store.LoadSettings(context.Background(), "")
	if st.Root != newRoot || st.PodmanHost != "" {
		t.Fatalf("after save: %+v", st)
	}
}

func TestSettingsNeedAnEngine(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	tok := b.token("/settings")
	for _, c := range []struct{ docker, podman, want string }{
		{"", "", "at least one"},
		{"/nowhere/docker.sock", "/nowhere/podman.sock", "no engine endpoint answered"},
		{"tcp://10.0.0.2:2375", "", "socket path"},
		{store.DefaultDockerHost, "unix://" + store.DefaultDockerHost, "same socket"},
	} {
		w := b.do("POST", "/settings", url.Values{"csrf": {tok}, "root": {h.root}, "docker_host": {c.docker}, "podman_host": {c.podman}}, nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("docker=%q podman=%q: %d, want %q", c.docker, c.podman, w.Code, c.want)
		}
	}
}

func TestPasswordChange(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	newPw := "an even longer password"
	w := b.do("POST", "/settings/password", url.Values{"csrf": {b.token("/settings")}, "current": {goodPassword}, "password": {newPw}, "confirm": {newPw}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("change: %d %s", w.Code, w.Body.String())
	}
	if w := b.do("GET", "/", nil, nil); w.Code != http.StatusOK {
		t.Error("this browser lost its session after changing the password")
	}
	fresh := h.browser()
	fresh.ip = "10.9.9.9:1"
	if w := fresh.do("POST", "/login", url.Values{"csrf": {fresh.token("/login")}, "password": {newPw}}, nil); w.Code != http.StatusSeeOther {
		t.Errorf("new password login: %d", w.Code)
	}
}

func TestSessionTokens(t *testing.T) {
	secret := randomBytes(32)
	now := time.Unix(2_000_000_000, 0)
	v, id := newSession(secret, now)
	got, ok := verifySession(secret, v, now.Add(time.Hour))
	if !ok || string(got) != string(id) {
		t.Fatal("fresh session did not verify")
	}
	if _, ok := verifySession(secret, v, now.Add(sessionTTL)); ok {
		t.Error("expired session verified")
	}
	if _, ok := verifySession(randomBytes(32), v, now); ok {
		t.Error("session verified under another secret")
	}
	raw, _ := b64.DecodeString(v)
	raw[3] ^= 1 // push the expiry
	if _, ok := verifySession(secret, b64.EncodeToString(raw), now); ok {
		t.Error("tampered session verified")
	}
	if csrfFor(secret, "session", id) == csrfFor(secret, "pre", id) {
		t.Error("session and pre-auth CSRF tokens collide")
	}
}

func TestClientAddr(t *testing.T) {
	trustedList, err := ParseTrustedProxies("172.17.0.1, 10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTrustedProxies("not-an-ip"); err == nil {
		t.Error("accepted a junk proxy entry")
	}
	cases := []struct {
		remote, xff, want string
		viaProxy          bool
	}{
		{"192.168.1.9:5000", "", "192.168.1.9", false},
		{"192.168.1.9:5000", "1.2.3.4", "192.168.1.9", false},             // untrusted peer, header ignored
		{"172.17.0.1:5000", "192.168.1.9", "192.168.1.9", true},           // NPMPlus on the bridge
		{"172.17.0.1:5000", "6.6.6.6, 192.168.1.9", "192.168.1.9", true},  // forged left entry ignored
		{"172.17.0.1:5000", "192.168.1.9, 10.1.2.3", "192.168.1.9", true}, // chain of trusted proxies
		{"[::ffff:172.17.0.1]:5000", "192.168.1.9", "192.168.1.9", true},  // v4-mapped peer
		{"172.17.0.1:5000", "garbage", "172.17.0.1", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		got, via := clientAddr(r, trustedList)
		if got != netip.MustParseAddr(c.want) || via != c.viaProxy {
			t.Errorf("%s xff=%q: got %s %v, want %s %v", c.remote, c.xff, got, via, c.want, c.viaProxy)
		}
	}
}

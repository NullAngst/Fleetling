package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
)

func TestEngineSuggestion(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	tok := b.token("/stacks/new")
	w := b.do("POST", "/stacks/detect", url.Values{"compose": {"services:\n  a:\n    image: x\n    userns_mode: keep-id\n"}}, map[string]string{"X-CSRF-Token": tok})
	var rep detectReply
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil || rep.Engine != "podman" || rep.Why != "Suggested Podman: line 4 uses userns_mode: keep-id" {
		t.Fatalf("detect: %d %s", w.Code, w.Body.String())
	}
	// No token: refused like every other POST.
	if w := b.do("POST", "/stacks/detect", url.Values{"compose": {"x"}}, nil); w.Code != http.StatusForbidden {
		t.Errorf("detect without CSRF: %d", w.Code)
	}
	// Plain Docker files get no suggestion, so Docker stays the default.
	w = b.do("POST", "/stacks/detect", url.Values{"compose": {"services: {a: {image: nginx}}\n"}}, map[string]string{"X-CSRF-Token": tok})
	if !strings.Contains(w.Body.String(), `"engine":""`) {
		t.Errorf("docker file: %s", w.Body.String())
	}
	// With Podman turned off, the hint still warns.
	st, _ := h.s.store.LoadSettings(context.Background(), h.root)
	st.PodmanHost = ""
	h.s.store.SaveSettings(context.Background(), st)
	w = b.do("POST", "/stacks/detect", url.Values{"compose": {"services:\n  a:\n    image: localhost/x\n"}}, map[string]string{"X-CSRF-Token": tok})
	if !strings.Contains(w.Body.String(), `"engine":""`) || !strings.Contains(w.Body.String(), "No Podman endpoint is configured") {
		t.Errorf("podman off: %s", w.Body.String())
	}
	if strings.Contains(b.do("GET", "/stacks/new", nil, nil).Body.String(), `<select name="engine"`) {
		t.Error("engine field shown with one engine configured")
	}
}

func TestEngineMove(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	h.engines[store.DefaultPodmanHost] = &fakeEngine{info: engine.Info{Kind: engine.KindPodman}}
	dir := filepath.Join(h.root, "media")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {a: {image: x}}\n"), 0o644)
	compose.WriteMeta(dir, &compose.Meta{Project: "media", Engine: "docker", Deployed: ptr(h.s.now())}, compose.Owner{})
	h.approve(dir)

	if !strings.Contains(b.do("GET", "/stacks/media", nil, nil).Body.String(), `href="/stacks/media/engine"`) {
		t.Error("no move link with both engines configured")
	}
	page := b.do("GET", "/stacks/media/engine", nil, nil).Body.String()
	for _, want := range []string{"Named volumes do not move", "Bind mounts under the root are unaffected", "dialog-band", "-p media"} {
		if !strings.Contains(page, want) {
			t.Errorf("move page missing %q", want)
		}
	}
	w := b.do("POST", "/stacks/media/engine", url.Values{"csrf": {b.token("/stacks/media/engine")}, "to": {"podman"}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	if exit := waitJob(t, h, w.Header().Get("Location")); exit != 0 {
		t.Fatalf("move exit %d", exit)
	}
	log, _ := os.ReadFile(h.dockerLog)
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "unix:///var/run/docker.sock compose -p media") || !strings.HasSuffix(lines[0], " down") ||
		!strings.HasPrefix(lines[1], "unix:///run/podman/podman.sock compose -p media") || !strings.HasSuffix(lines[1], " up -d") {
		t.Fatalf("move commands:\n%s", log)
	}
	m, _ := compose.ReadMeta(dir)
	if m.Engine != "podman" {
		t.Errorf("meta engine %q", m.Engine)
	}
	// The rewritten .fleetling.toml is Fleetling's own change, not drift.
	f, _ := compose.ReadFolder(dir)
	if rs, _ := h.s.reviewFor(context.Background(), f); rs.State != reviewOK {
		t.Errorf("review after move: %q", rs.State)
	}
	// Podman runs compose without BuildKit.
	tg, _ := h.s.target(mustSettings(t, h), f)
	if env := strings.Join(h.s.runner.Env(tg), " "); !strings.Contains(env, "DOCKER_BUILDKIT=0") || !strings.Contains(env, "DOCKER_HOST=unix:///run/podman/podman.sock") {
		t.Errorf("podman env: %s", env)
	}
}

func TestEngineMoveRefusesTakenProject(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	h.engines[store.DefaultPodmanHost] = &fakeEngine{info: engine.Info{Kind: engine.KindPodman},
		containers: []compose.Container{{Project: "media", Service: "a", Running: true}}}
	dir := filepath.Join(h.root, "media")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {a: {image: x}}\n"), 0o644)
	compose.WriteMeta(dir, &compose.Meta{Project: "media", Engine: "docker"}, compose.Owner{})
	h.approve(dir)
	w := b.do("POST", "/stacks/media/engine", url.Values{"csrf": {b.token("/stacks/media/engine")}, "to": {"podman"}}, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already runs on podman") {
		t.Errorf("taken project: %d %s", w.Code, w.Body.String())
	}
}

func TestPodmanRestartCheck(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	h.engines[store.DefaultPodmanHost] = &fakeEngine{info: engine.Info{Kind: engine.KindPodman, Version: "5.6"}}
	enabled := false
	h.inCtr = true
	h.s.checkUnit = func(_ context.Context, _ store.Settings, unit string) (bool, error) { return enabled, nil }
	test := func() string {
		return b.do("POST", "/settings/test", url.Values{"csrf": {b.token("/settings")}, "slot": {"podman"}, "podman_host": {store.DefaultPodmanHost}}, nil).Body.String()
	}
	if p := test(); !strings.Contains(p, "sudo systemctl enable --now podman-restart.service") {
		t.Errorf("missing unit not warned:\n%s", p)
	}
	enabled = true
	if p := test(); !strings.Contains(p, "podman-restart.service is enabled") {
		t.Errorf("enabled unit:\n%s", p)
	}
	// The file check itself: an enabled unit is a symlink whose target the
	// container can't see.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "default.target.wants"), 0o755)
	if UnitEnabled(dir, "podman-restart.service") {
		t.Error("enabled with no link")
	}
	os.Symlink("/usr/lib/systemd/system/podman-restart.service", filepath.Join(dir, "default.target.wants", "podman-restart.service"))
	if !UnitEnabled(dir, "podman-restart.service") {
		t.Error("dangling link not counted as enabled")
	}
	if got := strings.Join(unitCheckArgs("sha256:x", restartUnit), " "); got != "docker run --rm --network none -v /etc/systemd/system:/host/systemd:ro --entrypoint /usr/local/bin/fleetling sha256:x unitcheck podman-restart.service" {
		t.Errorf("check container: %s", got)
	}
}

func TestSwitcherOnAllResourcePages(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	h.engines[store.DefaultPodmanHost] = &fakeEngine{info: engine.Info{Kind: engine.KindPodman}}
	for _, p := range []string{"/containers", "/images", "/networks", "/volumes"} {
		page := b.do("GET", p+"?engine=podman", nil, nil).Body.String()
		if !strings.Contains(page, `<nav class="switch">`) || !strings.Contains(page, `href="`+p+`?engine=podman" aria-current="page"`) {
			t.Errorf("%s: no shared switcher with podman selected", p)
		}
	}
}

func mustSettings(t *testing.T, h *harness) store.Settings {
	t.Helper()
	st, err := h.s.store.LoadSettings(context.Background(), h.root)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

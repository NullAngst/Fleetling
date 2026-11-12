package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/store"
)

// fakeDocker stands in for the docker CLI. It logs every call with its
// DOCKER_HOST and acts out the compose subcommands Fleetling uses.
const fakeDocker = `#!/bin/sh
echo "$DOCKER_HOST $*" >> "$FAKE_DOCKER_LOG"
case " $* " in
  *" config -q "*)
    input=$(cat)
    case "$input" in *INVALID*) echo "services.app additional property bogus is not allowed" >&2; exit 15;; esac
    exit 0;;
  *" config --format json "*)
    # A stack folder can carry its own resolved config for the test.
    if [ -f .fake-config.json ]; then cat .fake-config.json; exit 0; fi
    printf '{"services":{"app":{"volumes":[{"type":"bind","source":"%s"}]}}}\n' "$FAKE_BIND"; exit 0;;
  *" up "*)
    [ -n "$FAKE_BIND" ] && mkdir -p "$FAKE_BIND"
    echo " Container app-1  Started"; exit 0;;
  *" pull "*) echo " app Pulled"; exit "${FAKE_PULL_EXIT:-0}";;
  *" down "*)
    # Something new shows up while down runs: a stack that uses a path.
    if [ -n "$FAKE_LATE_DIR" ]; then
      mkdir -p "$FAKE_LATE_DIR"
      printf 'services:\n  late:\n    image: x\n    volumes:\n      - %s:/d\n' "$FAKE_LATE_BIND" > "$FAKE_LATE_DIR/compose.yaml"
    fi
    echo " Container app-1  Removed"; exit "${FAKE_DOWN_EXIT:-0}";;
  *" logs "*) echo "app-1  | hello from app"; echo "db-1   | ready"; exit 0;;
esac
exit 0
`

func waitJob(t *testing.T, h *harness, location string) int {
	t.Helper()
	m := regexp.MustCompile(`job=([0-9a-f]+)`).FindStringSubmatch(location)
	if m == nil {
		t.Fatalf("no job in redirect %q", location)
	}
	j := h.s.jobs.Get(m[1])
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exit, err := j.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Done (the action log write) runs just after the job is marked done.
	for range 100 {
		if h.s.jobs.Running(j.Target) == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	return exit
}

const testCompose = "# test stack, keep this comment\nservices:\n  app:\n    image: alpine\n    volumes:\n      - ./data:/data\n"

// The phase 2 "done when": a throwaway test-stack goes create, update,
// down, with every command logged.
func TestStackLifecycle(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "test-stack")
	t.Setenv("FAKE_BIND", filepath.Join(dir, "data"))

	// Create and deploy. The browser sends CRLF; the file must not get it.
	w := b.do("POST", "/stacks/new", url.Values{
		"csrf": {b.token("/stacks/new")}, "project": {"test-stack"}, "folder": {""}, "engine": {"docker"},
		"compose": {strings.ReplaceAll(testCompose, "\n", "\r\n")}, "env": {"DB_PASSWORD=hunter2\r\n"}, "deploy": {"1"},
	}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if exit := waitJob(t, h, w.Header().Get("Location")); exit != 0 {
		t.Fatalf("deploy exit %d", exit)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if string(got) != testCompose {
		t.Errorf("compose.yaml not byte for byte:\n%q", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".env")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf(".env mode: %v %v", fi, err)
	}
	m, err := compose.ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Project != "test-stack" || m.Engine != "docker" || m.Deployed == nil || !slices.Equal(m.CreatedPaths, []string{filepath.Join(dir, "data")}) {
		t.Errorf("meta after deploy: %+v", m)
	}

	// The stack page shows Running-ish state, the actions and their commands.
	h.engines[store.DefaultDockerHost].containers = []compose.Container{{Project: "test-stack", Service: "app", WorkingDir: dir, Running: true}}
	page := b.do("GET", "/stacks/test-stack", nil, nil).Body.String()
	for _, want := range []string{"Running", "docker compose -p test-stack --project-directory " + dir, "pull &amp;&amp; docker compose", "Down"} {
		if !strings.Contains(page, want) {
			t.Errorf("stack page missing %q", want)
		}
	}
	if env := b.do("GET", "/stacks/test-stack?tab=env", nil, nil).Body.String(); !strings.Contains(env, `class="secret"`) {
		t.Error("DB_PASSWORD not masked on the env tab")
	}

	for _, action := range []string{"update", "down"} {
		w := b.do("POST", "/stacks/test-stack/action/"+action, url.Values{"csrf": {b.token("/stacks/test-stack")}}, nil)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", action, w.Code, w.Body.String())
		}
		if exit := waitJob(t, h, w.Header().Get("Location")); exit != 0 {
			t.Fatalf("%s exit %d", action, exit)
		}
	}

	log, _ := os.ReadFile(h.dockerLog)
	calls := string(log)
	prefix := "unix:///var/run/docker.sock compose -p test-stack --project-directory " + dir + " -f " + filepath.Join(dir, "compose.yaml") + " --ansi never --progress plain "
	for _, want := range []string{"config -q", prefix + "config --format json", prefix + "up -d", prefix + "pull", prefix + "down"} {
		if !strings.Contains(calls, want) {
			t.Errorf("docker never called with %q:\n%s", want, calls)
		}
	}
	if strings.Count(calls, "config --format json") != 1 {
		t.Errorf("path tracking ran after the first deploy:\n%s", calls)
	}

	actions, err := h.s.store.ListActions(context.Background(), "test-stack", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var cmds []string
	for _, a := range actions {
		if a.ExitCode == nil || *a.ExitCode != 0 {
			t.Errorf("action %q exit %v", a.Command, a.ExitCode)
		}
		cmds = append(cmds, a.Command)
	}
	all := strings.Join(cmds, "\n")
	for _, want := range []string{"fleetling: create stack test-stack", " up -d", " pull\n", " down"} {
		if !strings.Contains(all, want) {
			t.Errorf("action log missing %q:\n%s", want, all)
		}
	}
	if len(actions) != 4 {
		t.Errorf("want 4 log entries (create, deploy, update, down), got %d:\n%s", len(actions), all)
	}
	if !strings.Contains(actions[0].Output, "Removed") {
		t.Errorf("down output not stored: %q", actions[0].Output)
	}
}

func TestCreateValidation(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	form := url.Values{"csrf": {b.token("/stacks/new")}, "project": {"half"}, "engine": {"docker"}, "compose": {"services:\n  app:\n    bogus: INVALID\n"}}
	w := b.do("POST", "/stacks/new", form, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "additional property bogus") {
		t.Fatalf("invalid compose: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(h.root, "half")); err == nil {
		t.Fatal("folder created for a rejected compose file")
	}
	form.Set("force", "1")
	if w := b.do("POST", "/stacks/new", form, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("save anyway: %d", w.Code)
	}
	for _, c := range []struct{ project, folder, want string }{
		{"Bad", "", "lowercase"},
		{"ok", "../etc", "single name"},
		{"half", "other", "already uses that project name"},
		{"again", "half", "already holds a stack"},
	} {
		w := b.do("POST", "/stacks/new", url.Values{"csrf": {b.token("/stacks/new")}, "project": {c.project}, "folder": {c.folder}, "engine": {"docker"}, "compose": {"services: {}\n"}}, nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s/%s: %d, want %q", c.project, c.folder, w.Code, c.want)
		}
	}
}

func TestEditFlow(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "app")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: nginx:1\n"), 0o640)
	os.WriteFile(filepath.Join(dir, "stack.env"), []byte("A=1\n"), 0o600)
	os.Symlink("stack.env", filepath.Join(dir, ".env"))
	compose.WriteMeta(dir, &compose.Meta{Project: "app", Engine: "docker", Deployed: ptr(time.Now())}, compose.Owner{})
	h.approve(dir)

	edit := b.do("GET", "/stacks/app/edit", nil, nil).Body.String()
	hash := regexp.MustCompile(`name="hash" value="([0-9a-f]+)"`).FindStringSubmatch(edit)[1]
	tok := b.token("/stacks/app/edit")
	newCompose := "services:\n  web:\n    image: nginx:2\n"
	form := url.Values{"csrf": {tok}, "hash": {hash}, "compose": {newCompose}, "env": {"A=2\n"}, "step": {"review"}}
	w := b.do("POST", "/stacks/app/edit", form, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `class="d-change"`) || !strings.Contains(w.Body.String(), "nginx:2") {
		t.Fatalf("review: %d %s", w.Code, w.Body.String())
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "compose.yaml")); string(got) == newCompose {
		t.Fatal("review wrote the file")
	}
	form.Set("step", "save_deploy")
	w = b.do("POST", "/stacks/app/edit", form, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("save and redeploy: %d %s", w.Code, w.Body.String())
	}
	waitJob(t, h, w.Header().Get("Location"))
	got, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	fi, _ := os.Stat(filepath.Join(dir, "compose.yaml"))
	if string(got) != newCompose || fi.Mode().Perm() != 0o640 {
		t.Errorf("compose after save: %q mode %v", got, fi.Mode())
	}
	if env, _ := os.ReadFile(filepath.Join(dir, "stack.env")); string(env) != "A=2\n" {
		t.Errorf("env not written through the symlink: %q", env)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, ".env")); fi.Mode()&os.ModeSymlink == 0 {
		t.Error(".env symlink was replaced by a file")
	}
	if log, _ := os.ReadFile(h.dockerLog); !strings.Contains(string(log), "up -d --remove-orphans") {
		t.Error("save and redeploy did not run up -d --remove-orphans")
	}

	// The stale hash from before the save is now refused.
	form.Set("step", "save")
	if w := b.do("POST", "/stacks/app/edit", form, nil); w.Code != http.StatusConflict {
		t.Errorf("stale save: %d", w.Code)
	}
}

func TestManageOnDiskFolder(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "npmplus_data")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {npmplus: {image: x}}\n"), 0o644)
	h.engines[store.DefaultDockerHost].containers = []compose.Container{{Project: "npmplus", Service: "npmplus", WorkingDir: dir, Running: true}}
	if !strings.Contains(b.do("GET", "/stacks/npmplus_data", nil, nil).Body.String(), "Manage") {
		t.Fatal("no Manage button on an On disk folder")
	}
	if w := b.do("POST", "/stacks/npmplus_data/action/down", url.Values{"csrf": {b.token("/stacks/npmplus_data")}}, nil); w.Code != http.StatusBadRequest {
		t.Errorf("action on an unmanaged folder: %d", w.Code)
	}
	w := b.do("POST", "/stacks/npmplus_data/manage", url.Values{"csrf": {b.token("/stacks/npmplus_data")}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("manage: %d %s", w.Code, w.Body.String())
	}
	m, err := compose.ReadMeta(dir)
	if err != nil || m.Project != "npmplus" || m.Engine != "docker" || m.Deployed == nil {
		t.Fatalf("meta: %+v %v", m, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "compose.yaml")); string(b) != "services: {npmplus: {image: x}}\n" {
		t.Error("manage touched the compose file")
	}
}

// Phase 7: Fleetling's own stack runs Update and Restart through the
// helper container, refuses Stop, Down and Remove, and passes drift review
// like every other stack.
func TestSelfStackUsesHelper(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "fleetling")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {fleetling: {image: x}}\n"), 0o644)
	compose.WriteMeta(dir, &compose.Meta{Project: "fleetling", Engine: "docker"}, compose.Owner{})
	h.approve(dir)
	h.inCtr = true
	fe := h.engines[store.DefaultDockerHost]
	fe.mounts = []compose.Mount{{Type: "bind", Source: h.root, Destination: h.root}}
	fe.labels = map[string]string{"com.docker.compose.project": "fleetling", "com.docker.compose.project.working_dir": dir}
	fe.imageID = "sha256:selfimage"
	tok := b.token("/stacks/fleetling")

	for _, a := range []string{"down", "stop", "deploy"} {
		if w := b.do("POST", "/stacks/fleetling/action/"+a, url.Values{"csrf": {tok}}, nil); w.Code != http.StatusConflict {
			t.Errorf("%s on self: %d", a, w.Code)
		}
	}
	if w := b.do("POST", "/stacks/fleetling/remove", url.Values{"csrf": {tok}}, nil); w.Code == http.StatusSeeOther {
		t.Error("remove on self was accepted")
	}
	page := b.do("GET", "/stacks/fleetling", nil, nil).Body.String()
	for _, want := range []string{
		"docker run --rm -d --name fleetling-updater -v /var/run/docker.sock:/var/run/docker.sock -v " + h.root + ":" + h.root,
		"sha256:selfimage updater --dir " + dir + " --mode update",
		"--mode restart",
		"Down removes Fleetling", "leaves nothing running",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("self stack page missing %q", want)
		}
	}

	// Update starts the helper and sends the browser to the waiting page.
	w := b.do("POST", "/stacks/fleetling/action/update", url.Values{"csrf": {tok}}, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/self/updating?mode=update" {
		t.Fatalf("update on self: %d %s %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	log, _ := os.ReadFile(h.dockerLog)
	if !strings.Contains(string(log), "unix:///var/run/docker.sock run --rm -d --name fleetling-updater") ||
		!strings.Contains(string(log), "-e FLEETLING_ROOT="+h.root) || !strings.Contains(string(log), "--dir "+dir+" --mode update") {
		t.Errorf("helper not started as expected:\n%s", log)
	}
	acts, _ := h.s.store.ListActions(context.Background(), "fleetling", 5, 0)
	if len(acts) == 0 || !strings.Contains(acts[0].Command, "fleetling-updater") {
		t.Errorf("helper start not in the action log: %+v", acts)
	}
	if p := b.do("GET", "/self/updating?mode=update", nil, nil).Body.String(); !strings.Contains(p, `data-boot="`+h.s.boot+`"`) {
		t.Error("updating page has no boot ID to compare against")
	}
	if hz := b.do("GET", "/healthz", nil, nil); hz.Header().Get("X-Fleetling-Boot") != h.s.boot {
		t.Error("/healthz does not send the boot ID")
	}

	// A second helper is refused while the first exists.
	fe.exists = map[string]bool{"fleetling-updater": true}
	if w := b.do("POST", "/stacks/fleetling/action/restart", url.Values{"csrf": {tok}}, nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already running") {
		t.Errorf("second helper: %d %s", w.Code, w.Body.String())
	}
	fe.exists = nil

	// An unreviewed change to Fleetling's own compose file blocks its update.
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {fleetling: {image: evil}}\n"), 0o644)
	if w := b.do("POST", "/stacks/fleetling/action/update", url.Values{"csrf": {tok}}, nil); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "/review") {
		t.Errorf("drifted self update: %d %s", w.Code, w.Header().Get("Location"))
	}

	// The log page shows last-update.log, and only to a logged-in browser.
	os.MkdirAll(h.s.cfg.DataDir, 0o700)
	os.WriteFile(filepath.Join(h.s.cfg.DataDir, "last-update.log"), []byte("x rolling back\nx FAILED: rolled back\n"), 0o600)
	if p := b.do("GET", "/self/last-update", nil, nil).Body.String(); !strings.Contains(p, "rolling back") || !strings.Contains(p, "did not finish cleanly") {
		t.Error("last update page does not show the log")
	}
	if w := h.browser().do("GET", "/self/log", nil, nil); w.Code != http.StatusSeeOther {
		t.Errorf("log readable without a session: %d", w.Code)
	}
}

func TestJobWebsocket(t *testing.T) {
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

	srv := httptest.NewServer(h.s)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Add("Cookie", sessionCookie+"="+b.cookies[sessionCookie].Value)

	// No session: refused before the upgrade.
	if _, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/jobs/"+id+"/ws", nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated dial: %v %v", err, resp)
	}
	// Wrong Origin: refused.
	bad := hdr.Clone()
	bad.Set("Origin", "https://evil.example")
	if _, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/jobs/"+id+"/ws", &websocket.DialOptions{HTTPHeader: bad}); err == nil {
		t.Error("cross-origin websocket accepted")
	}

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/jobs/"+id+"/ws", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	var lines []string
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (lines so far %v)", err, lines)
		}
		var msg jobMessage
		json.Unmarshal(data, &msg)
		lines = append(lines, msg.Lines...)
		if msg.Done {
			if msg.Exit != 0 {
				t.Errorf("exit %d", msg.Exit)
			}
			break
		}
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "$ docker compose -p ws") || !strings.Contains(out, "Removed") {
		t.Errorf("stream:\n%s", out)
	}
}

func TestSideBySide(t *testing.T) {
	rows, changed, ok := sideBySide("a\nb\nc\n", "a\nB\nc\nd\n")
	if !ok || !changed {
		t.Fatal("no diff")
	}
	kinds := []string{}
	for _, r := range rows {
		kinds = append(kinds, r.Kind)
	}
	if !slices.Equal(kinds, []string{"same", "change", "same", "add"}) {
		t.Errorf("kinds %v", kinds)
	}
	if _, changed, _ := sideBySide("x\n", "x\n"); changed {
		t.Error("identical texts reported as changed")
	}
	if _, changed, _ := sideBySide("x", "x\n"); !changed {
		t.Error("a trailing newline change was missed")
	}
}

func TestNormalizeNewlinesAndEnvLines(t *testing.T) {
	if got := normalizeNewlines("a\r\nb\r\n", "a\nb\n"); got != "a\nb\n" {
		t.Errorf("got %q", got)
	}
	if got := normalizeNewlines("a\r\nb\r\n", "a\r\nb\r\n"); got != "a\r\nb\r\n" {
		t.Errorf("CRLF file lost its CRLF: %q", got)
	}
	lines := envLines("# c\nDB_PASSWORD=x\nTZ=UTC\nexport API_KEY=y\n")
	secret := map[string]bool{}
	for _, l := range lines {
		if l.IsVar {
			secret[l.Key] = l.Secret
		}
	}
	if !secret["DB_PASSWORD"] || secret["TZ"] || !secret["API_KEY"] {
		t.Errorf("secrets %v", secret)
	}
}

func ptr[T any](v T) *T { return &v }

package server

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The phase 9 done-when, against the fake docker CLI: a Dockerfile in
// /opt/<folder>/build builds from the stack page, then redeploys.
func TestStackBuildAndRedeploy(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "webapp")
	os.MkdirAll(filepath.Join(dir, "build"), 0o755)
	os.WriteFile(filepath.Join(dir, "build", "Dockerfile"), []byte("FROM scratch\nCOPY app /app\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "build", "app"), []byte("v1"), 0o644)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    build: ./build\n    image: webapp:local\n"), 0o644)
	if w := b.do("POST", "/stacks/webapp/manage", url.Values{"csrf": {b.token("/stacks/webapp")}}, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("manage: %d", w.Code)
	}
	if !strings.Contains(b.do("GET", "/stacks/webapp", nil, nil).Body.String(), `href="/stacks/webapp/build"`) {
		t.Fatal("no Build button for a stack with build:")
	}
	page := b.do("GET", "/stacks/webapp/build", nil, nil).Body.String()
	if !strings.Contains(page, "--progress plain build --pull --no-cache") {
		t.Error("build page lacks the toggled command variants")
	}
	w := b.do("POST", "/stacks/webapp/build", url.Values{"csrf": {b.token("/stacks/webapp/build")}, "pull": {"1"}}, nil)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "built=1") {
		t.Fatalf("build: %d %s", w.Code, w.Body.String())
	}
	if exit := waitJob(t, h, w.Header().Get("Location")); exit != 0 {
		t.Fatalf("build exit %d", exit)
	}
	page = b.do("GET", w.Header().Get("Location"), nil, nil).Body.String()
	if !strings.Contains(page, "redeploy to run the new images") {
		t.Fatal("no redeploy offer after the build")
	}
	w = b.do("POST", "/stacks/webapp/action/redeploy", url.Values{"csrf": {b.token("/stacks/webapp")}}, nil)
	if exit := waitJob(t, h, w.Header().Get("Location")); exit != 0 {
		t.Fatalf("redeploy exit %d", exit)
	}
	log, _ := os.ReadFile(h.dockerLog)
	if !strings.Contains(string(log), "--progress plain build --pull\n") || !strings.Contains(string(log), "up -d --remove-orphans") {
		t.Errorf("docker log:\n%s", log)
	}

	// A change to the build context made outside Fleetling blocks the build.
	os.WriteFile(filepath.Join(dir, "build", "app"), []byte("tampered"), 0o644)
	w = b.do("POST", "/stacks/webapp/build", url.Values{"csrf": {b.token("/stacks/webapp/build")}}, nil)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "/review") {
		t.Errorf("drifted build context: %d %s", w.Code, w.Header().Get("Location"))
	}
	if rv := b.do("GET", "/stacks/webapp/review", nil, nil).Body.String(); !strings.Contains(rv, "build/app") {
		t.Error("review page doesn't list the changed context file")
	}
}

func TestStandaloneBuild(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	ctxDir := filepath.Join(h.root, "tools", "img")
	os.MkdirAll(ctxDir, 0o755)
	os.WriteFile(filepath.Join(ctxDir, "Dockerfile.prod"), []byte("FROM scratch\n"), 0o644)
	tok := b.token("/builds")
	form := url.Values{"csrf": {tok}, "context": {"tools/img"}, "dockerfile": {"Dockerfile.prod"}, "tags": {"myapp:latest, registry.lan/myapp:1"},
		"arg_key": {"VERSION", ""}, "arg_val": {"1.2 beta", ""}, "target": {"runtime"}, "platform": {"linux/amd64"}, "pull": {"1"}, "do": {"check"}}
	w := b.do("POST", "/builds", form, nil)
	want := "docker build --progress plain -f " + filepath.Join(ctxDir, "Dockerfile.prod") + " -t myapp:latest -t registry.lan/myapp:1 --build-arg 'VERSION=1.2 beta' --target runtime --platform linux/amd64 --pull -- " + ctxDir
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), html.EscapeString(want)) {
		t.Fatalf("check: %d, want %q in\n%s", w.Code, want, w.Body.String())
	}
	// Build refuses a command the user wasn't shown.
	form.Set("do", "run")
	form.Set("shown", "docker build something else")
	if w := b.do("POST", "/builds", form, nil); w.Code != http.StatusConflict {
		t.Errorf("unshown command ran: %d", w.Code)
	}
	form.Set("shown", want)
	w = b.do("POST", "/builds", form, nil)
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/jobs/") {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}
	id := strings.TrimPrefix(w.Header().Get("Location"), "/jobs/")
	j := h.s.jobs.Get(id)
	if exit, _ := j.Wait(context.Background()); exit != 0 {
		t.Fatalf("build exit %d", exit)
	}
	log, _ := os.ReadFile(h.dockerLog)
	if !strings.Contains(string(log), "build --progress plain -f "+filepath.Join(ctxDir, "Dockerfile.prod")+" -t myapp:latest") {
		t.Errorf("docker log:\n%s", log)
	}

	// Recipes: save, run with one click, delete.
	form.Set("do", "save")
	form.Set("name", "my app")
	if w := b.do("POST", "/builds", form, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	page := b.do("GET", "/builds", nil, nil).Body.String()
	if !strings.Contains(page, `action="/builds/recipes/my%20app/run"`) || !strings.Contains(page, html.EscapeString(want)) {
		t.Fatal("recipe not listed with its command")
	}
	w = b.do("POST", "/builds/recipes/my%20app/run", url.Values{"csrf": {tok}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("recipe run: %d %s", w.Code, w.Body.String())
	}
	h.s.jobs.Get(strings.TrimPrefix(w.Header().Get("Location"), "/jobs/")).Wait(context.Background())
	if w := b.do("POST", "/builds/recipes/my%20app/delete", url.Values{"csrf": {tok}}, nil); w.Code != http.StatusSeeOther {
		t.Errorf("delete: %d", w.Code)
	}
	if recs, _ := h.s.store.ListRecipes(context.Background()); len(recs) != 0 {
		t.Errorf("recipe still there: %+v", recs)
	}
}

func TestBuildGitTokenStaysOutOfLog(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	tok := b.token("/builds")
	form := url.Values{"csrf": {tok}, "context": {"https://user:ghp_secret123@github.com/me/app.git#main"}, "tags": {"app:dev"}, "do": {"check"}}
	w := b.do("POST", "/builds", form, nil)
	shown := "docker build --progress plain -t app:dev -- 'https://github.com/me/app.git#main'"
	if !strings.Contains(w.Body.String(), "https://github.com/me/app.git#main") || strings.Contains(w.Body.String(), "ghp_secret123</p>") {
		t.Fatalf("check page:\n%s", w.Body.String())
	}
	form.Set("do", "run")
	form.Set("shown", shown)
	w = b.do("POST", "/builds", form, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}
	j := h.s.jobs.Get(strings.TrimPrefix(w.Header().Get("Location"), "/jobs/"))
	j.Wait(context.Background())
	waitDone(h, j.Target)
	acts, _ := h.s.store.ListActions(context.Background(), "build/app", 5, 0)
	if len(acts) != 1 || acts[0].Command != shown || strings.Contains(acts[0].Command+acts[0].Output, "ghp_secret123") {
		t.Errorf("action log: %+v", acts)
	}
	// The real command still has the token, or a private repo can't build.
	if log, _ := os.ReadFile(h.dockerLog); !strings.Contains(string(log), "user:ghp_secret123@github.com") {
		t.Errorf("token dropped from the real command:\n%s", log)
	}
}

func TestBuildRefusals(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	os.MkdirAll(filepath.Join(h.root, "ok"), 0o755)
	os.WriteFile(filepath.Join(h.root, "ok", "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	os.Symlink(outside, filepath.Join(h.root, "escape"))
	os.Symlink("/etc/passwd", filepath.Join(h.root, "ok", "Dockerfile.evil"))
	tok := b.token("/builds")
	for _, c := range []struct{ ctx, df, tags, want string }{
		{"file:///etc", "", "a:b", "only https:// and git@"},
		{"ssh://git@github.com/x", "", "a:b", "only https:// and git@"},
		{"../etc", "", "a:b", "must be a folder under"},
		{"/etc", "", "a:b", "must be a folder under"},
		{"escape", "", "a:b", "resolves outside"},
		{"ok", "../../etc/passwd", "a:b", "stay inside it"},
		{"ok", "", "", "at least one tag"},
		{"ok", "Dockerfile.evil", "a:b", "resolves outside the context"},
		{"ok", "", "UPPER:case", "tag"},
		{"git@github.com:me/app.git; rm -rf /", "", "a:b", "git@ URL looks like"},
	} {
		w := b.do("POST", "/builds", url.Values{"csrf": {tok}, "context": {c.ctx}, "dockerfile": {c.df}, "tags": {c.tags}, "do": {"check"}}, nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s/%s/%s: %d, want %q", c.ctx, c.df, c.tags, w.Code, c.want)
		}
	}
	w := b.do("POST", "/builds", url.Values{"csrf": {tok}, "context": {"ok"}, "tags": {"a:b"}, "arg_key": {"1BAD"}, "arg_val": {"x"}, "do": {"check"}}, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad build arg name: %d", w.Code)
	}
}

// waitDone waits for a job's Done hook (the action log write) to run.
func waitDone(h *harness, target string) {
	for range 200 {
		if h.s.jobs.Running(target) == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
}

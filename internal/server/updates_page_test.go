package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/release"
	"github.com/NullAngst/Fleetling/internal/store"
)

func (f *fakeEngine) SystemInfo(context.Context) (engine.SystemInfo, error) {
	return engine.SystemInfo{Name: "testhost", ServerVersion: "29.8.2", OS: "TestOS 1.0", Kernel: "6.12.0-test",
		Arch: "x86_64", CPUs: 4, Memory: 8 << 30, StorageDriver: "overlayfs", Containers: 3, Running: 2}, f.err
}

// fakeGitHub answers the three version requests: mainVersion on main, no
// releases, and a commit dated commitAt.
func fakeGitHub(t *testing.T, h *harness, mainVersion string, commitAt time.Time) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/main/VERSION"):
			w.Write([]byte(mainVersion + "\n"))
		case strings.HasSuffix(r.URL.Path, "/commits/main"):
			w.Write([]byte(`{"sha":"abcdef0123456789","html_url":"https://example/c","commit":{"committer":{"date":"` + commitAt.UTC().Format(time.RFC3339) + `"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	h.s.rel = release.New("test")
	h.s.rel.API, h.s.rel.Raw = srv.URL+"/api", srv.URL+"/raw"
	return &hits
}

// selfAndApp sets up Fleetling's own managed stack plus one other managed
// stack and one container that belongs to neither.
func selfAndApp(t *testing.T) (*harness, *browser) {
	t.Helper()
	h := newHarness(t)
	h.s.cfg.Version = "1.0.0"
	b := h.browser()
	h.setUp(b)
	for _, name := range []string{"fleetling", "app"} {
		dir := filepath.Join(h.root, name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {"+name+": {image: x}}\n"), 0o644)
		compose.WriteMeta(dir, &compose.Meta{Project: name, Engine: "docker"}, compose.Owner{})
		h.approve(dir)
	}
	h.inCtr = true
	fe := h.engines[store.DefaultDockerHost]
	fe.mounts = []compose.Mount{{Type: "bind", Source: h.root, Destination: h.root}}
	fe.labels = map[string]string{"com.docker.compose.project": "fleetling", "com.docker.compose.project.working_dir": filepath.Join(h.root, "fleetling")}
	fe.imageID = "sha256:selfimage"
	fe.images = map[string]engine.ImageInfo{"sha256:selfimage": {ID: "sha256:selfimage", Created: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}}
	fe.rows = []engine.ContainerRow{
		{ID: "aaa111", Name: "app-app-1", Image: "x", State: "running", Project: "app"},
		{ID: "bbb222", Name: "loose", Image: "nginx:latest", State: "running"},
	}
	return h, b
}

func TestUpdatesPage(t *testing.T) {
	h, b := selfAndApp(t)
	hits := fakeGitHub(t, h, "1.1.0", time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))

	page := b.do("GET", "/updates", nil, nil).Body.String()
	for _, want := range []string{
		`action="/stacks/fleetling/update-now"`, "Update Fleetling", "fleetling-updater", // its own box
		`hx-get="/updates/stack/app"`,                                        // app in the stacks table
		"loose", "nginx:latest", `hx-get="/updates/container/docker/bbb222"`, // the stray container
		"Update all",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("updates page missing %q", want)
		}
	}
	if strings.Contains(page, `hx-get="/updates/stack/fleetling"`) {
		t.Error("Fleetling's own stack listed among the stacks")
	}
	if strings.Contains(page, `/updates/container/docker/aaa111`) {
		t.Error("a managed stack's container listed as unmanaged")
	}
	if hits.Load() != 0 {
		t.Error("the page itself asked GitHub; that belongs in the partial")
	}

	v := b.do("GET", "/updates/fleetling", nil, nil).Body.String()
	if !strings.Contains(v, "v1.1.0 is on") || !strings.Contains(v, "update available") || !strings.Contains(v, "built 2026-11-18 12:00") {
		t.Errorf("version partial:\n%s", v)
	}
	if !strings.Contains(b.do("GET", "/", nil, nil).Body.String(), "nav-badge") {
		t.Error("no sidebar badge with a newer version on main")
	}

	// Turned off: no requests, and the partial says so.
	before := hits.Load()
	w := b.do("POST", "/updates/github", url.Values{"csrf": {b.token("/updates")}, "check": {"off"}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("toggle: %d", w.Code)
	}
	v = b.do("GET", "/updates/fleetling", nil, nil).Body.String()
	if !strings.Contains(v, "check is off") || hits.Load() != before {
		t.Errorf("check off, but: %d requests, partial:\n%s", hits.Load()-before, v)
	}
}

func TestCompareSelf(t *testing.T) {
	h := newHarness(t)
	h.s.cfg.Version = "1.2.0"
	built := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name              string
		st                release.Status
		main, commit, tag bool
	}{
		{"same version, older commit", release.Status{MainVersion: "1.2.0", MainDate: built.Add(-time.Hour)}, false, false, false},
		{"commit a minute after the build", release.Status{MainVersion: "1.2.0", MainDate: built.Add(time.Minute)}, false, false, false},
		{"dependency update since", release.Status{MainVersion: "1.2.0", MainDate: built.Add(24 * time.Hour)}, false, true, false},
		{"new version on main", release.Status{MainVersion: "1.3.0", MainDate: built.Add(24 * time.Hour)}, true, false, false},
		{"rollover", release.Status{MainVersion: "2.0.0"}, true, false, false},
		{"newer release", release.Status{MainVersion: "1.2.0", Release: "V1.3.0"}, false, false, true},
		{"older main", release.Status{MainVersion: "1.1.0"}, false, false, false},
	} {
		v := h.s.compareSelf(c.st, built)
		if v.OnMain != c.main || v.NewCommit != c.commit || v.NewTag != c.tag {
			t.Errorf("%s: main=%v commit=%v tag=%v", c.name, v.OnMain, v.NewCommit, v.NewTag)
		}
	}
}

// Update all runs every managed stack's update except Fleetling's own,
// which only goes through its helper from its own button.
func TestUpdateAllSkipsFleetling(t *testing.T) {
	h, b := selfAndApp(t)
	os.Remove(h.dockerLog)
	w := b.do("POST", "/updates/all", url.Values{"csrf": {b.token("/updates")}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("update all: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/stacks/app?tab=updates"`) || strings.Contains(body, `href="/stacks/fleetling?tab=updates"`) {
		t.Errorf("update all result:\n%s", body)
	}
	log, _ := os.ReadFile(h.dockerLog)
	if strings.Contains(string(log), "fleetling-updater") {
		t.Errorf("update all started Fleetling's helper:\n%s", log)
	}
}

func TestAboutPage(t *testing.T) {
	h, b := selfAndApp(t)
	page := b.do("GET", "/about", nil, nil).Body.String()
	for _, want := range []string{"About Fleetling", "v1.0.0", "v1.1.0", "TestOS 1.0", "6.12.0-test", "4 cores", "8.0 GiB", "sha256:selfimage", "built 2026-11-18 12:00", h.root} {
		if !strings.Contains(page, want) {
			t.Errorf("about page missing %q", want)
		}
	}
	if !strings.Contains(b.do("GET", "/", nil, nil).Body.String(), `href="/about"`) {
		t.Error("no About link in the sidebar")
	}
}

// Fleetling's own folder, not managed yet, offers Manage. Without it there
// was no way to reach self-update from the UI on a fresh install.
func TestSelfFolderCanBeManaged(t *testing.T) {
	h := newHarness(t)
	h.s.cfg.Version = "1.0.0"
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "fleetling")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {fleetling: {image: x}}\n"), 0o644)
	h.inCtr = true
	fe := h.engines[store.DefaultDockerHost]
	fe.mounts = []compose.Mount{{Type: "bind", Source: h.root, Destination: h.root}}
	fe.labels = map[string]string{"com.docker.compose.project": "fleetling", "com.docker.compose.project.working_dir": dir}
	fe.containers = []compose.Container{{Engine: "docker", Project: "fleetling", Service: "fleetling", WorkingDir: dir, Running: true}}
	fe.imageID = "sha256:selfimage"

	if up := b.do("GET", "/updates", nil, nil).Body.String(); !strings.Contains(up, `href="/stacks/fleetling">Open fleetling`) {
		t.Error("the Updates page doesn't point at Fleetling's unmanaged folder")
	}
	page := b.do("GET", "/stacks/fleetling", nil, nil).Body.String()
	if !strings.Contains(page, `action="/stacks/fleetling/manage"`) {
		t.Fatal("no Manage button on Fleetling's own unmanaged folder")
	}
	w := b.do("POST", "/stacks/fleetling/manage", url.Values{"csrf": {b.token("/stacks/fleetling")}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("manage: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, compose.MetaFile)); err != nil {
		t.Fatal("not managed after Manage")
	}
	if up := b.do("GET", "/updates", nil, nil).Body.String(); !strings.Contains(up, `action="/stacks/fleetling/update-now"`) {
		t.Error("no Update Fleetling button once managed")
	}
}

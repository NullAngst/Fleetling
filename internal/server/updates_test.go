package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/schedule"
	"github.com/NullAngst/Fleetling/internal/store"
)

// updateStack makes a managed, approved stack with one service whose
// resolved config names image.
func updateStack(t *testing.T, h *harness, name, image string) string {
	t.Helper()
	dir := filepath.Join(h.root, name)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: "+image+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".fake-config.json"), []byte(`{"services":{"web":{"image":"`+image+`"}}}`), 0o644)
	compose.WriteMeta(dir, &compose.Meta{Project: name, Engine: "docker", Deployed: ptr(time.Now())}, compose.Owner{})
	h.approve(dir)
	return dir
}

// The phase 11 done-when, second half: a scheduled run recreates only the
// stacks whose images changed, and skips a stack with unreviewed changes.
func TestScheduledUpdates(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	fe := h.engines[store.DefaultDockerHost]
	fe.project = []engine.ProjectContainer{
		{Name: "moved-web-1", Service: "web", ImageID: "sha256:old", Running: true},
		{Name: "steady-web-1", Service: "web", ImageID: "sha256:same", Running: true},
		{Name: "drifted-web-1", Service: "web", ImageID: "sha256:d", Running: true},
	}
	// After the pull, the moved stack's tag points at a new image.
	fe.images = map[string]engine.ImageInfo{"moved:latest": {ID: "sha256:new"}, "steady:latest": {ID: "sha256:same"}, "drifted:latest": {ID: "sha256:d2"}}
	updateStack(t, h, "moved", "moved:latest")
	updateStack(t, h, "steady", "steady:latest")
	drifted := updateStack(t, h, "drifted", "drifted:latest")
	ctx := context.Background()
	for _, n := range []string{"moved", "steady", "drifted"} {
		h.s.store.SetSchedule(ctx, n, schedule.Daily, true)
	}
	h.s.store.SetSchedule(ctx, "weekly-only", schedule.Weekly, true)
	os.WriteFile(filepath.Join(drifted, "compose.yaml"), []byte("services:\n  web:\n    image: drifted:latest\n    privileged: true\n"), 0o644)

	// 04:00 on a Thursday: the daily ones fire, the weekly one doesn't.
	at := time.Date(2026, 11, 12, 4, 0, 0, 0, time.Local)
	started := h.s.runSchedules(at)
	if strings.Join(started, ",") != "drifted,moved,steady" {
		t.Errorf("started %v", started)
	}
	for _, n := range []string{"moved", "steady"} {
		if j := h.s.jobs.Running(n); j != nil {
			j.Wait(ctx)
		}
		waitDone(h, n)
	}
	log, _ := os.ReadFile(h.dockerLog)
	calls := string(log)
	for _, c := range []struct {
		stack string
		up    bool
	}{{"moved", true}, {"steady", false}} {
		prefix := "compose -p " + c.stack + " "
		if !strings.Contains(calls, prefix) || !strings.Contains(calls, "pull --ignore-buildable") {
			t.Errorf("%s not pulled:\n%s", c.stack, calls)
		}
		upLine := false
		for _, l := range strings.Split(calls, "\n") {
			if strings.Contains(l, prefix) && strings.HasSuffix(l, " up -d") {
				upLine = true
			}
		}
		if upLine != c.up {
			t.Errorf("%s: up -d ran = %v, want %v\n%s", c.stack, upLine, c.up, calls)
		}
	}
	if strings.Contains(calls, "-p drifted") {
		t.Errorf("drifted stack ran compose:\n%s", calls)
	}

	// Results: ok, ok, skipped. The skip is in the action log with its
	// reason, and the stacks page shows both the review flag and the skip.
	for n, want := range map[string]string{"moved": store.UpdateOK, "steady": store.UpdateOK, "drifted": store.UpdateSkipped} {
		sc, _, _ := h.s.store.GetSchedule(ctx, n)
		if sc.LastResult != want {
			t.Errorf("%s: result %q, want %q (%s)", n, sc.LastResult, want, sc.LastMsg)
		}
	}
	acts, _ := h.s.store.ListActions(ctx, "drifted", 5, 0)
	if len(acts) == 0 || !strings.Contains(acts[0].Command, "update skipped") || !strings.Contains(acts[0].Output, "changed outside Fleetling") {
		t.Errorf("skip not logged: %+v", acts)
	}
	page := b.do("GET", "/", nil, nil).Body.String()
	if !strings.Contains(page, "changed outside Fleetling, review") || !strings.Contains(page, "auto-update skipped") {
		t.Error("stacks page doesn't flag the skipped stack")
	}

	// A failing pull: the result and the badge say so until the next good run.
	t.Setenv("FAKE_PULL_EXIT", "1")
	w := b.do("POST", "/stacks/steady/update-now", url.Values{"csrf": {b.token("/stacks/steady")}}, nil)
	if exit := waitJob(t, h, w.Header().Get("Location")); exit == 0 {
		t.Fatal("failing pull reported success")
	}
	if sc, _, _ := h.s.store.GetSchedule(ctx, "steady"); sc.LastResult != store.UpdateFailed {
		t.Errorf("failed run recorded as %q", sc.LastResult)
	}
	if !strings.Contains(b.do("GET", "/", nil, nil).Body.String(), "auto-update failed") {
		t.Error("no failure badge")
	}
	t.Setenv("FAKE_PULL_EXIT", "0")
	w = b.do("POST", "/stacks/steady/update-now", url.Values{"csrf": {b.token("/stacks/steady")}}, nil)
	waitJob(t, h, w.Header().Get("Location"))
	if strings.Contains(b.do("GET", "/", nil, nil).Body.String(), "auto-update failed") {
		t.Error("badge stayed after a good run")
	}
	// The manual button on a drifted stack goes to review, never updates.
	w = b.do("POST", "/stacks/drifted/update-now", url.Values{"csrf": {b.token("/stacks/drifted")}}, nil)
	if !strings.Contains(w.Header().Get("Location"), "/review") {
		t.Errorf("manual update on a drifted stack: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestScheduleForm(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	updateStack(t, h, "app", "nginx:latest")
	tok := b.token("/stacks/app")
	if w := b.do("POST", "/stacks/app/schedule", url.Values{"csrf": {tok}, "mode": {"custom"}, "cron": {"61 * * * *"}}, nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad cron: %d", w.Code)
	}
	if w := b.do("POST", "/stacks/app/schedule", url.Values{"csrf": {tok}, "mode": {"weekly"}}, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("weekly: %d %s", w.Code, w.Body.String())
	}
	sc, _, _ := h.s.store.GetSchedule(context.Background(), "app")
	if sc.Cron != schedule.Weekly || !sc.Enabled {
		t.Errorf("saved %+v", sc)
	}
	page := b.do("GET", "/stacks/app?tab=updates", nil, nil).Body.String()
	for _, want := range []string{"container's time zone", schedule.Zone(), "Next run", "weekly, Sunday at 04:00", "pull --ignore-buildable", "only if an image ID changed", `hx-get="/stacks/app/updates/check"`} {
		if !strings.Contains(page, want) && !strings.Contains(page, strings.ReplaceAll(want, "'", "&#39;")) {
			t.Errorf("updates tab missing %q", want)
		}
	}
	b.do("POST", "/stacks/app/schedule", url.Values{"csrf": {tok}, "mode": {"off"}}, nil)
	if sc, _, _ := h.s.store.GetSchedule(context.Background(), "app"); sc.Enabled {
		t.Error("off didn't turn it off")
	}
}

// The phase 11 done-when, first half: a stack on an old tag shows the
// badge, checked against an httptest registry.
func TestUpdateAvailableBadge(t *testing.T) {
	reg := httptest.NewServer(registry.New())
	defer reg.Close()
	host := strings.TrimPrefix(reg.URL, "http://")
	pushRandom := func(ref string) string {
		img, _ := random.Image(128, 1)
		r, _ := name.ParseReference(ref)
		if err := remote.Write(r, img); err != nil {
			t.Fatal(err)
		}
		d, _ := img.Digest()
		return d.String()
	}
	ref := host + "/app:latest"
	old := pushRandom(ref)
	pushRandom(ref) // the tag moves on; the local image is still the old one

	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	updateStack(t, h, "app", ref)
	fe := h.engines[store.DefaultDockerHost]
	fe.project = []engine.ProjectContainer{{Name: "app-web-1", Service: "web", ImageID: "sha256:local", Running: true}}
	fe.images = map[string]engine.ImageInfo{ref: {ID: "sha256:local", RepoDigests: []string{host + "/app@" + old}}}
	part := b.do("GET", "/stacks/app/updates/check", nil, nil).Body.String()
	if !strings.Contains(part, "update available") {
		t.Fatalf("no badge for an old tag:\n%s", part)
	}
	// Once the local image matches, it reads current (from a fresh check).
	h.s.reg.Now = func() time.Time { return time.Now().Add(7 * time.Hour) }
	d, _ := h.s.reg.Digest(context.Background(), ref)
	fe.images[ref] = engine.ImageInfo{ID: "sha256:local", RepoDigests: []string{host + "/app@" + d}}
	if part := b.do("GET", "/stacks/app/updates/check", nil, nil).Body.String(); !strings.Contains(part, "current") || strings.Contains(part, "update available") {
		t.Errorf("current image flagged:\n%s", part)
	}
	// Unreviewed files: no outbound check at all.
	os.WriteFile(filepath.Join(h.root, "app", "compose.yaml"), []byte("services:\n  web:\n    image: 169.254.169.254/x:y\n"), 0o644)
	if part := b.do("GET", "/stacks/app/updates/check", nil, nil).Body.String(); !strings.Contains(part, "review this stack") {
		t.Errorf("checked unreviewed files:\n%s", part)
	}
	h.approve(filepath.Join(h.root, "app"))
	os.WriteFile(filepath.Join(h.root, "app", "compose.yaml"), []byte("services:\n  web:\n    image: "+ref+"\n"), 0o644)
	h.approve(filepath.Join(h.root, "app"))
	// The tag already points at a newer pull than the container runs.
	fe.images[ref] = engine.ImageInfo{ID: "sha256:pulled", RepoDigests: []string{host + "/app@" + d}}
	if part := b.do("GET", "/stacks/app/updates/check", nil, nil).Body.String(); !strings.Contains(part, "already pulled but not running") {
		t.Errorf("pulled-not-running not flagged:\n%s", part)
	}
}

func TestUpdateLeavesStoppedStacksAlone(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	updateStack(t, h, "parked", "parked:latest")
	h.engines[store.DefaultDockerHost].project = []engine.ProjectContainer{{Name: "parked-web-1", Service: "web", ImageID: "sha256:x", Running: false}}
	h.s.store.SetSchedule(context.Background(), "parked", schedule.Daily, true)
	h.s.runSchedules(time.Date(2026, 11, 12, 4, 0, 0, 0, time.Local))
	if log, _ := os.ReadFile(h.dockerLog); strings.Contains(string(log), "-p parked") {
		t.Errorf("a stopped stack was updated:\n%s", log)
	}
	if sc, _, _ := h.s.store.GetSchedule(context.Background(), "parked"); sc.LastResult != store.UpdateSkipped || !strings.Contains(sc.LastMsg, "doesn't start a stopped stack") {
		t.Errorf("result %+v", sc)
	}
}

// Fleetling's own stack runs its schedule through the update helper.
func TestScheduledSelfUpdateUsesHelper(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := updateStack(t, h, "fleetling", "fleetling:local")
	h.inCtr = true
	fe := h.engines[store.DefaultDockerHost]
	fe.mounts = []compose.Mount{{Type: "bind", Source: h.root, Destination: h.root}}
	fe.labels = map[string]string{"com.docker.compose.project": "fleetling", "com.docker.compose.project.working_dir": dir}
	fe.imageID = "sha256:self"
	fe.project = []engine.ProjectContainer{{Name: "fleetling-fleetling-1", Service: "fleetling", ImageID: "sha256:self", Running: true}}
	h.s.store.SetSchedule(context.Background(), "fleetling", schedule.Daily, true)
	h.s.runSchedules(time.Date(2026, 11, 12, 4, 0, 0, 0, time.Local))
	log, _ := os.ReadFile(h.dockerLog)
	if !strings.Contains(string(log), "run --rm -d --name fleetling-updater") || strings.Contains(string(log), "pull --ignore-buildable") {
		t.Errorf("self update not through the helper:\n%s", log)
	}
}

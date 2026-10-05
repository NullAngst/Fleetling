package server

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NullAngst/Fleetling/internal/compose"
)

var fingerprintRe = regexp.MustCompile(`name="fingerprint" value="([0-9a-f]+)"`)

// A container that rewrites its own compose file must not get that change
// deployed without a review.
func TestDriftBlocksActionsUntilReviewed(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "copyparty")
	t.Setenv("FAKE_BIND", filepath.Join(dir, "hists"))
	w := b.do("POST", "/stacks/new", url.Values{"csrf": {b.token("/stacks/new")}, "project": {"copyparty"}, "engine": {"docker"},
		"compose": {"services:\n  copyparty:\n    image: copyparty/ac\n    volumes:\n      - ./hists:/hists\n"}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	// First deploy writes created_paths into .fleetling.toml. That is
	// Fleetling's own write and must not count as drift.
	w = b.do("POST", "/stacks/copyparty/action/deploy", url.Values{"csrf": {b.token("/stacks/copyparty")}}, nil)
	waitJob(t, h, w.Header().Get("Location"))
	if m, _ := compose.ReadMeta(dir); len(m.CreatedPaths) != 1 {
		t.Fatalf("path tracking did not run: %+v", m)
	}
	if page := b.do("GET", "/stacks/copyparty", nil, nil).Body.String(); strings.Contains(page, "changed outside Fleetling") {
		t.Fatal("Fleetling's own metadata write flagged as drift")
	}

	// The container rewrites the compose file.
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  copyparty:\n    image: copyparty/ac\n    privileged: true\n    volumes:\n      - /:/host\n"), 0o644)
	os.Remove(h.dockerLog)

	for _, action := range []string{"update", "down", "restart"} {
		w = b.do("POST", "/stacks/copyparty/action/"+action, url.Values{"csrf": {b.token("/stacks/copyparty")}}, nil)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/stacks/copyparty/review?action="+action {
			t.Fatalf("%s on a drifted stack: %d %s", action, w.Code, w.Header().Get("Location"))
		}
	}
	if log, err := os.ReadFile(h.dockerLog); err == nil && len(log) > 0 {
		t.Fatalf("docker ran on a drifted stack:\n%s", log)
	}
	if list := b.do("GET", "/", nil, nil).Body.String(); !strings.Contains(list, "changed outside Fleetling, review") {
		t.Error("stacks list does not flag the stack")
	}
	if w := b.do("GET", "/stacks/copyparty/edit", nil, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/stacks/copyparty/review" {
		t.Errorf("edit on a drifted stack: %d", w.Code)
	}

	review := b.do("GET", "/stacks/copyparty/review", nil, nil).Body.String()
	if !strings.Contains(review, "privileged: true") || !strings.Contains(review, "compose.yaml <span class=\"muted\">changed") {
		t.Fatalf("review page:\n%s", review)
	}
	fp := fingerprintRe.FindStringSubmatch(review)[1]
	tok := csrfRe.FindStringSubmatch(review)[1]

	// Approving a stale view is refused: the files moved on since.
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SNEAKY=1\n"), 0o600)
	if w := b.do("POST", "/stacks/copyparty/review", url.Values{"csrf": {tok}, "fingerprint": {fp}}, nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "SNEAKY=1") {
		t.Fatalf("stale approval: %d", w.Code)
	}
	review = b.do("GET", "/stacks/copyparty/review", nil, nil).Body.String()
	w = b.do("POST", "/stacks/copyparty/review", url.Values{"csrf": {tok}, "fingerprint": {fingerprintRe.FindStringSubmatch(review)[1]}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	acts, _ := h.s.store.ListActions(context.Background(), "copyparty", 1, 0)
	if !strings.Contains(acts[0].Command, "approved changes made outside Fleetling to .env, compose.yaml") {
		t.Errorf("log %q", acts[0].Command)
	}
	w = b.do("POST", "/stacks/copyparty/action/restart", url.Values{"csrf": {b.token("/stacks/copyparty")}}, nil)
	if !strings.Contains(w.Header().Get("Location"), "job=") {
		t.Fatalf("action after approval: %s", w.Header().Get("Location"))
	}
	waitJob(t, h, w.Header().Get("Location"))
}

// A changed project name in .fleetling.toml would aim Down at another
// stack; it counts as drift like any other file.
func TestMetadataDrift(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "app")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {a: {image: x}}\n"), 0o644)
	compose.WriteMeta(dir, &compose.Meta{Project: "app", Engine: "docker"}, compose.Owner{})

	// Never recorded: one review before anything runs.
	w := b.do("POST", "/stacks/app/action/down", url.Values{"csrf": {b.token("/stacks/app")}}, nil)
	if !strings.HasPrefix(w.Header().Get("Location"), "/stacks/app/review") {
		t.Fatalf("unrecorded stack ran: %s", w.Header().Get("Location"))
	}
	if page := b.do("GET", "/stacks/app/review", nil, nil).Body.String(); !strings.Contains(page, "no approved record") {
		t.Error("unrecorded review page")
	}
	h.approve(dir)

	compose.WriteMeta(dir, &compose.Meta{Project: "gitea", Engine: "docker"}, compose.Owner{})
	w = b.do("POST", "/stacks/app/action/down", url.Values{"csrf": {b.token("/stacks/app")}}, nil)
	if !strings.HasPrefix(w.Header().Get("Location"), "/stacks/app/review") {
		t.Fatalf("down with a swapped project ran: %s", w.Header().Get("Location"))
	}
	if log, err := os.ReadFile(h.dockerLog); err == nil && strings.Contains(string(log), "-p gitea") {
		t.Fatal("docker ran against the swapped project")
	}
}

// An env_file the compose file names is covered too.
func TestEnvFileDrift(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "svc")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  a:\n    image: x\n    env_file: conf/app.env\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "conf"), 0o755)
	os.WriteFile(filepath.Join(dir, "conf", "app.env"), []byte("MODE=safe\n"), 0o644)
	compose.WriteMeta(dir, &compose.Meta{Project: "svc", Engine: "docker"}, compose.Owner{})
	h.approve(dir)
	os.WriteFile(filepath.Join(dir, "conf", "app.env"), []byte("MODE=safe\nLD_PRELOAD=/conf/evil.so\n"), 0o644)
	w := b.do("POST", "/stacks/svc/action/recreate", url.Values{"csrf": {b.token("/stacks/svc")}}, nil)
	if !strings.HasPrefix(w.Header().Get("Location"), "/stacks/svc/review") {
		t.Fatalf("recreate ran with a changed env_file: %s", w.Header().Get("Location"))
	}
	if page := b.do("GET", "/stacks/svc/review", nil, nil).Body.String(); !strings.Contains(page, "LD_PRELOAD") || !strings.Contains(page, "conf/app.env") {
		t.Error("review page misses the env_file change")
	}
}

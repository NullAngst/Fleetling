package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NullAngst/Fleetling/internal/compose"
)

// A container that rewrites its compose file while its first deploy runs
// must not have that change approved along with Fleetling's own write of
// .fleetling.toml.
func TestFirstDeployKeepsContainerWritesFlagged(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "app")
	t.Setenv("FAKE_BIND", filepath.Join(dir, "data"))
	w := b.do("POST", "/stacks/new", url.Values{"csrf": {b.token("/stacks/new")}, "project": {"app"}, "engine": {"docker"},
		"compose": {"services:\n  app:\n    image: x\n    volumes:\n      - ./data:/data\n"}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	t.Setenv("FAKE_UP_WRITES", filepath.Join(dir, "compose.yaml"))
	w = b.do("POST", "/stacks/app/action/deploy", url.Values{"csrf": {b.token("/stacks/app")}}, nil)
	if exit := waitJob(t, h, w.Header().Get("Location")); exit != 0 {
		t.Fatalf("deploy exit %d", exit)
	}

	f, _ := loadFolder(mustSettings(t, h), "app")
	rs, err := h.s.reviewFor(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if rs.State != reviewModified {
		t.Fatalf("container's write was approved: state %q", rs.State)
	}
	for _, ch := range rs.Changes {
		if ch.Path != "compose.yaml" {
			t.Errorf("%s flagged; only compose.yaml changed outside Fleetling", ch.Path)
		}
	}
	if m := approvedMetaOrFail(t, h, f); m.Deployed == nil || len(m.CreatedPaths) != 1 {
		t.Errorf("Fleetling's own metadata write was not recorded: %+v", m)
	}
}

// Update pulls, then runs up. Files that change during the pull stop the
// job before up reads them.
func TestUpdateStopsWhenFilesChangeMidJob(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "app")
	w := b.do("POST", "/stacks/new", url.Values{"csrf": {b.token("/stacks/new")}, "project": {"app"}, "engine": {"docker"},
		"compose": {"services:\n  app:\n    image: x\n"}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	t.Setenv("FAKE_PULL_WRITES", filepath.Join(dir, "compose.yaml"))
	os.Remove(h.dockerLog)
	w = b.do("POST", "/stacks/app/action/update", url.Values{"csrf": {b.token("/stacks/app")}}, nil)
	if exit := waitJob(t, h, w.Header().Get("Location")); exit == 0 {
		t.Fatal("update finished after its files changed mid-job")
	}
	log, _ := os.ReadFile(h.dockerLog)
	if !strings.Contains(string(log), " pull") {
		t.Fatalf("pull never ran:\n%s", log)
	}
	if strings.Contains(string(log), " up ") {
		t.Fatalf("up ran on files nobody reviewed:\n%s", log)
	}
}

func approvedMetaOrFail(t *testing.T, h *harness, f compose.Folder) *compose.Meta {
	t.Helper()
	m, err := h.s.approvedMeta(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A symlink under the root isn't a stack, whether it is listed or opened
// by its URL.
func TestSymlinkedFolderIsNotAStack(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	elsewhere := t.TempDir()
	os.WriteFile(filepath.Join(elsewhere, "compose.yaml"), []byte("services: {a: {image: x}}\n"), 0o644)
	if err := os.Symlink(elsewhere, filepath.Join(h.root, "sneaky")); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadFolder(mustSettings(t, h), "sneaky"); ok {
		t.Fatal("loadFolder followed a symlink out of the root")
	}
	if w := b.do("GET", "/stacks/sneaky", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("GET /stacks/sneaky = %d, want 404", w.Code)
	}
}

// An update that can't reach its engine is skipped, not run blind.
func TestUpdateSkipsWhenEngineUnreachable(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	updateStack(t, h, "app", "app:latest")
	h.s.newEngine = func(string) (Engine, error) { return nil, errors.New("socket gone") }
	os.Remove(h.dockerLog)
	st := mustSettings(t, h)
	f, _ := loadFolder(st, "app")
	_, err := h.s.autoUpdate(context.Background(), st, f, "schedule")
	if !errors.Is(err, errSkipped) || !strings.Contains(err.Error(), "could not reach") {
		t.Fatalf("err = %v, want a skip", err)
	}
	if log, _ := os.ReadFile(h.dockerLog); len(log) > 0 {
		t.Fatalf("docker ran:\n%s", log)
	}
}

// An import that can't go ahead writes nothing, so trying again after
// fixing the folder works.
func TestImportRefusalLeavesFolderUntouched(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.root, "app")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("KEEP=1\n"), 0o600)
	c := &importCandidate{Name: "app", StackEnv: true}
	if _, err := h.s.writeImport(dir, c, "services: {a: {image: x}}\n", nil); err == nil {
		t.Fatal("import over an existing .env went ahead")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("refused import left files behind: %v", names)
	}
	if _, isStack := compose.ReadFolder(dir); isStack {
		t.Fatal("the folder now counts as a stack")
	}
}

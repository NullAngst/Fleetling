package server

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NullAngst/Fleetling/internal/engine/enginetest"
)

// importBoth runs the API import of both test stacks into gitea and
// npmplus_data.
func importBoth(t *testing.T, b *browser, ptURL string) {
	t.Helper()
	w := b.do("POST", "/import/connect", url.Values{"csrf": {b.token("/import")}, "url": {ptURL}, "api_key": {"ptr_good"}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("connect: %d", w.Code)
	}
	tok := csrfRe.FindStringSubmatch(w.Body.String())[1]
	w = b.do("POST", "/import/apply", url.Values{"csrf": {tok},
		"include_p3": {"1"}, "folder_p3": {"gitea"}, "fix_p3": {"1"},
		"include_p8": {"1"}, "folder_p8": {"npmplus_data"}}, nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "msg-error") {
		t.Fatalf("apply: %d %s", w.Code, w.Body.String())
	}
}

const portainerID = "3333333333333333333333333333333333333333333333333333333333333333"

func portainerRestart(fe *enginetest.Engine) (policy string, running bool) {
	fe.Update(portainerID, func(c *enginetest.Container) { policy, running = c.Restart, c.Running })
	return
}

// The whole round trip: import, stop Portainer, change one stack in
// Fleetling, go back. Portainer comes back as it was, Fleetling lets go of
// both stacks, the changed file survives, and no stack container is
// touched.
func TestPortainerTrialGoBack(t *testing.T) {
	h, b, fe, pt := importHarness(t)
	fe.Update(portainerID, func(c *enginetest.Container) { c.Restart = "always" })
	importBoth(t, b, pt.URL)

	tr, err := h.s.loadTrial(context.Background())
	if err != nil || tr == nil || len(tr.Stacks) != 2 {
		t.Fatalf("trial after import: %+v %v", tr, err)
	}

	w := b.do("POST", "/import/retire", url.Values{"csrf": {b.token("/import")}}, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/import/trial?notice=stopped" {
		t.Fatalf("retire: %d %s", w.Code, w.Header().Get("Location"))
	}
	if p, running := portainerRestart(fe); p != "no" || running {
		t.Fatalf("after stop: restart=%q running=%v", p, running)
	}
	if body := b.do("GET", "/", nil, nil).Body.String(); !strings.Contains(body, "Portainer migration on trial") {
		t.Error("no trial banner on the stacks page")
	}

	// A change made in Fleetling after the import.
	gitea := filepath.Join(h.root, "gitea")
	edited := "services:\n  gitea:\n    image: gitea/gitea:2\n"
	os.WriteFile(filepath.Join(gitea, "compose.yaml"), []byte(edited), 0o644)
	// App data the stack wrote into its folder.
	os.MkdirAll(filepath.Join(gitea, "data"), 0o755)
	os.WriteFile(filepath.Join(gitea, "data", "app.db"), []byte("rows"), 0o644)

	page := b.do("GET", "/import/trial", nil, nil).Body.String()
	for _, want := range []string{"Portainer trial", "changed in Fleetling after the import", "gitea/gitea:2", "Go back to Portainer"} {
		if !strings.Contains(page, want) {
			t.Errorf("trial page missing %q", want)
		}
	}

	os.Remove(h.dockerLog)
	before := len(fe.Calls())
	w = b.do("POST", "/import/trial/revert", url.Values{"csrf": {b.token("/import/trial")}}, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Back to Portainer") {
		t.Fatalf("revert: %d %s", w.Code, w.Body.String())
	}
	if calls := strings.Join(fe.Calls()[before:], ","); calls != "POST update portainer restart=always,POST start portainer" {
		t.Errorf("revert engine calls %s; want only Portainer's policy and start", calls)
	}
	if log, _ := os.ReadFile(h.dockerLog); len(log) > 0 {
		t.Errorf("revert ran docker compose:\n%s", log)
	}
	if p, running := portainerRestart(fe); p != "always" || !running {
		t.Errorf("after going back: restart=%q running=%v", p, running)
	}

	// gitea: Fleetling's files gone, the edited compose file and the app
	// data kept.
	for _, gone := range []string{".fleetling.toml", ".env"} {
		if _, err := os.Lstat(filepath.Join(gitea, gone)); err == nil {
			t.Errorf("gitea/%s still there", gone)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(gitea, "compose.yaml")); string(got) != edited {
		t.Errorf("edited compose file lost: %q", got)
	}
	if _, err := os.Stat(filepath.Join(gitea, "data", "app.db")); err != nil {
		t.Error("app data removed")
	}
	// npmplus_data was made by the import and held only its files, so the
	// folder goes too.
	if _, err := os.Lstat(filepath.Join(h.root, "npmplus_data")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(h.root, "npmplus_data"))
		t.Errorf("npmplus_data left behind: %v", entries)
	}

	if tr, _ := h.s.loadTrial(context.Background()); tr != nil {
		t.Errorf("trial still recorded: %+v", tr)
	}
	for _, f := range []string{"gitea", "npmplus_data"} {
		if _, ok, _ := h.s.store.GetBaseline(context.Background(), f); ok {
			t.Errorf("%s still has an approved snapshot", f)
		}
	}
	if body := b.do("GET", "/", nil, nil).Body.String(); strings.Contains(body, "on trial") {
		t.Error("trial banner still shown")
	}
}

// Finish removes Portainer's container and leaves the stacks managed.
func TestPortainerTrialFinish(t *testing.T) {
	h, b, fe, pt := importHarness(t)
	importBoth(t, b, pt.URL)
	b.do("POST", "/import/retire", url.Values{"csrf": {b.token("/import")}}, nil)
	w := b.do("POST", "/import/trial/finish", url.Values{"csrf": {b.token("/import/trial")}}, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?notice=migrated" {
		t.Fatalf("finish: %d %s", w.Code, w.Body.String())
	}
	if calls := strings.Join(fe.Calls(), ","); !strings.HasSuffix(calls, "DELETE remove portainer") {
		t.Errorf("finish calls %s", calls)
	}
	if tr, _ := h.s.loadTrial(context.Background()); tr != nil {
		t.Error("trial still recorded")
	}
	if _, err := os.Stat(filepath.Join(h.root, "gitea", ".fleetling.toml")); err != nil {
		t.Error("finishing let go of a stack")
	}
}

// With Portainer's container gone there is nothing to hand the stacks
// back to, so nothing is handed back.
func TestPortainerTrialRevertNeedsPortainer(t *testing.T) {
	h, b, fe, pt := importHarness(t)
	importBoth(t, b, pt.URL)
	b.do("POST", "/import/retire", url.Values{"csrf": {b.token("/import")}}, nil)
	e, _, err := h.s.engineFor(context.Background(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	e.Remove(context.Background(), portainerID)
	e.Close()
	_ = fe
	w := b.do("POST", "/import/trial/revert", url.Values{"csrf": {b.token("/import/trial")}}, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "container is gone") {
		t.Fatalf("revert without Portainer: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(h.root, "gitea", ".fleetling.toml")); err != nil {
		t.Error("stacks were handed back to a Portainer that isn't there")
	}
	if tr, _ := h.s.loadTrial(context.Background()); tr == nil {
		t.Error("trial dropped")
	}
}

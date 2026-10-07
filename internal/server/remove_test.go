package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/engine/enginetest"
)

var appID = strings.Repeat("a1", 32)

// removeYard sets up two stacks and a stray container against the fake
// Engine API:
//
//	app     managed; Docker created app-data and app/cache on its first
//	        deploy; it also binds app/conf, shared, media and /dev/dri
//	other   an On disk stack that binds shared
//	plex    a container outside any stack that mounts media
type removeYard struct {
	h   *harness
	b   *browser
	fe  *enginetest.Engine
	dir string // the app stack folder
}

func (y *removeYard) path(rel string) string { return filepath.Join(y.h.root, rel) }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newRemoveYard(t *testing.T) *removeYard {
	t.Helper()
	h, b, fe := containerHarness(t)
	y := &removeYard{h: h, b: b, fe: fe, dir: filepath.Join(h.root, "app")}

	writeFile(t, y.path("app/compose.yaml"), "services:\n  web:\n    image: nginx:1\n")
	writeFile(t, y.path("app-data/db"), "hello")
	writeFile(t, y.path("app/cache/x"), "12")
	writeFile(t, y.path("app/conf/app.ini"), "a=1")
	writeFile(t, y.path("shared/z"), "z")
	writeFile(t, y.path("media/movie"), "m")
	if err := compose.WriteMeta(y.dir, &compose.Meta{Project: "app", Engine: "docker", Deployed: ptr(time.Now()),
		CreatedPaths: []string{y.path("app-data"), y.path("app/cache")}}, compose.Owner{}); err != nil {
		t.Fatal(err)
	}
	// What `docker compose config --format json` says for app.
	writeFile(t, y.path("app/.fake-config.json"), fmt.Sprintf(`{"services":{"web":{"image":"nginx:1","volumes":[
	  {"type":"bind","source":%q},{"type":"bind","source":%q},{"type":"bind","source":%q},
	  {"type":"bind","source":%q},{"type":"bind","source":%q},{"type":"bind","source":"/dev/dri"},
	  {"type":"volume","source":"data"}]}},
	 "volumes":{"data":{"name":"app_data"},"ext":{"name":"ext","external":true}}}`,
		y.path("app-data"), y.path("app/cache"), y.path("app/conf"), y.path("shared"), y.path("media")))
	h.approve(y.dir)

	writeFile(t, y.path("other/compose.yaml"), "services:\n  o:\n    image: x\n    volumes:\n      - ../shared:/s\n")
	writeFile(t, y.path("other/.fake-config.json"), fmt.Sprintf(`{"services":{"o":{"volumes":[{"type":"bind","source":%q}]}}}`, y.path("shared")))

	fe.Add(&enginetest.Container{ID: appID, Name: "app-web-1", Image: "nginx:1", Running: true,
		Labels:  map[string]string{engine.LabelProject: "app", engine.LabelService: "web", engine.LabelWorkingDir: y.dir},
		Mounts:  []enginetest.Mount{{Source: y.path("app-data"), Destination: "/data"}},
		Volumes: []string{"app_data"}})
	fe.Add(&enginetest.Container{ID: strings.Repeat("b2", 32), Name: "plex", Image: "plex", Running: true,
		Mounts: []enginetest.Mount{{Source: y.path("media"), Destination: "/media"}}})
	fe.Volumes = []*enginetest.Volume{{Name: "app_data", Labels: map[string]string{engine.LabelProject: "app"}}}
	return y
}

func (y *removeYard) exists(rel string) bool {
	_, err := os.Lstat(y.path(rel))
	return err == nil
}

var jobRe = regexp.MustCompile(`^/jobs/([0-9a-f]+)$`)

// waitJobPage waits for the job a remove redirected to.
func waitJobPage(t *testing.T, h *harness, location string) int {
	t.Helper()
	m := jobRe.FindStringSubmatch(location)
	if m == nil {
		t.Fatalf("redirect %q is not a job page", location)
	}
	return waitJob(t, h, "job="+m[1])
}

func TestRemoveStackPage(t *testing.T) {
	y := newRemoveYard(t)
	page := y.b.do("GET", "/stacks/app/remove", nil, nil).Body.String()

	offered := regexp.MustCompile(`name="path" value="([^"]+)">`).FindAllStringSubmatch(page, -1)
	var got []string
	for _, m := range offered {
		got = append(got, m[1])
	}
	want := []string{y.path("app-data"), y.path("app/cache"), y.path("app/conf"), y.dir}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("offered\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, s := range []string{
		"5 B, 2 items",                       // app-data: the folder and one file
		"Kept: used by stack other",          // shared
		"Kept: used by container plex",       // media
		`<span class="mono">/dev/dri</span>`, // never offered
		"app_data", "nginx:1", "docker compose -p app",
		`data-flag="-v"`, `data-flag="--rmi all"`,
	} {
		if !strings.Contains(page, s) {
			t.Errorf("remove page missing %q", s)
		}
	}
	if strings.Contains(page, `value="/dev/dri"`) || strings.Contains(page, ">ext<") {
		t.Error("offered something it never should: /dev/dri or the external volume")
	}
	if !strings.Contains(y.b.do("GET", "/stacks/app", nil, nil).Body.String(), `href="/stacks/app/remove"`) {
		t.Error("stack page has no Remove link")
	}
}

func TestRemoveStackRefusesBadForms(t *testing.T) {
	y := newRemoveYard(t)
	for _, c := range []struct {
		name string
		form url.Values
		want string
	}{
		{"no confirm", url.Values{"path": {y.path("app-data")}}, "type app to confirm"},
		{"wrong confirm", url.Values{"path": {y.path("app-data")}, "confirm": {"App"}}, "type app to confirm"},
		{"volumes need confirm", url.Values{"volumes": {"1"}}, "type app to confirm"},
		{"refused path", url.Values{"path": {y.path("shared")}, "confirm": {"app"}}, "not on the list"},
		{"forged path", url.Values{"path": {"/etc"}, "confirm": {"app"}}, "not on the list"},
		{"forged root", url.Values{"path": {y.h.root}, "confirm": {"app"}}, "not on the list"},
	} {
		w := y.b.do("POST", "/stacks/app/remove", withCSRF(y, c.form), nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: status %d, want 400 saying %q", c.name, w.Code, c.want)
		}
	}
	if log, _ := os.ReadFile(y.h.dockerLog); strings.Contains(string(log), " down") {
		t.Errorf("down ran for a refused form:\n%s", log)
	}
	for _, p := range []string{"app-data", "shared", "app"} {
		if !y.exists(p) {
			t.Errorf("%s deleted by a refused form", p)
		}
	}
}

func withCSRF(y *removeYard, form url.Values) url.Values {
	f := url.Values{}
	for k, v := range form {
		f[k] = v
	}
	f.Set("csrf", y.b.token("/stacks/app/remove"))
	return f
}

// The phase 6 "done when", end to end: down with -v and --rmi all, then
// the ticked folders go, every one logged, and nothing else is touched.
func TestRemoveStackWithFolders(t *testing.T) {
	y := newRemoveYard(t)
	w := y.b.do("POST", "/stacks/app/remove", withCSRF(y, url.Values{
		"volumes": {"1"}, "images": {"1"}, "confirm": {"app"},
		"path": {y.dir, y.path("app-data"), y.path("app/cache")},
	}), nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if exit := waitJobPage(t, y.h, loc); exit != 0 {
		j := y.h.s.jobs.Get(jobRe.FindStringSubmatch(loc)[1])
		t.Fatalf("exit %d:\n%s", exit, strings.Join(j.Tail(50), "\n"))
	}
	log, _ := os.ReadFile(y.h.dockerLog)
	if !strings.Contains(string(log), "--progress plain down -v --rmi all") {
		t.Errorf("down not run with -v --rmi all:\n%s", log)
	}
	for _, gone := range []string{"app", "app-data"} {
		if y.exists(gone) {
			t.Errorf("%s still there", gone)
		}
	}
	for _, kept := range []string{"shared/z", "media/movie", "other/compose.yaml"} {
		if !y.exists(kept) {
			t.Errorf("%s was deleted", kept)
		}
	}

	acts, _ := y.h.s.store.ListActions(context.Background(), "app", 20, 0)
	var all []string
	for _, a := range acts {
		all = append(all, a.Command)
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{
		"fleetling: delete " + y.path("app-data") + " (5 B, 2 items)",
		"fleetling: delete " + y.path("app/cache") + " (2 B, 2 items)",
		"fleetling: delete " + y.dir + " (",
		"down -v --rmi all\nfleetling: delete " + y.path("app/cache") + " " + y.path("app-data") + " " + y.dir,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("action log missing %q:\n%s", want, joined)
		}
	}
	if _, ok, _ := y.h.s.store.GetBaseline(context.Background(), "app"); ok {
		t.Error("baseline kept for a deleted stack folder")
	}
	if page := y.b.do("GET", loc, nil, nil); page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Remove app") {
		t.Errorf("job page: %d", page.Code)
	}
}

func TestRemoveStackDownFailsDeletesNothing(t *testing.T) {
	y := newRemoveYard(t)
	t.Setenv("FAKE_DOWN_EXIT", "3")
	w := y.b.do("POST", "/stacks/app/remove", withCSRF(y, url.Values{"confirm": {"app"}, "path": {y.path("app-data")}}), nil)
	if exit := waitJobPage(t, y.h, w.Header().Get("Location")); exit != 3 {
		t.Fatalf("exit %d", exit)
	}
	if !y.exists("app-data/db") {
		t.Fatal("deleted after down failed")
	}
}

// Rule 3 at delete time: a stack that started using a path after the page
// loaded keeps it, because the index is rebuilt after down.
func TestRemoveStackRechecksAfterDown(t *testing.T) {
	y := newRemoveYard(t)
	t.Setenv("FAKE_LATE_DIR", y.path("late"))
	t.Setenv("FAKE_LATE_BIND", y.path("app-data"))
	w := y.b.do("POST", "/stacks/app/remove", withCSRF(y, url.Values{"confirm": {"app"}, "path": {y.path("app-data"), y.path("app/cache")}}), nil)
	loc := w.Header().Get("Location")
	if exit := waitJobPage(t, y.h, loc); exit != 1 {
		t.Fatalf("exit %d, want 1", exit)
	}
	if !y.exists("app-data/db") || y.exists("app/cache") {
		t.Fatal("wrong paths deleted")
	}
	out := strings.Join(y.h.s.jobs.Get(jobRe.FindStringSubmatch(loc)[1]).Tail(20), "\n")
	if !strings.Contains(out, "kept "+y.path("app-data")+": used by stack late") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRemoveStackNeedsReview(t *testing.T) {
	y := newRemoveYard(t)
	writeFile(t, y.path("app/compose.yaml"), "services:\n  web:\n    image: nginx:1\n    privileged: true\n")
	if page := y.b.do("GET", "/stacks/app/remove", nil, nil).Body.String(); !strings.Contains(page, "review them first") || strings.Contains(page, `name="path"`) {
		t.Error("remove page offered paths for a modified stack")
	}
	w := y.b.do("POST", "/stacks/app/remove", url.Values{"csrf": {y.b.token("/stacks/app")}, "confirm": {"app"}, "path": {y.path("app-data")}}, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/stacks/app/review?action=remove" {
		t.Fatalf("remove on a modified stack: %d %s", w.Code, w.Header().Get("Location"))
	}
	if !y.exists("app-data/db") {
		t.Fatal("deleted before review")
	}
}

// created_paths is read from the approved files. A container that rewrites
// .fleetling.toml gets the stack flagged, and even after approval each
// entry still goes through every rule.
func TestRemoveCreatedPathsAreUntrusted(t *testing.T) {
	y := newRemoveYard(t)
	m, _ := compose.ReadMeta(y.dir)
	m.CreatedPaths = append(m.CreatedPaths, "/etc", y.h.root, y.path("other"), filepath.Join(y.h.root, "..", "outside"))
	compose.WriteMeta(y.dir, m, compose.Owner{})
	if !strings.Contains(y.b.do("GET", "/stacks/app/remove", nil, nil).Body.String(), "review them first") {
		t.Fatal("a rewritten .fleetling.toml was not flagged")
	}
	y.h.approve(y.dir)
	page := y.b.do("GET", "/stacks/app/remove", nil, nil).Body.String()
	for _, p := range []string{"/etc", y.h.root, y.path("other"), filepath.Join(y.h.root, "..", "outside")} {
		if strings.Contains(page, `value="`+p+`">`) {
			t.Errorf("offered %s from created_paths", p)
		}
	}
	if !strings.Contains(page, "Kept: used by the folder of stack other") {
		t.Error("another stack's folder in created_paths was not refused")
	}
}

func TestRemoveRefusesSelfStack(t *testing.T) {
	y := newRemoveYard(t)
	y.h.inCtr = true
	y.h.s.selfIDs = func() []string { return []string{appID[:12]} }
	if page := y.b.do("GET", "/stacks/app/remove", nil, nil).Body.String(); !strings.Contains(page, "Fleetling&#39;s own stack") {
		t.Error("self stack remove page has no refusal")
	}
	w := y.b.do("POST", "/stacks/app/remove", url.Values{"csrf": {y.b.token("/stacks/app")}, "confirm": {"app"}}, nil)
	if w.Code != http.StatusConflict {
		t.Errorf("remove self: %d", w.Code)
	}
}

func TestRemoveContainerWithBindFolders(t *testing.T) {
	y := newRemoveYard(t)
	soloID := strings.Repeat("c3", 32)
	writeFile(t, y.path("solo-data/x"), "1234")
	y.fe.Add(&enginetest.Container{ID: soloID, Name: "solo", Image: "busybox", Running: true,
		Mounts: []enginetest.Mount{{Source: y.path("solo-data"), Destination: "/d"}, {Source: y.path("shared"), Destination: "/s"}, {Source: "/dev/dri", Destination: "/dev/dri"}}})
	base := "/containers/docker/" + soloID
	if !strings.Contains(y.b.do("GET", base, nil, nil).Body.String(), base+"/remove") {
		t.Fatal("container page has no Remove link")
	}
	page := y.b.do("GET", base+"/remove", nil, nil).Body.String()
	for _, s := range []string{`value="` + y.path("solo-data") + `">`, "Kept: used by stack app (bind mount of web)", `<span class="mono">/dev/dri</span>`, "docker rm -f solo"} {
		if !strings.Contains(page, s) {
			t.Errorf("container remove page missing %q", s)
		}
	}
	tok := y.b.token(base + "/remove")
	if w := y.b.do("POST", base+"/remove", url.Values{"csrf": {tok}, "path": {y.path("solo-data")}}, nil); w.Code != http.StatusBadRequest {
		t.Errorf("no confirm: %d", w.Code)
	}
	w := y.b.do("POST", base+"/remove", url.Values{"csrf": {tok}, "path": {y.path("solo-data")}, "confirm": {"solo"}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	if exit := waitJobPage(t, y.h, w.Header().Get("Location")); exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	if !strings.Contains(strings.Join(y.fe.Calls(), ","), "remove solo") {
		t.Errorf("container not removed: %v", y.fe.Calls())
	}
	if y.exists("solo-data") || !y.exists("shared/z") {
		t.Error("wrong folders deleted")
	}
	acts, _ := y.h.s.store.ListActions(context.Background(), "container/solo", 5, 0)
	if len(acts) < 2 || !strings.Contains(acts[0].Command+acts[1].Command, "fleetling: delete "+y.path("solo-data")+" (4 B, 2 items)") {
		t.Errorf("action log %+v", acts)
	}
}

// A container in a stack keeps the paths its stack's other services use,
// and its own service's paths are offered.
func TestRemoveContainerKeepsSiblingServicePaths(t *testing.T) {
	y := newRemoveYard(t)
	writeFile(t, y.path("app/.fake-config.json"), fmt.Sprintf(`{"services":{
	  "web":{"volumes":[{"type":"bind","source":%q},{"type":"bind","source":%q}]},
	  "db":{"volumes":[{"type":"bind","source":%q}]}}}`, y.path("app-data"), y.path("app/cache"), y.path("app/cache")))
	y.h.approve(y.dir)
	y.fe.Add(&enginetest.Container{ID: appID, Name: "app-web-1", Image: "nginx:1", Running: true,
		Labels: map[string]string{engine.LabelProject: "app", engine.LabelService: "web", engine.LabelWorkingDir: y.dir},
		Mounts: []enginetest.Mount{{Source: y.path("app-data"), Destination: "/data"}, {Source: y.path("app/cache"), Destination: "/cache"}}})
	page := y.b.do("GET", "/containers/docker/"+appID+"/remove", nil, nil).Body.String()
	if !strings.Contains(page, `value="`+y.path("app-data")+`">`) {
		t.Error("its own bind source not offered")
	}
	if !strings.Contains(page, "made by Docker on the stack&#39;s first deploy") {
		t.Error("created path not marked")
	}
	if !strings.Contains(page, "Kept: used by stack app (bind mount of db)") {
		t.Error("a path its sibling service uses was not refused")
	}
}

// An engine that is configured and present but doesn't answer could have
// containers using any of these paths, so no folder can be ticked.
func TestRemoveBlockedWhileAnEngineIsDown(t *testing.T) {
	y := newRemoveYard(t)
	broken := filepath.Join(t.TempDir(), "podman.sock")
	writeFile(t, broken, "not a socket")
	st, _ := y.h.s.store.LoadSettings(context.Background(), y.h.root)
	st.PodmanHost = broken
	y.h.s.store.SaveSettings(context.Background(), st)
	next := y.h.s.newEngine
	y.h.s.newEngine = func(ep string) (Engine, error) {
		if ep == broken {
			return engine.New(ep) // the real client, which finds a file that isn't a socket
		}
		return next(ep)
	}
	page := y.b.do("GET", "/stacks/app/remove", nil, nil).Body.String()
	if !strings.Contains(page, "Folder deletion is off") || strings.Contains(page, `value="`+y.path("app-data")+`">`) {
		t.Fatal("paths offered while podman could not be checked")
	}
	w := y.b.do("POST", "/stacks/app/remove", withCSRF(y, url.Values{"confirm": {"app"}, "path": {y.path("app-data")}}), nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "podman at "+broken+" did not answer") {
		t.Fatalf("post while blocked: %d", w.Code)
	}
	// Down alone still works: it deletes nothing.
	w = y.b.do("POST", "/stacks/app/remove", withCSRF(y, url.Values{}), nil)
	if exit := waitJobPage(t, y.h, w.Header().Get("Location")); exit != 0 || !y.exists("app-data/db") {
		t.Fatalf("plain down: exit %d", exit)
	}
}

// Fleetling's own data folder is never offered, even when it sits inside a
// stack folder that is.
func TestRemoveKeepsFleetlingData(t *testing.T) {
	y := newRemoveYard(t)
	y.h.s.cfg.DataDir = y.path("app/conf")
	page := y.b.do("GET", "/stacks/app/remove", nil, nil).Body.String()
	for _, p := range []string{y.path("app/conf"), y.dir} {
		if strings.Contains(page, `value="`+p+`">`) {
			t.Errorf("offered %s, which holds Fleetling's data", p)
		}
	}
	if !strings.Contains(page, "Kept: holds Fleetling&#39;s own data") {
		t.Error("no reason shown")
	}
}

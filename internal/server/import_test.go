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
	"strings"
	"testing"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/engine/enginetest"
	"github.com/NullAngst/Fleetling/internal/store"
)

const giteaCompose = "# gitea from portainer\nservices:\n  gitea:\n    image: gitea/gitea:1\n    volumes:\n      - ./data:/data\n"

func npmCompose(root string) string {
	return "services:\n  npmplus:\n    image: zoeyvid/npmplus\n    env_file: stack.env\n    volumes:\n      - " + root + "/npmplus_data/data:/data\n      - " + root + "/npmplus_data/certs:/certs\n"
}

func importHarness(t *testing.T) (*harness, *browser, *enginetest.Engine, *httptest.Server) {
	t.Helper()
	h, b, fe := containerHarness(t)
	root := h.root
	fe.Add(&enginetest.Container{ID: strings.Repeat("11", 32), Name: "gitea-gitea-1", Image: "gitea/gitea:1", Running: true,
		Labels: map[string]string{engine.LabelProject: "gitea-pt", engine.LabelService: "gitea", engine.LabelWorkingDir: "/data/compose/3"},
		Mounts: []enginetest.Mount{{Source: root + "/portainer/compose/3/data", Destination: "/data"}}})
	fe.Add(&enginetest.Container{ID: strings.Repeat("22", 32), Name: "npmplus", Image: "zoeyvid/npmplus", Running: true,
		Labels: map[string]string{engine.LabelProject: "npmplus", engine.LabelService: "npmplus", engine.LabelWorkingDir: "/data/compose/8"}})
	fe.Add(&enginetest.Container{ID: strings.Repeat("33", 32), Name: "portainer", Image: "portainer/portainer-ce:2.33.1", Running: true})

	files := map[string]string{"3": giteaCompose, "8": npmCompose(root)}
	pt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "ptr_good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/endpoints":
			w.Write([]byte(`[{"Id":1,"Name":"local","Type":1,"URL":"unix:///var/run/docker.sock"}]`))
		case r.URL.Path == "/api/stacks":
			w.Write([]byte(`[{"Id":3,"Name":"gitea-pt","Type":2,"Status":1,"EndpointId":1,"Env":[{"name":"TZ","value":"UTC"}]},
				{"Id":8,"Name":"npmplus","Type":2,"Status":1,"EndpointId":1,"Env":[{"name":"ACME_EMAIL","value":"me@example.com"}]},
				{"Id":9,"Name":"oldswarm","Type":1,"Status":1,"EndpointId":1}]`))
		case strings.HasSuffix(r.URL.Path, "/file"):
			id := strings.Split(r.URL.Path, "/")[3]
			json.NewEncoder(w).Encode(map[string]string{"StackFileContent": files[id]})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(pt.Close)
	return h, b, fe, pt
}

// The phase 4 done-when: every Portainer stack shows Running after import
// with zero restarts.
func TestImportFromAPI(t *testing.T) {
	h, b, fe, pt := importHarness(t)
	w := b.do("POST", "/import/connect", url.Values{"csrf": {b.token("/import")}, "url": {pt.URL}, "api_key": {"ptr_good"}}, nil)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "Import preview") {
		t.Fatalf("connect: %d %s", w.Code, body)
	}
	for _, want := range []string{"Swarm stack", `name="folder_p8" class="mono short" value="npmplus_data"`, `name="folder_p3" class="mono short" value="gitea-pt"`, "folder is a guess", h.root + "/portainer/compose/3/data:/data"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q", want)
		}
	}
	tok := csrfRe.FindStringSubmatch(body)[1]
	w = b.do("POST", "/import/apply", url.Values{"csrf": {tok},
		"include_p3": {"1"}, "folder_p3": {"gitea"}, "fix_p3": {"1"},
		"include_p8": {"1"}, "folder_p8": {"npmplus_data"}}, nil)
	body = w.Body.String()
	if w.Code != http.StatusOK || strings.Contains(body, "msg-error") {
		t.Fatalf("apply: %d %s", w.Code, body)
	}
	if n := strings.Count(body, "</span>Running</td>"); n != 2 {
		t.Errorf("want 2 stacks Running after import, got %d:\n%s", n, body)
	}

	gitea := filepath.Join(h.root, "gitea")
	got, _ := os.ReadFile(filepath.Join(gitea, "compose.yaml"))
	if string(got) != strings.Replace(giteaCompose, "./data:/data", h.root+"/portainer/compose/3/data:/data", 1) {
		t.Errorf("gitea compose:\n%s", got)
	}
	if env, _ := os.ReadFile(filepath.Join(gitea, ".env")); string(env) != "TZ=UTC\n" {
		t.Errorf("gitea .env %q", env)
	}
	if fi, _ := os.Stat(filepath.Join(gitea, ".env")); fi.Mode().Perm() != 0o600 {
		t.Errorf(".env mode %v", fi.Mode())
	}
	m, err := compose.ReadMeta(gitea)
	if err != nil || m.Project != "gitea-pt" || m.Engine != "docker" || m.Deployed == nil {
		t.Errorf("meta %+v %v", m, err)
	}

	npm := filepath.Join(h.root, "npmplus_data")
	if got, _ := os.ReadFile(filepath.Join(npm, "compose.yaml")); string(got) != npmCompose(h.root) {
		t.Errorf("npmplus compose changed:\n%s", got)
	}
	if link, err := os.Readlink(filepath.Join(npm, ".env")); err != nil || link != "stack.env" {
		t.Errorf(".env should link to stack.env: %q %v", link, err)
	}
	if env, _ := os.ReadFile(filepath.Join(npm, "stack.env")); string(env) != "ACME_EMAIL=me@example.com\n" {
		t.Errorf("stack.env %q", env)
	}

	// Zero restarts: no lifecycle call reached the engine and Compose never ran.
	if calls := fe.Calls(); len(calls) != 0 {
		t.Errorf("import touched containers: %v", calls)
	}
	if log, err := os.ReadFile(h.dockerLog); err == nil && len(log) > 0 {
		t.Errorf("import ran docker:\n%s", log)
	}
	if h.s.importSession() != nil {
		t.Error("import session (with credentials) kept after applying")
	}

	// Retire removes the Portainer container and nothing else.
	w = b.do("POST", "/import/retire", url.Values{"csrf": {b.token("/import")}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("retire: %d %s", w.Code, w.Body.String())
	}
	if calls := strings.Join(fe.Calls(), ","); calls != "DELETE remove portainer" {
		t.Errorf("retire calls %s", calls)
	}
}

func TestImportRefusesTakenFolder(t *testing.T) {
	h, b, _, pt := importHarness(t)
	os.MkdirAll(filepath.Join(h.root, "gitea"), 0o755)
	os.WriteFile(filepath.Join(h.root, "gitea", "compose.yaml"), []byte("services: {}\n"), 0o644)
	w := b.do("POST", "/import/connect", url.Values{"csrf": {b.token("/import")}, "url": {pt.URL}, "api_key": {"ptr_good"}}, nil)
	tok := csrfRe.FindStringSubmatch(w.Body.String())[1]
	w = b.do("POST", "/import/apply", url.Values{"csrf": {tok}, "include_p3": {"1"}, "folder_p3": {"gitea"}}, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "already holds a stack") {
		t.Fatalf("conflict: %d", w.Code)
	}
	if got, _ := os.ReadFile(filepath.Join(h.root, "gitea", "compose.yaml")); string(got) != "services: {}\n" {
		t.Error("existing compose file overwritten")
	}
}

func TestImportOffline(t *testing.T) {
	h, b, fe := containerHarness(t)
	data := filepath.Join(h.root, "portainer")
	os.MkdirAll(filepath.Join(data, "compose", "7"), 0o755)
	os.WriteFile(filepath.Join(data, "compose", "7", "docker-compose.yml"), []byte("services:\n  app:\n    image: app:1\n    environment:\n      TZ: UTC\n"), 0o644)
	os.MkdirAll(filepath.Join(data, "compose", "12"), 0o755)
	os.WriteFile(filepath.Join(data, "compose", "12", "docker-compose.yml"), []byte("services: {x: {image: x}}\n"), 0o644)
	fe.ImageEnv["app:1"] = []string{"PATH=/usr/bin"}
	fe.Add(&enginetest.Container{ID: strings.Repeat("44", 32), Name: "myapp-app-1", Image: "app:1", Running: true,
		Env:    []string{"PATH=/usr/bin", "TZ=UTC", "DB_PASSWORD=hunter2"},
		Labels: map[string]string{engine.LabelProject: "myapp", engine.LabelService: "app", engine.LabelWorkingDir: "/data/compose/7"}})

	w := b.do("POST", "/import/offline", url.Values{"csrf": {b.token("/import")}, "data_path": {data}}, nil)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "DB_PASSWORD=hunter2") || !strings.Contains(body, "its project name is unknown") {
		t.Fatalf("offline preview: %d %s", w.Code, body)
	}
	// TZ is also in the compose file's environment:, so it starts unticked.
	if regexp.MustCompile(`name="env_o7_\d" value="1" checked> TZ=UTC`).MatchString(body) {
		t.Error("TZ suggestion pre-ticked though it comes from the compose file")
	}
	tok := csrfRe.FindStringSubmatch(body)[1]
	w = b.do("POST", "/import/apply", url.Values{"csrf": {tok}, "include_o7": {"1"}, "folder_o7": {"myapp"}, "env_o7_0": {"1"}}, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Running") {
		t.Fatalf("offline apply: %d %s", w.Code, w.Body.String())
	}
	if env, _ := os.ReadFile(filepath.Join(h.root, "myapp", ".env")); string(env) != "DB_PASSWORD=hunter2\n" {
		t.Errorf(".env %q", env)
	}
	if m, err := compose.ReadMeta(filepath.Join(h.root, "myapp")); err != nil || m.Project != "myapp" {
		t.Errorf("meta %+v %v", m, err)
	}
	_ = store.Settings{}
}

func TestImportBadToken(t *testing.T) {
	_, b, _, pt := importHarness(t)
	w := b.do("POST", "/import/connect", url.Values{"csrf": {b.token("/import")}, "url": {pt.URL}, "api_key": {"nope"}}, nil)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "401") {
		t.Errorf("bad token: %d", w.Code)
	}
	_ = context.Background()
}

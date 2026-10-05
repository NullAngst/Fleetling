package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/engine/enginetest"
	"github.com/NullAngst/Fleetling/internal/store"
)

var giteaID = strings.Repeat("ab", 32)

// containerHarness points the docker slot at a fake Engine API, so these
// tests go through the real moby client, stdcopy and exec hijacking.
func containerHarness(t *testing.T) (*harness, *browser, *enginetest.Engine) {
	t.Helper()
	fe, err := enginetest.Start(
		&enginetest.Container{
			ID: giteaID, Name: "gitea", Image: "gitea/gitea:1", Running: true,
			Labels: map[string]string{engine.LabelProject: "gitea", engine.LabelService: "gitea", engine.LabelWorkingDir: "/opt/gitea"},
			Logs:   []enginetest.Line{{Text: "\x1b[32mserver started\x1b[0m"}, {Stderr: true, Text: "warning: slow disk"}},
		},
		&enginetest.Container{ID: strings.Repeat("cd", 32), Name: "distroless", Image: "gcr.io/distroless/static", Running: true, NoShell: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fe.Close)
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	old := h.s.newEngine
	h.s.newEngine = func(ep string) (Engine, error) {
		if ep == fe.Socket {
			return engine.New(ep)
		}
		return old(ep)
	}
	if err := h.s.store.SaveSettings(context.Background(), store.Settings{Root: h.root, DockerHost: fe.Socket, Ignore: []string{"containerd"}}); err != nil {
		t.Fatal(err)
	}
	return h, b, fe
}

func dial(t *testing.T, h *harness, b *browser, path string) (*websocket.Conn, context.Context) {
	t.Helper()
	srv := httptest.NewServer(h.s)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	hdr := http.Header{}
	hdr.Add("Cookie", sessionCookie+"="+b.cookies[sessionCookie].Value)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c, ctx
}

func TestContainersPages(t *testing.T) {
	h, b, _ := containerHarness(t)
	list := b.do("GET", "/containers", nil, nil).Body.String()
	for _, want := range []string{"/containers/docker/" + giteaID, "gitea/gitea:1", "3000-&gt;3000/tcp", "distroless"} {
		if !strings.Contains(list, want) {
			t.Errorf("containers page missing %q", want)
		}
	}
	detail := b.do("GET", "/containers/docker/"+giteaID, nil, nil).Body.String()
	for _, want := range []string{"data-logs=\"/containers/docker/" + giteaID + "/logs/ws\"", "docker rm -f gitea", "every 5s"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail page missing %q", want)
		}
	}
	if s := b.do("GET", "/containers/docker/"+giteaID+"/stats", nil, nil).Body.String(); !strings.Contains(s, "80.0%") || !strings.Contains(s, "200.0 MiB") {
		t.Errorf("stats partial: %s", s)
	}
	if s := b.do("GET", "/containers/docker/"+giteaID+"?tab=inspect", nil, nil).Body.String(); !strings.Contains(s, "&#34;Id&#34;") {
		t.Error("inspect tab has no JSON")
	}
	if s := b.do("GET", "/containers/docker/"+giteaID+"?tab=shell", nil, nil).Body.String(); !strings.Contains(s, "/static/dist/shell.js") {
		t.Error("shell tab does not load xterm")
	}
	_ = h
}

func TestContainerLogsSocket(t *testing.T) {
	h, b, _ := containerHarness(t)
	c, ctx := dial(t, h, b, "/containers/docker/"+giteaID+"/logs/ws?tail=100&follow=0")
	var got []logLine
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m logMessage
		json.Unmarshal(data, &m)
		got = append(got, m.Lines...)
		if m.Done {
			if m.Error != "" {
				t.Errorf("done with error %s", m.Error)
			}
			break
		}
	}
	if len(got) != 2 || got[0].Text != "\x1b[32mserver started\x1b[0m" || got[0].Err || !got[1].Err || got[1].Text != "warning: slow disk" {
		t.Errorf("lines %+v", got)
	}
}

// The phase 3 done-when, against the fake engine: a shell into gitea opens,
// resizes, and closes cleanly, and the session lands in the action log.
func TestShellSocket(t *testing.T) {
	h, b, fe := containerHarness(t)
	c, ctx := dial(t, h, b, "/containers/docker/"+giteaID+"/shell/ws?cols=100&rows=30&user=git")
	var out strings.Builder
	readUntil := func(want string) {
		t.Helper()
		for !strings.Contains(out.String(), want) {
			typ, data, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("waiting for %q: %v\nso far: %q", want, err, out.String())
			}
			if typ != websocket.MessageBinary {
				t.Fatalf("unexpected control message %s while waiting for %q", data, want)
			}
			out.Write(data)
		}
	}
	readUntil("$ ")
	if !strings.Contains(out.String(), `fake shell as "git"`) {
		t.Errorf("user not passed: %q", out.String())
	}
	c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`))
	c.Write(ctx, websocket.MessageBinary, []byte("whoami\r"))
	readUntil("you typed: whoami")
	c.Write(ctx, websocket.MessageBinary, []byte("exit\r"))
	var exit shellControl
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no exit message: %v", err)
		}
		if typ == websocket.MessageText {
			json.Unmarshal(data, &exit)
			break
		}
	}
	if exit.Type != "exit" || exit.Code != 0 {
		t.Errorf("control %+v", exit)
	}
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("close: %v", err)
	}
	if r := fe.Resizes(); len(r) != 1 || r[0] != "120x40" {
		t.Errorf("resizes %v", r)
	}
	time.Sleep(50 * time.Millisecond)
	acts, _ := h.s.store.ListActions(context.Background(), "container/gitea", 5, 0)
	if len(acts) != 1 || acts[0].Command != "docker exec -it -u git gitea 'bash || sh'" || acts[0].ExitCode == nil || *acts[0].ExitCode != 0 {
		t.Errorf("action log %+v", acts)
	}
}

func TestShellWithoutShellExplains(t *testing.T) {
	h, b, _ := containerHarness(t)
	c, ctx := dial(t, h, b, "/containers/docker/distroless/shell/ws")
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no error message: %v", err)
		}
		if typ == websocket.MessageText {
			var m shellControl
			json.Unmarshal(data, &m)
			if m.Type != "error" || !strings.Contains(m.Message, "Distroless") {
				t.Errorf("control %+v", m)
			}
			return
		}
	}
}

func TestContainerActions(t *testing.T) {
	h, b, fe := containerHarness(t)
	tok := b.token("/containers/docker/" + giteaID)
	if w := b.do("POST", "/containers/docker/"+giteaID+"/action/stop", url.Values{"csrf": {tok}}, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	if calls := strings.Join(fe.Calls(), ","); calls != "POST stop gitea" {
		t.Errorf("calls %s", calls)
	}
	acts, _ := h.s.store.ListActions(context.Background(), "container/gitea", 5, 0)
	if len(acts) != 1 || acts[0].Command != "docker stop gitea" {
		t.Errorf("log %+v", acts)
	}
	if w := b.do("POST", "/containers/docker/"+giteaID+"/action/explode", url.Values{"csrf": {tok}}, nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown action: %d", w.Code)
	}

	// Fleetling's own container refuses everything but start.
	h.inCtr = true
	h.s.selfIDs = func() []string { return []string{giteaID[:12]} }
	if w := b.do("POST", "/containers/docker/"+giteaID+"/action/remove", url.Values{"csrf": {tok}}, nil); w.Code != http.StatusConflict {
		t.Errorf("remove self: %d", w.Code)
	}
}

func TestRecreateGoesThroughCompose(t *testing.T) {
	h, b, _ := containerHarness(t)
	dir := filepath.Join(h.root, "gitea")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {gitea: {image: x}}\n"), 0o644)
	compose.WriteMeta(dir, &compose.Meta{Project: "gitea", Engine: "docker", Deployed: ptr(time.Now())}, compose.Owner{})
	h.approve(dir)
	if !strings.Contains(b.do("GET", "/containers/docker/"+giteaID, nil, nil).Body.String(), "--force-recreate --no-deps gitea") {
		t.Fatal("no Recreate for a managed stack's container")
	}
	w := b.do("POST", "/containers/docker/"+giteaID+"/action/recreate", url.Values{"csrf": {b.token("/containers/docker/" + giteaID)}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("recreate: %d %s", w.Code, w.Body.String())
	}
	waitJob(t, h, w.Header().Get("Location"))
	if log, _ := os.ReadFile(h.dockerLog); !strings.Contains(string(log), "up -d --force-recreate --no-deps gitea") {
		t.Errorf("docker log:\n%s", log)
	}
}

func TestStackLogsSocket(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	h.setUp(b)
	dir := filepath.Join(h.root, "app")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {app: {image: x}}\n"), 0o644)
	compose.WriteMeta(dir, &compose.Meta{Project: "app", Engine: "docker"}, compose.Owner{})
	h.approve(dir)
	c, ctx := dial(t, h, b, "/stacks/app/logs/ws?tail=200&follow=1")
	var lines []string
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var m logMessage
		json.Unmarshal(data, &m)
		for _, l := range m.Lines {
			lines = append(lines, l.Text)
		}
		if m.Done {
			break
		}
	}
	if strings.Join(lines, "|") != "app-1  | hello from app|db-1   | ready" {
		t.Errorf("lines %q", lines)
	}
	if log, _ := os.ReadFile(h.dockerLog); !strings.Contains(string(log), "--progress plain logs --tail 200 -f") {
		t.Errorf("docker log:\n%s", log)
	}
}

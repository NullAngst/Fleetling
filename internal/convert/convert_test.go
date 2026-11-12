package convert

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The phase 10 done-when: the lint flags devices and network_mode: host on
// a Plex compose file, at the lines they are on.
func TestLintPlex(t *testing.T) {
	user, _ := os.ReadFile("testdata/plex.yaml")
	norm, _ := os.ReadFile("testdata/plex.json")
	ws, err := Lint(norm, string(user))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, w := range ws {
		got[w.What] = w.Line
		if w.Quadlet == "" || w.Kubernetes == "" {
			t.Errorf("%s: missing a target note", w.What)
		}
	}
	for what, line := range map[string]int{"network_mode: host": 7, "devices": 17, "container_name: plex": 6, "relative bind mount ./config": 14} {
		if got[what] != line {
			t.Errorf("%s: line %d, want %d (all: %v)", what, got[what], line, got)
		}
	}
	if len(ws) != 4 {
		t.Errorf("want 4 warnings, got %d: %+v", len(ws), ws)
	}
}

func TestLintEveryRow(t *testing.T) {
	user := `services:
  app:
    image: nginx
    build: ./b
    depends_on:
      db:
        condition: service_healthy
      cache:
        condition: service_started
    healthcheck:
      test: ["CMD", "true"]
    privileged: true
    cap_add: [NET_ADMIN]
    env_file: app.env
  db:
    image: postgres
`
	norm := `{"services":{
	  "app":{"build":{"context":"/x/b"},"depends_on":{"db":{"condition":"service_healthy"},"cache":{"condition":"service_started"}},
	         "healthcheck":{"test":["CMD","true"]},"privileged":true,"cap_add":["NET_ADMIN"]},
	  "db":{"healthcheck":{"disable":true}}}}`
	ws, err := Lint([]byte(norm), user)
	if err != nil {
		t.Fatal(err)
	}
	var whats []string
	for _, w := range ws {
		whats = append(whats, w.What)
	}
	want := []string{"build", "depends_on with conditions: db (service_healthy)", "healthcheck", "privileged: true", "cap_add: NET_ADMIN", "relative env_file app.env"}
	if strings.Join(whats, "|") != strings.Join(want, "|") {
		t.Errorf("got  %q\nwant %q", whats, want)
	}
	// A setting that came from an include or extends file has no line in
	// this file and sorts last.
	ws, _ = Lint([]byte(`{"services":{"x":{"privileged":true},"y":{"network_mode":"host"}}}`), "services:\n  y:\n    network_mode: host\n")
	if ws[0].Line != 3 || ws[1].Line != 0 {
		t.Errorf("unknown lines not last: %+v", ws)
	}
}

func TestSplitPodlet(t *testing.T) {
	out := "# FileName=plex\n[Container]\nImage=lscr.io/linuxserver/plex:latest\nNetwork=host\n\n---\n\n# FileName=plex\n[Pod]\nPodName=plex\n"
	files, err := splitPodlet(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "plex.container" || files[1].Name != "plex.pod" || !strings.HasPrefix(files[0].Content, "[Container]") {
		t.Fatalf("%+v", files)
	}
	cmds := InstallCommands(files)
	if cmds[0] != "sudo cp plex.container plex.pod /etc/containers/systemd/" || cmds[2] != "sudo systemctl start plex-pod.service" {
		t.Errorf("install: %q", cmds)
	}
	if _, err := splitPodlet("# FileName=../../etc/x\n[Container]\n"); err == nil {
		t.Error("path in a podlet file name accepted")
	}
}

func TestSplitManifestsAndTar(t *testing.T) {
	out := "---\napiVersion: v1\nkind: Service\nmetadata:\n  name: plex\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: plex\n"
	files, err := splitManifests([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "plex-service.yaml" || files[1].Name != "plex-deployment.yaml" {
		t.Fatalf("%+v", files)
	}
	b, err := Tar("k8s", files, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "k8s/,k8s/plex-service.yaml,k8s/plex-deployment.yaml" {
		t.Errorf("tar: %v", names)
	}
}

func TestWithServiceType(t *testing.T) {
	in := "services:\n  a:\n    image: x\n  b:\n    image: y\n    labels:\n      keep: \"1\"\n  c:\n    image: z\n    labels: [\"k=v\"]\n"
	b, err := withServiceType([]byte(in), "nodeport")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Count(s, "kompose.service.type") != 3 || !strings.Contains(s, "keep: \"1\"") {
		t.Errorf("labels:\n%s", s)
	}
	o := K8sOptions{ServiceType: "evil; rm"}
	if o.Valid() == nil {
		t.Error("bad option accepted")
	}
}

// Runs the real tools when they are installed (the image has all three).
func TestRealTools(t *testing.T) {
	for _, b := range []string{"docker", "podlet", "kompose"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip(b + " not installed")
		}
	}
	user, _ := os.ReadFile("testdata/plex.yaml")
	tools := Tools{}
	ctx := context.Background()
	y, j, err := tools.Normalize(ctx, Input{Project: "plex", Dir: "/opt/plex", Text: string(user)})
	if err != nil {
		t.Fatal(err)
	}
	if ws, err := Lint(j, string(user)); err != nil || len(ws) != 4 {
		t.Errorf("lint on live config: %v %+v", err, ws)
	}
	q, err := tools.Quadlet(ctx, y)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("quadlet: %+v", q)
	k, notes, err := tools.Kubernetes(ctx, y, K8sOptions{ServiceType: "nodeport"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("k8s: %d files, notes %q", len(k), notes)
}

func TestForPodlet(t *testing.T) {
	in := "name: x\nservices:\n  a:\n    image: nginx\n    build:\n      context: /x\n    devices:\n      - source: /dev/dri\n        target: /dev/dri\n        permissions: rwm\n    depends_on:\n      b:\n        condition: service_healthy\n        required: true\n  b:\n    build:\n      context: /y\n"
	out, err := forPodlet([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"- /dev/dri:/dev/dri:rwm", "condition: service_started", "context: /y"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "context: /x") {
		t.Errorf("build kept next to image:\n%s", s)
	}
	if got := cleanMessage("\x1b[91mError:\x1b[0m\n   0: bad\n\nLocation:\n   src/x.rs:1\n"); got != "Error:\n   0: bad" {
		t.Errorf("cleanMessage: %q", got)
	}
}

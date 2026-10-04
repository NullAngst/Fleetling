package compose

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRunnerArgsAndEnv(t *testing.T) {
	r := Runner{}
	tg := Target{Project: "gitea", Dir: "/opt/gitea", File: "compose.yaml", Host: "unix:///var/run/docker.sock"}
	got := Quote(r.Args(tg, "down"))
	want := "docker compose -p gitea --project-directory /opt/gitea -f /opt/gitea/compose.yaml --ansi never --progress plain down"
	if got != want {
		t.Errorf("args:\n got %s\nwant %s", got, want)
	}
	t.Setenv("COMPOSE_PROJECT_NAME", "wrong")
	t.Setenv("DOCKER_HOST", "tcp://elsewhere:2375")
	env := r.Env(Target{Host: "unix:///run/podman/podman.sock", Podman: true})
	if slices.Contains(env, "COMPOSE_PROJECT_NAME=wrong") || slices.Contains(env, "DOCKER_HOST=tcp://elsewhere:2375") {
		t.Error("inherited COMPOSE_ or DOCKER_HOST leaked into the subprocess")
	}
	if !slices.Contains(env, "DOCKER_HOST=unix:///run/podman/podman.sock") || !slices.Contains(env, "DOCKER_BUILDKIT=0") {
		t.Errorf("podman env missing DOCKER_HOST or DOCKER_BUILDKIT=0: %v", env)
	}
}

func TestQuote(t *testing.T) {
	if got := Quote([]string{"docker", "compose", "-f", "/opt/my app/compose.yaml", "it's"}); got != `docker compose -f '/opt/my app/compose.yaml' 'it'\''s'` {
		t.Errorf("got %s", got)
	}
}

func TestFolderBindMounts(t *testing.T) {
	y := `services:
  copyparty:
    volumes:
      - /opt/copyparty:/cfg:z
  rel:
    volumes:
      - .:/data
  sub:
    volumes:
      - ./cfg:/cfg
      - named:/x
  long:
    volumes:
      - type: bind
        source: /opt/copyparty/
        target: /y
`
	got := FolderBindMounts([]byte(y), "/opt/copyparty")
	if !slices.Equal(got, []string{"copyparty", "long", "rel"}) {
		t.Errorf("got %v", got)
	}
}

func TestBindSourcesAndCreated(t *testing.T) {
	root := t.TempDir()
	exists := filepath.Join(root, "app", "conf")
	os.MkdirAll(exists, 0o755)
	later := filepath.Join(root, "app", "data")
	outside := filepath.Join(t.TempDir(), "elsewhere")
	js := `{"services":{"a":{"volumes":[{"type":"bind","source":"` + exists + `"},{"type":"bind","source":"` + later + `"},{"type":"volume","source":"named"}]},"b":{"volumes":[{"type":"bind","source":"` + outside + `"},{"type":"bind","source":"` + later + `"}]}}}`
	srcs, err := BindSources([]byte(js))
	if err != nil || len(srcs) != 3 {
		t.Fatalf("sources %v %v", srcs, err)
	}
	missing := Missing(srcs)
	if !slices.Equal(missing, []string{later, outside}) && !slices.Equal(missing, []string{outside, later}) {
		t.Fatalf("missing %v", missing)
	}
	os.MkdirAll(later, 0o755)   // Docker creates it during up
	os.MkdirAll(outside, 0o755) // outside the root, never recorded
	if got := NewlyCreated(missing, root); !slices.Equal(got, []string{later}) {
		t.Errorf("created %v", got)
	}
	if got := MergePaths([]string{"/b", "/a"}, []string{"/a", "/c"}); !slices.Equal(got, []string{"/a", "/b", "/c"}) {
		t.Errorf("merge %v", got)
	}
}

func TestWriteMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &Meta{Project: "copyparty", Engine: "docker", Created: time.Date(2026, 10, 4, 15, 20, 0, 0, time.UTC)}
	if err := WriteMeta(dir, m, Owner{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, MetaFile))
	if strings.Contains(string(b), "deployed") {
		t.Errorf("zero deployed time was written:\n%s", b)
	}
	dep := m.Created.Add(time.Minute)
	m.Deployed = &dep
	m.CreatedPaths = []string{"/opt/copyparty/hists"}
	if err := WriteMeta(dir, m, Owner{}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Deployed == nil || !got.Deployed.Equal(*m.Deployed) || !slices.Equal(got.CreatedPaths, m.CreatedPaths) {
		t.Errorf("got %+v", got)
	}
	if err := WriteMeta(dir, &Meta{Project: "Bad"}, Owner{}); err == nil {
		t.Error("wrote metadata with an invalid project")
	}
}

func TestWriteFileAtomicKeepsBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "compose.yaml")
	data := []byte("# my comment\nservices:\r\n  a: {image: x}   \n")
	if err := WriteFileAtomic(p, data, 0o600, Owner{}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	if string(got) != string(data) || fi.Mode().Perm() != 0o600 {
		t.Errorf("bytes or mode changed: %q %v", got, fi.Mode())
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".fleetling-tmp-*"))
	if len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestValidFolderName(t *testing.T) {
	for _, ok := range []string{"gitea", "npmplus_data", "My Stack"} {
		if !ValidFolderName(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", ".hidden", "a/b", `a\b`, "a\x00b"} {
		if ValidFolderName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestJobsRunAndStream(t *testing.T) {
	js := NewJobs(context.Background())
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	var after int = -99
	done := make(chan struct{})
	j, err := js.Start(JobSpec{
		Target: "x",
		Steps: []Step{
			{Cmd: exec.Command(sh, "-c", "echo one; echo two >&2; printf 'a\\rb\\n'")},
			{Cmd: exec.Command(sh, "-c", "exit 3")},
			{Cmd: exec.Command(sh, "-c", "echo never")},
		},
		After: func(_ context.Context, _ *Job, exit int) { after = exit },
		Done:  func(*Job) { close(done) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Start(JobSpec{Target: "x"}); err != ErrBusy {
		// The first job may already be done on a fast machine; only a
		// still-running job must refuse.
		if d, _, _ := j.Result(); !d {
			t.Errorf("second job on a busy stack: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exit, err := j.Wait(ctx)
	if err != nil || exit != 3 {
		t.Fatalf("exit %d %v", exit, err)
	}
	<-done
	lines, _, isDone, _, _ := j.Since(0)
	out := strings.Join(lines, "\n")
	for _, want := range []string{"one", "two", "a", "b", "exit code 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "never") || !isDone || after != 3 {
		t.Errorf("ran past a failed step, or After/done wrong: after=%d done=%v\n%s", after, isDone, out)
	}
	if js.Running("x") != nil {
		t.Error("stack still marked busy after the job finished")
	}
}

package updater

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/engine/enginetest"
)

// fakeDocker logs every call and prints a resolved config for Fleetling's
// own stack: one service that builds and is tagged fleetling:local.
const fakeDocker = `#!/bin/sh
echo "$*" >> "$FAKE_DOCKER_LOG"
case " $* " in
  *" config --format json "*)
    echo '{"services":{"fleetling":{"image":"fleetling:local","build":{"context":"https://github.com/NullAngst/Fleetling.git#main"}}}}'; exit 0;;
  *" up "*) echo " Container fleetling  Started"; exit 0;;
esac
exit 0
`

type fixture struct {
	root, dir, log, dockerLog string
	fe                        *enginetest.Engine
	cfg                       Config
}

func setup(t *testing.T, health string) *fixture {
	t.Helper()
	tmp := t.TempDir()
	fx := &fixture{root: filepath.Join(tmp, "opt")}
	fx.dir = filepath.Join(fx.root, "fleetling")
	if err := os.MkdirAll(fx.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(fx.dir, "compose.yaml"), []byte("services:\n  fleetling:\n    build: https://github.com/NullAngst/Fleetling.git#main\n    image: fleetling:local\n"), 0o644)
	if err := compose.WriteMeta(fx.dir, &compose.Meta{Project: "fleetling", Engine: compose.EngineDocker}, compose.Owner{}); err != nil {
		t.Fatal(err)
	}
	fx.dockerLog = filepath.Join(tmp, "docker.log")
	t.Setenv("FAKE_DOCKER_LOG", fx.dockerLog)
	bin := filepath.Join(tmp, "docker")
	os.WriteFile(bin, []byte(fakeDocker), 0o755)

	fe, err := enginetest.Start(&enginetest.Container{
		ID: strings.Repeat("a", 64), Name: "fleetling", Image: "fleetling:local", ImageID: "sha256:old", Running: true, Health: health,
		Labels: map[string]string{engine.LabelProject: "fleetling", engine.LabelService: "fleetling"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fe.Close)
	fx.fe = fe
	cl, err := engine.New("unix://" + fe.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	fx.log = filepath.Join(fx.dir, "data", LogFile)
	fx.cfg = Config{
		Root: fx.root, Dir: fx.dir, LogPath: fx.log, Mode: ModeUpdate, Host: "unix://" + fe.Socket,
		Runner: compose.Runner{Docker: bin}, Engine: cl, Timeout: 300 * time.Millisecond, Poll: 50 * time.Millisecond,
	}
	return fx
}

func (fx *fixture) calls(t *testing.T) []string {
	b, err := os.ReadFile(fx.dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		// Drop the long compose prefix so the order reads plainly.
		if i := strings.Index(l, "--progress plain "); i >= 0 {
			l = "compose " + l[i+len("--progress plain "):]
		}
		out = append(out, l)
	}
	return out
}

// The phase 7 "done when": the new container never reports healthy, so
// the helper re-tags the recorded image ID and runs up -d again without
// building, in that order.
func TestUpdateRollsBackWhenNeverHealthy(t *testing.T) {
	fx := setup(t, "starting")
	err := Run(context.Background(), fx.cfg)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("want a rollback error, got %v", err)
	}
	want := []string{
		"compose config --format json",
		"tag sha256:old fleetling:fleetling-previous",
		"compose build --pull",
		"compose up -d",
		"tag sha256:old fleetling:local",
		"compose up -d --no-build",
	}
	got := fx.calls(t)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls in the wrong order:\n got: %q\nwant: %q", got, want)
	}
	log, _ := os.ReadFile(fx.log)
	for _, w := range []string{"current image ID sha256:old", "still not healthy: fleetling=starting", "rolling back", "$ docker tag sha256:old fleetling:local", "FAILED: rolled back"} {
		if !strings.Contains(string(log), w) {
			t.Errorf("log missing %q:\n%s", w, log)
		}
	}
	if fi, err := os.Stat(fx.log); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode: %v %v", fi, err)
	}
}

func TestUpdateHealthyNoRollback(t *testing.T) {
	fx := setup(t, "starting")
	// Healthy a moment after up, the way a real health check flips.
	go func() {
		time.Sleep(120 * time.Millisecond)
		fx.fe.Update(strings.Repeat("a", 64), func(c *enginetest.Container) { c.Health = "healthy" })
	}()
	fx.cfg.Timeout = 3 * time.Second
	if err := Run(context.Background(), fx.cfg); err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.calls(t) {
		if c == "tag sha256:old fleetling:local" || strings.Contains(c, "--no-build") {
			t.Errorf("rolled back a healthy update: %q", c)
		}
	}
}

func TestRestartMode(t *testing.T) {
	fx := setup(t, "healthy")
	fx.cfg.Mode = ModeRestart
	if err := Run(context.Background(), fx.cfg); err != nil {
		t.Fatal(err)
	}
	got := fx.calls(t)
	if got[len(got)-1] != "compose restart" {
		t.Fatalf("restart calls: %q", got)
	}
}

func TestPreviousTag(t *testing.T) {
	for in, want := range map[string]string{
		"fleetling:local":                         "fleetling:fleetling-previous",
		"lscr.io/linuxserver/plex":                "lscr.io/linuxserver/plex:fleetling-previous",
		"registry.lan:5000/app:1.2":               "registry.lan:5000/app:fleetling-previous",
		"nginx@sha256:" + strings.Repeat("a", 64): "nginx:fleetling-previous",
		"Not A Ref!":                              "fleetling-rollback:previous",
	} {
		if got := PreviousTag(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestValidateDir(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "fleetling", "deep"), 0o755)
	os.Symlink(filepath.Join(root, "fleetling"), filepath.Join(root, "link"))
	if _, err := ValidateDir(root, filepath.Join(root, "fleetling")); err != nil {
		t.Errorf("good dir refused: %v", err)
	}
	for _, bad := range []string{
		root, filepath.Join(root, "fleetling", "deep"), "/etc", filepath.Join(root, ".hidden"),
		filepath.Join(root, "link"), "relative", filepath.Join(root, "missing"),
	} {
		if _, err := ValidateDir(root, bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

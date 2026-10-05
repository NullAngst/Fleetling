package engine

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/NullAngst/Fleetling/internal/engine/enginetest"
)

func fake(t *testing.T, cs ...*enginetest.Container) (*enginetest.Engine, *Client) {
	t.Helper()
	fe, err := enginetest.Start(cs...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fe.Close)
	c, err := New(fe.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return fe, c
}

func gitea() *enginetest.Container {
	return &enginetest.Container{
		ID: strings.Repeat("a1", 32), Name: "gitea", Image: "gitea/gitea:1", Running: true,
		Labels: map[string]string{LabelProject: "gitea", LabelService: "gitea", LabelWorkingDir: "/opt/gitea"},
		Logs:   []Line{{Text: "\x1b[32mserver started\x1b[0m"}, {Stderr: true, Text: "warning: slow disk"}},
		Follow: []Line{{Text: "GET / 200"}},
	}
}

type Line = enginetest.Line

func TestContainersAndInspect(t *testing.T) {
	_, c := fake(t, gitea())
	ctx := context.Background()
	rows, err := c.Containers(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	r := rows[0]
	if r.Name != "gitea" || r.Project != "gitea" || r.State != "running" || len(r.Ports) != 1 || r.Ports[0] != "3000->3000/tcp" || r.ShortID() != "a1a1a1a1a1a1" {
		t.Errorf("row %+v", r)
	}
	d, err := c.Inspect(ctx, "gitea")
	if err != nil || !d.Running || d.Tty || !strings.Contains(d.Raw, "\n  \"Id\"") {
		t.Errorf("inspect %+v %v", d, err)
	}
}

// Logs with Tty false arrive multiplexed; every line must come out clean,
// even when a frame boundary falls in the middle of it.
func TestLogsDemux(t *testing.T) {
	_, c := fake(t, gitea())
	var got []string
	err := c.Logs(context.Background(), "gitea", false, LogOptions{Tail: "100"}, func(stderr bool, line string) {
		p := "out "
		if stderr {
			p = "err "
		}
		got = append(got, p+line)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"out \x1b[32mserver started\x1b[0m", "err warning: slow disk"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q", got)
	}
}

func TestLogsFollowStopsWithContext(t *testing.T) {
	_, c := fake(t, gitea())
	ctx, cancel := context.WithCancel(context.Background())
	lines := make(chan string, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.Logs(ctx, "gitea", false, LogOptions{Tail: "all", Follow: true}, func(_ bool, l string) { lines <- l })
	}()
	for _, want := range []string{"\x1b[32mserver started\x1b[0m", "warning: slow disk", "GET / 200"} {
		select {
		case l := <-lines:
			if l != want {
				t.Fatalf("got %q want %q", l, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for", want)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("follow ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not stop when the context ended")
	}
}

func TestTTYLogsAreRaw(t *testing.T) {
	g := gitea()
	g.Tty = true
	_, c := fake(t, g)
	var got []string
	c.Logs(context.Background(), "gitea", true, LogOptions{}, func(_ bool, l string) { got = append(got, l) })
	if len(got) != 2 || got[1] != "warning: slow disk" {
		t.Errorf("got %q", got)
	}
}

func TestLifecycleCalls(t *testing.T) {
	fe, c := fake(t, gitea())
	ctx := context.Background()
	for _, f := range []func(context.Context, string) error{c.Stop, c.Start, c.Restart, c.Kill, c.Remove} {
		if err := f(ctx, "gitea"); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(fe.Calls(), ",")
	if got != "POST stop gitea,POST start gitea,POST restart gitea,POST kill gitea,DELETE remove gitea" {
		t.Errorf("calls %s", got)
	}
}

func TestStats(t *testing.T) {
	_, c := fake(t, gitea())
	s, err := c.Stats(context.Background(), "gitea")
	if err != nil {
		t.Fatal(err)
	}
	// cpu delta 2e6 over system delta 1e7 on 4 CPUs = 80%.
	if s.CPUPercent < 79.9 || s.CPUPercent > 80.1 || s.MemUsage != 200<<20 || s.RxBytes != 1000 || s.Pids != 7 {
		t.Errorf("stats %+v", s)
	}
	if z := StatsFrom(container.StatsResponse{}); z.CPUPercent != 0 {
		t.Errorf("empty sample gave %v", z)
	}
}

// The phase 3 shell check at the engine level: open, resize, type, exit.
func TestShell(t *testing.T) {
	fe, c := fake(t, gitea())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	x, err := c.Shell(ctx, "gitea", nil, "git", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.Resize(ctx, 132, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Conn.Write([]byte("whoami\rexit\r")); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(x.Reader)
	s := string(out)
	if !strings.Contains(s, `fake shell as "git": /bin/sh -c if [ -x /bin/bash ]`) || !strings.Contains(s, "you typed: whoami") {
		t.Errorf("output %q", s)
	}
	x.Close()
	if code, err := x.Wait(ctx); err != nil || code != 0 {
		t.Errorf("exit %d %v", code, err)
	}
	if r := fe.Resizes(); len(r) != 1 || r[0] != "132x40" {
		t.Errorf("resizes %v", r)
	}
}

func TestShellWithoutShell(t *testing.T) {
	g := gitea()
	g.NoShell = true
	_, c := fake(t, g)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	x, err := c.Shell(ctx, "gitea", nil, "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(x.Reader)
	if _, err := x.Wait(ctx); err != ErrNoShell {
		t.Errorf("distroless exec: %v, want ErrNoShell", err)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for in, want := range map[string]string{"": "", "10m": "1799999400", "2026-10-04T00:00:00Z": "1791072000", "1700000000": "1700000000"} {
		got, err := ParseSince(in, now)
		if err != nil || got != want {
			t.Errorf("ParseSince(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := ParseSince("yesterday", now); err == nil {
		t.Error("accepted yesterday")
	}
}

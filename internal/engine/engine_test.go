package engine

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"/var/run/docker.sock":               "unix:///var/run/docker.sock",
		" unix:///run/podman/podman.sock":    "unix:///run/podman/podman.sock",
		"/run//podman/../podman/podman.sock": "unix:///run/podman/podman.sock",
	}
	for in, want := range cases {
		got, _, err := NormalizeHost(in)
		if err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "tcp://10.0.0.2:2375", "docker.sock", "unix://relative.sock", "ssh://me@host"} {
		if _, _, err := NormalizeHost(bad); err == nil {
			t.Errorf("NormalizeHost(%q) accepted", bad)
		}
	}
}

func TestKindFromComponents(t *testing.T) {
	if k := KindFromComponents([]string{"Engine", "containerd", "runc", "docker-init"}); k != KindDocker {
		t.Errorf("docker components gave %s", k)
	}
	if k := KindFromComponents([]string{"Podman Engine", "Conmon", "OCI Runtime (crun)"}); k != KindPodman {
		t.Errorf("podman components gave %s", k)
	}
}

func TestMissingSocket(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "nope.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Identify(context.Background()); !errors.Is(err, ErrSocketMissing) {
		t.Errorf("Identify on a missing socket = %v, want ErrSocketMissing", err)
	}
}

func TestNotASocket(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir) // a folder, not a socket
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Identify(context.Background()); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("Identify on a folder = %v", err)
	}
}

func TestDeadSocket(t *testing.T) {
	// A real socket that answers nothing useful: Identify must fail, not hang.
	path := filepath.Join(t.TempDir(), "dead.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	defer l.Close()
	c, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Identify(context.Background()); err == nil {
		t.Error("Identify succeeded against a socket that hangs up")
	}
}

func TestIDFromMountinfo(t *testing.T) {
	id := strings.Repeat("ab12", 16)
	docker := "1234 1200 8:1 /var/lib/docker/containers/" + id + "/hostname /etc/hostname rw,relatime - ext4 /dev/sda1 rw\n"
	if got := idFromMountinfo(strings.NewReader("junk\n" + docker)); got != id {
		t.Errorf("docker mountinfo gave %q", got)
	}
	podman := "99 80 0:44 /containers/storage/overlay-containers/" + id + "/userdata/hosts /etc/hosts rw - tmpfs tmpfs rw\n"
	if got := idFromMountinfo(strings.NewReader(podman)); got != id {
		t.Errorf("podman mountinfo gave %q", got)
	}
	if got := idFromMountinfo(strings.NewReader("1 2 3 / / rw - ext4 /dev/sda rw\n")); got != "" {
		t.Errorf("host mountinfo gave %q", got)
	}
}

func TestIDFromContainerenv(t *testing.T) {
	s := "engine=\"podman-5.6.0\"\nname=\"fleetling\"\nid=\"deadbeef\"\nimage=\"localhost/fleetling\"\n"
	if got := idFromContainerenv(s); got != "deadbeef" {
		t.Errorf("got %q", got)
	}
}

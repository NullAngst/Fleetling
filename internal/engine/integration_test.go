//go:build integration

package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Runs against a real engine. CI starts a labeled container first:
//
//	docker run -d --label com.docker.compose.project=fltest \
//	  --label com.docker.compose.service=web alpine sleep 300
//	FLEETLING_IT_HOST=/var/run/docker.sock go test -tags integration ./internal/engine/
func TestIntegrationEngine(t *testing.T) {
	host := os.Getenv("FLEETLING_IT_HOST")
	if host == "" {
		t.Skip("FLEETLING_IT_HOST not set")
	}
	want := os.Getenv("FLEETLING_IT_KIND")
	if want == "" {
		want = KindDocker
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := New(host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	info, err := c.Identify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != want {
		t.Errorf("identified %s, want %s (%+v)", info.Kind, want, info)
	}
	t.Logf("engine: %+v", info)

	containers, err := c.ComposeContainers(ctx, "docker")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ct := range containers {
		if ct.Project == "fltest" && ct.Service == "web" && ct.Running {
			found = true
		}
	}
	if !found {
		t.Errorf("labeled test container not in %+v", containers)
	}
}

// The phase 5 done-when on a real engine: a macvlan network made through
// Fleetling inspects the same as one made with `docker network create`.
func TestIntegrationMacvlanMatchesCLI(t *testing.T) {
	host := os.Getenv("FLEETLING_IT_HOST")
	if host == "" {
		t.Skip("FLEETLING_IT_HOST not set")
	}
	parent := os.Getenv("FLEETLING_IT_PARENT")
	if parent == "" {
		parent = "eth0"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := New(host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	spec := NetworkSpec{Name: "fl-it-lan", Driver: "macvlan", Subnet: "10.231.1.0/24", Gateway: "10.231.1.1", IPRange: "10.231.1.192/27", Parent: parent}
	type shape struct {
		Driver     string
		EnableIPv4 bool
		EnableIPv6 bool
		Internal   bool
		Attachable bool
		IPAM       any
		Options    map[string]string
	}
	inspect := func() shape {
		t.Helper()
		n, err := c.InspectNetwork(ctx, spec.Name)
		if err != nil {
			t.Fatal(err)
		}
		return shape{n.Driver, n.EnableIPv4, n.EnableIPv6, n.Internal, n.Attachable, n.IPAM, n.Options}
	}

	if _, _, err := c.CreateNetwork(ctx, spec); err != nil {
		t.Fatal(err)
	}
	ui := inspect()
	if err := c.RemoveNetwork(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}

	cli := spec.CLI()
	cmd := exec.CommandContext(ctx, cli[0], cli[1:]...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+host)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	defer c.RemoveNetwork(context.Background(), spec.Name)
	want := inspect()

	uj, _ := json.Marshal(ui)
	wj, _ := json.Marshal(want)
	if string(uj) != string(wj) {
		t.Errorf("UI network differs from the CLI's:\n  ui %s\n cli %s", uj, wj)
	}
}

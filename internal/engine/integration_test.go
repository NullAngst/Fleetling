//go:build integration

package engine

import (
	"context"
	"os"
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

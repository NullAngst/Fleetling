package registry

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// push puts a random image at ref on the test registry and returns its digest.
func push(t *testing.T, ref string) string {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	return d.String()
}

func TestDigestAndCompare(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	ref := host + "/app:latest"
	old := push(t, ref)
	now := time.Now()
	c := NewChecker()
	c.Now = func() time.Time { return now }

	d, err := c.Digest(context.Background(), ref)
	if err != nil || d != old {
		t.Fatalf("digest: %s %v", d, err)
	}
	if Compare(ref, []string{host + "/app@" + old}, d) != StatusCurrent {
		t.Error("same digest not current")
	}
	// The tag moves. The cache still answers the old digest for 6 hours.
	newer := push(t, ref)
	if d, _ := c.Digest(context.Background(), ref); d != old {
		t.Error("cache not used")
	}
	now = now.Add(TTL + time.Minute)
	d, _ = c.Digest(context.Background(), ref)
	if d != newer {
		t.Fatalf("after the TTL: %s", d)
	}
	if Compare(ref, []string{host + "/app@" + old}, d) != StatusAvailable {
		t.Error("old local image not flagged")
	}
	if Compare(ref, nil, d) != StatusLocal || Compare(host+"/app@"+old, []string{host + "/app@" + old}, d) != StatusLocal {
		t.Error("local build or digest pin not treated as local")
	}
	// A digest under another repository name doesn't count.
	if Compare(ref, []string{host + "/other@" + d}, d) != StatusAvailable {
		t.Error("matched another repository's digest")
	}
	// Docker Hub short names compare against their full name.
	if Compare("nginx:latest", []string{"nginx@sha256:aa"}, "sha256:aa") != StatusCurrent {
		t.Error("short name not normalized")
	}
	if _, ok := c.Cached(ref); !ok {
		t.Error("fresh result not cached")
	}
	if _, err := c.Digest(context.Background(), host+"/missing:latest"); err == nil {
		t.Error("missing repo gave a digest")
	}
}

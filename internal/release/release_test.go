package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func fakeGitHub(t *testing.T, releases bool) (*Checker, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("User-Agent") == "" {
			t.Error("no User-Agent")
		}
		switch r.URL.Path {
		case "/raw/" + Repo + "/main/VERSION":
			w.Write([]byte("1.3.0\n"))
		case "/api/repos/" + Repo + "/releases/latest":
			if !releases {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(`{"tag_name":"v1.2.0","html_url":"https://github.com/x/releases/v1.2.0"}`))
		case "/api/repos/" + Repo + "/commits/main":
			w.Write([]byte(`{"sha":"0123456789abcdef0123","html_url":"https://github.com/x/commit/0123","commit":{"committer":{"date":"2026-11-19T10:00:00Z"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := New("Fleetling-test")
	c.API, c.Raw = srv.URL+"/api", srv.URL+"/raw"
	return c, &hits
}

func TestCheckReadsAllThree(t *testing.T) {
	c, hits := fakeGitHub(t, true)
	now := time.Unix(2_000_000_000, 0)
	st := c.Check(context.Background(), false, now)
	if len(st.Errs) > 0 {
		t.Fatal(st.Errs)
	}
	if st.MainVersion != "1.3.0" || st.Release != "v1.2.0" || st.MainCommit != "0123456789ab" || st.MainDate.IsZero() {
		t.Fatalf("%+v", st)
	}
	// Cached for TTL.
	c.Check(context.Background(), false, now.Add(TTL-time.Minute))
	if hits.Load() != 3 {
		t.Fatalf("%d requests; the cached result should have been reused", hits.Load())
	}
	c.Check(context.Background(), false, now.Add(TTL+time.Minute))
	if hits.Load() != 6 {
		t.Fatalf("%d requests after the TTL, want 6", hits.Load())
	}
}

// No release yet is a normal state, not an error.
func TestNoReleases(t *testing.T) {
	c, _ := fakeGitHub(t, false)
	st := c.Check(context.Background(), true, time.Now())
	if len(st.Errs) > 0 || st.Release != "" || st.MainVersion != "1.3.0" {
		t.Fatalf("%+v", st)
	}
}

func TestUnreachable(t *testing.T) {
	c := New("Fleetling-test")
	c.API, c.Raw = "http://127.0.0.1:1", "http://127.0.0.1:1"
	st := c.Check(context.Background(), true, time.Now())
	if len(st.Errs) != 3 {
		t.Fatalf("errors %v, want one per request", st.Errs)
	}
	if _, ok := c.Cached(); !ok {
		t.Fatal("a failed check should still be cached, so the page doesn't retry on every load")
	}
}

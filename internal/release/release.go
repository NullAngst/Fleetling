// Package release asks GitHub what the newest Fleetling is: the VERSION
// file on main, the newest release, and the newest commit on main. Three
// small anonymous requests, at most once per TTL, cached in memory.
//
// Why all three? The stock compose.yaml builds Fleetling straight from
// main, so the version on main is what an update brings. But the weekly
// dependency updates land on main without a version bump, so the newest
// commit's date, compared with when the running image was built, is what
// says those are waiting. The release is shown for anyone who builds from
// a tag instead.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"
)

// Repo is where Fleetling lives on GitHub.
const Repo = "NullAngst/Fleetling"

// TTL is how long one check is reused.
const TTL = 6 * time.Hour

// Status is one check's result. Each part can fail on its own; Errs says
// which, and the rest is still filled in.
type Status struct {
	Checked     time.Time
	MainVersion string    // VERSION on main
	Release     string    // newest release's tag, "" when there is none
	ReleaseURL  string    // its page
	MainCommit  string    // newest commit on main, short
	MainDate    time.Time // and when it was committed
	MainURL     string    // its page
	ReleaseOK   bool      // the release request worked; Release "" then means there are none
	Errs        []string
}

// Checker does the asking. The zero value isn't usable; use New.
type Checker struct {
	API, Raw string // base URLs, overridable for tests
	Client   *http.Client
	UA       string // User-Agent, which GitHub's API requires

	mu   sync.Mutex
	last Status
	busy chan struct{} // one check at a time
}

// New makes a checker for github.com.
func New(userAgent string) *Checker {
	return &Checker{
		API:    "https://api.github.com",
		Raw:    "https://raw.githubusercontent.com",
		Client: &http.Client{Timeout: 15 * time.Second},
		UA:     userAgent,
		busy:   make(chan struct{}, 1),
	}
}

// Cached returns the last result, and false when there is none yet.
func (c *Checker) Cached() (Status, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last, !c.last.Checked.IsZero()
}

// Check returns the cached result while it is fresh, and asks GitHub when
// it isn't or force is set.
func (c *Checker) Check(ctx context.Context, force bool, now time.Time) Status {
	if st, ok := c.Cached(); ok && !force && now.Sub(st.Checked) < TTL {
		return st
	}
	select {
	case c.busy <- struct{}{}:
		defer func() { <-c.busy }()
	case <-ctx.Done():
		st, _ := c.Cached()
		return st
	}
	// Someone else may have just finished one.
	if st, ok := c.Cached(); ok && now.Sub(st.Checked) < time.Minute {
		return st
	}
	st := Status{Checked: now}
	if v, err := c.text(ctx, c.Raw+"/"+Repo+"/main/VERSION"); err != nil {
		st.Errs = append(st.Errs, "VERSION on main: "+err.Error())
	} else {
		st.MainVersion = strings.TrimSpace(v)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	switch err := c.json(ctx, c.API+"/repos/"+Repo+"/releases/latest", &rel); {
	case errors.Is(err, errNotFound): // no releases yet
		st.ReleaseOK = true
	case err != nil:
		st.Errs = append(st.Errs, "latest release: "+err.Error())
	default:
		st.Release, st.ReleaseURL, st.ReleaseOK = rel.TagName, rel.HTMLURL, true
	}
	var commit struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
		Commit  struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := c.json(ctx, c.API+"/repos/"+Repo+"/commits/main", &commit); err != nil {
		st.Errs = append(st.Errs, "newest commit on main: "+err.Error())
	} else {
		st.MainCommit, st.MainURL, st.MainDate = short(commit.SHA), commit.HTMLURL, commit.Commit.Committer.Date
	}
	c.mu.Lock()
	c.last = st
	c.mu.Unlock()
	return st
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

var errNotFound = errors.New("not found")

func (c *Checker) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UA)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.Client.Do(req)
	if err != nil {
		var ue *neturl.Error
		if errors.As(err, &ue) {
			return nil, ue.Err // drop the "Get <url>:" in front
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("GitHub said %s; its API allows 60 anonymous requests an hour per address", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub said %s", resp.Status)
	}
	return body, nil
}

func (c *Checker) text(ctx context.Context, url string) (string, error) {
	b, err := c.get(ctx, url)
	return string(b), err
}

func (c *Checker) json(ctx context.Context, url string, v any) error {
	b, err := c.get(ctx, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

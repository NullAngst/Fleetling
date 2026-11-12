// Package registry asks image registries for the current digest of a tag,
// so Fleetling can tell when a stack's image has an update. It uses
// github.com/google/go-containerregistry for the request and the token
// handshake. Anonymous only: private registries are out of scope.
//
// Results are cached for six hours, so opening a stack page doesn't hit
// Docker Hub's pull-rate limit. A HEAD request for a manifest doesn't
// count against that limit anyway.
package registry

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// TTL is how long a remote digest is trusted.
const TTL = 6 * time.Hour

type entry struct {
	digest string
	err    error
	at     time.Time
}

// Checker caches remote digests by reference.
type Checker struct {
	Transport http.RoundTripper // nil uses the default, which honors HTTPS_PROXY
	Now       func() time.Time

	mu    sync.Mutex
	cache map[string]entry
}

// NewChecker returns a checker with an empty cache.
func NewChecker() *Checker { return &Checker{cache: map[string]entry{}} }

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Digest returns the registry's manifest digest for ref, from the cache
// when it is fresh. Errors are cached too, so a registry that is down
// isn't asked again on every page load.
func (c *Checker) Digest(ctx context.Context, ref string) (string, error) {
	c.mu.Lock()
	if e, ok := c.cache[ref]; ok && c.now().Sub(e.at) < TTL {
		c.mu.Unlock()
		return e.digest, e.err
	}
	c.mu.Unlock()

	digest, err := c.head(ctx, ref)
	c.mu.Lock()
	c.cache[ref] = entry{digest: digest, err: err, at: c.now()}
	c.mu.Unlock()
	return digest, err
}

// Cached returns a cached result without asking the registry. ok is false
// when there is nothing fresh.
func (c *Checker) Cached(ref string) (digest string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.cache[ref]
	if !found || e.err != nil || c.now().Sub(e.at) >= TTL {
		return "", false
	}
	return e.digest, true
}

func (c *Checker) head(ctx context.Context, ref string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuth(authn.Anonymous)}
	if c.Transport != nil {
		opts = append(opts, remote.WithTransport(c.Transport))
	}
	desc, err := remote.Head(r, opts...)
	if err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// Status is the update state of one image.
type Status string

const (
	StatusCurrent   Status = "current"   // the local image is what the tag points at
	StatusAvailable Status = "available" // the tag points at something newer
	StatusLocal     Status = "local"     // built here or pinned by digest; nothing to compare
	StatusUnknown   Status = "unknown"   // the registry didn't answer
)

// Compare checks a local image's RepoDigests against the remote digest of
// ref. RepoDigests hold "repo@sha256:..." for every digest the image was
// pulled as; Docker records the index digest for multi-arch tags, which
// is what a HEAD on the tag returns.
func Compare(ref string, repoDigests []string, remoteDigest string) Status {
	r, err := name.ParseReference(ref)
	if err != nil {
		return StatusUnknown
	}
	if _, ok := r.(name.Digest); ok {
		return StatusLocal // pinned by digest: it never moves
	}
	if len(repoDigests) == 0 {
		return StatusLocal
	}
	repo := r.Context().Name()
	for _, rd := range repoDigests {
		n, d, ok := strings.Cut(rd, "@")
		if !ok {
			continue
		}
		local, err := name.NewRepository(n)
		if err != nil || local.Name() != repo {
			continue
		}
		if d == remoteDigest {
			return StatusCurrent
		}
	}
	return StatusAvailable
}

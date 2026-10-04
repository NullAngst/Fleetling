// Package engine talks to the Docker and Podman API sockets through the
// official moby Go client. Podman is reached through its Docker-compatible
// API, so one client type covers both.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/NullAngst/Fleetling/internal/compose"
)

// Compose labels on every container Compose creates.
const (
	LabelProject    = "com.docker.compose.project"
	LabelService    = "com.docker.compose.service"
	LabelWorkingDir = "com.docker.compose.project.working_dir"
)

// Engine kinds, as identified from /version. Not to be confused with the
// endpoint slot (compose.EngineDocker / compose.EnginePodman): podman-docker
// puts Podman behind the Docker path, so the slot named "docker" can turn
// out to be Podman.
const (
	KindDocker = "Docker"
	KindPodman = "Podman"
)

// ErrSocketMissing means the socket file does not exist, which usually just
// means that engine isn't installed or isn't mounted into the container.
var ErrSocketMissing = errors.New("socket not found")

// Info is what the endpoint test shows.
type Info struct {
	Kind       string // KindDocker or KindPodman
	Version    string
	APIVersion string
	OS         string
	Arch       string
	Platform   string
}

// Client is one engine endpoint.
type Client struct {
	Host string // normalized, e.g. unix:///var/run/docker.sock
	path string
	c    *client.Client
}

// NormalizeHost turns a settings value into a DOCKER_HOST style URL.
// Accepted: /path/to.sock and unix:///path/to.sock. TCP endpoints are refused
// on purpose: an unauthenticated engine API on the network is root for
// anyone who can reach it.
func NormalizeHost(s string) (host, path string, err error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", "", errors.New("empty endpoint")
	case strings.HasPrefix(s, "unix://"):
		path = strings.TrimPrefix(s, "unix://")
	case strings.HasPrefix(s, "/"):
		path = s
	default:
		return "", "", fmt.Errorf("%q: use a socket path like /var/run/docker.sock", s)
	}
	if !filepath.IsAbs(path) {
		return "", "", fmt.Errorf("%q: socket path must be absolute", s)
	}
	path = filepath.Clean(path)
	return "unix://" + path, path, nil
}

// New builds a client for one endpoint. It does not connect yet.
func New(endpoint string) (*Client, error) {
	host, path, err := NormalizeHost(endpoint)
	if err != nil {
		return nil, err
	}
	c, err := client.New(client.WithHost(host))
	if err != nil {
		return nil, err
	}
	return &Client{Host: host, path: path, c: c}, nil
}

// Close releases idle connections.
func (e *Client) Close() error { return e.c.Close() }

// checkSocket gives a clear error before the client produces a long dial error.
func (e *Client) checkSocket() error {
	fi, err := os.Stat(e.path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", e.path, ErrSocketMissing)
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists but is not a socket", e.path)
	}
	return nil
}

// Identify calls /_ping and /version and works out which engine answered.
func (e *Client) Identify(ctx context.Context) (Info, error) {
	if err := e.checkSocket(); err != nil {
		return Info{}, err
	}
	if _, err := e.c.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		return Info{}, fmt.Errorf("ping: %w", err)
	}
	v, err := e.c.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return Info{}, fmt.Errorf("version: %w", err)
	}
	names := make([]string, 0, len(v.Components))
	for _, c := range v.Components {
		names = append(names, c.Name)
	}
	return Info{
		Kind:       KindFromComponents(names),
		Version:    v.Version,
		APIVersion: v.APIVersion,
		OS:         v.Os,
		Arch:       v.Arch,
		Platform:   v.Platform.Name,
	}, nil
}

// KindFromComponents identifies the engine from the component names in
// /version. Podman reports a component called "Podman Engine". The socket
// path is never used for this.
func KindFromComponents(names []string) string {
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), "podman") {
			return KindPodman
		}
	}
	return KindDocker
}

// ComposeContainers lists every container, running or not, that carries a
// Compose project label. slot is stamped on each result so the caller knows
// which endpoint it came from.
func (e *Client) ComposeContainers(ctx context.Context, slot string) ([]compose.Container, error) {
	if err := e.checkSocket(); err != nil {
		return nil, err
	}
	res, err := e.c.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelProject),
	})
	if err != nil {
		return nil, err
	}
	out := make([]compose.Container, 0, len(res.Items))
	for _, c := range res.Items {
		out = append(out, compose.Container{
			Engine:     slot,
			Project:    c.Labels[LabelProject],
			Service:    c.Labels[LabelService],
			WorkingDir: c.Labels[LabelWorkingDir],
			Running:    c.State == container.StateRunning,
		})
	}
	return out, nil
}

// ContainerLabels returns the labels of one container.
func (e *Client) ContainerLabels(ctx context.Context, id string) (map[string]string, error) {
	res, err := e.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	if res.Container.Config == nil {
		return map[string]string{}, nil
	}
	return res.Container.Config.Labels, nil
}

// ContainerMounts returns the mounts of one container.
func (e *Client) ContainerMounts(ctx context.Context, id string) ([]compose.Mount, error) {
	res, err := e.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]compose.Mount, 0, len(res.Container.Mounts))
	for _, m := range res.Container.Mounts {
		out = append(out, compose.Mount{Type: string(m.Type), Source: m.Source, Destination: m.Destination})
	}
	return out, nil
}

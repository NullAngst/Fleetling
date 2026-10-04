package store

import (
	"context"
	"strings"
)

// Setting keys. Kept in one place so a typo can't create a second key.
const (
	KeyPasswordHash  = "password_hash"
	KeySessionSecret = "session_secret"
	KeyRoot          = "stack_root"
	KeyDockerHost    = "docker_host"
	KeyPodmanHost    = "podman_host"
	KeyIgnore        = "ignore_list"
)

// Defaults for the engine endpoints and the ignore list.
const (
	DefaultDockerHost = "/var/run/docker.sock"
	DefaultPodmanHost = "/run/podman/podman.sock"
	DefaultIgnore     = "containerd"
)

// Settings is the editable part of the configuration.
type Settings struct {
	Root       string
	DockerHost string // empty means the Docker endpoint is off
	PodmanHost string // empty means the Podman endpoint is off
	Ignore     []string
}

// LoadSettings reads the settings, using defaultRoot when no root has been
// saved yet (it comes from FLEETLING_ROOT).
func (s *Store) LoadSettings(ctx context.Context, defaultRoot string) (Settings, error) {
	get := func(key, def string) (string, error) {
		v, ok, err := s.Get(ctx, key)
		if err != nil || !ok {
			return def, err
		}
		return v, nil
	}
	var st Settings
	var err error
	if st.Root, err = get(KeyRoot, defaultRoot); err != nil {
		return st, err
	}
	if st.DockerHost, err = get(KeyDockerHost, DefaultDockerHost); err != nil {
		return st, err
	}
	if st.PodmanHost, err = get(KeyPodmanHost, DefaultPodmanHost); err != nil {
		return st, err
	}
	ign, err := get(KeyIgnore, DefaultIgnore)
	if err != nil {
		return st, err
	}
	st.Ignore = ParseList(ign)
	return st, nil
}

// SaveSettings writes every editable setting in one transaction.
func (s *Store) SaveSettings(ctx context.Context, st Settings) error {
	return s.SetMany(ctx, map[string]string{
		KeyRoot:       st.Root,
		KeyDockerHost: st.DockerHost,
		KeyPodmanHost: st.PodmanHost,
		KeyIgnore:     strings.Join(st.Ignore, "\n"),
	})
}

// ParseList splits a newline or comma separated list, trimming blanks and
// dropping duplicates while keeping the original order.
func ParseList(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' || r == '\r' }) {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

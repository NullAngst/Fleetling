package compose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner builds and runs `docker compose` commands. Every stack action goes
// through the real Compose binary; Compose's Go packages are not a stable
// library API, the CLI is.
type Runner struct {
	Docker string // path to the docker binary, "docker" finds it on PATH
}

// Target is everything needed to point Compose at one stack.
type Target struct {
	Project string // passed with -p, never written into the compose file
	Dir     string // /opt/<folder>
	File    string // compose file name inside Dir
	Host    string // engine socket, e.g. unix:///var/run/docker.sock
	Podman  bool   // the engine answering on Host is Podman
}

// Args returns the full argv for `docker compose ... <cmd>`, starting with
// "docker". This is also exactly what the UI shows before an action runs.
func (r Runner) Args(t Target, cmd ...string) []string {
	args := []string{"docker", "compose",
		"-p", t.Project,
		"--project-directory", t.Dir,
		"-f", filepath.Join(t.Dir, t.File),
		"--ansi", "never",
		"--progress", "plain",
	}
	return append(args, cmd...)
}

// Env is the environment for a compose subprocess: the server's own
// environment minus anything that would make Compose ignore our flags, plus
// DOCKER_HOST for the stack's engine.
func (r Runner) Env(t Target) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "DOCKER_HOST", k == "DOCKER_CONTEXT", k == "DOCKER_BUILDKIT",
			strings.HasPrefix(k, "COMPOSE_"):
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "DOCKER_HOST="+t.Host)
	if t.Podman {
		// Podman's compat API does not speak BuildKit's session protocol.
		env = append(env, "DOCKER_BUILDKIT=0")
	}
	return env
}

// Command builds an exec.Cmd. Arguments are passed separately, never
// through a shell.
func (r Runner) Command(ctx context.Context, t Target, cmd ...string) *exec.Cmd {
	args := r.Args(t, cmd...)
	bin := r.Docker
	if bin == "" {
		bin = "docker"
	}
	c := exec.CommandContext(ctx, bin, args[1:]...)
	c.Env = r.Env(t)
	c.Dir = t.Dir
	return c
}

// Plain builds a docker command that is not tied to a stack, like
// `docker pull`, against the given engine host.
func (r Runner) Plain(ctx context.Context, host string, podman bool, args ...string) *exec.Cmd {
	bin := r.Docker
	if bin == "" {
		bin = "docker"
	}
	c := exec.CommandContext(ctx, bin, args...)
	c.Env = r.Env(Target{Host: host, Podman: podman})
	return c
}

// Validate runs `docker compose config -q` against compose text that is
// not on disk yet. The compose file goes in on stdin and the env text in a
// temporary file outside the stack folder, so nothing in the folder changes.
func (r Runner) Validate(ctx context.Context, t Target, composeText, envText string) error {
	envFile, err := os.CreateTemp("", "fleetling-env-*")
	if err != nil {
		return err
	}
	defer os.Remove(envFile.Name())
	if _, err := envFile.WriteString(envText); err != nil {
		envFile.Close()
		return err
	}
	envFile.Close()

	dir := t.Dir
	if _, err := os.Stat(dir); err != nil {
		// A new stack's folder doesn't exist yet. Its parent stands in, so
		// Compose has a real working directory.
		dir = filepath.Dir(dir)
	}
	bin := r.Docker
	if bin == "" {
		bin = "docker"
	}
	c := exec.CommandContext(ctx, bin, "compose",
		"-p", t.Project, "--project-directory", t.Dir, "-f", "-",
		"--env-file", envFile.Name(), "--ansi", "never", "config", "-q")
	c.Env = r.Env(t)
	c.Dir = dir
	c.Stdin = strings.NewReader(composeText)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		var ee *exec.ExitError
		if errors.As(err, &ee) && msg != "" {
			return errors.New(msg)
		}
		return fmt.Errorf("docker compose config: %w %s", err, msg)
	}
	return nil
}

// ResolvedConfig runs `docker compose config --format json` for a stack on
// disk and returns the raw JSON.
func (r Runner) ResolvedConfig(ctx context.Context, t Target) ([]byte, error) {
	c := r.Command(ctx, t, "config", "--format", "json")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("docker compose config: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Quote renders argv as one shell-style line for display and the action log.
func Quote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`*?[]{}()<>|&;#~") {
			parts[i] = a
			continue
		}
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}

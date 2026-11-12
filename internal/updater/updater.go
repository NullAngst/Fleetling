// Package updater updates or restarts Fleetling's own stack from outside
// it. Fleetling can't run `docker compose up -d` on its own project from
// inside its own container: Compose replaces the container halfway through
// and the process doing the work dies with it. So the server starts this
// code as a short-lived helper container ("fleetling updater") from its own
// image, with the engine socket and the stack root mounted at the same
// paths, and the helper does the update while the server restarts.
//
// Update: record each service's image ID, tag it <name>:fleetling-previous,
// build or pull, up -d, then wait for every container to be healthy. If
// that doesn't happen in time, put the old image IDs back under their
// names and up -d again without building. Every step goes to
// last-update.log.
//
// Why the extra tag: with Docker's containerd image store (the default
// since Docker 29), an image whose name moves to a new build is deleted as
// soon as no container uses it, which is exactly what up -d does to the
// old one. The fleetling-previous tag keeps it, so the rollback has
// something to put back. It stays after a good update too, as the manual
// rollback point maintenance.md describes.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
)

// HelperName is the helper container's name. While a container by this
// name exists, the server refuses to start another one.
const HelperName = "fleetling-updater"

// LogFile is the name of the log in Fleetling's data folder.
const LogFile = "last-update.log"

// Modes.
const (
	ModeUpdate  = "update"
	ModeRestart = "restart"
)

// Engine is the read side the helper needs. engine.Client implements it.
type Engine interface {
	ProjectContainers(ctx context.Context, project string) ([]engine.ProjectContainer, error)
	ImageInfo(ctx context.Context, ref string) (engine.ImageInfo, error)
}

// Config is one helper run.
type Config struct {
	Root    string // stack root, e.g. /opt
	Dir     string // the stack folder, directly under Root
	LogPath string // <data>/last-update.log
	Mode    string // ModeUpdate or ModeRestart
	Host    string // DOCKER_HOST for compose
	Runner  compose.Runner
	Engine  Engine
	Timeout time.Duration // how long to wait for healthy, 60s by default
	Poll    time.Duration // how often to check, 2s by default
	Out     io.Writer     // a copy of the log, e.g. stderr for `docker logs`
}

// ValidateDir checks that dir is a real folder directly under root. The
// helper runs with the engine socket, so it must not be pointed anywhere
// else.
func ValidateDir(root, dir string) (string, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(dir) {
		return "", errors.New("--dir and the root must be absolute paths")
	}
	root, dir = filepath.Clean(root), filepath.Clean(dir)
	if filepath.Dir(dir) != root || !compose.ValidFolderName(filepath.Base(dir)) {
		return "", fmt.Errorf("--dir %s must be a folder directly under the root %s", dir, root)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a folder (symlinks are refused)", dir)
	}
	return dir, nil
}

// service is what the helper needs from one service in the resolved config.
type service struct {
	Name  string
	Image string // the name the image is tagged as
	Build bool
}

// services reads `docker compose config --format json`. A service with
// build: and no image: is tagged <project>-<service> by Compose.
func services(configJSON []byte, project string) ([]service, error) {
	var cfg struct {
		Services map[string]struct {
			Image string          `json:"image"`
			Build json.RawMessage `json:"build"`
		} `json:"services"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parse compose config: %w", err)
	}
	var out []service
	for name, s := range cfg.Services {
		sv := service{Name: name, Image: s.Image, Build: len(s.Build) > 0 && string(s.Build) != "null"}
		if sv.Image == "" {
			sv.Image = project + "-" + name
		}
		out = append(out, sv)
	}
	slices.SortFunc(out, func(a, b service) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// logger writes timestamped lines to the log file and Out.
type logger struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *logger) printf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
}

// Run does one update or restart. A returned error means the stack did not
// end up healthy on the new images; the log says what happened.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Poll == 0 {
		cfg.Poll = 2 * time.Second
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var w io.Writer = f
	if cfg.Out != nil {
		w = io.MultiWriter(f, cfg.Out)
	}
	log := &logger{w: w}
	err = run(ctx, cfg, log)
	if err != nil {
		log.printf("FAILED: %v", err)
	} else {
		log.printf("done")
	}
	return err
}

func run(ctx context.Context, cfg Config, log *logger) error {
	log.printf("fleetling updater: %s %s", cfg.Mode, cfg.Dir)
	if cfg.Mode != ModeUpdate && cfg.Mode != ModeRestart {
		return fmt.Errorf("unknown mode %q", cfg.Mode)
	}
	dir, err := ValidateDir(cfg.Root, cfg.Dir)
	if err != nil {
		return err
	}
	f, ok := compose.ReadFolder(dir)
	if !ok || f.Meta == nil || f.ComposeFile == "" {
		return fmt.Errorf("%s is not a managed stack folder with a compose file", dir)
	}
	t := compose.Target{
		Project: f.Meta.Project, Dir: f.Dir, File: f.ComposeFile, Host: cfg.Host,
		Podman: f.Meta.Engine == compose.EnginePodman,
	}
	cj, err := cfg.Runner.ResolvedConfig(ctx, t)
	if err != nil {
		return err
	}
	svcs, err := services(cj, t.Project)
	if err != nil {
		return err
	}

	// Record what runs now, so a failed update can be put back.
	old := map[string]string{}
	ctrs, err := cfg.Engine.ProjectContainers(ctx, t.Project)
	if err != nil {
		return fmt.Errorf("list the project's containers: %w", err)
	}
	for _, c := range ctrs {
		if old[c.Service] == "" && c.ImageID != "" {
			old[c.Service] = c.ImageID
		}
	}
	for _, s := range svcs {
		if old[s.Name] == "" {
			if info, err := cfg.Engine.ImageInfo(ctx, s.Image); err == nil {
				old[s.Name] = info.ID
			}
		}
		log.printf("service %s: image %s, current image ID %s", s.Name, s.Image, orNone(old[s.Name]))
	}

	if cfg.Mode == ModeRestart {
		if err := step(ctx, log, cfg.Runner.Command(ctx, t, "restart")); err != nil {
			return err
		}
		ok, state := waitHealthy(ctx, cfg, t.Project, svcs, log)
		if !ok {
			return fmt.Errorf("not healthy %s after the restart: %s", cfg.Timeout, state)
		}
		return nil
	}

	// Keep the current images alive under a second name; see the package
	// comment. No rollback point means no update.
	for _, s := range svcs {
		if old[s.Name] == "" {
			continue
		}
		if err := step(ctx, log, cfg.Runner.Plain(ctx, t.Host, t.Podman, "tag", old[s.Name], PreviousTag(s.Image))); err != nil {
			return fmt.Errorf("could not keep the current image as a rollback point, so nothing was changed: %w", err)
		}
	}

	hasBuild := slices.ContainsFunc(svcs, func(s service) bool { return s.Build })
	hasImage := slices.ContainsFunc(svcs, func(s service) bool { return !s.Build })
	if hasBuild {
		if err := step(ctx, log, cfg.Runner.Command(ctx, t, "build", "--pull")); err != nil {
			return err
		}
	}
	if hasImage {
		if err := step(ctx, log, cfg.Runner.Command(ctx, t, "pull", "--ignore-buildable")); err != nil {
			return err
		}
	}
	upErr := step(ctx, log, cfg.Runner.Command(ctx, t, "up", "-d"))
	if upErr == nil {
		ok, state := waitHealthy(ctx, cfg, t.Project, svcs, log)
		if ok {
			return nil
		}
		upErr = fmt.Errorf("not healthy %s after up: %s", cfg.Timeout, state)
	}

	// Roll back: the old image IDs go back under their names, then up -d
	// without building, which recreates the containers from them.
	log.printf("rolling back: %v", upErr)
	for _, s := range svcs {
		id := old[s.Name]
		if id == "" {
			log.printf("service %s: no recorded image ID, nothing to put back", s.Name)
			continue
		}
		if err := step(ctx, log, cfg.Runner.Plain(ctx, t.Host, t.Podman, "tag", id, s.Image)); err != nil {
			return fmt.Errorf("rollback failed at re-tag: %w (after %v)", err, upErr)
		}
	}
	if err := step(ctx, log, cfg.Runner.Command(ctx, t, "up", "-d", "--no-build")); err != nil {
		return fmt.Errorf("rollback failed at up: %w (after %v)", err, upErr)
	}
	if ok, state := waitHealthy(ctx, cfg, t.Project, svcs, log); !ok {
		return fmt.Errorf("rolled back, but the old images are not healthy either: %s (after %v)", state, upErr)
	}
	return fmt.Errorf("rolled back to the previous images: %v", upErr)
}

// PreviousTag is the name the current image is kept under during an
// update: the same repository, tag fleetling-previous. A reference with a
// digest or an unparseable one falls back to a fixed local name.
func PreviousTag(image string) string {
	n, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "fleetling-rollback:previous"
	}
	return reference.FamiliarName(n) + ":fleetling-previous"
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// step runs one command, copying its output into the log.
func step(ctx context.Context, log *logger, c *exec.Cmd) error {
	log.printf("$ %s", compose.Quote(append([]string{"docker"}, c.Args[1:]...)))
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	c.Stdout, c.Stderr = pw, pw
	if err := c.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}
	pw.Close()
	compose.ReadLines(pr, func(line string) { log.printf("  %s", line) })
	pr.Close()
	if err := c.Wait(); err != nil {
		return fmt.Errorf("%s: %w", c.Args[len(c.Args)-1], err)
	}
	return ctx.Err()
}

// waitHealthy polls until every service has a running container that is
// healthy, or running with no health check at all.
func waitHealthy(ctx context.Context, cfg Config, project string, svcs []service, log *logger) (bool, string) {
	deadline := time.Now().Add(cfg.Timeout)
	state := "no containers yet"
	for {
		ctrs, err := cfg.Engine.ProjectContainers(ctx, project)
		if err != nil {
			state = err.Error()
		} else {
			ok, s := healthy(ctrs, svcs)
			state = s
			if ok {
				log.printf("healthy: %s", state)
				return true, state
			}
		}
		if time.Now().After(deadline) {
			log.printf("still not healthy: %s", state)
			return false, state
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err().Error()
		case <-time.After(cfg.Poll):
		}
	}
}

func healthy(ctrs []engine.ProjectContainer, svcs []service) (bool, string) {
	all := true
	var parts []string
	for _, s := range svcs {
		st := "missing"
		for _, c := range ctrs {
			if c.Service != s.Name {
				continue
			}
			switch {
			case !c.Running:
				st = "not running"
			case c.Health == "" || c.Health == "healthy":
				st = "ok"
			default:
				st = c.Health
			}
			if st == "ok" {
				break
			}
		}
		if st != "ok" {
			all = false
		}
		parts = append(parts, s.Name+"="+st)
	}
	return all, strings.Join(parts, ", ")
}

package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
)

// stackAction is one button on a stack page.
type stackAction struct {
	Key    string
	Label  string
	Steps  [][]string // compose subcommands, run in order, stopping at the first failure
	Tracks bool       // an up: record bind sources Docker creates on the first deploy
	Danger bool
}

var stackActions = []stackAction{
	{Key: "deploy", Label: "Deploy", Steps: [][]string{{"up", "-d"}}, Tracks: true},
	{Key: "update", Label: "Update", Steps: [][]string{{"pull"}, {"up", "-d"}}, Tracks: true},
	{Key: "restart", Label: "Restart", Steps: [][]string{{"restart"}}},
	{Key: "recreate", Label: "Recreate", Steps: [][]string{{"up", "-d", "--force-recreate"}}, Tracks: true},
	{Key: "start", Label: "Start", Steps: [][]string{{"start"}}},
	{Key: "stop", Label: "Stop", Steps: [][]string{{"stop"}}},
	{Key: "down", Label: "Down", Steps: [][]string{{"down"}}, Danger: true},
}

// redeploy is what "Save and redeploy" runs.
var redeploy = stackAction{Key: "redeploy", Label: "Redeploy", Steps: [][]string{{"up", "-d", "--remove-orphans"}}, Tracks: true}

func findAction(key string) (stackAction, bool) {
	for _, a := range stackActions {
		if a.Key == key {
			return a, true
		}
	}
	return stackAction{}, false
}

// loadFolder reads one stack folder by name. ok is false for a bad name,
// a symlink, or a folder that isn't a stack.
func loadFolder(st store.Settings, name string) (compose.Folder, bool) {
	if !compose.ValidFolderName(name) {
		return compose.Folder{}, false
	}
	return compose.ReadFolder(filepath.Join(st.Root, name))
}

func hostFor(st store.Settings, slot string) string {
	switch slot {
	case compose.EngineDocker:
		return st.DockerHost
	case compose.EnginePodman:
		return st.PodmanHost
	}
	return ""
}

// target builds the Compose target for a managed folder.
func (s *Server) target(st store.Settings, f compose.Folder) (compose.Target, error) {
	if f.Meta == nil {
		return compose.Target{}, errors.New("this folder is not managed yet, use Manage first")
	}
	if f.ComposeFile == "" {
		return compose.Target{}, errors.New("this folder has no compose file")
	}
	ep := hostFor(st, f.Meta.Engine)
	if ep == "" {
		return compose.Target{}, fmt.Errorf("the %s endpoint is turned off in settings", f.Meta.Engine)
	}
	host, _, err := engine.NormalizeHost(ep)
	if err != nil {
		return compose.Target{}, err
	}
	return compose.Target{
		Project: f.Meta.Project, Dir: f.Dir, File: f.ComposeFile, Host: host,
		Podman: f.Meta.Engine == compose.EnginePodman,
	}, nil
}

// commandLine is the one monospace line shown under an action's confirm.
func (s *Server) commandLine(t compose.Target, a stackAction) string {
	parts := make([]string, len(a.Steps))
	for i, step := range a.Steps {
		parts[i] = compose.Quote(s.runner.Args(t, step...))
	}
	return strings.Join(parts, " && ")
}

// runAction starts a compose action as a background job and records it in
// the action log.
func (s *Server) runAction(ctx context.Context, st store.Settings, f compose.Folder, a stackAction) (*compose.Job, error) {
	t, err := s.target(st, f)
	if err != nil {
		return nil, err
	}
	if s.jobs.Running(f.Name) != nil {
		return nil, compose.ErrBusy
	}
	// Nothing runs from files that changed outside Fleetling until the
	// user has looked at them.
	if err := s.reviewedOrErr(ctx, f); err != nil {
		return nil, err
	}
	var steps []compose.Step
	var lines []string
	for _, argv := range a.Steps {
		steps = append(steps, compose.Step{Cmd: s.runner.Command(s.base, t, argv...)})
		lines = append(lines, compose.Quote(s.runner.Args(t, argv...)))
	}
	started := s.now()
	id, err := s.store.StartAction(ctx, store.Action{Started: started, Target: f.Name, Engine: f.Meta.Engine, Command: strings.Join(lines, "\n")})
	if err != nil {
		return nil, err
	}

	// Path tracking runs on every up until the first one succeeds. Docker
	// creates missing bind sources as root-owned folders, and those are
	// what "remove with folders" offers to clean up later.
	tracking := a.Tracks && f.Meta.Deployed == nil
	var missing []string
	root := st.Root
	spec := compose.JobSpec{
		Target: f.Name,
		Engine: f.Meta.Engine,
		Steps:  steps,
		Before: func(ctx context.Context, j *compose.Job) error {
			if !tracking {
				return nil
			}
			cfg, err := s.runner.ResolvedConfig(ctx, t)
			if err != nil {
				return err
			}
			srcs, err := compose.BindSources(cfg)
			if err != nil {
				return err
			}
			missing = compose.Missing(srcs)
			if len(missing) > 0 {
				j.Linef("fleetling: first deploy, watching bind sources that do not exist yet: %s", strings.Join(missing, ", "))
			}
			return nil
		},
		After: func(ctx context.Context, j *compose.Job, exit int) {
			if exit != 0 || !tracking {
				return
			}
			m, err := compose.ReadMeta(f.Dir)
			if err != nil {
				j.Linef("fleetling: could not update %s: %v", compose.MetaFile, err)
				return
			}
			created := compose.NewlyCreated(missing, root)
			m.CreatedPaths = compose.MergePaths(m.CreatedPaths, created)
			now := s.now().UTC().Truncate(time.Second)
			m.Deployed = &now
			if err := compose.WriteMeta(f.Dir, m, compose.OwnerOf(filepath.Join(f.Dir, compose.MetaFile))); err != nil {
				j.Linef("fleetling: could not update %s: %v", compose.MetaFile, err)
				return
			}
			if len(created) > 0 {
				j.Linef("fleetling: recorded created paths: %s", strings.Join(created, ", "))
			}
			// Fleetling just changed .fleetling.toml itself; that is not drift.
			if err := s.recordBaseline(context.Background(), f.Dir, "deploy"); err != nil {
				j.Linef("fleetling: could not record the stack's files: %v", err)
			}
		},
		Done: func(j *compose.Job) {
			_, exit, fin := j.Result()
			// The request that started the job is long gone; log on a
			// fresh context so the entry is written even during shutdown.
			if err := s.store.FinishAction(context.Background(), id, fin, exit, strings.Join(j.Tail(200), "\n")); err != nil {
				s.log.Error("action log", "err", err)
			}
		},
	}
	job, err := s.jobs.Start(spec)
	if err != nil {
		s.store.FinishAction(ctx, id, s.now(), -1, "fleetling: "+err.Error())
		return nil, err
	}
	s.log.Info("action", "stack", f.Name, "action", a.Key, "job", job.ID)
	return job, nil
}

// logFileAction records a change Fleetling made itself, like writing a
// compose file, in the action log next to the compose commands.
func (s *Server) logFileAction(ctx context.Context, target, engineSlot, what string, err error) {
	exit, out := 0, ""
	if err != nil {
		exit, out = 1, err.Error()
	}
	now := s.now()
	id, e := s.store.StartAction(ctx, store.Action{Started: now, Target: target, Engine: engineSlot, Command: what})
	if e == nil {
		e = s.store.FinishAction(ctx, id, now, exit, out)
	}
	if e != nil {
		s.log.Error("action log", "err", e)
	}
}

// selfInfo identifies Fleetling's own Compose project.
type selfInfo struct {
	Project    string
	Slot       string
	WorkingDir string
}

// self finds Fleetling's own container and reads its Compose labels.
// ok is false outside a container or when it can't be found.
func (s *Server) self(ctx context.Context, st store.Settings) (selfInfo, bool) {
	if !s.inContainer() {
		return selfInfo{}, false
	}
	ids := s.selfIDs()
	for _, ep := range endpoints(st) {
		e, err := s.newEngine(ep.Host)
		if err != nil {
			continue
		}
		for _, id := range ids {
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			labels, err := e.ContainerLabels(c, id)
			cancel()
			if err == nil && labels[engine.LabelProject] != "" {
				e.Close()
				return selfInfo{Project: labels[engine.LabelProject], Slot: ep.Slot, WorkingDir: labels[engine.LabelWorkingDir]}, true
			}
		}
		e.Close()
	}
	return selfInfo{}, false
}

func (si selfInfo) is(f compose.Folder) bool {
	if si.Project == "" {
		return false
	}
	if si.WorkingDir != "" && filepath.Clean(si.WorkingDir) == f.Dir {
		return true
	}
	return f.Meta != nil && f.Meta.Project == si.Project && f.Meta.Engine == si.Slot
}

// existingStackFor reports a folder or running project that already uses
// project on slot, so a new stack can't silently take over its containers.
func (s *Server) existingStackFor(ctx context.Context, st store.Settings, project, slot string) string {
	folders, _ := compose.Discover(st.Root, st.Ignore)
	for _, f := range folders {
		if f.Project == project && (f.Meta == nil || f.Meta.Engine == slot) {
			return "the folder " + f.Dir + " already uses that project name"
		}
	}
	containers, _ := s.gather(ctx, st)
	for _, c := range containers {
		if c.Project == project && c.Engine == slot {
			return "a project with that name is already running on " + slot + " (Adopt arrives with the Portainer importer)"
		}
	}
	return ""
}

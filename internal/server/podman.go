package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/detect"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
)

// engineHint is the line under the create form's engine field. With only
// Docker configured a Podman file still gets a warning, since it would be
// deployed somewhere it doesn't fit.
func engineHint(r detect.Result, slots []string) string {
	why := r.Why()
	if why == "" {
		return ""
	}
	if !slices.Contains(slots, r.Engine) {
		return why + ". No Podman endpoint is configured in Settings, so this stack would run on Docker."
	}
	return why
}

type detectReply struct {
	Engine string `json:"engine"` // "" means keep the default
	Why    string `json:"why"`
}

// stackDetect answers the create form's script as the compose text changes.
func (s *Server) stackDetect(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.LoadSettings(r.Context(), s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	res := detect.Detect(r.PostFormValue("compose"))
	slots := engineSlots(st)
	rep := detectReply{Why: engineHint(res, slots)}
	if res.Engine != "" && slices.Contains(slots, res.Engine) {
		rep.Engine = res.Engine
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(rep)
}

// The podman-restart check. Podman has no daemon to bring containers back
// after a reboot; podman-restart.service does that for every container
// with a restart policy. Without it, restart: unless-stopped does nothing
// on a reboot.

const restartUnit = "podman-restart.service"

// unitWants are the folders under /etc/systemd/system where an enabled
// unit leaves its link. podman-restart.service installs into default.target.
var unitWants = []string{"default.target.wants", "multi-user.target.wants"}

// UnitEnabled reports whether unit is linked under one of unitWants in
// dir. Lstat, not Stat: the link points at /usr/lib/systemd/..., which a
// container usually can't see.
func UnitEnabled(dir, unit string) bool {
	for _, w := range unitWants {
		if _, err := os.Lstat(filepath.Join(dir, w, unit)); err == nil {
			return true
		}
	}
	return false
}

// podmanRestartEnabled checks the host. Outside a container that is a
// plain file check. Inside one, a container can't see the host's systemd,
// so a throwaway container from Fleetling's own image mounts
// /etc/systemd/system read-only and checks there. It runs on whichever
// engine Fleetling itself runs on, since that is where its image is; both
// engines share the host's /etc.
func (s *Server) podmanRestartEnabled(ctx context.Context, st store.Settings) (bool, error) {
	if !s.inContainer() {
		return UnitEnabled("/etc/systemd/system", restartUnit), nil
	}
	if s.checkUnit != nil {
		return s.checkUnit(ctx, st, restartUnit)
	}
	si, ok := s.self(ctx, st)
	if !ok {
		return false, errors.New("could not find Fleetling's own container to run the check from")
	}
	image, err := s.selfImage(ctx, st, si)
	if err != nil {
		return false, err
	}
	host, _, err := engine.NormalizeHost(hostFor(st, si.Slot))
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := s.runner.Plain(ctx, host, si.Slot == compose.EnginePodman,
		unitCheckArgs(image, restartUnit)[1:]...).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("checking %s: %v %s", restartUnit, err, strings.TrimSpace(string(out)))
	}
	switch strings.TrimSpace(string(out)) {
	case "enabled":
		return true, nil
	case "missing":
		return false, nil
	}
	return false, fmt.Errorf("checking %s: unexpected answer %q", restartUnit, strings.TrimSpace(string(out)))
}

// unitCheckArgs is the throwaway container. No network, read-only mount.
func unitCheckArgs(image, unit string) []string {
	return []string{"docker", "run", "--rm", "--network", "none",
		"-v", "/etc/systemd/system:/host/systemd:ro",
		"--entrypoint", "/usr/local/bin/fleetling", image, "unitcheck", unit}
}

// Moving a stack between engines: down on the old engine, rewrite the
// engine in .fleetling.toml, then up -d on the new one. Images are pulled
// again on the new engine. Named volumes live inside each engine and do
// not move; bind mounts under the root are the same folders either way.

type engineMoveData struct {
	Folder  compose.Folder
	From    string
	To      string
	DownCmd string
	UpCmd   string
	Refusal string
}

// moveTargets builds the old and new Compose targets for a move.
func (s *Server) moveTargets(ctx context.Context, st store.Settings, f compose.Folder) (engineMoveData, compose.Target, compose.Target) {
	d := engineMoveData{Folder: f}
	if f.Meta == nil {
		d.Refusal = "Manage this folder first."
		return d, compose.Target{}, compose.Target{}
	}
	d.From = f.Meta.Engine
	for _, slot := range engineSlots(st) {
		if slot != d.From {
			d.To = slot
		}
	}
	if d.To == "" {
		d.Refusal = "Only one engine is configured. Add the other one in Settings to move stacks between them."
		return d, compose.Target{}, compose.Target{}
	}
	if si, ok := s.self(ctx, st); ok && si.is(f) {
		d.Refusal = "This is Fleetling's own stack. Moving it would take Fleetling down halfway through; move it from the host instead."
		return d, compose.Target{}, compose.Target{}
	}
	from, err := s.target(st, f)
	if err != nil {
		d.Refusal = err.Error()
		return d, from, compose.Target{}
	}
	moved := f
	m := *f.Meta
	m.Engine = d.To
	moved.Meta = &m
	to, err := s.target(st, moved)
	if err != nil {
		d.Refusal = err.Error()
		return d, from, to
	}
	d.DownCmd = compose.Quote(s.runner.Args(from, "down"))
	d.UpCmd = compose.Quote(s.runner.Args(to, "up", "-d"))
	return d, from, to
}

func (s *Server) engineMovePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, ok := loadFolder(st, r.PathValue("folder"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, _, _ := s.moveTargets(ctx, st, f)
	s.render(w, http.StatusOK, "stack_engine", s.page(r, "Move "+f.Name, "stacks", d))
}

func (s *Server) engineMove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, ok := loadFolder(st, r.PathValue("folder"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, from, to := s.moveTargets(ctx, st, f)
	if d.Refusal != "" {
		http.Error(w, d.Refusal, http.StatusConflict)
		return
	}
	if r.PostFormValue("to") != d.To {
		http.Error(w, "the target engine changed; reload the page", http.StatusConflict)
		return
	}
	if err := s.reviewedOrErr(ctx, f); errors.Is(err, errNeedsReview) {
		http.Redirect(w, r, "/stacks/"+f.Name+"/review", http.StatusSeeOther)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The project must not already be running on the new engine, or the
	// up would quietly take over someone else's containers.
	containers, _ := s.gather(ctx, st)
	for _, c := range containers {
		if c.Engine == d.To && c.Project == f.Meta.Project {
			http.Error(w, "a project named "+f.Meta.Project+" already runs on "+d.To+"; stop it there first", http.StatusConflict)
			return
		}
	}
	if s.jobs.Running(f.Name) != nil {
		http.Error(w, compose.ErrBusy.Error(), http.StatusConflict)
		return
	}
	meta := "fleetling: set engine = " + d.To + " in " + filepath.Join(f.Dir, compose.MetaFile)
	steps := []compose.Step{
		{Cmd: s.runner.Command(s.base, from, "down")},
		{Show: meta, Run: func(ctx context.Context, j *compose.Job) int {
			j.Line(meta)
			m, err := compose.ReadMeta(f.Dir)
			if err == nil {
				m.Engine = d.To
				err = compose.WriteMeta(f.Dir, m, compose.OwnerOf(filepath.Join(f.Dir, compose.MetaFile)))
			}
			if err == nil {
				err = s.recordBaseline(context.Background(), f.Dir, "engine move")
			}
			if err != nil {
				j.Linef("fleetling: %v", err)
				return 1
			}
			return 0
		}},
		{Cmd: s.runner.Command(s.base, to, "up", "-d")},
	}
	lines := []string{d.DownCmd, meta, d.UpCmd}
	id, err := s.store.StartAction(ctx, store.Action{Started: s.now(), Target: f.Name, Engine: d.From + " -> " + d.To, Command: strings.Join(lines, "\n")})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	job, err := s.jobs.Start(compose.JobSpec{
		Target: f.Name, Engine: d.To, Steps: steps,
		Label: "Move " + f.Name + " to " + d.To, Back: "/stacks/" + f.Name,
		Done: func(j *compose.Job) {
			_, exit, fin := j.Result()
			if err := s.store.FinishAction(context.Background(), id, fin, exit, strings.Join(j.Tail(200), "\n")); err != nil {
				s.log.Error("action log", "err", err)
			}
		},
	})
	if err != nil {
		s.store.FinishAction(ctx, id, s.now(), -1, "fleetling: "+err.Error())
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/stacks/"+f.Name+"?job="+job.ID, http.StatusSeeOther)
}

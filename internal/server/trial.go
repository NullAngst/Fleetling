package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/store"
)

// The Portainer trial. Moving off Portainer is easier to commit to when
// it can be undone, so an import doesn't burn the bridge:
//
//   - Every import records what it wrote into each folder.
//   - Stopping Portainer stops its container and turns its restart policy
//     off for the trial, since --restart=always would bring it back on the
//     next reboot. Nothing is removed.
//   - Go back to Portainer puts the restart policy back, starts Portainer,
//     and hands each imported stack back: .fleetling.toml goes, and so does
//     every file still exactly as the import wrote it. A file changed in
//     Fleetling since then stays, so no edit is lost. The containers are
//     never touched; Portainer finds them by project name, the same way
//     Fleetling did.
//   - Finish migration removes Portainer's container, which is what Retire
//     used to do on the spot. Its data folder stays either way.
//
// The record is a setting, not its own table, so it needs no schema change.

const keyPortainerTrial = "portainer_trial"

type portainerTrial struct {
	Started time.Time
	// Set once Portainer was stopped for the trial. Empty while it still
	// runs alongside Fleetling.
	Container  string
	Name       string
	Image      string
	Restart    string // its restart policy before the trial
	RestartMax int
	Stopped    *time.Time
	Stacks     []trialStack
}

type trialStack struct {
	Folder    string
	Project   string
	NewFolder bool              // the import made the folder
	Files     map[string]string // file -> sha256 of what the import wrote
	EnvLink   bool              // .env is a symlink to stack.env the import made
	Compose   string            // compose.yaml as the import wrote it, for the diff
}

func (s *Server) loadTrial(ctx context.Context) (*portainerTrial, error) {
	v, ok, err := s.store.Get(ctx, keyPortainerTrial)
	if err != nil || !ok || v == "" {
		return nil, err
	}
	var t portainerTrial
	if err := json.Unmarshal([]byte(v), &t); err != nil {
		return nil, fmt.Errorf("the Portainer trial record is unreadable: %w", err)
	}
	return &t, nil
}

// saveTrial writes the record; nil ends the trial.
func (s *Server) saveTrial(ctx context.Context, t *portainerTrial) error {
	if t == nil {
		return s.store.Set(ctx, keyPortainerTrial, "")
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return s.store.Set(ctx, keyPortainerTrial, string(b))
}

// updateTrial loads, changes and saves the record under one lock, so two
// imports at once can't drop each other's stacks.
func (s *Server) updateTrial(ctx context.Context, fn func(t *portainerTrial) *portainerTrial) error {
	s.trialMu.Lock()
	defer s.trialMu.Unlock()
	t, err := s.loadTrial(ctx)
	if err != nil {
		return err
	}
	return s.saveTrial(ctx, fn(t))
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// trialAdd records one imported stack.
func (s *Server) trialAdd(ctx context.Context, folder, project string, newFolder bool, wrote map[string][]byte) error {
	ts := trialStack{Folder: folder, Project: project, NewFolder: newFolder, Files: map[string]string{}, Compose: string(wrote["compose.yaml"])}
	for name, b := range wrote {
		if name == compose.MetaFile {
			continue // Fleetling's own file; always removed on the way back
		}
		ts.Files[name] = sha(b)
	}
	if _, linked := wrote["stack.env"]; linked {
		ts.EnvLink = true
		delete(ts.Files, ".env") // the link, not a file of its own
	}
	return s.updateTrial(ctx, func(t *portainerTrial) *portainerTrial {
		if t == nil {
			t = &portainerTrial{Started: s.now().UTC().Truncate(time.Second)}
		}
		// A folder imported again replaces its old entry.
		kept := t.Stacks[:0]
		for _, o := range t.Stacks {
			if o.Folder != folder {
				kept = append(kept, o)
			}
		}
		t.Stacks = append(kept, ts)
		return t
	})
}

// What going back does to one stack, worked out before anything changes
// so the page can show it.
type trialPlan struct {
	Folder  string
	Project string
	Gone    bool     // the folder is no longer a stack: removed in Fleetling
	Remove  []string // files that go: exactly as the import wrote them, plus .fleetling.toml
	Keep    []string // files that changed after the import and stay
	Diff    []diffRow
	Running int // the project's running containers, as Portainer will find them
	Busy    bool
	Err     string
}

func (s *Server) planTrial(ctx context.Context, st store.Settings, t *portainerTrial) []trialPlan {
	var running map[string]int
	if e, _, err := s.engineFor(ctx, "docker"); err == nil {
		running = map[string]int{}
		for _, ts := range t.Stacks {
			if cs, err := e.ProjectContainers(ctx, ts.Project); err == nil {
				for _, c := range cs {
					if c.Running {
						running[ts.Project]++
					}
				}
			}
		}
		e.Close()
	}
	var out []trialPlan
	for _, ts := range t.Stacks {
		p := trialPlan{Folder: ts.Folder, Project: ts.Project, Running: running[ts.Project]}
		dir := filepath.Join(st.Root, ts.Folder)
		f, ok := loadFolder(st, ts.Folder)
		if !ok || f.Meta == nil {
			p.Gone = true
		} else if s.jobs.Running(f.Name) != nil {
			p.Busy = true
		}
		if _, err := os.Lstat(filepath.Join(dir, compose.MetaFile)); err == nil {
			p.Remove = append(p.Remove, compose.MetaFile)
		}
		names := make([]string, 0, len(ts.Files))
		for n := range ts.Files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(dir, name)
			fi, err := os.Lstat(path)
			if err != nil {
				continue // already gone
			}
			b, err := os.ReadFile(path)
			if err != nil || !fi.Mode().IsRegular() || sha(b) != ts.Files[name] {
				p.Keep = append(p.Keep, name)
				if name == "compose.yaml" && err == nil {
					p.Diff, _, _ = sideBySide(ts.Compose, string(b))
				}
				continue
			}
			p.Remove = append(p.Remove, name)
		}
		if ts.EnvLink {
			if l, err := os.Readlink(filepath.Join(dir, ".env")); err == nil && l == "stack.env" {
				// The link goes with stack.env, or stays with it.
				if containsStr(p.Keep, "stack.env") {
					p.Keep = append(p.Keep, ".env")
				} else {
					p.Remove = append(p.Remove, ".env")
				}
			}
		}
		out = append(out, p)
	}
	return out
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

type trialData struct {
	Trial     *portainerTrial
	Plans     []trialPlan
	Portainer portainerBox
	State     string // running, stopped, gone
	Root      string
	Changed   int
	Busy      bool
}

func (s *Server) trialPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.loadTrial(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if t == nil {
		http.Redirect(w, r, "/import", http.StatusSeeOther)
		return
	}
	s.renderTrial(w, r, t, "", http.StatusOK)
}

func (s *Server) renderTrial(w http.ResponseWriter, r *http.Request, t *portainerTrial, msg string, status int) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := trialData{Trial: t, Plans: s.planTrial(ctx, st, t), Root: st.Root}
	d.Portainer, d.State = s.trialPortainer(ctx, t)
	for _, p := range d.Plans {
		if len(p.Keep) > 0 {
			d.Changed++
		}
		if p.Busy {
			d.Busy = true
		}
	}
	p := s.page(r, "Portainer trial", "import", d)
	p.Error = msg
	switch r.URL.Query().Get("notice") {
	case "stopped":
		p.Notice = "Portainer is stopped, not removed. Its restart policy is off until you go back or finish."
	}
	s.render(w, status, "import_trial", p)
}

// trialPortainer finds Portainer's container, the one the trial stopped
// or else the one findPortainer sees, and says whether it is running,
// stopped or gone.
func (s *Server) trialPortainer(ctx context.Context, t *portainerTrial) (portainerBox, string) {
	id := t.Container
	if id == "" {
		pb := s.findPortainer(ctx)
		if pb.Err != "" {
			return pb, ""
		}
		if pb.ID == "" {
			return pb, "gone"
		}
		id = pb.ID
	}
	e, _, err := s.engineFor(ctx, "docker")
	if err != nil {
		return portainerBox{Err: err.Error()}, ""
	}
	defer e.Close()
	d, err := e.Inspect(ctx, id)
	if err != nil {
		return portainerBox{Name: t.Name, Image: t.Image}, "gone"
	}
	state := "stopped"
	if d.Running {
		state = "running"
	}
	return portainerBox{Name: d.Name, ID: d.ID, Image: d.Image, Command: cliLine("remove", d.Name)}, state
}

// importRetire stops Portainer for the trial. Its container, data folder
// and volume all stay; Finish migration removes the container later.
func (s *Server) importRetire(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pb := s.findPortainer(ctx)
	if pb.ID == "" {
		http.Error(w, "no Portainer container found", http.StatusNotFound)
		return
	}
	if s.isSelfContainer(pb.ID) {
		http.Error(w, "refusing to stop Fleetling's own container", http.StatusConflict)
		return
	}
	e, _, err := s.engineFor(ctx, "docker")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer e.Close()
	d, err := e.Inspect(ctx, pb.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// A trial that already stopped this container keeps the policy it
	// recorded then; the live one is "no" by now.
	restart, max := d.Restart, d.RestartMax
	if t, _ := s.loadTrial(ctx); t != nil && t.Container == d.ID {
		restart, max = t.Restart, t.RestartMax
	}
	// Policy first: if it can't be turned off, nothing is stopped, rather
	// than a stop that a reboot quietly undoes.
	err = e.SetRestartPolicy(ctx, d.ID, "no", 0)
	if err == nil {
		err = e.Stop(ctx, d.ID)
	}
	s.logFileAction(ctx, "container/"+d.Name, "docker",
		compose.Quote([]string{"docker", "update", "--restart", "no", d.Name})+" && "+cliLine("stop", d.Name), err)
	if err != nil {
		if d.Restart != "" && d.Restart != "no" {
			e.SetRestartPolicy(ctx, d.ID, d.Restart, d.RestartMax)
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	now := s.now().UTC().Truncate(time.Second)
	err = s.updateTrial(ctx, func(t *portainerTrial) *portainerTrial {
		if t == nil {
			t = &portainerTrial{Started: now}
		}
		t.Container, t.Name, t.Image = d.ID, d.Name, d.Image
		t.Restart, t.RestartMax, t.Stopped = restart, max, &now
		return t
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/import/trial?notice=stopped", http.StatusSeeOther)
}

type trialOutcome struct {
	Folder string
	Did    string
	Kept   []string
	Err    string
}

// trialRevert goes back to Portainer.
func (s *Server) trialRevert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.trialMu.Lock()
	defer s.trialMu.Unlock()
	t, err := s.loadTrial(ctx)
	if err != nil || t == nil {
		http.Redirect(w, r, "/import", http.StatusSeeOther)
		return
	}
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	plans := s.planTrial(ctx, st, t)
	for _, p := range plans {
		if p.Busy {
			s.renderTrial(w, r, t, p.Folder+" has an action running. Wait for it to finish, then go back.", http.StatusConflict)
			return
		}
	}

	// Portainer first. If it can't be brought back, nothing is handed
	// back to it either.
	pb, state := s.trialPortainer(ctx, t)
	switch state {
	case "":
		s.renderTrial(w, r, t, "Could not reach Docker to start Portainer: "+pb.Err, http.StatusBadGateway)
		return
	case "gone":
		s.renderTrial(w, r, t, "Portainer's container is gone, so there is nothing to start. Bring Portainer back from its data folder first; this page stays until you do.", http.StatusConflict)
		return
	}
	if t.Container != "" || state != "running" {
		e, _, err := s.engineFor(ctx, "docker")
		if err != nil {
			s.renderTrial(w, r, t, err.Error(), http.StatusBadGateway)
			return
		}
		var cmds []string
		if t.Container != "" {
			// The trial turned the policy off; put back what it was.
			restart := t.Restart
			if restart == "" {
				restart = "no"
			}
			err = e.SetRestartPolicy(ctx, pb.ID, restart, t.RestartMax)
			cmds = append(cmds, compose.Quote([]string{"docker", "update", "--restart", restart, pb.Name}))
		}
		if err == nil && state != "running" {
			err = e.Start(ctx, pb.ID)
			cmds = append(cmds, cliLine("start", pb.Name))
		}
		e.Close()
		s.logFileAction(ctx, "container/"+pb.Name, "docker", strings.Join(cmds, " && "), err)
		if err != nil {
			s.renderTrial(w, r, t, "Could not start Portainer: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	var outcomes []trialOutcome
	for _, p := range plans {
		o := trialOutcome{Folder: p.Folder, Kept: p.Keep}
		dir := filepath.Join(st.Root, p.Folder)
		var errs []string
		for _, name := range p.Remove {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err.Error())
			}
		}
		// Approved files and the update schedule belong to the managed
		// stack, which this was.
		if err := s.store.DeleteBaseline(ctx, p.Folder); err != nil {
			errs = append(errs, err.Error())
		}
		if err := s.store.DeleteSchedule(ctx, p.Folder); err != nil {
			errs = append(errs, err.Error())
		}
		removedDir := false
		if ts := trialStackFor(t, p.Folder); ts != nil && ts.NewFolder && len(p.Keep) == 0 {
			// Only when empty: app data the stack wrote there stays.
			removedDir = os.Remove(dir) == nil
		}
		switch {
		case p.Gone && len(p.Remove) == 0:
			o.Did = "Already removed in Fleetling. Nothing left to hand back."
		case len(p.Keep) > 0:
			o.Did = "No longer managed by Fleetling. Kept what changed after the import: " + strings.Join(p.Keep, ", ") + "."
		case removedDir:
			o.Did = "No longer managed by Fleetling. The folder was empty and is gone."
		default:
			o.Did = "No longer managed by Fleetling. The files the import wrote are gone."
		}
		if len(errs) > 0 {
			o.Err = strings.Join(errs, "; ")
		}
		s.logFileAction(ctx, p.Folder, "docker", "fleetling: hand "+p.Folder+" back to Portainer, remove "+strings.Join(p.Remove, ", "), errorsJoin(errs))
		outcomes = append(outcomes, o)
	}
	if err := s.saveTrial(ctx, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "import_trial_done", s.page(r, "Back to Portainer", "import",
		map[string]any{"Outcomes": outcomes, "Portainer": pb}))
}

func trialStackFor(t *portainerTrial, folder string) *trialStack {
	for i := range t.Stacks {
		if t.Stacks[i].Folder == folder {
			return &t.Stacks[i]
		}
	}
	return nil
}

func errorsJoin(msgs []string) error {
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, "; "))
}

// trialFinish ends the trial for good: Portainer's container goes, its
// data stays.
func (s *Server) trialFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.trialMu.Lock()
	defer s.trialMu.Unlock()
	t, err := s.loadTrial(ctx)
	if err != nil || t == nil {
		http.Redirect(w, r, "/import", http.StatusSeeOther)
		return
	}
	pb, state := s.trialPortainer(ctx, t)
	if state == "" {
		s.renderTrial(w, r, t, "Could not reach Docker to remove Portainer: "+pb.Err, http.StatusBadGateway)
		return
	}
	if state != "gone" {
		if s.isSelfContainer(pb.ID) {
			http.Error(w, "refusing to remove Fleetling's own container", http.StatusConflict)
			return
		}
		e, _, err := s.engineFor(ctx, "docker")
		if err != nil {
			s.renderTrial(w, r, t, err.Error(), http.StatusBadGateway)
			return
		}
		err = e.Remove(ctx, pb.ID)
		e.Close()
		s.logFileAction(ctx, "container/"+pb.Name, "docker", pb.Command, err)
		if err != nil {
			s.renderTrial(w, r, t, "Could not remove Portainer's container: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	if err := s.saveTrial(ctx, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/?notice=migrated", http.StatusSeeOther)
}

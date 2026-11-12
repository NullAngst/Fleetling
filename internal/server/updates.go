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
	"sync"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/registry"
	"github.com/NullAngst/Fleetling/internal/schedule"
	"github.com/NullAngst/Fleetling/internal/store"
	"github.com/NullAngst/Fleetling/internal/updater"
)

// Auto-update. A run records each service's image ID, pulls, and runs
// up -d only if an image ID changed, so nothing is recreated for no
// reason. Every run, scheduled or manual, passes the drift gate first: a
// stack whose files changed outside Fleetling is skipped, never updated.

// errSkipped means a run was skipped; the reason is already recorded.
var errSkipped = errors.New("skipped")

// pullCmd is the pull every run starts with. Services that build are
// left out; their images don't come from a registry.
var pullCmd = []string{"pull", "--ignore-buildable"}

// updateCommand is what the "Pull and redeploy now" button shows.
func (s *Server) updateCommand(t compose.Target) string {
	return compose.Quote(s.runner.Args(t, pullCmd...)) + " && (only if an image ID changed) " + compose.Quote(s.runner.Args(t, "up", "-d"))
}

// autoUpdate runs one update. trigger is "schedule" or "manual".
func (s *Server) autoUpdate(ctx context.Context, st store.Settings, f compose.Folder, trigger string) (*compose.Job, error) {
	record := func(result, msg string) {
		if err := s.store.RecordUpdate(context.Background(), f.Name, s.now(), result, msg); err != nil {
			s.log.Error("record update", "err", err)
		}
	}
	slot := ""
	if f.Meta != nil {
		slot = f.Meta.Engine
	}
	skip := func(reason string) error {
		s.logFileAction(ctx, f.Name, slot, "fleetling: "+trigger+" update skipped for "+f.Name, errors.New(reason))
		record(store.UpdateSkipped, reason)
		return fmt.Errorf("%w: %s", errSkipped, reason)
	}
	t, err := s.target(st, f)
	if err != nil {
		return nil, skip(err.Error())
	}
	// The drift gate. Zack's hard requirement: never update a stack whose
	// files changed outside Fleetling. The stack keeps its review flag.
	if err := s.reviewedOrErr(ctx, f); err != nil {
		return nil, skip(err.Error())
	}
	if s.jobs.Running(f.Name) != nil {
		return nil, skip("another action was running on this stack")
	}
	if si, ok := s.self(ctx, st); ok && si.is(f) {
		// Fleetling's own stack goes through the update helper. It reports
		// its own result in last-update.log; reconcileSelfUpdate reads it
		// when the new process starts.
		if err := s.runSelfHelper(ctx, st, f, si, updater.ModeUpdate); err != nil {
			record(store.UpdateFailed, err.Error())
			return nil, err
		}
		record(store.UpdateOK, "handed to the update helper; see the last update log")
		return nil, nil
	}

	// A stack the user stopped or took down stays that way: up -d would
	// start it again.
	if e, err := s.newEngine(hostFor(st, f.Meta.Engine)); err == nil {
		ctrs, err := e.ProjectContainers(ctx, t.Project)
		e.Close()
		if err != nil {
			return nil, skip("could not list the stack's containers: " + err.Error())
		}
		if !slices.ContainsFunc(ctrs, func(c engine.ProjectContainer) bool { return c.Running }) {
			return nil, skip("nothing in this stack is running, and an update doesn't start a stopped stack")
		}
	}

	before := map[string]string{} // service -> image ID before the pull
	images := map[string]string{} // service -> image name
	recordStep := "fleetling: record each service's image ID"
	compare := "fleetling: up -d only if an image ID changed"
	steps := []compose.Step{
		{Show: recordStep, Run: func(ctx context.Context, j *compose.Job) int {
			j.Line(recordStep)
			cfg, err := s.runner.ResolvedConfig(ctx, t)
			if err != nil {
				j.Linef("fleetling: %v", err)
				return 1
			}
			svcs, err := serviceImages(cfg, t.Project)
			if err != nil {
				j.Linef("fleetling: %v", err)
				return 1
			}
			for _, sv := range svcs {
				images[sv[0]] = sv[1]
			}
			e, err := s.newEngine(hostFor(st, f.Meta.Engine))
			if err != nil {
				j.Linef("fleetling: %v", err)
				return 1
			}
			defer e.Close()
			ctrs, err := e.ProjectContainers(ctx, t.Project)
			if err != nil {
				j.Linef("fleetling: %v", err)
				return 1
			}
			for _, c := range ctrs {
				if c.Running && before[c.Service] == "" {
					before[c.Service] = c.ImageID
				}
			}
			for _, sv := range svcs {
				j.Linef("fleetling: %s runs %s (%s)", sv[0], orDash(before[sv[0]]), sv[1])
			}
			return 0
		}},
		{Cmd: s.runner.Command(s.base, t, pullCmd...)},
		{Show: compare, Run: func(ctx context.Context, j *compose.Job) int {
			j.Line(compare)
			e, err := s.newEngine(hostFor(st, f.Meta.Engine))
			if err != nil {
				j.Linef("fleetling: %v", err)
				return 1
			}
			defer e.Close()
			var changed []string
			names := make([]string, 0, len(images))
			for svc := range images {
				names = append(names, svc)
			}
			slices.Sort(names)
			for _, svc := range names {
				info, err := e.ImageInfo(ctx, images[svc])
				switch {
				case err != nil:
					j.Linef("fleetling: %s: no local image %s (%v)", svc, images[svc], err)
				case before[svc] == "":
					j.Linef("fleetling: %s isn't running; left alone", svc)
				case info.ID != before[svc]:
					changed = append(changed, svc)
					j.Linef("fleetling: %s: %s -> %s", svc, before[svc], info.ID)
				}
			}
			if len(changed) == 0 {
				j.Line("fleetling: no image changed; nothing recreated")
				return 0
			}
			j.Linef("fleetling: changed: %s", strings.Join(changed, ", "))
			return compose.RunCmd(j, s.runner.Command(s.base, t, "up", "-d"), compose.Quote(s.runner.Args(t, "up", "-d")))
		}},
	}
	var lines []string
	for _, st := range steps {
		if st.Cmd != nil {
			lines = append(lines, compose.Quote(s.runner.Args(t, pullCmd...)))
		} else {
			lines = append(lines, st.Show)
		}
	}
	id, err := s.store.StartAction(ctx, store.Action{Started: s.now(), Target: f.Name, Engine: f.Meta.Engine, Command: strings.Join(lines, "\n")})
	if err != nil {
		return nil, err
	}
	job, err := s.jobs.Start(compose.JobSpec{
		Target: f.Name, Engine: f.Meta.Engine, Steps: steps,
		Label: "Update " + f.Name, Back: "/stacks/" + f.Name + "?tab=updates",
		Done: func(j *compose.Job) {
			_, exit, fin := j.Result()
			tail := j.Tail(200)
			if err := s.store.FinishAction(context.Background(), id, fin, exit, strings.Join(tail, "\n")); err != nil {
				s.log.Error("action log", "err", err)
			}
			if exit == 0 {
				record(store.UpdateOK, "")
			} else {
				msg := fmt.Sprintf("exit %d", exit)
				if len(tail) > 0 {
					msg += ": " + tail[len(tail)-1]
				}
				record(store.UpdateFailed, msg)
			}
		},
	})
	if err != nil {
		s.store.FinishAction(ctx, id, s.now(), -1, "fleetling: "+err.Error())
		return nil, err
	}
	s.log.Info("update", "stack", f.Name, "trigger", trigger, "job", job.ID)
	return job, nil
}

func orDash(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

// serviceImages lists [service, image name] from the resolved config. A
// build service without image: is tagged <project>-<service>.
func serviceImages(configJSON []byte, project string) ([][2]string, error) {
	var cfg struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parse compose config: %w", err)
	}
	var out [][2]string
	for name, s := range cfg.Services {
		img := s.Image
		if img == "" {
			img = project + "-" + name
		}
		out = append(out, [2]string{name, img})
	}
	slices.SortFunc(out, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	return out, nil
}

// runSchedules starts every enabled schedule due in the minute now falls
// in. The scheduler loop calls it once a minute.
func (s *Server) runSchedules(now time.Time) []string {
	ctx := s.base
	scheds, err := s.store.ListSchedules(ctx)
	if err != nil {
		s.log.Error("schedules", "err", err)
		return nil
	}
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		s.log.Error("schedules", "err", err)
		return nil
	}
	var started []string
	for _, sc := range scheds {
		if !sc.Enabled || sc.Cron == "" || !schedule.Due(sc.Cron, now) {
			continue
		}
		f, ok := loadFolder(st, sc.Folder)
		if !ok {
			s.log.Warn("scheduled update for a folder that is gone", "folder", sc.Folder)
			continue
		}
		if _, err := s.autoUpdate(ctx, st, f, "scheduled"); err != nil && !errors.Is(err, errSkipped) {
			s.log.Error("scheduled update", "stack", f.Name, "err", err)
		}
		started = append(started, f.Name)
	}
	return started
}

// scheduleLoop wakes at the start of every minute.
func (s *Server) scheduleLoop() {
	for {
		now := s.now()
		next := now.Truncate(time.Minute).Add(time.Minute)
		select {
		case <-time.After(next.Sub(now)):
			s.runSchedules(next)
		case <-s.base.Done():
			return
		}
	}
}

// reconcileSelfUpdate runs at startup: a scheduled self-update is only
// known to have failed once the helper has written its log, and by then
// the old process is gone.
func (s *Server) reconcileSelfUpdate(ctx context.Context) {
	d := s.readLastUpdate()
	if d.Missing || !d.Failed {
		return
	}
	fi, err := os.Stat(filepath.Join(s.cfg.DataDir, updater.LogFile))
	if err != nil {
		return
	}
	scheds, _ := s.store.ListSchedules(ctx)
	for _, sc := range scheds {
		if sc.LastResult == store.UpdateOK && strings.Contains(sc.LastMsg, "update helper") && fi.ModTime().After(sc.LastRun) {
			s.store.RecordUpdate(ctx, sc.Folder, fi.ModTime(), store.UpdateFailed, "the update helper rolled back or failed; see the last update log")
		}
	}
}

type updatesTab struct {
	Cron     string
	Mode     string // off, daily, weekly, custom
	Enabled  bool
	Describe string
	Next     string
	Zone     string
	Last     store.Schedule
	HasLast  bool
	Command  string
}

func (s *Server) updatesTabData(ctx context.Context, st store.Settings, f compose.Folder) updatesTab {
	u := updatesTab{Mode: "off", Zone: schedule.Zone()}
	if sc, ok, err := s.store.GetSchedule(ctx, f.Name); err == nil && ok {
		u.Last, u.HasLast = sc, !sc.LastRun.IsZero()
		u.Cron, u.Enabled = sc.Cron, sc.Enabled
		switch {
		case !sc.Enabled || sc.Cron == "":
		case sc.Cron == schedule.Daily:
			u.Mode = "daily"
		case sc.Cron == schedule.Weekly:
			u.Mode = "weekly"
		default:
			u.Mode = "custom"
		}
		if sc.Enabled && sc.Cron != "" {
			u.Describe = schedule.Describe(sc.Cron)
			if n, err := schedule.Next(sc.Cron, s.now()); err == nil {
				u.Next = n.Format("Mon 2006-01-02 15:04")
			}
		}
	}
	if t, err := s.target(st, f); err == nil {
		u.Command = s.updateCommand(t)
	}
	return u
}

func (s *Server) scheduleSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, ok := loadFolder(st, r.PathValue("folder"))
	if !ok || f.Meta == nil {
		http.NotFound(w, r)
		return
	}
	mode := r.PostFormValue("mode")
	expr, enabled := "", true
	switch mode {
	case "off":
		enabled = false
		if sc, ok, _ := s.store.GetSchedule(ctx, f.Name); ok {
			expr = sc.Cron
		}
	case "daily", "weekly":
		expr, _ = schedule.Parse(mode)
	case "custom":
		expr, err = schedule.Parse(r.PostFormValue("cron"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "unknown schedule", http.StatusBadRequest)
		return
	}
	err = s.store.SetSchedule(ctx, f.Name, expr, enabled)
	what := "fleetling: auto-update for " + f.Name + " off"
	if enabled {
		what = "fleetling: auto-update for " + f.Name + " " + schedule.Describe(expr) + " (" + expr + "), " + schedule.Zone()
	}
	s.logFileAction(ctx, f.Name, f.Meta.Engine, what, err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/stacks/"+f.Name+"?tab=updates&saved=1", http.StatusSeeOther)
}

func (s *Server) updateNow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, ok := loadFolder(st, r.PathValue("folder"))
	if !ok || f.Meta == nil {
		http.NotFound(w, r)
		return
	}
	job, err := s.autoUpdate(ctx, st, f, "manual")
	switch {
	case errors.Is(err, errSkipped) && strings.Contains(err.Error(), errNeedsReview.Error()):
		http.Redirect(w, r, "/stacks/"+f.Name+"/review", http.StatusSeeOther)
	case err != nil:
		http.Error(w, err.Error(), http.StatusConflict)
	case job == nil:
		http.Redirect(w, r, "/self/updating?mode=update", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/stacks/"+f.Name+"?tab=updates&job="+job.ID, http.StatusSeeOther)
	}
}

type imageCheck struct {
	Service string
	Image   string
	Status  registry.Status
	Detail  string
}

// updateCheck is the per-service "update available" partial, loaded by
// the stack page after it opens. It asks registries anonymously, once per
// image per six hours.
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, ok := loadFolder(st, r.PathValue("folder"))
	if !ok || f.Meta == nil {
		http.NotFound(w, r)
		return
	}
	var rows []imageCheck
	var problem string
	func() {
		// The image names come from the files on disk. Until those are
		// reviewed, they may name anything, and the check would send
		// requests wherever they point.
		if err := s.reviewedOrErr(ctx, f); err != nil {
			problem = "review this stack's files first; the check reads image names from them"
			return
		}
		t, err := s.target(st, f)
		if err != nil {
			problem = err.Error()
			return
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		cfg, err := s.runner.ResolvedConfig(cctx, t)
		if err != nil {
			problem = err.Error()
			return
		}
		svcs, err := serviceImages(cfg, t.Project)
		if err != nil {
			problem = err.Error()
			return
		}
		e, err := s.newEngine(hostFor(st, f.Meta.Engine))
		if err != nil {
			problem = err.Error()
			return
		}
		defer e.Close()
		ctrs, _ := e.ProjectContainers(cctx, t.Project)
		running := map[string]string{}
		for _, c := range ctrs {
			if running[c.Service] == "" {
				running[c.Service] = c.ImageID
			}
		}
		rows = make([]imageCheck, len(svcs))
		var wg sync.WaitGroup
		for i, sv := range svcs {
			rows[i] = imageCheck{Service: sv[0], Image: sv[1]}
			wg.Go(func() {
				row := &rows[i]
				// What the tag points at locally. With Docker's containerd
				// image store, the image a container runs is dropped from
				// the store once its tag moves, so the tag is the one
				// reliable thing to inspect.
				info, err := e.ImageInfo(cctx, sv[1])
				run := running[sv[0]]
				if err != nil && run != "" {
					info, err = e.ImageInfo(cctx, run)
				}
				if err != nil {
					row.Status, row.Detail = registry.StatusUnknown, "no local image yet"
					return
				}
				if run != "" && info.ID != run {
					row.Status, row.Detail = registry.StatusAvailable, "a newer image is already pulled but not running"
					return
				}
				if len(info.RepoDigests) == 0 {
					row.Status, row.Detail = registry.StatusLocal, "built here, nothing to compare"
					return
				}
				d, err := s.reg.Digest(cctx, sv[1])
				if err != nil {
					row.Status, row.Detail = registry.StatusUnknown, shortErr(err)
					return
				}
				row.Status = registry.Compare(sv[1], info.RepoDigests, d)
			})
		}
		wg.Wait()
	}()
	s.render(w, http.StatusOK, "partial_updates", map[string]any{"Rows": rows, "Problem": problem, "TTL": fmt.Sprintf("%d hours", int(registry.TTL.Hours()))})
}

func shortErr(err error) string {
	msg := err.Error()
	if len(msg) > 160 {
		msg = msg[:160] + "..."
	}
	return msg
}

// lastUpdateFor reads every stack's last update result for the stacks page.
func (s *Server) lastUpdates(ctx context.Context) map[string]store.Schedule {
	out := map[string]store.Schedule{}
	scheds, err := s.store.ListSchedules(ctx)
	if err != nil {
		return out
	}
	for _, sc := range scheds {
		out[sc.Folder] = sc
	}
	return out
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	fleetling "github.com/NullAngst/Fleetling"
	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/registry"
	"github.com/NullAngst/Fleetling/internal/release"
	"github.com/NullAngst/Fleetling/internal/store"
)

// The Updates page: Fleetling itself first, then every managed stack,
// then the containers Fleetling doesn't manage, read-only. Every network
// check on it (GitHub for Fleetling, registries for images) is loaded
// after the page opens, so the page itself is never slow.

// keyCheckGitHub turns the Fleetling version check on or off. Unset is on.
const keyCheckGitHub = "check_github"

func (s *Server) githubCheckOn(ctx context.Context) bool {
	v, ok, err := s.store.Get(ctx, keyCheckGitHub)
	return err != nil || !ok || v != "off"
}

// releaseLoop checks GitHub in the background, a minute after start and
// then every TTL, so the sidebar can say an update is waiting without a
// visit to the Updates page.
func (s *Server) releaseLoop() {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.base.Done():
			return
		case <-t.C:
		}
		if s.githubCheckOn(s.base) {
			ctx, cancel := context.WithTimeout(s.base, 30*time.Second)
			s.rel.Check(ctx, false, s.now())
			cancel()
		}
		t.Reset(release.TTL)
	}
}

// selfVersion is what the Fleetling box shows: the running version against
// what GitHub has.
type selfVersion struct {
	Running   string
	Built     time.Time // the running image's build time, zero outside a container
	Check     release.Status
	Checked   bool
	Enabled   bool
	OnMain    bool // a newer version on main
	NewCommit bool // main has commits newer than this build
	NewTag    bool // a newer release than this version
	Problem   string
	CSRF      string // the partial carries its own Check again form
}

// Available says whether the box should say "update available".
func (v selfVersion) Available() bool { return v.OnMain || v.NewCommit || v.NewTag }

func (s *Server) compareSelf(st release.Status, built time.Time) selfVersion {
	v := selfVersion{Running: s.cfg.Version, Check: st, Checked: !st.Checked.IsZero(), Built: built}
	cur, err := fleetling.Parse(s.cfg.Version)
	if err != nil {
		v.Problem = "this build's version isn't readable: " + err.Error()
		return v
	}
	if mv, err := fleetling.Parse(st.MainVersion); err == nil {
		v.OnMain = cur.Less(mv)
	}
	if rv, err := fleetling.Parse(st.Release); err == nil {
		v.NewTag = cur.Less(rv)
	}
	// Commits with no version bump, like the weekly dependency updates.
	// The image is built a little after the commit it holds, so a commit
	// only counts as newer past a small margin.
	if !built.IsZero() && !st.MainDate.IsZero() && !v.OnMain {
		v.NewCommit = st.MainDate.After(built.Add(2 * time.Minute))
	}
	return v
}

// updateBadge is the sidebar's "update waiting" mark, from the cached
// check only: no network, no engine.
func (s *Server) updateBadge() bool {
	st, ok := s.rel.Cached()
	if !ok {
		return false
	}
	cur, err1 := fleetling.Parse(s.cfg.Version)
	mv, err2 := fleetling.Parse(st.MainVersion)
	return err1 == nil && err2 == nil && cur.Less(mv)
}

type selfBox struct {
	InContainer bool
	Managed     bool   // Fleetling's own folder is a managed stack
	Folder      string // its folder under the root
	Sources     []string
	Command     string // the helper command the Update button runs
	Reason      string // why Update isn't offered
	Review      bool   // its files need review first
	Busy        bool
	Last        lastUpdateData
	Sched       updatesTab
}

type stackUpdateRow struct {
	Folder  string
	Engine  string
	Running int
	Total   int
	Review  string
	Sched   updatesTab
}

type otherContainer struct {
	Slot, ID, Name, Image, Project string
	Running                        bool
}

type updatesData struct {
	Self    selfBox
	Enabled bool
	Stacks  []stackUpdateRow
	Others  []otherContainer
	ScanErr string
	Engines []engineStatus
	TTL     string
}

func (s *Server) updatesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := updatesData{Enabled: s.githubCheckOn(ctx), TTL: fmt.Sprintf("%d hours", int(registry.TTL.Hours()))}
	d.Self.InContainer = s.inContainer()
	d.Self.Last = s.readLastUpdate()
	si, isSelf := s.self(ctx, st)

	folders, err := compose.Discover(st.Root, st.Ignore)
	if err != nil {
		d.ScanErr = err.Error()
	}
	containers, status := s.gather(ctx, st)
	d.Engines = status
	running, total := map[string]int{}, map[string]int{}
	for _, c := range containers {
		total[c.Engine+"/"+c.Project]++
		if c.Running {
			running[c.Engine+"/"+c.Project]++
		}
	}
	projects := map[string]bool{} // slot/project of every managed stack
	for _, f := range folders {
		if f.Meta == nil {
			if isSelf && si.is(f) {
				d.Self.Folder = f.Name // found, just not managed yet
			}
			continue
		}
		t, err := s.target(st, f)
		if err != nil {
			continue
		}
		key := f.Meta.Engine + "/" + t.Project
		projects[key] = true
		if isSelf && si.is(f) {
			d.Self.Managed, d.Self.Folder = true, f.Name
			d.Self.Sched = s.updatesTabData(ctx, st, f)
			s.fillSelfBox(ctx, st, f, si, &d.Self)
			continue
		}
		row := stackUpdateRow{Folder: f.Name, Engine: f.Meta.Engine, Running: running[key], Total: total[key], Sched: s.updatesTabData(ctx, st, f)}
		if rs, err := s.reviewFor(ctx, f); err == nil && rs.State != reviewOK {
			row.Review = rs.State
		}
		d.Stacks = append(d.Stacks, row)
	}
	if !d.Self.Managed {
		switch {
		case !d.Self.InContainer:
			d.Self.Reason = "Fleetling isn't running in a container here, so it can't rebuild itself. Download the new binary from the release page and restart it."
		case !isSelf:
			d.Self.Reason = "Fleetling can't find its own container's Compose labels, so it doesn't know which stack is its own. Run it with Compose from a folder under the stack root."
		case d.Self.Folder != "":
			d.Self.Reason = "Fleetling's own folder isn't managed yet. Open it and click Manage; then it can update itself from here."
		default:
			d.Self.Reason = "Fleetling's own folder isn't under the stack root, or the root is set to somewhere else, so it can't manage its own stack."
		}
	}

	// Containers that belong to no managed stack: standalone containers and
	// Compose projects run from somewhere else.
	for _, ep := range endpoints(st) {
		e, err := s.newEngine(ep.Host)
		if err != nil {
			continue
		}
		rows, err := e.Containers(ctx)
		e.Close()
		if err != nil {
			continue
		}
		for _, c := range rows {
			if c.Project != "" && projects[ep.Slot+"/"+c.Project] {
				continue
			}
			if isSelf && (c.ID == si.ContainerID || strings.HasPrefix(c.ID, si.ContainerID) || c.Name == si.ContainerID) {
				continue
			}
			d.Others = append(d.Others, otherContainer{Slot: ep.Slot, ID: c.ID, Name: c.Name, Image: c.Image, Project: c.Project, Running: c.State == "running"})
		}
	}
	sort.Slice(d.Others, func(i, j int) bool { return d.Others[i].Name < d.Others[j].Name })

	p := s.page(r, "Updates", "updates", d)
	switch r.URL.Query().Get("notice") {
	case "checked":
		p.Notice = "Checked GitHub just now."
	case "github-on":
		p.Notice = "Fleetling will check GitHub for new versions, at most every 6 hours."
	case "github-off":
		p.Notice = "Fleetling won't contact GitHub. Check the repository yourself for new versions."
	}
	s.render(w, http.StatusOK, "updates", p)
}

// fillSelfBox works out whether the Update button can run, and what
// Fleetling builds from.
func (s *Server) fillSelfBox(ctx context.Context, st store.Settings, f compose.Folder, si selfInfo, b *selfBox) {
	if rs, err := s.reviewFor(ctx, f); err == nil && rs.State != reviewOK {
		b.Review = true
	}
	b.Busy = s.jobs.Running(f.Name) != nil
	if cmd, err := s.selfCommand(ctx, st, f, si, "update"); err == nil {
		b.Command = cmd
	} else {
		b.Reason = err.Error()
	}
	if b.Review {
		return // the sources come from the files, which aren't trusted yet
	}
	t, err := s.target(st, f)
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if cfg, err := s.runner.ResolvedConfig(cctx, t); err == nil {
		b.Sources = buildSources(cfg)
	}
}

// buildSources lists where each service's image comes from: the build
// context for a built one, the image reference for a pulled one.
func buildSources(configJSON []byte) []string {
	var cfg struct {
		Services map[string]struct {
			Image string          `json:"image"`
			Build json.RawMessage `json:"build"`
		} `json:"services"`
	}
	if json.Unmarshal(configJSON, &cfg) != nil {
		return nil
	}
	var out []string
	for name, sv := range cfg.Services {
		var b struct {
			Context string `json:"context"`
		}
		if len(sv.Build) > 0 && string(sv.Build) != "null" && json.Unmarshal(sv.Build, &b) == nil && b.Context != "" {
			out = append(out, name+": built from "+b.Context)
		} else if sv.Image != "" {
			out = append(out, name+": pulls "+sv.Image)
		}
	}
	slices.Sort(out)
	return out
}

// updatesFleetling is the version partial. It asks GitHub, or answers
// from the cache while that is fresh.
func (s *Server) updatesFleetling(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var built time.Time
	if st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot); err == nil {
		if si, ok := s.self(ctx, st); ok {
			if id, err := s.selfImage(ctx, st, si); err == nil {
				if e, err := s.newEngine(hostFor(st, si.Slot)); err == nil {
					if info, err := e.ImageInfo(ctx, id); err == nil {
						built = info.Created
					}
					e.Close()
				}
			}
		}
	}
	var rs release.Status
	enabled := s.githubCheckOn(ctx)
	if enabled {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rs = s.rel.Check(cctx, false, s.now())
		cancel()
	}
	v := s.compareSelf(rs, built)
	v.Enabled = enabled
	v.CSRF = s.csrfToken(r)
	s.render(w, http.StatusOK, "partial_self_version", v)
}

// updatesCheckNow asks GitHub again, ignoring the cache.
func (s *Server) updatesCheckNow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if s.githubCheckOn(ctx) {
		s.rel.Check(ctx, true, s.now())
	}
	http.Redirect(w, r, "/updates?notice=checked", http.StatusSeeOther)
}

// updatesGitHub turns the GitHub check on or off.
func (s *Server) updatesGitHub(w http.ResponseWriter, r *http.Request) {
	on := r.PostFormValue("check") == "on"
	v, notice := "off", "github-off"
	if on {
		v, notice = "on", "github-on"
	}
	err := s.store.Set(r.Context(), keyCheckGitHub, v)
	s.logFileAction(r.Context(), "fleetling", "", "fleetling: check GitHub for new Fleetling versions "+v, err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/updates?notice="+notice, http.StatusSeeOther)
}

// updatesStack is one stack row's image summary partial.
func (s *Server) updatesStack(w http.ResponseWriter, r *http.Request) {
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
	rows, problem := s.stackImageChecks(ctx, st, f)
	sum := imageSummary{Problem: problem}
	for _, row := range rows {
		switch row.Status {
		case registry.StatusAvailable:
			sum.Available = append(sum.Available, row.Service)
		case registry.StatusCurrent:
			sum.Current++
		case registry.StatusLocal:
			sum.Local++
		default:
			sum.Unknown++
		}
	}
	s.render(w, http.StatusOK, "partial_update_summary", sum)
}

type imageSummary struct {
	Available []string
	Current   int
	Local     int
	Unknown   int
	Problem   string
	Detail    string // for a single container: why it's unknown or local
}

// updatesContainer is the check partial for a container no managed stack
// owns. Read-only: Fleetling didn't deploy it, so it doesn't update it.
func (s *Server) updatesContainer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slot := r.PathValue("slot")
	host := hostFor(st, slot)
	if host == "" {
		http.NotFound(w, r)
		return
	}
	e, err := s.newEngine(host)
	if err != nil {
		s.render(w, http.StatusOK, "partial_update_summary", imageSummary{Problem: err.Error()})
		return
	}
	defer e.Close()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	d, err := e.Inspect(cctx, r.PathValue("id"))
	if err != nil {
		s.render(w, http.StatusOK, "partial_update_summary", imageSummary{Problem: err.Error()})
		return
	}
	status, detail := s.imageStatus(cctx, e, d.Image, d.ImageID)
	sum := imageSummary{Detail: detail}
	switch status {
	case registry.StatusAvailable:
		sum.Available = []string{d.Name}
	case registry.StatusCurrent:
		sum.Current = 1
	case registry.StatusLocal:
		sum.Local = 1
	default:
		sum.Unknown = 1
	}
	s.render(w, http.StatusOK, "partial_update_summary", sum)
}

type updateAllRow struct {
	Folder string
	Job    string // job ID when one started
	Note   string
	Err    bool
}

// updatesAll runs Update now on every managed stack except Fleetling's
// own. Each one goes through the same checks as a scheduled run: the
// drift gate, nothing running means skip, and up -d only when an image
// changed. So it is safe to press with nothing to update.
func (s *Server) updatesAll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	folders, err := compose.Discover(st.Root, st.Ignore)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	si, isSelf := s.self(ctx, st)
	var out []updateAllRow
	for _, f := range folders {
		if f.Meta == nil || (isSelf && si.is(f)) {
			continue
		}
		row := updateAllRow{Folder: f.Name}
		job, err := s.autoUpdate(s.base, st, f, "manual")
		switch {
		case errors.Is(err, errSkipped):
			row.Note = strings.TrimPrefix(err.Error(), errSkipped.Error()+": ")
		case err != nil:
			row.Note, row.Err = err.Error(), true
		case job != nil:
			row.Job, row.Note = job.ID, "pulling; up -d only if an image changed"
		}
		out = append(out, row)
	}
	s.render(w, http.StatusOK, "updates_all", s.page(r, "Update all stacks", "updates", out))
}

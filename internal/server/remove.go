package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
)

// Remove with folders. Removing a stack runs `down`, optionally with -v and
// --rmi all, and then deletes the folders the user ticked. Removing one
// container does the same with its own bind mounts. Every path goes
// through compose.Guard twice: once to build the list, and again right
// before it is deleted, against a reference index rebuilt after `down`.

// pathRow is one path on the remove page.
type pathRow struct {
	compose.Candidate
	Note string
}

// Size is the human size for the page and the log.
func (p pathRow) SizeText() string { return sizeText(p.Candidate) }

func sizeText(c compose.Candidate) string {
	if c.Link {
		return "symlink"
	}
	s := humanBytes(uint64(max(c.Size, 0)))
	if c.Dir {
		s += ", " + plural(c.Files, "item")
	}
	if c.Partial {
		s = "at least " + s
	}
	return s
}

type volumeRow struct {
	Name      string
	Exists    bool
	Anonymous bool
	Others    []string // containers outside this stack that use it
}

type removeData struct {
	Kind     string // "stack" or "container"
	Name     string // typed to confirm: the project or container name
	Action   string // form action
	Back     string
	Folder   compose.Folder
	Slot, ID string
	Root     string
	BaseCmd  string // shown with -v and --rmi all added as ticked
	Volumes  []volumeRow
	VolErr   string
	Images   []string
	Created  []pathRow
	Binds    []pathRow
	StackDir []pathRow // the stack folder itself, when removing a stack
	Outside  []string  // bind sources outside the root: never offered
	Blocked  string    // why no folder can be ticked on this page
	Refusal  string    // why this can't be removed at all
	Review   string    // drift state of the stack, if not clean
	Picked   map[string]bool
	Vols     bool
	Imgs     bool
}

// offered returns the paths on the page that can be ticked.
func (d *removeData) offered() map[string]bool {
	out := map[string]bool{}
	if d.Blocked != "" {
		return out
	}
	for _, rows := range [][]pathRow{d.Created, d.Binds, d.StackDir} {
		for _, r := range rows {
			if r.OK() {
				out[r.Path] = true
			}
		}
	}
	return out
}

// refScope is what a removal is for, so its own references don't count
// against it.
type refScope struct {
	stackDir    string // the stack folder being removed, or the removed container's stack
	project     string // stack removal: its containers don't count
	slot        string
	service     string // container removal: only this service's lines in its own stack don't count
	containerID string
}

// referenceIndex lists every host path something else still uses: other
// stack folders, every path their compose files name (resolved by Compose,
// plus the file as written), and every mount of every other container on
// every engine. The second result lists what couldn't be checked; while it
// is non-empty, no folder is deleted.
func (s *Server) referenceIndex(ctx context.Context, st store.Settings, sc refScope) ([]compose.Ref, []string) {
	folders, err := compose.Discover(st.Root, st.Ignore)
	if err != nil {
		return nil, []string{"could not list " + st.Root + ": " + err.Error()}
	}
	type result struct {
		refs  []compose.Ref
		notes []string
	}
	results := make([]result, len(folders))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, f := range folders {
		own := f.Dir == sc.stackDir
		if own && sc.service == "" {
			continue // the stack being removed
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i].refs, results[i].notes = s.folderRefs(ctx, st, f, own, sc.service)
		})
	}
	wg.Wait()
	var refs []compose.Ref
	var unchecked []string
	for _, r := range results {
		refs = append(refs, r.refs...)
		unchecked = append(unchecked, r.notes...)
	}

	for _, ep := range endpoints(st) {
		rows, err := s.listContainers(ctx, ep.Host)
		if errors.Is(err, engine.ErrSocketMissing) {
			continue // no engine there, so no containers either
		}
		if err != nil {
			unchecked = append(unchecked, fmt.Sprintf("%s at %s did not answer, so its containers could not be checked: %v. Turn the endpoint off in settings if it is not in use", ep.Slot, ep.Host, err))
			continue
		}
		for _, c := range rows {
			if c.ID == sc.containerID {
				continue
			}
			if sc.service == "" && sc.project != "" && c.Project == sc.project && ep.Slot == sc.slot {
				continue // the stack's own containers; down removes them first
			}
			for _, m := range c.Mounts {
				if m.Source != "" && filepath.IsAbs(m.Source) {
					refs = append(refs, compose.Ref{Path: m.Source, Owner: "container " + c.Name})
				}
			}
		}
	}
	return refs, unchecked
}

func (s *Server) listContainers(ctx context.Context, host string) ([]engine.ContainerRow, error) {
	e, err := s.newEngine(host)
	if err != nil {
		return nil, err
	}
	defer e.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return e.Containers(ctx)
}

// folderRefs reads one stack folder's references. own is set when the
// folder is the removed container's own stack: its folder and the
// container's own service don't count, its other services do.
func (s *Server) folderRefs(ctx context.Context, st store.Settings, f compose.Folder, own bool, service string) ([]compose.Ref, []string) {
	var refs []compose.Ref
	if !own {
		refs = append(refs, compose.Ref{Path: f.Dir, Owner: "the folder of stack " + f.Project})
	}
	if f.ComposeFile == "" {
		return refs, nil
	}
	keep := func(r compose.Reference) {
		if own && r.Service == service && service != "" {
			return
		}
		owner := "stack " + f.Project + " (" + r.What
		if r.Service != "" {
			owner += " of " + r.Service
		}
		refs = append(refs, compose.Ref{Path: r.Path, Owner: owner + ")"})
	}

	// What Compose resolves: variables filled in, relative paths made
	// absolute. The engine doesn't matter for `config`.
	host := hostFor(st, compose.EngineDocker)
	if f.Meta != nil {
		host = hostFor(st, f.Meta.Engine)
	}
	h, _, _ := engine.NormalizeHost(host)
	t := compose.Target{Project: f.Project, Dir: f.Dir, File: f.ComposeFile, Host: h}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	cfg, cfgErr := s.runner.ResolvedConfig(cctx, t)
	cancel()
	if cfgErr == nil {
		if rs, err := compose.ConfigReferences(cfg); err == nil {
			for _, r := range rs {
				keep(r)
			}
		} else {
			cfgErr = err
		}
	}

	// The file as written: include and extends files, and everything
	// again in case Compose couldn't read it.
	var notes []string
	text, err := os.ReadFile(filepath.Join(f.Dir, f.ComposeFile))
	if err != nil {
		return refs, []string{"could not read " + filepath.Join(f.Dir, f.ComposeFile) + ": " + err.Error()}
	}
	static, unresolved := compose.StaticReferences(text, f.Dir)
	for _, r := range static {
		if cfgErr != nil && r.Partial && (filepath.Clean(r.Path) == filepath.Clean(st.Root) || compose.Nested(st.Root, r.Path)) {
			unresolved = append(unresolved, r.Path+"/...")
			continue
		}
		keep(r)
	}
	if cfgErr != nil && len(unresolved) > 0 {
		notes = append(notes, fmt.Sprintf("Compose could not read %s (%v), and without it %s can't be placed",
			filepath.Join(f.Dir, f.ComposeFile), firstLine(cfgErr.Error()), strings.Join(unresolved, ", ")))
	}
	return refs, notes
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// guard builds the deletion guard with Fleetling's own data folder and the
// ignore list protected.
func (s *Server) guard(st store.Settings, refs []compose.Ref) (*compose.Guard, error) {
	var protected []compose.Ref
	if s.cfg.DataDir != "" {
		// FLEETLING_DATA may be relative; the rules compare absolute paths.
		data, err := filepath.Abs(s.cfg.DataDir)
		if err != nil {
			return nil, err
		}
		protected = append(protected, compose.Ref{Path: data, Owner: "holds Fleetling's own data (" + data + ")"})
	}
	for _, name := range st.Ignore {
		if compose.ValidFolderName(name) {
			protected = append(protected, compose.Ref{Path: filepath.Join(st.Root, name), Owner: "on the ignore list in settings"})
		}
	}
	return compose.NewGuard(compose.GuardOptions{Root: st.Root, Refs: refs, Protected: protected, MountInfo: s.mountInfo, DevOf: s.devOf})
}

// approvedMeta reads .fleetling.toml as it was last approved, not as it is
// on disk now. created_paths decides what may be deleted, and a container
// that mounts its stack folder can write that file.
func (s *Server) approvedMeta(ctx context.Context, f compose.Folder) (*compose.Meta, error) {
	b, ok, err := s.store.GetBaseline(ctx, f.Name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errNeedsReview
	}
	var snap compose.Snapshot
	if err := json.Unmarshal([]byte(b.Snapshot), &snap); err != nil {
		return nil, err
	}
	for _, fs := range snap.Files {
		if fs.Path == compose.MetaFile && fs.Exists && !fs.TooLarge {
			return compose.ParseMeta([]byte(fs.Content))
		}
	}
	return nil, fmt.Errorf("the approved files of %s have no %s", f.Name, compose.MetaFile)
}

// stackRemoveData builds the remove page for a managed stack.
func (s *Server) stackRemoveData(ctx context.Context, st store.Settings, f compose.Folder) (*removeData, compose.Target) {
	d := &removeData{Kind: "stack", Folder: f, Root: st.Root, Action: "/stacks/" + f.Name + "/remove", Back: "/stacks/" + f.Name, Picked: map[string]bool{}}
	if f.Meta == nil {
		d.Name = f.Project
		d.Refusal = "This folder isn't managed yet. Manage it first; until then Fleetling doesn't run anything here."
		return d, compose.Target{}
	}
	d.Name, d.Slot = f.Meta.Project, f.Meta.Engine
	if si, ok := s.self(ctx, st); ok && si.is(f) {
		d.Refusal = "This is Fleetling's own stack. Removing it from inside would cut Fleetling off halfway through."
		return d, compose.Target{}
	}
	t, err := s.target(st, f)
	if err != nil {
		d.Refusal = err.Error()
		return d, t
	}
	if rs, err := s.reviewFor(ctx, f); err != nil {
		d.Refusal = "Could not check the stack's files: " + err.Error()
		return d, t
	} else if rs.State != reviewOK {
		// down -v with a rewritten compose file could take another
		// project's volumes, and created_paths comes from a file a
		// container may have written. Review first, like every action.
		d.Review = rs.State
		return d, t
	}
	d.BaseCmd = compose.Quote(s.runner.Args(t, "down"))

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	cfg, err := s.runner.ResolvedConfig(cctx, t)
	cancel()
	if err != nil {
		d.Refusal = "Compose could not read this stack's file, so down would fail too: " + err.Error()
		return d, t
	}
	meta, err := s.approvedMeta(ctx, f)
	if err != nil {
		d.Refusal = "Could not read the approved " + compose.MetaFile + ": " + err.Error()
		return d, t
	}

	// Named and anonymous volumes, as down -v would see them.
	if vols, err := compose.ConfigVolumes(cfg, t.Project); err == nil {
		s.fillVolumes(ctx, st, d, vols, t.Project)
	}
	d.Images, _ = compose.ConfigImages(cfg, t.Project)

	binds, _ := compose.BindSources(cfg)
	services := map[string][]string{}
	if refs, err := compose.ConfigReferences(cfg); err == nil {
		for _, r := range refs {
			if r.What == "bind mount" && !slices.Contains(services[r.Path], r.Service) {
				services[r.Path] = append(services[r.Path], r.Service)
			}
		}
	}
	// created_paths is checked like any other path: it only says Docker
	// made these, not that they are safe to delete.
	var createdClean []string
	for _, p := range meta.CreatedPaths {
		if filepath.IsAbs(p) {
			createdClean = append(createdClean, filepath.Clean(p))
		}
	}
	created, createdOut := compose.SplitByRoot(createdClean, st.Root)
	under, outside := compose.SplitByRoot(binds, st.Root)
	d.Outside = mergeSorted(outside, createdOut)

	refs, unchecked := s.referenceIndex(ctx, st, refScope{stackDir: f.Dir, project: t.Project, slot: f.Meta.Engine})
	g, err := s.guard(st, refs)
	if err != nil {
		d.Blocked = err.Error()
		return d, t
	}
	if len(unchecked) > 0 {
		d.Blocked = "Folder deletion is off, since not every path could be checked: " + strings.Join(unchecked, "; ") + "."
	}
	for _, p := range created {
		if filepath.Clean(p) == f.Dir {
			continue
		}
		note := "Docker made it on the first deploy"
		if svc := services[filepath.Clean(p)]; len(svc) > 0 {
			note += ", bind mount of " + strings.Join(svc, ", ")
		}
		d.Created = append(d.Created, pathRow{Candidate: g.Check(p), Note: note})
	}
	for _, p := range under {
		if p == f.Dir || slices.Contains(createdClean, p) {
			continue
		}
		d.Binds = append(d.Binds, pathRow{Candidate: g.Check(p), Note: "bind mount of " + strings.Join(services[p], ", ")})
	}
	d.StackDir = []pathRow{{Candidate: g.Check(f.Dir), Note: f.ComposeFile + ", .env, " + compose.MetaFile + " and everything else in it"}}
	return d, t
}

func mergeSorted(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	slices.Sort(out)
	return out
}

var anonVolume = regexp.MustCompile(`^[0-9a-f]{64}$`)

// fillVolumes lists what down -v removes: the declared named volumes that
// aren't external, plus anonymous volumes on the stack's containers.
func (s *Server) fillVolumes(ctx context.Context, st store.Settings, d *removeData, declared []compose.ConfigVolume, project string) {
	e, err := s.newEngine(hostFor(st, d.Slot))
	if err != nil {
		d.VolErr = err.Error()
		return
	}
	defer e.Close()
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	have, err := e.Volumes(lctx)
	if err != nil {
		d.VolErr = err.Error()
	}
	ctrs, _ := e.Containers(lctx)
	mine := map[string]bool{}
	for _, c := range ctrs {
		if c.Project == project {
			mine[c.Name] = true
		}
	}
	byName := map[string]engine.VolumeRow{}
	for _, v := range have {
		byName[v.Name] = v
	}
	add := func(name string, anon bool) {
		for _, r := range d.Volumes {
			if r.Name == name {
				return
			}
		}
		v, ok := byName[name]
		row := volumeRow{Name: name, Exists: ok, Anonymous: anon || v.Anonymous}
		for _, u := range v.UsedBy {
			if !mine[u] {
				row.Others = append(row.Others, u)
			}
		}
		d.Volumes = append(d.Volumes, row)
	}
	for _, v := range declared {
		if !v.External {
			add(v.Name, false)
		}
	}
	for _, c := range ctrs {
		if c.Project != project {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == "volume" && anonVolume.MatchString(m.Name) {
				add(m.Name, true)
			}
		}
	}
}

func (s *Server) stackRemovePage(w http.ResponseWriter, r *http.Request) {
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
	d, _ := s.stackRemoveData(ctx, st, f)
	s.renderRemove(w, r, http.StatusOK, d, removeForm{}, "")
}

// removeForm is what the remove page posts.
type removeForm struct {
	volumes, images bool
	paths           []string
	confirm         string
}

func parseRemoveForm(r *http.Request) removeForm {
	r.ParseForm()
	f := removeForm{volumes: r.PostFormValue("volumes") == "1", images: r.PostFormValue("images") == "1", confirm: strings.TrimSpace(r.PostFormValue("confirm"))}
	for _, p := range r.PostForm["path"] {
		if p != "" && !slices.Contains(f.paths, p) {
			f.paths = append(f.paths, p)
		}
	}
	return f
}

// check validates a posted form against the page as rebuilt now. Only
// paths the page offers right now can be deleted, whatever was posted.
func (rf removeForm) check(d *removeData) error {
	if d.Blocked != "" && len(rf.paths) > 0 {
		return errors.New(d.Blocked)
	}
	offered := d.offered()
	for _, p := range rf.paths {
		if !offered[p] {
			return fmt.Errorf("%s is not on the list of paths this can delete right now. Reload the page and check the list again", p)
		}
	}
	if (len(rf.paths) > 0 || rf.volumes) && rf.confirm != d.Name {
		return fmt.Errorf("type %s to confirm deleting volumes or folders", d.Name)
	}
	return nil
}

func (s *Server) renderRemove(w http.ResponseWriter, r *http.Request, status int, d *removeData, rf removeForm, msg string) {
	for _, p := range rf.paths {
		d.Picked[p] = true
	}
	d.Vols, d.Imgs = rf.volumes, rf.images
	p := s.page(r, "Remove "+d.Name, "stacks", d)
	if d.Kind == "container" {
		p.Active = "containers"
	}
	p.Error = msg
	s.render(w, status, "remove", p)
}

func (s *Server) stackRemove(w http.ResponseWriter, r *http.Request) {
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
	rf := parseRemoveForm(r)
	if s.jobs.Running(f.Name) != nil {
		http.Error(w, compose.ErrBusy.Error(), http.StatusConflict)
		return
	}
	d, t := s.stackRemoveData(ctx, st, f)
	switch {
	case d.Review != "":
		http.Redirect(w, r, "/stacks/"+f.Name+"/review?action=remove", http.StatusSeeOther)
		return
	case d.Refusal != "":
		s.renderRemove(w, r, http.StatusConflict, d, rf, "")
		return
	}
	if err := rf.check(d); err != nil {
		s.renderRemove(w, r, http.StatusBadRequest, d, rf, err.Error())
		return
	}

	down := []string{"down"}
	if rf.volumes {
		down = append(down, "-v")
	}
	if rf.images {
		down = append(down, "--rmi", "all")
	}
	steps := []compose.Step{{Cmd: s.runner.Command(s.base, t, down...)}}
	if len(rf.paths) > 0 {
		steps = append(steps, s.deleteStep(st, f.Name, d.Slot, refScope{stackDir: f.Dir, project: t.Project, slot: d.Slot}, orderPaths(rf.paths, f.Dir), f))
	}
	job, err := s.startLogged(ctx, compose.JobSpec{Target: f.Name, Engine: d.Slot, Label: "Remove " + d.Name, Back: "/", Steps: steps})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/jobs/"+job.ID, http.StatusSeeOther)
}

// orderPaths deletes the deepest paths first and the stack folder last, so
// every path the user ticked gets its own line in the log.
func orderPaths(paths []string, stackDir string) []string {
	out := slices.Clone(paths)
	slices.SortStableFunc(out, func(a, b string) int {
		da, db := strings.Count(filepath.Clean(a), "/"), strings.Count(filepath.Clean(b), "/")
		if da != db {
			return db - da
		}
		return strings.Compare(a, b)
	})
	if i := slices.Index(out, stackDir); i >= 0 {
		out = append(slices.Delete(out, i, i+1), stackDir)
	}
	return out
}

// deleteStep deletes paths after the step before it succeeded. It rebuilds
// the reference index first: down has removed the stack's containers, and
// anything that started using these paths since the page loaded counts.
func (s *Server) deleteStep(st store.Settings, target, slot string, sc refScope, paths []string, f compose.Folder) compose.Step {
	return compose.Step{
		Show: "fleetling: delete " + strings.Join(paths, " "),
		Run: func(ctx context.Context, j *compose.Job) int {
			j.Linef("fleetling: checking %s again before deleting", plural(len(paths), "path"))
			refs, unchecked := s.referenceIndex(ctx, st, sc)
			if len(unchecked) > 0 {
				j.Linef("fleetling: deleting nothing, since not every path could be checked: %s", strings.Join(unchecked, "; "))
				return 1
			}
			g, err := s.guard(st, refs)
			if err != nil {
				j.Linef("fleetling: deleting nothing: %v", err)
				return 1
			}
			failed := 0
			for _, p := range paths {
				c, err := g.Delete(p)
				if err != nil {
					failed++
					j.Linef("fleetling: kept %s: %v", p, err)
					s.logFileAction(context.Background(), target, slot, "fleetling: delete "+p, err)
					continue
				}
				j.Linef("fleetling: deleted %s (%s)", p, sizeText(c))
				s.logFileAction(context.Background(), target, slot, "fleetling: delete "+p+" ("+sizeText(c)+")", nil)
				if f.Dir != "" && filepath.Clean(p) == f.Dir {
					if err := s.store.DeleteBaseline(context.Background(), f.Name); err != nil {
						j.Linef("fleetling: could not forget the approved files of %s: %v", f.Name, err)
					}
				}
			}
			if failed > 0 {
				j.Linef("fleetling: kept %d of %s", failed, plural(len(paths), "path"))
				return 1
			}
			return 0
		},
	}
}

// startLogged starts a job and records it in the action log, finishing
// the entry with the exit code and the last 200 lines.
func (s *Server) startLogged(ctx context.Context, spec compose.JobSpec) (*compose.Job, error) {
	var lines []string
	for _, st := range spec.Steps {
		if st.Cmd != nil {
			lines = append(lines, compose.Quote(append([]string{"docker"}, st.Cmd.Args[1:]...)))
		} else {
			lines = append(lines, st.Show)
		}
	}
	id, err := s.store.StartAction(ctx, store.Action{Started: s.now(), Target: spec.Target, Engine: spec.Engine, Command: strings.Join(lines, "\n")})
	if err != nil {
		return nil, err
	}
	spec.Done = func(j *compose.Job) {
		_, exit, fin := j.Result()
		if err := s.store.FinishAction(context.Background(), id, fin, exit, strings.Join(j.Tail(200), "\n")); err != nil {
			s.log.Error("action log", "err", err)
		}
	}
	job, err := s.jobs.Start(spec)
	if err != nil {
		s.store.FinishAction(ctx, id, s.now(), -1, "fleetling: "+err.Error())
		return nil, err
	}
	s.log.Info("action", "target", spec.Target, "action", spec.Label, "job", job.ID)
	return job, nil
}

// --- One container ---

func (s *Server) containerRemoveData(ctx context.Context, st store.Settings, slot string, c engine.Detail) *removeData {
	d := &removeData{Kind: "container", Name: c.Name, Slot: slot, ID: c.ID, Root: st.Root, Picked: map[string]bool{},
		Action: "/containers/" + slot + "/" + c.ID + "/remove", Back: "/containers/" + slot + "/" + c.ID,
		BaseCmd: compose.Quote([]string{"docker", "rm", "-f", c.Name})}
	if s.isSelfContainer(c.ID) {
		d.Refusal = "This is Fleetling's own container. Removing it from inside would cut Fleetling off halfway through."
		return d
	}
	var binds []string
	for _, m := range c.Mounts {
		if m.Type == "bind" && m.Source != "" && !slices.Contains(binds, filepath.Clean(m.Source)) {
			binds = append(binds, filepath.Clean(m.Source))
		}
	}
	under, outside := compose.SplitByRoot(binds, st.Root)
	d.Outside = outside

	sc := refScope{service: c.Service, containerID: c.ID}
	var created []string
	if f, ok := stackFolderFor(st, slot, c); ok {
		sc.stackDir = f.Dir
		d.Folder = f
		if c.Service == "" {
			sc.service = "\x00" // no service label: nothing in its stack is its own
		}
		if f.Meta != nil {
			if m, err := s.approvedMeta(ctx, f); err == nil {
				created = m.CreatedPaths
			}
		}
	}
	refs, unchecked := s.referenceIndex(ctx, st, sc)
	g, err := s.guard(st, refs)
	if err != nil {
		d.Blocked = err.Error()
		return d
	}
	if len(unchecked) > 0 {
		d.Blocked = "Folder deletion is off, since not every path could be checked: " + strings.Join(unchecked, "; ") + "."
	}
	for _, p := range under {
		note := "bind mount"
		if slices.Contains(created, p) {
			note = "bind mount, made by Docker on the stack's first deploy"
		}
		d.Binds = append(d.Binds, pathRow{Candidate: g.Check(p), Note: note})
	}
	return d
}

func (s *Server) containerRemovePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slot := r.PathValue("slot")
	e, st, err := s.engineFor(ctx, slot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	c, err := e.Inspect(ctx, r.PathValue("id"))
	e.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	d := s.containerRemoveData(ctx, st, slot, c)
	s.renderRemove(w, r, http.StatusOK, d, removeForm{}, "")
}

func (s *Server) containerRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slot := r.PathValue("slot")
	e, st, err := s.engineFor(ctx, slot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	c, err := e.Inspect(ctx, r.PathValue("id"))
	e.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	rf := parseRemoveForm(r)
	rf.volumes = false // a lone container has no volume option
	d := s.containerRemoveData(ctx, st, slot, c)
	if d.Refusal != "" {
		s.renderRemove(w, r, http.StatusConflict, d, rf, "")
		return
	}
	if err := rf.check(d); err != nil {
		s.renderRemove(w, r, http.StatusBadRequest, d, rf, err.Error())
		return
	}
	target := "container/" + c.Name
	host := hostFor(st, slot)
	id, name := c.ID, c.Name
	steps := []compose.Step{{
		Show: d.BaseCmd,
		Run: func(ctx context.Context, j *compose.Job) int {
			j.Line("$ " + d.BaseCmd)
			e, err := s.newEngine(host)
			if err == nil {
				actx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				err = e.Remove(actx, id)
				cancel()
				e.Close()
			}
			if err != nil {
				j.Linef("fleetling: %v", err)
				return 1
			}
			j.Linef("fleetling: removed container %s", name)
			return 0
		},
	}}
	if len(rf.paths) > 0 {
		sc := refScope{service: c.Service, containerID: c.ID, stackDir: d.Folder.Dir}
		if sc.stackDir != "" && c.Service == "" {
			sc.service = "\x00"
		}
		steps = append(steps, s.deleteStep(st, target, slot, sc, orderPaths(rf.paths, ""), compose.Folder{}))
	}
	job, err := s.startLogged(ctx, compose.JobSpec{Target: target, Engine: slot, Label: "Remove container " + c.Name, Back: "/containers?engine=" + slot, Steps: steps})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/jobs/"+job.ID, http.StatusSeeOther)
}

// --- The job page ---

type jobData struct {
	Job     *compose.Job
	Command []string
}

// jobPage shows one job's live output on its own page, for jobs whose
// stack may be gone by the time they finish.
func (s *Server) jobPage(w http.ResponseWriter, r *http.Request) {
	j := s.jobs.Get(r.PathValue("id"))
	if j == nil {
		http.Error(w, "that job is no longer in memory; its result is in the action log", http.StatusNotFound)
		return
	}
	title := j.Label
	if title == "" {
		title = j.Target
	}
	d := jobData{Job: j, Command: strings.Split(j.Command, "\n")}
	active := "stacks"
	if strings.HasPrefix(j.Target, "container/") {
		active = "containers"
	}
	s.render(w, http.StatusOK, "job", s.page(r, title, active, d))
}

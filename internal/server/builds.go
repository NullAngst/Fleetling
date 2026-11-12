package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/distribution/reference"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/portainer"
	"github.com/NullAngst/Fleetling/internal/store"
)

// Builds. Two kinds: a stack's own `docker compose build`, and standalone
// `docker build` runs from a folder under the root or a git URL. Both run
// as jobs with live output, and both land in the action log.

// buildSpec is the standalone build form, and what a recipe stores.
type buildSpec struct {
	Slot       string      `json:"slot"`
	Context    string      `json:"context"`    // a folder under the root, or a git URL
	Dockerfile string      `json:"dockerfile"` // relative to the context; empty for Dockerfile
	Tags       []string    `json:"tags"`
	Args       [][2]string `json:"args"`
	Target     string      `json:"target"`
	Platform   string      `json:"platform"`
	NoCache    bool        `json:"no_cache"`
	Pull       bool        `json:"pull"`
}

var (
	buildArgKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	stageRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	platformRe    = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9_]+(/[a-z0-9]+)?(,[a-z0-9]+/[a-z0-9_]+(/[a-z0-9]+)?)*$`)
	gitSSHRe      = regexp.MustCompile(`^git@[A-Za-z0-9.-]+:[A-Za-z0-9._~/-]+(\.git)?(#[A-Za-z0-9._/:-]*)?$`)
	recipeNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)
)

// gitContext reports whether ctx is a git URL Fleetling accepts: https://
// or git@host:path. file:// and every other scheme are refused, since the
// builder would read whatever local path they name.
func gitContext(ctx string) (bool, error) {
	switch {
	case strings.HasPrefix(ctx, "https://"):
		u, err := url.Parse(ctx)
		if err != nil || u.Host == "" || strings.ContainsAny(ctx, " \t\r\n") {
			return true, errors.New("that https:// URL doesn't parse")
		}
		return true, nil
	case strings.HasPrefix(ctx, "git@"):
		if !gitSSHRe.MatchString(ctx) {
			return true, errors.New("a git@ URL looks like git@github.com:owner/repo.git")
		}
		return true, nil
	case strings.Contains(ctx, "://"):
		return true, errors.New("only https:// and git@ URLs are allowed as a build context")
	}
	return false, nil
}

// redact strips credentials from a git URL for display and the log.
func redact(ctx string) string {
	if strings.HasPrefix(ctx, "https://") {
		return portainer.RedactURL(ctx)
	}
	return ctx
}

// resolved is a validated build ready to run.
type resolved struct {
	Spec   buildSpec
	Dir    string // the context folder, empty for git
	Git    bool
	Argv   []string // what runs
	Shown  string   // what the page and the log show: Argv with credentials stripped
	Secret string   // the user:password part of the URL, to scrub from the output
}

// resolve checks every field and builds the argv.
func (b buildSpec) resolve(st store.Settings) (resolved, error) {
	r := resolved{Spec: b}
	if hostFor(st, b.Slot) == "" {
		return r, fmt.Errorf("the %q engine is not configured", b.Slot)
	}
	b.Context = strings.TrimSpace(b.Context)
	if b.Context == "" {
		return r, errors.New("pick a context: a folder under " + st.Root + " or a git URL")
	}
	git, err := gitContext(b.Context)
	if err != nil {
		return r, err
	}
	r.Git = git
	ctxArg := b.Context
	if !git {
		p := b.Context
		if !filepath.IsAbs(p) {
			p = filepath.Join(st.Root, p)
		}
		p = filepath.Clean(p)
		if !isUnderDir(p, st.Root) || p == filepath.Clean(st.Root) {
			return r, fmt.Errorf("the context must be a folder under %s", st.Root)
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return r, fmt.Errorf("context: %w", err)
		}
		realRoot, err := filepath.EvalSymlinks(st.Root)
		if err != nil {
			return r, err
		}
		if !isUnderDir(real, realRoot) {
			return r, fmt.Errorf("the context resolves outside %s", st.Root)
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
			return r, fmt.Errorf("%s is not a folder", p)
		}
		r.Dir, ctxArg = real, real
	} else if u, err := url.Parse(b.Context); err == nil && u.User != nil {
		r.Secret = u.User.String()
	}

	df := strings.TrimSpace(b.Dockerfile)
	if df != "" {
		clean := filepath.Clean(df)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "-") {
			return r, errors.New("the Dockerfile path must be relative to the context and stay inside it")
		}
		if !git {
			df = filepath.Join(r.Dir, clean)
			// A symlinked Dockerfile must still point inside the context:
			// the CLI follows it and would read whatever it names.
			realDF, err := filepath.EvalSymlinks(df)
			if err != nil {
				return r, fmt.Errorf("no Dockerfile at %s", df)
			}
			if !isUnderDir(realDF, r.Dir) {
				return r, errors.New("the Dockerfile resolves outside the context")
			}
			if fi, err := os.Stat(realDF); err != nil || !fi.Mode().IsRegular() {
				return r, fmt.Errorf("no Dockerfile at %s", df)
			}
		} else {
			df = clean
		}
	} else if !git {
		if _, err := os.Stat(filepath.Join(r.Dir, "Dockerfile")); err != nil {
			return r, fmt.Errorf("no Dockerfile in %s; set the path", r.Dir)
		}
	}

	var tags []string
	for _, t := range b.Tags {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if _, err := reference.ParseNormalizedNamed(t); err != nil {
			return r, fmt.Errorf("tag %q: %v", t, err)
		}
		tags = append(tags, t)
	}
	if len(tags) == 0 {
		return r, errors.New("give the image at least one tag, like myapp:latest")
	}
	if len(tags) > 10 {
		return r, errors.New("ten tags at most")
	}
	var args [][2]string
	for _, a := range b.Args {
		if a[0] == "" && a[1] == "" {
			continue
		}
		if !buildArgKeyRe.MatchString(a[0]) {
			return r, fmt.Errorf("build arg name %q: letters, digits and _, not starting with a digit", a[0])
		}
		if strings.ContainsRune(a[1], 0) {
			return r, fmt.Errorf("build arg %s holds a NUL byte", a[0])
		}
		args = append(args, a)
	}
	if b.Target != "" && !stageRe.MatchString(b.Target) {
		return r, errors.New("the target stage is a name like build or runtime")
	}
	if b.Platform != "" && !platformRe.MatchString(b.Platform) {
		return r, errors.New("platform looks like linux/amd64 or linux/amd64,linux/arm64")
	}
	r.Spec.Tags, r.Spec.Args = tags, args

	argv := []string{"docker", "build"}
	if b.Slot != compose.EnginePodman {
		// The classic builder Podman's compat API runs (BuildKit off) has
		// no --progress flag.
		argv = append(argv, "--progress", "plain")
	}
	if df != "" {
		argv = append(argv, "-f", df)
	}
	for _, t := range tags {
		argv = append(argv, "-t", t)
	}
	for _, a := range args {
		argv = append(argv, "--build-arg", a[0]+"="+a[1])
	}
	if b.Target != "" {
		argv = append(argv, "--target", b.Target)
	}
	if b.Platform != "" {
		argv = append(argv, "--platform", b.Platform)
	}
	if b.NoCache {
		argv = append(argv, "--no-cache")
	}
	if b.Pull {
		argv = append(argv, "--pull")
	}
	argv = append(argv, "--", ctxArg)
	r.Argv = argv
	shown := slices.Clone(argv)
	shown[len(shown)-1] = redact(ctxArg)
	r.Shown = compose.Quote(shown)
	return r, nil
}

// startBuild runs a resolved build as a job. key names it in the job lock
// and the action log, e.g. build/myapp.
func (s *Server) startBuild(ctx context.Context, st store.Settings, r resolved, key string) (*compose.Job, error) {
	host, _, err := engine.NormalizeHost(hostFor(st, r.Spec.Slot))
	if err != nil {
		return nil, err
	}
	if s.jobs.Running(key) != nil {
		return nil, compose.ErrBusy
	}
	cmd := s.runner.Plain(s.base, host, r.Spec.Slot == compose.EnginePodman, r.Argv[1:]...)
	if r.Dir != "" {
		cmd.Dir = r.Dir
	}
	id, err := s.store.StartAction(ctx, store.Action{Started: s.now(), Target: key, Engine: r.Spec.Slot, Command: r.Shown})
	if err != nil {
		return nil, err
	}
	scrub := func(lines []string) []string {
		if r.Secret == "" {
			return lines
		}
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = strings.ReplaceAll(l, r.Secret, "***")
		}
		return out
	}
	job, err := s.jobs.Start(compose.JobSpec{
		Target: key, Engine: r.Spec.Slot, Label: "Build " + strings.Join(r.Spec.Tags, ", "), Back: "/builds",
		Steps: []compose.Step{{Cmd: cmd, Show: r.Shown}},
		Done: func(j *compose.Job) {
			_, exit, fin := j.Result()
			if err := s.store.FinishAction(context.Background(), id, fin, exit, strings.Join(scrub(j.Tail(200)), "\n")); err != nil {
				s.log.Error("action log", "err", err)
			}
		},
	})
	if err != nil {
		s.store.FinishAction(ctx, id, s.now(), -1, "fleetling: "+err.Error())
		return nil, err
	}
	return job, nil
}

type recipeRow struct {
	Name    string
	Spec    buildSpec
	Shown   string // the command, or why it won't run now
	Invalid bool
}

type buildsData struct {
	Root    string
	Slots   []string
	Spec    buildSpec
	Rows    [][2]string // build arg rows, always a few empty ones
	Tags    string
	Command string // set after Check: the exact command Build will run
	Recipes []recipeRow
	Name    string // recipe name field
	Folders []string
}

func (s *Server) buildsData(ctx context.Context, st store.Settings, spec buildSpec) buildsData {
	d := buildsData{Root: st.Root, Slots: engineSlots(st), Spec: spec, Tags: strings.Join(spec.Tags, " ")}
	if d.Spec.Slot == "" && len(d.Slots) > 0 {
		d.Spec.Slot = d.Slots[0]
	}
	for _, a := range spec.Args {
		if a[0] != "" || a[1] != "" {
			d.Rows = append(d.Rows, a)
		}
	}
	d.Rows = append(d.Rows, [2]string{}, [2]string{})
	if recs, err := s.store.ListRecipes(ctx); err == nil {
		for _, rc := range recs {
			row := recipeRow{Name: rc.Name}
			if err := json.Unmarshal([]byte(rc.Spec), &row.Spec); err != nil {
				row.Shown, row.Invalid = "unreadable: "+err.Error(), true
			} else if res, err := row.Spec.resolve(st); err != nil {
				row.Shown, row.Invalid = err.Error(), true
			} else {
				row.Shown = res.Shown
			}
			d.Recipes = append(d.Recipes, row)
		}
	}
	// Folders with a Dockerfile somewhere near the top, as suggestions.
	if entries, err := os.ReadDir(st.Root); err == nil {
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			for _, sub := range []string{"", "build"} {
				if _, err := os.Stat(filepath.Join(st.Root, e.Name(), sub, "Dockerfile")); err == nil {
					d.Folders = append(d.Folders, filepath.Join(e.Name(), sub))
				}
			}
		}
	}
	return d
}

func (s *Server) buildsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var spec buildSpec
	name := r.URL.Query().Get("recipe")
	if name != "" {
		if rc, ok, err := s.store.GetRecipe(ctx, name); err == nil && ok {
			json.Unmarshal([]byte(rc.Spec), &spec)
		}
	}
	d := s.buildsData(ctx, st, spec)
	d.Name = name
	p := s.page(r, "Builds", "builds", d)
	if r.URL.Query().Get("saved") != "" {
		p.Notice = "Recipe saved."
	}
	s.render(w, http.StatusOK, "builds", p)
}

// specFromForm reads the build form.
func specFromForm(r *http.Request) buildSpec {
	b := buildSpec{
		Slot:       r.PostFormValue("slot"),
		Context:    strings.TrimSpace(r.PostFormValue("context")),
		Dockerfile: strings.TrimSpace(r.PostFormValue("dockerfile")),
		Tags:       strings.Fields(strings.ReplaceAll(r.PostFormValue("tags"), ",", " ")),
		Target:     strings.TrimSpace(r.PostFormValue("target")),
		Platform:   strings.TrimSpace(r.PostFormValue("platform")),
		NoCache:    r.PostFormValue("no_cache") == "1",
		Pull:       r.PostFormValue("pull") == "1",
	}
	keys, vals := r.PostForm["arg_key"], r.PostForm["arg_val"]
	for i := range keys {
		v := ""
		if i < len(vals) {
			v = vals[i]
		}
		b.Args = append(b.Args, [2]string{strings.TrimSpace(keys[i]), v})
	}
	return b
}

// buildsSubmit handles the form's three buttons: Check shows the exact
// command, Build runs it, Save stores it as a recipe.
func (s *Server) buildsSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	spec := specFromForm(r)
	if slots := engineSlots(st); len(slots) == 1 || spec.Slot == "" && len(slots) > 0 {
		spec.Slot = slots[0]
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	d := s.buildsData(ctx, st, spec)
	d.Name = name
	fail := func(status int, msg string) {
		p := s.page(r, "Builds", "builds", d)
		p.Error = msg
		s.render(w, status, "builds", p)
	}
	res, err := spec.resolve(st)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	switch r.PostFormValue("do") {
	case "save":
		if !recipeNameRe.MatchString(name) {
			fail(http.StatusBadRequest, "name the recipe: letters, digits, spaces, . _ and -, up to 64")
			return
		}
		b, _ := json.Marshal(res.Spec)
		err := s.store.SaveRecipe(ctx, store.Recipe{Name: name, Spec: string(b), Updated: s.now()})
		s.logFileAction(ctx, "build/"+name, spec.Slot, "fleetling: save build recipe "+name+": "+res.Shown, err)
		if err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
		http.Redirect(w, r, "/builds?recipe="+url.QueryEscape(name)+"&saved=1", http.StatusSeeOther)
	case "run":
		// The command must be the one the user was shown.
		if r.PostFormValue("shown") != res.Shown {
			d.Command = res.Shown
			fail(http.StatusConflict, "the form changed since the command was shown; check it again below, then build")
			return
		}
		key := "build/" + buildKey(name, res.Spec.Tags[0])
		job, err := s.startBuild(ctx, st, res, key)
		if err != nil {
			fail(http.StatusConflict, err.Error())
			return
		}
		http.Redirect(w, r, "/jobs/"+job.ID, http.StatusSeeOther)
	default:
		d.Command = res.Shown
		s.render(w, http.StatusOK, "builds", s.page(r, "Builds", "builds", d))
	}
}

// buildKey is the action log name for a build: the recipe, else the
// image name without registry or tag.
func buildKey(recipe, tag string) string {
	if recipe != "" {
		return recipe
	}
	if n, err := reference.ParseNormalizedNamed(tag); err == nil {
		return reference.FamiliarName(n)
	}
	return tag
}

func (s *Server) recipeRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rc, ok, err := s.store.GetRecipe(ctx, r.PathValue("name"))
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	var spec buildSpec
	if err := json.Unmarshal([]byte(rc.Spec), &spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := spec.resolve(st)
	if err != nil {
		http.Error(w, "this recipe won't run as saved: "+err.Error(), http.StatusBadRequest)
		return
	}
	job, err := s.startBuild(ctx, st, res, "build/"+rc.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/jobs/"+job.ID, http.StatusSeeOther)
}

func (s *Server) recipeDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	err := s.store.DeleteRecipe(ctx, name)
	s.logFileAction(ctx, "build/"+name, "", "fleetling: delete build recipe "+name, err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/builds", http.StatusSeeOther)
}

// Stack builds: `docker compose build` with pull and no-cache toggles,
// for stacks where any service has build:.

func buildAction(pull, noCache bool) stackAction {
	argv := []string{"build"}
	if pull {
		argv = append(argv, "--pull")
	}
	if noCache {
		argv = append(argv, "--no-cache")
	}
	return stackAction{Key: "build", Label: "Build", Steps: [][]string{argv}}
}

type stackBuildData struct {
	Folder   compose.Folder
	Commands map[string]string // "pull,nocache" -> command, for the live preview
	Base     string
	Err      string
}

func (s *Server) stackBuildPage(w http.ResponseWriter, r *http.Request) {
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
	d := stackBuildData{Folder: f, Commands: map[string]string{}}
	t, err := s.target(st, f)
	if err != nil {
		d.Err = err.Error()
	} else {
		for _, pull := range []bool{false, true} {
			for _, nc := range []bool{false, true} {
				d.Commands[fmt.Sprintf("%t,%t", pull, nc)] = s.commandLine(t, buildAction(pull, nc))
			}
		}
		d.Base = d.Commands["false,false"]
	}
	s.render(w, http.StatusOK, "stack_build", s.page(r, "Build "+f.Name, "stacks", d))
}

func (s *Server) stackBuild(w http.ResponseWriter, r *http.Request) {
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
	if si, ok := s.self(ctx, st); ok && si.is(f) {
		http.Error(w, "Fleetling's own stack builds through Update, which runs build --pull in the helper", http.StatusConflict)
		return
	}
	job, err := s.runAction(ctx, st, f, buildAction(r.PostFormValue("pull") == "1", r.PostFormValue("no_cache") == "1"))
	if errors.Is(err, errNeedsReview) {
		http.Redirect(w, r, "/stacks/"+f.Name+"/review?action=build", http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/stacks/"+f.Name+"?job="+job.ID+"&built=1", http.StatusSeeOther)
}

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/portainer"
	"github.com/NullAngst/Fleetling/internal/store"
)

// The importer moves Portainer's compose stacks into stack folders. It
// writes files only: the running containers are picked up by project name,
// so nothing restarts.

const importTTL = time.Hour

type envSuggestion struct {
	KV          string
	FromCompose bool // the key is also set under environment: in the compose file
}

type importCandidate struct {
	Key         string // form field suffix, unique per session
	PortainerID int
	Name        string // Portainer's stack name, used unchanged as the project name
	Skip        string // why this stack can't be imported, empty when it can
	Folder      string
	FolderSure  bool
	Compose     string
	Fixed       string // Compose with the relative-path fixes applied
	Fixes       []portainer.Fix
	Diff        []diffRow
	Env         []portainer.Pair
	EnvSource   string // "Portainer API", "stack.env in Portainer's folder", "suggestions"
	Suggestions []envSuggestion
	StackEnv    bool
	GitURL      string
	Warnings    []string
	Conflict    bool
	Containers  int
	Running     int
}

type importSession struct {
	client    *portainer.Client
	endpoints []portainer.Endpoint
	offline   bool
	dataPath  string
	cands     []*importCandidate
	created   time.Time
}

// importState holds the one import session. It lives in memory only, so
// Portainer credentials never reach the disk, and a restart forgets them.
type importState struct {
	mu   sync.Mutex
	sess *importSession
}

func (s *Server) importSession() *importSession {
	s.imp.mu.Lock()
	defer s.imp.mu.Unlock()
	if s.imp.sess != nil && time.Since(s.imp.sess.created) > importTTL {
		s.imp.sess = nil
	}
	return s.imp.sess
}

func (s *Server) setImportSession(x *importSession) {
	s.imp.mu.Lock()
	s.imp.sess = x
	s.imp.mu.Unlock()
}

type portainerBox struct {
	Name    string
	ID      string
	Image   string
	Command string
	Err     string
}

// findPortainer looks for the Portainer container on the Docker endpoint.
func (s *Server) findPortainer(ctx context.Context) portainerBox {
	e, _, err := s.engineFor(ctx, compose.EngineDocker)
	if err != nil {
		return portainerBox{Err: err.Error()}
	}
	defer e.Close()
	rows, err := e.Containers(ctx)
	if err != nil {
		return portainerBox{Err: err.Error()}
	}
	for _, r := range rows {
		img := strings.ToLower(r.Image)
		if strings.Contains(img, "portainer/portainer") && !strings.Contains(img, "agent") {
			return portainerBox{Name: r.Name, ID: r.ID, Image: r.Image, Command: cliLine("remove", r.Name)}
		}
	}
	return portainerBox{}
}

type importLanding struct {
	Portainer portainerBox
	DataPath  string
	Root      string
}

func (s *Server) importPage(w http.ResponseWriter, r *http.Request) {
	if x := s.importSession(); x != nil && len(x.cands) > 0 {
		s.renderPreview(w, r, x, "", http.StatusOK)
		return
	}
	st, _ := s.store.LoadSettings(r.Context(), s.cfg.DefaultRoot)
	d := importLanding{Portainer: s.findPortainer(r.Context()), DataPath: filepath.Join(st.Root, "portainer"), Root: st.Root}
	s.render(w, http.StatusOK, "import", s.page(r, "Import from Portainer", "import", d))
}

func (s *Server) importFail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	st, _ := s.store.LoadSettings(r.Context(), s.cfg.DefaultRoot)
	d := importLanding{Portainer: s.findPortainer(r.Context()), DataPath: r.PostFormValue("data_path"), Root: st.Root}
	if d.DataPath == "" {
		d.DataPath = filepath.Join(st.Root, "portainer")
	}
	p := s.page(r, "Import from Portainer", "import", d)
	p.Error = msg
	s.render(w, status, "import", p)
}

func (s *Server) importConnect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	c, err := portainer.Connect(ctx, portainer.Options{
		URL: r.PostFormValue("url"), APIKey: r.PostFormValue("api_key"),
		Username: r.PostFormValue("username"), Password: r.PostFormValue("password"),
		SkipTLSVerify: r.PostFormValue("skip_tls") == "1",
	})
	if err != nil {
		s.importFail(w, r, http.StatusBadGateway, err.Error())
		return
	}
	eps, err := c.Endpoints(ctx)
	if err != nil {
		s.importFail(w, r, http.StatusBadGateway, err.Error())
		return
	}
	x := &importSession{client: c, endpoints: eps, created: time.Now()}
	s.setImportSession(x)
	// Pre-select the local socket environment; skip the question when it is
	// the only Docker one.
	var local []portainer.Endpoint
	for _, e := range eps {
		if e.Type == portainer.EndpointDockerLocal {
			local = append(local, e)
		}
	}
	if len(local) == 1 && len(eps) == 1 {
		s.planFromAPI(w, r, x, local[0].ID)
		return
	}
	pre := 0
	if len(local) > 0 {
		pre = local[0].ID
	}
	s.render(w, http.StatusOK, "import_endpoint", s.page(r, "Pick the environment", "import", map[string]any{"Endpoints": eps, "Selected": pre}))
}

func (s *Server) importEndpoint(w http.ResponseWriter, r *http.Request) {
	x := s.importSession()
	if x == nil || x.client == nil {
		http.Redirect(w, r, "/import", http.StatusSeeOther)
		return
	}
	id, _ := strconv.Atoi(r.PostFormValue("endpoint"))
	s.planFromAPI(w, r, x, id)
}

// projectContainers groups the Docker endpoint's containers by project and
// inspects each, for mounts and env.
func (s *Server) projectContainers(ctx context.Context) (map[string][]engine.Detail, Engine, error) {
	e, _, err := s.engineFor(ctx, compose.EngineDocker)
	if err != nil {
		return nil, nil, err
	}
	rows, err := e.Containers(ctx)
	if err != nil {
		e.Close()
		return nil, nil, err
	}
	out := map[string][]engine.Detail{}
	for _, r := range rows {
		if r.Project == "" {
			continue
		}
		d, err := e.Inspect(ctx, r.ID)
		if err != nil {
			continue
		}
		out[r.Project] = append(out[r.Project], d)
	}
	return out, e, nil
}

func (s *Server) planFromAPI(w http.ResponseWriter, r *http.Request, x *importSession, endpointID int) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		s.importFail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	stacks, err := x.client.Stacks(ctx)
	if err != nil {
		s.importFail(w, r, http.StatusBadGateway, err.Error())
		return
	}
	byProject, e, err := s.projectContainers(ctx)
	if err != nil {
		s.importFail(w, r, http.StatusBadGateway, "Docker endpoint: "+err.Error())
		return
	}
	e.Close()
	slices.SortFunc(stacks, func(a, b portainer.Stack) int { return strings.Compare(a.Name, b.Name) })
	x.cands = nil
	for _, ps := range stacks {
		c := &importCandidate{Key: "p" + strconv.Itoa(ps.ID), PortainerID: ps.ID, Name: ps.Name, Env: ps.Env, EnvSource: "Portainer API"}
		x.cands = append(x.cands, c)
		switch {
		case ps.EndpointID != endpointID:
			c.Skip = "belongs to another Portainer environment"
			continue
		case ps.Type == portainer.TypeSwarm:
			c.Skip = "Swarm stack; Fleetling runs Compose only"
			continue
		case ps.Type == portainer.TypeKubernetes:
			c.Skip = "Kubernetes stack"
			continue
		case ps.Type != portainer.TypeCompose:
			c.Skip = fmt.Sprintf("unknown stack type %d", ps.Type)
			continue
		}
		if ps.GitConfig != nil && ps.GitConfig.URL != "" {
			c.GitURL = ps.GitConfig.URL
			c.Warnings = append(c.Warnings, "deployed from git ("+ps.GitConfig.URL+"). The compose file is imported as it is now; Fleetling records the repo but won't pull from it")
		}
		text, err := x.client.StackFile(ctx, ps.ID)
		if err != nil {
			c.Skip = "could not read its compose file: " + err.Error()
			continue
		}
		c.Compose = text
		if ps.Status == 2 {
			c.Warnings = append(c.Warnings, "stopped in Portainer")
		}
		s.finishCandidate(st, c, byProject[ps.Name])
	}
	x.offline = false
	s.renderPreview(w, r, x, "", http.StatusOK)
}

// portainerIDRe pulls the stack ID out of a working_dir label like
// /data/compose/12.
var portainerIDRe = regexp.MustCompile(`/compose/([0-9]+)/?$`)

func (s *Server) importOffline(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		s.importFail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	dataPath := filepath.Clean(strings.TrimSpace(r.PostFormValue("data_path")))
	entries, err := os.ReadDir(filepath.Join(dataPath, "compose"))
	if err != nil {
		s.importFail(w, r, http.StatusBadRequest, fmt.Sprintf("cannot read %s: %v. The Portainer data folder has to be visible inside the Fleetling container, at the same path", filepath.Join(dataPath, "compose"), err))
		return
	}
	byProject, e, err := s.projectContainers(ctx)
	if err != nil {
		s.importFail(w, r, http.StatusBadGateway, "Docker endpoint: "+err.Error())
		return
	}
	defer e.Close()
	idToProject := map[string]string{}
	for project, ds := range byProject {
		for _, d := range ds {
			if m := portainerIDRe.FindStringSubmatch(d.WorkDir); m != nil {
				idToProject[m[1]] = project
			}
		}
	}
	x := &importSession{offline: true, dataPath: dataPath, created: time.Now()}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		id := ent.Name()
		if _, err := strconv.Atoi(id); err != nil {
			continue
		}
		dir := filepath.Join(dataPath, "compose", id)
		c := &importCandidate{Key: "o" + id, Name: "compose/" + id}
		x.cands = append(x.cands, c)
		for _, n := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yaml", "compose.yml"} {
			if b, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
				c.Compose = string(b)
				break
			}
		}
		if c.Compose == "" {
			c.Skip = "no compose file in " + dir
			continue
		}
		project, ok := idToProject[id]
		if !ok {
			// Spec: never guess the project name.
			c.Skip = "no container's working_dir label ends in /compose/" + id + ", so its project name is unknown. Start it once in Portainer, or import it by hand"
			continue
		}
		c.Name = project
		pid, _ := strconv.Atoi(id)
		c.PortainerID = pid
		if b, err := os.ReadFile(filepath.Join(dir, "stack.env")); err == nil {
			if vars, err := compose.ParseEnv(bytes.NewReader(b)); err == nil {
				for _, v := range vars {
					c.Env = append(c.Env, portainer.Pair{Name: v.Key, Value: v.Value})
				}
				c.EnvSource = "stack.env in Portainer's folder"
			}
		}
		if c.EnvSource == "" {
			c.EnvSource = "suggestions"
			fromCompose := portainer.ComposeEnvKeys(c.Compose)
			seen := map[string]bool{}
			for _, d := range byProject[project] {
				imgEnv, _ := e.ImageEnv(ctx, d.Image)
				for _, kv := range portainer.EnvSuggestions(d.Env, imgEnv) {
					if seen[kv] {
						continue
					}
					seen[kv] = true
					k, _, _ := strings.Cut(kv, "=")
					c.Suggestions = append(c.Suggestions, envSuggestion{KV: kv, FromCompose: fromCompose[k]})
				}
			}
			c.Warnings = append(c.Warnings, "env vars can't be recovered exactly offline. Tick the suggestions that belong in .env")
		}
		s.finishCandidate(st, c, byProject[project])
	}
	slices.SortFunc(x.cands, func(a, b *importCandidate) int { return strings.Compare(a.Name, b.Name) })
	s.setImportSession(x)
	s.renderPreview(w, r, x, "", http.StatusOK)
}

// finishCandidate fills in everything that doesn't depend on where the
// compose text came from.
func (s *Server) finishCandidate(st store.Settings, c *importCandidate, containers []engine.Detail) {
	if err := compose.ValidProject(c.Name); err != nil {
		c.Skip = "Portainer name is not a valid Compose project name: " + err.Error()
		return
	}
	mounts := map[string][]compose.Mount{}
	for _, d := range containers {
		c.Containers++
		if d.Running {
			c.Running++
		}
		mounts[d.Service] = append(mounts[d.Service], d.Mounts...)
	}
	fixes, unresolved := portainer.RelativeFixes(c.Compose, mounts)
	c.Fixed = c.Compose
	if len(fixes) > 0 {
		if fixed, err := portainer.ApplyFixes(c.Compose, fixes); err == nil {
			c.Fixes, c.Fixed = fixes, fixed
			c.Diff, _, _ = sideBySide(c.Compose, fixed)
		} else {
			c.Warnings = append(c.Warnings, "could not rewrite relative paths: "+err.Error())
		}
	}
	for _, u := range unresolved {
		c.Warnings = append(c.Warnings, "relative path "+u+" has no matching mount on a running container; it will resolve against the new folder")
	}
	c.Folder, c.FolderSure = portainer.FolderGuess(c.Compose, st.Root, c.Name)
	if !c.FolderSure {
		c.Warnings = append(c.Warnings, "folder is a guess; check it")
	}
	if _, isStack := compose.ReadFolder(filepath.Join(st.Root, c.Folder)); isStack {
		c.Conflict = true
	}
	c.StackEnv = portainer.UsesStackEnv(c.Compose)
	if _, problems := portainer.EnvText(c.Env); len(problems) > 0 {
		c.Warnings = append(c.Warnings, problems...)
	}
	if c.Containers == 0 {
		c.Warnings = append(c.Warnings, "no containers exist for this project, so it will show Stopped")
	}
}

type previewData struct {
	Offline  bool
	DataPath string
	Cands    []*importCandidate
	Root     string
}

func (s *Server) renderPreview(w http.ResponseWriter, r *http.Request, x *importSession, msg string, status int) {
	st, _ := s.store.LoadSettings(r.Context(), s.cfg.DefaultRoot)
	p := s.page(r, "Import preview", "import", previewData{Offline: x.offline, DataPath: x.dataPath, Cands: x.cands, Root: st.Root})
	p.Error = msg
	s.render(w, status, "import_preview", p)
}

func (s *Server) importReset(w http.ResponseWriter, r *http.Request) {
	s.setImportSession(nil)
	http.Redirect(w, r, "/import", http.StatusSeeOther)
}

type importResult struct {
	Name   string
	Folder string
	Status string
	OK     bool
	Note   string
}

func (s *Server) importApply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	x := s.importSession()
	if x == nil {
		http.Redirect(w, r, "/import", http.StatusSeeOther)
		return
	}
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if st.DockerHost == "" {
		s.renderPreview(w, r, x, "the Docker endpoint is off in settings; Portainer's stacks run on Docker", http.StatusBadRequest)
		return
	}

	// Validate every ticked row before writing anything.
	type job struct {
		c       *importCandidate
		folder  string
		text    string
		env     []portainer.Pair
		applied bool
	}
	var jobs []job
	used := map[string]string{}
	for _, c := range x.cands {
		if c.Skip != "" || r.PostFormValue("include_"+c.Key) != "1" {
			continue
		}
		c.Folder = strings.TrimSpace(r.PostFormValue("folder_" + c.Key))
		if !compose.ValidFolderName(c.Folder) {
			s.renderPreview(w, r, x, c.Name+": the folder must be a single name directly under "+st.Root, http.StatusBadRequest)
			return
		}
		if prev, dup := used[c.Folder]; dup {
			s.renderPreview(w, r, x, c.Name+" and "+prev+" both point at "+c.Folder, http.StatusBadRequest)
			return
		}
		used[c.Folder] = c.Name
		dir := filepath.Join(st.Root, c.Folder)
		if fi, err := os.Lstat(dir); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir()) {
			s.renderPreview(w, r, x, dir+" exists and is not a plain folder", http.StatusBadRequest)
			return
		}
		if _, isStack := compose.ReadFolder(dir); isStack {
			s.renderPreview(w, r, x, dir+" already holds a stack; pick another folder for "+c.Name, http.StatusBadRequest)
			return
		}
		j := job{c: c, folder: c.Folder, text: c.Compose, env: c.Env}
		if len(c.Fixes) > 0 && r.PostFormValue("fix_"+c.Key) == "1" {
			j.text, j.applied = c.Fixed, true
		}
		for i, sg := range c.Suggestions {
			if r.PostFormValue(fmt.Sprintf("env_%s_%d", c.Key, i)) == "1" {
				k, v, _ := strings.Cut(sg.KV, "=")
				j.env = append(j.env, portainer.Pair{Name: k, Value: v})
			}
		}
		jobs = append(jobs, j)
	}
	if len(jobs) == 0 {
		s.renderPreview(w, r, x, "nothing ticked", http.StatusBadRequest)
		return
	}

	var results []importResult
	for _, j := range jobs {
		dir := filepath.Join(st.Root, j.folder)
		err := s.writeImport(dir, j.c, j.text, j.env)
		what := "fleetling: import Portainer stack " + j.c.Name + " into " + dir
		if j.applied {
			what += fmt.Sprintf(" (%d relative paths made absolute)", len(j.c.Fixes))
		}
		s.logFileAction(ctx, j.folder, compose.EngineDocker, what, err)
		res := importResult{Name: j.c.Name, Folder: j.folder}
		if err != nil {
			res.Note = err.Error()
		}
		results = append(results, res)
	}
	s.setImportSession(nil)

	// Verify: every imported stack should line up with its running
	// containers. One that doesn't means the project name didn't match.
	folders, _ := compose.Discover(st.Root, st.Ignore)
	containers, status := s.gather(ctx, st)
	reachable := map[string]bool{}
	for _, e := range status {
		reachable[e.Slot] = e.Up
	}
	rows := compose.Merge(folders, containers, reachable)
	mismatch := false
	for i, res := range results {
		if res.Note != "" {
			mismatch = true
			continue
		}
		for _, row := range rows {
			if row.Folder == res.Folder {
				results[i].Status = row.Label()
				results[i].OK = row.State == compose.StateRunning || row.State == compose.StatePartial
				if row.Engine == "" && slices.ContainsFunc(rows, func(o compose.Stack) bool { return o.Kind == compose.KindExternal && o.Project == res.Name }) {
					results[i].Note = "its containers still show as External: the project name did not match. Nothing was guessed; check the name"
					mismatch = true
				}
			}
		}
	}
	p := s.page(r, "Import finished", "import", map[string]any{"Results": results, "Portainer": s.findPortainer(ctx)})
	if mismatch {
		p.Error = "some stacks did not line up with their containers. Stop here and check them before retiring Portainer"
	}
	s.render(w, http.StatusOK, "import_done", p)
}

// writeImport writes one stack: compose.yaml exactly as returned (or with
// approved fixes), the env vars at mode 600, and .fleetling.toml.
func (s *Server) writeImport(dir string, c *importCandidate, text string, env []portainer.Pair) error {
	var owner compose.Owner
	if _, err := os.Stat(dir); err == nil {
		owner = compose.OwnerOf(dir)
	} else if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	if err := compose.WriteFileAtomic(filepath.Join(dir, "compose.yaml"), []byte(text), 0o644, owner); err != nil {
		return err
	}
	envText, _ := portainer.EnvText(env)
	if c.StackEnv {
		// Compose only reads .env for interpolation; env_file: stack.env
		// reads stack.env. One file, two names.
		if err := compose.WriteFileAtomic(filepath.Join(dir, "stack.env"), []byte(envText), 0o600, owner); err != nil {
			return err
		}
		link := filepath.Join(dir, ".env")
		if _, err := os.Lstat(link); err == nil {
			return errors.New(".env already exists; left it alone, stack.env written next to it")
		}
		if err := os.Symlink("stack.env", link); err != nil {
			return err
		}
	} else if envText != "" {
		if err := compose.WriteFileAtomic(filepath.Join(dir, ".env"), []byte(envText), 0o600, owner); err != nil {
			return err
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	m := &compose.Meta{Project: c.Name, Engine: compose.EngineDocker, Created: now, GitRepo: c.GitURL}
	if c.Containers > 0 {
		m.Deployed = &now // already deployed under Portainer; nothing to track
	}
	return compose.WriteMeta(dir, m, owner)
}

// importRetire stops and removes the Portainer container, and nothing else.
// Its data folder and named volume stay.
func (s *Server) importRetire(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pb := s.findPortainer(ctx)
	if pb.ID == "" {
		http.Error(w, "no Portainer container found", http.StatusNotFound)
		return
	}
	if s.isSelfContainer(pb.ID) {
		http.Error(w, "refusing to remove Fleetling's own container", http.StatusConflict)
		return
	}
	e, _, err := s.engineFor(ctx, compose.EngineDocker)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer e.Close()
	err = e.Remove(ctx, pb.ID)
	s.logFileAction(ctx, "container/"+pb.Name, compose.EngineDocker, pb.Command, err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/?retired=1", http.StatusSeeOther)
}

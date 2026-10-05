package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/store"
)

type actionButton struct {
	Key     string
	Label   string
	Command string
	Danger  bool
}

type envLine struct {
	Raw    string
	Key    string
	Value  string
	IsVar  bool
	Secret bool
}

type serviceRow struct {
	Name     string
	Running  int
	Total    int // containers
	Declared bool
}

type stackPageData struct {
	Folder        compose.Folder
	Stack         compose.Stack
	Tab           string
	Compose       string
	Env           []envLine
	HasEnv        bool
	Services      []serviceRow
	Actions       []actionButton
	Self          bool
	FolderMounted []string
	JobID         string
	Recent        []store.Action
	TargetErr     string
}

// Keys whose values the env view blurs until clicked.
var secretMarkers = []string{"PASS", "SECRET", "TOKEN", "KEY"}

func envLines(text string) []envLine {
	var out []envLine
	for _, raw := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		l := envLine{Raw: raw}
		t := strings.TrimSpace(raw)
		if t != "" && !strings.HasPrefix(t, "#") {
			if k, v, ok := strings.Cut(strings.TrimPrefix(t, "export "), "="); ok {
				l.IsVar, l.Key, l.Value = true, strings.TrimSpace(k), v
				up := strings.ToUpper(l.Key)
				for _, m := range secretMarkers {
					if strings.Contains(up, m) {
						l.Secret = true
					}
				}
			}
		}
		out = append(out, l)
	}
	return out
}

func (s *Server) stackPage(w http.ResponseWriter, r *http.Request) {
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
	d := stackPageData{Folder: f, Tab: r.URL.Query().Get("tab")}
	switch d.Tab {
	case "env", "services", "history", "logs":
	default:
		d.Tab = "compose"
	}

	containers, status := s.gather(ctx, st)
	reachable := map[string]bool{}
	for _, e := range status {
		reachable[e.Slot] = e.Up
	}
	for _, row := range compose.Merge([]compose.Folder{f}, containers, reachable) {
		if row.Dir == f.Dir && row.Kind != compose.KindExternal {
			d.Stack = row
		}
	}

	if f.ComposeFile != "" {
		if b, err := os.ReadFile(filepath.Join(f.Dir, f.ComposeFile)); err == nil {
			d.Compose = string(b)
			d.FolderMounted = compose.FolderBindMounts(b, f.Dir)
		}
	}
	if b, err := os.ReadFile(filepath.Join(f.Dir, ".env")); err == nil {
		d.HasEnv = true
		d.Env = envLines(string(b))
	}

	// Services: what the file declares plus what is actually running.
	rows := map[string]*serviceRow{}
	for _, name := range f.Services {
		rows[name] = &serviceRow{Name: name, Declared: true}
	}
	for _, c := range containers {
		if c.Project != d.Stack.Project || c.Engine != d.Stack.Engine || d.Stack.Engine == "" {
			continue
		}
		row := rows[c.Service]
		if row == nil {
			row = &serviceRow{Name: c.Service}
			rows[c.Service] = row
		}
		row.Total++
		if c.Running {
			row.Running++
		}
	}
	for _, row := range rows {
		d.Services = append(d.Services, *row)
	}
	slices.SortFunc(d.Services, func(a, b serviceRow) int { return strings.Compare(a.Name, b.Name) })

	if si, ok := s.self(ctx, st); ok && si.is(f) {
		d.Self = true
	}
	if t, err := s.target(st, f); err == nil {
		for _, a := range stackActions {
			d.Actions = append(d.Actions, actionButton{Key: a.Key, Label: a.Label, Command: s.commandLine(t, a), Danger: a.Danger})
		}
	} else {
		d.TargetErr = err.Error()
	}

	if j := s.jobs.Running(f.Name); j != nil {
		d.JobID = j.ID
	} else if id := r.URL.Query().Get("job"); id != "" && s.jobs.Get(id) != nil && s.jobs.Get(id).Target == f.Name {
		d.JobID = id
	}
	d.Recent, _ = s.store.ListActions(ctx, f.Name, 10, 0)

	p := s.page(r, f.Name, "stacks", d)
	switch r.URL.Query().Get("saved") {
	case "1":
		p.Notice = "Saved."
	case "managed":
		p.Notice = "Now managed. The project name and engine live in " + compose.MetaFile + "."
	}
	s.render(w, http.StatusOK, "stack", p)
}

// manageStack writes .fleetling.toml for an On disk folder. The project
// name and engine come from whatever is running, else Compose's default
// for the folder and the first configured engine.
func (s *Server) manageStack(w http.ResponseWriter, r *http.Request) {
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
	if f.Meta != nil {
		http.Redirect(w, r, "/stacks/"+f.Name, http.StatusSeeOther)
		return
	}
	containers, status := s.gather(ctx, st)
	reachable := map[string]bool{}
	for _, e := range status {
		reachable[e.Slot] = e.Up
	}
	project, slot := f.Project, ""
	for _, row := range compose.Merge([]compose.Folder{f}, containers, reachable) {
		if row.Dir == f.Dir && row.Engine != "" {
			project, slot = row.Project, row.Engine
		}
	}
	if slot == "" {
		if eps := endpoints(st); len(eps) > 0 {
			slot = eps[0].Slot
		}
	}
	if err := compose.ValidProject(project); err != nil {
		http.Error(w, "cannot manage this folder: "+err.Error(), http.StatusBadRequest)
		return
	}
	m := &compose.Meta{Project: project, Engine: slot, Created: s.now().UTC().Truncate(time.Second)}
	// A stack that is already running has had its first deploy; there is
	// nothing to track, and pretending otherwise would claim folders the
	// user made by hand.
	if len(containers) > 0 && slot != "" {
		for _, c := range containers {
			if c.Project == project && c.Engine == slot {
				m.Deployed = &m.Created
				break
			}
		}
	}
	err = compose.WriteMeta(f.Dir, m, compose.OwnerOf(f.Dir))
	s.logFileAction(ctx, f.Name, slot, "fleetling: write "+filepath.Join(f.Dir, compose.MetaFile)+" (project "+project+", engine "+slot+")", err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/stacks/"+f.Name+"?saved=managed", http.StatusSeeOther)
}

func (s *Server) stackAction(w http.ResponseWriter, r *http.Request) {
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
	a, ok := findAction(r.PathValue("action"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if si, ok := s.self(ctx, st); ok && si.is(f) {
		http.Error(w, "this is Fleetling's own stack; it can't run Compose on itself from inside (the Update button for it arrives with self-management)", http.StatusConflict)
		return
	}
	job, err := s.runAction(ctx, st, f, a)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, compose.ErrBusy) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	http.Redirect(w, r, "/stacks/"+f.Name+"?job="+job.ID, http.StatusSeeOther)
}

type jobMessage struct {
	Lines []string `json:"lines,omitempty"`
	Done  bool     `json:"done,omitempty"`
	Exit  int      `json:"exit"`
}

// jobSocket streams a job's output. Late joiners get everything from the
// start. The session cookie is checked by requireAuth; websocket.Accept
// refuses an Origin that doesn't match the Host.
func (s *Server) jobSocket(w http.ResponseWriter, r *http.Request) {
	j := s.jobs.Get(r.PathValue("id"))
	if j == nil {
		http.NotFound(w, r)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already wrote the error response
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context()) // we only write; this notices the browser leaving
	pos := 0
	for {
		lines, next, done, exit, changed := j.Since(pos)
		pos = next
		for len(lines) > 0 {
			n := min(len(lines), 500)
			if err := writeJSON(ctx, c, jobMessage{Lines: lines[:n]}); err != nil {
				return
			}
			lines = lines[n:]
		}
		if done {
			writeJSON(ctx, c, jobMessage{Done: true, Exit: exit})
			c.Close(websocket.StatusNormalClosure, "")
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
	}
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, buf.Bytes())
}

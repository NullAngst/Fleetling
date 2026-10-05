package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
)

type stackFormData struct {
	Root          string
	Project       string
	FolderName    string
	Engine        string
	Engines       []string
	Compose       string
	Env           string
	Deploy        bool
	ValidationErr string

	// Edit only.
	Folder      compose.Folder
	Hash        string
	Rows        []diffRow
	EnvRows     []diffRow
	Changed     bool
	DiffOK      bool
	CanRedeploy bool
}

func engineSlots(st store.Settings) []string {
	var out []string
	for _, ep := range endpoints(st) {
		out = append(out, ep.Slot)
	}
	return out
}

// normalizeNewlines undoes the CRLF that browsers send for every textarea.
// A file that already used CRLF keeps it; anything else is saved with LF,
// exactly as typed otherwise.
func normalizeNewlines(submitted, original string) string {
	if strings.Contains(original, "\r\n") {
		return submitted
	}
	return strings.ReplaceAll(submitted, "\r\n", "\n")
}

func contentHash(composeText, envText string, envExists bool) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d:%s\x00%t:%s", len(composeText), composeText, envExists, envText)
	return hex.EncodeToString(h.Sum(nil))
}

// envPath is where .env text gets written. When .env is a symlink (the
// Portainer importer links it to stack.env), the write goes to the target,
// but only if the target is inside the stack folder.
func envPath(dir string) (string, error) {
	p := filepath.Join(dir, ".env")
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return p, nil
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf(".env is a broken symlink: %w", err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if filepath.Dir(real) != realDir {
		return "", fmt.Errorf(".env links to %s, outside the stack folder; edit that file directly", real)
	}
	return real, nil
}

func (s *Server) newStackForm(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.LoadSettings(r.Context(), s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := stackFormData{Root: st.Root, Engines: engineSlots(st), Deploy: true,
		Compose: "services:\n  app:\n    image: \n    restart: unless-stopped\n"}
	if len(d.Engines) > 0 {
		d.Engine = d.Engines[0]
	}
	p := s.page(r, "New stack", "stacks", d)
	p.Editor = true
	s.render(w, http.StatusOK, "stack_new", p)
}

func (s *Server) newStackSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := stackFormData{
		Root:       st.Root,
		Engines:    engineSlots(st),
		Project:    strings.TrimSpace(r.PostFormValue("project")),
		FolderName: strings.TrimSpace(r.PostFormValue("folder")),
		Engine:     r.PostFormValue("engine"),
		Compose:    normalizeNewlines(r.PostFormValue("compose"), ""),
		Env:        normalizeNewlines(r.PostFormValue("env"), ""),
		Deploy:     r.PostFormValue("deploy") == "1",
	}
	if len(d.Engines) == 1 {
		d.Engine = d.Engines[0] // the field is hidden when there is no choice
	}
	if d.FolderName == "" {
		d.FolderName = d.Project
	}
	fail := func(msg string) {
		p := s.page(r, "New stack", "stacks", d)
		p.Editor = true
		p.Error = msg
		s.render(w, http.StatusBadRequest, "stack_new", p)
	}

	if err := compose.ValidProject(d.Project); err != nil {
		fail(err.Error())
		return
	}
	if !compose.ValidFolderName(d.FolderName) {
		fail("the folder must be a single name directly under " + st.Root)
		return
	}
	host := hostFor(st, d.Engine)
	if host == "" {
		fail("pick a configured engine")
		return
	}
	normHost, _, err := engine.NormalizeHost(host)
	if err != nil {
		fail(err.Error())
		return
	}
	if strings.TrimSpace(d.Compose) == "" {
		fail("the compose file is empty")
		return
	}

	dir := filepath.Join(st.Root, d.FolderName)
	existed := false
	if fi, err := os.Lstat(dir); err == nil {
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			fail(dir + " is a symlink; stack folders must be real folders under the root")
			return
		case !fi.IsDir():
			fail(dir + " exists and is not a folder")
			return
		}
		if _, isStack := compose.ReadFolder(dir); isStack {
			fail(dir + " already holds a stack. Open it from the stacks page instead")
			return
		}
		existed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		fail(err.Error())
		return
	}
	if msg := s.existingStackFor(ctx, st, d.Project, d.Engine); msg != "" {
		fail("project " + d.Project + ": " + msg)
		return
	}

	t := compose.Target{Project: d.Project, Dir: dir, File: "compose.yaml", Host: normHost, Podman: d.Engine == compose.EnginePodman}
	if r.PostFormValue("force") != "1" {
		vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.runner.Validate(vctx, t, d.Compose, d.Env)
		cancel()
		if err != nil {
			d.ValidationErr = err.Error()
			fail("docker compose config rejected the file. Fix it, or tick Save anyway to keep a half-finished file")
			return
		}
	}

	err = s.writeNewStack(dir, existed, d)
	if err == nil {
		err = s.recordBaseline(ctx, dir, "create")
	}
	s.logFileAction(ctx, d.FolderName, d.Engine, "fleetling: create stack "+d.Project+" in "+dir, err)
	if err != nil {
		fail(err.Error())
		return
	}
	s.log.Info("stack created", "project", d.Project, "dir", dir)

	if d.Deploy {
		f, ok := compose.ReadFolder(dir)
		if ok {
			if job, err := s.runAction(ctx, st, f, stackActions[0]); err == nil {
				http.Redirect(w, r, "/stacks/"+d.FolderName+"?job="+job.ID, http.StatusSeeOther)
				return
			} else {
				s.log.Error("deploy after create", "err", err)
			}
		}
	}
	http.Redirect(w, r, "/stacks/"+d.FolderName+"?saved=1", http.StatusSeeOther)
}

func (s *Server) writeNewStack(dir string, existed bool, d stackFormData) error {
	var owner compose.Owner
	if existed {
		owner = compose.OwnerOf(dir)
	} else if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	if err := compose.WriteFileAtomic(filepath.Join(dir, "compose.yaml"), []byte(d.Compose), 0o644, owner); err != nil {
		return err
	}
	if d.Env != "" {
		if err := compose.WriteFileAtomic(filepath.Join(dir, ".env"), []byte(d.Env), 0o600, owner); err != nil {
			return err
		}
	}
	return compose.WriteMeta(dir, &compose.Meta{
		Project: d.Project, Engine: d.Engine, Created: time.Now().UTC().Truncate(time.Second),
	}, owner)
}

// readStackFiles returns the current compose and env text.
func readStackFiles(f compose.Folder) (composeText, envText string, envExists bool, err error) {
	b, err := os.ReadFile(filepath.Join(f.Dir, f.ComposeFile))
	if err != nil {
		return "", "", false, err
	}
	e, err := os.ReadFile(filepath.Join(f.Dir, ".env"))
	if err == nil {
		return string(b), string(e), true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return string(b), "", false, nil
	}
	return "", "", false, err
}

func (s *Server) editForm(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.LoadSettings(r.Context(), s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, ok := loadFolder(st, r.PathValue("folder"))
	if !ok || f.ComposeFile == "" {
		http.NotFound(w, r)
		return
	}
	// Editing a stack whose files changed outside Fleetling would load the
	// changed text into the editor and approve it on save without anyone
	// looking at the difference. Review first.
	if rs, err := s.reviewFor(r.Context(), f); err != nil || rs.State != reviewOK {
		http.Redirect(w, r, "/stacks/"+f.Name+"/review", http.StatusSeeOther)
		return
	}
	c, e, ex, err := readStackFiles(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := stackFormData{Root: st.Root, Folder: f, Compose: c, Env: e, Hash: contentHash(c, e, ex), CanRedeploy: f.Meta != nil}
	p := s.page(r, "Edit "+f.Name, "stacks", d)
	p.Editor = true
	s.render(w, http.StatusOK, "stack_edit", p)
}

// editSubmit handles the three buttons on the edit flow: Review (show the
// diff), Save, and Save and redeploy.
func (s *Server) editSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, ok := loadFolder(st, r.PathValue("folder"))
	if !ok || f.ComposeFile == "" {
		http.NotFound(w, r)
		return
	}
	oldC, oldE, envExists, err := readStackFiles(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := stackFormData{
		Root: st.Root, Folder: f, CanRedeploy: f.Meta != nil,
		Compose: normalizeNewlines(r.PostFormValue("compose"), oldC),
		Env:     normalizeNewlines(r.PostFormValue("env"), oldE),
		Hash:    r.PostFormValue("hash"),
	}
	step := r.PostFormValue("step")
	force := r.PostFormValue("force") == "1"

	render := func(status int, msg string) {
		d.Rows, d.Changed, d.DiffOK = sideBySide(oldC, d.Compose)
		envRows, envChanged, _ := sideBySide(oldE, d.Env)
		d.EnvRows, d.Changed = envRows, d.Changed || envChanged
		p := s.page(r, "Review "+f.Name, "stacks", d)
		p.Error = msg
		s.render(w, status, "stack_diff", p)
	}

	if d.Hash != contentHash(oldC, oldE, envExists) {
		d.Hash = contentHash(oldC, oldE, envExists)
		render(http.StatusConflict, "the files changed on disk since you opened the editor. The diff below is against what is there now; review it again before saving")
		return
	}

	// Validate on every step unless the user asked to save anyway.
	if !force {
		t := compose.Target{Dir: f.Dir, File: f.ComposeFile, Project: f.Project}
		if tt, err := s.target(st, f); err == nil {
			t = tt
		} else if h := hostFor(st, compose.EngineDocker); h != "" {
			t.Host, _, _ = engine.NormalizeHost(h)
		}
		vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.runner.Validate(vctx, t, d.Compose, d.Env)
		cancel()
		if err != nil {
			d.ValidationErr = err.Error()
		}
	}
	if step != "save" && step != "save_deploy" || d.ValidationErr != "" {
		status := http.StatusOK
		if d.ValidationErr != "" && step != "" && step != "review" {
			status = http.StatusBadRequest
		}
		render(status, "")
		return
	}
	if step == "save_deploy" {
		if si, ok := s.self(ctx, st); ok && si.is(f) {
			render(http.StatusConflict, "this is Fleetling's own stack. Save it, then redeploy it from the host until self-management lands")
			return
		}
	}

	if rs, err := s.reviewFor(ctx, f); err != nil || rs.State != reviewOK {
		render(http.StatusConflict, "files in this stack changed outside Fleetling. Review them on the stack page before saving")
		return
	}
	err = s.saveStackFiles(f, oldE, envExists, d.Compose, d.Env)
	if err == nil {
		err = s.recordBaseline(ctx, f.Dir, "save")
	}
	s.logFileAction(ctx, f.Name, engineOf(f), "fleetling: save "+filepath.Join(f.Dir, f.ComposeFile)+" and .env", err)
	if err != nil {
		render(http.StatusInternalServerError, err.Error())
		return
	}
	if step == "save_deploy" && f.Meta != nil {
		job, err := s.runAction(ctx, st, f, redeploy)
		if err != nil {
			render(http.StatusConflict, "saved, but the redeploy did not start: "+err.Error())
			return
		}
		http.Redirect(w, r, "/stacks/"+f.Name+"?job="+job.ID, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/stacks/"+f.Name+"?saved=1", http.StatusSeeOther)
}

func engineOf(f compose.Folder) string {
	if f.Meta != nil {
		return f.Meta.Engine
	}
	return ""
}

// saveStackFiles writes the compose file byte for byte, keeping its mode
// and owner, and the env file at mode 600. An env file that never existed
// is only created when there is something to put in it.
func (s *Server) saveStackFiles(f compose.Folder, oldEnv string, envExists bool, composeText, envText string) error {
	cpath := filepath.Join(f.Dir, f.ComposeFile)
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(cpath); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := compose.WriteFileAtomic(cpath, []byte(composeText), mode, compose.OwnerOf(cpath)); err != nil {
		return err
	}
	if !envExists && envText == "" || envExists && envText == oldEnv {
		return nil
	}
	ep, err := envPath(f.Dir)
	if err != nil {
		return err
	}
	owner := compose.OwnerOf(ep)
	if !owner.Set {
		owner = compose.OwnerOf(f.Dir)
	}
	return compose.WriteFileAtomic(ep, []byte(envText), 0o600, owner)
}

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/convert"
	"github.com/NullAngst/Fleetling/internal/store"
)

// The converter. It never installs Quadlets or applies manifests; it
// shows them, offers them as a .tar, and can save them next to a stack.

type convertData struct {
	Stacks       []string
	Stack        string
	Text         string
	Project      string
	Opts         convert.K8sOptions
	Volumes      []string
	ServiceTypes []string
	Controllers  []string

	Done       bool
	Normalized string
	NormErr    string
	Warnings   []convert.Warning
	Quadlet    []convert.File
	QuadletErr string
	Install    []string
	K8s        []convert.File
	K8sErr     string
	K8sNotes   string
}

// convertSlots limits conversions running at once: each one is three to
// five subprocesses.
var convertSlots = make(chan struct{}, 2)

func (s *Server) convertBase(st store.Settings) convertData {
	d := convertData{Project: "converted", Volumes: convert.K8sVolumes, ServiceTypes: convert.K8sServiceType, Controllers: convert.K8sControllers}
	folders, _ := compose.Discover(st.Root, st.Ignore)
	for _, f := range folders {
		if f.ComposeFile != "" {
			d.Stacks = append(d.Stacks, f.Name)
		}
	}
	d.Opts.Valid()
	return d
}

func (s *Server) convertPage(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.LoadSettings(r.Context(), s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := s.convertBase(st)
	d.Stack = r.URL.Query().Get("stack")
	s.render(w, http.StatusOK, "convert", s.page(r, "Converter", "convert", d))
}

// convertInput reads the form into a converter input and the user's text.
func (s *Server) convertInput(st store.Settings, r *http.Request, d *convertData) (convert.Input, compose.Folder, error) {
	d.Stack = r.PostFormValue("stack")
	d.Opts = convert.K8sOptions{Volumes: r.PostFormValue("volumes"), ServiceType: r.PostFormValue("service_type"), Controller: r.PostFormValue("controller")}
	if err := d.Opts.Valid(); err != nil {
		return convert.Input{}, compose.Folder{}, err
	}
	if d.Stack != "" {
		f, ok := loadFolder(st, d.Stack)
		if !ok || f.ComposeFile == "" {
			return convert.Input{}, f, errors.New("no compose file in that stack folder")
		}
		b, err := readCapped(filepath.Join(f.Dir, f.ComposeFile), 4<<20)
		if err != nil {
			return convert.Input{}, f, err
		}
		d.Text, d.Project = string(b), f.Project
		return convert.Input{Project: f.Project, Dir: f.Dir, File: filepath.Join(f.Dir, f.ComposeFile)}, f, nil
	}
	d.Text = normalizeNewlines(r.PostFormValue("compose"), "")
	if p := strings.TrimSpace(r.PostFormValue("project")); p != "" {
		d.Project = p
	}
	if err := compose.ValidProject(d.Project); err != nil {
		return convert.Input{}, compose.Folder{}, err
	}
	if len(d.Text) > 1<<20 {
		return convert.Input{}, compose.Folder{}, errors.New("that compose file is over 1 MB")
	}
	// A pasted file is converted as if it lived in <root>/<project>, so
	// its relative paths come out the way they would for a real stack.
	return convert.Input{Project: d.Project, Dir: filepath.Join(st.Root, d.Project), Text: d.Text}, compose.Folder{}, nil
}

// runConvert fills in the three outputs and the lint. A failing tool only
// fails its own tab.
func (s *Server) runConvert(ctx context.Context, in convert.Input, d *convertData) {
	d.Done = true
	y, j, err := s.tools.Normalize(ctx, in)
	if err != nil {
		d.NormErr = err.Error()
		return
	}
	d.Normalized = string(y)
	if d.Warnings, err = convert.Lint(j, d.Text); err != nil {
		d.NormErr = err.Error()
	}
	if d.Quadlet, err = s.tools.Quadlet(ctx, y); err != nil {
		d.QuadletErr = toolErr("podlet", err)
	} else {
		d.Install = convert.InstallCommands(d.Quadlet)
	}
	if d.K8s, d.K8sNotes, err = s.tools.Kubernetes(ctx, y, d.Opts); err != nil {
		d.K8sErr = toolErr("kompose", err)
	}
}

func toolErr(tool string, err error) string {
	if errors.Is(err, convert.ErrMissing) {
		return tool + " isn't installed here. The Fleetling image ships it; outside the image, install " + tool + " and put it on PATH."
	}
	return err.Error()
}

func (s *Server) convertSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	d := s.convertBase(st)
	in, f, err := s.convertInput(st, r, &d)
	if err != nil {
		p := s.page(r, "Converter", "convert", d)
		p.Error = err.Error()
		s.render(w, http.StatusBadRequest, "convert", p)
		return
	}
	select {
	case convertSlots <- struct{}{}:
		defer func() { <-convertSlots }()
	case <-ctx.Done():
		return
	}
	s.runConvert(ctx, in, &d)

	format := r.PostFormValue("format")
	var files []convert.File
	var sub string
	switch format {
	case "quadlet":
		files, sub = d.Quadlet, "quadlet"
	case "k8s":
		files, sub = d.K8s, "k8s"
	}
	switch r.PostFormValue("do") {
	case "download":
		if len(files) == 0 {
			http.Error(w, "nothing to download: the conversion failed", http.StatusBadRequest)
			return
		}
		b, err := convert.Tar(d.Project+"-"+sub, files, s.now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", d.Project+"-"+sub+".tar"))
		w.Write(b)
		return
	case "save":
		if f.Name == "" || len(files) == 0 {
			http.Error(w, "save next to stack needs a stack and a successful conversion", http.StatusBadRequest)
			return
		}
		dir, err := s.saveConverted(ctx, f, sub, files)
		p := s.page(r, "Converter", "convert", d)
		if err != nil {
			p.Error = err.Error()
			s.render(w, http.StatusInternalServerError, "convert", p)
			return
		}
		p.Notice = fmt.Sprintf("Saved %d files in %s. Nothing was installed or applied.", len(files), dir)
		s.render(w, http.StatusOK, "convert", p)
		return
	}
	s.render(w, http.StatusOK, "convert", s.page(r, "Converter", "convert", d))
}

// saveConverted writes the files into <stack>/quadlet/ or <stack>/k8s/
// with the stack folder's owner, and logs the write.
func (s *Server) saveConverted(ctx context.Context, f compose.Folder, sub string, files []convert.File) (string, error) {
	dir := filepath.Join(f.Dir, sub)
	owner := compose.OwnerOf(f.Dir)
	// Review state before the write: if the stack was clean, Fleetling's
	// own write keeps it clean (it matters when the stack builds from its
	// own folder). If it wasn't, the user still has to review.
	clean := false
	if rs, err := s.reviewFor(ctx, f); err == nil && rs.State == reviewOK && f.Meta != nil {
		clean = true
	}
	var names []string
	err := writeConfined(f.Dir, sub, files, owner, &names)
	slot := ""
	if f.Meta != nil {
		slot = f.Meta.Engine
	}
	s.logFileAction(ctx, f.Name, slot, "fleetling: write "+dir+"/: "+strings.Join(names, ", "), err)
	if err != nil {
		return dir, err
	}
	if clean {
		if err := s.recordBaseline(ctx, f.Dir, "convert"); err != nil {
			s.log.Error("record baseline after convert", "err", err)
		}
	}
	return dir, nil
}

// writeConfined writes files into dir/sub through os.Root, which refuses
// any path that leaves dir, symlinks included. A container that can write
// its own stack folder could otherwise swap sub for a symlink to, say,
// /etc/containers/systemd between a check and the write.
func writeConfined(dir, sub string, files []convert.File, owner compose.Owner, names *[]string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	switch err := root.Mkdir(sub, 0o755); {
	case err == nil:
		if owner.Set {
			if err := root.Lchown(sub, owner.UID, owner.GID); err != nil && !errors.Is(err, os.ErrPermission) {
				return err
			}
		}
	case errors.Is(err, os.ErrExist):
		fi, err := root.Lstat(sub)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a folder (symlinks are refused)", filepath.Join(dir, sub))
		}
	default:
		return err
	}
	sr, err := root.OpenRoot(sub)
	if err != nil {
		return err
	}
	defer sr.Close()
	for _, cf := range files {
		if !convert.SafeName(cf.Name) {
			return fmt.Errorf("refusing file name %q", cf.Name)
		}
		if fi, err := sr.Lstat(cf.Name); err == nil && !fi.Mode().IsRegular() {
			return fmt.Errorf("%s exists and is not a plain file", filepath.Join(dir, sub, cf.Name))
		}
		// Write a temporary file and rename it over the target, so a crash
		// never leaves half a unit file.
		tmp := ".fleetling-tmp-" + cf.Name
		fh, err := sr.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, werr := fh.Write([]byte(cf.Content))
		cerr := fh.Close()
		if werr == nil {
			werr = cerr
		}
		if werr == nil && owner.Set {
			if err := sr.Lchown(tmp, owner.UID, owner.GID); err != nil && !errors.Is(err, os.ErrPermission) {
				werr = err
			}
		}
		if werr == nil {
			werr = sr.Rename(tmp, cf.Name)
		}
		if werr != nil {
			sr.Remove(tmp)
			return werr
		}
		*names = append(*names, cf.Name)
	}
	return nil
}

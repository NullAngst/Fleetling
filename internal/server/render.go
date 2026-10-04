package server

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
)

// page is what every full-page template receives.
type page struct {
	Title   string
	Active  string // sidebar item to highlight
	Authed  bool
	CSRF    string
	Version string
	Error   string
	Notice  string
	Data    any
}

var funcs = template.FuncMap{
	"join": strings.Join,
}

// loadTemplates parses each page together with layout.html, and each
// partial on its own. A missing or broken template fails at startup, not
// on the first request.
func loadTemplates(fsys fs.FS) (map[string]*template.Template, error) {
	out := map[string]*template.Template{}
	pages, err := fs.Glob(fsys, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		var t *template.Template
		if strings.HasPrefix(name, "partial_") {
			t, err = template.New(name).Funcs(funcs).ParseFS(fsys, p)
		} else {
			t, err = template.New(name).Funcs(funcs).ParseFS(fsys, "templates/layout.html", p)
		}
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		out[name] = t
	}
	return out, nil
}

// render writes a template to a buffer first, so a template error becomes
// a clean 500 instead of half a page.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	t := s.tmpl[name]
	if t == nil {
		http.Error(w, "unknown template "+name, http.StatusInternalServerError)
		return
	}
	root := "layout"
	if strings.HasPrefix(name, "partial_") {
		root = name + ".html"
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, root, data); err != nil {
		s.log.Error("render", "template", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// page fills in the parts of page every handler needs.
func (s *Server) page(r *http.Request, title, active string, data any) page {
	return page{
		Title:   title,
		Active:  active,
		Authed:  s.authed(r),
		CSRF:    s.csrfToken(r),
		Version: s.cfg.Version,
		Data:    data,
	}
}

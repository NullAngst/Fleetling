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
	Editor  bool   // load the CodeMirror bundle
	Shell   bool   // load the xterm.js bundle
	Nonce   string // CSP nonce for injected <style> elements
	Data    any
}

var funcs = template.FuncMap{
	"join":      strings.Join,
	"add":       func(a, b int) int { return a + b },
	"sub":       func(a, b int) int { return a - b },
	"deref":     func(p *int) int { return *p },
	"hasPrefix": strings.HasPrefix,
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
	"bytes": humanBytes,
	"pct":   func(f float64) string { return fmt.Sprintf("%.1f%%", f) },
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
	// shared_*.html hold {{define}} blocks any page may use.
	shared, err := fs.Glob(fsys, "templates/shared_*.html")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".html")
		if name == "layout" || strings.HasPrefix(name, "shared_") {
			continue
		}
		var t *template.Template
		if strings.HasPrefix(name, "partial_") {
			t, err = template.New(name).Funcs(funcs).ParseFS(fsys, p)
		} else {
			files := append([]string{"templates/layout.html", p}, shared...)
			t, err = template.New(name).Funcs(funcs).ParseFS(fsys, files...)
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
		Nonce:   nonceFrom(r),
		Data:    data,
	}
}

// humanBytes formats a byte count the way docker stats does, in binary units.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

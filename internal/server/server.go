// Package server is the web app: routes, middleware, sessions, CSRF and the
// page handlers.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/NullAngst/Fleetling/internal/auth"
	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
	"github.com/NullAngst/Fleetling/web"
)

// Config is everything the server needs that does not live in the database.
type Config struct {
	DefaultRoot    string // FLEETLING_ROOT, pre-fills first-run setup
	DataDir        string // FLEETLING_DATA, shown on the settings page
	Version        string
	TrustedProxies []netip.Prefix
	Logger         *slog.Logger
}

// Engine is the part of engine.Client the server uses. Tests swap in a fake.
type Engine interface {
	Identify(ctx context.Context) (engine.Info, error)
	ComposeContainers(ctx context.Context, slot string) ([]compose.Container, error)
	ContainerMounts(ctx context.Context, id string) ([]compose.Mount, error)
	Close() error
}

// Server is the Fleetling web app.
type Server struct {
	cfg     Config
	log     *slog.Logger
	store   *store.Store
	tmpl    map[string]*template.Template
	limiter *auth.Limiter
	handler http.Handler

	// Hooks, replaced in tests.
	newEngine   func(endpoint string) (Engine, error)
	inContainer func() bool
	selfIDs     func() []string
	now         func() time.Time

	mu         sync.RWMutex
	secret     []byte
	setUp      bool   // a password exists
	setupToken string // one-time token for first-run setup, empty once set up
}

var errNotInContainer = errors.New("not running in a container")

// New loads state from the store and builds the handler.
func New(ctx context.Context, cfg Config, st *store.Store) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	tmpl, err := loadTemplates(web.FS)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:         cfg,
		log:         cfg.Logger,
		store:       st,
		tmpl:        tmpl,
		limiter:     auth.NewLimiter(5, 15*time.Minute, 15*time.Minute),
		newEngine:   func(ep string) (Engine, error) { return engine.New(ep) },
		inContainer: engine.InContainer,
		selfIDs:     engine.SelfIDCandidates,
		now:         time.Now,
	}

	secretHex, ok, err := st.Get(ctx, store.KeySessionSecret)
	if err != nil {
		return nil, err
	}
	if ok {
		s.secret, err = hex.DecodeString(secretHex)
	}
	if !ok || err != nil || len(s.secret) < 32 {
		if err := s.rotateSecret(ctx); err != nil {
			return nil, err
		}
	}
	_, s.setUp, err = st.Get(ctx, store.KeyPasswordHash)
	if err != nil {
		return nil, err
	}
	if !s.setUp {
		s.setupToken = hex.EncodeToString(randomBytes(12))
	}
	s.handler = s.routes()
	return s, nil
}

// SetupToken is the one-time first-run token, or "" once a password exists.
// main prints it to the log.
func (s *Server) SetupToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.setupToken
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(web.FS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("GET /setup", s.setupForm)
	mux.HandleFunc("POST /setup", s.setupSubmit)
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.requireAuth(s.logout))

	mux.HandleFunc("GET /{$}", s.requireAuth(s.stacksPage))
	mux.HandleFunc("GET /settings", s.requireAuth(s.settingsPage))
	mux.HandleFunc("POST /settings", s.requireAuth(s.settingsSave))
	mux.HandleFunc("POST /settings/test", s.requireAuth(s.engineTest))
	mux.HandleFunc("POST /settings/password", s.requireAuth(s.passwordSave))
	mux.HandleFunc("POST /settings/logout-all", s.requireAuth(s.logoutAll))

	// Outermost first: headers, cross-origin refusal, then the setup gate.
	var h http.Handler = mux
	h = s.setupGate(h)
	h = http.NewCrossOriginProtection().Handler(h)
	h = securityHeaders(h)
	return h
}

func staticHandler(fsys fs.FS) http.Handler {
	fh := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assets change with every release, and the binary is the only
		// source, so a short cache is plenty.
		w.Header().Set("Cache-Control", "public, max-age=300")
		fh.ServeHTTP(w, r)
	})
}

// securityHeaders sets a strict CSP: no inline script, no inline style, no
// framing. Everything the UI needs is served from /static.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// setupGate sends everything to /setup until a password exists, and keeps
// /setup closed afterwards.
func (s *Server) setupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		setUp := s.setUp
		s.mu.RUnlock()
		p := r.URL.Path
		open := p == "/healthz" || len(p) >= 8 && p[:8] == "/static/"
		switch {
		case open:
		case !setUp && p != "/setup":
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		case setUp && p == "/setup":
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sessionID returns the ID of a valid session cookie.
func (s *Server) sessionID(r *http.Request) ([]byte, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, false
	}
	s.mu.RLock()
	secret := s.secret
	s.mu.RUnlock()
	return verifySession(secret, c.Value, s.now())
}

func (s *Server) authed(r *http.Request) bool {
	_, ok := s.sessionID(r)
	return ok
}

// csrfToken returns the token forms on this page must send back: the
// session's token when logged in, otherwise the pre-auth cookie's.
func (s *Server) csrfToken(r *http.Request) string {
	s.mu.RLock()
	secret := s.secret
	s.mu.RUnlock()
	if id, ok := s.sessionID(r); ok {
		return csrfFor(secret, "session", id)
	}
	if c, err := r.Cookie(preauthCookie); err == nil {
		if raw, err := b64.DecodeString(c.Value); err == nil && len(raw) == 16 {
			return csrfFor(secret, "pre", raw)
		}
	}
	return ""
}

// checkCSRF compares the token in the header (HTMX) or form field against
// the one this request should carry.
func (s *Server) checkCSRF(r *http.Request) bool {
	want := s.csrfToken(r)
	got := r.Header.Get(csrfHeader)
	if got == "" {
		got = r.PostFormValue(csrfField)
	}
	return tokenEqual(want, got)
}

// ensurePreauth gives a logged-out browser a pre-auth cookie so the login
// and setup forms can carry a CSRF token. The cookie is also added to r so
// the page rendered in this same request sees it.
func (s *Server) ensurePreauth(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(preauthCookie); err == nil {
		if raw, err := b64.DecodeString(c.Value); err == nil && len(raw) == 16 {
			return
		}
	}
	c := &http.Cookie{
		Name: preauthCookie, Value: b64.EncodeToString(randomBytes(16)), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isHTTPS(r, s.cfg.TrustedProxies),
	}
	http.SetCookie(w, c)
	r.AddCookie(c)
}

// requireAuth sends logged-out requests to /login and rejects state-changing
// requests without a valid CSRF token.
func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.checkCSRF(r) {
			http.Error(w, "missing or stale CSRF token, reload the page", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	secret := s.secret
	s.mu.RUnlock()
	v, _ := newSession(secret, s.now())
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: v, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isHTTPS(r, s.cfg.TrustedProxies),
	})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// rotateSecret replaces the session secret, which ends every session.
func (s *Server) rotateSecret(ctx context.Context) error {
	secret := randomBytes(32)
	if err := s.store.Set(ctx, store.KeySessionSecret, hex.EncodeToString(secret)); err != nil {
		return err
	}
	s.mu.Lock()
	s.secret = secret
	s.mu.Unlock()
	return nil
}

func constantEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// limiterKey is the client address the login limiter counts against.
func (s *Server) limiterKey(r *http.Request) string {
	a, _ := clientAddr(r, s.cfg.TrustedProxies)
	if !a.IsValid() {
		return r.RemoteAddr
	}
	return a.String()
}

package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/NullAngst/Fleetling/internal/auth"
	"github.com/NullAngst/Fleetling/internal/store"
)

type setupData struct {
	Root    string
	Warning string
}

func (s *Server) setupForm(w http.ResponseWriter, r *http.Request) {
	s.ensurePreauth(w, r)
	s.render(w, http.StatusOK, "setup", s.page(r, "First run", "", setupData{Root: s.cfg.DefaultRoot}))
}

// maxPreauthBody caps the body of the forms reachable without a session.
// They carry a token, a password and a path; 64 KiB is far more than that.
const maxPreauthBody = 64 << 10

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPreauthBody)
	if !s.checkCSRF(r) {
		http.Error(w, "missing or stale CSRF token, reload the page", http.StatusForbidden)
		return
	}
	data := setupData{Root: r.PostFormValue("root")}
	fail := func(status int, msg string) {
		p := s.page(r, "First run", "", data)
		p.Error = msg
		s.render(w, status, "setup", p)
	}

	ip := s.limiterKey(r)
	if ok, retry := s.limiter.Allowed(ip); !ok {
		fail(http.StatusTooManyRequests, fmt.Sprintf("Too many wrong setup tokens. Try again in %s.", retry.Round(time.Minute)))
		return
	}
	s.mu.RLock()
	token := s.setupToken
	s.mu.RUnlock()
	if !constantEqual(strings.TrimSpace(r.PostFormValue("token")), token) {
		s.limiter.Fail(ip)
		fail(http.StatusUnauthorized, "Wrong setup token. It is printed in the container log: docker logs fleetling")
		return
	}

	pw := r.PostFormValue("password")
	if err := auth.CheckPasswordPolicy(pw); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if pw != r.PostFormValue("confirm") {
		fail(http.StatusBadRequest, "The two passwords do not match.")
		return
	}

	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	root, warning, err := s.checkRoot(ctx, data.Root, st)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	st.Root = root

	hash, err := auth.HashPassword(pw)
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}

	// Check and write under the lock, so two browsers racing the setup page
	// can't both set a password.
	s.mu.Lock()
	if s.setUp {
		s.mu.Unlock()
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	err = s.saveSetup(ctx, st, hash)
	if err == nil {
		s.setUp = true
		s.setupToken = ""
	}
	s.mu.Unlock()
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	s.limiter.Reset(ip)
	if warning != "" {
		s.log.Warn("setup", "root", root, "warning", warning)
	}
	s.log.Info("first-run setup finished", "root", root)
	clearCookie(w, preauthCookie)
	s.setSessionCookie(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) saveSetup(ctx context.Context, st store.Settings, hash string) error {
	if err := s.store.SaveSettings(ctx, st); err != nil {
		return err
	}
	return s.store.Set(ctx, store.KeyPasswordHash, hash)
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if s.authed(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.ensurePreauth(w, r)
	s.render(w, http.StatusOK, "login", s.page(r, "Log in", "", nil))
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPreauthBody)
	fail := func(status int, msg string) {
		p := s.page(r, "Log in", "", nil)
		p.Error = msg
		s.render(w, status, "login", p)
	}
	if !s.checkCSRF(r) {
		http.Error(w, "missing or stale CSRF token, reload the page", http.StatusForbidden)
		return
	}
	ip := s.limiterKey(r)
	if ok, retry := s.limiter.Allowed(ip); !ok {
		fail(http.StatusTooManyRequests, fmt.Sprintf("Too many failed logins from %s. Try again in %s.", ip, retry.Round(time.Minute)))
		return
	}
	hash, _, err := s.store.Get(r.Context(), store.KeyPasswordHash)
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	ok, err := auth.VerifyPassword(hash, r.PostFormValue("password"))
	if err != nil {
		s.log.Error("stored password hash is unreadable", "err", err)
		fail(http.StatusInternalServerError, "The stored password hash is unreadable.")
		return
	}
	if !ok {
		if s.limiter.Fail(ip) {
			s.log.Warn("login lockout", "ip", ip)
		}
		fail(http.StatusUnauthorized, "Wrong password.")
		return
	}
	s.limiter.Reset(ip)
	clearCookie(w, preauthCookie)
	s.setSessionCookie(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) logoutAll(w http.ResponseWriter, r *http.Request) {
	if err := s.rotateSecret(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("session secret rotated, every session logged out")
	clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

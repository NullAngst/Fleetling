package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/NullAngst/Fleetling/internal/auth"
	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
)

type settingsData struct {
	Root          string
	DockerHost    string
	PodmanHost    string
	Ignore        string
	DataDir       string
	DefaultDocker string
	DefaultPodman string
}

func (s *Server) settingsData(st store.Settings) settingsData {
	return settingsData{
		Root:          st.Root,
		DockerHost:    st.DockerHost,
		PodmanHost:    st.PodmanHost,
		Ignore:        strings.Join(st.Ignore, "\n"),
		DataDir:       s.cfg.DataDir,
		DefaultDocker: store.DefaultDockerHost,
		DefaultPodman: store.DefaultPodmanHost,
	}
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.LoadSettings(r.Context(), s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p := s.page(r, "Settings", "settings", s.settingsData(st))
	switch r.URL.Query().Get("saved") {
	case "settings":
		p.Notice = "Settings saved."
	case "password":
		p.Notice = "Password changed. Every other session was logged out."
	}
	s.render(w, http.StatusOK, "settings", p)
}

type confirmRootData struct {
	Form    settingsData
	OldRoot string
	Moving  []compose.Folder
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	old, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	next := store.Settings{
		Root:       strings.TrimSpace(r.PostFormValue("root")),
		DockerHost: strings.TrimSpace(r.PostFormValue("docker_host")),
		PodmanHost: strings.TrimSpace(r.PostFormValue("podman_host")),
		Ignore:     store.ParseList(r.PostFormValue("ignore")),
	}
	fail := func(msg string) {
		d := s.settingsData(next)
		d.Ignore = r.PostFormValue("ignore")
		p := s.page(r, "Settings", "settings", d)
		p.Error = msg
		s.render(w, http.StatusBadRequest, "settings", p)
	}

	if err := s.checkEndpoints(ctx, next); err != nil {
		fail(err.Error())
		return
	}
	// The mount check looks the container up through the new endpoints, so
	// it runs after they are known to answer.
	root, warning, err := s.checkRoot(ctx, next.Root, next)
	if err != nil {
		fail(err.Error())
		return
	}
	next.Root = root

	if next.Root != old.Root && r.PostFormValue("confirm_root") != "yes" {
		// Changing the root moves nothing. Show what will drop to External
		// before saving.
		folders, _ := compose.Discover(old.Root, old.Ignore)
		d := s.settingsData(next)
		d.Ignore = strings.Join(next.Ignore, "\n")
		p := s.page(r, "Change stack root", "settings", confirmRootData{Form: d, OldRoot: old.Root, Moving: folders})
		p.Notice = warning
		s.render(w, http.StatusOK, "confirm_root", p)
		return
	}

	if err := s.store.SaveSettings(ctx, next); err != nil {
		fail(err.Error())
		return
	}
	if warning != "" {
		s.log.Warn("settings", "warning", warning)
	}
	s.log.Info("settings saved", "root", next.Root, "docker", next.DockerHost, "podman", next.PodmanHost)
	http.Redirect(w, r, "/settings?saved=settings", http.StatusSeeOther)
}

// checkEndpoints enforces the spec's rule: both endpoints are optional, but
// at least one has to answer.
func (s *Server) checkEndpoints(ctx context.Context, st store.Settings) error {
	eps := endpoints(st)
	if len(eps) == 0 {
		return errors.New("set at least one engine endpoint")
	}
	if st.DockerHost != "" && st.PodmanHost != "" {
		a, _, errA := engine.NormalizeHost(st.DockerHost)
		b, _, errB := engine.NormalizeHost(st.PodmanHost)
		if errA == nil && errB == nil && a == b {
			return errors.New("the Docker and Podman endpoints point at the same socket, clear one of them")
		}
	}
	var problems []string
	up := 0
	for _, ep := range eps {
		if _, _, err := engine.NormalizeHost(ep.Host); err != nil {
			return fmt.Errorf("%s endpoint: %w", ep.Slot, err)
		}
		if _, err := s.identify(ctx, ep.Host); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", ep.Slot, err))
			continue
		}
		up++
	}
	if up == 0 {
		return fmt.Errorf("no engine endpoint answered: %s", strings.Join(problems, "; "))
	}
	return nil
}

func (s *Server) identify(ctx context.Context, host string) (engine.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e, err := s.newEngine(host)
	if err != nil {
		return engine.Info{}, err
	}
	defer e.Close()
	return e.Identify(ctx)
}

type engineTestData struct {
	Slot string
	Info engine.Info
	Err  string
	Note string
}

// engineTest answers the Test button next to an endpoint field. It tests
// what is typed in the field, saved or not.
func (s *Server) engineTest(w http.ResponseWriter, r *http.Request) {
	slot := r.PostFormValue("slot")
	field := "docker_host"
	if slot == compose.EnginePodman {
		field = "podman_host"
	} else {
		slot = compose.EngineDocker
	}
	d := engineTestData{Slot: slot}
	host := strings.TrimSpace(r.PostFormValue(field))
	if host == "" {
		d.Err = "empty, this endpoint is off"
		s.render(w, http.StatusOK, "partial_engine_test", d)
		return
	}
	info, err := s.identify(r.Context(), host)
	if err != nil {
		d.Err = err.Error()
	} else {
		d.Info = info
		switch {
		case slot == compose.EngineDocker && info.Kind == engine.KindPodman:
			d.Note = "This is Podman answering on the Docker path (podman-docker). It works, but put it in the Podman field instead so builds and other Podman-specific handling apply."
		case slot == compose.EnginePodman && info.Kind == engine.KindDocker:
			d.Note = "This is Docker, not Podman. Put it in the Docker field."
		}
	}
	s.render(w, http.StatusOK, "partial_engine_test", d)
}

func (s *Server) passwordSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fail := func(status int, msg string) {
		st, _ := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
		p := s.page(r, "Settings", "settings", s.settingsData(st))
		p.Error = msg
		s.render(w, status, "settings", p)
	}
	ip := s.limiterKey(r)
	if ok, retry := s.limiter.Allowed(ip); !ok {
		fail(http.StatusTooManyRequests, fmt.Sprintf("Too many wrong passwords. Try again in %s.", retry.Round(time.Minute)))
		return
	}
	hash, _, err := s.store.Get(ctx, store.KeyPasswordHash)
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	if ok, err := auth.VerifyPassword(hash, r.PostFormValue("current")); err != nil || !ok {
		s.limiter.Fail(ip)
		fail(http.StatusUnauthorized, "The current password is wrong.")
		return
	}
	pw := r.PostFormValue("password")
	if err := auth.CheckPasswordPolicy(pw); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if pw != r.PostFormValue("confirm") {
		fail(http.StatusBadRequest, "The two new passwords do not match.")
		return
	}
	newHash, err := auth.HashPassword(pw)
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.Set(ctx, store.KeyPasswordHash, newHash); err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	// A new password ends every other session. This browser gets a fresh one.
	if err := s.rotateSecret(ctx); err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	s.limiter.Reset(ip)
	s.setSessionCookie(w, r)
	s.log.Info("password changed")
	http.Redirect(w, r, "/settings?saved=password", http.StatusSeeOther)
}

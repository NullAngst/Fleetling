package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	fleetling "github.com/NullAngst/Fleetling"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/release"
)

type aboutEngine struct {
	Slot    string
	Host    string
	Info    engine.SystemInfo
	Compose string // the Compose plugin's version
	Err     string
	Missing bool // no socket at that path: the engine isn't installed, not broken
}

type aboutData struct {
	Version   string
	Next      string // the next release's version, by the numbering
	Repo      string
	GoVersion string
	Platform  string // linux/amd64
	Build     [][2]string
	Started   time.Time
	Uptime    string
	PID       int
	Threads   int
	Mem       string // Go heap in use
	OSMem     string // what the Go runtime holds from the OS

	InContainer bool
	Container   string
	Image       string
	ImageBuilt  time.Time
	Hostname    string // the container's or machine's own name

	Root      string
	DataDir   string
	DBSize    string
	TLS       bool // this page came over HTTPS
	Proxies   []string
	Origins   []string
	GitHubOff bool
	Engines   []aboutEngine
}

func (s *Server) aboutPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := aboutData{
		Version:   s.cfg.Version,
		Repo:      "https://github.com/" + release.Repo,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Started:   s.start,
		Uptime:    roughDuration(s.now().Sub(s.start)),
		PID:       os.Getpid(),
		Threads:   runtime.NumGoroutine(),
		Root:      st.Root,
		DataDir:   s.cfg.DataDir,
		TLS:       r.TLS != nil,
		Origins:   s.cfg.Origins,
		GitHubOff: !s.githubCheckOn(ctx),
	}
	for _, p := range s.cfg.TrustedProxies {
		d.Proxies = append(d.Proxies, p.String())
	}
	if v, err := fleetling.Parse(s.cfg.Version); err == nil {
		d.Next = v.Next().String()
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	d.Mem, d.OSMem = humanBytes(ms.HeapInuse), humanBytes(ms.Sys)
	d.Hostname, _ = os.Hostname()
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, kv := range bi.Settings {
			switch kv.Key {
			case "-trimpath", "CGO_ENABLED", "GOAMD64", "GOARM64", "vcs.revision", "vcs.time", "vcs.modified":
				d.Build = append(d.Build, [2]string{kv.Key, kv.Value})
			}
		}
		d.Build = append(d.Build, [2]string{"modules", strconv.Itoa(len(bi.Deps))})
	}
	if fi, err := os.Stat(filepath.Join(s.cfg.DataDir, "fleetling.db")); err == nil {
		d.DBSize = humanBytes(uint64(fi.Size()))
	}
	d.InContainer = s.inContainer()
	if si, ok := s.self(ctx, st); ok {
		d.Container = si.ContainerID
		if id, err := s.selfImage(ctx, st, si); err == nil {
			d.Image = id
			if e, err := s.newEngine(hostFor(st, si.Slot)); err == nil {
				if info, err := e.ImageInfo(ctx, id); err == nil {
					d.ImageBuilt = info.Created
				}
				e.Close()
			}
		}
	}

	eps := endpoints(st)
	d.Engines = make([]aboutEngine, len(eps))
	var wg sync.WaitGroup
	for i, ep := range eps {
		d.Engines[i] = aboutEngine{Slot: ep.Slot, Host: ep.Host}
		wg.Go(func() {
			ae := &d.Engines[i]
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			e, err := s.newEngine(ep.Host)
			if err != nil {
				ae.Err = err.Error()
				return
			}
			defer e.Close()
			if ae.Info, err = e.SystemInfo(cctx); err != nil {
				ae.Err, ae.Missing = err.Error(), errors.Is(err, engine.ErrSocketMissing)
				return
			}
			out, err := s.runner.Plain(cctx, ep.Host, ep.Slot == "podman", "compose", "version", "--short").Output()
			if err == nil {
				ae.Compose = strings.TrimSpace(string(out))
			}
		})
	}
	wg.Wait()
	s.render(w, http.StatusOK, "about", s.page(r, "About", "about", d))
}

// roughDuration is an uptime for people: "3 days, 4 hours".
func roughDuration(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return strconv.Itoa(n) + " " + unit + "s"
	}
	switch {
	case days > 0:
		return plural(days, "day") + ", " + plural(hours, "hour")
	case hours > 0:
		return plural(hours, "hour") + ", " + plural(mins, "minute")
	}
	return plural(mins, "minute")
}

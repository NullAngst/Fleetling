package server

import (
	"net/http"

	"github.com/NullAngst/Fleetling/internal/compose"
)

type stacksData struct {
	Root     string
	Stacks   []compose.Stack
	Engines  []engineStatus
	ScanErr  string
	Managed  int
	OnDisk   int
	External int
}

// stacksPage is the home page. Read-only in phase 1: it scans the root,
// asks each engine what Compose projects exist, and lines the two up.
func (s *Server) stacksPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := stacksData{Root: st.Root}

	folders, err := compose.Discover(st.Root, st.Ignore)
	if err != nil {
		d.ScanErr = err.Error()
	}
	containers, status := s.gather(ctx, st)
	d.Engines = status

	reachable := map[string]bool{}
	for _, e := range status {
		reachable[e.Slot] = e.Up
	}
	d.Stacks = compose.Merge(folders, containers, reachable)
	byDir := map[string]compose.Folder{}
	for _, f := range folders {
		byDir[f.Dir] = f
	}
	for i := range d.Stacks {
		if f, ok := byDir[d.Stacks[i].Dir]; ok && f.Meta != nil {
			if rs, err := s.reviewFor(ctx, f); err == nil {
				d.Stacks[i].Review = rs.State
			}
		}
	}
	for _, st := range d.Stacks {
		switch st.Kind {
		case compose.KindManaged:
			d.Managed++
		case compose.KindOnDisk:
			d.OnDisk++
		case compose.KindExternal:
			d.External++
		}
	}
	s.render(w, http.StatusOK, "stacks", s.page(r, "Stacks", "stacks", d))
}

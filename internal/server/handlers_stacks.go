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
	Counts   []labelCount // one per status present, for the filter chips
}

type labelCount struct {
	Label string
	State string // dot class: running, partial, stopped, ondisk, external, unknown
	N     int
}

// labelOrder is the order the filter chips appear in.
var labelOrder = []string{"Running", "Partial", "Stopped", "On disk", "External", "Unknown"}

func countLabels(stacks []compose.Stack) []labelCount {
	n := map[string]int{}
	state := map[string]string{}
	for _, st := range stacks {
		n[st.Label()]++
		state[st.Label()] = dotClass(st)
	}
	var out []labelCount
	for _, l := range labelOrder {
		if n[l] > 0 {
			out = append(out, labelCount{Label: l, State: state[l], N: n[l]})
		}
	}
	return out
}

// dotClass is the status dot for a stack row.
func dotClass(st compose.Stack) string {
	if st.Kind == compose.KindManaged {
		return string(st.State)
	}
	return string(st.Kind)
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
	d.Counts = countLabels(d.Stacks)
	s.render(w, http.StatusOK, "stacks", s.page(r, "Stacks", "stacks", d))
}

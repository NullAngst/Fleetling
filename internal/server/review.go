package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/store"
)

// Drift review. Fleetling records each stack's deployment files whenever it
// writes them itself (create, save, manage, import, path tracking) or the
// user approves them. Any compose action first compares the files on disk
// with that record. If they differ, or nothing was ever recorded, the
// action is refused and the stack is flagged until the user reviews the
// changes. The scheduled auto-update uses the same check
// and skips a flagged stack instead of deploying it.
//
// The check runs when an action is asked for and again before every step
// of the job, since a pull or a build can take minutes and a container can
// rewrite its own folder in that time. When Fleetling writes a file it
// records only that file, from the bytes it wrote (recordWrites), so a
// change something else made in the meantime stays flagged.

// Review states.
const (
	reviewOK         = ""
	reviewModified   = "modified"   // files changed outside Fleetling since the last approval
	reviewUnrecorded = "unrecorded" // managed before review existed, or never approved
)

// errNeedsReview means a compose action was refused until the user reviews
// the stack's files.
var errNeedsReview = errors.New("this stack's files changed outside Fleetling; review them before running anything")

type reviewState struct {
	State    string
	Approved compose.Snapshot
	Current  compose.Snapshot
	Changes  []compose.FileChange
	Recorded store.Baseline
}

// reviewFor compares a managed stack's files with its approved snapshot.
// Unmanaged folders have nothing to deploy and always come back OK.
func (s *Server) reviewFor(ctx context.Context, f compose.Folder) (reviewState, error) {
	if f.Meta == nil {
		return reviewState{}, nil
	}
	cur, err := compose.TakeSnapshot(f)
	if err != nil {
		return reviewState{}, err
	}
	rs := reviewState{Current: cur}
	b, ok, err := s.store.GetBaseline(ctx, f.Name)
	if err != nil {
		return rs, err
	}
	if !ok {
		rs.State = reviewUnrecorded
		return rs, nil
	}
	rs.Recorded = b
	if err := json.Unmarshal([]byte(b.Snapshot), &rs.Approved); err != nil {
		rs.State = reviewUnrecorded
		return rs, nil
	}
	rs.Changes = compose.Changes(rs.Approved, cur)
	if len(rs.Changes) > 0 {
		rs.State = reviewModified
	}
	return rs, nil
}

// recordBaseline takes the folder's current files, all of them, as
// approved, except the ones in wrote, which are recorded from the bytes
// Fleetling wrote. Only for the moment the user vouches for the whole
// folder: Manage. Anywhere Fleetling has just written files into a stack
// it already tracks, use recordWrites, which approves only what Fleetling
// wrote.
func (s *Server) recordBaseline(ctx context.Context, dir, source string, wrote map[string][]byte) error {
	return s.record(ctx, dir, source, wrote, false)
}

// recordWrites updates a stack's approved snapshot after Fleetling wrote
// files into its folder. wrote maps each file, named the way the snapshot
// names it (relative to the stack folder), to the exact bytes Fleetling
// wrote.
//
// Those files are recorded from the bytes, never read back from disk, so
// a container that rewrites one a moment after Fleetling did still shows
// up as drift. Every other file keeps the state it was last approved in:
// re-reading the whole folder here would quietly approve anything that
// changed while a job ran, which is exactly what drift review is for. A
// file the old record never had, like an env_file a freshly saved compose
// file names for the first time, is recorded as it is now.
func (s *Server) recordWrites(ctx context.Context, dir, source string, wrote map[string][]byte) error {
	return s.record(ctx, dir, source, wrote, true)
}

func (s *Server) record(ctx context.Context, dir, source string, wrote map[string][]byte, keepApproved bool) error {
	f, ok := compose.ReadFolder(dir)
	if !ok {
		return fmt.Errorf("%s is not a stack folder", dir)
	}
	cur, err := compose.TakeSnapshot(f)
	if err != nil {
		return err
	}
	approved := map[string]compose.FileState{}
	if keepApproved {
		b, ok, err := s.store.GetBaseline(ctx, f.Name)
		if err != nil {
			return err
		}
		if ok {
			var old compose.Snapshot
			if json.Unmarshal([]byte(b.Snapshot), &old) == nil {
				for _, fs := range old.Files {
					approved[fs.Path] = fs
				}
			}
		}
	}
	for i, fs := range cur.Files {
		if data, ok := wrote[fs.Path]; ok {
			st := compose.StateOf(data)
			st.Path = fs.Path
			cur.Files[i] = st
		} else if old, ok := approved[fs.Path]; ok {
			cur.Files[i] = old
		}
	}
	return s.saveSnapshot(ctx, f.Name, source, cur)
}

func (s *Server) saveSnapshot(ctx context.Context, folder, source string, snap compose.Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return s.store.SetBaseline(ctx, store.Baseline{Folder: folder, Recorded: s.now(), Source: source, Snapshot: string(b)})
}

// reviewedOrErr is the gate every compose action passes through.
func (s *Server) reviewedOrErr(ctx context.Context, f compose.Folder) error {
	rs, err := s.reviewFor(ctx, f)
	if err != nil {
		return fmt.Errorf("could not check the stack's files: %w", err)
	}
	if rs.State != reviewOK {
		return errNeedsReview
	}
	return nil
}

// driftGuard is the JobSpec.Guard for a stack job: the folder is read
// again, since a new compose file name changes what Compose loads, and has
// to pass review the same way it did when the job was asked for.
func (s *Server) driftGuard(f compose.Folder) func(context.Context) error {
	return func(ctx context.Context) error {
		cur, ok := compose.ReadFolder(f.Dir)
		if !ok || cur.Meta == nil {
			return fmt.Errorf("%s is no longer a managed stack folder", f.Dir)
		}
		if cur.ComposeFile != f.ComposeFile {
			return fmt.Errorf("the compose file is now %s, not %s; %w", cur.ComposeFile, f.ComposeFile, errNeedsReview)
		}
		return s.reviewedOrErr(ctx, cur)
	}
}

type fileReview struct {
	compose.FileChange
	Rows []diffRow
}

type reviewData struct {
	Folder      compose.Folder
	State       string
	Files       []fileReview
	Current     compose.Snapshot
	Fingerprint string
	Recorded    store.Baseline
	Action      string
}

func (s *Server) reviewPage(w http.ResponseWriter, r *http.Request) {
	s.renderReview(w, r, http.StatusOK, "")
}

func (s *Server) renderReview(w http.ResponseWriter, r *http.Request, status int, msg string) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, ok := loadFolder(st, r.PathValue("folder"))
	if !ok || f.Meta == nil {
		http.NotFound(w, r)
		return
	}
	rs, err := s.reviewFor(ctx, f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := reviewData{Folder: f, State: rs.State, Current: rs.Current, Fingerprint: rs.Current.Fingerprint(), Recorded: rs.Recorded, Action: r.URL.Query().Get("action")}
	for _, ch := range rs.Changes {
		fr := fileReview{FileChange: ch}
		if ch.Diffable {
			fr.Rows, _, _ = sideBySide(ch.Old.Content, ch.New.Content)
		}
		d.Files = append(d.Files, fr)
	}
	p := s.page(r, "Review "+f.Name, "stacks", d)
	p.Error = msg
	s.render(w, status, "review", p)
}

// reviewApprove records the files exactly as the reviewer saw them. If they
// changed again in the meantime, the page is shown again with the new diff.
func (s *Server) reviewApprove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, ok := loadFolder(st, r.PathValue("folder"))
	if !ok || f.Meta == nil {
		http.NotFound(w, r)
		return
	}
	cur, err := compose.TakeSnapshot(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if cur.Fingerprint() != r.PostFormValue("fingerprint") {
		s.renderReview(w, r, http.StatusConflict, "the files changed again while you were reviewing. This is the new state; review it again")
		return
	}
	rs, _ := s.reviewFor(ctx, f)
	var names []string
	for _, ch := range rs.Changes {
		names = append(names, ch.Path)
	}
	what := "fleetling: approved the current files of " + f.Dir
	if len(names) > 0 {
		what = "fleetling: approved changes made outside Fleetling to " + strings.Join(names, ", ") + " in " + f.Dir
	}
	err = s.saveSnapshot(ctx, f.Name, "approve", cur)
	s.logFileAction(ctx, f.Name, f.Meta.Engine, what, err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/stacks/"+f.Name+"?saved=approved", http.StatusSeeOther)
}

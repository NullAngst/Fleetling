package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
	"github.com/NullAngst/Fleetling/internal/updater"
)

// Self-management. Fleetling's own stack allows Update and Restart, both
// run by a helper container (see internal/updater), and Edit. Stop, Down
// and Remove stay off: they would end Fleetling with nothing left running
// to finish the job or bring it back.

// selfDisabled are the actions the Self stack shows greyed out, with the
// reason next to each.
var selfDisabled = []struct{ Key, Label, Reason string }{
	{"stop", "Stop", "Stopping Fleetling from inside leaves nothing running to start it again."},
	{"down", "Down", "Down removes Fleetling's own container while it is running the command."},
	{"remove", "Remove", "Removing Fleetling from inside would cut it off halfway through."},
}

// errHelperBusy means a helper container already exists.
var errHelperBusy = errors.New("an update helper is already running (container " + updater.HelperName + "). Wait for it to finish; if it is stuck, check `docker logs " + updater.HelperName + "` and remove it with `docker rm -f " + updater.HelperName + "`")

// selfHelperArgs is the full `docker run` argv that starts the helper. The
// engine socket and the root are mounted at the same paths they have
// outside, so the compose file's paths mean the same thing in the helper.
// image is Fleetling's own image ID, not its tag: the build moves the tag,
// and the helper must keep running the old code while it rolls back.
func (s *Server) selfHelperArgs(st store.Settings, t compose.Target, f compose.Folder, image, mode string) ([]string, error) {
	_, sock, err := engine.NormalizeHost(t.Host)
	if err != nil {
		return nil, err
	}
	args := []string{"docker", "run", "--rm", "-d", "--name", updater.HelperName}
	if sock != "" {
		args = append(args, "-v", sock+":"+sock)
	}
	args = append(args, "-v", st.Root+":"+st.Root)
	data := filepath.Clean(s.cfg.DataDir)
	if data != "" && data != "." && !isUnderDir(data, st.Root) {
		args = append(args, "-v", data+":"+data)
	}
	args = append(args,
		"-e", "FLEETLING_ROOT="+st.Root,
		"-e", "FLEETLING_DATA="+data,
		"-e", "DOCKER_HOST="+t.Host,
		image, "updater", "--dir", f.Dir, "--mode", mode)
	return args, nil
}

func isUnderDir(p, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// selfImage reads the image ID Fleetling's own container runs.
func (s *Server) selfImage(ctx context.Context, st store.Settings, si selfInfo) (string, error) {
	e, err := s.newEngine(hostFor(st, si.Slot))
	if err != nil {
		return "", err
	}
	defer e.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d, err := e.Inspect(ctx, si.ContainerID)
	if err != nil {
		return "", fmt.Errorf("could not read Fleetling's own image: %w", err)
	}
	if d.ImageID == "" {
		return "", errors.New("the Fleetling container reports no image ID")
	}
	return d.ImageID, nil
}

// selfCommand is the helper command shown under the Update and Restart
// buttons. It needs the engine, so a failure shows as the reason instead.
func (s *Server) selfCommand(ctx context.Context, st store.Settings, f compose.Folder, si selfInfo, mode string) (string, error) {
	t, err := s.target(st, f)
	if err != nil {
		return "", err
	}
	image, err := s.selfImage(ctx, st, si)
	if err != nil {
		return "", err
	}
	args, err := s.selfHelperArgs(st, t, f, image, mode)
	if err != nil {
		return "", err
	}
	return compose.Quote(args), nil
}

// runSelfHelper starts the helper container. It passes the same drift gate
// as every other compose action: an unreviewed change to Fleetling's own
// compose file blocks its own update.
func (s *Server) runSelfHelper(ctx context.Context, st store.Settings, f compose.Folder, si selfInfo, mode string) error {
	t, err := s.target(st, f)
	if err != nil {
		return err
	}
	if s.jobs.Running(f.Name) != nil {
		return compose.ErrBusy
	}
	if err := s.reviewedOrErr(ctx, f); err != nil {
		return err
	}
	e, err := s.newEngine(hostFor(st, si.Slot))
	if err != nil {
		return err
	}
	exists, err := e.Exists(ctx, updater.HelperName)
	e.Close()
	if err != nil {
		return fmt.Errorf("could not check for a running helper: %w", err)
	}
	if exists {
		return errHelperBusy
	}
	image, err := s.selfImage(ctx, st, si)
	if err != nil {
		return err
	}
	args, err := s.selfHelperArgs(st, t, f, image, mode)
	if err != nil {
		return err
	}
	line := compose.Quote(args)
	started := s.now()
	id, err := s.store.StartAction(ctx, store.Action{Started: started, Target: f.Name, Engine: f.Meta.Engine, Command: line})
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, runErr := s.runner.Plain(rctx, t.Host, t.Podman, args[1:]...).CombinedOutput()
	exit := 0
	if runErr != nil {
		exit = 1
	}
	msg := strings.TrimSpace(string(out))
	if exit == 0 {
		msg += "\nfleetling: helper started; it writes every step to " + filepath.Join(s.cfg.DataDir, updater.LogFile)
	}
	if err := s.store.FinishAction(context.Background(), id, s.now(), exit, msg); err != nil {
		s.log.Error("action log", "err", err)
	}
	if runErr != nil {
		return fmt.Errorf("starting the helper failed: %v %s", runErr, msg)
	}
	s.log.Info("self "+mode+" helper started", "stack", f.Name)
	return nil
}

type selfUpdatingData struct {
	Mode string
	Boot string
}

// selfUpdatingPage waits for the new process. Its script polls /healthz
// until a different boot ID answers, then moves to the log page.
func (s *Server) selfUpdatingPage(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode != updater.ModeRestart {
		mode = updater.ModeUpdate
	}
	s.render(w, http.StatusOK, "self_updating", s.page(r, "Updating Fleetling", "stacks", selfUpdatingData{Mode: mode, Boot: s.boot}))
}

type lastUpdateData struct {
	Path    string
	Log     string
	Missing bool
	Failed  bool
}

func (s *Server) readLastUpdate() lastUpdateData {
	p := filepath.Join(s.cfg.DataDir, updater.LogFile)
	d := lastUpdateData{Path: p}
	b, err := readCapped(p, 1<<20)
	if err != nil {
		d.Missing = true
		return d
	}
	d.Log = string(b)
	d.Failed = strings.Contains(d.Log, "FAILED:")
	return d
}

func (s *Server) lastUpdatePage(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "Last update", "stacks", s.readLastUpdate())
	if r.URL.Query().Get("done") == "1" {
		p.Notice = "Fleetling is back."
	}
	s.render(w, http.StatusOK, "last_update", p)
}

// lastUpdateLog is the raw log, for the waiting page to show progress.
func (s *Server) lastUpdateLog(w http.ResponseWriter, r *http.Request) {
	d := s.readLastUpdate()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if d.Missing {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Write([]byte(d.Log))
}

// readCapped reads at most the last n bytes of a file.
func readCapped(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > n {
		if _, err := f.Seek(fi.Size()-n, 0); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(f, n))
}

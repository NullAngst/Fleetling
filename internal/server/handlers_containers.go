package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
)

// engineFor opens the engine behind a slot name from the URL.
func (s *Server) engineFor(ctx context.Context, slot string) (Engine, store.Settings, error) {
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		return nil, st, err
	}
	host := hostFor(st, slot)
	if host == "" {
		return nil, st, fmt.Errorf("the %q endpoint is not configured", slot)
	}
	e, err := s.newEngine(host)
	return e, st, err
}

type containersData struct {
	Slots    []string
	Slot     string
	Rows     []engine.ContainerRow
	Projects []string
	Err      string
}

func (s *Server) containersPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := containersData{Slots: engineSlots(st), Slot: r.URL.Query().Get("engine")}
	if !slices.Contains(d.Slots, d.Slot) && len(d.Slots) > 0 {
		d.Slot = d.Slots[0]
	}
	if d.Slot != "" {
		e, _, err := s.engineFor(ctx, d.Slot)
		if err == nil {
			defer e.Close()
			lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			d.Rows, err = e.Containers(lctx)
			cancel()
		}
		if err != nil {
			d.Err = err.Error()
		}
	}
	seen := map[string]bool{}
	for _, c := range d.Rows {
		if c.Project != "" && !seen[c.Project] {
			seen[c.Project] = true
			d.Projects = append(d.Projects, c.Project)
		}
	}
	slices.Sort(d.Projects)
	s.render(w, http.StatusOK, "containers", s.page(r, "Containers", "containers", d))
}

type containerAction struct {
	Key     string
	Label   string
	Command string
	Danger  bool
}

type containerData struct {
	Slot      string
	Detail    engine.Detail
	Tab       string
	Self      bool
	Folder    string // stack folder under the root, when the container belongs to one
	Managed   bool
	Actions   []containerAction
	JobAction string
}

// stackFolderFor finds the stack folder a container belongs to: by its
// working_dir label first, then by a managed folder's project name.
func stackFolderFor(st store.Settings, slot string, d engine.Detail) (compose.Folder, bool) {
	if d.Project == "" {
		return compose.Folder{}, false
	}
	folders, _ := compose.Discover(st.Root, st.Ignore)
	for _, f := range folders {
		if d.WorkDir != "" && f.Dir == d.WorkDir {
			return f, true
		}
	}
	for _, f := range folders {
		if f.Meta != nil && f.Meta.Project == d.Project && f.Meta.Engine == slot {
			return f, true
		}
	}
	return compose.Folder{}, false
}

// isSelfContainer reports whether id is Fleetling's own container.
func (s *Server) isSelfContainer(id string) bool {
	if !s.inContainer() {
		return false
	}
	for _, c := range s.selfIDs() {
		if len(c) >= 12 && strings.HasPrefix(id, c) {
			return true
		}
	}
	return false
}

func (s *Server) containerPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slot := r.PathValue("slot")
	e, st, err := s.engineFor(ctx, slot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer e.Close()
	d, err := e.Inspect(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	cd := containerData{Slot: slot, Detail: d, Tab: r.URL.Query().Get("tab"), Self: s.isSelfContainer(d.ID)}
	switch cd.Tab {
	case "shell", "inspect":
	default:
		cd.Tab = "logs"
	}
	if f, ok := stackFolderFor(st, slot, d); ok {
		cd.Folder, cd.Managed = f.Name, f.Meta != nil
	}
	for _, a := range []struct {
		key, label string
		danger     bool
	}{{"start", "Start", false}, {"stop", "Stop", false}, {"restart", "Restart", false}, {"kill", "Kill", true}, {"remove", "Remove", true}} {
		cd.Actions = append(cd.Actions, containerAction{Key: a.key, Label: a.label, Danger: a.danger, Command: cliLine(a.key, d.Name)})
	}
	if cd.Managed {
		if f, ok := loadFolder(st, cd.Folder); ok {
			if t, err := s.target(st, f); err == nil && d.Service != "" {
				cd.Actions = append(cd.Actions, containerAction{Key: "recreate", Label: "Recreate", Command: compose.Quote(s.runner.Args(t, recreateStep(d.Service)...))})
			}
		}
	}
	p := s.page(r, d.Name, "containers", cd)
	p.Shell = cd.Tab == "shell"
	s.render(w, http.StatusOK, "container", p)
}

func recreateStep(service string) []string {
	return []string{"up", "-d", "--force-recreate", "--no-deps", service}
}

// cliLine is the docker command equivalent to an Engine API call, shown
// before it runs and written to the action log.
func cliLine(action, name string) string {
	switch action {
	case "kill":
		return compose.Quote([]string{"docker", "kill", name})
	case "remove":
		return compose.Quote([]string{"docker", "rm", "-f", name})
	}
	return compose.Quote([]string{"docker", action, name})
}

func (s *Server) containerAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slot, action := r.PathValue("slot"), r.PathValue("action")
	e, st, err := s.engineFor(ctx, slot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer e.Close()
	d, err := e.Inspect(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if s.isSelfContainer(d.ID) && action != "start" {
		http.Error(w, "this is Fleetling's own container; stopping or replacing it from inside would cut this request off", http.StatusConflict)
		return
	}

	if action == "recreate" {
		f, ok := stackFolderFor(st, slot, d)
		if !ok || f.Meta == nil || d.Service == "" {
			http.Error(w, "recreate needs the container's stack to be managed by Fleetling", http.StatusBadRequest)
			return
		}
		job, err := s.runAction(ctx, st, f, stackAction{Key: "recreate-" + d.Service, Label: "Recreate " + d.Service, Steps: [][]string{recreateStep(d.Service)}})
		if errors.Is(err, errNeedsReview) {
			http.Redirect(w, r, "/stacks/"+f.Name+"/review", http.StatusSeeOther)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/stacks/"+f.Name+"?job="+job.ID, http.StatusSeeOther)
		return
	}

	fns := map[string]func(context.Context, string) error{
		"start": e.Start, "stop": e.Stop, "restart": e.Restart, "kill": e.Kill, "remove": e.Remove,
	}
	fn, ok := fns[action]
	if !ok {
		http.NotFound(w, r)
		return
	}
	actx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = fn(actx, d.ID)
	cancel()
	s.logFileAction(ctx, "container/"+d.Name, slot, cliLine(action, d.Name), err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if action == "remove" {
		http.Redirect(w, r, "/containers?engine="+slot, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/containers/"+slot+"/"+d.ID, http.StatusSeeOther)
}

func (s *Server) containerStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, _, err := s.engineFor(ctx, r.PathValue("slot"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer e.Close()
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := e.Stats(sctx, r.PathValue("id"))
	data := map[string]any{"Stats": st, "Err": ""}
	if err != nil {
		data["Err"] = err.Error()
	}
	s.render(w, http.StatusOK, "partial_stats", data)
}

type logLine struct {
	Err  bool   `json:"e,omitempty"`
	Text string `json:"t"`
}

type logMessage struct {
	Lines []logLine `json:"lines,omitempty"`
	Done  bool      `json:"done,omitempty"`
	Error string    `json:"error,omitempty"`
}

// streamLines sends lines from ch to the browser in batches until ch closes.
func streamLines(ctx context.Context, c *websocket.Conn, ch <-chan logLine) error {
	for {
		l, ok := <-ch
		if !ok {
			return nil
		}
		batch := []logLine{l}
	drain:
		for len(batch) < 500 {
			select {
			case l, ok := <-ch:
				if !ok {
					break drain
				}
				batch = append(batch, l)
			default:
				break drain
			}
		}
		if err := writeJSON(ctx, c, logMessage{Lines: batch}); err != nil {
			return err
		}
	}
}

func (s *Server) containerLogsSocket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, _, err := s.engineFor(ctx, r.PathValue("slot"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer e.Close()
	d, err := e.Inspect(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	tail := q.Get("tail")
	if _, err := strconv.Atoi(tail); err != nil && tail != "all" {
		tail = "100"
	}
	since, err := engine.ParseSince(q.Get("since"), s.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx = c.CloseRead(ctx)

	ch := make(chan logLine, 4096)
	errc := make(chan error, 1)
	go func() {
		errc <- e.Logs(ctx, d.ID, d.Tty, engine.LogOptions{Tail: tail, Since: since, Timestamps: q.Get("timestamps") == "1", Follow: q.Get("follow") == "1"},
			func(stderr bool, line string) {
				select {
				case ch <- logLine{Err: stderr, Text: line}:
				case <-ctx.Done():
				}
			})
		close(ch)
	}()
	if streamLines(ctx, c, ch) != nil {
		return
	}
	msg := logMessage{Done: true}
	if err := <-errc; err != nil {
		msg.Error = err.Error()
	}
	writeJSON(ctx, c, msg)
	c.Close(websocket.StatusNormalClosure, "")
}

// stackLogsSocket follows `docker compose logs -f` for a whole stack, so
// every line carries its service prefix. The compose process dies with the
// websocket.
func (s *Server) stackLogsSocket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, ok := loadFolder(st, r.PathValue("folder"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, err := s.target(st, f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tail := r.URL.Query().Get("tail")
	if _, err := strconv.Atoi(tail); err != nil && tail != "all" {
		tail = "100"
	}
	args := []string{"logs", "--tail", tail}
	if r.URL.Query().Get("follow") == "1" {
		args = append(args, "-f")
	}
	if r.URL.Query().Get("timestamps") == "1" {
		args = append(args, "--timestamps")
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx = c.CloseRead(ctx)

	cmd := s.runner.Command(ctx, t, args...)
	pipe, err := cmd.StdoutPipe()
	if err == nil {
		cmd.Stderr = cmd.Stdout
		err = cmd.Start()
	}
	if err != nil {
		writeJSON(ctx, c, logMessage{Done: true, Error: err.Error()})
		return
	}
	ch := make(chan logLine, 4096)
	go func() {
		compose.ReadLines(pipe, func(l string) {
			select {
			case ch <- logLine{Text: l}:
			case <-ctx.Done():
			}
		})
		close(ch)
	}()
	if streamLines(ctx, c, ch) != nil {
		return
	}
	msg := logMessage{Done: true}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		msg.Error = err.Error()
	}
	writeJSON(ctx, c, msg)
	c.Close(websocket.StatusNormalClosure, "")
}

type shellControl struct {
	Type    string `json:"type"`
	Cols    uint   `json:"cols,omitempty"`
	Rows    uint   `json:"rows,omitempty"`
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

func clampSize(s string, def uint) uint {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 1000 {
		return def
	}
	return uint(n)
}

// shellSocket bridges a browser terminal to an exec with a TTY. Binary
// websocket messages carry terminal bytes both ways; text messages carry
// JSON control: resize from the browser, exit or error from here.
func (s *Server) shellSocket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slot := r.PathValue("slot")
	e, _, err := s.engineFor(ctx, slot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer e.Close()
	d, err := e.Inspect(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !d.Running {
		http.Error(w, "the container is not running", http.StatusConflict)
		return
	}
	q := r.URL.Query()
	cmd := strings.Fields(q.Get("cmd"))
	user := strings.TrimSpace(q.Get("user"))
	cols, rows := clampSize(q.Get("cols"), 80), clampSize(q.Get("rows"), 24)

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)

	shown := cmd
	if len(shown) == 0 {
		shown = []string{"bash || sh"}
	}
	logCmd := []string{"docker", "exec", "-it"}
	if user != "" {
		logCmd = append(logCmd, "-u", user)
	}
	logCmd = append(append(logCmd, d.Name), shown...)
	started := s.now()
	actionID, _ := s.store.StartAction(context.Background(), store.Action{Started: started, Target: "container/" + d.Name, Engine: slot, Command: compose.Quote(logCmd)})

	x, err := e.Shell(ctx, d.ID, cmd, user, cols, rows)
	if err != nil {
		writeJSON(ctx, c, shellControl{Type: "error", Message: err.Error()})
		s.store.FinishAction(context.Background(), actionID, s.now(), -1, err.Error())
		c.Close(websocket.StatusNormalClosure, "")
		return
	}
	defer x.Close()

	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := x.Reader.Read(buf)
			if n > 0 {
				if c.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	inDone := make(chan struct{})
	go func() {
		defer close(inDone)
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if _, err := x.Conn.Write(data); err != nil {
					return
				}
				continue
			}
			var m shellControl
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 && m.Cols <= 1000 && m.Rows <= 1000 {
				x.Resize(ctx, m.Cols, m.Rows)
			}
		}
	}()

	select {
	case <-outDone:
	case <-inDone:
		x.Close() // browser left: end the exec's stdin
		<-outDone
	}
	wctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	code, werr := x.Wait(wctx)
	cancel()
	msg := shellControl{Type: "exit", Code: code}
	note := ""
	switch {
	case errors.Is(werr, engine.ErrNoShell):
		msg = shellControl{Type: "error", Code: code, Message: "This container has no usable shell (exit " + strconv.Itoa(code) + "). Distroless and scratch images ship without /bin/sh. Try a custom command, or use Logs and Inspect."}
		note = msg.Message
	case werr != nil:
		note = werr.Error()
	}
	s.store.FinishAction(context.Background(), actionID, s.now(), code, note)
	writeJSON(ctx, c, msg)
	c.Close(websocket.StatusNormalClosure, "")
}

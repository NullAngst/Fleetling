// Package enginetest is a fake Docker Engine API on a unix socket, for
// tests. It speaks just enough of the API for the real moby client:
// ping, version, container list and inspect, multiplexed logs, stats,
// lifecycle calls, and exec with a hijacked TTY stream that echoes input.
package enginetest

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Container is one fake container.
type Container struct {
	ID      string
	Name    string
	Image   string
	Running bool
	Tty     bool
	Labels  map[string]string
	Logs    []Line // sent on every logs request
	Follow  []Line // sent 50ms later when following
	NoShell bool   // exec fails like a distroless image
}

// Line is one log line.
type Line struct {
	Stderr bool
	Text   string
}

// Engine is the fake.
type Engine struct {
	Socket string

	mu         sync.Mutex
	containers map[string]*Container
	execs      map[string]*exec
	calls      []string
	resizes    []string
	srv        *http.Server
	dir        string
}

type exec struct {
	container *Container
	cmd       []string
	user      string
	exit      int
	running   bool
}

// Start listens on a fresh unix socket. Call Close when done.
func Start(containers ...*Container) (*Engine, error) {
	// Unix socket paths are limited to about 108 bytes, so keep it short.
	dir, err := os.MkdirTemp("", "fe")
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	e := &Engine{Socket: sock, dir: dir, containers: map[string]*Container{}, execs: map[string]*exec{}}
	for _, c := range containers {
		e.containers[c.ID] = c
	}
	e.srv = &http.Server{Handler: http.HandlerFunc(e.serve)}
	go e.srv.Serve(l)
	return e, nil
}

// Close stops the fake.
func (e *Engine) Close() {
	e.srv.Close()
	os.RemoveAll(e.dir)
}

// Calls returns "METHOD /path" for every lifecycle call made.
func (e *Engine) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

// Resizes returns "WxH" for every exec resize.
func (e *Engine) Resizes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.resizes...)
}

var versionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func (e *Engine) find(id string) *Container {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.containers[id]; c != nil {
		return c
	}
	for _, c := range e.containers {
		if c.Name == id || strings.HasPrefix(c.ID, id) {
			return c
		}
	}
	return nil
}

func (e *Engine) serve(w http.ResponseWriter, r *http.Request) {
	path := versionPrefix.ReplaceAllString(r.URL.Path, "")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	j := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	switch {
	case path == "/_ping":
		w.Header().Set("API-Version", "1.53")
		w.Header().Set("OSType", "linux")
		fmt.Fprint(w, "OK")
	case path == "/version":
		j(map[string]any{"Version": "29.8.2", "ApiVersion": "1.53", "MinAPIVersion": "1.24", "Os": "linux", "Arch": "amd64",
			"Components": []map[string]any{{"Name": "Engine", "Version": "29.8.2"}}})
	case path == "/containers/json":
		var out []map[string]any
		e.mu.Lock()
		for _, c := range e.containers {
			state := "exited"
			if c.Running {
				state = "running"
			}
			out = append(out, map[string]any{"Id": c.ID, "Names": []string{"/" + c.Name}, "Image": c.Image, "State": state,
				"Status": "Up 3 hours", "Labels": c.Labels, "Created": time.Now().Add(-3 * time.Hour).Unix(),
				"Ports": []map[string]any{{"IP": "0.0.0.0", "PrivatePort": 3000, "PublicPort": 3000, "Type": "tcp"}, {"IP": "::", "PrivatePort": 3000, "PublicPort": 3000, "Type": "tcp"}}})
		}
		e.mu.Unlock()
		j(out)
	case len(parts) >= 2 && parts[0] == "containers":
		c := e.find(parts[1])
		if c == nil {
			w.WriteHeader(http.StatusNotFound)
			j(map[string]string{"message": "No such container: " + parts[1]})
			return
		}
		e.container(w, r, c, parts[2:], j)
	case len(parts) == 3 && parts[0] == "exec":
		e.execCall(w, r, parts[1], parts[2], j)
	default:
		w.WriteHeader(http.StatusNotFound)
		j(map[string]string{"message": "fake engine: no route " + r.Method + " " + path})
	}
}

func (e *Engine) record(r *http.Request, c *Container, what string) {
	e.mu.Lock()
	e.calls = append(e.calls, r.Method+" "+what+" "+c.Name)
	e.mu.Unlock()
}

func (e *Engine) container(w http.ResponseWriter, r *http.Request, c *Container, rest []string, j func(any)) {
	if len(rest) == 0 && r.Method == http.MethodDelete {
		e.record(r, c, "remove")
		e.mu.Lock()
		delete(e.containers, c.ID)
		e.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	what := ""
	if len(rest) > 0 {
		what = rest[0]
	}
	switch what {
	case "json":
		status := "exited"
		if c.Running {
			status = "running"
		}
		j(map[string]any{"Id": c.ID, "Name": "/" + c.Name, "Image": "sha256:abc",
			"State":  map[string]any{"Status": status, "Running": c.Running, "StartedAt": "2026-10-04T12:00:00Z"},
			"Config": map[string]any{"Image": c.Image, "Tty": c.Tty, "Labels": c.Labels}, "Mounts": []any{}})
	case "start", "stop", "restart", "kill":
		e.record(r, c, what)
		e.mu.Lock()
		c.Running = what == "start" || what == "restart"
		e.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "logs":
		e.logs(w, r, c)
	case "stats":
		j(map[string]any{
			"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": 3_000_000}, "system_cpu_usage": 20_000_000, "online_cpus": 4},
			"precpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": 1_000_000}, "system_cpu_usage": 10_000_000},
			"memory_stats": map[string]any{"usage": 300 << 20, "limit": 4 << 30, "stats": map[string]any{"inactive_file": 100 << 20}},
			"networks":     map[string]any{"eth0": map[string]any{"rx_bytes": 1000, "tx_bytes": 2000}},
			"pids_stats":   map[string]any{"current": 7},
		})
	case "exec":
		var body struct {
			Cmd  []string
			User string
		}
		json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		id := fmt.Sprintf("exec%d", len(e.execs)+1)
		e.execs[id] = &exec{container: c, cmd: body.Cmd, user: body.User, running: true}
		e.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		j(map[string]string{"Id": id})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// logs writes the container's lines, multiplexed with 8-byte frame headers
// unless the container has a TTY, exactly like the real Engine API.
func (e *Engine) logs(w http.ResponseWriter, r *http.Request, c *Container) {
	if c.Tty {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	} else {
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	}
	w.WriteHeader(http.StatusOK)
	write := func(l Line) {
		text := l.Text + "\n"
		if c.Tty {
			fmt.Fprint(w, text)
			return
		}
		// Split each line across two frames to prove reassembly works.
		half := len(text) / 2
		for _, chunk := range []string{text[:half], text[half:]} {
			hdr := make([]byte, 8)
			hdr[0] = 1
			if l.Stderr {
				hdr[0] = 2
			}
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(chunk)))
			w.Write(hdr)
			w.Write([]byte(chunk))
		}
	}
	for _, l := range c.Logs {
		write(l)
	}
	w.(http.Flusher).Flush()
	if r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true" {
		select {
		case <-time.After(50 * time.Millisecond):
			for _, l := range c.Follow {
				write(l)
			}
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			return
		}
		<-r.Context().Done()
	}
}

func (e *Engine) execCall(w http.ResponseWriter, r *http.Request, id, what string, j func(any)) {
	e.mu.Lock()
	x := e.execs[id]
	e.mu.Unlock()
	if x == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch what {
	case "json":
		e.mu.Lock()
		defer e.mu.Unlock()
		j(map[string]any{"ID": id, "Running": x.running, "ExitCode": x.exit})
	case "resize":
		e.mu.Lock()
		e.resizes = append(e.resizes, r.URL.Query().Get("w")+"x"+r.URL.Query().Get("h"))
		e.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	case "start":
		// The real daemon reads the JSON start body before upgrading;
		// leaving it unread would make it the first "keystrokes".
		io.Copy(io.Discard, r.Body)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(buf, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		finish := func(code int) {
			e.mu.Lock()
			x.running, x.exit = false, code
			e.mu.Unlock()
		}
		if x.container.NoShell {
			fmt.Fprint(buf, "OCI runtime exec failed: exec failed: unable to start container process: exec: \"/bin/sh\": stat /bin/sh: no such file or directory: unknown\r\n")
			buf.Flush()
			finish(126)
			return
		}
		fmt.Fprintf(buf, "fake shell as %q: %s\r\n$ ", x.user, strings.Join(x.cmd, " "))
		buf.Flush()
		rd := bufio.NewReader(buf)
		var line []byte
		for {
			b, err := rd.ReadByte()
			if err != nil {
				finish(0)
				return
			}
			// Echo like a TTY in cooked mode.
			if b == '\r' || b == '\n' {
				fmt.Fprint(buf, "\r\n")
				if string(line) == "exit" {
					buf.Flush()
					finish(0)
					return
				}
				fmt.Fprintf(buf, "you typed: %s\r\n$ ", line)
				line = line[:0]
			} else {
				buf.WriteByte(b)
				line = append(line, b)
			}
			buf.Flush()
		}
	}
}

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
	"slices"
	"strings"
	"sync"
	"time"
)

// Container is one fake container.
type Container struct {
	ID       string
	Name     string
	Image    string
	Running  bool
	Tty      bool
	Labels   map[string]string
	Logs     []Line // sent on every logs request
	Follow   []Line // sent 50ms later when following
	NoShell  bool   // exec fails like a distroless image
	Env      []string
	Mounts   []Mount
	ImageID  string
	Volumes  []string // named volumes mounted
	Networks []string // networks attached
}

// Network is a fake network.
type Network struct {
	ID      string
	Name    string
	Driver  string
	Labels  map[string]string
	Subnet  string
	Gateway string
	Options map[string]string
}

// Image is a fake image.
type Image struct {
	ID   string
	Tags []string
	Size int64
}

// Volume is a fake volume.
type Volume struct {
	Name   string
	Labels map[string]string
}

// Mount is a bind mount on a fake container.
type Mount struct {
	Source, Destination string
}

// Line is one log line.
type Line struct {
	Stderr bool
	Text   string
}

// Engine is the fake.
type Engine struct {
	Socket string

	// ImageEnv maps an image reference to the Env baked into it.
	ImageEnv map[string][]string

	Networks []*Network
	Images   []*Image
	Volumes  []*Volume
	// Created holds the raw body of every POST /networks/create.
	Created [][]byte
	// Connects holds the raw body of every network connect.
	Connects [][]byte

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
	e := &Engine{Socket: sock, dir: dir, containers: map[string]*Container{}, execs: map[string]*exec{}, ImageEnv: map[string][]string{}}
	for _, c := range containers {
		e.containers[c.ID] = c
	}
	e.srv = &http.Server{Handler: http.HandlerFunc(e.serve)}
	go e.srv.Serve(l)
	return e, nil
}

// Add puts another container on the fake.
func (e *Engine) Add(c *Container) {
	e.mu.Lock()
	e.containers[c.ID] = c
	e.mu.Unlock()
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
			mounts := []map[string]any{}
			for _, v := range c.Volumes {
				mounts = append(mounts, map[string]any{"Type": "volume", "Name": v, "Destination": "/data"})
			}
			nets := map[string]any{}
			for _, n := range c.Networks {
				nets[n] = map[string]any{"NetworkID": n}
			}
			out = append(out, map[string]any{"Id": c.ID, "Names": []string{"/" + c.Name}, "Image": c.Image, "ImageID": c.ImageID, "State": state,
				"Status": "Up 3 hours", "Labels": c.Labels, "Created": time.Now().Add(-3 * time.Hour).Unix(),
				"Mounts": mounts, "NetworkSettings": map[string]any{"Networks": nets},
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
	case path == "/networks" && r.Method == http.MethodGet:
		e.mu.Lock()
		var out []map[string]any
		for _, n := range e.Networks {
			cfg := []map[string]any{}
			if n.Subnet != "" {
				cfg = append(cfg, map[string]any{"Subnet": n.Subnet, "Gateway": n.Gateway})
			}
			out = append(out, map[string]any{"Id": n.ID, "Name": n.Name, "Driver": n.Driver, "Scope": "local", "Labels": n.Labels,
				"Options": n.Options, "IPAM": map[string]any{"Driver": "default", "Config": cfg}})
		}
		e.mu.Unlock()
		j(out)
	case path == "/networks/create":
		b, _ := io.ReadAll(r.Body)
		var req struct{ Name, Driver string }
		json.Unmarshal(b, &req)
		e.mu.Lock()
		e.Created = append(e.Created, b)
		id := fmt.Sprintf("net%d", len(e.Networks)+1)
		e.Networks = append(e.Networks, &Network{ID: id, Name: req.Name, Driver: req.Driver})
		e.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		j(map[string]any{"Id": id, "Warning": ""})
	case len(parts) == 3 && parts[0] == "networks" && (parts[2] == "connect" || parts[2] == "disconnect"):
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.Connects = append(e.Connects, b)
		e.calls = append(e.calls, "POST network "+parts[2]+" "+parts[1])
		e.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case len(parts) == 2 && parts[0] == "networks" && r.Method == http.MethodDelete:
		e.mu.Lock()
		e.calls = append(e.calls, "DELETE network "+parts[1])
		e.Networks = slices.DeleteFunc(e.Networks, func(n *Network) bool { return n.ID == parts[1] || n.Name == parts[1] })
		e.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case path == "/images/json":
		e.mu.Lock()
		var out []map[string]any
		for _, im := range e.Images {
			out = append(out, map[string]any{"Id": im.ID, "RepoTags": im.Tags, "Size": im.Size, "Created": time.Now().Add(-48 * time.Hour).Unix(), "Containers": -1})
		}
		e.mu.Unlock()
		j(out)
	case path == "/images/prune":
		e.mu.Lock()
		e.calls = append(e.calls, "POST images prune "+r.URL.Query().Get("filters"))
		e.mu.Unlock()
		j(map[string]any{"ImagesDeleted": []map[string]string{{"Deleted": "sha256:x"}}, "SpaceReclaimed": 1 << 20})
	case len(parts) >= 2 && parts[0] == "images" && r.Method == http.MethodDelete:
		e.mu.Lock()
		e.calls = append(e.calls, "DELETE image "+strings.Join(parts[1:], "/"))
		e.mu.Unlock()
		j([]map[string]string{{"Untagged": strings.Join(parts[1:], "/")}})
	case path == "/volumes" && r.Method == http.MethodGet:
		e.mu.Lock()
		var out []map[string]any
		for _, v := range e.Volumes {
			out = append(out, map[string]any{"Name": v.Name, "Driver": "local", "Mountpoint": "/var/lib/docker/volumes/" + v.Name + "/_data", "Labels": v.Labels, "Scope": "local"})
		}
		e.mu.Unlock()
		j(map[string]any{"Volumes": out, "Warnings": nil})
	case path == "/volumes/prune":
		e.mu.Lock()
		e.calls = append(e.calls, "POST volumes prune "+r.URL.Query().Get("filters"))
		e.mu.Unlock()
		j(map[string]any{"VolumesDeleted": []string{"anon1"}, "SpaceReclaimed": 4096})
	case len(parts) == 2 && parts[0] == "volumes" && r.Method == http.MethodDelete:
		e.mu.Lock()
		e.calls = append(e.calls, "DELETE volume "+parts[1])
		e.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case len(parts) >= 3 && parts[0] == "images" && parts[len(parts)-1] == "json":
		ref := strings.Join(parts[1:len(parts)-1], "/")
		e.mu.Lock()
		env, ok := e.ImageEnv[ref]
		e.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			j(map[string]string{"message": "No such image: " + ref})
			return
		}
		j(map[string]any{"Id": "sha256:" + strings.Repeat("e", 64), "RepoTags": []string{ref}, "Config": map[string]any{"Env": env}})
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
		mounts := []map[string]any{}
		for _, m := range c.Mounts {
			mounts = append(mounts, map[string]any{"Type": "bind", "Source": m.Source, "Destination": m.Destination, "RW": true})
		}
		j(map[string]any{"Id": c.ID, "Name": "/" + c.Name, "Image": "sha256:abc",
			"State":  map[string]any{"Status": status, "Running": c.Running, "StartedAt": "2026-10-04T12:00:00Z"},
			"Config": map[string]any{"Image": c.Image, "Tty": c.Tty, "Labels": c.Labels, "Env": c.Env}, "Mounts": mounts})
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

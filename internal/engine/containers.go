package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/NullAngst/Fleetling/internal/compose"
)

// ContainerRow is one line on the containers page.
type ContainerRow struct {
	ID      string
	Name    string
	Image   string
	State   string // running, exited, ...
	Status  string // "Up 3 hours (healthy)"
	Health  string // healthy, unhealthy, starting, or ""
	Project string
	Service string
	WorkDir string
	Ports   []string
	Created time.Time
}

// ShortID is the 12-character form the CLI shows.
func (r ContainerRow) ShortID() string { return shortID(r.ID) }

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func rowFromSummary(c container.Summary) ContainerRow {
	r := ContainerRow{
		ID: c.ID, Image: c.Image, State: string(c.State), Status: c.Status,
		Project: c.Labels[LabelProject], Service: c.Labels[LabelService], WorkDir: c.Labels[LabelWorkingDir],
		Created: time.Unix(c.Created, 0),
	}
	if len(c.Names) > 0 {
		r.Name = strings.TrimPrefix(c.Names[0], "/")
	}
	if c.Health != nil && c.Health.Status != container.NoHealthcheck {
		r.Health = string(c.Health.Status)
	}
	seen := map[string]bool{}
	for _, p := range c.Ports {
		var s string
		if p.PublicPort != 0 {
			host := ""
			if p.IP.IsValid() && !p.IP.IsUnspecified() {
				host = p.IP.String() + ":"
			}
			s = fmt.Sprintf("%s%d->%d/%s", host, p.PublicPort, p.PrivatePort, p.Type)
		} else {
			s = fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)
		}
		if !seen[s] { // IPv4 and IPv6 bindings of the same port show once
			seen[s] = true
			r.Ports = append(r.Ports, s)
		}
	}
	slices.Sort(r.Ports)
	return r
}

// Containers lists every container, running or not.
func (e *Client) Containers(ctx context.Context) ([]ContainerRow, error) {
	if err := e.checkSocket(); err != nil {
		return nil, err
	}
	res, err := e.c.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerRow, 0, len(res.Items))
	for _, c := range res.Items {
		out = append(out, rowFromSummary(c))
	}
	slices.SortFunc(out, func(a, b ContainerRow) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Detail is one container's inspect output, plus the raw JSON.
type Detail struct {
	ContainerRow
	Running   bool
	Tty       bool
	StartedAt string
	ExitCode  int
	Mounts    []compose.Mount
	Env       []string // Config.Env, KEY=value
	Raw       string   // indented JSON for the Inspect tab
}

// Inspect reads one container.
func (e *Client) Inspect(ctx context.Context, id string) (Detail, error) {
	if err := e.checkSocket(); err != nil {
		return Detail{}, err
	}
	res, err := e.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return Detail{}, err
	}
	c := res.Container
	d := Detail{ContainerRow: ContainerRow{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/")}}
	if c.Config != nil {
		d.Image = c.Config.Image
		d.Tty = c.Config.Tty
		d.Project, d.Service, d.WorkDir = c.Config.Labels[LabelProject], c.Config.Labels[LabelService], c.Config.Labels[LabelWorkingDir]
		d.Env = c.Config.Env
	}
	for _, m := range c.Mounts {
		d.Mounts = append(d.Mounts, compose.Mount{Type: string(m.Type), Source: m.Source, Destination: m.Destination})
	}
	if c.State != nil {
		d.Running = c.State.Running
		d.StartedAt = c.State.StartedAt
		d.ExitCode = c.State.ExitCode
		d.State = string(c.State.Status)
		if c.State.Health != nil {
			d.Health = string(c.State.Health.Status)
		}
	}
	var buf bytes.Buffer
	if json.Indent(&buf, res.Raw, "", "  ") == nil {
		d.Raw = buf.String()
	} else {
		d.Raw = string(res.Raw)
	}
	return d, nil
}

// ImageEnv returns the Env baked into an image, KEY=value.
func (e *Client) ImageEnv(ctx context.Context, ref string) ([]string, error) {
	res, err := e.c.ImageInspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if res.Config == nil {
		return nil, nil
	}
	return res.Config.Env, nil
}

// Start, Stop, Restart, Kill and Remove act on one container through the
// Engine API, the same calls `docker start` and friends make.
func (e *Client) Start(ctx context.Context, id string) error {
	_, err := e.c.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (e *Client) Stop(ctx context.Context, id string) error {
	_, err := e.c.ContainerStop(ctx, id, client.ContainerStopOptions{})
	return err
}

func (e *Client) Restart(ctx context.Context, id string) error {
	_, err := e.c.ContainerRestart(ctx, id, client.ContainerRestartOptions{})
	return err
}

func (e *Client) Kill(ctx context.Context, id string) error {
	_, err := e.c.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: "KILL"})
	return err
}

// Remove force-removes a container, stopping it first if it runs. Named
// volumes stay; bind mounts on disk are never touched here.
func (e *Client) Remove(ctx context.Context, id string) error {
	_, err := e.c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	return err
}

// LogOptions mirror `docker logs`.
type LogOptions struct {
	Tail       string // a number or "all"
	Since      string // unix seconds, empty for all
	Timestamps bool
	Follow     bool
}

// Logs streams a container's logs, calling emit once per line. With
// Tty false the Engine API multiplexes stdout and stderr with 8-byte frame
// headers; stdcopy splits them, or the log fills with garbage bytes. A TTY
// container sends one raw stream.
func (e *Client) Logs(ctx context.Context, id string, tty bool, o LogOptions, emit func(stderr bool, line string)) error {
	rc, err := e.c.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: o.Tail, Since: o.Since, Timestamps: o.Timestamps, Follow: o.Follow,
	})
	if err != nil {
		return err
	}
	defer rc.Close()
	stdout := &lineWriter{emit: func(s string) { emit(false, s) }}
	stderr := &lineWriter{emit: func(s string) { emit(true, s) }}
	if tty {
		_, err = io.Copy(stdout, rc)
	} else {
		_, err = stdcopy.StdCopy(stdout, stderr, rc)
	}
	stdout.Flush()
	stderr.Flush()
	if ctx.Err() != nil {
		return nil // the browser went away
	}
	return err
}

// lineWriter turns a byte stream into lines. A partial line waits for its
// newline, so a frame boundary in the middle of a line doesn't split it.
type lineWriter struct {
	buf  []byte
	emit func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(strings.TrimSuffix(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 1<<20 { // a megabyte with no newline: emit it anyway
		w.Flush()
	}
	return len(p), nil
}

func (w *lineWriter) Flush() {
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}

// Stats is one sample for the detail page.
type Stats struct {
	CPUPercent float64
	MemUsage   uint64
	MemLimit   uint64
	RxBytes    uint64
	TxBytes    uint64
	Pids       uint64
}

// Stats takes one sample, asking the daemon for a previous one too so CPU
// usage can be worked out as a delta. That costs about a second.
func (e *Client) Stats(ctx context.Context, id string) (Stats, error) {
	res, err := e.c.ContainerStats(ctx, id, client.ContainerStatsOptions{Stream: false, IncludePreviousSample: true})
	if err != nil {
		return Stats{}, err
	}
	defer res.Body.Close()
	var s container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		return Stats{}, err
	}
	return StatsFrom(s), nil
}

// StatsFrom does the same arithmetic as `docker stats`.
func StatsFrom(s container.StatsResponse) Stats {
	out := Stats{MemUsage: s.MemoryStats.Usage, MemLimit: s.MemoryStats.Limit, Pids: s.PidsStats.Current}
	// cgroup v2 reports page cache inside usage; docker stats subtracts it.
	if v, ok := s.MemoryStats.Stats["inactive_file"]; ok && v < out.MemUsage {
		out.MemUsage -= v
	}
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpuDelta > 0 && sysDelta > 0 {
		out.CPUPercent = cpuDelta / sysDelta * cpus * 100
	}
	for _, n := range s.Networks {
		out.RxBytes += n.RxBytes
		out.TxBytes += n.TxBytes
	}
	return out
}

// DefaultShell tries bash and falls back to sh, inside the container,
// in one exec.
var DefaultShell = []string{"/bin/sh", "-c", "if [ -x /bin/bash ]; then exec /bin/bash; else exec /bin/sh; fi"}

// Exec is an interactive exec session with a TTY.
type Exec struct {
	ID     string
	Conn   net.Conn
	Reader *bufio.Reader
	c      *client.Client
	start  time.Time
}

// Shell starts an interactive exec with a TTY, sized cols x rows.
func (e *Client) Shell(ctx context.Context, id string, cmd []string, user string, cols, rows uint) (*Exec, error) {
	if len(cmd) == 0 {
		cmd = DefaultShell
	}
	size := client.ConsoleSize{Height: rows, Width: cols}
	cr, err := e.c.ExecCreate(ctx, id, client.ExecCreateOptions{
		User: user, TTY: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
		Cmd: cmd, ConsoleSize: size, Env: []string{"TERM=xterm-256color"},
	})
	if err != nil {
		return nil, err
	}
	at, err := e.c.ExecAttach(ctx, cr.ID, client.ExecAttachOptions{TTY: true, ConsoleSize: size})
	if err != nil {
		return nil, err
	}
	return &Exec{ID: cr.ID, Conn: at.Conn, Reader: at.Reader, c: e.c, start: time.Now()}, nil
}

// Resize forwards a terminal resize.
func (x *Exec) Resize(ctx context.Context, cols, rows uint) error {
	_, err := x.c.ExecResize(ctx, x.ID, client.ExecResizeOptions{Height: rows, Width: cols})
	return err
}

// Close ends the session from our side.
func (x *Exec) Close() error { return x.Conn.Close() }

// ErrNoShell means the exec could not find its program, which is what a
// distroless or scratch image does with /bin/sh.
var ErrNoShell = errors.New("no shell in this container")

// Wait returns the exec's exit code once the process is gone. A 126 or 127
// within a few seconds of starting is reported as ErrNoShell.
func (x *Exec) Wait(ctx context.Context) (int, error) {
	for range 50 {
		res, err := x.c.ExecInspect(ctx, x.ID, client.ExecInspectOptions{})
		if err != nil {
			return -1, err
		}
		if !res.Running {
			if (res.ExitCode == 126 || res.ExitCode == 127) && time.Since(x.start) < 5*time.Second {
				return res.ExitCode, ErrNoShell
			}
			return res.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return -1, errors.New("exec still running after its output closed")
}

// ParseSince turns "10m", "2h", an RFC 3339 time or unix seconds into the
// unix seconds the Engine API wants. Empty stays empty.
func ParseSince(s string, now time.Time) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return strconv.FormatInt(now.Add(-d).Unix(), 10), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return strconv.FormatInt(t.Unix(), 10), nil
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return s, nil
	}
	return "", fmt.Errorf("since %q: use a duration like 10m or 2h, or an RFC 3339 time", s)
}

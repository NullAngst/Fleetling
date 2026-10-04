package compose

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Job is one stack action: one or more compose commands run in order, with
// their combined output kept in memory so a browser can join late and still
// see everything.
type Job struct {
	ID      string
	Target  string // stack folder, used for the per-stack lock
	Engine  string
	Command string // what the action log stores, one line per step
	Started time.Time

	mu       sync.Mutex
	lines    []string
	dropped  int // lines dropped from the front once maxLines is hit
	done     bool
	exit     int
	finished time.Time
	changed  chan struct{} // closed and replaced on every change
}

const maxJobLines = 20000

// Step is one command inside a job.
type Step struct {
	Cmd *exec.Cmd
}

// JobSpec describes a job before it starts.
type JobSpec struct {
	Target string
	Engine string
	Steps  []Step
	// Before runs first; an error fails the job without running any step.
	Before func(ctx context.Context, j *Job) error
	// After runs once every step has finished, with the final exit code.
	After func(ctx context.Context, j *Job, exit int)
	// Done is called last, after After, with the full output.
	Done func(j *Job)
}

// Line appends one output line.
func (j *Job) Line(s string) {
	j.mu.Lock()
	j.lines = append(j.lines, s)
	if len(j.lines) > maxJobLines {
		cut := len(j.lines) - maxJobLines
		j.lines = append([]string(nil), j.lines[cut:]...)
		j.dropped += cut
	}
	j.notifyLocked()
	j.mu.Unlock()
}

// Linef appends a formatted line.
func (j *Job) Linef(format string, a ...any) { j.Line(fmt.Sprintf(format, a...)) }

func (j *Job) notifyLocked() {
	close(j.changed)
	j.changed = make(chan struct{})
}

// Since returns lines from absolute index from on, the next index to ask
// for, whether the job is done, its exit code, and a channel that closes on
// the next change.
func (j *Job) Since(from int) (lines []string, next int, done bool, exit int, changed <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	start := from - j.dropped
	if start < 0 {
		start = 0
	}
	if start < len(j.lines) {
		lines = append(lines, j.lines[start:]...)
	}
	return lines, j.dropped + len(j.lines), j.done, j.exit, j.changed
}

// Tail returns the last n lines.
func (j *Job) Tail(n int) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.lines) <= n {
		return append([]string(nil), j.lines...)
	}
	return append([]string(nil), j.lines[len(j.lines)-n:]...)
}

// Result reports whether the job finished and its exit code.
func (j *Job) Result() (done bool, exit int, finished time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done, j.exit, j.finished
}

// Wait blocks until the job is done or ctx ends.
func (j *Job) Wait(ctx context.Context) (int, error) {
	for {
		_, _, done, exit, ch := j.Since(1 << 62)
		if done {
			return exit, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return -1, ctx.Err()
		}
	}
}

// ErrBusy means the stack already has an action running.
var ErrBusy = errors.New("another action is already running on this stack")

// Jobs runs jobs and remembers recent ones. One job per stack at a time:
// two `compose up` runs racing on one project end badly.
type Jobs struct {
	mu      sync.Mutex
	byID    map[string]*Job
	running map[string]string // target -> job ID
	keep    time.Duration
	base    context.Context
}

// NewJobs builds a registry. Jobs run under base, so cancelling it (server
// shutdown) kills running compose processes.
func NewJobs(base context.Context) *Jobs {
	return &Jobs{byID: map[string]*Job{}, running: map[string]string{}, keep: time.Hour, base: base}
}

// Get returns a job by ID.
func (js *Jobs) Get(id string) *Job {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.byID[id]
}

// Running returns the job currently running on target, if any.
func (js *Jobs) Running(target string) *Job {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.byID[js.running[target]]
}

// Start launches a job in the background.
func (js *Jobs) Start(spec JobSpec) (*Job, error) {
	js.mu.Lock()
	if id, ok := js.running[spec.Target]; ok && id != "" {
		js.mu.Unlock()
		return nil, ErrBusy
	}
	js.pruneLocked()
	b := make([]byte, 8)
	rand.Read(b)
	var cmds []string
	for _, s := range spec.Steps {
		cmds = append(cmds, Quote(append([]string{"docker"}, s.Cmd.Args[1:]...)))
	}
	j := &Job{
		ID: hex.EncodeToString(b), Target: spec.Target, Engine: spec.Engine,
		Command: strings.Join(cmds, "\n"), Started: time.Now(), changed: make(chan struct{}),
	}
	js.byID[j.ID] = j
	js.running[spec.Target] = j.ID
	js.mu.Unlock()

	go js.run(j, spec)
	return j, nil
}

func (js *Jobs) run(j *Job, spec JobSpec) {
	ctx := js.base
	exit := 0
	if spec.Before != nil {
		if err := spec.Before(ctx, j); err != nil {
			j.Linef("fleetling: %v", err)
			exit = -1
		}
	}
	for _, s := range spec.Steps {
		if exit != 0 {
			break
		}
		j.Line("$ " + Quote(append([]string{"docker"}, s.Cmd.Args[1:]...)))
		exit = runStep(j, s.Cmd)
		if exit != 0 {
			j.Linef("fleetling: exit code %d", exit)
		}
	}
	if spec.After != nil {
		spec.After(ctx, j, exit)
	}
	j.mu.Lock()
	j.done, j.exit, j.finished = true, exit, time.Now()
	j.notifyLocked()
	j.mu.Unlock()

	js.mu.Lock()
	delete(js.running, j.Target)
	js.mu.Unlock()
	if spec.Done != nil {
		spec.Done(j)
	}
}

// runStep runs one command with stdout and stderr merged into one pipe, so
// lines keep the order Compose printed them in.
func runStep(j *Job, c *exec.Cmd) int {
	pr, pw, err := os.Pipe()
	if err != nil {
		j.Linef("fleetling: %v", err)
		return -1
	}
	c.Stdout, c.Stderr = pw, pw
	if err := c.Start(); err != nil {
		pw.Close()
		pr.Close()
		j.Linef("fleetling: %v", err)
		return -1
	}
	pw.Close() // the child holds its own copy
	readLines(pr, j.Line)
	pr.Close()
	err = c.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if code := ee.ExitCode(); code > 0 {
			return code
		}
		return -1 // killed by a signal
	}
	j.Linef("fleetling: %v", err)
	return -1
}

// readLines splits on \n and \r so progress redraws become separate lines.
func readLines(r io.Reader, emit func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		for i, b := range data {
			if b == '\n' || b == '\r' {
				return i + 1, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			emit(line)
		}
	}
}

// pruneLocked forgets finished jobs older than keep.
func (js *Jobs) pruneLocked() {
	cutoff := time.Now().Add(-js.keep)
	for id, j := range js.byID {
		if done, _, fin := j.Result(); done && fin.Before(cutoff) {
			delete(js.byID, id)
		}
	}
}

package compose

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Guard runs before every step, and an error stops the job right there.
func TestJobGuardStopsBeforeStep(t *testing.T) {
	js := NewJobs(context.Background())
	var ran []string
	checks := 0
	step := func(name string) Step {
		return Step{Show: name, Run: func(ctx context.Context, j *Job) int { ran = append(ran, name); return 0 }}
	}
	j, err := js.Start(JobSpec{
		Target: "guarded",
		Steps:  []Step{step("pull"), step("up")},
		Guard: func(context.Context) error {
			checks++
			if checks == 2 {
				return errors.New("files changed")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exit, err := j.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if exit == 0 || len(ran) != 1 || ran[0] != "pull" {
		t.Fatalf("exit %d, ran %v; want the job stopped before up", exit, ran)
	}
	tail := j.Tail(1)
	if len(tail) != 1 || tail[0] != "fleetling: stopped before up: files changed" {
		t.Fatalf("last line %q", tail)
	}
}

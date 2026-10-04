package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
)

// endpoint is one configured engine slot.
type endpoint struct {
	Slot string // compose.EngineDocker or compose.EnginePodman
	Host string // as saved in settings
}

func endpoints(st store.Settings) []endpoint {
	var out []endpoint
	if st.DockerHost != "" {
		out = append(out, endpoint{compose.EngineDocker, st.DockerHost})
	}
	if st.PodmanHost != "" {
		out = append(out, endpoint{compose.EnginePodman, st.PodmanHost})
	}
	return out
}

// engineStatus is one slot's result for the stacks page.
type engineStatus struct {
	Slot       string
	Host       string
	Up         bool
	Missing    bool // socket file absent: shown quietly, not as an error
	Err        string
	Containers int
}

// gather lists Compose containers from every configured endpoint in
// parallel. One engine being down never hides the other's stacks.
func (s *Server) gather(ctx context.Context, st store.Settings) ([]compose.Container, []engineStatus) {
	eps := endpoints(st)
	results := make([][]compose.Container, len(eps))
	status := make([]engineStatus, len(eps))
	var wg sync.WaitGroup
	for i, ep := range eps {
		status[i] = engineStatus{Slot: ep.Slot, Host: ep.Host}
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			e, err := s.newEngine(ep.Host)
			if err == nil {
				defer e.Close()
				results[i], err = e.ComposeContainers(ctx, ep.Slot)
			}
			switch {
			case errors.Is(err, engine.ErrSocketMissing):
				status[i].Missing = true
				status[i].Err = err.Error()
			case err != nil:
				status[i].Err = err.Error()
			default:
				status[i].Up = true
				status[i].Containers = len(results[i])
			}
		})
	}
	wg.Wait()
	var all []compose.Container
	for _, r := range results {
		all = append(all, r...)
	}
	return all, status
}

// selfMounts finds Fleetling's own container through any configured engine
// and returns its mounts.
func (s *Server) selfMounts(ctx context.Context, st store.Settings) ([]compose.Mount, error) {
	if !s.inContainer() {
		return nil, errNotInContainer
	}
	ids := s.selfIDs()
	var lastErr error = errors.New("no engine endpoint configured")
	for _, ep := range endpoints(st) {
		e, err := s.newEngine(ep.Host)
		if err != nil {
			lastErr = err
			continue
		}
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			m, err := e.ContainerMounts(ctx, id)
			cancel()
			if err == nil {
				e.Close()
				return m, nil
			}
			lastErr = err
		}
		e.Close()
	}
	return nil, fmt.Errorf("could not find this container through the engine API: %w", lastErr)
}

// checkRoot validates a proposed stack root. It refuses anything that is
// not a folder, and anything whose mount breaks the identical-path rule.
// When the mount can't be checked at all (not in a container, or no engine
// answers) the root is accepted with a warning, since there is nothing to
// compare against.
func (s *Server) checkRoot(ctx context.Context, root string, st store.Settings) (clean, warning string, err error) {
	clean, err = compose.ValidateRoot(root)
	if err != nil {
		return "", "", err
	}
	mounts, err := s.selfMounts(ctx, st)
	switch {
	case errors.Is(err, errNotInContainer):
		return clean, "Fleetling is not running in a container, so the identical-path mount check was skipped.", nil
	case err != nil:
		return clean, "The mount check was skipped: " + err.Error(), nil
	}
	if err := compose.CheckRootMount(clean, mounts); err != nil {
		return "", "", err
	}
	return clean, "", nil
}

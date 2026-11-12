package engine

import (
	"context"
	"slices"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// ProjectContainer is one container of a Compose project, with what the
// updater and the update check need: which image it runs and its health.
type ProjectContainer struct {
	ID      string
	Name    string
	Service string
	Image   string // as configured, e.g. "fleetling:local"
	ImageID string // sha256:...
	Running bool
	Health  string // healthy, unhealthy, starting, or "" when the image has no health check
}

// ProjectContainers lists every container of one Compose project, running
// or not, sorted by service. Health comes from inspect, since the list
// call only carries it inside the status text.
func (e *Client) ProjectContainers(ctx context.Context, project string) ([]ProjectContainer, error) {
	if err := e.checkSocket(); err != nil {
		return nil, err
	}
	res, err := e.c.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelProject+"="+project),
	})
	if err != nil {
		return nil, err
	}
	var out []ProjectContainer
	for _, c := range res.Items {
		// Filter again here: a filter the engine ignores must not widen
		// the result to every container on the host.
		if c.Labels[LabelProject] != project {
			continue
		}
		pc := ProjectContainer{
			ID: c.ID, Service: c.Labels[LabelService], Image: c.Image, ImageID: c.ImageID,
			Running: c.State == container.StateRunning,
		}
		if len(c.Names) > 0 {
			pc.Name = strings.TrimPrefix(c.Names[0], "/")
		}
		if ins, err := e.c.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{}); err == nil {
			if ins.Container.Image != "" {
				pc.ImageID = ins.Container.Image
			}
			if st := ins.Container.State; st != nil {
				pc.Running = st.Running
				if st.Health != nil {
					pc.Health = string(st.Health.Status)
				}
			}
		}
		out = append(out, pc)
	}
	slices.SortFunc(out, func(a, b ProjectContainer) int {
		if c := strings.Compare(a.Service, b.Service); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// ImageInfo is the part of an image inspect the updater and the update
// check use.
type ImageInfo struct {
	ID          string
	RepoTags    []string
	RepoDigests []string // name@sha256:..., the registry digests this image was pulled as
}

// ImageInfo inspects one image by reference or ID.
func (e *Client) ImageInfo(ctx context.Context, ref string) (ImageInfo, error) {
	if err := e.checkSocket(); err != nil {
		return ImageInfo{}, err
	}
	res, err := e.c.ImageInspect(ctx, ref)
	if err != nil {
		return ImageInfo{}, err
	}
	return ImageInfo{ID: res.ID, RepoTags: res.RepoTags, RepoDigests: res.RepoDigests}, nil
}

// Exists reports whether a container with this name or ID exists, running
// or not. Used to refuse a second updater helper.
func (e *Client) Exists(ctx context.Context, nameOrID string) (bool, error) {
	if err := e.checkSocket(); err != nil {
		return false, err
	}
	_, err := e.c.ContainerInspect(ctx, nameOrID, client.ContainerInspectOptions{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// isNotFound reports a 404 from the engine. The moby client maps status
// codes onto containerd's error classes.
func isNotFound(err error) bool { return cerrdefs.IsNotFound(err) }

package compose

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Kind says how Fleetling relates to a stack.
type Kind string

const (
	KindManaged  Kind = "managed"  // folder with .fleetling.toml
	KindOnDisk   Kind = "ondisk"   // folder with a compose file, no metadata yet
	KindExternal Kind = "external" // running project with no folder under the root
)

// State is the run state across a stack's services.
type State string

const (
	StateRunning State = "running" // every service has a running container
	StatePartial State = "partial" // some services running, some not
	StateStopped State = "stopped" // nothing running
	StateUnknown State = "unknown" // the stack's engine could not be reached
)

// Container is the slice of a container the stack list needs, taken from
// its Compose labels.
type Container struct {
	Engine     string // endpoint slot, "docker" or "podman"
	Project    string // com.docker.compose.project
	Service    string // com.docker.compose.service
	WorkingDir string // com.docker.compose.project.working_dir
	Running    bool
}

// Stack is one row on the stacks page.
type Stack struct {
	Project       string
	ProjectSource string
	Engine        string // "" when unknown, e.g. an On disk folder with nothing running
	Dir           string // stack folder, or the working dir label for External
	Folder        string // folder name under the root, empty for External
	ComposeFile   string
	Kind          Kind
	State         State
	Running       int // services with at least one running container
	Total         int
	Warnings      []string
	Review        string // set by the server: "", "modified" or "unrecorded"
}

// Label is the text for the status column. Managed stacks show their run
// state; the other kinds show what they are, per the spec.
func (s Stack) Label() string {
	switch s.Kind {
	case KindOnDisk:
		return "On disk"
	case KindExternal:
		return "External"
	}
	switch s.State {
	case StateRunning:
		return "Running"
	case StatePartial:
		return "Partial"
	case StateStopped:
		return "Stopped"
	}
	return "Unknown"
}

type groupKey struct{ engine, project string }

type group struct {
	key         groupKey
	services    map[string]bool // service -> has a running container
	workingDirs map[string]bool
	claimed     bool
}

// Merge lines up folders on disk with the Compose projects the engines
// report. reachable lists the engine slots that answered; a stack on an
// engine that did not answer shows as Unknown rather than Stopped.
//
// Matching order:
//  1. Managed folders take their project by name on their own engine.
//  2. Unmanaged folders take a project whose working_dir label is the folder.
//  3. Unmanaged folders take a project by name on any engine.
//  4. Whatever is left over shows as External.
func Merge(folders []Folder, containers []Container, reachable map[string]bool) []Stack {
	groups := map[groupKey]*group{}
	var order []*group
	for _, c := range containers {
		if c.Project == "" {
			continue
		}
		k := groupKey{c.Engine, c.Project}
		g := groups[k]
		if g == nil {
			g = &group{key: k, services: map[string]bool{}, workingDirs: map[string]bool{}}
			groups[k] = g
			order = append(order, g)
		}
		svc := c.Service
		if svc == "" {
			svc = "(no service label)"
		}
		g.services[svc] = g.services[svc] || c.Running
		if c.WorkingDir != "" {
			g.workingDirs[filepath.Clean(c.WorkingDir)] = true
		}
	}

	stacks := make([]Stack, len(folders))
	owner := map[groupKey]string{} // project key -> folder that claimed it

	claim := func(i int, g *group) {
		g.claimed = true
		owner[g.key] = folders[i].Dir
		stacks[i].Project = g.key.project
		stacks[i].Engine = g.key.engine
		fill(&stacks[i], folders[i].Services, g)
	}

	for i, f := range folders {
		stacks[i] = Stack{
			Project:       f.Project,
			ProjectSource: f.ProjectSource,
			Dir:           f.Dir,
			Folder:        f.Name,
			ComposeFile:   f.ComposeFile,
			Kind:          KindOnDisk,
			Warnings:      slices.Clone(f.Warnings),
		}
		fill(&stacks[i], f.Services, nil)
	}

	// 1. Managed folders.
	for i, f := range folders {
		if f.Meta == nil {
			continue
		}
		stacks[i].Kind = KindManaged
		stacks[i].Engine = f.Meta.Engine
		k := groupKey{f.Meta.Engine, f.Meta.Project}
		if prev, taken := owner[k]; taken {
			stacks[i].Warnings = append(stacks[i].Warnings, fmt.Sprintf("project %q on %s is already used by %s", k.project, k.engine, prev))
			continue
		}
		owner[k] = f.Dir
		if g := groups[k]; g != nil {
			claim(i, g)
		}
	}

	// 2. Unmanaged folders by working_dir label.
	for i, f := range folders {
		if f.Meta != nil {
			continue
		}
		for _, g := range order {
			if !g.claimed && g.workingDirs[filepath.Clean(f.Dir)] {
				claim(i, g)
				if g.key.project != f.Project {
					stacks[i].ProjectSource = "running containers"
				}
				break
			}
		}
	}

	// 3. Unmanaged folders by project name, for anything step 2 missed.
	for i, f := range folders {
		if f.Meta != nil || stacks[i].Engine != "" {
			continue
		}
		for _, g := range order {
			if g.claimed || g.key.project != f.Project {
				continue
			}
			claim(i, g)
			if !g.workingDirs[filepath.Clean(f.Dir)] && len(g.workingDirs) > 0 {
				stacks[i].Warnings = append(stacks[i].Warnings,
					"matched by name only; the running containers were started from "+strings.Join(sortedKeys(g.workingDirs), ", "))
			}
			break
		}
	}

	// 4. Leftovers are External.
	for _, g := range order {
		if g.claimed || owner[g.key] != "" {
			continue
		}
		s := Stack{Project: g.key.project, Engine: g.key.engine, Kind: KindExternal, ProjectSource: "running containers"}
		dirs := sortedKeys(g.workingDirs)
		if len(dirs) > 0 {
			s.Dir = dirs[0]
		}
		if len(dirs) > 1 {
			s.Warnings = append(s.Warnings, "containers report more than one working folder: "+strings.Join(dirs, ", "))
		}
		fill(&s, nil, g)
		stacks = append(stacks, s)
	}

	for i := range stacks {
		stacks[i].State = stateOf(stacks[i], reachable)
	}
	slices.SortStableFunc(stacks, func(a, b Stack) int { return strings.Compare(a.Project, b.Project) })
	return stacks
}

// fill counts services: the union of what the compose file declares and
// what is actually running.
func fill(s *Stack, declared []string, g *group) {
	all := map[string]bool{}
	for _, d := range declared {
		all[d] = false
	}
	if g != nil {
		for svc, running := range g.services {
			all[svc] = all[svc] || running
		}
	}
	s.Total, s.Running = len(all), 0
	for _, r := range all {
		if r {
			s.Running++
		}
	}
}

func stateOf(s Stack, reachable map[string]bool) State {
	if s.Engine == "" {
		// Nothing claimed this folder. It is only "stopped" if at least one
		// engine answered, otherwise we can't know.
		anyUp := false
		for _, up := range reachable {
			anyUp = anyUp || up
		}
		if !anyUp {
			return StateUnknown
		}
	} else if !reachable[s.Engine] {
		return StateUnknown
	}
	switch {
	case s.Running > 0 && s.Running == s.Total:
		return StateRunning
	case s.Running > 0:
		return StatePartial
	}
	return StateStopped
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

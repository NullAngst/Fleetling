// Package convert turns a compose file into Podman Quadlet units (with
// podlet) and Kubernetes manifests (with kompose), and lints it for what
// won't survive either conversion. Fleetling never installs the Quadlets
// or applies the manifests; it shows them, offers them as a .tar, and
// can save them next to the stack.
package convert

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Warning is one lint finding, pointing at the line in the user's file.
type Warning struct {
	Line       int    // 0 when the setting came from an include or extends file
	Service    string // "" for project-level findings
	What       string // the setting, e.g. "network_mode: host"
	Quadlet    string // what happens to it in the Quadlet output
	Kubernetes string // what happens to it in the Kubernetes output
}

// normalized is the slice of `docker compose config --format json` the
// lint reads. Compose has already merged includes and extends, filled in
// variables and expanded short syntax, so each check is one field.
type normalized struct {
	Services map[string]struct {
		NetworkMode string          `json:"network_mode"`
		Devices     json.RawMessage `json:"devices"`
		DependsOn   map[string]struct {
			Condition string `json:"condition"`
		} `json:"depends_on"`
		Build         json.RawMessage `json:"build"`
		ContainerName string          `json:"container_name"`
		Healthcheck   *struct {
			Disable bool `json:"disable"`
		} `json:"healthcheck"`
		Privileged bool     `json:"privileged"`
		CapAdd     []string `json:"cap_add"`
	} `json:"services"`
}

// Lint checks the normalized config and maps each finding back to a line
// in userText, the file as the user wrote it. Relative paths are read from
// userText itself, since the normalized config has already made them
// absolute.
func Lint(normalizedJSON []byte, userText string) ([]Warning, error) {
	var cfg normalized
	if err := json.Unmarshal(normalizedJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parse compose config: %w", err)
	}
	src := parseUser(userText)
	var out []Warning
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, name := range names {
		s := cfg.Services[name]
		add := func(key, what, quadlet, k8s string) {
			out = append(out, Warning{Line: src.line(name, key), Service: name, What: what, Quadlet: quadlet, Kubernetes: k8s})
		}
		if s.NetworkMode == "host" {
			add("network_mode", "network_mode: host",
				"Works: the container shares the host's network (Network=host). Inside the pod podlet makes, the whole pod does.",
				"Becomes hostNetwork: true at best, which most clusters refuse, and the Service kompose writes for it routes nowhere useful.")
		}
		if len(s.Devices) > 0 && string(s.Devices) != "null" {
			add("devices", "devices",
				"Works as AddDevice=, as long as the device exists on the host running the unit.",
				"Dropped. Kubernetes has no device passthrough in a pod spec; it needs a device plugin (for /dev/dri, the GPU plugin) or a privileged pod.")
		}
		var conds []string
		for dep, d := range s.DependsOn {
			if d.Condition != "" && d.Condition != "service_started" {
				conds = append(conds, dep+" ("+d.Condition+")")
			}
		}
		slices.Sort(conds)
		if len(conds) > 0 {
			add("depends_on", "depends_on with conditions: "+strings.Join(conds, ", "),
				"The condition is lost: podlet only takes plain dependencies, so the copy it gets has none. systemd starts this once the other unit has started, not once it is healthy. podlet suggests Notify=healthy in the dependency's [Container] section.",
				"Ignored entirely. Pods start in any order; the app has to retry its connections, or use an init container.")
		}
		if len(s.Build) > 0 && string(s.Build) != "null" {
			add("build", "build",
				"Not built, and dropped from the copy podlet gets when image: is set. Build and tag the image first (the Builds page can), then the unit runs it by name.",
				"Not built. Push the image to a registry the cluster can pull from; kompose only references it.")
		}
		if s.ContainerName != "" {
			add("container_name", "container_name: "+s.ContainerName,
				"Kept as ContainerName=, but podlet renames containers in its pod to <pod>-<container>, so anything that reaches it by this name breaks.",
				"Ignored. Pods get generated names; other services must use the Service name instead.")
		}
		if s.Healthcheck != nil && !s.Healthcheck.Disable {
			add("healthcheck", "healthcheck",
				"Works as HealthCmd= and friends. Podman only marks the container unhealthy; add HealthOnFailure= if it should restart.",
				"Becomes a liveness probe, which restarts the pod when it fails instead of just marking it unhealthy. Check the timings before applying.")
		}
		if s.Privileged {
			add("privileged", "privileged: true",
				"Works (PodmanArgs=--privileged) on rootful Podman; rootless can't grant real host privileges.",
				"Becomes securityContext.privileged, which the Pod Security baseline and restricted levels reject.")
		}
		if len(s.CapAdd) > 0 {
			add("cap_add", "cap_add: "+strings.Join(s.CapAdd, ", "),
				"Works as AddCapability=.",
				"Becomes securityContext.capabilities.add, which the restricted Pod Security level rejects for anything past NET_BIND_SERVICE.")
		}
	}
	for _, r := range src.relative {
		out = append(out, Warning{Line: r.line, Service: r.service, What: r.what + " " + r.path,
			Quadlet:    "Made absolute against the stack folder, so the unit only works where that folder exists.",
			Kubernetes: "A host path doesn't exist on a cluster node. kompose turns bind mounts into PersistentVolumeClaims (or hostPath, with that option), and the data isn't copied."})
	}
	slices.SortStableFunc(out, func(a, b Warning) int {
		if a.Line == 0 || b.Line == 0 {
			return b.Line - a.Line // unknown lines last
		}
		return a.Line - b.Line
	})
	return out, nil
}

type relPath struct {
	line          int
	service, what string
	path          string
}

type userFile struct {
	keys     map[string]int // "service\x00key" -> line
	services map[string]int
	relative []relPath
}

func (u userFile) line(service, key string) int {
	if l, ok := u.keys[service+"\x00"+key]; ok {
		return l
	}
	return 0
}

// parseUser reads the YAML node positions of the user's own file.
func parseUser(text string) userFile {
	u := userFile{keys: map[string]int{}, services: map[string]int{}}
	var doc yaml.Node
	if yaml.Unmarshal([]byte(text), &doc) != nil || len(doc.Content) == 0 {
		return u
	}
	root := doc.Content[0]
	svcs := mapValue(root, "services")
	if svcs == nil || svcs.Kind != yaml.MappingNode {
		return u
	}
	rel := func(p string) bool {
		return strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || p == "." || p == ".."
	}
	for i := 0; i+1 < len(svcs.Content); i += 2 {
		name, svc := svcs.Content[i].Value, svcs.Content[i+1]
		u.services[name] = svcs.Content[i].Line
		if svc.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(svc.Content); j += 2 {
			k, v := svc.Content[j], svc.Content[j+1]
			u.keys[name+"\x00"+k.Value] = k.Line
			switch k.Value {
			case "volumes":
				for _, vol := range seq(v) {
					src := ""
					if vol.Kind == yaml.ScalarNode {
						src, _, _ = strings.Cut(vol.Value, ":")
					} else if s := mapValue(vol, "source"); s != nil {
						src = s.Value
					}
					if rel(src) {
						u.relative = append(u.relative, relPath{vol.Line, name, "relative bind mount", src})
					}
				}
			case "env_file":
				nodes := seq(v)
				if v.Kind == yaml.ScalarNode {
					nodes = []*yaml.Node{v}
				}
				for _, ef := range nodes {
					p := ef.Value
					if ef.Kind == yaml.MappingNode {
						if pn := mapValue(ef, "path"); pn != nil {
							p = pn.Value
						}
					}
					if rel(p) || p != "" && !strings.HasPrefix(p, "/") && !strings.Contains(p, "$") {
						u.relative = append(u.relative, relPath{ef.Line, name, "relative env_file", p})
					}
				}
			}
		}
	}
	return u
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func seq(n *yaml.Node) []*yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	return n.Content
}

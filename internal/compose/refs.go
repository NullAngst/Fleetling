package compose

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Reference is one host path a compose project uses. Removing a stack must
// never delete a path another stack still references, so every stack on
// disk is read for these before anything is deleted.
type Reference struct {
	Path    string // absolute and clean
	Service string // empty for project-level paths: configs, secrets, include
	What    string // "bind mount", "env file", "build context", ...
	// Partial is set when a variable hid the rest of the path and only the
	// folders before it are known.
	Partial bool
}

// ConfigReferences reads every host path out of `docker compose config
// --format json`, where Compose has already filled in variables and made
// relative paths absolute.
func ConfigReferences(configJSON []byte) ([]Reference, error) {
	var cfg struct {
		Services map[string]struct {
			Volumes []struct {
				Type   string `json:"type"`
				Source string `json:"source"`
			} `json:"volumes"`
			EnvFile json.RawMessage `json:"env_file"`
			Build   json.RawMessage `json:"build"`
		} `json:"services"`
		Configs map[string]struct {
			File string `json:"file"`
		} `json:"configs"`
		Secrets map[string]struct {
			File string `json:"file"`
		} `json:"secrets"`
		Volumes map[string]struct {
			DriverOpts map[string]string `json:"driver_opts"`
		} `json:"volumes"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parse compose config: %w", err)
	}
	var out []Reference
	add := func(p, svc, what string) {
		if p != "" && filepath.IsAbs(p) {
			out = append(out, Reference{Path: filepath.Clean(p), Service: svc, What: what})
		}
	}
	for name, svc := range cfg.Services {
		for _, v := range svc.Volumes {
			if v.Type == "bind" {
				add(v.Source, name, "bind mount")
			}
		}
		// env_file is a list of {path, required} in current Compose and a
		// list of strings in older releases.
		var envObjs []struct {
			Path string `json:"path"`
		}
		var envStrs []string
		if json.Unmarshal(svc.EnvFile, &envObjs) == nil {
			for _, e := range envObjs {
				add(e.Path, name, "env file")
			}
		} else if json.Unmarshal(svc.EnvFile, &envStrs) == nil {
			for _, e := range envStrs {
				add(e, name, "env file")
			}
		}
		var build struct {
			Context    string            `json:"context"`
			Dockerfile string            `json:"dockerfile"`
			Additional map[string]string `json:"additional_contexts"`
		}
		if json.Unmarshal(svc.Build, &build) == nil && build.Context != "" {
			add(build.Context, name, "build context")
			if build.Dockerfile != "" {
				df := build.Dockerfile
				if !filepath.IsAbs(df) && filepath.IsAbs(build.Context) {
					df = filepath.Join(build.Context, df)
				}
				add(df, name, "Dockerfile")
			}
			for _, c := range build.Additional {
				add(c, name, "build context")
			}
		}
	}
	for _, c := range cfg.Configs {
		add(c.File, "", "config file")
	}
	for _, s := range cfg.Secrets {
		add(s.File, "", "secret file")
	}
	for _, v := range cfg.Volumes {
		add(v.DriverOpts["device"], "", "volume device")
	}
	sortRefs(out)
	return out, nil
}

// StaticReferences reads the same paths from the compose text as written,
// resolving relative ones against dir the way Compose does. It covers what
// the resolved config leaves out (include and extends files) and stands in
// when Compose can't resolve the file at all.
//
// A value built from a variable, like ${DATA}/plex, is cut back to the
// last folder written out in full. When nothing usable is left the value
// goes into unresolved, so the caller knows this stack's paths aren't
// fully known.
func StaticReferences(composeText []byte, dir string) (refs []Reference, unresolved []string) {
	var doc yaml.Node
	if yaml.Unmarshal(composeText, &doc) != nil || len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	add := func(raw, svc, what string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		p, partial, ok := staticPath(raw, dir)
		if !ok {
			unresolved = append(unresolved, raw)
			return
		}
		if p != "" {
			refs = append(refs, Reference{Path: p, Service: svc, What: what, Partial: partial})
		}
	}
	scalar := func(n *yaml.Node) string {
		if n != nil && n.Kind == yaml.ScalarNode {
			return n.Value
		}
		return ""
	}

	if svcs := mapValue(root, "services"); svcs != nil && svcs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(svcs.Content); i += 2 {
			name, svc := svcs.Content[i].Value, svcs.Content[i+1]
			if vols := mapValue(svc, "volumes"); vols != nil && vols.Kind == yaml.SequenceNode {
				for _, v := range vols.Content {
					switch v.Kind {
					case yaml.ScalarNode:
						src, _, _ := strings.Cut(v.Value, ":")
						if looksLikePath(src) {
							add(src, name, "bind mount")
						}
					case yaml.MappingNode:
						if t := scalar(mapValue(v, "type")); t == "bind" || t == "" && looksLikePath(scalar(mapValue(v, "source"))) {
							add(scalar(mapValue(v, "source")), name, "bind mount")
						}
					}
				}
			}
			if ef := mapValue(svc, "env_file"); ef != nil {
				addEnvFiles(ef, func(n *yaml.Node) { add(scalar(n), name, "env file") })
			}
			switch b := mapValue(svc, "build"); {
			case b == nil:
			case b.Kind == yaml.ScalarNode:
				if !isRemoteContext(b.Value) {
					add(b.Value, name, "build context")
				}
			case b.Kind == yaml.MappingNode:
				if ctx := scalar(mapValue(b, "context")); ctx != "" && !isRemoteContext(ctx) {
					add(ctx, name, "build context")
				}
			}
			if ext := mapValue(svc, "extends"); ext != nil && ext.Kind == yaml.MappingNode {
				add(scalar(mapValue(ext, "file")), name, "extends file")
			}
		}
	}
	if inc := mapValue(root, "include"); inc != nil && inc.Kind == yaml.SequenceNode {
		for _, it := range inc.Content {
			switch it.Kind {
			case yaml.ScalarNode:
				add(it.Value, "", "include")
			case yaml.MappingNode:
				p := mapValue(it, "path")
				if p != nil && p.Kind == yaml.SequenceNode {
					for _, x := range p.Content {
						add(scalar(x), "", "include")
					}
				} else {
					add(scalar(p), "", "include")
				}
				if pd := scalar(mapValue(it, "project_directory")); pd != "" {
					add(pd, "", "include")
				}
			}
		}
	}
	for _, sect := range []struct{ key, what string }{{"configs", "config file"}, {"secrets", "secret file"}} {
		if m := mapValue(root, sect.key); m != nil && m.Kind == yaml.MappingNode {
			for i := 1; i < len(m.Content); i += 2 {
				add(scalar(mapValue(m.Content[i], "file")), "", sect.what)
			}
		}
	}
	if m := mapValue(root, "volumes"); m != nil && m.Kind == yaml.MappingNode {
		for i := 1; i < len(m.Content); i += 2 {
			if opts := mapValue(m.Content[i], "driver_opts"); opts != nil {
				if dev := scalar(mapValue(opts, "device")); looksLikePath(dev) {
					add(dev, "", "volume device")
				}
			}
		}
	}
	sortRefs(refs)
	return refs, unresolved
}

// looksLikePath tells a bind source from a named volume in short syntax:
// Compose treats a source as a path when it starts with /, . or ~.
func looksLikePath(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, "$")
}

func isRemoteContext(s string) bool {
	for _, p := range []string{"http://", "https://", "git://", "git@", "github.com/", "ssh://"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// staticPath turns a path as written into an absolute one. partial says a
// variable cut it short. ok is false when a variable hides everything
// useful. An empty result with ok true means the path is in a home folder,
// which is never under the root.
func staticPath(raw, dir string) (p string, partial, ok bool) {
	if strings.HasPrefix(raw, "~") {
		return "", false, true
	}
	if i := strings.Index(raw, "$"); i >= 0 {
		// Keep only the folders written out in full before the variable:
		// "/opt/plex/${X}" keeps /opt/plex, "/opt/plex-${X}" keeps /opt.
		prefix := raw[:i]
		if j := strings.LastIndex(prefix, "/"); j >= 0 {
			prefix = prefix[:j]
		} else {
			prefix = ""
		}
		if prefix == "" && !strings.HasPrefix(raw, ".") {
			return "", false, false
		}
		raw, partial = prefix, true
		if raw == "" || raw == "." {
			// "./${X}" is somewhere inside the project folder.
			raw = "."
		}
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(dir, raw)
	}
	return filepath.Clean(raw), partial, true
}

func sortRefs(r []Reference) {
	slices.SortFunc(r, func(a, b Reference) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if c := strings.Compare(a.Service, b.Service); c != 0 {
			return c
		}
		return strings.Compare(a.What, b.What)
	})
}

// ConfigVolume is one named volume a project declares.
type ConfigVolume struct {
	Key      string // its key under volumes:
	Name     string // the engine's name for it, <project>_<key> unless name: says otherwise
	External bool   // made outside the project; `down -v` leaves it alone
}

// ConfigVolumes lists the named volumes in `docker compose config --format
// json` output.
func ConfigVolumes(configJSON []byte, project string) ([]ConfigVolume, error) {
	var cfg struct {
		Volumes map[string]struct {
			Name     string `json:"name"`
			External bool   `json:"external"`
		} `json:"volumes"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parse compose config: %w", err)
	}
	var out []ConfigVolume
	for k, v := range cfg.Volumes {
		name := v.Name
		if name == "" {
			name = project + "_" + k
		}
		out = append(out, ConfigVolume{Key: k, Name: name, External: v.External})
	}
	slices.SortFunc(out, func(a, b ConfigVolume) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ConfigImages lists the image each service runs, which is what
// `down --rmi all` removes. A service with build: and no image: gets
// Compose's default name, <project>-<service>.
func ConfigImages(configJSON []byte, project string) ([]string, error) {
	var cfg struct {
		Services map[string]struct {
			Image string          `json:"image"`
			Build json.RawMessage `json:"build"`
		} `json:"services"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parse compose config: %w", err)
	}
	var out []string
	for name, svc := range cfg.Services {
		img := svc.Image
		if img == "" && len(svc.Build) > 0 && string(svc.Build) != "null" {
			img = project + "-" + name
		}
		if img != "" && !slices.Contains(out, img) {
			out = append(out, img)
		}
	}
	slices.Sort(out)
	return out, nil
}

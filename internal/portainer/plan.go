package portainer

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/NullAngst/Fleetling/internal/compose"
)

// FolderGuess picks the stack folder for an imported compose file: the
// <root>/<folder> its bind mounts reference most. Only the compose text
// counts: the mounts a relative path resolved to under Portainer point into
// Portainer's own data folder, which is never the right home. ok is false
// on a tie or when nothing points under the root; the caller then proposes
// <root>/<name> and asks.
func FolderGuess(composeText, root, name string) (folder string, ok bool) {
	root = filepath.Clean(root)
	counts := map[string]int{}
	add := func(p string) {
		if !filepath.IsAbs(p) {
			return
		}
		p = filepath.Clean(p)
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return
		}
		first := strings.Split(rel, string(filepath.Separator))[0]
		if compose.ValidFolderName(first) {
			counts[first]++
		}
	}
	for _, v := range volumeSources(composeText) {
		add(v.source)
	}
	best, bestN, tie := "", 0, false
	for f, n := range counts {
		switch {
		case n > bestN:
			best, bestN, tie = f, n, false
		case n == bestN:
			tie = true
		}
	}
	if best == "" || tie {
		return name, false
	}
	return best, true
}

type volumeRef struct {
	service string
	source  string
	target  string
	node    *yaml.Node // the scalar holding the source, for in-place rewriting
	short   bool       // "src:dst[:opts]" form
}

// volumeSources lists every service volume with its host side and target.
func volumeSources(composeText string) []volumeRef {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(composeText), &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	services := mapValue(doc.Content[0], "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil
	}
	var out []volumeRef
	for i := 0; i+1 < len(services.Content); i += 2 {
		name := services.Content[i].Value
		vols := mapValue(services.Content[i+1], "volumes")
		if vols == nil || vols.Kind != yaml.SequenceNode {
			continue
		}
		for _, v := range vols.Content {
			switch v.Kind {
			case yaml.ScalarNode:
				parts := strings.SplitN(v.Value, ":", 3)
				if len(parts) < 2 {
					continue
				}
				out = append(out, volumeRef{service: name, source: parts[0], target: parts[1], node: v, short: true})
			case yaml.MappingNode:
				if t := mapValue(v, "type"); t == nil || t.Value != "bind" {
					continue
				}
				src, dst := mapValue(v, "source"), mapValue(v, "target")
				if src == nil || dst == nil {
					continue
				}
				out = append(out, volumeRef{service: name, source: src.Value, target: dst.Value, node: src})
			}
		}
	}
	return out
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

// Fix is one proposed rewrite of a relative bind source to the absolute
// host path the running container actually uses.
type Fix struct {
	Service string
	Target  string
	From    string // as written, e.g. ./conf
	To      string // from inspect, e.g. /opt/portainer/compose/12/conf
	Line    int
}

// RelativeFixes finds bind sources written relative to the compose file and
// proposes the absolute path each one resolved to under Portainer. mounts
// maps a service name to its container's mounts. A relative source with no
// matching mount gets no fix; the caller warns about it.
func RelativeFixes(composeText string, mounts map[string][]compose.Mount) (fixes []Fix, unresolved []string) {
	for _, v := range volumeSources(composeText) {
		if !strings.HasPrefix(v.source, ".") {
			continue
		}
		found := ""
		for _, m := range mounts[v.service] {
			if filepath.Clean(m.Destination) == filepath.Clean(v.target) {
				found = m.Source
				break
			}
		}
		if found == "" {
			unresolved = append(unresolved, fmt.Sprintf("%s: %s", v.service, v.source))
			continue
		}
		fixes = append(fixes, Fix{Service: v.service, Target: v.target, From: v.source, To: found, Line: v.node.Line})
	}
	return fixes, unresolved
}

// ApplyFixes rewrites only the source text of each fix, leaving every other
// byte of the file alone: comments, quoting, ordering and blank lines stay.
func ApplyFixes(composeText string, fixes []Fix) (string, error) {
	lines := strings.SplitAfter(composeText, "\n")
	for _, f := range fixes {
		i := f.Line - 1
		if i < 0 || i >= len(lines) {
			return "", fmt.Errorf("line %d is outside the file", f.Line)
		}
		// Match the source where it sits: at the start of a short-syntax
		// value or after "source:", possibly quoted.
		re := regexp.MustCompile(`(^|[\s"'-]|source:\s*["']?)` + regexp.QuoteMeta(f.From) + `([:"'\s]|$)`)
		loc := re.FindStringSubmatchIndex(lines[i])
		if loc == nil {
			return "", fmt.Errorf("line %d: could not find %s to rewrite", f.Line, f.From)
		}
		start := loc[3] // end of the leading group
		lines[i] = lines[i][:start] + f.To + lines[i][start+len(f.From):]
	}
	return strings.Join(lines, ""), nil
}

// EnvText renders Portainer's env pairs the way Portainer itself writes
// stack.env: one raw NAME=value per line, so Compose reads them exactly as
// it did under Portainer. A value with a newline can't be written that way
// and is reported.
func EnvText(pairs []Pair) (string, []string) {
	var b strings.Builder
	var problems []string
	for _, p := range pairs {
		if strings.ContainsAny(p.Value, "\r\n") {
			problems = append(problems, p.Name+" holds a line break and was left out; add it by hand")
			continue
		}
		b.WriteString(p.Name + "=" + p.Value + "\n")
	}
	return b.String(), problems
}

var stackEnvRe = regexp.MustCompile(`(?m)(^|[\s"'/-])stack\.env(["'\s]|$)`)

// UsesStackEnv reports whether the compose file reads Portainer's stack.env.
func UsesStackEnv(composeText string) bool { return stackEnvRe.MatchString(composeText) }

// EnvSuggestions are the variables set on a container that its image did
// not set. Under the offline fallback they are the best available guess at
// the stack's env vars, offered one by one, since some came from
// environment: lines in the compose file instead. Sorted, de-duplicated.
func EnvSuggestions(containerEnv, imageEnv []string) []string {
	base := map[string]string{}
	for _, kv := range imageEnv {
		k, v, _ := strings.Cut(kv, "=")
		base[k] = v
	}
	seen := map[string]bool{}
	var out []string
	for _, kv := range containerEnv {
		k, v, _ := strings.Cut(kv, "=")
		if old, ok := base[k]; ok && old == v {
			continue
		}
		if !seen[kv] {
			seen[kv] = true
			out = append(out, kv)
		}
	}
	slices.Sort(out)
	return out
}

// ComposeEnvKeys lists keys a service sets under environment:, so the UI
// can say a suggestion probably came from the compose file.
func ComposeEnvKeys(composeText string) map[string]bool {
	var doc struct {
		Services map[string]struct {
			Environment yaml.Node `yaml:"environment"`
		} `yaml:"services"`
	}
	keys := map[string]bool{}
	if yaml.Unmarshal([]byte(composeText), &doc) != nil {
		return keys
	}
	for _, s := range doc.Services {
		env := s.Environment
		switch env.Kind {
		case yaml.MappingNode:
			for i := 0; i < len(env.Content); i += 2 {
				keys[env.Content[i].Value] = true
			}
		case yaml.SequenceNode:
			for _, n := range env.Content {
				k, _, _ := strings.Cut(n.Value, "=")
				keys[k] = true
			}
		}
	}
	return keys
}

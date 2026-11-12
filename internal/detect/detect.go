// Package detect reads a compose file for signs that it was written for
// Podman, so the create form can suggest the engine and say why. Docker
// stays the default: a file with no Podman signal gets no suggestion.
//
// Every signal carries the line it was found on, read from the YAML node
// positions, so the reason can point at it: "line 14 uses userns_mode:
// keep-id".
package detect

import (
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Signal is one thing in the file that only makes sense on Podman.
type Signal struct {
	Line   int
	What   string // the key and value as written, e.g. "userns_mode: keep-id"
	Reason string // why it points at Podman
}

// Result is the suggestion for one compose file.
type Result struct {
	Engine  string // "podman", or "" for no suggestion (Docker, the default)
	Signals []Signal
}

// Why is the one-line explanation the form shows.
func (r Result) Why() string {
	if r.Engine == "" || len(r.Signals) == 0 {
		return ""
	}
	s := r.Signals[0]
	msg := fmt.Sprintf("Suggested Podman: line %d uses %s", s.Line, s.What)
	if n := len(r.Signals) - 1; n > 0 {
		msg += fmt.Sprintf(" (and %d more Podman-only setting", n)
		if n > 1 {
			msg += "s"
		}
		msg += ")"
	}
	return msg
}

// The signal table. Each entry is a Podman-only setting; Docker either
// rejects it or ignores it silently, which is worse.
//
//	userns_mode: keep-id[:...]     Podman's user namespace mode
//	group_add: keep-groups         Podman only
//	network_mode: slirp4netns|pasta Podman's rootless network stacks
//	volume option U                Podman chowns the source to the container user
//	x-podman: ...                  Podman Compose extension, top level or per service
//	image: localhost/...           where Podman puts locally built images
//	a bind of podman.sock          the stack talks to Podman's own API

// Detect parses composeText and lists the Podman signals in it, in file
// order. A file that doesn't parse gives an empty result; validation is
// Compose's job, not this one.
func Detect(composeText string) Result {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(composeText), &doc) != nil || len(doc.Content) == 0 {
		return Result{}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return Result{}
	}
	var sigs []Signal
	add := func(n *yaml.Node, what, why string) {
		sigs = append(sigs, Signal{Line: n.Line, What: what, Reason: why})
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if k := root.Content[i]; k.Value == "x-podman" {
			add(k, "x-podman", "a Podman Compose extension")
		}
	}
	svcs := value(root, "services")
	if svcs != nil && svcs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(svcs.Content); i += 2 {
			svc := svcs.Content[i+1]
			if svc.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(svc.Content); j += 2 {
				k, v := svc.Content[j], svc.Content[j+1]
				switch k.Value {
				case "x-podman":
					add(k, "x-podman", "a Podman Compose extension")
				case "userns_mode":
					if strings.HasPrefix(v.Value, "keep-id") {
						add(v, "userns_mode: "+v.Value, "keep-id is Podman's user namespace mode")
					}
				case "network_mode":
					if v.Value == "slirp4netns" || strings.HasPrefix(v.Value, "slirp4netns:") || v.Value == "pasta" || strings.HasPrefix(v.Value, "pasta:") {
						add(v, "network_mode: "+v.Value, "a Podman network stack")
					}
				case "group_add":
					for _, g := range seq(v) {
						if g.Value == "keep-groups" {
							add(g, "group_add: keep-groups", "Podman only")
						}
					}
				case "image":
					if strings.HasPrefix(v.Value, "localhost/") {
						add(v, "image: "+v.Value, "Podman tags local builds under localhost/")
					}
				case "volumes":
					for _, vol := range seq(v) {
						volumeSignals(vol, add)
					}
				}
			}
		}
	}
	slices.SortStableFunc(sigs, func(a, b Signal) int { return a.Line - b.Line })
	r := Result{Signals: sigs}
	if len(sigs) > 0 {
		r.Engine = "podman"
	}
	return r
}

// volumeSignals looks at one service volume, short or long syntax.
func volumeSignals(vol *yaml.Node, add func(*yaml.Node, string, string)) {
	var src, opts string
	switch vol.Kind {
	case yaml.ScalarNode:
		parts := strings.Split(vol.Value, ":")
		src = parts[0]
		if len(parts) >= 3 {
			opts = parts[len(parts)-1]
		}
	case yaml.MappingNode:
		if s := value(vol, "source"); s != nil {
			src = s.Value
		}
	}
	if strings.HasSuffix(src, "/podman.sock") || strings.HasSuffix(src, "/podman/podman.sock") {
		add(vol, "a bind of "+src, "the stack talks to Podman's own API")
	}
	for _, o := range strings.Split(opts, ",") {
		if o == "U" {
			add(vol, "the volume option U ("+vol.Value+")", "only Podman chowns a volume source with U")
		}
	}
}

func value(m *yaml.Node, key string) *yaml.Node {
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

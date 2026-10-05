package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// A Snapshot records the files a stack's deployment is made from, as
// Fleetling last wrote or approved them. Before any compose action the
// stack's files are compared against it. A container that bind-mounts its
// own stack folder can rewrite these files, and without the check the next
// deploy (or a scheduled update) would run whatever it wrote.
//
// Covered: the compose file, .env, .fleetling.toml, every env_file a
// service names, every file pulled in with include:, and every
// extends: file. Not covered: build contexts and Dockerfiles, and the
// files behind configs:/secrets:, which apps often own and rewrite.

// Snapshot is the recorded state of a stack's deployment files.
type Snapshot struct {
	Files []FileState `json:"files"`
}

// FileState is one file in a snapshot. Content is kept so a review can
// show a diff; files over maxSnapshotContent are kept as a hash only.
type FileState struct {
	Path     string `json:"path"` // relative to the stack folder when inside it
	Exists   bool   `json:"exists"`
	SHA256   string `json:"sha256,omitempty"`
	Content  string `json:"content,omitempty"`
	TooLarge bool   `json:"too_large,omitempty"`
}

const maxSnapshotContent = 1 << 20

// TakeSnapshot reads the current deployment files of a stack folder.
func TakeSnapshot(f Folder) (Snapshot, error) {
	paths := []string{MetaFile, ".env"}
	if f.ComposeFile != "" {
		paths = append([]string{f.ComposeFile}, paths...)
		if b, err := readSmall(filepath.Join(f.Dir, f.ComposeFile)); err == nil {
			paths = append(paths, referencedFiles(b)...)
		}
	}
	seen := map[string]bool{}
	var snap Snapshot
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(p) {
			abs = filepath.Join(f.Dir, p)
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		fs, err := fileState(abs)
		if err != nil {
			return Snapshot{}, err
		}
		fs.Path = displayPath(f.Dir, abs)
		snap.Files = append(snap.Files, fs)
	}
	return snap, nil
}

func displayPath(dir, abs string) string {
	if rel, err := filepath.Rel(dir, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return abs
}

// fileState hashes one file, following symlinks: what matters is the
// content Compose will read. A folder where a file should be counts as a
// change too.
func fileState(path string) (FileState, error) {
	fh, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return FileState{}, nil
	}
	if err != nil {
		return FileState{}, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return FileState{}, err
	}
	if !fi.Mode().IsRegular() {
		return FileState{Exists: true, SHA256: "not a regular file: " + fi.Mode().Type().String()}, nil
	}
	h := sha256.New()
	var buf strings.Builder
	w := io.Writer(h)
	if fi.Size() <= maxSnapshotContent {
		w = io.MultiWriter(h, &buf)
	}
	if _, err := io.Copy(w, fh); err != nil {
		return FileState{}, err
	}
	st := FileState{Exists: true, SHA256: hex.EncodeToString(h.Sum(nil))}
	if fi.Size() <= maxSnapshotContent && buf.Len() <= maxSnapshotContent {
		st.Content = buf.String()
	} else {
		st.TooLarge = true
	}
	return st, nil
}

// referencedFiles lists the extra files a compose file reads: env_file,
// include and extends. Paths are as written; relative ones resolve against
// the stack folder, the same as Compose resolves them.
func referencedFiles(composeText []byte) []string {
	var doc yaml.Node
	if yaml.Unmarshal(composeText, &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	var out []string
	add := func(n *yaml.Node) {
		if n != nil && n.Kind == yaml.ScalarNode && n.Value != "" && !strings.Contains(n.Value, "$") {
			out = append(out, n.Value)
		}
	}
	// include: - path | - path: [a, b] | - path: a
	if inc := mapValue(root, "include"); inc != nil && inc.Kind == yaml.SequenceNode {
		for _, it := range inc.Content {
			switch it.Kind {
			case yaml.ScalarNode:
				add(it)
			case yaml.MappingNode:
				p := mapValue(it, "path")
				if p != nil && p.Kind == yaml.SequenceNode {
					for _, x := range p.Content {
						add(x)
					}
				} else {
					add(p)
				}
				if ef := mapValue(it, "env_file"); ef != nil {
					addEnvFiles(ef, add)
				}
			}
		}
	}
	if svcs := mapValue(root, "services"); svcs != nil && svcs.Kind == yaml.MappingNode {
		for i := 1; i < len(svcs.Content); i += 2 {
			svc := svcs.Content[i]
			if ef := mapValue(svc, "env_file"); ef != nil {
				addEnvFiles(ef, add)
			}
			if ext := mapValue(svc, "extends"); ext != nil && ext.Kind == yaml.MappingNode {
				add(mapValue(ext, "file"))
			}
		}
	}
	return out
}

// env_file: path | [path, ...] | [{path: ..., required: false}, ...]
func addEnvFiles(n *yaml.Node, add func(*yaml.Node)) {
	switch n.Kind {
	case yaml.ScalarNode:
		add(n)
	case yaml.SequenceNode:
		for _, it := range n.Content {
			if it.Kind == yaml.MappingNode {
				add(mapValue(it, "path"))
			} else {
				add(it)
			}
		}
	}
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

// FileChange is one file that differs between an approved snapshot and now.
type FileChange struct {
	Path     string
	Kind     string // "changed", "added", "removed"
	Old      FileState
	New      FileState
	Diffable bool // both sides have content to show
}

// Changes lists the differences from approved to current, in a stable
// order. An empty result means nothing changed.
func Changes(approved, current Snapshot) []FileChange {
	idx := func(s Snapshot) map[string]FileState {
		m := map[string]FileState{}
		for _, f := range s.Files {
			m[f.Path] = f
		}
		return m
	}
	a, c := idx(approved), idx(current)
	var paths []string
	for p := range a {
		paths = append(paths, p)
	}
	for p := range c {
		if _, ok := a[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	var out []FileChange
	for _, p := range paths {
		o, n := a[p], c[p]
		if o.Exists == n.Exists && o.SHA256 == n.SHA256 {
			continue
		}
		ch := FileChange{Path: p, Old: o, New: n, Kind: "changed"}
		switch {
		case !o.Exists:
			ch.Kind = "added"
		case !n.Exists:
			ch.Kind = "removed"
		}
		ch.Diffable = !o.TooLarge && !n.TooLarge
		out = append(out, ch)
	}
	return out
}

// Fingerprint is a short stable digest of a snapshot, used to make sure an
// approval covers exactly the files the reviewer saw.
func (s Snapshot) Fingerprint() string {
	h := sha256.New()
	for _, f := range s.Files {
		io.WriteString(h, f.Path+"\x00")
		if f.Exists {
			io.WriteString(h, f.SHA256)
		} else {
			io.WriteString(h, "-")
		}
		io.WriteString(h, "\x00")
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

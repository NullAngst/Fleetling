package compose

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"go.yaml.in/yaml/v3"
)

// MetaFile is the metadata file Fleetling owns inside each stack folder.
const MetaFile = ".fleetling.toml"

// ComposeFileNames in the order Compose itself picks them when a folder has
// more than one.
var ComposeFileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

var overrideFileNames = []string{"compose.override.yaml", "compose.override.yml", "docker-compose.override.yml", "docker-compose.override.yaml"}

// Biggest compose or env file read during discovery. Real ones are a few KB.
const maxReadSize = 4 << 20

// Meta is the content of .fleetling.toml.
type Meta struct {
	Project      string     `toml:"project"`
	Engine       string     `toml:"engine"`
	Created      time.Time  `toml:"created"`
	CreatedPaths []string   `toml:"created_paths"`
	Deployed     *time.Time `toml:"deployed,omitempty"` // first successful up; path tracking runs until it is set
	GitRepo      string     `toml:"git_repo,omitempty"`
}

// Engine slot names. A stack runs on one of the two configured endpoints.
const (
	EngineDocker = "docker"
	EnginePodman = "podman"
)

// Folder is one stack folder found under the root.
type Folder struct {
	Name        string // folder name, e.g. "copyparty"
	Dir         string // absolute path, e.g. "/opt/copyparty"
	ComposeFile string // file name inside Dir, empty if missing
	Meta        *Meta  // nil when there is no usable .fleetling.toml

	// Project is the project name from the metadata, or Compose's own
	// default for this folder when there is none. ProjectSource says which.
	Project       string
	ProjectSource string

	Services []string // service names from the compose file, sorted
	Warnings []string
}

// Discover scans the direct children of root for stack folders. A folder
// counts when it has a compose file or a .fleetling.toml. Hidden folders,
// symlinks and names on the ignore list are skipped.
func Discover(root string, ignore []string) ([]Folder, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Folder
	for _, e := range entries {
		name := e.Name()
		// e.IsDir is false for a symlink to a folder, which is what we want:
		// a stack folder has to really live under the root.
		if !e.IsDir() || strings.HasPrefix(name, ".") || slices.Contains(ignore, name) {
			continue
		}
		f, ok := readFolder(filepath.Join(root, name))
		if ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// ReadFolder reads one stack folder. ok is false when it holds neither a
// compose file nor a .fleetling.toml.
func ReadFolder(dir string) (Folder, bool) { return readFolder(dir) }

// ValidFolderName reports whether name is usable as a stack folder directly
// under the root: one path element, not hidden.
func ValidFolderName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.HasPrefix(name, ".") &&
		!strings.ContainsAny(name, "/\\\x00") && len(name) <= 255
}

func readFolder(dir string) (Folder, bool) {
	f := Folder{Name: filepath.Base(dir), Dir: dir}

	var found []string
	for _, n := range ComposeFileNames {
		if fileExists(filepath.Join(dir, n)) {
			found = append(found, n)
		}
	}
	hasMeta := fileExists(filepath.Join(dir, MetaFile))
	if len(found) == 0 && !hasMeta {
		return f, false
	}
	if len(found) > 0 {
		f.ComposeFile = found[0]
	}
	if len(found) > 1 {
		f.Warnings = append(f.Warnings, fmt.Sprintf("found %s; Compose uses %s", strings.Join(found, " and "), found[0]))
	}
	for _, n := range overrideFileNames {
		if fileExists(filepath.Join(dir, n)) {
			f.Warnings = append(f.Warnings, n+" exists. Fleetling runs Compose with -f, so the override file is not applied")
			break
		}
	}

	if hasMeta {
		m, err := ReadMeta(dir)
		if err != nil {
			f.Warnings = append(f.Warnings, "metadata unreadable, treated as unmanaged: "+err.Error())
		} else {
			f.Meta = m
		}
	}

	var top *topLevel
	if f.ComposeFile == "" {
		f.Warnings = append(f.Warnings, "has "+MetaFile+" but no compose file")
	} else {
		t, err := readTopLevel(filepath.Join(dir, f.ComposeFile))
		if err != nil {
			f.Warnings = append(f.Warnings, "compose file did not parse: "+err.Error())
		} else {
			top = t
			f.Services = t.services
			if t.hasInclude {
				f.Warnings = append(f.Warnings, "uses include:, so the service count only covers this file")
			}
		}
	}

	f.Project, f.ProjectSource = projectFor(f, top)
	return f, true
}

// projectFor works out the project name, highest priority first:
// metadata, COMPOSE_PROJECT_NAME in .env, top-level name:, folder name.
func projectFor(f Folder, top *topLevel) (string, string) {
	if f.Meta != nil {
		return f.Meta.Project, "metadata"
	}
	if b, err := readSmall(filepath.Join(f.Dir, ".env")); err == nil {
		if vars, err := ParseEnv(bytes.NewReader(b)); err == nil {
			if v := EnvMap(vars)["COMPOSE_PROJECT_NAME"]; v != "" && ValidProject(v) == nil {
				return v, ".env"
			}
		}
	}
	// A name: with ${...} in it needs interpolation, which only Compose can do.
	if top != nil && top.name != "" && !strings.Contains(top.name, "$") && ValidProject(top.name) == nil {
		return top.name, "name: in compose file"
	}
	return NormalizeProject(f.Name), "folder name"
}

// ReadMeta reads and checks <dir>/.fleetling.toml.
func ReadMeta(dir string) (*Meta, error) {
	b, err := readSmall(filepath.Join(dir, MetaFile))
	if err != nil {
		return nil, err
	}
	var m Meta
	md, err := toml.Decode(string(b), &m)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("unknown key %q", und[0].String())
	}
	if err := ValidProject(m.Project); err != nil {
		return nil, err
	}
	if m.Engine != EngineDocker && m.Engine != EnginePodman {
		return nil, fmt.Errorf("engine must be %q or %q, got %q", EngineDocker, EnginePodman, m.Engine)
	}
	return &m, nil
}

type topLevel struct {
	name       string
	services   []string
	hasInclude bool
}

// readTopLevel pulls the few top-level facts discovery needs out of a
// compose file without interpreting it. Full resolution is Compose's job.
func readTopLevel(path string) (*topLevel, error) {
	b, err := readSmall(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	t := &topLevel{}
	if len(doc.Content) == 0 {
		return t, nil // empty file
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, errors.New("top level is not a mapping")
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		switch k.Value {
		case "name":
			if v.Kind == yaml.ScalarNode {
				t.name = v.Value
			}
		case "include":
			t.hasInclude = true
		case "services":
			if v.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(v.Content); j += 2 {
				t.services = append(t.services, v.Content[j].Value)
			}
		}
	}
	slices.Sort(t.services)
	return t, nil
}

func readSmall(path string) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, maxReadSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxReadSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), maxReadSize)
	}
	return b, nil
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

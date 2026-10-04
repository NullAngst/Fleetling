package compose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
	"go.yaml.in/yaml/v3"
)

// Owner is a uid/gid pair. Files written into an existing folder take the
// folder's owner, so /opt/jellyfin stays owned by whoever owned it.
type Owner struct {
	UID, GID int
	Set      bool
}

// OwnerOf reads the owner of path.
func OwnerOf(path string) Owner {
	fi, err := os.Stat(path)
	if err != nil {
		return Owner{}
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return Owner{UID: int(st.Uid), GID: int(st.Gid), Set: true}
	}
	return Owner{}
}

// WriteFileAtomic writes data to path through a temp file in the same
// folder and a rename, so a crash never leaves half a compose file. The
// bytes are written exactly as given.
func WriteFileAtomic(path string, data []byte, mode os.FileMode, owner Owner) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".fleetling-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	if owner.Set {
		if err := os.Lchown(name, owner.UID, owner.GID); err != nil && !errors.Is(err, os.ErrPermission) {
			return err
		}
	}
	return os.Rename(name, path)
}

// WriteMeta writes <dir>/.fleetling.toml.
func WriteMeta(dir string, m *Meta, owner Owner) error {
	if err := ValidProject(m.Project); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# Written by Fleetling. The project name is passed to Compose with -p.\n")
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(dir, MetaFile), buf.Bytes(), 0o644, owner)
}

// FolderBindMounts finds volumes in a compose file whose host side is the
// stack folder itself, like "- /opt/copyparty:/cfg:z" or "- .:/data". That
// lets the container read .env and edit compose.yaml. It returns the
// offending service names.
func FolderBindMounts(composeText []byte, dir string) []string {
	var doc struct {
		Services map[string]struct {
			Volumes []yaml.Node `yaml:"volumes"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal(composeText, &doc) != nil {
		return nil
	}
	dir = filepath.Clean(dir)
	var out []string
	for name, svc := range doc.Services {
		for _, v := range svc.Volumes {
			var src string
			switch v.Kind {
			case yaml.ScalarNode:
				src, _, _ = strings.Cut(v.Value, ":")
			case yaml.MappingNode:
				var long struct {
					Type   string `yaml:"type"`
					Source string `yaml:"source"`
				}
				if v.Decode(&long) != nil || long.Type != "bind" {
					continue
				}
				src = long.Source
			}
			if src == "" || strings.Contains(src, "$") {
				continue
			}
			if !filepath.IsAbs(src) {
				if !strings.HasPrefix(src, ".") {
					continue // a named volume
				}
				src = filepath.Join(dir, src)
			}
			if filepath.Clean(src) == dir {
				out = append(out, name)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// BindSources reads the host side of every bind mount from the output of
// `docker compose config --format json`, which has every path made
// absolute and every variable filled in.
func BindSources(configJSON []byte) ([]string, error) {
	var cfg struct {
		Services map[string]struct {
			Volumes []struct {
				Type   string `json:"type"`
				Source string `json:"source"`
			} `json:"volumes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parse compose config: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, svc := range cfg.Services {
		for _, v := range svc.Volumes {
			if v.Type != "bind" || v.Source == "" || seen[v.Source] {
				continue
			}
			seen[v.Source] = true
			out = append(out, filepath.Clean(v.Source))
		}
	}
	slices.Sort(out)
	return out, nil
}

// Missing returns the paths in list that do not exist right now. Lstat, so
// a dangling symlink counts as existing and is never claimed later.
func Missing(list []string) []string {
	var out []string
	for _, p := range list {
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			out = append(out, p)
		}
	}
	return out
}

// NewlyCreated returns the paths from before that exist now and sit under
// root. Docker silently creates missing bind sources as root-owned folders;
// these are the ones a later "remove with folders" may offer to delete.
func NewlyCreated(before []string, root string) []string {
	root = filepath.Clean(root)
	var out []string
	for _, p := range before {
		if !isUnder(filepath.Clean(p), root) {
			continue
		}
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// MergePaths adds new paths to a list without duplicates, sorted.
func MergePaths(have, add []string) []string {
	out := slices.Clone(have)
	for _, p := range add {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

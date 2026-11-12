package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"go.yaml.in/yaml/v3"
)

// Build context coverage for drift review. A stack that builds from a
// local folder deploys whatever is in that folder, so a container that can
// write there can change what gets built. Each local build context is
// walked the way the builder would send it: .dockerignore honored, the
// Dockerfile and .dockerignore always kept. Paths the stack bind-mounts
// are skipped, since app data changes constantly and would flag the stack
// every day. The walk stops at the caps below; past them the snapshot is
// marked partial and the stack page says so.

const (
	maxContextFiles = 5000
	maxContextBytes = 256 << 20 // hashed per snapshot; cached hashes don't count
)

// buildContext is one local build: its context folder and Dockerfile.
type buildContext struct {
	Dir        string // absolute
	Dockerfile string // absolute
}

// localBuilds reads every local build context out of a compose file.
// Remote contexts (git URLs) and anything a variable hides are skipped.
func localBuilds(composeText []byte, dir string) []buildContext {
	var doc yaml.Node
	if yaml.Unmarshal(composeText, &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	svcs := mapValue(doc.Content[0], "services")
	if svcs == nil || svcs.Kind != yaml.MappingNode {
		return nil
	}
	var out []buildContext
	for i := 1; i < len(svcs.Content); i += 2 {
		b := mapValue(svcs.Content[i], "build")
		var ctx, df string
		switch {
		case b == nil:
			continue
		case b.Kind == yaml.ScalarNode:
			ctx = b.Value
		case b.Kind == yaml.MappingNode:
			if c := mapValue(b, "context"); c != nil {
				ctx = c.Value
			} else {
				ctx = "."
			}
			if d := mapValue(b, "dockerfile"); d != nil {
				df = d.Value
			}
			if mapValue(b, "dockerfile_inline") != nil {
				df = "" // inline: the Dockerfile is the compose file itself
			}
		}
		if ctx == "" || isRemoteContext(ctx) || strings.Contains(ctx+df, "$") {
			continue
		}
		if !filepath.IsAbs(ctx) {
			ctx = filepath.Join(dir, ctx)
		}
		ctx = filepath.Clean(ctx)
		bc := buildContext{Dir: ctx, Dockerfile: filepath.Join(ctx, "Dockerfile")}
		if df != "" {
			if filepath.IsAbs(df) {
				bc.Dockerfile = filepath.Clean(df)
			} else {
				bc.Dockerfile = filepath.Join(ctx, df)
			}
		}
		if !slices.Contains(out, bc) {
			out = append(out, bc)
		}
	}
	return out
}

// HasBuild reports whether any service in the compose file has build:.
func HasBuild(composeText []byte) bool {
	var doc yaml.Node
	if yaml.Unmarshal(composeText, &doc) != nil || len(doc.Content) == 0 {
		return false
	}
	svcs := mapValue(doc.Content[0], "services")
	if svcs == nil || svcs.Kind != yaml.MappingNode {
		return false
	}
	for i := 1; i < len(svcs.Content); i += 2 {
		if mapValue(svcs.Content[i], "build") != nil {
			return true
		}
	}
	return false
}

// bindSources are the host paths the stack mounts, from the compose file
// as written.
func bindSources(composeText []byte, dir string) []string {
	refs, _ := StaticReferences(composeText, dir)
	var out []string
	for _, r := range refs {
		if r.What == "bind mount" {
			out = append(out, r.Path)
		}
	}
	return out
}

// contextFiles walks every local build context and returns file states for
// the snapshot. partial is true when a cap cut the walk short.
func contextFiles(composeText []byte, dir string) (files []FileState, partial bool, err error) {
	builds := localBuilds(composeText, dir)
	if len(builds) == 0 {
		return nil, false, nil
	}
	binds := bindSources(composeText, dir)
	underBind := func(p string) bool {
		for _, b := range binds {
			if p == b || strings.HasPrefix(p, b+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	var hashed int64
	for _, bc := range builds {
		// The Dockerfile is the recipe, so it is always recorded, content
		// included, even under a bind mount.
		if !seen[bc.Dockerfile] {
			seen[bc.Dockerfile] = true
			st, err := fileState(bc.Dockerfile)
			if err != nil {
				return nil, false, err
			}
			st.Path = displayPath(dir, bc.Dockerfile)
			files = append(files, st)
		}
		pm, err := ignoreMatcher(bc)
		if err != nil {
			return nil, false, err
		}
		fi, err := os.Lstat(bc.Dir)
		if err != nil || !fi.IsDir() {
			continue // missing context: Compose will say so at build time
		}
		walkErr := filepath.WalkDir(bc.Dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == bc.Dir {
				return nil
			}
			if underBind(p) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(bc.Dir, p)
			rel = filepath.ToSlash(rel)
			always := p == bc.Dockerfile || rel == ".dockerignore"
			if pm != nil && !always {
				ignored, err := pm.MatchesOrParentMatches(rel)
				if err != nil {
					return err
				}
				if ignored {
					if d.IsDir() && !pm.Exclusions() {
						return filepath.SkipDir
					}
					return nil
				}
			}
			if d.IsDir() || seen[p] {
				return nil
			}
			seen[p] = true
			if len(files) >= maxContextFiles || hashed >= maxContextBytes {
				partial = true
				return filepath.SkipAll
			}
			st, n, err := hashOnlyState(p)
			if err != nil {
				return err
			}
			hashed += n
			st.Path = displayPath(dir, p)
			files = append(files, st)
			return nil
		})
		if walkErr != nil {
			return nil, false, walkErr
		}
		if partial {
			break
		}
	}
	return files, partial, nil
}

// ignoreMatcher reads <Dockerfile>.dockerignore if it exists, else the
// context's .dockerignore, the same order BuildKit uses.
func ignoreMatcher(bc buildContext) (*patternmatcher.PatternMatcher, error) {
	for _, p := range []string{bc.Dockerfile + ".dockerignore", filepath.Join(bc.Dir, ".dockerignore")} {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		patterns, err := ignorefile.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		return patternmatcher.New(patterns)
	}
	return nil, nil
}

// hashKey identifies a file version cheaply. ctime is in it because a
// container can set a file's mtime back but not its ctime.
type hashKey struct {
	size     int64
	mtime    int64
	ctime    int64
	ino, dev uint64
}

var hashCache sync.Map // path -> cachedHash

type cachedHash struct {
	key  hashKey
	hash string
}

// hashOnlyState hashes one context file without keeping its content.
// Symlinks are recorded by target, not followed, the way the builder
// sends them. n is how many bytes were read, 0 on a cache hit.
func hashOnlyState(p string) (FileState, int64, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return FileState{}, 0, err
	}
	st := FileState{Exists: true, TooLarge: true} // content is never kept for context files
	if fi.Mode()&os.ModeSymlink != 0 {
		tgt, err := os.Readlink(p)
		if err != nil {
			return FileState{}, 0, err
		}
		sum := sha256.Sum256([]byte("symlink:" + tgt))
		st.SHA256 = hex.EncodeToString(sum[:])
		return st, 0, nil
	}
	if !fi.Mode().IsRegular() {
		st.SHA256 = "not a regular file: " + fi.Mode().Type().String()
		return st, 0, nil
	}
	k := hashKey{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		k.ctime = sys.Ctim.Nano()
		k.ino, k.dev = sys.Ino, uint64(sys.Dev)
	}
	if c, ok := hashCache.Load(p); ok && c.(cachedHash).key == k {
		st.SHA256 = c.(cachedHash).hash
		return st, 0, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return FileState{}, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return FileState{}, 0, err
	}
	st.SHA256 = hex.EncodeToString(h.Sum(nil))
	hashCache.Store(p, cachedHash{key: k, hash: st.SHA256})
	return st, n, nil
}

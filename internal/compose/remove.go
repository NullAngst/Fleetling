package compose

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Deletion safety. Removing a stack can delete folders on the host, so
// every path goes through Guard.Check before it is offered, and Guard.Delete
// runs the same checks again right before it deletes. The rules, numbered
// as in the spec:
//
//  1. Only paths under the stack root. Never the root itself.
//  2. Resolve symlinks first. A path that resolves outside the root is
//     refused.
//  3. Never a path that another stack's compose file references, or that
//     another container mounts. Nothing inside such a path, and nothing
//     that contains one, either.
//  4. Never a mount point, and never a folder with a mount point inside it.
//     Checked twice: against the kernel's mount table, and by comparing
//     device IDs, since a bind mount from the same disk keeps its device ID.
//  5. Never follow a symlink while deleting. A symlink is removed as a
//     link; its target is left alone.
//  6. Bind sources outside the root are never offered. The planner leaves
//     them out, and Check refuses them anyway under rule 1.
//  7. Sizes are counted here so the full list shows before anything
//     happens. The server logs every deleted path.
//
// On top of those, Fleetling's own data folder and the folders on the
// ignore list are never deleted.

// Ref is a path that something other than the thing being removed uses.
type Ref struct {
	Path  string // absolute
	Owner string // shown to the user, e.g. "stack gitea (bind mount)"
}

// Candidate is one path checked for deletion.
type Candidate struct {
	Path    string // as offered, cleaned
	Real    string // what would actually be removed: every folder above it resolved, the last part as is
	Exists  bool
	Link    bool // a symlink; only the link would go
	Dir     bool
	Size    int64
	Files   int
	Partial bool   // the size count stopped early; Size and Files are a floor
	Refused string // why it can't be deleted; empty when it can
}

// OK reports whether the path may be deleted.
func (c Candidate) OK() bool { return c.Refused == "" && c.Exists }

// GuardOptions configures a Guard.
type GuardOptions struct {
	Root      string
	Refs      []Ref
	Protected []Ref  // never deleted, and never deleted around
	MountInfo string // mount table to read; SelfMountInfo when empty

	// Size counting stops after this long or this many entries per path.
	CountTime  time.Duration
	CountFiles int

	// DevOf returns a path's device ID without following a final symlink.
	// Tests replace it to fake a mount point.
	DevOf func(path string) (uint64, error)
}

// Guard checks and deletes paths under one stack root.
type Guard struct {
	root      string // as configured, cleaned
	realRoot  string // symlinks resolved
	refs      []refForms
	protected []refForms
	mounts    []string
	devOf     func(string) (uint64, error)
	countTime time.Duration
	countMax  int
}

// refForms keeps a path as written and as resolved, since a stack may
// reach a folder through a symlink and the check has to see both.
type refForms struct {
	Ref
	forms []string
}

// NewGuard resolves the root and reads the mount table. It fails when the
// mount table can't be read, since rule 4 can't be checked without it.
func NewGuard(o GuardOptions) (*Guard, error) {
	root, err := CleanRoot(o.Root)
	if err != nil {
		return nil, err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve the stack root: %w", err)
	}
	mi := o.MountInfo
	if mi == "" {
		mi = SelfMountInfo
	}
	mounts, err := ReadMountPoints(mi)
	if err != nil {
		return nil, fmt.Errorf("read the mount table, so mount points can't be checked: %w", err)
	}
	g := &Guard{root: root, realRoot: realRoot, mounts: mounts, devOf: o.DevOf, countTime: o.CountTime, countMax: o.CountFiles}
	if g.devOf == nil {
		g.devOf = deviceOf
	}
	if g.countTime <= 0 {
		g.countTime = 5 * time.Second
	}
	if g.countMax <= 0 {
		g.countMax = 1_000_000
	}
	for _, r := range o.Refs {
		rf := g.forms(r)
		// A mount of the whole root, or of something above it like
		// node-exporter's "/:/host", covers every path and so tells us
		// nothing about any one of them. Fleetling's own /opt:/opt is one.
		if g.coversRoot(rf) {
			continue
		}
		g.refs = append(g.refs, rf)
	}
	for _, p := range o.Protected {
		g.protected = append(g.protected, g.forms(p))
	}
	return g, nil
}

// Root returns the configured root.
func (g *Guard) Root() string { return g.root }

func (g *Guard) forms(r Ref) refForms {
	p := filepath.Clean(r.Path)
	rf := refForms{Ref: Ref{Path: p, Owner: r.Owner}, forms: []string{p}}
	if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
		rf.forms = append(rf.forms, real)
	} else if err != nil {
		// The path doesn't exist yet. Resolve the part that does, so a
		// missing bind source under a symlinked folder still matches.
		if parent, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			if q := filepath.Join(parent, filepath.Base(p)); q != p {
				rf.forms = append(rf.forms, q)
			}
		}
	}
	return rf
}

func (g *Guard) coversRoot(rf refForms) bool {
	for _, f := range rf.forms {
		for _, root := range []string{g.root, g.realRoot} {
			if f == root || isUnder(root, f) {
				return true
			}
		}
	}
	return false
}

// related reports whether deleting x would touch a, or a sits inside x:
// the same path, one inside the other either way.
func related(a, x string) bool {
	return a == x || isUnder(a, x) || isUnder(x, a)
}

// Check runs every rule against path and counts its size.
func (g *Guard) Check(path string) Candidate {
	c := Candidate{Path: filepath.Clean(path)}
	if !filepath.IsAbs(path) {
		c.Refused = "not an absolute path"
		return c
	}

	// Rules 1 and 6, on the path as written.
	if c.Path == g.root || c.Path == g.realRoot {
		c.Refused = "the stack root itself"
		return c
	}
	if !isUnder(c.Path, g.root) && !isUnder(c.Path, g.realRoot) {
		c.Refused = "outside the stack root " + g.root
		return c
	}

	fi, err := os.Lstat(c.Path)
	if errors.Is(err, fs.ErrNotExist) {
		c.Refused = "already gone"
		return c
	}
	if err != nil {
		c.Refused = err.Error()
		return c
	}
	c.Exists = true
	c.Link = fi.Mode()&os.ModeSymlink != 0
	c.Dir = fi.IsDir()

	// Rule 2. Resolve every folder above the path the way the kernel will
	// when it deletes, and keep the last part as it is: deleting a symlink
	// removes the link, not what it points at.
	parent, err := filepath.EvalSymlinks(filepath.Dir(c.Path))
	if err != nil {
		c.Refused = "can't resolve the folder above it: " + err.Error()
		return c
	}
	c.Real = filepath.Join(parent, filepath.Base(c.Path))
	if c.Real == g.realRoot {
		c.Refused = "resolves to the stack root itself"
		return c
	}
	if !isUnder(c.Real, g.realRoot) {
		c.Refused = "resolves to " + c.Real + ", outside the stack root"
		return c
	}
	if c.Link {
		target, err := filepath.EvalSymlinks(c.Path)
		switch {
		case err != nil:
			c.Refused = "a symlink whose target can't be resolved"
			return c
		case target == g.realRoot:
			c.Refused = "a symlink to the stack root"
			return c
		case !isUnder(target, g.realRoot):
			c.Refused = "a symlink to " + target + ", outside the stack root"
			return c
		}
	}
	forms := []string{c.Path}
	if c.Real != c.Path {
		forms = append(forms, c.Real)
	}

	for _, p := range g.protected {
		if relatedAny(p.forms, forms) {
			c.Refused = p.Owner
			return c
		}
	}

	// Rule 3.
	for _, r := range g.refs {
		if relatedAny(r.forms, forms) {
			c.Refused = "used by " + r.Owner
			if r.Path != c.Path {
				c.Refused += " at " + r.Path
			}
			return c
		}
	}

	// Rule 4, from the mount table.
	for _, m := range g.mounts {
		for _, f := range forms {
			switch {
			case m == f:
				c.Refused = "a mount point"
				return c
			case isUnder(m, f):
				c.Refused = "has a mount point inside it at " + m
				return c
			}
		}
	}
	// Rule 4, from device IDs. A symlink is just a name in its folder.
	if !c.Link {
		d, err1 := g.devOf(c.Real)
		dp, err2 := g.devOf(parent)
		if err := errors.Join(err1, err2); err != nil {
			c.Refused = "can't read its device: " + err.Error()
			return c
		}
		if d != dp {
			c.Refused = "a mount point: it is on a different filesystem from " + parent
			return c
		}
	}

	if !c.Dir || c.Link {
		c.Size, c.Files = fi.Size(), 1
		return c
	}
	if err := g.count(&c); err != nil {
		c.Refused = err.Error()
	}
	return c
}

func relatedAny(as, xs []string) bool {
	for _, a := range as {
		for _, x := range xs {
			if related(a, x) {
				return true
			}
		}
	}
	return false
}

// count adds up a folder's size without following symlinks (rule 5) and
// refuses it if any folder inside is on another device (rule 4 again, for
// a mount the table missed).
func (g *Guard) count(c *Candidate) error {
	top, err := g.devOf(c.Real)
	if err != nil {
		return err
	}
	stop := time.Now().Add(g.countTime)
	errStop := errors.New("stop")
	var mountErr error
	err = filepath.WalkDir(c.Real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable corners still get deleted with everything else;
			// the count just misses them.
			c.Partial = true
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() && p != c.Real {
			if dev, err := g.devOf(p); err == nil && dev != top {
				mountErr = fmt.Errorf("has a mount point inside it at %s", p)
				return errStop
			}
		}
		c.Files++
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				c.Size += fi.Size()
			}
		}
		if c.Files >= g.countMax || c.Files%1024 == 0 && time.Now().After(stop) {
			c.Partial = true
			return errStop
		}
		return nil
	})
	if mountErr != nil {
		return mountErr
	}
	if err != nil && err != errStop {
		return err
	}
	return nil
}

// Delete checks path again and removes it. The removal goes through an
// os.Root opened at the resolved stack root: it never follows a symlink
// inside the tree, removes a final symlink as a link, and refuses outright
// if a folder on the way was swapped for a symlink that leads out of the
// root after the check ran.
func (g *Guard) Delete(path string) (Candidate, error) {
	c := g.Check(path)
	if !c.OK() {
		if c.Refused == "" {
			c.Refused = "already gone"
		}
		return c, errors.New(c.Refused)
	}
	rel, err := filepath.Rel(g.realRoot, c.Real)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return c, fmt.Errorf("%s is not under %s", c.Real, g.realRoot)
	}
	r, err := os.OpenRoot(g.realRoot)
	if err != nil {
		return c, err
	}
	defer r.Close()
	return c, r.RemoveAll(rel)
}

// SplitByRoot separates paths under root from the rest. Bind sources
// outside the root, like /dev/dri or the engine socket, are never offered
// for deletion (rule 6). The root itself counts as outside.
func SplitByRoot(paths []string, root string) (under, outside []string) {
	root = filepath.Clean(root)
	for _, p := range paths {
		if isUnder(filepath.Clean(p), root) {
			under = append(under, p)
		} else {
			outside = append(outside, p)
		}
	}
	slices.Sort(under)
	slices.Sort(outside)
	return under, outside
}

// Nested reports whether p is strictly inside dir.
func Nested(p, dir string) bool { return isUnder(filepath.Clean(p), filepath.Clean(dir)) }

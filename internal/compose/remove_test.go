package compose

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// yard is a temp stack root with an empty mount table, so tests control
// exactly which mount points exist.
type yard struct {
	t       *testing.T
	base    string // holds the root and things outside it
	root    string
	outside string
	mi      string // fake mountinfo file
}

func newYard(t *testing.T) *yard {
	t.Helper()
	base := t.TempDir()
	// Resolve the temp dir up front: on some systems it sits under a
	// symlink, which would make every "as written" path differ.
	base, _ = filepath.EvalSymlinks(base)
	y := &yard{t: t, base: base, root: filepath.Join(base, "opt"), outside: filepath.Join(base, "outside"), mi: filepath.Join(base, "mountinfo")}
	for _, d := range []string{y.root, y.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	y.write("outside/keep.txt", "precious")
	y.mounts()
	return y
}

func (y *yard) path(rel string) string { return filepath.Join(y.base, rel) }

func (y *yard) write(rel, content string) {
	y.t.Helper()
	p := y.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		y.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		y.t.Fatal(err)
	}
}

func (y *yard) mkdir(rel string) string {
	y.t.Helper()
	p := y.path(rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		y.t.Fatal(err)
	}
	return p
}

func (y *yard) symlink(target, rel string) string {
	y.t.Helper()
	p := y.path(rel)
	if err := os.Symlink(target, p); err != nil {
		y.t.Fatal(err)
	}
	return p
}

// mounts writes a mountinfo file listing the given mount points, in the
// kernel's format, spaces escaped.
func (y *yard) mounts(points ...string) {
	y.t.Helper()
	var b strings.Builder
	b.WriteString("22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n")
	for i, p := range points {
		b.WriteString("3" + string(rune('0'+i)) + " 22 0:4" + string(rune('0'+i)) + " / " + strings.ReplaceAll(p, " ", `\040`) + " rw,relatime - nfs nas:/media rw\n")
	}
	if err := os.WriteFile(y.mi, []byte(b.String()), 0o644); err != nil {
		y.t.Fatal(err)
	}
}

func (y *yard) guard(o GuardOptions) *Guard {
	y.t.Helper()
	o.Root = y.root
	if o.MountInfo == "" {
		o.MountInfo = y.mi
	}
	g, err := NewGuard(o)
	if err != nil {
		y.t.Fatal(err)
	}
	return g
}

func (y *yard) exists(rel string) bool {
	_, err := os.Lstat(y.path(rel))
	return err == nil
}

func wantRefused(t *testing.T, c Candidate, contains string) {
	t.Helper()
	if c.OK() {
		t.Fatalf("%s was allowed, want refused (%s)", c.Path, contains)
	}
	if !strings.Contains(c.Refused, contains) {
		t.Fatalf("%s refused with %q, want it to mention %q", c.Path, c.Refused, contains)
	}
}

func wantOK(t *testing.T, c Candidate) {
	t.Helper()
	if !c.OK() {
		t.Fatalf("%s refused: %s", c.Path, c.Refused)
	}
}

// Rule 1: only paths under the root, never the root itself.
func TestRemoveRule1UnderRootOnly(t *testing.T) {
	y := newYard(t)
	y.write("opt/app/data/a", "x")
	g := y.guard(GuardOptions{})

	wantRefused(t, g.Check(y.root), "stack root itself")
	wantRefused(t, g.Check(y.root+"/"), "stack root itself")
	wantRefused(t, g.Check(y.outside), "outside the stack root")
	wantRefused(t, g.Check("/"), "outside the stack root")
	// Cleaning happens before the check, so .. can't climb out.
	wantRefused(t, g.Check(y.root+"/app/../../outside"), "outside the stack root")
	wantRefused(t, g.Check("opt/app/data"), "absolute")
	// A sibling whose name starts with the root's name is not under it.
	y.mkdir("opt-backup")
	wantRefused(t, g.Check(y.path("opt-backup")), "outside the stack root")
	wantOK(t, g.Check(y.path("opt/app/data")))

	if _, err := g.Delete(y.outside); err == nil {
		t.Fatal("Delete removed a path outside the root")
	}
	if !y.exists("outside/keep.txt") {
		t.Fatal("outside file gone")
	}
}

// Rule 2: symlinks are resolved first; anything that resolves outside the
// root is refused.
func TestRemoveRule2SymlinkEscape(t *testing.T) {
	y := newYard(t)
	y.mkdir("opt/app")
	// The path itself is a link out of the root.
	y.symlink(y.outside, "opt/app/data")
	// A folder above the path is a link out of the root.
	y.symlink(y.outside, "opt/sneaky")
	// A dangling link.
	y.symlink(filepath.Join(y.base, "nowhere"), "opt/app/dangling")
	// A link back to the root itself.
	y.symlink(y.root, "opt/app/home")
	g := y.guard(GuardOptions{})

	wantRefused(t, g.Check(y.path("opt/app/data")), "outside the stack root")
	wantRefused(t, g.Check(y.path("opt/sneaky/keep.txt")), "outside the stack root")
	wantRefused(t, g.Check(y.path("opt/app/dangling")), "can't be resolved")
	wantRefused(t, g.Check(y.path("opt/app/home")), "a symlink to the stack root")
	for _, p := range []string{"opt/app/data", "opt/sneaky/keep.txt", "opt/sneaky"} {
		if _, err := g.Delete(y.path(p)); err == nil {
			t.Errorf("Delete(%s) succeeded", p)
		}
	}
	if !y.exists("outside/keep.txt") {
		t.Fatal("a symlink escape deleted a file outside the root")
	}
}

// Rule 2, the other way: a root that is itself reached through a symlink
// still works, and a path written through it is judged by where it lands.
func TestRemoveRule2SymlinkedRoot(t *testing.T) {
	y := newYard(t)
	y.write("opt/app/data/a", "x")
	alias := y.symlink(y.root, "srv")
	g, err := NewGuard(GuardOptions{Root: alias, MountInfo: y.mi})
	if err != nil {
		t.Fatal(err)
	}
	c := g.Check(filepath.Join(alias, "app/data"))
	wantOK(t, c)
	if c.Real != y.path("opt/app/data") {
		t.Errorf("Real = %s", c.Real)
	}
	if _, err := g.Delete(filepath.Join(alias, "app/data")); err != nil {
		t.Fatal(err)
	}
	if y.exists("opt/app/data") {
		t.Fatal("not deleted")
	}
}

// Rule 3: never a path another stack or container uses, nothing inside one
// and nothing containing one.
func TestRemoveRule3References(t *testing.T) {
	y := newYard(t)
	y.mkdir("opt/shared/media")
	y.mkdir("opt/app/cache")
	y.mkdir("opt/app/conf")
	y.mkdir("opt/gitea")
	y.mkdir("opt/real-data")
	y.symlink(y.path("opt/real-data"), "opt/linked")
	g := y.guard(GuardOptions{Refs: []Ref{
		{Path: y.path("opt/shared"), Owner: "stack jellyfin (bind mount)"},
		{Path: y.path("opt/app/conf/app.ini"), Owner: "stack other (env file)"},
		{Path: y.path("opt/gitea"), Owner: "stack folder of gitea"},
		{Path: y.path("opt/linked/sub"), Owner: "container plex"},
		// Mounts of the whole root or above it don't count.
		{Path: y.root, Owner: "container fleetling"},
		{Path: "/", Owner: "container node-exporter"},
	}})

	wantRefused(t, g.Check(y.path("opt/shared")), "stack jellyfin")
	wantRefused(t, g.Check(y.path("opt/shared/media")), "stack jellyfin")
	wantRefused(t, g.Check(y.path("opt/app/conf")), "stack other")
	wantRefused(t, g.Check(y.path("opt/gitea")), "gitea")
	// Reached through a symlink by the other side.
	wantRefused(t, g.Check(y.path("opt/real-data")), "container plex")
	wantOK(t, g.Check(y.path("opt/app/cache")))

	if _, err := g.Delete(y.path("opt/shared/media")); err == nil || !y.exists("opt/shared/media") {
		t.Fatal("deleted a path another stack uses")
	}
}

// Rule 3 also covers what the user can't see: Fleetling's own data folder
// and the ignore list.
func TestRemoveProtectedPaths(t *testing.T) {
	y := newYard(t)
	y.write("opt/fleetling/data/fleetling.db", "db")
	y.mkdir("opt/containerd/bin")
	g := y.guard(GuardOptions{Protected: []Ref{
		{Path: y.path("opt/fleetling/data"), Owner: "holds Fleetling's own data"},
		{Path: y.path("opt/containerd"), Owner: "on the ignore list"},
	}})
	wantRefused(t, g.Check(y.path("opt/fleetling")), "Fleetling's own data")
	wantRefused(t, g.Check(y.path("opt/fleetling/data/fleetling.db")), "Fleetling's own data")
	wantRefused(t, g.Check(y.path("opt/containerd/bin")), "ignore list")
}

// Rule 4: a mount point, or a folder with one inside, is refused, from
// the mount table and from device IDs.
func TestRemoveRule4FakedMountPoint(t *testing.T) {
	y := newYard(t)
	y.mkdir("opt/media/movies")
	y.mkdir("opt/app/nas/photos")
	y.mkdir("opt/my media")
	y.mkdir("opt/app/data/inner")
	y.mkdir("opt/app/disk")
	y.mkdir("opt/app/ok")
	y.mounts(y.path("opt/media"), y.path("opt/app/nas"), y.path("opt/my media"))

	// Device IDs: opt/app/disk is its own filesystem, and opt/app/data has
	// one inside it that the mount table doesn't list.
	devs := map[string]uint64{y.path("opt/app/disk"): 7, y.path("opt/app/data/inner"): 9}
	g := y.guard(GuardOptions{DevOf: func(p string) (uint64, error) {
		if _, err := os.Lstat(p); err != nil {
			return 0, err
		}
		if d, ok := devs[p]; ok {
			return d, nil
		}
		return 1, nil
	}})

	wantRefused(t, g.Check(y.path("opt/media")), "a mount point")
	wantRefused(t, g.Check(y.path("opt/app")), "mount point inside it")
	wantRefused(t, g.Check(y.path("opt/my media")), "a mount point")
	wantRefused(t, g.Check(y.path("opt/app/disk")), "different filesystem")
	wantRefused(t, g.Check(y.path("opt/app/data")), "mount point inside it at "+y.path("opt/app/data/inner"))
	wantOK(t, g.Check(y.path("opt/app/ok")))
	// Inside a mount is fine; it's only the mount point itself.
	wantOK(t, g.Check(y.path("opt/media/movies")))

	if _, err := g.Delete(y.path("opt/media")); err == nil || !y.exists("opt/media/movies") {
		t.Fatal("deleted a mount point")
	}
}

// Rule 4 against a real mount, where the sandbox allows mounting.
func TestRemoveRule4RealMount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount a tmpfs")
	}
	y := newYard(t)
	nas := y.mkdir("opt/nas")
	if out, err := exec.Command("mount", "-t", "tmpfs", "tmpfs", nas).CombinedOutput(); err != nil {
		t.Skipf("can't mount here: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("umount", nas).Run() })
	y.write("opt/nas/precious", "x")
	// The real mount table and real device IDs.
	g, err := NewGuard(GuardOptions{Root: y.root})
	if err != nil {
		t.Fatal(err)
	}
	wantRefused(t, g.Check(nas), "mount point")
	// With the mount table blanked, the device ID check alone still catches it.
	g = y.guard(GuardOptions{})
	wantRefused(t, g.Check(nas), "different filesystem")
	wantRefused(t, g.Check(y.root+"/nas/../nas"), "different filesystem")
	if !y.exists("opt/nas/precious") {
		t.Fatal("file on the mount gone")
	}
}

// Rule 5: deleting never follows a symlink. A link inside the tree is
// removed as a link; a path that is itself a link inside the root loses
// only the link.
func TestRemoveRule5NoFollow(t *testing.T) {
	y := newYard(t)
	y.write("opt/app/data/file", "x")
	y.symlink(y.outside, "opt/app/data/escape")
	y.symlink(filepath.Join(y.outside, "keep.txt"), "opt/app/data/escape-file")
	y.write("opt/other/keep", "x")
	y.symlink(y.path("opt/other"), "opt/app/alias")
	g := y.guard(GuardOptions{})

	c, err := g.Delete(y.path("opt/app/data"))
	if err != nil {
		t.Fatal(err)
	}
	if y.exists("opt/app/data") || !y.exists("outside/keep.txt") {
		t.Fatal("followed a symlink inside the tree")
	}
	if c.Files != 4 || c.Size != 1 {
		t.Errorf("counted %d entries, %d bytes; want 4 entries (folder, file, two links) and 1 byte", c.Files, c.Size)
	}

	c, err = g.Delete(y.path("opt/app/alias"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Link || y.exists("opt/app/alias") || !y.exists("opt/other/keep") {
		t.Fatal("deleting a symlink touched its target")
	}
}

// Rule 5 again: if a folder on the way is swapped for a symlink out of the
// root after the check, the delete itself refuses.
func TestRemoveRule5SwapAfterCheck(t *testing.T) {
	y := newYard(t)
	y.write("opt/app/data/keep.txt", "x")
	g := y.guard(GuardOptions{})
	wantOK(t, g.Check(y.path("opt/app/data")))

	// A container that can write /opt/app replaces data's parent.
	os.Rename(y.path("opt/app"), y.path("opt/app-old"))
	y.symlink(y.outside, "opt/app")
	y.write("outside/data/keep.txt", "precious")
	if _, err := g.Delete(y.path("opt/app/data")); err == nil {
		t.Fatal("deleted through a swapped symlink")
	}
	if !y.exists("outside/data/keep.txt") {
		t.Fatal("outside data gone")
	}
}

// Rule 6: bind sources outside the root are never offered.
func TestRemoveRule6OutsideNeverOffered(t *testing.T) {
	root := "/opt"
	under, outside := SplitByRoot([]string{"/opt/app/data", "/dev/dri", "/var/run/docker.sock", "/opt", "/optical", "/opt/app/../../etc"}, root)
	if !slices.Equal(under, []string{"/opt/app/data"}) {
		t.Errorf("under = %v", under)
	}
	if !slices.Equal(outside, []string{"/dev/dri", "/opt", "/opt/app/../../etc", "/optical", "/var/run/docker.sock"}) {
		t.Errorf("outside = %v", outside)
	}
}

// Rule 7: sizes are counted before anything happens.
func TestRemoveRule7Sizes(t *testing.T) {
	y := newYard(t)
	y.write("opt/app/data/a", "12345")
	y.write("opt/app/data/sub/b", "123")
	y.write("opt/app/file.txt", "1234567")
	g := y.guard(GuardOptions{})
	c := g.Check(y.path("opt/app/data"))
	wantOK(t, c)
	if c.Size != 8 || c.Files != 4 || c.Partial {
		t.Errorf("size %d, entries %d, partial %v", c.Size, c.Files, c.Partial)
	}
	if c := g.Check(y.path("opt/app/file.txt")); c.Size != 7 || c.Dir {
		t.Errorf("file: %+v", c)
	}
	// A huge folder stops counting and says so.
	g = y.guard(GuardOptions{CountFiles: 2})
	if c := g.Check(y.path("opt/app/data")); !c.Partial || !c.OK() {
		t.Errorf("capped count: %+v", c)
	}
	wantRefused(t, g.Check(y.path("opt/app/nothing")), "already gone")
}

func TestNewGuardNeedsMountTable(t *testing.T) {
	y := newYard(t)
	if _, err := NewGuard(GuardOptions{Root: y.root, MountInfo: y.path("missing")}); err == nil {
		t.Fatal("guard built without a mount table")
	}
}

func TestParseMountInfo(t *testing.T) {
	in := "36 35 98:0 /mnt1 /mnt/parent\\040dir rw,noatime master:1 - ext3 /dev/root rw\n" +
		"37 35 98:0 / /opt/back\\134slash rw - ext4 /dev/sdb rw\nshort line\n"
	got, err := parseMountInfo(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"/mnt/parent dir", `/opt/back\slash`}) {
		t.Errorf("got %q", got)
	}
}

func TestConfigReferences(t *testing.T) {
	cfg := `{"services":{
	  "web":{"volumes":[{"type":"bind","source":"/opt/app/conf"},{"type":"volume","source":"data"}],
	         "env_file":[{"path":"/opt/app/web.env","required":true}],
	         "build":{"context":"/opt/app/build","dockerfile":"Dockerfile.web"}},
	  "old":{"env_file":["/opt/app/old.env"],"build":{"context":"https://github.com/x/y.git"}}},
	 "configs":{"c":{"file":"/opt/app/c.conf"}},
	 "secrets":{"s":{"file":"/opt/app/s.txt"},"e":{"environment":"X"}},
	 "volumes":{"data":{"driver_opts":{"type":"none","o":"bind","device":"/opt/app/vol"}}}}`
	refs, err := ConfigReferences([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range refs {
		got = append(got, r.Path+" "+r.What)
	}
	want := []string{
		"/opt/app/build build context", "/opt/app/build/Dockerfile.web Dockerfile", "/opt/app/c.conf config file",
		"/opt/app/conf bind mount", "/opt/app/old.env env file", "/opt/app/s.txt secret file", "/opt/app/vol volume device", "/opt/app/web.env env file",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestStaticReferences(t *testing.T) {
	text := `include:
  - ../shared/base.yaml
services:
  web:
    build: ./build
    env_file: [web.env]
    extends: {file: ../common.yaml, service: base}
    volumes:
      - ./conf:/conf
      - data:/data
      - /opt/media/${SHOW}:/shows
      - /opt/plex-${X}:/x
      - ${CONFIG}/plex:/config
      - ~/stuff:/stuff
      - type: bind
        source: /opt/long
        target: /long
configs:
  c: {file: ./c.conf}
volumes:
  data:
    driver_opts: {type: none, o: bind, device: /opt/app/vol}
`
	refs, unresolved := StaticReferences([]byte(text), "/opt/app")
	var got []string
	for _, r := range refs {
		s := r.Path + " " + r.What
		if r.Partial {
			s += " partial"
		}
		got = append(got, s)
	}
	want := []string{
		"/opt bind mount partial",
		"/opt/app/build build context", "/opt/app/c.conf config file", "/opt/app/conf bind mount",
		"/opt/app/vol volume device", "/opt/app/web.env env file", "/opt/common.yaml extends file",
		"/opt/long bind mount", "/opt/media bind mount partial", "/opt/shared/base.yaml include",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Equal(unresolved, []string{"${CONFIG}/plex"}) {
		t.Errorf("unresolved %q", unresolved)
	}
}

func TestConfigVolumesAndImages(t *testing.T) {
	cfg := []byte(`{"services":{"web":{"image":"nginx:1"},"api":{"build":{"context":"/opt/app"}},"db":{"image":"nginx:1"}},
	 "volumes":{"data":{"name":"app_data"},"cache":{},"shared":{"name":"shared","external":true}}}`)
	vols, err := ConfigVolumes(cfg, "app")
	if err != nil {
		t.Fatal(err)
	}
	want := []ConfigVolume{{Key: "cache", Name: "app_cache"}, {Key: "data", Name: "app_data"}, {Key: "shared", Name: "shared", External: true}}
	if !slices.Equal(vols, want) {
		t.Errorf("volumes %+v", vols)
	}
	imgs, err := ConfigImages(cfg, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(imgs, []string{"app-api", "nginx:1"}) {
		t.Errorf("images %v", imgs)
	}
}

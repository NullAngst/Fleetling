package compose

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func snapFolder(t *testing.T) Folder {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "compose.yaml"), `include:
  - shared.yaml
  - path: [more.yaml, missing.yaml]
services:
  app:
    image: x
    env_file: app.env
    extends:
      file: base.yaml
      service: base
  db:
    image: y
    env_file:
      - db.env
      - path: optional.env
        required: false
      - ${NOPE}/skip.env
`)
	write(t, filepath.Join(dir, ".fleetling.toml"), "project = \"app\"\nengine = \"docker\"\n")
	write(t, filepath.Join(dir, "stack.env"), "A=1\n")
	os.Symlink("stack.env", filepath.Join(dir, ".env"))
	write(t, filepath.Join(dir, "app.env"), "B=2\n")
	write(t, filepath.Join(dir, "db.env"), "C=3\n")
	write(t, filepath.Join(dir, "shared.yaml"), "services: {}\n")
	f, ok := ReadFolder(dir)
	if !ok {
		t.Fatal("not a stack")
	}
	return f
}

func TestSnapshotCoversReferencedFiles(t *testing.T) {
	f := snapFolder(t)
	snap, err := TakeSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	exists := map[string]bool{}
	for _, fs := range snap.Files {
		paths = append(paths, fs.Path)
		exists[fs.Path] = fs.Exists
	}
	for _, want := range []string{"compose.yaml", ".fleetling.toml", ".env", "app.env", "db.env", "optional.env", "shared.yaml", "more.yaml", "missing.yaml", "base.yaml"} {
		if !slices.Contains(paths, want) {
			t.Errorf("snapshot misses %s: %v", want, paths)
		}
	}
	if slices.Contains(paths, "${NOPE}/skip.env") {
		t.Error("interpolated path was taken literally")
	}
	if !exists[".env"] || exists["missing.yaml"] {
		t.Errorf("exists %v", exists)
	}
	for _, fs := range snap.Files {
		if fs.Path == ".env" && fs.Content != "A=1\n" {
			t.Errorf(".env symlink not followed: %q", fs.Content)
		}
	}
}

func TestChanges(t *testing.T) {
	f := snapFolder(t)
	before, _ := TakeSnapshot(f)
	if ch := Changes(before, before); len(ch) != 0 || before.Fingerprint() == "" {
		t.Fatalf("no-op changes %v", ch)
	}
	write(t, filepath.Join(f.Dir, "compose.yaml"), "services:\n  app:\n    image: x\n    privileged: true\n")
	write(t, filepath.Join(f.Dir, "stack.env"), "A=2\n") // through the .env symlink
	after, _ := TakeSnapshot(f)
	ch := Changes(before, after)
	kinds := map[string]string{}
	for _, c := range ch {
		kinds[c.Path] = c.Kind
	}
	if kinds["compose.yaml"] != "changed" || kinds[".env"] != "changed" || kinds["app.env"] != "removed" {
		t.Errorf("changes %v", kinds)
	}
	if before.Fingerprint() == after.Fingerprint() {
		t.Error("fingerprint did not change")
	}
	// A file that appears where none was is a change too.
	write(t, filepath.Join(f.Dir, "missing.yaml"), "x")
	g := f
	write(t, filepath.Join(f.Dir, "compose.yaml"), "include: [missing.yaml]\nservices: {}\n")
	base, _ := TakeSnapshot(g)
	os.Remove(filepath.Join(f.Dir, "missing.yaml"))
	now, _ := TakeSnapshot(g)
	if c := Changes(base, now); len(c) != 1 || c[0].Kind != "removed" {
		t.Errorf("removed include %+v", c)
	}
}

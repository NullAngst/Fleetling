package compose

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func paths(s Snapshot) []string {
	var out []string
	for _, f := range s.Files {
		if f.Exists {
			out = append(out, f.Path)
		}
	}
	slices.Sort(out)
	return out
}

func TestSnapshotCoversBuildContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	writeTree(t, dir, map[string]string{
		"compose.yaml": "services:\n  app:\n    build:\n      context: ./build\n      dockerfile: docker/Dockerfile\n    volumes:\n      - ./build/data:/data\n" +
			"  remote:\n    build: https://github.com/example/repo.git\n",
		"build/docker/Dockerfile":       "FROM scratch\nCOPY main /main\n",
		"build/main":                    "binary",
		"build/.dockerignore":           "node_modules\n*.log\n!keep.log\n",
		"build/node_modules/x/index.js": "ignored",
		"build/debug.log":               "ignored",
		"build/keep.log":                "kept by the ! rule",
		"build/data/app.db":             "app data, changes daily",
		"build/src/a.go":                "package a",
	})
	os.Symlink("src/a.go", filepath.Join(dir, "build", "link"))
	f, ok := ReadFolder(dir)
	if !ok {
		t.Fatal("not a stack")
	}
	snap, err := TakeSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build/.dockerignore", "build/docker/Dockerfile", "build/keep.log", "build/link", "build/main", "build/src/a.go", "compose.yaml"}
	if got := paths(snap); !slices.Equal(got, want) {
		t.Fatalf("covered:\n got %q\nwant %q", got, want)
	}
	if snap.BuildPartial {
		t.Error("marked partial under the caps")
	}
	for _, fs := range snap.Files {
		switch fs.Path {
		case "build/docker/Dockerfile":
			if fs.Content == "" || fs.TooLarge {
				t.Error("Dockerfile content not kept for the diff")
			}
		case "build/main":
			if fs.Content != "" || !fs.TooLarge {
				t.Error("context file content kept")
			}
		}
	}

	// A change in the context is drift; a change in the bind-mounted data
	// or an ignored file is not.
	os.WriteFile(filepath.Join(dir, "build", "data", "app.db"), []byte("new rows"), 0o644)
	os.WriteFile(filepath.Join(dir, "build", "debug.log"), []byte("more"), 0o644)
	again, _ := TakeSnapshot(f)
	if ch := Changes(snap, again); len(ch) != 0 {
		t.Errorf("data or ignored file flagged: %+v", ch)
	}
	os.WriteFile(filepath.Join(dir, "build", "src", "a.go"), []byte("package b"), 0o644)
	again, _ = TakeSnapshot(f)
	if ch := Changes(snap, again); len(ch) != 1 || ch[0].Path != "build/src/a.go" || ch[0].Diffable {
		t.Errorf("source change: %+v", ch)
	}
	// Same size, mtime put back: the cache keys on ctime, so it is caught.
	p := filepath.Join(dir, "build", "main")
	fi, _ := os.Stat(p)
	os.WriteFile(p, []byte("BINARY"), 0o644)
	os.Chtimes(p, fi.ModTime(), fi.ModTime())
	again, _ = TakeSnapshot(f)
	found := false
	for _, ch := range Changes(snap, again) {
		found = found || ch.Path == "build/main"
	}
	if !found {
		t.Error("same-size change with the mtime reset was missed")
	}
	// The symlink is recorded by its target.
	os.Remove(filepath.Join(dir, "build", "link"))
	os.Symlink("/etc/passwd", filepath.Join(dir, "build", "link"))
	again, _ = TakeSnapshot(f)
	found = false
	for _, ch := range Changes(snap, again) {
		found = found || ch.Path == "build/link"
	}
	if !found {
		t.Error("retargeted symlink missed")
	}
}

func TestSnapshotBuildCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "big")
	files := map[string]string{"compose.yaml": "services:\n  app:\n    build: .\n", "Dockerfile": "FROM scratch\n"}
	for i := range maxContextFiles + 10 {
		files[filepath.Join("src", string(rune('a'+i%26)), "f"+itoa(i))] = "x"
	}
	writeTree(t, dir, files)
	f, _ := ReadFolder(dir)
	snap, err := TakeSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.BuildPartial {
		t.Error("cap not reported")
	}
	if len(snap.Files) > maxContextFiles+4 {
		t.Errorf("cap not applied: %d files", len(snap.Files))
	}
}

func itoa(i int) string {
	b := []byte{}
	for {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
		if i == 0 {
			return string(b)
		}
	}
}

func TestHasBuild(t *testing.T) {
	if !HasBuild([]byte("services:\n  a:\n    build: .\n")) || HasBuild([]byte("services:\n  a:\n    image: x\n")) {
		t.Error("HasBuild")
	}
}

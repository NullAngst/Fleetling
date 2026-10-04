package compose

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestValidProject(t *testing.T) {
	for _, ok := range []string{"gitea", "npmplus", "ghost-2", "a", "0day", "my_app"} {
		if err := ValidProject(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Gitea", "-app", "_app", "my app", "app.1", "app/x", "ä", strings.Repeat("a", 129)} {
		if ValidProject(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNormalizeProject(t *testing.T) {
	cases := map[string]string{
		"copyparty":    "copyparty",
		"NPMplus_data": "npmplus_data",
		"My App.v2":    "myappv2",
		"--weird":      "weird",
		"_x-y":         "x-y",
	}
	for in, want := range cases {
		if got := NormalizeProject(in); got != want {
			t.Errorf("NormalizeProject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEnv(t *testing.T) {
	in := "\ufeff# comment\n\nexport A=1\nB = two words # trailing\nC=\"x\\ny \\\"q\\\"\"\nD='lit $X # not a comment'\nE=\nA=override\n"
	vars, err := ParseEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	m := EnvMap(vars)
	want := map[string]string{"A": "override", "B": "two words", "C": "x\ny \"q\"", "D": "lit $X # not a comment", "E": ""}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if vars[0].Line != 3 {
		t.Errorf("first var on line %d, want 3", vars[0].Line)
	}
	for _, bad := range []string{"NOEQUALS\n", "1BAD=x\n", "Q=\"open\n", "S='open\n"} {
		if _, err := ParseEnv(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "copyparty/compose.yaml"), "services:\n  copyparty:\n    image: x\n")
	write(t, filepath.Join(root, "npmplus_data/compose.yaml"), "services:\n  npmplus:\n    image: x\n  crowdsec:\n    image: y\n")
	write(t, filepath.Join(root, "npmplus_data/.fleetling.toml"), "project = \"npmplus\"\nengine = \"docker\"\n")
	write(t, filepath.Join(root, "both/compose.yaml"), "services: {a: {image: x}}\n")
	write(t, filepath.Join(root, "both/docker-compose.yml"), "services: {b: {image: x}}\n")
	write(t, filepath.Join(root, "both/compose.override.yaml"), "services: {}\n")
	write(t, filepath.Join(root, "named/docker-compose.yaml"), "name: custom\nservices: {a: {image: x}}\n")
	write(t, filepath.Join(root, "envnamed/compose.yml"), "services: {a: {image: x}}\n")
	write(t, filepath.Join(root, "envnamed/.env"), "COMPOSE_PROJECT_NAME=fromenv\n")
	write(t, filepath.Join(root, "broken/compose.yaml"), "services: [unclosed\n")
	write(t, filepath.Join(root, "badmeta/compose.yaml"), "services: {a: {image: x}}\n")
	write(t, filepath.Join(root, "badmeta/.fleetling.toml"), "project = \"Bad Name\"\nengine = \"docker\"\n")
	write(t, filepath.Join(root, "metaonly/.fleetling.toml"), "project = \"metaonly\"\nengine = \"podman\"\n")
	write(t, filepath.Join(root, "containerd/compose.yaml"), "services: {a: {image: x}}\n") // ignored
	write(t, filepath.Join(root, ".hidden/compose.yaml"), "services: {a: {image: x}}\n")
	write(t, filepath.Join(root, "notastack/readme.txt"), "hi")
	write(t, filepath.Join(root, "plainfile"), "hi")
	if err := os.Symlink(filepath.Join(root, "copyparty"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}

	folders, err := Discover(root, []string{"containerd"})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Folder{}
	for _, f := range folders {
		byName[f.Name] = f
	}
	var names []string
	for n := range byName {
		names = append(names, n)
	}
	slices.Sort(names)
	want := []string{"badmeta", "both", "broken", "copyparty", "envnamed", "metaonly", "named", "npmplus_data"}
	if !slices.Equal(names, want) {
		t.Fatalf("found %v, want %v", names, want)
	}

	if f := byName["copyparty"]; f.Project != "copyparty" || f.ProjectSource != "folder name" || !slices.Equal(f.Services, []string{"copyparty"}) {
		t.Errorf("copyparty = %+v", f)
	}
	if f := byName["npmplus_data"]; f.Meta == nil || f.Project != "npmplus" || len(f.Services) != 2 {
		t.Errorf("npmplus_data = %+v", f)
	}
	if f := byName["both"]; f.ComposeFile != "compose.yaml" || len(f.Warnings) != 2 {
		t.Errorf("both = %+v", f)
	}
	if f := byName["named"]; f.Project != "custom" || f.ComposeFile != "docker-compose.yaml" {
		t.Errorf("named = %+v", f)
	}
	if f := byName["envnamed"]; f.Project != "fromenv" || f.ProjectSource != ".env" {
		t.Errorf("envnamed = %+v", f)
	}
	if f := byName["broken"]; len(f.Warnings) != 1 || !strings.Contains(f.Warnings[0], "did not parse") {
		t.Errorf("broken = %+v", f)
	}
	if f := byName["badmeta"]; f.Meta != nil || f.Project != "badmeta" || len(f.Warnings) != 1 {
		t.Errorf("badmeta = %+v", f)
	}
	if f := byName["metaonly"]; f.Meta == nil || f.ComposeFile != "" || len(f.Warnings) != 1 {
		t.Errorf("metaonly = %+v", f)
	}
}

func TestReadMetaRejectsUnknownKeysAndEngines(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, MetaFile), "project = \"a\"\nengine = \"docker\"\ntypo = 1\n")
	if _, err := ReadMeta(dir); err == nil {
		t.Error("accepted an unknown key")
	}
	write(t, filepath.Join(dir, MetaFile), "project = \"a\"\nengine = \"lxc\"\n")
	if _, err := ReadMeta(dir); err == nil {
		t.Error("accepted engine lxc")
	}
	write(t, filepath.Join(dir, MetaFile), "project = \"a\"\nengine = \"podman\"\ncreated = 2026-10-04T15:20:00Z\ncreated_paths = [\"/opt/a/hists\"]\n")
	m, err := ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Created.IsZero() || !slices.Equal(m.CreatedPaths, []string{"/opt/a/hists"}) {
		t.Errorf("meta = %+v", m)
	}
}

func find(t *testing.T, stacks []Stack, project string) Stack {
	t.Helper()
	for _, s := range stacks {
		if s.Project == project {
			return s
		}
	}
	t.Fatalf("no stack %q in %+v", project, stacks)
	return Stack{}
}

func TestMerge(t *testing.T) {
	folders := []Folder{
		{Name: "gitea", Dir: "/opt/gitea", Project: "gitea", ProjectSource: "folder name", Services: []string{"db", "gitea"}},
		{Name: "npmplus_data", Dir: "/opt/npmplus_data", Project: "npmplus", Services: []string{"npmplus"},
			Meta: &Meta{Project: "npmplus", Engine: "docker"}},
		{Name: "jellyfin", Dir: "/opt/jellyfin", Project: "jellyfin", Services: []string{"jellyfin"}},
		{Name: "bar", Dir: "/opt/bar", Project: "bar", Services: []string{"web"}},
		{Name: "idle", Dir: "/opt/idle", Project: "idle", Services: []string{"x"}},
		{Name: "pod", Dir: "/opt/pod", Project: "pod", Services: []string{"x"},
			Meta: &Meta{Project: "pod", Engine: "podman"}},
	}
	containers := []Container{
		{Engine: "docker", Project: "gitea", Service: "gitea", WorkingDir: "/opt/gitea", Running: true},
		{Engine: "docker", Project: "gitea", Service: "db", WorkingDir: "/opt/gitea", Running: false},
		{Engine: "docker", Project: "npmplus", Service: "npmplus", WorkingDir: "/data/compose/3", Running: true},
		// Portainer-started stack with a folder of the same name: matched by name.
		{Engine: "docker", Project: "jellyfin", Service: "jellyfin", WorkingDir: "/data/compose/7", Running: true},
		// Started with -p foo from /opt/bar: matched by working dir.
		{Engine: "docker", Project: "foo", Service: "web", WorkingDir: "/opt/bar", Running: true},
		// No folder at all.
		{Engine: "docker", Project: "plex", Service: "plex", WorkingDir: "/data/compose/9", Running: true},
		{Engine: "docker", Project: "plex", Service: "plex", WorkingDir: "/data/compose/9", Running: false}, // scaled, one down
		{Engine: "docker", Project: "", Service: "loose"},
	}
	reachable := map[string]bool{"docker": true, "podman": false}
	stacks := Merge(folders, containers, reachable)

	if s := find(t, stacks, "gitea"); s.Kind != KindOnDisk || s.State != StatePartial || s.Running != 1 || s.Total != 2 || s.Label() != "On disk" {
		t.Errorf("gitea = %+v", s)
	}
	if s := find(t, stacks, "npmplus"); s.Kind != KindManaged || s.State != StateRunning || s.Label() != "Running" || s.Dir != "/opt/npmplus_data" {
		t.Errorf("npmplus = %+v", s)
	}
	if s := find(t, stacks, "jellyfin"); s.Kind != KindOnDisk || s.Running != 1 || len(s.Warnings) != 1 {
		t.Errorf("jellyfin = %+v", s)
	}
	if s := find(t, stacks, "foo"); s.Dir != "/opt/bar" || s.ProjectSource != "running containers" || s.State != StateRunning {
		t.Errorf("foo = %+v", s)
	}
	if s := find(t, stacks, "plex"); s.Kind != KindExternal || s.Dir != "/data/compose/9" || s.Running != 1 || s.Total != 1 || s.Label() != "External" {
		t.Errorf("plex = %+v", s)
	}
	if s := find(t, stacks, "idle"); s.State != StateStopped || s.Engine != "" {
		t.Errorf("idle = %+v", s)
	}
	if s := find(t, stacks, "pod"); s.State != StateUnknown || s.Label() != "Unknown" {
		t.Errorf("pod = %+v", s)
	}
	if len(stacks) != 7 {
		t.Errorf("got %d stacks, want 7: %+v", len(stacks), stacks)
	}
	if !slices.IsSortedFunc(stacks, func(a, b Stack) int { return strings.Compare(a.Project, b.Project) }) {
		t.Error("stacks not sorted by project")
	}
}

func TestMergeDuplicateProject(t *testing.T) {
	folders := []Folder{
		{Name: "a", Dir: "/opt/a", Project: "app", Meta: &Meta{Project: "app", Engine: "docker"}},
		{Name: "b", Dir: "/opt/b", Project: "app", Meta: &Meta{Project: "app", Engine: "docker"}},
	}
	stacks := Merge(folders, []Container{{Engine: "docker", Project: "app", Service: "s", Running: true}}, map[string]bool{"docker": true})
	if len(stacks) != 2 {
		t.Fatalf("got %+v", stacks)
	}
	var warned, running int
	for _, s := range stacks {
		warned += len(s.Warnings)
		running += s.Running
	}
	if warned != 1 || running != 1 {
		t.Errorf("want one warning and one claim, got %+v", stacks)
	}
}

func TestMergeNoEngineIsUnknown(t *testing.T) {
	stacks := Merge([]Folder{{Name: "a", Dir: "/opt/a", Project: "a", Services: []string{"x"}}}, nil, map[string]bool{"docker": false})
	if stacks[0].State != StateUnknown {
		t.Errorf("state = %s, want unknown", stacks[0].State)
	}
}

func TestCleanRoot(t *testing.T) {
	for in, want := range map[string]string{"/opt": "/opt", "/opt/": "/opt", " /containers ": "/containers", "/root/containers": "/root/containers", "/srv/../opt": "/opt"} {
		got, err := CleanRoot(in)
		if err != nil || got != want {
			t.Errorf("CleanRoot(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "opt", "./opt", "/", "/proc", "/proc/1", "/sys/fs", "/dev", "/etc", "/usr/local", "/var/run", "/opt/../"} {
		if _, err := CleanRoot(bad); err == nil {
			t.Errorf("CleanRoot(%q) accepted", bad)
		}
	}
}

func TestValidateRoot(t *testing.T) {
	dir := t.TempDir()
	if got, err := ValidateRoot(dir + "/"); err != nil || got != dir {
		t.Errorf("ValidateRoot(existing) = %q %v", got, err)
	}
	if _, err := ValidateRoot(filepath.Join(dir, "nope")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing folder: %v", err)
	}
	write(t, filepath.Join(dir, "file"), "x")
	if _, err := ValidateRoot(filepath.Join(dir, "file")); err == nil {
		t.Error("accepted a file as the root")
	}
}

func TestCheckRootMount(t *testing.T) {
	sock := Mount{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"}
	cases := []struct {
		name   string
		root   string
		mounts []Mount
		ok     bool
		hint   string
	}{
		{"identical", "/opt", []Mount{sock, {Type: "bind", Source: "/opt", Destination: "/opt"}}, true, ""},
		{"root below mount", "/srv/stacks", []Mount{{Type: "bind", Source: "/srv", Destination: "/srv"}}, true, ""},
		{"not mounted", "/opt", []Mount{sock}, false, "- /opt:/opt"},
		{"different source", "/opt", []Mount{{Type: "bind", Source: "/mnt/stacks", Destination: "/opt"}}, false, "- /opt:/opt"},
		{"named volume", "/opt", []Mount{{Type: "volume", Source: "/var/lib/docker/volumes/x/_data", Destination: "/opt"}}, false, "volume"},
		{"deepest mount wins", "/opt/media", []Mount{
			{Type: "bind", Source: "/opt", Destination: "/opt"},
			{Type: "bind", Source: "/nas", Destination: "/opt/media"},
		}, false, "- /opt/media:/opt/media"},
		{"prefix is not a parent", "/optional", []Mount{{Type: "bind", Source: "/opt", Destination: "/opt"}}, false, "not mounted"},
	}
	for _, c := range cases {
		err := CheckRootMount(c.root, c.mounts)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), c.hint) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.hint)
		}
	}
}

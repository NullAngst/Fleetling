package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestOpenMigratesAndLocksDownFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "fleetling.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("db mode = %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("data folder mode = %v, want 0700", di.Mode().Perm())
	}
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != len(migrations) {
		t.Errorf("user_version = %d, want %d", v, len(migrations))
	}
}

func TestReopenKeepsData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleetling.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "k", "v2"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, ok, err := s.Get(ctx, "k")
	if err != nil || !ok || v != "v2" {
		t.Fatalf("Get = %q %v %v, want v2 true nil", v, ok, err)
	}
	if _, ok, _ := s.Get(ctx, "missing"); ok {
		t.Error("missing key reported as set")
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleetling.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, path); err == nil {
		t.Fatal("opened a database from a newer schema")
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "fleetling.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	st, err := s.LoadSettings(ctx, "/opt")
	if err != nil {
		t.Fatal(err)
	}
	if st.Root != "/opt" || st.DockerHost != DefaultDockerHost || st.PodmanHost != DefaultPodmanHost || !slices.Equal(st.Ignore, []string{"containerd"}) {
		t.Fatalf("defaults = %+v", st)
	}

	st = Settings{Root: "/containers", DockerHost: "", PodmanHost: "/run/podman/podman.sock", Ignore: []string{"containerd", "backups"}}
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSettings(ctx, "/opt")
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != "/containers" || got.DockerHost != "" || !slices.Equal(got.Ignore, st.Ignore) {
		t.Fatalf("after save = %+v, want %+v", got, st)
	}
}

func TestParseList(t *testing.T) {
	got := ParseList(" containerd \r\nbackups,containerd,\n\n tmp ")
	want := []string{"containerd", "backups", "tmp"}
	if !slices.Equal(got, want) {
		t.Errorf("ParseList = %q, want %q", got, want)
	}
}

func TestActionLog(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "fleetling.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := time.Now().Add(-100 * 24 * time.Hour)
	if _, err := s.StartAction(ctx, Action{Started: old, Target: "old", Command: "docker compose up -d"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.StartAction(ctx, Action{Started: time.Now(), Target: "test-stack", Engine: "docker", Command: "docker compose -p test-stack up -d"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListActions(ctx, "test-stack", 10, 0)
	if err != nil || len(list) != 1 || list[0].ExitCode != nil {
		t.Fatalf("running entry: %+v %v", list, err)
	}
	if err := s.FinishAction(ctx, id, time.Now(), 0, "Container test-stack-web-1  Started"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListActions(ctx, "", 10, 0)
	if len(list) != 2 || list[0].Target != "test-stack" || list[0].ExitCode == nil || *list[0].ExitCode != 0 || list[0].Output == "" {
		t.Fatalf("after finish: %+v", list)
	}
	n, err := s.PruneActions(ctx, time.Now().Add(-ActionRetention))
	if err != nil || n != 1 {
		t.Fatalf("prune removed %d, %v", n, err)
	}
}

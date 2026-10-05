package portainer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/NullAngst/Fleetling/internal/compose"
)

// fakePortainer answers like Portainer 2.x, over TLS with a self-signed cert.
func fakePortainer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("X-API-Key") == "ptr_good" || r.Header.Get("Authorization") == "Bearer jwt123"
		j := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
		switch {
		case r.URL.Path == "/api/auth" && r.Method == http.MethodPost:
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			if b["username"] == "admin" && b["password"] == "secret" {
				j(map[string]string{"jwt": "jwt123"})
				return
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			j(map[string]string{"message": "Invalid credentials"})
		case !auth:
			w.WriteHeader(http.StatusUnauthorized)
			j(map[string]string{"message": "Unauthorized"})
		case r.URL.Path == "/api/endpoints":
			j([]map[string]any{{"Id": 1, "Name": "local", "Type": 1, "URL": "unix:///var/run/docker.sock"}})
		case r.URL.Path == "/api/stacks":
			// lowercase "env" pair keys, as Portainer sends them
			w.Write([]byte(`[{"Id":3,"Name":"gitea","Type":2,"Status":1,"EndpointId":1,"EntryPoint":"docker-compose.yml","ProjectPath":"/data/compose/3","Env":[{"name":"TZ","value":"UTC"}],"GitConfig":null},
				{"Id":4,"Name":"swarmy","Type":1,"Status":1,"EndpointId":1},
				{"Id":5,"Name":"wiki","Type":2,"Status":2,"EndpointId":1,"GitConfig":{"URL":"https://github.com/me/wiki.git","ReferenceName":"refs/heads/main"}}]`))
		case r.URL.Path == "/api/stacks/3/file":
			j(map[string]string{"StackFileContent": "services:\n  gitea:\n    image: gitea/gitea:1\n"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConnectAndRead(t *testing.T) {
	srv := fakePortainer(t)
	ctx := context.Background()

	if _, err := Connect(ctx, Options{URL: srv.URL, APIKey: "ptr_good"}); err == nil || !strings.Contains(err.Error(), "Skip TLS verify") {
		t.Errorf("self-signed cert without skip: %v", err)
	}
	if _, err := Connect(ctx, Options{URL: srv.URL, APIKey: "ptr_bad", SkipTLSVerify: true}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("bad token: %v", err)
	}
	if _, err := Connect(ctx, Options{URL: srv.URL, Username: "admin", Password: "wrong", SkipTLSVerify: true}); err == nil {
		t.Error("bad password accepted")
	}
	if _, err := Connect(ctx, Options{URL: "portainer:9443"}); err == nil {
		t.Error("URL without a scheme accepted")
	}

	for _, o := range []Options{{URL: srv.URL + "/", APIKey: "ptr_good", SkipTLSVerify: true}, {URL: srv.URL, Username: "admin", Password: "secret", SkipTLSVerify: true}} {
		c, err := Connect(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		stacks, err := c.Stacks(ctx)
		if err != nil || len(stacks) != 3 {
			t.Fatalf("stacks %v %v", stacks, err)
		}
		g := stacks[0]
		if g.ID != 3 || g.Type != TypeCompose || g.EndpointID != 1 || len(g.Env) != 1 || g.Env[0].Name != "TZ" || g.Env[0].Value != "UTC" || g.ProjectPath != "/data/compose/3" {
			t.Errorf("gitea %+v", g)
		}
		if stacks[2].GitConfig == nil || stacks[2].GitConfig.URL != "https://github.com/me/wiki.git" {
			t.Errorf("git config %+v", stacks[2])
		}
		text, err := c.StackFile(ctx, 3)
		if err != nil || !strings.HasPrefix(text, "services:") {
			t.Errorf("file %q %v", text, err)
		}
	}
}

func TestFolderGuess(t *testing.T) {
	cases := []struct {
		text, want string
		ok         bool
	}{
		{"services:\n  a:\n    volumes:\n      - /opt/npmplus_data/data:/data\n      - /opt/npmplus_data/certs:/certs\n      - /opt/other/x:/x\n", "npmplus_data", true},
		{"services:\n  a:\n    volumes:\n      - type: bind\n        source: /opt/jelly\n        target: /config\n", "jelly", true},
		{"services:\n  a:\n    volumes:\n      - /opt/a/x:/x\n      - /opt/b/y:/y\n", "stack", false},
		{"services:\n  a:\n    volumes:\n      - data:/data\n      - ./conf:/conf\n      - /srv/elsewhere:/e\n", "stack", false},
		{"not: [valid", "stack", false},
	}
	for _, c := range cases {
		got, ok := FolderGuess(c.text, "/opt", "stack")
		if got != c.want || ok != c.ok {
			t.Errorf("FolderGuess = %q %v, want %q %v\n%s", got, ok, c.want, c.ok, c.text)
		}
	}
}

func TestRelativeFixesKeepEveryOtherByte(t *testing.T) {
	text := `# my gitea, keep this comment
services:
  gitea:
    image: gitea/gitea:1
    volumes:
      - ./data:/data   # trailing comment
      - "./conf:/etc/gitea:ro"
      - /etc/localtime:/etc/localtime:ro
      - type: bind
        source: ./custom
        target: /custom
  db:
    volumes:
      - ./db:/var/lib/postgresql/data
`
	mounts := map[string][]compose.Mount{
		"gitea": {
			{Source: "/opt/portainer/compose/3/data", Destination: "/data"},
			{Source: "/opt/portainer/compose/3/conf", Destination: "/etc/gitea"},
			{Source: "/opt/portainer/compose/3/custom", Destination: "/custom"},
		},
	}
	fixes, unresolved := RelativeFixes(text, mounts)
	if len(fixes) != 3 || !slices.Equal(unresolved, []string{"db: ./db"}) {
		t.Fatalf("fixes %+v unresolved %v", fixes, unresolved)
	}
	got, err := ApplyFixes(text, fixes)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(
		"- ./data:/data", "- /opt/portainer/compose/3/data:/data",
		`"./conf:/etc/gitea:ro"`, `"/opt/portainer/compose/3/conf:/etc/gitea:ro"`,
		"source: ./custom", "source: /opt/portainer/compose/3/custom",
	).Replace(text)
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestEnvText(t *testing.T) {
	text, problems := EnvText([]Pair{{"TZ", "UTC"}, {"PASS", "a b#c$d"}, {"BAD", "two\nlines"}})
	if text != "TZ=UTC\nPASS=a b#c$d\n" || len(problems) != 1 {
		t.Errorf("%q %v", text, problems)
	}
}

func TestUsesStackEnv(t *testing.T) {
	for text, want := range map[string]bool{
		"services:\n  a:\n    env_file: stack.env\n":          true,
		"services:\n  a:\n    env_file:\n      - stack.env\n": true,
		"services:\n  a:\n    env_file: ./stack.env\n":        true,
		"services:\n  a:\n    env_file: mystack.env\n":        false,
		"services:\n  a:\n    image: x\n":                     false,
	} {
		if UsesStackEnv(text) != want {
			t.Errorf("%q: want %v", text, want)
		}
	}
}

func TestEnvSuggestions(t *testing.T) {
	got := EnvSuggestions(
		[]string{"PATH=/usr/bin", "TZ=UTC", "DB_PASS=x", "LANG=C.UTF-8", "TZ=UTC"},
		[]string{"PATH=/usr/bin", "LANG=en_US"},
	)
	if !slices.Equal(got, []string{"DB_PASS=x", "LANG=C.UTF-8", "TZ=UTC"}) {
		t.Errorf("%v", got)
	}
	keys := ComposeEnvKeys("services:\n  a:\n    environment:\n      TZ: UTC\n  b:\n    environment:\n      - LANG=C\n")
	if !keys["TZ"] || !keys["LANG"] || keys["DB_PASS"] {
		t.Errorf("%v", keys)
	}
}

package detect

import (
	"strings"
	"testing"
)

func TestDetectLines(t *testing.T) {
	text := `name: media
services:
  app:
    image: localhost/myapp:latest
    userns_mode: keep-id
    group_add:
      - keep-groups
    volumes:
      - ./data:/data:Z,U
      - /run/podman/podman.sock:/var/run/docker.sock
  web:
    image: nginx
    network_mode: pasta
x-podman:
  in_pod: false
`
	r := Detect(text)
	if r.Engine != "podman" {
		t.Fatalf("no suggestion: %+v", r)
	}
	want := map[int]string{4: "image: localhost/myapp:latest", 5: "userns_mode: keep-id", 7: "group_add: keep-groups", 9: "the volume option U", 10: "a bind of /run/podman/podman.sock", 13: "network_mode: pasta", 14: "x-podman"}
	got := map[int]string{}
	for _, s := range r.Signals {
		got[s.Line] = s.What
	}
	for line, w := range want {
		if !strings.HasPrefix(got[line], w) {
			t.Errorf("line %d: got %q, want %q", line, got[line], w)
		}
	}
	if len(r.Signals) != len(want) {
		t.Errorf("signals: %+v", r.Signals)
	}
	if !strings.HasPrefix(r.Why(), "Suggested Podman: line 4 uses image: localhost/myapp:latest (and 6 more") {
		t.Errorf("why: %q", r.Why())
	}
}

func TestDetectPlainDocker(t *testing.T) {
	for _, text := range []string{
		"services:\n  app:\n    image: nginx\n    volumes:\n      - ./a:/a:z\n      - /var/run/docker.sock:/var/run/docker.sock:ro\n    userns_mode: host\n",
		"not: [valid",
		"",
		"- a list\n",
	} {
		if r := Detect(text); r.Engine != "" || r.Why() != "" {
			t.Errorf("%q: %+v", text, r)
		}
	}
}

func TestDetectSingleSignal(t *testing.T) {
	r := Detect("services:\n  a:\n    image: x\n    userns_mode: keep-id:uid=1000\n")
	if r.Why() != "Suggested Podman: line 4 uses userns_mode: keep-id:uid=1000" {
		t.Errorf("why: %q", r.Why())
	}
}

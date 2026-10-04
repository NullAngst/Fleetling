package engine

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strings"
)

// InContainer reports whether this process runs inside a Docker or Podman
// container. Docker creates /.dockerenv; Podman creates /run/.containerenv.
func InContainer() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// SelfIDCandidates returns ways to name this container to the engine API,
// best first. The hostname is the short container ID unless the compose
// file sets hostname:, so the full ID from mountinfo comes next, then
// Podman's own record.
func SelfIDCandidates() []string {
	var out []string
	if h, err := os.Hostname(); err == nil && h != "" {
		out = append(out, h)
	}
	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		if id := idFromMountinfo(f); id != "" {
			out = append(out, id)
		}
		f.Close()
	}
	if b, err := os.ReadFile("/run/.containerenv"); err == nil {
		if id := idFromContainerenv(string(b)); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// Docker bind-mounts /etc/hostname, /etc/hosts and /etc/resolv.conf from
// /var/lib/docker/containers/<64 hex id>/, and that path shows up in
// mountinfo. Podman uses .../overlay-containers/<id>/userdata/.
var mountinfoIDRe = regexp.MustCompile(`/(?:containers|overlay-containers)/([0-9a-f]{64})/`)

func idFromMountinfo(r io.Reader) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "/etc/hostname") && !strings.Contains(line, "/etc/hosts") {
			continue
		}
		if m := mountinfoIDRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// /run/.containerenv holds lines like id="abc..." on rootful Podman.
func idFromContainerenv(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "id="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

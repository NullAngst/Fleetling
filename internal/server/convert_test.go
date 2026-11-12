package server

import (
	"archive/tar"
	"bytes"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/convert"
)

// The converter page against the real docker CLI (needed for compose
// config). podlet and kompose are used when installed, as in the image;
// without them their tabs say so and the rest still works.
func TestConverterPage(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	h := newHarness(t)
	h.s.tools = convert.Tools{}
	b := h.browser()
	h.setUp(b)
	plex, _ := os.ReadFile("../convert/testdata/plex.yaml")
	dir := filepath.Join(h.root, "plex")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), plex, 0o644)
	compose.WriteMeta(dir, &compose.Meta{Project: "plex", Engine: "docker"}, compose.Owner{})
	h.approve(dir)

	page := b.do("GET", "/stacks/plex?tab=convert", nil, nil).Body.String()
	if !strings.Contains(page, `name="stack" value="plex"`) {
		t.Fatal("no Convert tab on the stack page")
	}
	tok := b.token("/convert")
	w := b.do("POST", "/convert", url.Values{"csrf": {tok}, "stack": {"plex"}, "do": {"convert"}}, nil)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("convert: %d %s", w.Code, body)
	}
	for _, want := range []string{"network_mode: host", ">devices<", "needs no conversion", "kompose.service.type"} {
		if !strings.Contains(body, want) {
			t.Errorf("converter page missing %q", want)
		}
	}
	_, podlet := exec.LookPath("podlet")
	_, kompose := exec.LookPath("kompose")
	if podlet != nil && !strings.Contains(body, "podlet isn&#39;t installed here") {
		t.Error("missing podlet not explained")
	}
	if podlet == nil {
		if !strings.Contains(body, "plex-plex.container") || !strings.Contains(body, "sudo systemctl start plex-pod.service") {
			t.Error("quadlet output or install commands missing")
		}
		// Save next to stack writes into <stack>/quadlet/ and logs it.
		w := b.do("POST", "/convert", url.Values{"csrf": {tok}, "stack": {"plex"}, "do": {"save"}, "format": {"quadlet"}}, nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Nothing was installed") {
			t.Fatalf("save: %d %s", w.Code, w.Body.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "quadlet", "plex.pod")); err != nil {
			t.Error("plex.pod not saved")
		}
		if out := b.do("GET", "/actions?target=plex", nil, nil).Body.String(); !strings.Contains(out, filepath.Join(dir, "quadlet")) {
			t.Error("save not in the action log")
		}
	}
	if kompose == nil {
		w := b.do("POST", "/convert", url.Values{"csrf": {tok}, "stack": {"plex"}, "do": {"download"}, "format": {"k8s"}, "service_type": {"nodeport"}}, nil)
		if w.Header().Get("Content-Type") != "application/x-tar" {
			t.Fatalf("download: %d %s", w.Code, w.Body.String())
		}
		tr := tar.NewReader(bytes.NewReader(w.Body.Bytes()))
		n := 0
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			if strings.HasPrefix(h.Name, "plex-k8s/") && strings.HasSuffix(h.Name, ".yaml") {
				n++
			}
		}
		if n == 0 {
			t.Error("tar has no manifests")
		}
	}

	// Pasted text resolves under <root>/<project>, and bad input is refused.
	w = b.do("POST", "/convert", url.Values{"csrf": {tok}, "compose": {"services:\n  a:\n    image: x\n    volumes: [\"./d:/d\"]\n"}, "project": {"pasted"}, "do": {"convert"}}, nil)
	if !strings.Contains(w.Body.String(), filepath.Join(h.root, "pasted", "d")) || !strings.Contains(w.Body.String(), "relative bind mount ./d") {
		t.Errorf("pasted file:\n%s", w.Body.String())
	}
	if w := b.do("POST", "/convert", url.Values{"csrf": {tok}, "compose": {"x"}, "project": {"Bad Name"}}, nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad project name: %d", w.Code)
	}
	if w := b.do("POST", "/convert", url.Values{"csrf": {tok}, "stack": {"../etc"}}, nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad stack: %d", w.Code)
	}
}

// Save next to stack stays inside the stack folder even when the target
// subfolder is a symlink planted by a container.
func TestWriteConfined(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(dir, "quadlet"))
	var names []string
	err := writeConfined(dir, "quadlet", []convert.File{{Name: "x.container", Content: "[Container]\n"}}, compose.Owner{}, &names)
	if err == nil {
		t.Fatal("wrote through a symlinked folder")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("file escaped: %v", entries)
	}
	if err := writeConfined(dir, "k8s", []convert.File{{Name: "a-service.yaml", Content: "kind: Service\n"}}, compose.Owner{}, &names); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "k8s", "a-service.yaml")); string(b) != "kind: Service\n" {
		t.Errorf("content %q", b)
	}
	if err := writeConfined(dir, "k8s", []convert.File{{Name: "../escape.yaml", Content: "x"}}, compose.Owner{}, &names); err == nil {
		t.Error("path in a file name accepted")
	}
}

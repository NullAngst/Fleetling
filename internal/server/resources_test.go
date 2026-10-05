package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/engine/enginetest"
	"github.com/NullAngst/Fleetling/internal/store"
)

func resourceHarness(t *testing.T) (*harness, *browser, *enginetest.Engine) {
	h, b, fe := containerHarness(t)
	fe.Add(&enginetest.Container{ID: strings.Repeat("55", 32), Name: "plex", Image: "plexinc/pms", ImageID: "sha256:plex", Running: true,
		Volumes: []string{"plex_config"}, Networks: []string{"lan"}})
	fe.Networks = []*enginetest.Network{
		{ID: "n1", Name: "bridge", Driver: "bridge"},
		{ID: "n2", Name: "lan", Driver: "macvlan", Subnet: "192.168.1.0/24", Gateway: "192.168.1.1", Options: map[string]string{"parent": "eth0"}},
		{ID: "n3", Name: "gitea_default", Driver: "bridge", Labels: map[string]string{engine.LabelNetwork: "default", engine.LabelProject: "gitea"}},
	}
	fe.Images = []*enginetest.Image{{ID: "sha256:plex", Tags: []string{"plexinc/pms:latest"}, Size: 300 << 20}, {ID: "sha256:" + strings.Repeat("d", 64), Tags: []string{"<none>:<none>"}, Size: 10 << 20}, {ID: "sha256:old", Tags: []string{"nginx:1.25"}, Size: 50 << 20}}
	fe.Volumes = []*enginetest.Volume{{Name: "plex_config"}, {Name: "3f9a0c", Labels: map[string]string{engine.LabelAnonymous: ""}}, {Name: "old_named"}}
	h.s.listIfaces = func(context.Context, store.Settings, string) ([]string, error) {
		return []string{"enp3s0", "eth0"}, nil
	}
	return h, b, fe
}

// The phase 5 done-when, at the API level: a macvlan made in the UI sends
// the same create request `docker network create` does.
func TestMacvlanFromUI(t *testing.T) {
	h, b, fe := resourceHarness(t)
	form := b.do("GET", "/networks/new", nil, nil).Body.String()
	if !strings.Contains(form, "<option >enp3s0</option>") || !strings.Contains(form, `data-show-driver="ipvlan"`) {
		t.Fatalf("form: %s", form)
	}
	w := b.do("POST", "/networks/new", url.Values{
		"csrf": {csrfRe.FindStringSubmatch(form)[1]}, "engine": {"docker"}, "name": {"lan2"}, "driver": {"macvlan"},
		"subnet": {"192.168.1.0/24"}, "gateway": {"192.168.1.1"}, "ip_range": {"192.168.1.192/27"}, "parent": {"eth0"},
		"k_opt": {"", ""}, "v_opt": {"", ""}, "k_label": {""}, "v_label": {""},
	}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var got, want map[string]any
	json.Unmarshal(fe.Created[0], &got)
	json.Unmarshal([]byte(`{"Name":"lan2","Driver":"macvlan","IPAM":{"Driver":"default","Options":{},"Config":[{"Subnet":"192.168.1.0/24","IPRange":"192.168.1.192/27","Gateway":"192.168.1.1"}]},"Internal":false,"Attachable":false,"Ingress":false,"ConfigOnly":false,"Options":{"parent":"eth0"},"Labels":{}}`), &want)
	for k, v := range want {
		gj, _ := json.Marshal(got[k])
		wj, _ := json.Marshal(v)
		if string(gj) != string(wj) {
			t.Errorf("%s: got %s want %s", k, gj, wj)
		}
	}
	acts, _ := h.s.store.ListActions(context.Background(), "network/lan2", 5, 0)
	if len(acts) != 1 || acts[0].Command != "docker network create -d macvlan --subnet 192.168.1.0/24 --gateway 192.168.1.1 --ip-range 192.168.1.192/27 -o parent=eth0 lan2" {
		t.Errorf("log %+v", acts)
	}

	// A bad form comes back with the error and keeps what was typed.
	w = b.do("POST", "/networks/new", url.Values{"csrf": {csrfRe.FindStringSubmatch(form)[1]}, "engine": {"docker"}, "name": {"bad"}, "driver": {"ipvlan"}, "subnet": {"10.0.0.5/24"}, "ipvlan_mode": {"l2"}}, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "did you mean 10.0.0.0/24") || !strings.Contains(w.Body.String(), `value="bad"`) {
		t.Errorf("bad subnet: %d", w.Code)
	}
}

func TestNetworksPage(t *testing.T) {
	_, b, fe := resourceHarness(t)
	page := b.do("GET", "/networks", nil, nil).Body.String()
	for _, want := range []string{"stack gitea", "built in", "parent=eth0", "docker network disconnect lan plex", "belongs to stack gitea"} {
		if !strings.Contains(page, want) {
			t.Errorf("networks page missing %q", want)
		}
	}
	tok := csrfRe.FindStringSubmatch(page)[1]
	if w := b.do("POST", "/networks/docker/n2/remove", url.Values{"csrf": {tok}}, nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "plex") {
		t.Errorf("remove attached network: %d %s", w.Code, w.Body.String())
	}
	if w := b.do("POST", "/networks/docker/n1/remove", url.Values{"csrf": {tok}}, nil); w.Code != http.StatusConflict {
		t.Errorf("remove bridge: %d", w.Code)
	}
	if w := b.do("POST", "/networks/docker/n3/remove", url.Values{"csrf": {tok}}, nil); w.Code != http.StatusSeeOther {
		t.Errorf("remove unused network: %d", w.Code)
	}
	if w := b.do("POST", "/networks/docker/n2/connect", url.Values{"csrf": {tok}, "container": {"gitea"}, "ip": {"192.168.1.50"}, "aliases": {"git, code"}}, nil); w.Code != http.StatusSeeOther {
		t.Errorf("connect: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(string(fe.Connects[0]), `"Aliases":["git","code"]`) {
		t.Errorf("connect body %s", fe.Connects[0])
	}
}

func TestImagesAndVolumes(t *testing.T) {
	h, b, fe := resourceHarness(t)
	page := b.do("GET", "/images", nil, nil).Body.String()
	for _, want := range []string{"plexinc/pms:latest", "300.0 MiB", "&lt;none&gt;", "docker rmi nginx:1.25"} {
		if !strings.Contains(page, want) {
			t.Errorf("images page missing %q", want)
		}
	}
	tok := csrfRe.FindStringSubmatch(page)[1]
	// Prune shows its list first and does nothing until confirmed.
	w := b.do("POST", "/images/docker/prune", url.Values{"csrf": {tok}, "all": {"1"}}, nil)
	if !strings.Contains(w.Body.String(), "nginx:1.25 (50.0 MiB)") || !strings.Contains(w.Body.String(), "&lt;none&gt; dddddddddddd") || strings.Contains(w.Body.String(), "plexinc") {
		t.Errorf("prune preview:\n%s", w.Body.String())
	}
	if len(fe.Calls()) != 0 {
		t.Fatalf("preview pruned: %v", fe.Calls())
	}
	w = b.do("POST", "/images/docker/prune", url.Values{"csrf": {tok}, "all": {"1"}, "confirm": {"yes"}}, nil)
	if w.Code != http.StatusSeeOther || !slices.ContainsFunc(fe.Calls(), func(c string) bool { return strings.HasPrefix(c, "POST images prune") }) {
		t.Errorf("prune: %d %v", w.Code, fe.Calls())
	}

	vols := b.do("GET", "/volumes", nil, nil).Body.String()
	if !strings.Contains(vols, "anonymous") || !strings.Contains(vols, "docker volume rm old_named") || strings.Contains(vols, "docker volume rm plex_config") {
		t.Errorf("volumes page:\n%s", vols)
	}
	w = b.do("POST", "/volumes/docker/prune", url.Values{"csrf": {tok}}, nil)
	if body := w.Body.String(); !strings.Contains(body, "<li>3f9a0c</li>") || strings.Contains(body, "<li>old_named</li>") {
		t.Errorf("anonymous-only prune preview:\n%s", body)
	}

	// Pull streams through a job and lands in the log as docker pull.
	w = b.do("POST", "/images/docker/pull", url.Values{"csrf": {tok}, "ref": {"nginx:latest"}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("pull: %d %s", w.Code, w.Body.String())
	}
	waitJob(t, h, w.Header().Get("Location"))
	if log, _ := os.ReadFile(h.dockerLog); !strings.Contains(string(log), "unix://"+fe.Socket+" pull nginx:latest") {
		t.Errorf("docker log:\n%s", log)
	}
	if w := b.do("POST", "/images/docker/pull", url.Values{"csrf": {tok}, "ref": {"--help"}}, nil); w.Code != http.StatusBadRequest {
		t.Errorf("flag as image name: %d", w.Code)
	}
}

func TestFilterIfaces(t *testing.T) {
	got := filterIfaces([]string{"lo", "docker0", "eth0", "veth12ab", "br-3f2a", "enp3s0", "wlan0"})
	if !slices.Equal(got, []string{"enp3s0", "eth0", "wlan0", "br-3f2a", "docker0"}) {
		t.Errorf("%v", got)
	}
}

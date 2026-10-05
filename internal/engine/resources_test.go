package engine

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine/enginetest"
)

// What `docker network create -d macvlan --subnet 192.168.1.0/24
// --gateway 192.168.1.1 --ip-range 192.168.1.192/27 -o parent=eth0 lan`
// puts in its request body.
const cliMacvlanBody = `{"Name":"lan","Driver":"macvlan","IPAM":{"Driver":"default","Options":{},"Config":[{"Subnet":"192.168.1.0/24","IPRange":"192.168.1.192/27","Gateway":"192.168.1.1"}]},"Internal":false,"Attachable":false,"Options":{"parent":"eth0"},"Labels":{}}`

type createBody struct {
	Name       string
	Driver     string
	EnableIPv6 *bool
	IPAM       struct {
		Driver  string
		Options map[string]string
		Config  []struct{ Subnet, IPRange, Gateway string }
	}
	Internal, Attachable bool
	Options, Labels      map[string]string
}

func TestMacvlanMatchesCLI(t *testing.T) {
	fe, c := fake(t)
	spec := NetworkSpec{Name: "lan", Driver: "macvlan", Subnet: "192.168.1.0/24", Gateway: "192.168.1.1", IPRange: "192.168.1.192/27", Parent: "eth0"}
	if _, _, err := c.CreateNetwork(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	var got, want createBody
	json.Unmarshal(fe.Created[0], &got)
	json.Unmarshal([]byte(cliMacvlanBody), &want)
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Errorf("request differs from the CLI's:\n got %s\nwant %s\nraw %s", gj, wj, fe.Created[0])
	}
	if cli := strings.Join(spec.CLI(), " "); cli != "docker network create -d macvlan --subnet 192.168.1.0/24 --gateway 192.168.1.1 --ip-range 192.168.1.192/27 -o parent=eth0 lan" {
		t.Errorf("CLI line %s", cli)
	}
}

func TestNetworkSpecValidation(t *testing.T) {
	ok := []NetworkSpec{
		{Name: "a"},
		{Name: "iv", Driver: "ipvlan", Parent: "enp3s0", IPvlanMode: "l3", Subnet: "10.9.0.0/24"},
		{Name: "v6", IPv6: true, Subnet: "10.8.0.0/24", Subnet6: "fd00:8::/64", Gateway6: "fd00:8::1"},
		{Name: "o", Driver: "overlay-ish", Options: map[string]string{"com.docker.network.bridge.name": "br-lan"}},
	}
	for _, s := range ok {
		if _, err := s.Build(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	bad := map[string]NetworkSpec{
		"no name":            {},
		"space in name":      {Name: "my net"},
		"host bits":          {Name: "a", Subnet: "192.168.1.5/24"},
		"gateway outside":    {Name: "a", Subnet: "192.168.1.0/24", Gateway: "10.0.0.1"},
		"range outside":      {Name: "a", Subnet: "192.168.1.0/24", IPRange: "192.168.2.0/27"},
		"gateway, no subnet": {Name: "a", Gateway: "10.0.0.1"},
		"parent on bridge":   {Name: "a", Parent: "eth0"},
		"mode on macvlan":    {Name: "a", Driver: "macvlan", IPvlanMode: "l2"},
		"bad mode":           {Name: "a", Driver: "ipvlan", IPvlanMode: "l4"},
		"v6 subnet, no v6":   {Name: "a", Subnet6: "fd00::/64"},
		"v4 in the v6 field": {Name: "a", IPv6: true, Subnet6: "10.0.0.0/24"},
	}
	for why, s := range bad {
		if _, err := s.Build(); err == nil {
			t.Errorf("%s accepted", why)
		}
	}
	o, _ := NetworkSpec{Name: "v6", IPv6: true, Subnet: "10.8.0.0/24", Subnet6: "fd00:8::/64"}.Build()
	if o.EnableIPv6 == nil || !*o.EnableIPv6 || len(o.IPAM.Config) != 2 || o.IPAM.Config[1].Subnet != netip.MustParsePrefix("fd00:8::/64") {
		t.Errorf("v6 build %+v", o)
	}
}

func TestResourcesUsage(t *testing.T) {
	fe, c := fake(t, &enginetest.Container{ID: strings.Repeat("a1", 32), Name: "gitea", Image: "gitea/gitea:1", ImageID: "sha256:aaa", Running: true,
		Volumes: []string{"gitea_data"}, Networks: []string{"gitea_default"}})
	fe.Networks = []*enginetest.Network{{ID: "n1", Name: "gitea_default", Driver: "bridge", Subnet: "172.20.0.0/16", Gateway: "172.20.0.1",
		Labels: map[string]string{LabelNetwork: "default", LabelProject: "gitea"}}, {ID: "n2", Name: "bridge", Driver: "bridge"}}
	fe.Images = []*enginetest.Image{{ID: "sha256:aaa", Tags: []string{"gitea/gitea:1"}, Size: 100}, {ID: "sha256:bbb", Tags: []string{"<none>:<none>"}}}
	fe.Volumes = []*enginetest.Volume{{Name: "gitea_data", Labels: map[string]string{LabelProject: "gitea"}}, {Name: "0123abc", Labels: map[string]string{LabelAnonymous: ""}}}
	ctx := context.Background()

	nets, err := c.Networks(ctx)
	if err != nil || len(nets) != 2 {
		t.Fatalf("%v %v", nets, err)
	}
	if n := nets[1]; n.Name != "gitea_default" || !n.StackOwned || n.Project != "gitea" || !slices.Equal(n.Containers, []string{"gitea"}) || n.Subnets[0] != "172.20.0.0/16" {
		t.Errorf("network %+v", n)
	}
	if !nets[0].BuiltIn {
		t.Error("bridge not marked built in")
	}
	imgs, _ := c.Images(ctx)
	if len(imgs) != 2 || !slices.Equal(imgs[0].UsedBy, []string{"gitea"}) || !imgs[1].Dangling || imgs[0].ShortID() != "aaa" {
		t.Errorf("images %+v", imgs)
	}
	vols, _ := c.Volumes(ctx)
	if len(vols) != 2 || !vols[0].Anonymous || !slices.Equal(vols[1].UsedBy, []string{"gitea"}) {
		t.Errorf("volumes %+v", vols)
	}
	if err := c.ConnectNetwork(ctx, "n1", "gitea", "172.20.0.50", []string{"git"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fe.Connects[0]), `"IPv4Address":"172.20.0.50"`) || !strings.Contains(string(fe.Connects[0]), `"Aliases":["git"]`) {
		t.Errorf("connect body %s", fe.Connects[0])
	}
	c.PruneImages(ctx, true)
	c.PruneVolumes(ctx, false)
	calls := strings.Join(fe.Calls(), "|")
	if !strings.Contains(calls, `images prune {"dangling":{"false":true}}`) || !strings.Contains(calls, "volumes prune") {
		t.Errorf("calls %s", calls)
	}
	_ = compose.Mount{}
}

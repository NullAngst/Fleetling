package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// LabelNetwork marks networks Compose created for a stack.
const LabelNetwork = "com.docker.compose.network"

// usage maps images, volumes and networks to the containers using them,
// from one container list call.
type usage struct {
	images   map[string][]string // image ID -> container names
	volumes  map[string][]string // volume name -> container names
	networks map[string][]string // network name -> container names
}

func (e *Client) usage(ctx context.Context) (usage, error) {
	res, err := e.c.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return usage{}, err
	}
	u := usage{images: map[string][]string{}, volumes: map[string][]string{}, networks: map[string][]string{}}
	for _, c := range res.Items {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		u.images[c.ImageID] = append(u.images[c.ImageID], name)
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name != "" {
				u.volumes[m.Name] = append(u.volumes[m.Name], name)
			}
		}
		if c.NetworkSettings != nil {
			for n := range c.NetworkSettings.Networks {
				u.networks[n] = append(u.networks[n], name)
			}
		}
	}
	for _, m := range []map[string][]string{u.images, u.volumes, u.networks} {
		for k := range m {
			slices.Sort(m[k])
		}
	}
	return u, nil
}

// --- Networks ---

// NetworkRow is one network on the networks page.
type NetworkRow struct {
	ID         string
	Name       string
	Driver     string
	Scope      string
	Subnets    []string
	Gateways   []string
	Internal   bool
	Attachable bool
	IPv6       bool
	Project    string // com.docker.compose.project, for stack-owned networks
	StackOwned bool
	BuiltIn    bool // bridge, host, none: Docker's own
	Options    map[string]string
	Containers []string
}

// Networks lists networks with the containers attached to each.
func (e *Client) Networks(ctx context.Context) ([]NetworkRow, error) {
	if err := e.checkSocket(); err != nil {
		return nil, err
	}
	res, err := e.c.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, err
	}
	u, err := e.usage(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NetworkRow, 0, len(res.Items))
	for _, n := range res.Items {
		r := NetworkRow{
			ID: n.ID, Name: n.Name, Driver: n.Driver, Scope: n.Scope, Internal: n.Internal, Attachable: n.Attachable,
			IPv6: n.EnableIPv6, Options: n.Options, Containers: u.networks[n.Name],
			Project: n.Labels[LabelProject], BuiltIn: n.Name == "bridge" || n.Name == "host" || n.Name == "none" || n.Name == "podman",
		}
		_, r.StackOwned = n.Labels[LabelNetwork]
		for _, c := range n.IPAM.Config {
			if c.Subnet.IsValid() {
				r.Subnets = append(r.Subnets, c.Subnet.String())
			}
			if c.Gateway.IsValid() {
				r.Gateways = append(r.Gateways, c.Gateway.String())
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// NetworkSpec is the create form, field for field with Portainer's.
type NetworkSpec struct {
	Name       string
	Driver     string // bridge, macvlan, ipvlan or anything else
	Subnet     string
	Gateway    string
	IPRange    string
	Parent     string // macvlan and ipvlan: host interface
	IPvlanMode string // l2 or l3
	Internal   bool
	Attachable bool
	IPv6       bool
	Subnet6    string
	Gateway6   string
	Labels     map[string]string
	Options    map[string]string
}

// Build turns the form into the Engine API request, the same request
// `docker network create` sends for the equivalent flags.
func (s NetworkSpec) Build() (client.NetworkCreateOptions, error) {
	if s.Name == "" {
		return client.NetworkCreateOptions{}, errors.New("name is required")
	}
	if strings.ContainsAny(s.Name, " /\\:") {
		return client.NetworkCreateOptions{}, errors.New("name may not contain spaces, slashes or colons")
	}
	driver := strings.TrimSpace(s.Driver)
	if driver == "" {
		driver = "bridge"
	}
	o := client.NetworkCreateOptions{
		Driver:     driver,
		Internal:   s.Internal,
		Attachable: s.Attachable,
		IPAM:       &network.IPAM{Driver: "default", Options: map[string]string{}},
		Options:    map[string]string{},
		Labels:     map[string]string{},
	}
	for k, v := range s.Options {
		o.Options[k] = v
	}
	for k, v := range s.Labels {
		o.Labels[k] = v
	}
	if s.Parent != "" {
		if driver != "macvlan" && driver != "ipvlan" {
			return o, fmt.Errorf("a parent interface only applies to macvlan and ipvlan, not %s", driver)
		}
		o.Options["parent"] = s.Parent
	}
	if s.IPvlanMode != "" {
		if driver != "ipvlan" {
			return o, errors.New("ipvlan mode only applies to the ipvlan driver")
		}
		if s.IPvlanMode != "l2" && s.IPvlanMode != "l3" && s.IPvlanMode != "l3s" {
			return o, fmt.Errorf("ipvlan mode %q: use l2 or l3", s.IPvlanMode)
		}
		o.Options["ipvlan_mode"] = s.IPvlanMode
	}

	v4, err := ipamConfig(s.Subnet, s.Gateway, s.IPRange, false)
	if err != nil {
		return o, err
	}
	if v4 != nil {
		o.IPAM.Config = append(o.IPAM.Config, *v4)
	}
	if s.IPv6 {
		t := true
		o.EnableIPv6 = &t
		v6, err := ipamConfig(s.Subnet6, s.Gateway6, "", true)
		if err != nil {
			return o, err
		}
		if v6 != nil {
			o.IPAM.Config = append(o.IPAM.Config, *v6)
		}
	} else if s.Subnet6 != "" || s.Gateway6 != "" {
		return o, errors.New("tick IPv6 to use an IPv6 subnet")
	}
	return o, nil
}

func ipamConfig(subnet, gateway, ipRange string, v6 bool) (*network.IPAMConfig, error) {
	subnet, gateway, ipRange = strings.TrimSpace(subnet), strings.TrimSpace(gateway), strings.TrimSpace(ipRange)
	if subnet == "" {
		if gateway != "" || ipRange != "" {
			return nil, errors.New("a gateway or IP range needs a subnet")
		}
		return nil, nil
	}
	fam := "IPv4"
	if v6 {
		fam = "IPv6"
	}
	p, err := netip.ParsePrefix(subnet)
	if err != nil || p.Addr().Is6() != v6 {
		return nil, fmt.Errorf("subnet %q is not an %s CIDR like %s", subnet, fam, map[bool]string{false: "192.168.1.0/24", true: "fd00:1::/64"}[v6])
	}
	if p.Masked() != p {
		return nil, fmt.Errorf("subnet %s has host bits set; did you mean %s?", p, p.Masked())
	}
	c := &network.IPAMConfig{Subnet: p}
	if gateway != "" {
		g, err := netip.ParseAddr(gateway)
		if err != nil || !p.Contains(g) {
			return nil, fmt.Errorf("gateway %q is not an address inside %s", gateway, p)
		}
		c.Gateway = g
	}
	if ipRange != "" {
		r, err := netip.ParsePrefix(ipRange)
		if err != nil || r.Masked() != r || !p.Contains(r.Addr()) || r.Bits() < p.Bits() {
			return nil, fmt.Errorf("IP range %q must be a CIDR inside %s", ipRange, p)
		}
		c.IPRange = r
	}
	return c, nil
}

// CLI is the equivalent `docker network create` command line, shown under
// the confirm and written to the action log.
func (s NetworkSpec) CLI() []string {
	driver := strings.TrimSpace(s.Driver)
	if driver == "" {
		driver = "bridge"
	}
	a := []string{"docker", "network", "create", "-d", driver}
	if s.Subnet != "" {
		a = append(a, "--subnet", s.Subnet)
	}
	if s.Gateway != "" {
		a = append(a, "--gateway", s.Gateway)
	}
	if s.IPRange != "" {
		a = append(a, "--ip-range", s.IPRange)
	}
	if s.IPv6 {
		a = append(a, "--ipv6")
		if s.Subnet6 != "" {
			a = append(a, "--subnet", s.Subnet6)
		}
		if s.Gateway6 != "" {
			a = append(a, "--gateway", s.Gateway6)
		}
	}
	if s.Parent != "" {
		a = append(a, "-o", "parent="+s.Parent)
	}
	if s.IPvlanMode != "" {
		a = append(a, "-o", "ipvlan_mode="+s.IPvlanMode)
	}
	for _, k := range sortedMapKeys(s.Options) {
		a = append(a, "-o", k+"="+s.Options[k])
	}
	for _, k := range sortedMapKeys(s.Labels) {
		a = append(a, "--label", k+"="+s.Labels[k])
	}
	if s.Internal {
		a = append(a, "--internal")
	}
	if s.Attachable {
		a = append(a, "--attachable")
	}
	return append(a, s.Name)
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// CreateNetwork creates a network from the form.
func (e *Client) CreateNetwork(ctx context.Context, s NetworkSpec) (string, []string, error) {
	o, err := s.Build()
	if err != nil {
		return "", nil, err
	}
	res, err := e.c.NetworkCreate(ctx, s.Name, o)
	if err != nil {
		return "", nil, err
	}
	return res.ID, res.Warning, nil
}

// InspectNetwork returns the raw network as the engine sees it.
func (e *Client) InspectNetwork(ctx context.Context, id string) (network.Inspect, error) {
	res, err := e.c.NetworkInspect(ctx, id, client.NetworkInspectOptions{})
	return res.Network, err
}

// RemoveNetwork removes a network.
func (e *Client) RemoveNetwork(ctx context.Context, id string) error {
	_, err := e.c.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
	return err
}

// ConnectNetwork attaches a container, optionally with a static IPv4 and aliases.
func (e *Client) ConnectNetwork(ctx context.Context, netID, ctr, ip string, aliases []string) error {
	es := &network.EndpointSettings{Aliases: aliases}
	if ip != "" {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			return fmt.Errorf("static IP %q is not an address", ip)
		}
		if a.Is6() {
			es.IPAMConfig = &network.EndpointIPAMConfig{IPv6Address: a}
		} else {
			es.IPAMConfig = &network.EndpointIPAMConfig{IPv4Address: a}
		}
	}
	_, err := e.c.NetworkConnect(ctx, netID, client.NetworkConnectOptions{Container: ctr, EndpointConfig: es})
	return err
}

// DisconnectNetwork detaches a container.
func (e *Client) DisconnectNetwork(ctx context.Context, netID, ctr string) error {
	_, err := e.c.NetworkDisconnect(ctx, netID, client.NetworkDisconnectOptions{Container: ctr})
	return err
}

// --- Images ---

// ImageRow is one image on the images page.
type ImageRow struct {
	ID       string
	Tags     []string
	Size     int64
	Created  time.Time
	UsedBy   []string
	Dangling bool
}

// ShortID is the 12 characters after sha256:.
func (r ImageRow) ShortID() string { return shortID(strings.TrimPrefix(r.ID, "sha256:")) }

// Images lists images with the containers using each.
func (e *Client) Images(ctx context.Context) ([]ImageRow, error) {
	if err := e.checkSocket(); err != nil {
		return nil, err
	}
	res, err := e.c.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return nil, err
	}
	u, err := e.usage(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ImageRow, 0, len(res.Items))
	for _, im := range res.Items {
		r := ImageRow{ID: im.ID, Size: im.Size, Created: time.Unix(im.Created, 0), UsedBy: u.images[im.ID]}
		for _, t := range im.RepoTags {
			if t != "<none>:<none>" {
				r.Tags = append(r.Tags, t)
			}
		}
		r.Dangling = len(r.Tags) == 0
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.Join(out[i].Tags, ","), strings.Join(out[j].Tags, ",")
		if (a == "") != (b == "") {
			return b == "" // tagged first
		}
		return a < b
	})
	return out, nil
}

// RemoveImage removes one image. It refuses images a container uses, like
// `docker rmi` without -f.
func (e *Client) RemoveImage(ctx context.Context, ref string) error {
	_, err := e.c.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true})
	return err
}

// PruneImages removes dangling images, or every unused image with all.
func (e *Client) PruneImages(ctx context.Context, all bool) (int, uint64, error) {
	f := make(client.Filters)
	if all {
		f = f.Add("dangling", "false")
	}
	res, err := e.c.ImagePrune(ctx, client.ImagePruneOptions{Filters: f})
	if err != nil {
		return 0, 0, err
	}
	return len(res.Report.ImagesDeleted), res.Report.SpaceReclaimed, nil
}

// --- Volumes ---

// LabelAnonymous marks volumes Docker created without a name.
const LabelAnonymous = "com.docker.volume.anonymous"

// VolumeRow is one volume on the volumes page.
type VolumeRow struct {
	Name       string
	Driver     string
	Mountpoint string
	Created    string
	Project    string
	Anonymous  bool
	UsedBy     []string
}

// Volumes lists volumes with the containers using each.
func (e *Client) Volumes(ctx context.Context) ([]VolumeRow, error) {
	if err := e.checkSocket(); err != nil {
		return nil, err
	}
	res, err := e.c.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return nil, err
	}
	u, err := e.usage(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]VolumeRow, 0, len(res.Items))
	for _, v := range res.Items {
		_, anon := v.Labels[LabelAnonymous]
		out = append(out, VolumeRow{Name: v.Name, Driver: v.Driver, Mountpoint: v.Mountpoint, Created: v.CreatedAt,
			Project: v.Labels[LabelProject], Anonymous: anon, UsedBy: u.volumes[v.Name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RemoveVolume removes one volume. Docker refuses one that is in use.
func (e *Client) RemoveVolume(ctx context.Context, name string) error {
	_, err := e.c.VolumeRemove(ctx, name, client.VolumeRemoveOptions{})
	return err
}

// PruneVolumes removes unused anonymous volumes, or every unused volume
// with all, matching `docker volume prune` and `docker volume prune -a`.
func (e *Client) PruneVolumes(ctx context.Context, all bool) (int, uint64, error) {
	res, err := e.c.VolumePrune(ctx, client.VolumePruneOptions{All: all})
	if err != nil {
		return 0, 0, err
	}
	return len(res.Report.VolumesDeleted), res.Report.SpaceReclaimed, nil
}

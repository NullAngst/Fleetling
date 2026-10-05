package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/NullAngst/Fleetling/internal/compose"
	"github.com/NullAngst/Fleetling/internal/engine"
	"github.com/NullAngst/Fleetling/internal/store"
)

// slotFromQuery picks the engine for a list page: ?engine=, else the first
// configured one.
func (s *Server) slotFromQuery(r *http.Request) (store.Settings, []string, string, error) {
	st, err := s.store.LoadSettings(r.Context(), s.cfg.DefaultRoot)
	if err != nil {
		return st, nil, "", err
	}
	slots := engineSlots(st)
	slot := r.URL.Query().Get("engine")
	if r.Method == http.MethodPost {
		slot = r.PostFormValue("engine")
	}
	if !slices.Contains(slots, slot) && len(slots) > 0 {
		slot = slots[0]
	}
	return st, slots, slot, nil
}

type listPage struct {
	Slots []string
	Slot  string
	Err   string
	JobID string
	Rows  any
	Extra any
}

func (s *Server) listData(r *http.Request, load func(ctx context.Context, e Engine) (any, error)) listPage {
	_, slots, slot, err := s.slotFromQuery(r)
	d := listPage{Slots: slots, Slot: slot, JobID: r.URL.Query().Get("job")}
	if err == nil && slot != "" {
		var e Engine
		e, _, err = s.engineFor(r.Context(), slot)
		if err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			d.Rows, err = load(ctx, e)
			cancel()
			e.Close()
		}
	}
	if err != nil {
		d.Err = err.Error()
	}
	if d.JobID != "" && s.jobs.Get(d.JobID) == nil {
		d.JobID = ""
	}
	return d
}

// apiAction runs one Engine API call for a resource page and records it
// in the action log under its CLI equivalent.
func (s *Server) apiAction(r *http.Request, slot, target, cli string, fn func(ctx context.Context, e Engine) error) error {
	e, _, err := s.engineFor(r.Context(), slot)
	if err != nil {
		return err
	}
	defer e.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	err = fn(ctx, e)
	s.logFileAction(r.Context(), target, slot, cli, err)
	return err
}

func back(w http.ResponseWriter, r *http.Request, page, slot, notice string) {
	http.Redirect(w, r, page+"?engine="+slot+"&notice="+notice, http.StatusSeeOther)
}

var notices = map[string]string{
	"created": "Network created.", "removed": "Removed.", "connected": "Connected.", "disconnected": "Disconnected.", "pruned": "Pruned.",
}

func (s *Server) listPage(w http.ResponseWriter, r *http.Request, tmpl, title, active string, d listPage) {
	p := s.page(r, title, active, d)
	p.Notice = notices[r.URL.Query().Get("notice")]
	s.render(w, http.StatusOK, tmpl, p)
}

// --- Networks ---

type networksExtra struct {
	Containers []engine.ContainerRow
}

func (s *Server) networksPage(w http.ResponseWriter, r *http.Request) {
	var ctrs []engine.ContainerRow
	d := s.listData(r, func(ctx context.Context, e Engine) (any, error) {
		ctrs, _ = e.Containers(ctx)
		return e.Networks(ctx)
	})
	d.Extra = networksExtra{Containers: ctrs}
	s.listPage(w, r, "networks", "Networks", "networks", d)
}

type networkFormData struct {
	Slots   []string
	Slot    string
	Spec    engine.NetworkSpec
	Ifaces  []string
	IfErr   string
	Options [][2]string
	Labels  [][2]string
}

// hostInterfaces lists the host's network interfaces. A container sees only
// its own, so this runs a throwaway container from Fleetling's own image
// with --network host and reads /sys/class/net there. Outside a container
// it reads /sys/class/net directly.
func (s *Server) hostInterfaces(ctx context.Context, st store.Settings, slot string) ([]string, error) {
	var names []string
	if !s.inContainer() {
		entries, err := os.ReadDir("/sys/class/net")
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return filterIfaces(names), nil
	}
	e, _, err := s.engineFor(ctx, slot)
	if err != nil {
		return nil, err
	}
	image := ""
	for _, id := range s.selfIDs() {
		if d, err := e.Inspect(ctx, id); err == nil {
			image = d.Image
			break
		}
	}
	e.Close()
	if image == "" {
		return nil, errors.New("could not find Fleetling's own image to run the interface lister")
	}
	host, _, err := engine.NormalizeHost(hostFor(st, slot))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := s.runner.Plain(ctx, host, slot == compose.EnginePodman,
		"run", "--rm", "--network", "host", "--entrypoint", "/usr/local/bin/fleetling", image, "ifaces")
	out, err := cmd.Output()
	if err != nil {
		var msg string
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			msg = string(ee.Stderr)
		}
		return nil, fmt.Errorf("listing host interfaces: %v %s", err, strings.TrimSpace(msg))
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if n := strings.TrimSpace(sc.Text()); n != "" {
			names = append(names, n)
		}
	}
	return filterIfaces(names), nil
}

// filterIfaces drops loopback and per-container veth pairs, which are never
// a sensible parent, and sorts physical-looking names first.
func filterIfaces(names []string) []string {
	var out []string
	for _, n := range names {
		if n == "lo" || strings.HasPrefix(n, "veth") {
			continue
		}
		out = append(out, n)
	}
	virtual := func(n string) bool {
		return strings.HasPrefix(n, "docker") || strings.HasPrefix(n, "br-") || strings.HasPrefix(n, "podman") || strings.HasPrefix(n, "cni")
	}
	slices.SortStableFunc(out, func(a, b string) int {
		if virtual(a) != virtual(b) {
			if virtual(a) {
				return 1
			}
			return -1
		}
		return strings.Compare(a, b)
	})
	return out
}

func (s *Server) networkForm(w http.ResponseWriter, r *http.Request) {
	st, slots, slot, err := s.slotFromQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := networkFormData{Slots: slots, Slot: slot, Spec: engine.NetworkSpec{Driver: "bridge"}, Options: make([][2]string, 3), Labels: make([][2]string, 2)}
	if slot != "" {
		d.Ifaces, err = s.listIfaces(r.Context(), st, slot)
		if err != nil {
			d.IfErr = err.Error()
		}
	}
	s.render(w, http.StatusOK, "network_new", s.page(r, "New network", "networks", d))
}

// pairs reads key/value rows posted as k_name[] and v_name[].
func pairs(r *http.Request, name string) (map[string]string, [][2]string, error) {
	ks, vs := r.PostForm["k_"+name], r.PostForm["v_"+name]
	m := map[string]string{}
	var rows [][2]string
	for i, k := range ks {
		k = strings.TrimSpace(k)
		v := ""
		if i < len(vs) {
			v = strings.TrimSpace(vs[i])
		}
		rows = append(rows, [2]string{k, v})
		if k == "" {
			if v != "" {
				return nil, rows, fmt.Errorf("%s value %q has no key", name, v)
			}
			continue
		}
		m[k] = v
	}
	return m, rows, nil
}

func (s *Server) networkCreate(w http.ResponseWriter, r *http.Request) {
	st, slots, slot, err := s.slotFromQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	driver := r.PostFormValue("driver")
	if driver == "other" {
		driver = strings.TrimSpace(r.PostFormValue("driver_other"))
	}
	parent := strings.TrimSpace(r.PostFormValue("parent_other"))
	if parent == "" {
		parent = r.PostFormValue("parent")
	}
	spec := engine.NetworkSpec{
		Name: strings.TrimSpace(r.PostFormValue("name")), Driver: driver,
		Subnet: r.PostFormValue("subnet"), Gateway: r.PostFormValue("gateway"), IPRange: r.PostFormValue("ip_range"),
		Internal: r.PostFormValue("internal") == "1", Attachable: r.PostFormValue("attachable") == "1",
		IPv6: r.PostFormValue("ipv6") == "1", Subnet6: r.PostFormValue("subnet6"), Gateway6: r.PostFormValue("gateway6"),
	}
	if driver == "macvlan" || driver == "ipvlan" {
		spec.Parent = parent
	}
	if driver == "ipvlan" {
		spec.IPvlanMode = r.PostFormValue("ipvlan_mode")
	}
	r.ParseForm()
	var optRows, labelRows [][2]string
	spec.Options, optRows, err = pairs(r, "opt")
	if err == nil {
		spec.Labels, labelRows, err = pairs(r, "label")
	}
	fail := func(msg string) {
		ifs, _ := s.listIfaces(r.Context(), st, slot)
		d := networkFormData{Slots: slots, Slot: slot, Spec: spec, Ifaces: ifs, Options: append(optRows, [2]string{}), Labels: append(labelRows, [2]string{})}
		p := s.page(r, "New network", "networks", d)
		p.Error = msg
		s.render(w, http.StatusBadRequest, "network_new", p)
	}
	if err != nil {
		fail(err.Error())
		return
	}
	if _, err := spec.Build(); err != nil {
		fail(err.Error())
		return
	}
	var warnings []string
	err = s.apiAction(r, slot, "network/"+spec.Name, compose.Quote(spec.CLI()), func(ctx context.Context, e Engine) error {
		var err error
		_, warnings, err = e.CreateNetwork(ctx, spec)
		return err
	})
	if err != nil {
		fail(err.Error())
		return
	}
	if len(warnings) > 0 && warnings[0] != "" {
		s.log.Warn("network create", "name", spec.Name, "warnings", warnings)
	}
	back(w, r, "/networks", slot, "created")
}

func (s *Server) findNetwork(r *http.Request, slot, id string) (engine.NetworkRow, error) {
	e, _, err := s.engineFor(r.Context(), slot)
	if err != nil {
		return engine.NetworkRow{}, err
	}
	defer e.Close()
	nets, err := e.Networks(r.Context())
	if err != nil {
		return engine.NetworkRow{}, err
	}
	for _, n := range nets {
		if n.ID == id || n.Name == id {
			return n, nil
		}
	}
	return engine.NetworkRow{}, errors.New("no such network")
}

func (s *Server) networkRemove(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	n, err := s.findNetwork(r, slot, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if n.BuiltIn {
		http.Error(w, n.Name+" is one of the engine's built-in networks", http.StatusConflict)
		return
	}
	if len(n.Containers) > 0 {
		http.Error(w, "refusing to remove "+n.Name+" while containers are attached: "+strings.Join(n.Containers, ", "), http.StatusConflict)
		return
	}
	err = s.apiAction(r, slot, "network/"+n.Name, compose.Quote([]string{"docker", "network", "rm", n.Name}), func(ctx context.Context, e Engine) error {
		return e.RemoveNetwork(ctx, n.ID)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	back(w, r, "/networks", slot, "removed")
}

func (s *Server) networkConnect(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	n, err := s.findNetwork(r, slot, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	ctr := strings.TrimSpace(r.PostFormValue("container"))
	ip := strings.TrimSpace(r.PostFormValue("ip"))
	var aliases []string
	for a := range strings.SplitSeq(r.PostFormValue("aliases"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			aliases = append(aliases, a)
		}
	}
	cli := []string{"docker", "network", "connect"}
	if ip != "" {
		cli = append(cli, "--ip", ip)
	}
	for _, a := range aliases {
		cli = append(cli, "--alias", a)
	}
	cli = append(cli, n.Name, ctr)
	err = s.apiAction(r, slot, "network/"+n.Name, compose.Quote(cli), func(ctx context.Context, e Engine) error {
		return e.ConnectNetwork(ctx, n.ID, ctr, ip, aliases)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	back(w, r, "/networks", slot, "connected")
}

func (s *Server) networkDisconnect(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	n, err := s.findNetwork(r, slot, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	ctr := r.PostFormValue("container")
	if s.isSelfContainer(ctr) {
		http.Error(w, "refusing to disconnect Fleetling's own container", http.StatusConflict)
		return
	}
	err = s.apiAction(r, slot, "network/"+n.Name, compose.Quote([]string{"docker", "network", "disconnect", n.Name, ctr}), func(ctx context.Context, e Engine) error {
		return e.DisconnectNetwork(ctx, n.ID, ctr)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	back(w, r, "/networks", slot, "disconnected")
}

// --- Images ---

func (s *Server) imagesPage(w http.ResponseWriter, r *http.Request) {
	d := s.listData(r, func(ctx context.Context, e Engine) (any, error) { return e.Images(ctx) })
	s.listPage(w, r, "images", "Images", "images", d)
}

var imageRefRe = strings.NewReplacer(" ", "", "\t", "")

// imagePull runs `docker pull` as a job so the progress streams live.
func (s *Server) imagePull(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slot := r.PathValue("slot")
	st, err := s.store.LoadSettings(ctx, s.cfg.DefaultRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ref := imageRefRe.Replace(r.PostFormValue("ref"))
	if ref == "" || strings.HasPrefix(ref, "-") {
		http.Error(w, "give an image name like nginx:latest", http.StatusBadRequest)
		return
	}
	host, _, err := engine.NormalizeHost(hostFor(st, slot))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	argv := []string{"docker", "pull", ref}
	job, err := s.runCLIJob(ctx, "image/"+ref, slot, argv, s.runner.Plain(s.base, host, slot == compose.EnginePodman, argv[1:]...))
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/images?engine="+slot+"&job="+job.ID, http.StatusSeeOther)
}

// runCLIJob runs one docker command as a background job, logged like a
// stack action.
func (s *Server) runCLIJob(ctx context.Context, target, slot string, argv []string, c *exec.Cmd) (*compose.Job, error) {
	id, err := s.store.StartAction(ctx, store.Action{Started: s.now(), Target: target, Engine: slot, Command: compose.Quote(argv)})
	if err != nil {
		return nil, err
	}
	job, err := s.jobs.Start(compose.JobSpec{Target: target, Engine: slot, Steps: []compose.Step{{Cmd: c}},
		Done: func(j *compose.Job) {
			_, exit, fin := j.Result()
			s.store.FinishAction(context.Background(), id, fin, exit, strings.Join(j.Tail(200), "\n"))
		}})
	if err != nil {
		s.store.FinishAction(ctx, id, s.now(), -1, err.Error())
	}
	return job, err
}

func (s *Server) imageRemove(w http.ResponseWriter, r *http.Request) {
	slot, ref := r.PathValue("slot"), r.PostFormValue("ref")
	err := s.apiAction(r, slot, "image/"+ref, compose.Quote([]string{"docker", "rmi", ref}), func(ctx context.Context, e Engine) error {
		return e.RemoveImage(ctx, ref)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	back(w, r, "/images", slot, "removed")
}

type pruneData struct {
	Kind  string // images or volumes
	Slot  string
	All   bool
	Items []string
	CLI   string
}

// imagePrune shows what would go first (confirm empty), then prunes.
func (s *Server) imagePrune(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	all := r.PostFormValue("all") == "1"
	cli := []string{"docker", "image", "prune", "-f"}
	if all {
		cli = append(cli, "-a")
	}
	if r.PostFormValue("confirm") != "yes" {
		var items []string
		e, _, err := s.engineFor(r.Context(), slot)
		if err == nil {
			var imgs []engine.ImageRow
			imgs, err = e.Images(r.Context())
			e.Close()
			for _, im := range imgs {
				if len(im.UsedBy) == 0 && (all || im.Dangling) {
					name := strings.Join(im.Tags, ", ")
					if name == "" {
						name = "<none> " + im.ShortID()
					}
					items = append(items, fmt.Sprintf("%s (%s)", name, humanBytes(uint64(max(im.Size, 0)))))
				}
			}
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.render(w, http.StatusOK, "prune", s.page(r, "Prune images", "images", pruneData{Kind: "images", Slot: slot, All: all, Items: items, CLI: compose.Quote(cli)}))
		return
	}
	err := s.apiAction(r, slot, "images", compose.Quote(cli), func(ctx context.Context, e Engine) error {
		_, _, err := e.PruneImages(ctx, all)
		return err
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	back(w, r, "/images", slot, "pruned")
}

// --- Volumes ---

func (s *Server) volumesPage(w http.ResponseWriter, r *http.Request) {
	d := s.listData(r, func(ctx context.Context, e Engine) (any, error) { return e.Volumes(ctx) })
	s.listPage(w, r, "volumes", "Volumes", "volumes", d)
}

func (s *Server) volumeRemove(w http.ResponseWriter, r *http.Request) {
	slot, name := r.PathValue("slot"), r.PostFormValue("name")
	err := s.apiAction(r, slot, "volume/"+name, compose.Quote([]string{"docker", "volume", "rm", name}), func(ctx context.Context, e Engine) error {
		return e.RemoveVolume(ctx, name)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	back(w, r, "/volumes", slot, "removed")
}

func (s *Server) volumePrune(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	all := r.PostFormValue("all") == "1"
	cli := []string{"docker", "volume", "prune", "-f"}
	if all {
		cli = append(cli, "-a")
	}
	if r.PostFormValue("confirm") != "yes" {
		var items []string
		e, _, err := s.engineFor(r.Context(), slot)
		if err == nil {
			var vols []engine.VolumeRow
			vols, err = e.Volumes(r.Context())
			e.Close()
			for _, v := range vols {
				if len(v.UsedBy) == 0 && (all || v.Anonymous) {
					items = append(items, v.Name)
				}
			}
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.render(w, http.StatusOK, "prune", s.page(r, "Prune volumes", "volumes", pruneData{Kind: "volumes", Slot: slot, All: all, Items: items, CLI: compose.Quote(cli)}))
		return
	}
	err := s.apiAction(r, slot, "volumes", compose.Quote(cli), func(ctx context.Context, e Engine) error {
		_, _, err := e.PruneVolumes(ctx, all)
		return err
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	back(w, r, "/volumes", slot, "pruned")
}

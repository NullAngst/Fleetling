package convert

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Tools names the binaries the converter shells out to. Empty fields use
// the name on PATH.
type Tools struct {
	Docker  string
	Kompose string
	Podlet  string
}

func (t Tools) bin(name, set string) string {
	if set != "" {
		return set
	}
	return name
}

// Input is one compose project to convert: a stack on disk (File set) or
// pasted text (Text set, with Dir standing in as its project folder).
type Input struct {
	Project string
	Dir     string
	File    string // absolute path of a compose file on disk
	Text    string // pasted compose text, when File is empty
}

// File is one output file.
type File struct {
	Name    string
	Content string
}

// ErrMissing means a tool isn't installed.
var ErrMissing = errors.New("not installed")

// run runs one tool with a minimal environment, so ${VARIABLES} in a pasted
// file can't pull values out of Fleetling's own environment. Compose still
// reads the stack's own .env.
func run(ctx context.Context, dir, stdin string, name string, args ...string) ([]byte, []byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", filepath.Base(name), ErrMissing)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	home, err := os.MkdirTemp("", "fleetling-convert-home-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(home)
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "NO_COLOR=1", "RUST_BACKTRACE=0", "RUST_LIB_BACKTRACE=0"}
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err = c.Run()
	stderr := []byte(cleanMessage(errb.String()))
	if err != nil {
		msg := string(stderr)
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), stderr, fmt.Errorf("%s", msg)
	}
	return out.Bytes(), stderr, nil
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// cleanMessage drops color codes, and the source location and backtrace
// podlet's error reporter appends after the message itself.
func cleanMessage(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	for _, cut := range []string{"\nLocation:", "\n  ━━━", "\nBacktrace omitted"} {
		if i := strings.Index(s, cut); i >= 0 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// existingDir is the closest folder that exists, for a working directory:
// a pasted file's project folder usually doesn't.
func existingDir(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		if d == filepath.Dir(d) {
			return os.TempDir()
		}
	}
}

// Normalize runs `docker compose config` twice: YAML for the Normalized
// tab and the converters, JSON for the lint.
func (t Tools) Normalize(ctx context.Context, in Input) (yamlOut, jsonOut []byte, err error) {
	args := []string{"compose", "-p", in.Project, "--project-directory", in.Dir}
	stdin := ""
	if in.File != "" {
		args = append(args, "-f", in.File)
	} else {
		args = append(args, "-f", "-")
		stdin = in.Text
		if strings.TrimSpace(stdin) == "" {
			return nil, nil, errors.New("paste a compose file first")
		}
	}
	args = append(args, "--ansi", "never", "config")
	wd := existingDir(in.Dir)
	docker := t.bin("docker", t.Docker)
	yamlOut, _, err = run(ctx, wd, stdin, docker, args...)
	if err != nil {
		return nil, nil, err
	}
	jsonOut, _, err = run(ctx, wd, stdin, docker, append(args, "--format", "json")...)
	return yamlOut, jsonOut, err
}

// The Quadlet unit types podlet writes, by their section header.
var quadletExt = map[string]string{
	"[Container]": "container", "[Pod]": "pod", "[Volume]": "volume",
	"[Network]": "network", "[Kube]": "kube", "[Image]": "image", "[Build]": "build",
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,200}$`)

// Quadlet runs `podlet compose --pod` on the normalized file and splits
// its output into one file per unit. podlet needs a top-level name: for
// --pod; the normalized file always has one.
func (t Tools) Quadlet(ctx context.Context, normalizedYAML []byte) ([]File, error) {
	dir, err := os.MkdirTemp("", "fleetling-quadlet-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	adapted, err := forPodlet(normalizedYAML)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(p, adapted, 0o600); err != nil {
		return nil, err
	}
	out, _, err := run(ctx, dir, "", t.bin("podlet", t.Podlet), "compose", "--pod", p)
	if err != nil {
		return nil, err
	}
	return splitPodlet(string(out))
}

// forPodlet adapts the normalized file to what podlet's compose parser
// takes, on the temporary copy only. Each change matches a lint warning:
//   - devices: Compose writes {source, target, permissions} maps, podlet
//     wants "source:target:permissions" strings
//   - build: podlet refuses build next to image; the image has to be built
//     first anyway
//   - depends_on conditions: podlet only takes service_started, so other
//     conditions become plain dependencies
func forPodlet(normalizedYAML []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(normalizedYAML, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("empty compose file")
	}
	svcs := mapValue(doc.Content[0], "services")
	if svcs == nil || svcs.Kind != yaml.MappingNode {
		return nil, errors.New("no services")
	}
	for i := 1; i < len(svcs.Content); i += 2 {
		svc := svcs.Content[i]
		if dev := mapValue(svc, "devices"); dev != nil {
			for j, d := range dev.Content {
				if d.Kind != yaml.MappingNode {
					continue
				}
				src, tgt, perm := scalar(mapValue(d, "source")), scalar(mapValue(d, "target")), scalar(mapValue(d, "permissions"))
				v := src + ":" + tgt
				if perm != "" {
					v += ":" + perm
				}
				dev.Content[j] = &yaml.Node{Kind: yaml.ScalarNode, Value: v}
			}
		}
		if mapValue(svc, "image") != nil {
			removeKey(svc, "build")
		}
		if deps := mapValue(svc, "depends_on"); deps != nil && deps.Kind == yaml.MappingNode {
			for j := 1; j < len(deps.Content); j += 2 {
				if c := mapValue(deps.Content[j], "condition"); c != nil {
					c.Value = "service_started"
				}
			}
		}
	}
	return yaml.Marshal(&doc)
}

func scalar(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	return n.Value
}

func removeKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// splitPodlet reads podlet's stdout: files joined by "\n---\n\n", each
// starting with "# FileName=<name>".
func splitPodlet(out string) ([]File, error) {
	var files []File
	for _, chunk := range strings.Split(out, "\n---\n") {
		chunk = strings.TrimLeft(chunk, "\n")
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		head, body, _ := strings.Cut(chunk, "\n")
		name, ok := strings.CutPrefix(strings.TrimSpace(head), "# FileName=")
		if !ok {
			return nil, fmt.Errorf("unexpected podlet output: %q", head)
		}
		ext := ""
		for _, line := range strings.Split(body, "\n") {
			if e, ok := quadletExt[strings.TrimSpace(line)]; ok {
				ext = e
				break
			}
		}
		if ext == "" {
			return nil, fmt.Errorf("podlet output for %s has no unit section", name)
		}
		if !strings.HasSuffix(name, "."+ext) {
			name += "." + ext
		}
		if !safeName.MatchString(name) {
			return nil, fmt.Errorf("podlet named a file %q, which isn't a plain file name", name)
		}
		files = append(files, File{Name: name, Content: strings.TrimRight(body, "\n") + "\n"})
	}
	if len(files) == 0 {
		return nil, errors.New("podlet wrote nothing")
	}
	return files, nil
}

// InstallCommands are the three commands that install Quadlet output on a
// host as root. Fleetling never runs them.
func InstallCommands(files []File) []string {
	var names, exts []string
	pod := ""
	for _, f := range files {
		names = append(names, f.Name)
		ext := filepath.Ext(f.Name)
		if !slices.Contains(exts, ext) {
			exts = append(exts, ext)
		}
		if ext == ".pod" {
			pod = strings.TrimSuffix(f.Name, ".pod")
		}
	}
	start := "sudo systemctl start "
	if pod != "" {
		start += pod + "-pod.service"
	} else {
		var units []string
		for _, n := range names {
			if strings.HasSuffix(n, ".container") {
				units = append(units, strings.TrimSuffix(n, ".container")+".service")
			}
		}
		start += strings.Join(units, " ")
	}
	return []string{
		"sudo cp " + strings.Join(names, " ") + " /etc/containers/systemd/",
		"sudo systemctl daemon-reload",
		start,
	}
}

// K8sOptions are the kompose choices on the Kubernetes tab.
type K8sOptions struct {
	Volumes     string // persistentVolumeClaim, emptyDir, hostPath, configMap
	ServiceType string // clusterip, nodeport, loadbalancer, headless
	Controller  string // deployment, daemonSet, statefulset
}

var (
	K8sVolumes     = []string{"persistentVolumeClaim", "emptyDir", "hostPath", "configMap"}
	K8sServiceType = []string{"clusterip", "nodeport", "loadbalancer", "headless"}
	K8sControllers = []string{"deployment", "daemonSet", "statefulset"}
)

// Valid fills in defaults and checks the choices against kompose's own.
func (o *K8sOptions) Valid() error {
	if o.Volumes == "" {
		o.Volumes = K8sVolumes[0]
	}
	if o.ServiceType == "" {
		o.ServiceType = K8sServiceType[0]
	}
	if o.Controller == "" {
		o.Controller = K8sControllers[0]
	}
	if !slices.Contains(K8sVolumes, o.Volumes) || !slices.Contains(K8sServiceType, o.ServiceType) || !slices.Contains(K8sControllers, o.Controller) {
		return errors.New("unknown Kubernetes option")
	}
	return nil
}

// Kubernetes runs `kompose convert --stdout`. kompose has no flag for the
// Service type, so it is set through its documented label,
// kompose.service.type, on every service in a temporary copy of the
// normalized file. The user's file is never touched.
func (t Tools) Kubernetes(ctx context.Context, normalizedYAML []byte, o K8sOptions) ([]File, string, error) {
	if err := o.Valid(); err != nil {
		return nil, "", err
	}
	labeled, err := withServiceType(normalizedYAML, o.ServiceType)
	if err != nil {
		return nil, "", err
	}
	dir, err := os.MkdirTemp("", "fleetling-k8s-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(p, labeled, 0o600); err != nil {
		return nil, "", err
	}
	out, stderr, err := run(ctx, dir, "", t.bin("kompose", t.Kompose),
		"convert", "--stdout", "-f", p, "--volumes", o.Volumes, "--controller", o.Controller)
	if err != nil {
		return nil, "", err
	}
	files, err := splitManifests(out)
	return files, strings.TrimSpace(string(stderr)), err
}

// withServiceType sets labels.kompose.service.type on every service.
func withServiceType(normalizedYAML []byte, typ string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(normalizedYAML, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("empty compose file")
	}
	svcs := mapValue(doc.Content[0], "services")
	if svcs == nil || svcs.Kind != yaml.MappingNode {
		return nil, errors.New("no services")
	}
	for i := 1; i < len(svcs.Content); i += 2 {
		svc := svcs.Content[i]
		labels := mapValue(svc, "labels")
		if labels == nil {
			labels = &yaml.Node{Kind: yaml.MappingNode}
			svc.Content = append(svc.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "labels"}, labels)
		}
		if labels.Kind == yaml.SequenceNode {
			labels.Content = append(labels.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "kompose.service.type=" + typ})
			continue
		}
		if v := mapValue(labels, "kompose.service.type"); v != nil {
			v.Value = typ
			continue
		}
		labels.Content = append(labels.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "kompose.service.type"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: typ, Style: yaml.DoubleQuotedStyle})
	}
	return yaml.Marshal(&doc)
}

// splitManifests splits kompose's multi-document output into one file
// per object, named <name>-<kind>.yaml like kompose's own -o folder.
func splitManifests(out []byte) ([]File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(out))
	var files []File
	seen := map[string]int{}
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("read kompose output: %w", err)
		}
		if len(n.Content) == 0 {
			continue
		}
		obj := n.Content[0]
		kind, name := "", ""
		if k := mapValue(obj, "kind"); k != nil {
			kind = strings.ToLower(k.Value)
		}
		if md := mapValue(obj, "metadata"); md != nil {
			if nm := mapValue(md, "name"); nm != nil {
				name = nm.Value
			}
		}
		if kind == "list" {
			// Some kompose versions print a single List; split its items.
			if items := mapValue(obj, "items"); items != nil {
				for _, it := range items.Content {
					b, _ := yaml.Marshal(it)
					sub, err := splitManifests(b)
					if err != nil {
						return nil, err
					}
					files = append(files, sub...)
				}
			}
			continue
		}
		b, err := yaml.Marshal(&n)
		if err != nil {
			return nil, err
		}
		fn := name + "-" + kind + ".yaml"
		if seen[fn]++; seen[fn] > 1 {
			fn = fmt.Sprintf("%s-%s-%d.yaml", name, kind, seen[fn])
		}
		if !safeName.MatchString(fn) {
			return nil, fmt.Errorf("kompose wrote an object named %q, which isn't a plain file name", fn)
		}
		files = append(files, File{Name: fn, Content: string(b)})
	}
	if len(files) == 0 {
		return nil, errors.New("kompose wrote nothing")
	}
	return files, nil
}

// Tar packs files into one archive under folder/, built with archive/tar.
func Tar(folder string, files []File, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: folder + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: now}); err != nil {
		return nil, err
	}
	for _, f := range files {
		if !safeName.MatchString(f.Name) {
			return nil, fmt.Errorf("refusing file name %q", f.Name)
		}
		if err := tw.WriteHeader(&tar.Header{Name: folder + "/" + f.Name, Mode: 0o644, Size: int64(len(f.Content)), ModTime: now}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(f.Content)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SafeName reports whether a converter output name is usable as a file name.
func SafeName(n string) bool { return safeName.MatchString(n) }

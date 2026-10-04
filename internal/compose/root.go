package compose

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mount is one mount of Fleetling's own container, as the engine reports it.
type Mount struct {
	Type        string // "bind", "volume", ...
	Source      string // host path
	Destination string // path inside the container
}

// Paths that can never be a stack root. "/" would put every host path
// "under the root", which the deletion rules in a later phase rely on not
// being true.
var forbiddenRoots = []string{"/", "/proc", "/sys", "/dev", "/run", "/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/boot", "/var/run"}

// CleanRoot checks the shape of a root path without touching the disk.
func CleanRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("stack root is empty")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("stack root %q must be an absolute path", root)
	}
	root = filepath.Clean(root)
	for _, f := range forbiddenRoots {
		if root == f || (f != "/" && isUnder(root, f)) {
			return "", fmt.Errorf("%s cannot be the stack root", root)
		}
	}
	return root, nil
}

// ValidateRoot checks the shape of root and that it is an existing folder.
// It returns the cleaned path.
func ValidateRoot(root string) (string, error) {
	root, err := CleanRoot(root)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s does not exist inside the Fleetling container. Mount it with the volume line - %s:%s", root, root, root)
	}
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a folder", root)
	}
	return root, nil
}

// CheckRootMount enforces the identical-path rule: the root has to come from
// a bind mount whose host path and container path are the same, so a bind
// source in a compose file means the same thing to Compose inside this
// container and to the engine on the host.
//
// The root may sit below the mount point. /srv:/srv with a root of
// /srv/stacks is fine.
func CheckRootMount(root string, mounts []Mount) error {
	fix := fmt.Sprintf("add the volume line - %s:%s to Fleetling's compose file and recreate it", root, root)
	var best *Mount
	for i := range mounts {
		m := &mounts[i]
		d := filepath.Clean(m.Destination)
		if (d == root || isUnder(root, d)) && (best == nil || len(d) > len(filepath.Clean(best.Destination))) {
			best = m
		}
	}
	if best == nil {
		return fmt.Errorf("%s is not mounted into this container; %s", root, fix)
	}
	if best.Type != "" && best.Type != "bind" {
		return fmt.Errorf("%s comes from a %s mount at %s, not a bind mount of the host folder; %s", root, best.Type, best.Destination, fix)
	}
	if filepath.Clean(best.Source) != filepath.Clean(best.Destination) {
		return fmt.Errorf("%s is mounted from host path %s at container path %s; the two must be identical. %s",
			root, best.Source, best.Destination, strings.ToUpper(fix[:1])+fix[1:])
	}
	return nil
}

// isUnder reports whether p is strictly inside dir. Both must be clean.
func isUnder(p, dir string) bool {
	if dir == "/" {
		return p != "/"
	}
	return strings.HasPrefix(p, dir+"/")
}

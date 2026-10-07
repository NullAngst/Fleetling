package compose

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// SelfMountInfo is where Linux lists the mounts this process can see.
// Inside Fleetling's container that is the container's own view, which is
// the view deletions happen in.
const SelfMountInfo = "/proc/self/mountinfo"

// ReadMountPoints returns every mount point listed in a mountinfo file.
func ReadMountPoints(path string) ([]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	return parseMountInfo(fh)
}

// parseMountInfo reads the fifth field of each line, the mount point.
// The kernel writes spaces, tabs, newlines and backslashes in paths as
// three-digit octal escapes, so "/opt/my media" arrives as
// "/opt/my\040media".
func parseMountInfo(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		out = append(out, filepath.Clean(unescapeOctal(f[4])))
	}
	return out, sc.Err()
}

func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// deviceOf returns the device ID of path without following a final
// symlink. Two paths on different devices are on different filesystems,
// so a folder whose device differs from its parent's is a mount point.
func deviceOf(path string) (uint64, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), nil
	}
	return 0, nil
}

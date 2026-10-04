package compose

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// EnvVar is one KEY=value pair from an env file, with its line number.
type EnvVar struct {
	Key   string
	Value string
	Line  int
}

// ParseEnv reads a .env file the way Compose does for the common cases:
//
//	# comments and blank lines are skipped
//	export KEY=value     the export prefix is dropped
//	KEY=value # note     an unquoted value ends at " #"
//	KEY="a b"            double quotes allow \n, \t, \" and \\ escapes
//	KEY='a b'            single quotes are literal
//
// Multi-line quoted values are not supported and are reported as an error,
// so nothing is silently misread. A later key wins over an earlier one when
// looked up with EnvMap.
func ParseEnv(r io.Reader) ([]EnvVar, error) {
	var out []EnvVar
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if n == 1 {
			line = strings.TrimPrefix(line, "\ufeff") // byte order mark
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !envKeyRe.MatchString(key) {
			return nil, fmt.Errorf("line %d: expected KEY=value", n)
		}
		v, err := parseEnvValue(strings.TrimSpace(val))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		out = append(out, EnvVar{Key: key, Value: v, Line: n})
	}
	return out, sc.Err()
}

func parseEnvValue(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	switch v[0] {
	case '\'':
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("unclosed single quote")
		}
		return v[1 : end+1], nil
	case '"':
		var b strings.Builder
		for i := 1; i < len(v); i++ {
			c := v[i]
			if c == '"' {
				return b.String(), nil
			}
			if c == '\\' && i+1 < len(v) {
				i++
				switch v[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				default:
					b.WriteByte(v[i])
				}
				continue
			}
			b.WriteByte(c)
		}
		return "", fmt.Errorf("unclosed double quote")
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v), nil
}

// EnvMap flattens parsed vars into a map, later keys winning.
func EnvMap(vars []EnvVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Key] = v.Value
	}
	return m
}

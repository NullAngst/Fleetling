// Package compose knows about stacks on disk: discovery, metadata, project
// names, and how folders line up with what the engines are running.
//
// Later phases add the runner that shells out to `docker compose` here too.
package compose

import (
	"errors"
	"regexp"
	"strings"
)

// Compose's own rule: lowercase letters, digits, dashes and underscores,
// starting with a letter or digit.
var projectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidProject returns an error when name is not a legal Compose project name.
func ValidProject(name string) error {
	if name == "" {
		return errors.New("project name is empty")
	}
	if len(name) > 128 {
		return errors.New("project name is longer than 128 characters")
	}
	if !projectRe.MatchString(name) {
		return errors.New("project name may only use lowercase letters, digits, - and _, and must start with a letter or digit")
	}
	return nil
}

// NormalizeProject turns a folder name into the project name Compose would
// pick for it when no -p or name: is given. Same steps as Compose: lowercase,
// drop anything outside [a-z0-9_-], then trim leading - and _.
// For "/opt/NPMplus_data" that gives "npmplus_data".
func NormalizeProject(folder string) string {
	folder = strings.ToLower(folder)
	var b strings.Builder
	for _, r := range folder {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "-_")
}

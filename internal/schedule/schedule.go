// Package schedule parses auto-update schedules: a preset or a five-field
// cron expression, in the container's time zone. Parsing and matching
// come from github.com/adhocore/gronx.
package schedule

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/adhocore/gronx"
)

// Presets, both at 04:00.
const (
	Daily  = "0 4 * * *"
	Weekly = "0 4 * * 0" // Sunday
)

// Parse turns form input into a cron expression. "daily" and "weekly" are
// the presets; anything else must be five cron fields.
func Parse(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	switch strings.ToLower(s) {
	case "daily":
		return Daily, nil
	case "weekly":
		return Weekly, nil
	}
	if len(strings.Fields(s)) != 5 {
		return "", errors.New("a schedule is five cron fields (minute hour day month weekday), like 0 4 * * *")
	}
	if !gronx.IsValid(s) {
		return "", errors.New("that cron expression doesn't parse")
	}
	return s, nil
}

// Next is the first run strictly after t.
func Next(expr string, t time.Time) (time.Time, error) {
	return gronx.NextTickAfter(expr, t, false)
}

// Due reports whether expr fires in the minute t falls in.
func Due(expr string, t time.Time) bool {
	ok, err := gronx.New().IsDue(expr, t.Truncate(time.Minute))
	return err == nil && ok
}

// Describe names a preset, or returns the expression.
func Describe(expr string) string {
	switch expr {
	case Daily:
		return "daily at 04:00"
	case Weekly:
		return "weekly, Sunday at 04:00"
	}
	return expr
}

// Zone says which time zone schedules run in, for the page to show.
func Zone() string {
	name, _ := time.Now().Zone()
	if tz := os.Getenv("TZ"); tz != "" {
		return tz + " (" + name + ")"
	}
	if time.Local.String() == "Local" || time.Local.String() == "UTC" {
		return name + ", since TZ isn't set in the container"
	}
	return time.Local.String() + " (" + name + ")"
}

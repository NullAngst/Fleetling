package schedule

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]string{"daily": Daily, "Weekly": Weekly, " 30  2 * * 1-5 ": "30 2 * * 1-5"} {
		if got, err := Parse(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "* * * *", "0 0 4 * * *", "61 * * * *", "@reboot", "0 4 * * 9"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestNextAndDue(t *testing.T) {
	loc := time.FixedZone("X", -5*3600)
	at := time.Date(2026, 11, 12, 3, 59, 30, 0, loc)
	n, err := Next(Daily, at)
	if err != nil || !n.Equal(time.Date(2026, 11, 12, 4, 0, 0, 0, loc)) {
		t.Errorf("next daily: %v %v", n, err)
	}
	if !Due(Daily, time.Date(2026, 11, 12, 4, 0, 40, 0, loc)) || Due(Daily, time.Date(2026, 11, 12, 4, 1, 0, 0, loc)) {
		t.Error("due")
	}
	n, _ = Next(Weekly, at) // a Thursday
	if n.Weekday() != time.Sunday || n.Hour() != 4 {
		t.Errorf("next weekly: %v", n)
	}
}

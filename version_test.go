package fleetling

import "testing"

func TestVersionFileFollowsTheNumbering(t *testing.T) {
	v, err := Parse(Version())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Valid(); err != nil {
		t.Fatalf("VERSION: %v", err)
	}
}

func TestNext(t *testing.T) {
	for in, want := range map[string]string{"1.0.0": "1.1.0", "1.8.0": "1.9.0", "1.9.0": "2.0.0", "2.9.0": "3.0.0"} {
		v, _ := Parse(in)
		if got := v.Next().String(); got != want {
			t.Errorf("Next(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestParseAndCompare(t *testing.T) {
	for _, bad := range []string{"", "1.0", "1.0.0.0", "1.x.0", "01.0.0", "-1.0.0"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	old, _ := Parse("V0.9.1")
	cur, _ := Parse("v1.0.0")
	if !old.Less(cur) || cur.Less(old) {
		t.Error("0.9.1 should be older than 1.0.0")
	}
	if old.Valid() == nil {
		t.Error("0.9.1 is from before the numbering")
	}
	for _, bad := range []string{"1.10.0", "1.2.3"} {
		v, _ := Parse(bad)
		if v.Valid() == nil {
			t.Errorf("%s passed as valid", bad)
		}
	}
}

package auth

import (
	"strings"
	"testing"
	"time"
)

// cheap keeps the tests fast. The format and verify path are the same.
var cheap = Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashAndVerify(t *testing.T) {
	h, err := hashWith("correct horse battery", cheap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("unexpected format: %s", h)
	}
	ok, err := VerifyPassword(h, "correct horse battery")
	if err != nil || !ok {
		t.Fatalf("right password: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(h, "correct horse batterY")
	if err != nil || ok {
		t.Fatalf("wrong password: ok=%v err=%v", ok, err)
	}
}

func TestSaltsDiffer(t *testing.T) {
	a, _ := hashWith("same password here", cheap)
	b, _ := hashWith("same password here", cheap)
	if a == b {
		t.Fatal("two hashes of the same password are identical, salt is not random")
	}
}

func TestDefaultParamsVerify(t *testing.T) {
	h, err := HashPassword("twelve chars")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword(h, "twelve chars"); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestMalformedHashes(t *testing.T) {
	for _, h := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=1024,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=18$m=1024,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=0,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=1024,t=1,p=1$!!!$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=1024,t=1,p=1$c2FsdHNhbHQ$c2hvcnQ",
	} {
		if _, err := VerifyPassword(h, "x"); err == nil {
			t.Errorf("accepted malformed hash %q", h)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	if CheckPasswordPolicy("elevenchars") == nil {
		t.Error("accepted an 11 character password")
	}
	if err := CheckPasswordPolicy("twelve chars"); err != nil {
		t.Error(err)
	}
	// 12 runes, more than 12 bytes. Counted as characters.
	if err := CheckPasswordPolicy("ääääääääääää"); err != nil {
		t.Error(err)
	}
}

func TestLimiterLocksAfterFiveFailures(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewLimiter(5, 15*time.Minute, 15*time.Minute)
	l.now = func() time.Time { return now }

	for i := range 4 {
		if l.Fail("10.0.0.5") {
			t.Fatalf("locked after %d failures", i+1)
		}
		now = now.Add(time.Minute)
	}
	if ok, _ := l.Allowed("10.0.0.5"); !ok {
		t.Fatal("locked before the fifth failure")
	}
	if !l.Fail("10.0.0.5") {
		t.Fatal("fifth failure did not lock")
	}
	ok, retry := l.Allowed("10.0.0.5")
	if ok || retry != 15*time.Minute {
		t.Fatalf("Allowed = %v %v, want false 15m", ok, retry)
	}
	if ok, _ := l.Allowed("10.0.0.6"); !ok {
		t.Fatal("another IP got locked out")
	}
	now = now.Add(15 * time.Minute)
	if ok, _ := l.Allowed("10.0.0.5"); !ok {
		t.Fatal("still locked after the lockout ended")
	}
}

func TestLimiterWindowSlides(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewLimiter(5, 15*time.Minute, 15*time.Minute)
	l.now = func() time.Time { return now }
	// Four failures, then wait out the window. The old ones should not count.
	for range 4 {
		l.Fail("ip")
	}
	now = now.Add(16 * time.Minute)
	if l.Fail("ip") {
		t.Fatal("failures outside the window still counted")
	}
}

func TestLimiterReset(t *testing.T) {
	l := NewLimiter(2, time.Hour, time.Hour)
	l.Fail("ip")
	l.Reset("ip")
	if l.Fail("ip") {
		t.Fatal("reset did not clear earlier failures")
	}
}

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

// fail runs one whole wrong try and reports whether it was let through and
// whether it started a lockout.
func fail(l *Limiter, key string) (allowed, locked bool) {
	if ok, _ := l.Take(key); !ok {
		return false, false
	}
	return true, l.Done(key, Failure)
}

func TestLimiterLocksAfterFiveFailures(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewLimiter(5, 15*time.Minute, 15*time.Minute)
	l.now = func() time.Time { return now }

	for i := range 4 {
		if ok, locked := fail(l, "10.0.0.5"); !ok || locked {
			t.Fatalf("try %d: allowed %v, locked %v", i+1, ok, locked)
		}
		now = now.Add(time.Minute)
	}
	if ok, locked := fail(l, "10.0.0.5"); !ok || !locked {
		t.Fatalf("fifth failure: allowed %v, locked %v", ok, locked)
	}
	ok, retry := l.Take("10.0.0.5")
	if ok || retry != 15*time.Minute {
		t.Fatalf("Take = %v %v, want false 15m", ok, retry)
	}
	if ok, _ := l.Take("10.0.0.6"); !ok {
		t.Fatal("another address got locked out")
	}
	now = now.Add(15 * time.Minute)
	if ok, _ := l.Take("10.0.0.5"); !ok {
		t.Fatal("still locked after the lockout ended")
	}
}

func TestLimiterWindowSlides(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewLimiter(5, 15*time.Minute, 15*time.Minute)
	l.now = func() time.Time { return now }
	// Four failures, then wait out the window. The old ones should not count.
	for range 4 {
		fail(l, "ip")
	}
	now = now.Add(16 * time.Minute)
	if _, locked := fail(l, "ip"); locked {
		t.Fatal("failures outside the window still counted")
	}
}

func TestLimiterSuccessClears(t *testing.T) {
	l := NewLimiter(2, time.Hour, time.Hour)
	fail(l, "ip")
	l.Take("ip")
	l.Done("ip", Success)
	if _, locked := fail(l, "ip"); locked {
		t.Fatal("a right password did not clear earlier failures")
	}
}

// Tries are counted when they start. A burst of parallel logins gets no
// more tries through than the limit, however slow each check is.
func TestLimiterCountsTriesInFlight(t *testing.T) {
	l := NewLimiter(5, 15*time.Minute, 15*time.Minute)
	let := 0
	for range 1000 {
		if ok, _ := l.Take("burst"); ok {
			let++
		}
	}
	if let != 5 {
		t.Fatalf("%d tries let through at once, want 5", let)
	}
	// A try that never checked anything gives its place back.
	l.Done("burst", Abandoned)
	if ok, _ := l.Take("burst"); !ok {
		t.Fatal("an abandoned try kept its place")
	}
	for range 5 {
		l.Done("burst", Failure)
	}
	if ok, _ := l.Take("burst"); ok {
		t.Fatal("not locked after five failures")
	}
}

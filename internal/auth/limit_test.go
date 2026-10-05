package auth

import (
	"testing"
	"time"
)

// A burst of logins must not run more than two argon2 computations at
// once; the rest wait their turn.
func TestArgonConcurrencyIsCapped(t *testing.T) {
	h, err := hashWith("correct horse battery", cheap)
	if err != nil {
		t.Fatal(err)
	}
	// Take both slots, as two long-running logins would.
	argonSlots <- struct{}{}
	argonSlots <- struct{}{}
	done := make(chan bool)
	go func() {
		ok, _ := VerifyPassword(h, "correct horse battery")
		done <- ok
	}()
	select {
	case <-done:
		t.Fatal("a third argon2 computation ran while both slots were taken")
	case <-time.After(150 * time.Millisecond):
	}
	<-argonSlots
	select {
	case ok := <-done:
		if !ok {
			t.Error("verify failed after getting a slot")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verify never ran after a slot freed up")
	}
	<-argonSlots
}

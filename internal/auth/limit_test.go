package auth

import (
	"context"
	"errors"
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

// Past the queue cap a check is refused at once instead of waiting, and a
// waiter whose request ends leaves the queue.
func TestArgonQueueIsBounded(t *testing.T) {
	h, err := hashWith("correct horse battery", cheap)
	if err != nil {
		t.Fatal(err)
	}
	argonSlots <- struct{}{}
	argonSlots <- struct{}{}
	defer func() { <-argonSlots; <-argonSlots }()

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, maxArgonWaiting)
	for range maxArgonWaiting {
		go func() {
			_, err := VerifyPasswordContext(ctx, h, "correct horse battery")
			errs <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for argonWaiting.Load() < maxArgonWaiting {
		if time.Now().After(deadline) {
			t.Fatalf("only %d waiting", argonWaiting.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := VerifyPasswordContext(context.Background(), h, "x"); !errors.Is(err, ErrBusy) {
		t.Fatalf("over the cap: %v, want ErrBusy", err)
	}
	cancel()
	for range maxArgonWaiting {
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter: %v", err)
		}
	}
	if n := argonWaiting.Load(); n != 0 {
		t.Fatalf("%d still counted as waiting", n)
	}
}

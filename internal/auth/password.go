// Package auth holds password hashing and the login rate limiter.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// MinPasswordLength is counted in characters, not bytes.
const MinPasswordLength = 12

// Params are the argon2id cost settings. The defaults are RFC 9106's second
// recommended option: 64 MiB, 3 passes, 4 lanes. A login takes a few tens
// of milliseconds on a home server, which is fine for one user.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams is used for every new hash.
var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 4, SaltLen: 16, KeyLen: 32}

// argonSlots caps how many argon2 computations run at once. Each one
// allocates Params.Memory (64 MiB by default), and the login form is
// reachable without a session, so without a cap a burst of concurrent
// login posts allocates 64 MiB apiece until the kernel's OOM killer
// starts picking containers. Two slots keep a real login fast while
// bounding the worst case at about 128 MiB.
var argonSlots = make(chan struct{}, 2)

// maxArgonWaiting caps the queue behind the two slots. Past it a check is
// refused with ErrBusy straight away instead of waiting, so a flood can't
// park an unbounded number of requests in front of a real login.
const maxArgonWaiting = 16

var argonWaiting atomic.Int32

// ErrBusy means too many password checks are already queued.
var ErrBusy = errors.New("too many password checks at once, try again in a moment")

// idKey runs argon2id once a slot is free. It gives up when ctx ends, so a
// browser that went away doesn't keep its place in the queue.
func idKey(ctx context.Context, pw, salt []byte, p Params, keyLen uint32) ([]byte, error) {
	select {
	case argonSlots <- struct{}{}:
	default:
		if argonWaiting.Add(1) > maxArgonWaiting {
			argonWaiting.Add(-1)
			return nil, ErrBusy
		}
		select {
		case argonSlots <- struct{}{}:
			argonWaiting.Add(-1)
		case <-ctx.Done():
			argonWaiting.Add(-1)
			return nil, ctx.Err()
		}
	}
	defer func() { <-argonSlots }()
	return argon2.IDKey(pw, salt, p.Time, p.Memory, p.Threads, keyLen), nil
}

// ErrMalformedHash means a stored hash could not be parsed.
var ErrMalformedHash = errors.New("malformed password hash")

var b64 = base64.RawStdEncoding

// CheckPasswordPolicy returns a user-facing error when a new password is too weak.
func CheckPasswordPolicy(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	return nil
}

// HashPassword returns a PHC-format string:
// $argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>
// The parameters travel with the hash, so they can be raised later without
// breaking existing logins.
func HashPassword(pw string) (string, error) {
	return hashWith(pw, DefaultParams)
}

func hashWith(pw string, p Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := idKey(context.Background(), []byte(pw), salt, p, p.KeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether pw matches the stored hash. The comparison
// is constant time.
func VerifyPassword(encoded, pw string) (bool, error) {
	return VerifyPasswordContext(context.Background(), encoded, pw)
}

// VerifyPasswordContext is VerifyPassword for a request: it stops waiting
// for an argon2 slot when ctx ends, and returns ErrBusy when the queue is
// full. Neither of those means the password was wrong.
func VerifyPasswordContext(ctx context.Context, encoded, pw string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, ErrMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrMalformedHash
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return false, ErrMalformedHash
	}
	// Refuse absurd costs from a tampered database instead of allocating them.
	if p.Memory == 0 || p.Memory > 4*1024*1024 || p.Time == 0 || p.Time > 64 || p.Threads == 0 {
		return false, ErrMalformedHash
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, ErrMalformedHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 128 {
		return false, ErrMalformedHash
	}
	got, err := idKey(ctx, []byte(pw), salt, p, uint32(len(want)))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// Package auth holds password hashing and the login rate limiter.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
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
	key := argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether pw matches the stored hash. The comparison
// is constant time.
func VerifyPassword(encoded, pw string) (bool, error) {
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
	got := argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"time"
)

// Sessions are stateless signed cookies. The cookie holds an expiry and a
// random ID, signed with HMAC-SHA256 using the session secret from the
// database. "Log out everywhere" replaces the secret, which makes every
// cookie ever issued fail verification at once.

const (
	sessionCookie = "fleetling_session"
	preauthCookie = "fleetling_pre"
	sessionTTL    = 7 * 24 * time.Hour
	csrfHeader    = "X-CSRF-Token"
	csrfField     = "csrf"
)

var b64 = base64.RawURLEncoding

func sign(secret []byte, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, secret)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	// crypto/rand.Read never returns an error on Linux since Go 1.24; it
	// crashes the program instead, which is the right call for key material.
	rand.Read(b)
	return b
}

// newSession returns a cookie value and the session ID inside it.
func newSession(secret []byte, now time.Time) (value string, id []byte) {
	payload := make([]byte, 8, 8+16)
	binary.BigEndian.PutUint64(payload, uint64(now.Add(sessionTTL).Unix()))
	id = randomBytes(16)
	payload = append(payload, id...)
	return b64.EncodeToString(append(payload, sign(secret, payload)...)), id
}

// verifySession returns the session ID when value is a valid, unexpired cookie.
func verifySession(secret []byte, value string, now time.Time) ([]byte, bool) {
	raw, err := b64.DecodeString(value)
	if err != nil || len(raw) != 8+16+sha256.Size {
		return nil, false
	}
	payload, mac := raw[:24], raw[24:]
	if !hmac.Equal(mac, sign(secret, payload)) {
		return nil, false
	}
	if now.Unix() >= int64(binary.BigEndian.Uint64(payload[:8])) {
		return nil, false
	}
	return payload[8:24], true
}

// csrfFor derives the CSRF token for a session (or a pre-auth cookie) so
// nothing extra has to be stored. kind keeps the two token types apart.
func csrfFor(secret []byte, kind string, id []byte) string {
	return b64.EncodeToString(sign(secret, []byte("csrf:"+kind+":"), id))
}

func tokenEqual(a, b string) bool {
	return a != "" && hmac.Equal([]byte(a), []byte(b))
}

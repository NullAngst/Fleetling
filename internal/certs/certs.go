// Package certs gives Fleetling a TLS certificate: the user's own when one
// is configured, otherwise a self-signed one it generates and renews.
//
// A self-signed certificate does not prove identity on first contact; the
// browser warns once and the user accepts it. What it buys is encryption:
// the password and the session cookie no longer cross the LAN in clear
// text. The SHA-256 fingerprint is logged at startup so the certificate
// the browser shows can be checked against it before accepting.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Validity of a generated certificate. Apple platforms reject server
// certificates valid for more than 825 days even when trusted by hand; 397
// days stays inside every browser's limits.
const (
	validity    = 397 * 24 * time.Hour
	renewBefore = 30 * 24 * time.Hour
)

// Options say where certificates come from.
type Options struct {
	Dir      string   // where a generated certificate lives, e.g. <data>/tls
	CertFile string   // the user's own certificate (PEM, may include the chain)
	KeyFile  string   // and its key
	Hosts    []string // extra names and IPs for a generated certificate
	Now      func() time.Time
}

// Result is a ready TLS configuration plus what to tell the user.
type Result struct {
	Config      *tls.Config
	Fingerprint string // SHA-256 of the leaf certificate, colon separated
	Generated   bool   // true when a new self-signed certificate was made now
	SelfSigned  bool
	NotAfter    time.Time
	Names       []string
}

// Setup loads or creates the certificate and returns the server config.
func Setup(o Options) (*Result, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if (o.CertFile == "") != (o.KeyFile == "") {
		return nil, errors.New("set both FLEETLING_TLS_CERT and FLEETLING_TLS_KEY, or neither")
	}
	if o.CertFile != "" {
		return userCert(o)
	}
	return selfSigned(o)
}

func baseConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// userCert serves the user's own files and reloads them when they change
// on disk, so a renewed certificate takes effect without a restart.
func userCert(o Options) (*Result, error) {
	r := &reloader{certFile: o.CertFile, keyFile: o.KeyFile}
	if err := r.load(); err != nil {
		return nil, err
	}
	cfg := baseConfig()
	cfg.GetCertificate = r.get
	leaf := r.cert.Leaf
	return &Result{Config: cfg, Fingerprint: Fingerprint(leaf), NotAfter: leaf.NotAfter, Names: names(leaf)}, nil
}

type reloader struct {
	certFile, keyFile string
	mu                sync.Mutex
	cert              *tls.Certificate
	mtime             time.Time
}

func (r *reloader) load() error {
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	if c.Leaf == nil {
		if c.Leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
			return err
		}
	}
	fi, err := os.Stat(r.certFile)
	if err != nil {
		return err
	}
	r.cert, r.mtime = &c, fi.ModTime()
	return nil
}

func (r *reloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fi, err := os.Stat(r.certFile); err == nil && !fi.ModTime().Equal(r.mtime) {
		// A half-written renewal fails to load; keep serving the old one.
		_ = r.load()
	}
	return r.cert, nil
}

func selfSigned(o Options) (*Result, error) {
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	certPath, keyPath := filepath.Join(o.Dir, "cert.pem"), filepath.Join(o.Dir, "key.pem")
	want := wantedNames(o.Hosts)

	generated := false
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err == nil {
		c.Leaf, err = x509.ParseCertificate(c.Certificate[0])
	}
	if err != nil || o.Now().Add(renewBefore).After(c.Leaf.NotAfter) || !covers(c.Leaf, want) {
		if c, err = generate(certPath, keyPath, want, o.Now()); err != nil {
			return nil, err
		}
		generated = true
	}
	rn := &renewer{cert: &c, certPath: certPath, keyPath: keyPath, names: want, now: o.Now}
	cfg := baseConfig()
	cfg.GetCertificate = rn.get
	return &Result{Config: cfg, Fingerprint: Fingerprint(c.Leaf), Generated: generated, SelfSigned: true, NotAfter: c.Leaf.NotAfter, Names: names(c.Leaf)}, nil
}

// renewer replaces a generated certificate that is about to expire while
// the server keeps running, so a long uptime never ends in an expired one.
type renewer struct {
	mu                sync.Mutex
	cert              *tls.Certificate
	certPath, keyPath string
	names             []string
	now               func() time.Time
}

func (r *renewer) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.now().Add(renewBefore).After(r.cert.Leaf.NotAfter) {
		if c, err := generate(r.certPath, r.keyPath, r.names, r.now()); err == nil {
			r.cert = &c
		}
	}
	return r.cert, nil
}

// wantedNames is what a generated certificate covers: localhost, loopback,
// and whatever FLEETLING_TLS_HOSTS adds (the server's LAN IP and DNS name,
// which a container can't see itself). The container's own hostname is left
// out on purpose: it changes on every recreate, and a name that changes
// would mean a new certificate, and a new browser warning, after every
// update.
func wantedNames(extra []string) []string {
	out := []string{"localhost", "127.0.0.1", "::1"}
	for _, e := range extra {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func names(c *x509.Certificate) []string {
	out := slices.Clone(c.DNSNames)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	slices.Sort(out)
	return out
}

func covers(c *x509.Certificate, want []string) bool {
	have := names(c)
	for _, w := range want {
		if ip := net.ParseIP(w); ip != nil {
			w = ip.String()
		}
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

func generate(certPath, keyPath string, hosts []string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Fleetling self-signed", Organization: []string{"Fleetling"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	// The key first, at 0600, so there is never a moment with a new
	// certificate and an old or world-readable key.
	if err := writeFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	c.Leaf, err = x509.ParseCertificate(c.Certificate[0])
	return c, err
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tls-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Fingerprint is the certificate's SHA-256 in the colon form browsers show.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

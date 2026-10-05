package certs

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSelfSignedIsGeneratedOnceAndReused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	r1, err := Setup(Options{Dir: dir, Hosts: []string{"192.168.1.10", "fleet.lan", ""}})
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Generated || !r1.SelfSigned || len(r1.Fingerprint) != 95 {
		t.Fatalf("first run %+v", r1)
	}
	for _, n := range []string{"localhost", "127.0.0.1", "::1", "192.168.1.10", "fleet.lan"} {
		if !slices.Contains(r1.Names, n) {
			t.Errorf("certificate misses %s: %v", n, r1.Names)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "key.pem")); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", fi.Mode())
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Errorf("tls dir mode %v", di.Mode())
	}
	r2, err := Setup(Options{Dir: dir, Hosts: []string{"fleet.lan", "192.168.1.10"}})
	if err != nil || r2.Generated || r2.Fingerprint != r1.Fingerprint {
		t.Fatalf("restart made a new certificate: %+v %v", r2, err)
	}
	// Asking for a new name makes a new certificate.
	r3, _ := Setup(Options{Dir: dir, Hosts: []string{"fleet.lan", "192.168.1.10", "10.0.0.5"}})
	if !r3.Generated || r3.Fingerprint == r1.Fingerprint {
		t.Error("new host name did not regenerate")
	}
}

func TestRenewsBeforeExpiry(t *testing.T) {
	dir := t.TempDir()
	r1, _ := Setup(Options{Dir: dir})
	later := func() time.Time { return time.Now().Add(validity - renewBefore + time.Hour) }
	r2, err := Setup(Options{Dir: dir, Now: later})
	if err != nil || !r2.Generated || r2.Fingerprint == r1.Fingerprint {
		t.Fatalf("expiring certificate kept at startup: %+v %v", r2, err)
	}
	// And while running.
	now := time.Now()
	r3, _ := Setup(Options{Dir: dir, Now: func() time.Time { return now }})
	c1, _ := r3.Config.GetCertificate(nil)
	now = now.Add(2 * validity)
	c2, _ := r3.Config.GetCertificate(nil)
	if c1 == c2 || !c2.Leaf.NotAfter.After(now) {
		t.Error("running server did not renew an expiring certificate")
	}
}

func TestServesTLS(t *testing.T) {
	res, err := Setup(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// A plain listener with our config; httptest would swap in its own certificate.
	l, err := tls.Listen("tcp", "127.0.0.1:0", res.Config)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })}
	go hs.Serve(l)
	defer hs.Close()
	srv := struct{ URL string }{"https://" + l.Addr().String()}
	// The browser's view: an unknown issuer, then the user checks the fingerprint.
	_, err = http.Get(srv.URL)
	var unknown x509.UnknownAuthorityError
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("self-signed certificate verified without trust: %v %T", err, unknown)
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		if Fingerprint(cs.PeerCertificates[0]) != res.Fingerprint {
			t.Error("served certificate does not match the logged fingerprint")
		}
		return nil
	}}}}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("TLS version %x", resp.TLS.Version)
	}
}

func TestUserCertificate(t *testing.T) {
	// Make a pair with the generator and hand it over as "the user's".
	src, _ := Setup(Options{Dir: t.TempDir()})
	_ = src
	dir := t.TempDir()
	gen, _ := Setup(Options{Dir: dir, Hosts: []string{"mine.lan"}})
	res, err := Setup(Options{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem")})
	if err != nil || res.SelfSigned || res.Fingerprint != gen.Fingerprint {
		t.Fatalf("user cert %+v %v", res, err)
	}
	if _, err := Setup(Options{CertFile: filepath.Join(dir, "cert.pem")}); err == nil {
		t.Error("certificate without a key accepted")
	}
	// Replacing the files on disk takes effect without a restart.
	newDir := t.TempDir()
	repl, _ := Setup(Options{Dir: newDir, Hosts: []string{"other.lan"}})
	time.Sleep(20 * time.Millisecond)
	for _, n := range []string{"cert.pem", "key.pem"} {
		b, _ := os.ReadFile(filepath.Join(newDir, n))
		os.WriteFile(filepath.Join(dir, n), b, 0o600)
	}
	future := time.Now().Add(time.Second)
	os.Chtimes(filepath.Join(dir, "cert.pem"), future, future)
	c, _ := res.Config.GetCertificate(nil)
	if Fingerprint(c.Leaf) != repl.Fingerprint {
		t.Error("user certificate not reloaded after it changed on disk")
	}
}

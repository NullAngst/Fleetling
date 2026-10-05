// Command fleetling runs the Fleetling web app.
//
//	fleetling              serve the web UI (default)
//	fleetling healthcheck  exit 0 if the local server answers /healthz
//	fleetling version      print the version
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/NullAngst/Fleetling/internal/certs"
	"github.com/NullAngst/Fleetling/internal/server"
	"github.com/NullAngst/Fleetling/internal/store"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println(version)
			return
		case "healthcheck":
			os.Exit(healthcheck())
		case "ifaces":
			// Run in a throwaway --network host container, so /sys/class/net
			// is the host's. That is how the networks page fills its parent
			// interface list.
			os.Exit(ifaces())
		case "serve":
		default:
			fmt.Fprintf(os.Stderr, "usage: fleetling [serve|healthcheck|ifaces|version]\n")
			os.Exit(2)
		}
	}
	if err := serve(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func listenAddr() string { return env("FLEETLING_ADDR", ":8420") }

// tlsEnabled is on unless FLEETLING_TLS=off. Plain HTTP is for running
// behind a reverse proxy on the same host, bound to 127.0.0.1.
func tlsEnabled() bool {
	switch strings.ToLower(os.Getenv("FLEETLING_TLS")) {
	case "off", "false", "0", "no":
		return false
	}
	return true
}

func serve() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	root := env("FLEETLING_ROOT", "/opt")
	dataDir := env("FLEETLING_DATA", filepath.Join(root, "fleetling", "data"))
	proxies, err := server.ParseTrustedProxies(os.Getenv("FLEETLING_TRUSTED_PROXIES"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, filepath.Join(dataDir, "fleetling.db"))
	if err != nil {
		return fmt.Errorf("open settings database in %s: %w", dataDir, err)
	}
	defer st.Close()

	srv, err := server.New(ctx, server.Config{
		DefaultRoot:    root,
		DataDir:        dataDir,
		Version:        version,
		TrustedProxies: proxies,
		Logger:         log,
	}, st)
	if err != nil {
		return err
	}
	if tok := srv.SetupToken(); tok != "" {
		log.Warn("first run: open /setup in a browser and enter this setup token", "token", tok)
	}

	hs := &http.Server{
		Addr:              listenAddr(),
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: later phases stream logs and shells for as long
		// as the browser keeps them open.
	}
	if tlsEnabled() {
		res, err := certs.Setup(certs.Options{
			Dir:      filepath.Join(dataDir, "tls"),
			CertFile: os.Getenv("FLEETLING_TLS_CERT"),
			KeyFile:  os.Getenv("FLEETLING_TLS_KEY"),
			Hosts:    strings.Split(os.Getenv("FLEETLING_TLS_HOSTS"), ","),
		})
		if err != nil {
			return err
		}
		hs.TLSConfig = res.Config
		kind := "your certificate"
		if res.SelfSigned {
			kind = "self-signed certificate"
			if res.Generated {
				kind = "new self-signed certificate"
			}
		}
		// Compare this with what the browser shows before accepting it.
		log.Info("TLS on with "+kind, "sha256", res.Fingerprint, "names", strings.Join(res.Names, ","), "expires", res.NotAfter.Format("2006-01-02"))
	} else {
		log.Warn("TLS is off (FLEETLING_TLS=off); only do this behind a reverse proxy on the same host")
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", hs.Addr, "tls", hs.TLSConfig != nil, "version", version, "root", root, "data", dataDir)
		if hs.TLSConfig != nil {
			errc <- hs.ListenAndServeTLS("", "")
		} else {
			errc <- hs.ListenAndServe()
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// healthcheck is what the image's HEALTHCHECK runs, so the image needs no
// curl or wget.
func healthcheck() int {
	_, port, err := net.SplitHostPort(listenAddr())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	scheme := "http"
	c := http.Client{Timeout: 3 * time.Second}
	if tlsEnabled() {
		// Our own process on loopback: the certificate is self-signed, and
		// there is nothing to verify it against.
		scheme = "https"
		c.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
	}
	resp, err := c.Get(scheme + "://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "status", resp.Status)
		return 1
	}
	return 0
}

func ifaces() int {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, e := range entries {
		fmt.Println(e.Name())
	}
	return 0
}

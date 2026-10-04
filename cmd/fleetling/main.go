// Command fleetling runs the Fleetling web app.
//
//	fleetling              serve the web UI (default)
//	fleetling healthcheck  exit 0 if the local server answers /healthz
//	fleetling version      print the version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

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
		case "serve":
		default:
			fmt.Fprintf(os.Stderr, "usage: fleetling [serve|healthcheck|version]\n")
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
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", hs.Addr, "version", version, "root", root, "data", dataDir)
		errc <- hs.ListenAndServe()
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
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
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

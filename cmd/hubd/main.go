// Command hubd is the browser fleet controller.
//
// One static binary hosts all three planes: the agent gateway that browsers
// dial in to, the northbound Control API that automations drive, and the
// operator console. See ARCHITECTURE.md.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is stamped by the release build with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	addr     string
	dataDir  string
	tlsCert  string
	tlsKey   string
	logLevel string
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func parseConfig(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("hubd", flag.ContinueOnError)
	fs.StringVar(&cfg.addr, "addr", envOr("HUB_ADDR", ":8080"), "listen address")
	fs.StringVar(&cfg.dataDir, "data", envOr("HUB_DATA", "./data"), "data directory (SQLite database and blob store)")
	fs.StringVar(&cfg.tlsCert, "tls-cert", envOr("HUB_TLS_CERT", ""), "TLS certificate file (optional; prefer terminating TLS at a proxy)")
	fs.StringVar(&cfg.tlsKey, "tls-key", envOr("HUB_TLS_KEY", ""), "TLS key file")
	fs.StringVar(&cfg.logLevel, "log-level", envOr("HUB_LOG_LEVEL", "info"), "debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if (cfg.tlsCert == "") != (cfg.tlsKey == "") {
		return cfg, errors.New("-tls-cert and -tls-key must be given together")
	}
	return cfg, nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hubd:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(cfg.logLevel)}))
	slog.SetDefault(log)

	if err := os.MkdirAll(cfg.dataDir, 0o755); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(version + "\n"))
	})

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		// The command channel is a hand-rolled RFC 6455 upgrade, which needs
		// http.Hijacker. Hijacker does not exist under HTTP/2, and crypto/tls
		// negotiates h2 automatically whenever ListenAndServeTLS is used. An
		// empty (non-nil) TLSNextProto disables that negotiation and pins the
		// connection to HTTP/1.1 so the upgrade stays possible.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("hubd listening", "addr", cfg.addr, "version", version, "tls", cfg.tlsCert != "")
		var err error
		if cfg.tlsCert != "" {
			err = srv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

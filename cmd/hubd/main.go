// Command hubd is the browser fleet controller.
//
// One static binary hosts all three planes: the agent gateway that browsers
// dial in to, the northbound Control API that automations drive, and the
// operator console. See ARCHITECTURE.md.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tayyebi/scraper/internal/adapters/agentws"
	"github.com/tayyebi/scraper/internal/adapters/console"
	"github.com/tayyebi/scraper/internal/adapters/controlhttp"
	"github.com/tayyebi/scraper/internal/auth"
	"github.com/tayyebi/scraper/internal/bus"
	"github.com/tayyebi/scraper/internal/core"
	"github.com/tayyebi/scraper/internal/mirror"
	"github.com/tayyebi/scraper/internal/registry"
	"github.com/tayyebi/scraper/internal/store/blob"
	"github.com/tayyebi/scraper/internal/store/sqlite"
)

// version is stamped by the release build with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	addr        string
	dataDir     string
	tlsCert     string
	tlsKey      string
	logLevel    string
	consoleUser string
	consoleHash string

	retentionAge   time.Duration
	maxBlobBytes   int64
	maxArtifact    int64
	sweepInterval  time.Duration
	secureCookies  bool
	printVersionEx bool
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
	fs.StringVar(&cfg.consoleUser, "console-user", envOr("HUB_CONSOLE_USER", ""), "operator console username")
	fs.StringVar(&cfg.consoleHash, "console-password-hash", envOr("HUB_CONSOLE_PASSWORD_HASH", ""), "console password hash from `hubd hash-password`")
	fs.DurationVar(&cfg.retentionAge, "retention", 14*24*time.Hour, "drop events, commands and exchanges older than this (0 disables)")
	fs.Int64Var(&cfg.maxBlobBytes, "max-blob-bytes", 8<<30, "total artifact storage cap (0 disables)")
	fs.Int64Var(&cfg.maxArtifact, "max-artifact-bytes", agentws.DefaultMaxArtifactBytes, "per-body upload cap")
	fs.DurationVar(&cfg.sweepInterval, "sweep-interval", time.Hour, "how often retention runs")
	fs.BoolVar(&cfg.printVersionEx, "version", false, "print the version and exit")

	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "hubd - browser fleet controller\n\nUsage:\n  hubd [flags]\n  hubd hash-password\n\nFlags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if (cfg.tlsCert == "") != (cfg.tlsKey == "") {
		return cfg, errors.New("-tls-cert and -tls-key must be given together")
	}
	// A Secure cookie is silently dropped over plain HTTP, which presents as
	// "signing in does nothing" -- so it follows TLS rather than being a flag
	// somebody has to know to set.
	cfg.secureCookies = cfg.tlsCert != ""
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
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		if err := hashPassword(); err != nil {
			fmt.Fprintln(os.Stderr, "hubd:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hubd:", err)
		os.Exit(1)
	}
}

// hashPassword turns a password into the value for -console-password-hash.
//
// It reads from stdin rather than a flag because a password on a command line
// lands in shell history and in the process table, where anyone on the machine
// can read it.
func hashPassword() error {
	fmt.Fprint(os.Stderr, "Password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("reading password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return errors.New("the password is empty")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr)
	fmt.Println(hash)
	return nil
}

func run(args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}
	if cfg.printVersionEx {
		fmt.Println(version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(cfg.logLevel)}))
	slog.SetDefault(log)

	if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}

	store, err := sqlite.Open(filepath.Join(cfg.dataDir, "hub.db"))
	if err != nil {
		return fmt.Errorf("opening the database: %w", err)
	}
	defer store.Close()

	blobs, err := blob.New(cfg.dataDir)
	if err != nil {
		return fmt.Errorf("opening the blob store: %w", err)
	}

	events := bus.New()
	defer events.Close()

	mirrors := mirror.NewManager(0)

	fleet := registry.New(registry.Options{
		Store:     store,
		Blobs:     blobs,
		Bus:       events,
		Documents: mirrors,
		Logger:    log,
	})

	authSvc := auth.New(auth.Options{
		Store:               store,
		ConsoleUser:         cfg.consoleUser,
		ConsolePasswordHash: cfg.consoleHash,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := bootstrapAdminKey(ctx, authSvc, log); err != nil {
		return err
	}

	mux := http.NewServeMux()

	agentPlane := agentws.New(agentws.Options{
		Registry:         fleet,
		Auth:             authSvc,
		Store:            store,
		Blobs:            blobs,
		Mirrors:          mirrors,
		Logger:           log,
		MaxArtifactBytes: cfg.maxArtifact,
	})
	agentPlane.Routes(mux)

	controlPlane := controlhttp.New(controlhttp.Options{
		Registry: fleet,
		Store:    store,
		Blobs:    blobs,
		Auth:     authSvc,
		Logger:   log,
		Version:  version,
	})
	controlPlane.Routes(mux)

	operatorConsole, err := console.New(console.Options{
		Auth:          authSvc,
		Logger:        log,
		SecureCookies: cfg.secureCookies,
	})
	if err != nil {
		return fmt.Errorf("preparing the console: %w", err)
	}
	operatorConsole.Routes(mux)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	go sweep(ctx, store, blobs, cfg, log)

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

	errCh := make(chan error, 1)
	go func() {
		log.Info("hubd listening",
			"addr", cfg.addr, "version", version, "tls", cfg.tlsCert != "",
			"console", authSvc.ConsoleEnabled())
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

// bootstrapAdminKey mints a first admin key when the hub has none.
//
// Without this, a fresh hub is unusable: every Control API endpoint needs a
// key, and the only way to mint one is a Control API endpoint. The key is
// printed once, to stderr, and never recoverable afterwards.
func bootstrapAdminKey(ctx context.Context, authSvc *auth.Service, log *slog.Logger) error {
	keys, err := authSvc.ListAPIKeys(ctx)
	if err != nil {
		return fmt.Errorf("listing API keys: %w", err)
	}
	for _, k := range keys {
		if k.RevokedAt == nil {
			return nil
		}
	}

	_, secret, err := authSvc.MintAPIKey(ctx, "bootstrap admin", core.ScopeAdmin)
	if err != nil {
		return fmt.Errorf("minting the first admin key: %w", err)
	}

	fmt.Fprintf(os.Stderr, `
────────────────────────────────────────────────────────────────────────
This hub had no API keys, so one was created for you:

    %s

It has the admin scope. It is shown once and is not recoverable -- store
it now. Revoke it from the console once you have minted your own.
────────────────────────────────────────────────────────────────────────

`, secret)
	log.Info("bootstrap admin key created")
	return nil
}

// sweep enforces retention on a timer.
//
// Order matters: the database gives up the digests nothing references any more,
// and only then does the blob store delete files. Doing it the other way would
// leave request-log rows pointing at artifacts that no longer exist.
func sweep(ctx context.Context, store *sqlite.Store, blobs *blob.Store, cfg config, log *slog.Logger) {
	if cfg.sweepInterval <= 0 {
		return
	}
	ticker := time.NewTicker(cfg.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if cfg.retentionAge > 0 {
			unreferenced, err := store.Retention(ctx, time.Now().Add(-cfg.retentionAge))
			if err != nil {
				log.Error("retention sweep", "err", err)
				continue
			}
			for _, digest := range unreferenced {
				if err := blobs.Delete(ctx, digest); err != nil {
					log.Warn("deleting an unreferenced artifact", "digest", digest, "err", err)
				}
			}
			if len(unreferenced) > 0 {
				log.Info("retention removed artifacts", "count", len(unreferenced))
			}
		}

		if cfg.maxBlobBytes > 0 {
			removed, err := blobs.Sweep(ctx, 0, cfg.maxBlobBytes, func(digest string) bool {
				return store.Referenced(ctx, digest)
			})
			if err != nil {
				log.Error("blob sweep", "err", err)
			} else if removed > 0 {
				log.Info("blob store trimmed to size", "removed", removed)
			}
		}
	}
}

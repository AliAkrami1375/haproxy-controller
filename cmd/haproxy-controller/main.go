// Command haproxy-controller runs the HAProxy control panel.
//
// Powered by Ebdaa.me - https://ebdaa.me
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ebdaa/haproxy-controller/internal/config"
	"github.com/ebdaa/haproxy-controller/internal/db"
	"github.com/ebdaa/haproxy-controller/internal/hap"
	"github.com/ebdaa/haproxy-controller/internal/store"
	"github.com/ebdaa/haproxy-controller/internal/web"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "haproxy-controller: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Handle -version and -healthcheck before the full config load: both must
	// work without a writable data directory, and the health check must not
	// touch the database the running instance owns.
	for _, a := range os.Args[1:] {
		switch a {
		case "-version", "--version":
			fmt.Printf("haproxy-controller %s\n", version)
			return nil
		case "-healthcheck", "--healthcheck":
			return healthcheck()
		}
	}

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("starting haproxy-controller",
		"version", version, "config", cfg.Path(), "data_dir", cfg.DataDir)

	// ---- storage
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	st := store.New(database)

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A container has no syslog socket, so seed the global section to log to
	// stdout where `docker logs` will pick it up.
	bootOpts := store.BootstrapOptions{}
	if cfg.ProcessMode == config.ProcessModeSupervised {
		bootOpts.LogTarget = "stdout format raw local0"
	}
	if err := st.Bootstrap(ctx, bootOpts); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	// ---- first-run administrator
	adminUser := envOr("HC_ADMIN_USER", "admin")
	if password, err := st.EnsureAdmin(ctx, adminUser); err != nil {
		return fmt.Errorf("create initial administrator: %w", err)
	} else if password != "" {
		printFirstRunCredentials(cfg, adminUser, password)
		logger.Warn("created initial administrator account", "username", adminUser)
	}

	// ---- HAProxy integration
	validator := &hap.Validator{Binary: cfg.HAProxyBin}
	runtime := &hap.Runtime{Socket: cfg.RuntimeSocket}
	renderer := hap.NewRenderer(st, hap.Paths{
		ConfigPath:    cfg.ConfigPath,
		ErrorPagesDir: cfg.ErrorPagesDir,
		CertsDir:      cfg.CertsDir,
		MapsDir:       cfg.MapsDir,
	})
	// How HAProxy is driven depends on where the controller runs: through an
	// init system on a host, or as a direct child inside a container.
	var (
		process    hap.ProcessManager
		supervisor *hap.Supervisor
	)
	if cfg.ProcessMode == config.ProcessModeSupervised {
		supervisor = hap.NewSupervisor(cfg.HAProxyBin, cfg.ConfigPath,
			cfg.PIDFile, cfg.MasterSocket, logger)
		process = supervisor
		logger.Info("supervising haproxy directly", "binary", cfg.HAProxyBin)
	} else {
		process = &hap.CommandManager{
			ReloadCommand:  cfg.ReloadCommand,
			RestartCommand: cfg.RestartCommand,
			StatusCommand:  cfg.StatusCommand,
		}
	}

	deployer := &hap.Deployer{
		Store:         st,
		Renderer:      renderer,
		Validator:     validator,
		ConfigPath:    cfg.ConfigPath,
		ErrorPagesDir: cfg.ErrorPagesDir,
		CertsDir:      cfg.CertsDir,
		BackupDir:     cfg.BackupDir,
		Process:       process,
	}

	if v, err := validator.Version(ctx); err != nil {
		logger.Warn("haproxy binary is not usable yet",
			"path", cfg.HAProxyBin, "error", err,
			"hint", "run scripts/build-haproxy.sh, or set haproxy_bin under Settings")
	} else {
		logger.Info("detected haproxy", "version", v)
	}

	// ---- control panel
	srv, err := web.NewServer(web.Options{
		Config:    cfg,
		Store:     st,
		Deployer:  deployer,
		Runtime:   runtime,
		Validator: validator,
		Logger:    logger,
	})
	if err != nil {
		return fmt.Errorf("build control panel: %w", err)
	}

	go housekeeping(ctx, st, logger)

	// Bring HAProxy up with whatever configuration was last applied. A fresh
	// install has no frontends yet, so there is nothing to start until the
	// first apply; that is expected, not an error.
	if supervisor != nil {
		if err := supervisor.Start(ctx); err != nil {
			logger.Warn("haproxy is not running yet", "reason", err,
				"hint", "add a frontend and apply the configuration from the panel")
		}
	}

	// stopHAProxy drains HAProxy before the process exits. It runs on both the
	// clean and the failed path, so a container stop never orphans a worker.
	stopHAProxy := func() {
		if supervisor == nil {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := supervisor.Shutdown(stopCtx); err != nil {
			logger.Error("stopping haproxy", "error", err)
		}
	}

	if err := srv.Run(ctx); err != nil {
		stopHAProxy()
		return fmt.Errorf("control panel: %w", err)
	}
	stopHAProxy()
	logger.Info("shut down cleanly")
	return nil
}

// healthcheck probes the panel over the loopback interface. It is what the
// container HEALTHCHECK runs, so it must stay dependency-free and fast.
func healthcheck() error {
	port := strings.TrimSpace(os.Getenv("HC_LISTEN_PORT"))
	if port == "" {
		port = strconv.Itoa(config.DefaultControlPort)
	}
	scheme := "http"
	if isTruthy(os.Getenv("HC_TLS_ENABLED")) {
		scheme = "https"
	}

	client := &http.Client{
		Timeout: 4 * time.Second,
		Transport: &http.Transport{
			// The panel may serve a self-signed certificate; this probe only
			// needs to know the process answers, not who it claims to be.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get(scheme + "://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// housekeeping periodically drops expired sessions and trims the audit log.
func housekeeping(ctx context.Context, st *store.Store, logger *slog.Logger) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	purge := func() {
		if n, err := st.PurgeExpiredSessions(ctx); err != nil {
			logger.Error("purge sessions", "error", err)
		} else if n > 0 {
			logger.Debug("purged expired sessions", "count", n)
		}
		if err := st.TrimAudit(ctx, 20000); err != nil {
			logger.Error("trim audit log", "error", err)
		}
	}
	purge()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge()
		}
	}
}

// printFirstRunCredentials writes the generated administrator password to
// stderr. It is shown once and never stored in plaintext.
func printFirstRunCredentials(cfg *config.Config, username, password string) {
	scheme := "http"
	if enabled, _, _ := cfg.TLS(); enabled {
		scheme = "https"
	}
	bar := strings.Repeat("=", 68)
	fmt.Fprintf(os.Stderr, `
%s
  HAProxy Controller - initial administrator account
%s
  URL       : %s://<host>:%d/
  Username  : %s
  Password  : %s

  This password is shown once only. Sign in and change it immediately.
  Powered by %s - %s
%s

`, bar, bar, scheme, cfg.ListenPort, username, password, store.BrandName, store.BrandURL, bar)
}

// newLogger builds the structured logger at the configured level.
func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// Command github-api-proxy is a transparent reverse proxy for the GitHub REST
// and GraphQL APIs. Point any client's base URL at it instead of
// https://api.github.com; every call is forwarded unchanged and recorded in
// MySQL (who, when, what, how it went, rate limits), with a stats API for
// Projects Monitor and operational events sent to LogStream. Settings and
// secrets come from Grimoire; only allow-listed networks may call it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/guibranco/github-api-proxy/internal/api"
	"github.com/guibranco/github-api-proxy/internal/config"
	"github.com/guibranco/github-api-proxy/internal/grimoire"
	"github.com/guibranco/github-api-proxy/internal/identity"
	"github.com/guibranco/github-api-proxy/internal/logstream"
	"github.com/guibranco/github-api-proxy/internal/network"
	"github.com/guibranco/github-api-proxy/internal/proxy"
	"github.com/guibranco/github-api-proxy/internal/store"
	"github.com/guibranco/github-api-proxy/internal/systemd"
)

// version is the GitVersion semVer, set at build time: -ldflags "-X main.version=1.2.3".
// It is also the LogStream app_id unless LogStream:AppId overrides it.
var version = "0.0.0-dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println(version)
		return
	}
	os.Exit(run())
}

func run() (code int) {
	started := time.Now()
	userAgent := fmt.Sprintf("github-api-proxy/%s (Go %s; %s/%s)", version, runtime.Version()[2:], runtime.GOOS, runtime.GOARCH)
	boot := slog.New(logstream.NewHandler(nil, "info"))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	bootstrap := config.BootstrapFromEnv()
	cfg, gm, err := config.Load(ctx, bootstrap, version, userAgent)
	if err != nil {
		boot.Error("Invalid configuration, refusing to start", "category", "startup", "error", err)
		return 2
	}

	ls := logstream.New(cfg.LogStream, userAgent)
	log := slog.New(logstream.NewHandler(ls, cfg.LogStream.MinLevel))
	slog.SetDefault(log)
	defer func() {
		if r := recover(); r != nil {
			log.Error("github-api-proxy crashed", "category", "fatal", "panic", fmt.Sprint(r))
			code = 1
		}
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ls.Close(fctx)
	}()

	bg, cancelBG := context.WithCancel(context.Background())
	defer cancelBG()

	logStreamState := "disabled: " + cfg.LogStream.DisableReason
	if ls != nil {
		logStreamState = ls.Transport()
	}
	log.Info("github-api-proxy starting", "category", "startup",
		"version", version, "go", runtime.Version(), "pid", os.Getpid(), "settings", cfg.Origin,
		"listen", cfg.ListenAddr, "admin_listen", cfg.AdminListenAddr, "upstream", cfg.UpstreamURL.String(),
		"retention_days", cfg.RetentionDays, "resolve_identities", cfg.ResolveIdentity,
		"logstream", logStreamState, "logstream_app_id", cfg.LogStream.AppID)

	// ── Network allow-list ────────────────────────────────────────────────
	// Grimoire's value is re-read only when a sentinel says changes were
	// published; the timer below only re-resolves host names (dynamic DNS).
	sources := newAllowListSource(cfg, gm)
	allowList := network.New(sources.get, nil, log)
	var watcher *grimoire.Watcher
	if sources.live {
		watcher = grimoire.NewWatcher(gm, nil, cfg.GrimoireRefreshInterval, func(ctx context.Context) error {
			if err := sources.fetch(ctx); err != nil {
				return err
			}
			return allowList.Refresh(ctx)
		}, log)
		// Baseline first, then the value: a publish in between is seen on the first check.
		if err := watcher.Prime(ctx); err != nil {
			log.Warn("Could not read Grimoire's sentinels, the first check will reload the allow-list",
				"category", "startup", "error", err)
		}
		if err := sources.fetch(ctx); err != nil {
			log.Error("Cannot read the network allow-list from Grimoire, refusing to start", "category", "startup", "error", err)
			return 2
		}
	}
	if err := allowList.Refresh(ctx); err != nil {
		log.Error("Cannot load the network allow-list, refusing to start", "category", "startup", "error", err)
		return 2
	}
	go allowList.Run(bg, cfg.RefreshInterval)
	if watcher != nil {
		go watcher.Run(bg)
	}

	// ── Storage ───────────────────────────────────────────────────────────
	db, err := store.Open(cfg.MySQLDSN, cfg.LogQueueSize, cfg.LogBatchSize, cfg.LogFlushInterval, cfg.RetentionDays, log)
	if err != nil {
		log.Error("Cannot initialise MySQL", "category", "startup", "error", err)
		return 1
	}
	// MySQL being unavailable must not stop GitHub traffic: migrate in the
	// background and keep retrying; records queue up (bounded) meanwhile.
	go func() {
		for attempt := 1; ; attempt++ {
			mctx, cancel := context.WithTimeout(bg, time.Minute)
			err := db.Migrate(mctx)
			cancel()
			if err == nil {
				log.Info("Database ready", "category", "startup")
				go db.RunRetention(bg)
				return
			}
			if bg.Err() != nil {
				return
			}
			log.Error("Database migration failed, retrying in 30s", "category", "startup", "attempt", attempt, "error", err)
			select {
			case <-bg.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()
	go db.Run(context.Background())

	resolver := identity.NewResolver(cfg.UpstreamURL, userAgent, db, cfg.ResolveIdentity, log)
	resolver.Start(bg, 2)

	// ── HTTP servers ──────────────────────────────────────────────────────
	clientIP := func(r *http.Request) string { return proxy.ClientIP(r, cfg.TrustedProxies) }
	denied := proxy.NewDeniedRecorder(db, log)

	proxyHandler := allowList.Middleware(proxy.New(cfg, db, resolver, log), clientIP, nil, denied.Record)
	adminHandler := allowList.Middleware(api.New(db, cfg.AdminAPIToken, version, started, allowList, log), clientIP,
		func(r *http.Request) bool { return cfg.PublicHealth && r.URL.Path == "/health" }, denied.Log)

	httpLog := slog.NewLogLogger(log.With("category", "http").Handler(), slog.LevelWarn)
	proxyServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           proxyHandler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: archive and log downloads can legitimately take long.
		ErrorLog: httpLog,
	}
	adminServer := &http.Server{
		Addr:              cfg.AdminListenAddr,
		Handler:           adminHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          httpLog,
	}

	errc := make(chan error, 2)
	for _, srv := range []*http.Server{proxyServer, adminServer} {
		go func(s *http.Server) {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", s.Addr, err)
			}
		}(srv)
	}

	// Give the listeners a moment to fail on a busy port before reporting ready.
	select {
	case err := <-errc:
		log.Error("HTTP server failed to start", "category", "startup", "error", err)
		return 1
	case <-time.After(200 * time.Millisecond):
	}
	_ = systemd.Notify("READY=1")
	log.Info("github-api-proxy started", "category", "startup", "version", version,
		"startup_ms", time.Since(started).Milliseconds())

	select {
	case <-ctx.Done():
		log.Info("Shutdown signal received, draining connections", "category", "shutdown")
	case err := <-errc:
		log.Error("HTTP server failed, shutting down", "category", "shutdown", "error", err)
		code = 1
	}
	_ = systemd.Notify("STOPPING=1")

	sctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	for _, srv := range []*http.Server{proxyServer, adminServer} {
		if err := srv.Shutdown(sctx); err != nil {
			log.Warn("Graceful shutdown timed out", "category", "shutdown", "server", srv.Addr, "error", err)
		}
	}
	cancelBG()
	db.Close(sctx)
	log.Info("github-api-proxy stopped", "category", "shutdown",
		"uptime", time.Since(started).Round(time.Second).String(), "records_dropped", db.Stats().Dropped)
	return code
}

// allowListSource holds the raw Network:AllowedSources. When it comes from
// Grimoire (and the environment doesn't pin it), the sentinel watcher re-reads
// it after each publish, so a new machine or a changed home IP applies without
// a restart; the allow-list's own timer only re-resolves host names.
type allowListSource struct {
	gm   *grimoire.Client
	live bool

	mu  sync.RWMutex
	raw string
}

func newAllowListSource(cfg *config.Config, gm *grimoire.Client) *allowListSource {
	_, pinned := os.LookupEnv("Network__AllowedSources")
	_, pinnedUpper := os.LookupEnv("NETWORK__ALLOWEDSOURCES")
	return &allowListSource{gm: gm, live: gm != nil && !pinned && !pinnedUpper, raw: cfg.AllowedSources}
}

func (s *allowListSource) get(context.Context) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.raw, nil
}

// fetch re-reads the value from Grimoire; on error the previous value stays.
func (s *allowListSource) fetch(ctx context.Context) error {
	v, err := s.gm.Configuration(ctx, config.KeyAllowedSources)
	if errors.Is(err, grimoire.ErrNotFound) {
		return fmt.Errorf("%s was removed from Grimoire", config.KeyAllowedSources)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.raw = v
	s.mu.Unlock()
	return nil
}

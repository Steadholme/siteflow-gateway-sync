// Command siteflow-gateway-sync reconciles the set of publicly reachable
// SiteFlow deployment hosts into the Holdfast gateway (Sluice) routes table so
// deployed sites become reachable from the public internet.
//
// It runs a reconcile loop (default every 30s), reading host sources from the
// read-only SiteFlow database and upserting/pruning only the sfsite- namespace of
// the read-write Holdfast routes database. A /healthz endpoint is served on
// BIND_ADDR for container health checks.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/holdfast/siteflow-gateway-sync/internal/sfsync"
)

const (
	defaultBindAddr = "0.0.0.0:9385"
	defaultInterval = 30 * time.Second
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz on BIND_ADDR and exit 0 (healthy) or 1")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := loadConfig()
	if err != nil {
		log.Error("load config", "error", err)
		os.Exit(1)
	}

	// Graceful shutdown: SIGINT/SIGTERM cancel the root context, which stops the
	// sync loop and triggers pool/server teardown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The SiteFlow pool is opened read-only so this service can never write to
	// SiteFlow even by a bug; the routes pool is read-write (sfsite- namespace only).
	sfPool, err := newReadOnlyPool(ctx, cfg.siteflowDSN)
	if err != nil {
		log.Error("connect siteflow database (read-only)", "error", err)
		os.Exit(1)
	}
	defer sfPool.Close()

	routesPool, err := newPool(ctx, cfg.routesDSN)
	if err != nil {
		log.Error("connect routes database", "error", err)
		os.Exit(1)
	}
	defer routesPool.Close()

	syncer := sfsync.NewSyncer(
		sfsync.NewSiteflowDB(sfPool),
		sfsync.NewRoutesDB(routesPool),
		cfg.upstream,
		log,
	)

	srv := &http.Server{
		Addr:              cfg.bindAddr,
		Handler:           healthzHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("healthz listening", "addr", cfg.bindAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("healthz server stopped", "error", err)
			stop() // a dead health server takes the process down for the orchestrator
		}
	}()

	log.Info("siteflow-gateway-sync started",
		"bind", cfg.bindAddr,
		"interval", cfg.interval.String(),
		"upstream", cfg.upstream,
	)

	runLoop(ctx, syncer, cfg.interval, log)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("healthz server shutdown", "error", err)
	}
	log.Info("siteflow-gateway-sync stopped")
}

// runLoop runs one reconcile immediately, then every interval, until ctx is done.
func runLoop(ctx context.Context, syncer *sfsync.Syncer, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		// Bound each round so a hung database cannot stall the loop past one interval.
		roundCtx, cancel := context.WithTimeout(ctx, interval)
		st, err := syncer.RunOnce(roundCtx)
		cancel()
		if err != nil {
			log.Error("sync round failed", "error", err)
		} else {
			log.Info("sync round complete",
				"hosts", st.Hosts,
				"upserted", st.Upserted,
				"pruned", st.Pruned,
				"prune_skipped", st.PruneSkipped,
			)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// config holds the resolved runtime configuration from the environment.
type config struct {
	bindAddr    string
	siteflowDSN string
	routesDSN   string
	upstream    string
	interval    time.Duration
}

// loadConfig resolves configuration from the environment, applying defaults and
// validating the two required DSNs.
func loadConfig() (config, error) {
	c := config{
		bindAddr:    getenv("BIND_ADDR", defaultBindAddr),
		siteflowDSN: os.Getenv("SITEFLOW_DATABASE_URL"),
		routesDSN:   os.Getenv("ROUTES_DATABASE_URL"),
		upstream:    getenv("SITEFLOW_UPSTREAM", sfsync.DefaultUpstream),
		interval:    defaultInterval,
	}
	if v := os.Getenv("SYNC_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, fmt.Errorf("parse SYNC_INTERVAL %q: %w", v, err)
		}
		if d <= 0 {
			return config{}, fmt.Errorf("SYNC_INTERVAL must be positive, got %q", v)
		}
		c.interval = d
	}
	if c.siteflowDSN == "" {
		return config{}, fmt.Errorf("SITEFLOW_DATABASE_URL is required")
	}
	if c.routesDSN == "" {
		return config{}, fmt.Errorf("ROUTES_DATABASE_URL is required")
	}
	return c, nil
}

// newPool opens a pgx connection pool and verifies it with a ping.
func newPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(connectCtx, dsn)
	if err != nil {
		return nil, fmt.Errorf("new pool: %w", err)
	}
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// newReadOnlyPool opens a pool whose every session is read-only
// (default_transaction_read_only=on sent in the startup packet), so the SiteFlow
// side cannot be mutated by this service under any code path.
func newReadOnlyPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"

	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(connectCtx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("new pool: %w", err)
	}
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// healthzHandler serves a minimal 200 OK liveness endpoint.
func healthzHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// runHealthcheck performs an in-container liveness probe of /healthz. It is the
// entrypoint for the Docker HEALTHCHECK on a scratch runtime with no shell.
func runHealthcheck() int {
	addr := getenv("BIND_ADDR", defaultBindAddr)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: bad BIND_ADDR %q: %v\n", addr, err)
		return 1
	}
	// A bind host of 0.0.0.0 / :: / empty is not dialable; probe loopback.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: GET %s: %v\n", url, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s -> %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}

// getenv returns the environment value for key, or def when unset/empty.
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

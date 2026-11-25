// Command tollgate is the gateway binary.
//
//	tollgate serve                              run the gateway (default)
//	tollgate migrate                            apply database migrations
//	tollgate keys create --project P [--name N] [--rpm N] [--tpm N]
//	tollgate keys revoke --prefix tg_XXXXXXXX
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/krishbansalofficial/tollgate/internal/auth"
	"github.com/krishbansalofficial/tollgate/internal/cache"
	"github.com/krishbansalofficial/tollgate/internal/config"
	"github.com/krishbansalofficial/tollgate/internal/providers"
	"github.com/krishbansalofficial/tollgate/internal/ratelimit"
	"github.com/krishbansalofficial/tollgate/internal/router"
	"github.com/krishbansalofficial/tollgate/internal/server"
	"github.com/krishbansalofficial/tollgate/internal/telemetry"
	"github.com/krishbansalofficial/tollgate/internal/usage"
	"github.com/krishbansalofficial/tollgate/migrations"
)

// version is set at build time: -ldflags "-X main.version=<git sha>".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tollgate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := slog.New(telemetry.TraceHandler{Handler: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})}).With("version", version)

	// SIGTERM is what Kubernetes sends; SIGINT is Ctrl-C locally.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	switch cmd {
	case "serve":
		return serve(ctx, cfg, log, pool)
	case "migrate":
		return migrate(ctx, pool)
	case "keys":
		return keys(ctx, auth.NewPGStore(pool), args)
	default:
		return fmt.Errorf("unknown command %q (serve|migrate|keys)", cmd)
	}
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger, pool *pgxpool.Pool) error {
	catalog, err := router.LoadCatalog(cfg.CatalogPath)
	if err != nil {
		return err
	}
	shutdownTracing, err := telemetry.SetupTracing(ctx, cfg.OTLPEndpoint, cfg.TraceSampleRatio, version)
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	metrics := telemetry.NewMetrics()
	ledger := usage.NewWriter(usage.NewPGSink(pool), cfg.UsageBuffer, log)
	metrics.UsageGauges(ledger.Dropped, ledger.Written, ledger.Buffered)
	ledger.Start()
	provs := buildProviders(cfg)
	if len(provs) == 0 {
		return errors.New("no providers configured: set at least one provider API key or TOLLGATE_MOCK_BASE_URL")
	}
	if f := cfg.FallbackModel; f != "" {
		if _, priced := catalog.Models[f]; !priced {
			return fmt.Errorf("TOLLGATE_FALLBACK_MODEL %q is not in the catalog", f)
		}
	}
	rcfg := router.DefaultConfig()
	rcfg.MaxAttempts, rcfg.AttemptTimeout, rcfg.Fallback = cfg.MaxAttempts, cfg.AttemptTimeout, cfg.FallbackModel
	rt := router.New(rcfg, catalog, provs, func(p string, s router.State) {
		log.Warn("circuit breaker state change", "provider", p, "state", s.String())
		metrics.BreakerChanged(p, s)
	})

	var limiter *ratelimit.Limiter
	var respCache *cache.Cache
	if cfg.RedisURL != "" {
		opts, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("TOLLGATE_REDIS_URL: %w", err)
		}
		rdb := redis.NewClient(opts)
		defer func() { _ = rdb.Close() }()
		limiter = ratelimit.New(rdb, cfg.RateLimitFailOpen)
		respCache = cache.New(rdb, cfg.CacheTTL)
	} else {
		log.Warn("TOLLGATE_REDIS_URL not set: rate limiting and caching are DISABLED")
	}

	var draining atomic.Bool
	handler := server.New(server.Deps{
		Auth:    auth.NewAuthenticator(auth.NewPGStore(pool), cfg.KeyCacheTTL),
		Limiter: limiter,
		Cache:   respCache,
		Router:  rt,
		// Fan out each request's Outcome to metrics and the ledger. Both are
		// non-blocking (counter increments; a non-blocking channel send).
		Observer: server.ObserverFunc(func(o server.Outcome) {
			metrics.Observe(o)
			ledger.Record(toEvent(o))
		}),
		Ready: func(ctx context.Context) error {
			if draining.Load() {
				return errors.New("shutting down")
			}
			// Redis is deliberately not checked: with fail-open, a Redis outage
			// should degrade the gateway, not remove every pod from service.
			return pool.Ping(ctx)
		},
		Logger:           log,
		UpstreamTimeout:  cfg.UpstreamTimeout,
		DefaultMaxTokens: cfg.DefaultMaxTokens,
	})

	// otelhttp is the outermost layer so the root span (and its trace ID,
	// used as the request ID) exists before any other middleware runs.
	// Probes are filtered out: they'd be most of the traces and none of the value.
	root := otelhttp.NewHandler(metrics.Middleware(handler), "gateway",
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" && r.URL.Path != "/readyz" }),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + r.URL.Path }))
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second, // slowloris guard
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: it caps the whole response, which would kill long
		// streams. Per-write deadlines via http.ResponseController instead.
	}
	// Metrics on their own port: Prometheus scrapes it in-cluster, and the
	// ingress only routes the API port, so /metrics is never public.
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	msrv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := msrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server", "err", err)
		}
	}()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "providers", len(provs), "fallback", cfg.FallbackModel)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err // failed to bind, etc.
	case <-ctx.Done():
	}

	// Graceful shutdown, in order:
	// 1. Fail readiness so Kubernetes removes the pod from Service endpoints.
	// 2. Keep serving for ShutdownDelay: endpoint removal propagates to
	//    kube-proxy/ingress asynchronously, so for a few seconds new requests
	//    still arrive. Closing the listener immediately would refuse them.
	// 3. Shutdown: stop accepting, wait for in-flight requests to finish.
	// 4. Only then close the ledger, so every finished request's row is
	//    buffered before the final flush; then flush spans; metrics last so
	//    a final scrape can still see the shutdown.
	log.Info("shutdown: draining", "delay", cfg.ShutdownDelay)
	draining.Store(true)
	time.Sleep(cfg.ShutdownDelay)

	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	var errs []error
	if err := srv.Shutdown(sctx); err != nil {
		errs = append(errs, fmt.Errorf("http shutdown: %w", err))
	}
	if err := ledger.Close(sctx); err != nil {
		errs = append(errs, fmt.Errorf("usage flush: %w", err))
	}
	if err := shutdownTracing(sctx); err != nil {
		errs = append(errs, fmt.Errorf("trace flush: %w", err))
	}
	_ = msrv.Shutdown(sctx)
	log.Info("shutdown: complete", "usage_written", ledger.Written(), "usage_dropped", ledger.Dropped())
	return errors.Join(errs...)
}

// toEvent converts a request Outcome into a ledger row.
func toEvent(o server.Outcome) usage.Event {
	e := usage.Event{
		TS: time.Now().Add(-o.Latency), RequestID: o.RequestID, ProjectID: o.ProjectID, KeyID: o.KeyID,
		Model: o.Model, Provider: o.Provider, TokensIn: o.TokensIn, TokensOut: o.TokensOut,
		UsageEstimated: o.UsageEstimated, LatencyMS: int(o.Latency.Milliseconds()), UpstreamMS: int(o.Upstream.Milliseconds()),
		CostUSD: o.CostUSD, CostSavedUSD: o.CostSavedUSD, Cache: o.Cache, Status: o.Status,
		ErrorClass: o.ErrorClass, Attempts: o.Attempts, Stream: o.Stream,
	}
	if o.TTFT > 0 {
		ms := int(o.TTFT.Milliseconds())
		e.TTFTMS = &ms
	}
	if e.Provider == "" {
		e.Provider = "none"
	}
	return e
}

// waitForDB pings with capped exponential backoff until Postgres answers.
// A migration Job's first dial can lose two races: Postgres still starting,
// and the NetworkPolicy controller (kube-router in k3s) not yet having added
// the brand-new pod's IP to the allow-list, which REJECTs the connection.
// Failing fast would just burn the Job's retries on identical races.
func waitForDB(ctx context.Context, pool *pgxpool.Pool, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	delay := 250 * time.Millisecond
	for {
		err := pool.Ping(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("database not reachable after %s: %w", limit, err)
		case <-time.After(delay):
		}
		delay = min(2*delay, 5*time.Second)
	}
}

// buildProviders enables each provider whose credentials or URL are set. All
// share one tuned transport per provider (see providers.NewHTTPClient).
func buildProviders(cfg config.Config) map[string]providers.Provider {
	provs := map[string]providers.Provider{}
	if cfg.GroqAPIKey != "" {
		provs["groq"] = providers.NewOpenAICompat("groq", cfg.GroqBaseURL, cfg.GroqAPIKey, providers.NewHTTPClient())
	}
	if cfg.AnthropicAPIKey != "" {
		provs["anthropic"] = providers.NewAnthropic(cfg.AnthropicBaseURL, cfg.AnthropicAPIKey, cfg.DefaultMaxTokens, providers.NewHTTPClient())
	}
	if cfg.OllamaBaseURL != "" {
		provs["ollama"] = providers.NewOpenAICompat("ollama", cfg.OllamaBaseURL, "", providers.NewHTTPClient())
	}
	if cfg.MockBaseURL != "" {
		provs["mock"] = providers.NewOpenAICompat("mock", cfg.MockBaseURL, "", providers.NewHTTPClient())
	}
	return provs
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if err := waitForDB(ctx, pool, 90*time.Second); err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return err
	}
	results, err := p.Up(ctx)
	for _, r := range results {
		fmt.Println(r)
	}
	return err
}

func keys(ctx context.Context, st *auth.PGStore, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: keys create|revoke [flags]")
	}
	fs := flag.NewFlagSet("keys "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "create":
		project := fs.String("project", "", "project name (created if missing)")
		name := fs.String("name", "", "key label")
		rpm := fs.Int("rpm", 0, "requests/min override (0 = project default)")
		tpm := fs.Int("tpm", 0, "tokens/min override (0 = project default)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *project == "" {
			return errors.New("--project is required")
		}
		raw, err := st.CreateKey(ctx, *project, *name, *rpm, *tpm)
		if err != nil {
			return err
		}
		fmt.Println(raw)
		fmt.Fprintln(os.Stderr, "store this key now; it cannot be shown again")
		return nil
	case "revoke":
		prefix := fs.String("prefix", "", "display prefix, e.g. tg_a1B2c3D4")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		n, err := st.RevokeKey(ctx, *prefix)
		if err != nil {
			return err
		}
		fmt.Printf("revoked %d key(s)\n", n)
		return nil
	default:
		return fmt.Errorf("unknown keys subcommand %q", args[0])
	}
}

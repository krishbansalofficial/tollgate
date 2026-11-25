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

	"github.com/krishbansalofficial/tollgate/internal/auth"
	"github.com/krishbansalofficial/tollgate/internal/cache"
	"github.com/krishbansalofficial/tollgate/internal/config"
	"github.com/krishbansalofficial/tollgate/internal/providers"
	"github.com/krishbansalofficial/tollgate/internal/ratelimit"
	"github.com/krishbansalofficial/tollgate/internal/router"
	"github.com/krishbansalofficial/tollgate/internal/server"
	"github.com/krishbansalofficial/tollgate/migrations"
)

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
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

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

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second, // slowloris guard
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: it caps the whole response, which would kill long
		// streams. Per-write deadlines via http.ResponseController instead.
	}
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
	log.Info("shutdown: draining", "delay", cfg.ShutdownDelay)
	draining.Store(true)
	time.Sleep(cfg.ShutdownDelay)

	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("shutdown: complete")
	return nil
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

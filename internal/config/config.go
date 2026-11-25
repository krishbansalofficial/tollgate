// Package config loads Tollgate's configuration from environment variables.
// It is parsed once at startup; invalid values fail fast instead of surfacing
// as confusing behaviour under traffic.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

type Config struct {
	Addr        string // TOLLGATE_ADDR
	MetricsAddr string // TOLLGATE_METRICS_ADDR; separate port, never exposed via ingress
	DatabaseURL string // TOLLGATE_DATABASE_URL (required)
	RedisURL    string // TOLLGATE_REDIS_URL; empty disables rate limiting and caching (dev only)
	CatalogPath string // TOLLGATE_CATALOG; model prices and policy aliases (YAML)

	// Providers: each is enabled only when its key/URL is set.
	GroqAPIKey       string // GROQ_API_KEY
	GroqBaseURL      string // TOLLGATE_GROQ_BASE_URL
	AnthropicAPIKey  string // ANTHROPIC_API_KEY
	AnthropicBaseURL string // TOLLGATE_ANTHROPIC_BASE_URL
	GeminiAPIKey     string // GEMINI_API_KEY
	GeminiBaseURL    string // TOLLGATE_GEMINI_BASE_URL
	OllamaBaseURL    string // TOLLGATE_OLLAMA_BASE_URL (e.g. http://laptop.tailnet:11434/v1)
	MockBaseURL      string // TOLLGATE_MOCK_BASE_URL; enables the "mock" provider (tests, load tests)

	// Routing.
	FallbackModel    string        // TOLLGATE_FALLBACK_MODEL; last resort for every request, e.g. ollama/llama3.2:3b
	MaxAttempts      int           // TOLLGATE_MAX_ATTEMPTS per candidate
	AttemptTimeout   time.Duration // TOLLGATE_ATTEMPT_TIMEOUT
	UpstreamTimeout  time.Duration // TOLLGATE_UPSTREAM_TIMEOUT, whole request incl. fallbacks
	DefaultMaxTokens int           // TOLLGATE_DEFAULT_MAX_TOKENS, when the client sends none

	// Limits and cache.
	RateLimitFailOpen bool          // TOLLGATE_RATELIMIT_FAIL_MODE=open|closed
	CacheTTL          time.Duration // TOLLGATE_CACHE_TTL
	KeyCacheTTL       time.Duration // TOLLGATE_KEY_CACHE_TTL

	// Usage ledger and telemetry.
	UsageBuffer      int     // TOLLGATE_USAGE_BUFFER rows held in memory before dropping
	OTLPEndpoint     string  // OTEL_EXPORTER_OTLP_ENDPOINT (host:port); empty disables tracing
	TraceSampleRatio float64 // TOLLGATE_TRACE_SAMPLE_RATIO (head sampling; the collector tail-samples)

	// Lifecycle.
	ShutdownDelay   time.Duration // TOLLGATE_SHUTDOWN_DELAY
	ShutdownTimeout time.Duration // TOLLGATE_SHUTDOWN_TIMEOUT

	LogLevel slog.Level // TOLLGATE_LOG_LEVEL (debug|info|warn|error)
}

// Load reads configuration through getenv (os.Getenv in production, a map in
// tests) and returns every invalid value at once rather than the first one.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	str := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	dur := func(key string, def time.Duration) time.Duration {
		v := getenv(key)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("%s: invalid duration %q", key, v))
		}
		return d
	}
	integer := func(key string, def, lo int) int {
		v := getenv(key)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo {
			errs = append(errs, fmt.Errorf("%s: want an integer >= %d, got %q", key, lo, v))
		}
		return n
	}

	c := Config{
		Addr:             str("TOLLGATE_ADDR", ":8080"),
		MetricsAddr:      str("TOLLGATE_METRICS_ADDR", ":9090"),
		DatabaseURL:      getenv("TOLLGATE_DATABASE_URL"),
		RedisURL:         getenv("TOLLGATE_REDIS_URL"),
		CatalogPath:      getenv("TOLLGATE_CATALOG"),
		GroqAPIKey:       getenv("GROQ_API_KEY"),
		GroqBaseURL:      str("TOLLGATE_GROQ_BASE_URL", "https://api.groq.com/openai/v1"),
		AnthropicAPIKey:  getenv("ANTHROPIC_API_KEY"),
		AnthropicBaseURL: str("TOLLGATE_ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
		GeminiAPIKey:     getenv("GEMINI_API_KEY"),
		GeminiBaseURL:    str("TOLLGATE_GEMINI_BASE_URL", "https://generativelanguage.googleapis.com/v1beta"),
		OllamaBaseURL:    getenv("TOLLGATE_OLLAMA_BASE_URL"),
		MockBaseURL:      getenv("TOLLGATE_MOCK_BASE_URL"),
		FallbackModel:    getenv("TOLLGATE_FALLBACK_MODEL"),
		MaxAttempts:      integer("TOLLGATE_MAX_ATTEMPTS", 2, 1),
		AttemptTimeout:   dur("TOLLGATE_ATTEMPT_TIMEOUT", 30*time.Second),
		UpstreamTimeout:  dur("TOLLGATE_UPSTREAM_TIMEOUT", 120*time.Second),
		DefaultMaxTokens: integer("TOLLGATE_DEFAULT_MAX_TOKENS", 4096, 1),
		CacheTTL:         dur("TOLLGATE_CACHE_TTL", time.Hour),
		KeyCacheTTL:      dur("TOLLGATE_KEY_CACHE_TTL", 30*time.Second),
		UsageBuffer:      integer("TOLLGATE_USAGE_BUFFER", 10_000, 1),
		OTLPEndpoint:     getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		ShutdownDelay:    dur("TOLLGATE_SHUTDOWN_DELAY", 5*time.Second),
		ShutdownTimeout:  dur("TOLLGATE_SHUTDOWN_TIMEOUT", 20*time.Second),
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("TOLLGATE_DATABASE_URL is required"))
	}
	switch m := str("TOLLGATE_RATELIMIT_FAIL_MODE", "open"); m {
	case "open":
		c.RateLimitFailOpen = true
	case "closed":
	default:
		errs = append(errs, fmt.Errorf("TOLLGATE_RATELIMIT_FAIL_MODE: want open|closed, got %q", m))
	}
	ratio, err := strconv.ParseFloat(str("TOLLGATE_TRACE_SAMPLE_RATIO", "1"), 64)
	if err != nil || ratio < 0 || ratio > 1 {
		errs = append(errs, errors.New("TOLLGATE_TRACE_SAMPLE_RATIO: want a number in [0,1]"))
	}
	c.TraceSampleRatio = ratio
	if err := c.LogLevel.UnmarshalText([]byte(str("TOLLGATE_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("TOLLGATE_LOG_LEVEL: %w", err))
	}
	return c, errors.Join(errs...)
}

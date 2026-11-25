// Package config loads Tollgate's configuration from environment variables.
// It is parsed once at startup; invalid values fail fast instead of surfacing
// as confusing behaviour under traffic.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type Config struct {
	Addr        string // TOLLGATE_ADDR
	DatabaseURL string // TOLLGATE_DATABASE_URL (required)

	GroqAPIKey  string // GROQ_API_KEY; Groq provider is enabled only when set
	GroqBaseURL string // TOLLGATE_GROQ_BASE_URL
	MockBaseURL string // TOLLGATE_MOCK_BASE_URL; enables the "mock" provider (tests, load tests)

	UpstreamTimeout time.Duration // TOLLGATE_UPSTREAM_TIMEOUT
	KeyCacheTTL     time.Duration // TOLLGATE_KEY_CACHE_TTL
	ShutdownDelay   time.Duration // TOLLGATE_SHUTDOWN_DELAY
	ShutdownTimeout time.Duration // TOLLGATE_SHUTDOWN_TIMEOUT

	LogLevel slog.Level // TOLLGATE_LOG_LEVEL (debug|info|warn|error)
}

// Load reads configuration through getenv (os.Getenv in production, a map in
// tests) and returns every invalid value at once rather than the first one.
func Load(getenv func(string) string) (Config, error) {
	str := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	var errs []error
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

	c := Config{
		Addr:            str("TOLLGATE_ADDR", ":8080"),
		DatabaseURL:     getenv("TOLLGATE_DATABASE_URL"),
		GroqAPIKey:      getenv("GROQ_API_KEY"),
		GroqBaseURL:     str("TOLLGATE_GROQ_BASE_URL", "https://api.groq.com/openai/v1"),
		MockBaseURL:     getenv("TOLLGATE_MOCK_BASE_URL"),
		UpstreamTimeout: dur("TOLLGATE_UPSTREAM_TIMEOUT", 120*time.Second),
		KeyCacheTTL:     dur("TOLLGATE_KEY_CACHE_TTL", 30*time.Second),
		ShutdownDelay:   dur("TOLLGATE_SHUTDOWN_DELAY", 5*time.Second),
		ShutdownTimeout: dur("TOLLGATE_SHUTDOWN_TIMEOUT", 20*time.Second),
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("TOLLGATE_DATABASE_URL is required"))
	}
	if err := c.LogLevel.UnmarshalText([]byte(str("TOLLGATE_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("TOLLGATE_LOG_LEVEL: %w", err))
	}
	return c, errors.Join(errs...)
}

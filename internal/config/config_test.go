package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string // substring; empty means success
		check   func(t *testing.T, c Config)
	}{
		{
			name: "defaults",
			env:  map[string]string{"TOLLGATE_DATABASE_URL": "postgres://x"},
			check: func(t *testing.T, c Config) {
				if c.Addr != ":8080" || c.KeyCacheTTL != 30*time.Second || c.LogLevel != slog.LevelInfo || !c.RateLimitFailOpen || c.MaxAttempts != 2 {
					t.Errorf("unexpected defaults: %+v", c)
				}
			},
		},
		{
			name: "overrides",
			env: map[string]string{
				"TOLLGATE_DATABASE_URL": "postgres://x", "TOLLGATE_KEY_CACHE_TTL": "5s", "TOLLGATE_LOG_LEVEL": "debug",
			},
			check: func(t *testing.T, c Config) {
				if c.KeyCacheTTL != 5*time.Second || c.LogLevel != slog.LevelDebug {
					t.Errorf("overrides not applied: %+v", c)
				}
			},
		},
		{name: "missing database url", env: map[string]string{}, wantErr: "TOLLGATE_DATABASE_URL"},
		{
			name:    "bad duration",
			env:     map[string]string{"TOLLGATE_DATABASE_URL": "x", "TOLLGATE_UPSTREAM_TIMEOUT": "soon"},
			wantErr: "TOLLGATE_UPSTREAM_TIMEOUT",
		},
		{
			name:    "negative duration",
			env:     map[string]string{"TOLLGATE_DATABASE_URL": "x", "TOLLGATE_KEY_CACHE_TTL": "-1s"},
			wantErr: "TOLLGATE_KEY_CACHE_TTL",
		},
		{
			name:    "bad fail mode",
			env:     map[string]string{"TOLLGATE_DATABASE_URL": "x", "TOLLGATE_RATELIMIT_FAIL_MODE": "maybe"},
			wantErr: "FAIL_MODE",
		},
		{
			name:    "bad int",
			env:     map[string]string{"TOLLGATE_DATABASE_URL": "x", "TOLLGATE_MAX_ATTEMPTS": "0"},
			wantErr: "TOLLGATE_MAX_ATTEMPTS",
		},
		{
			name:    "bad sample ratio",
			env:     map[string]string{"TOLLGATE_DATABASE_URL": "x", "TOLLGATE_TRACE_SAMPLE_RATIO": "2"},
			wantErr: "SAMPLE_RATIO",
		},
		{
			name:    "bad log level",
			env:     map[string]string{"TOLLGATE_DATABASE_URL": "x", "TOLLGATE_LOG_LEVEL": "loud"},
			wantErr: "TOLLGATE_LOG_LEVEL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tt.check(t, c)
		})
	}
}

// Package cache is an exact-match response cache in Redis for deterministic
// (temperature 0) chat completions.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

var tracer = otel.Tracer("github.com/krishbansalofficial/tollgate/internal/cache")

// Cacheable reports whether a response may be reused for an identical
// request. Only an explicit temperature of 0 qualifies: if temperature is
// absent the provider default (usually 1) applies and output is random.
// Streaming is excluded in this version (a hit would need synthetic SSE).
func Cacheable(req *api.ChatRequest) bool {
	return !req.Stream && req.Temperature != nil && *req.Temperature == 0
}

// Key hashes the fields that determine the output, scoped to a project.
//
// Scoping: entries are never shared across projects. A shared cache would
// let one tenant learn, via X-Tollgate-Cache: HIT or timing, whether any
// other tenant sent a given prompt (a membership oracle). The hit rate cost
// is small: cross-project identical prompts are rare.
//
// It is canonical: the
// fields are encoded, decoded into generic values and re-encoded, which
// sorts every object's keys and normalises whitespace and number formatting
// (so `{"b":1,"a":2}` and `{"a": 2, "b": 1.0}` inside tools or content parts
// map to the same key). The "v1" prefix lets a format change invalidate
// everything at once.
func Key(project string, req *api.ChatRequest) string {
	canon := struct {
		Model            string          `json:"model"`
		Messages         []api.Message   `json:"messages"`
		Temperature      *float64        `json:"temperature,omitempty"`
		TopP             *float64        `json:"top_p,omitempty"`
		MaxTokens        *int            `json:"max_tokens,omitempty"`
		Stop             json.RawMessage `json:"stop,omitempty"`
		Seed             *int64          `json:"seed,omitempty"`
		PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
		FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
		Tools            json.RawMessage `json:"tools,omitempty"`
		ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
		ResponseFormat   json.RawMessage `json:"response_format,omitempty"`
	}{
		req.Model, req.Messages, req.Temperature, req.TopP, req.MaxTokens, req.Stop, req.Seed,
		req.PresencePenalty, req.FrequencyPenalty, req.Tools, req.ToolChoice, req.ResponseFormat,
	}
	b, _ := json.Marshal(canon) // cannot fail: all fields are JSON-safe
	var generic any
	_ = json.Unmarshal(b, &generic)
	b, _ = json.Marshal(generic) // encoding/json sorts map keys
	sum := sha256.Sum256(b)
	return "cache:v1:" + project + ":" + hex.EncodeToString(sum[:])
}

type Cache struct {
	rdb     redis.Cmdable
	ttl     time.Duration
	timeout time.Duration
}

func New(rdb redis.Cmdable, ttl time.Duration) *Cache {
	return &Cache{rdb: rdb, ttl: ttl, timeout: 50 * time.Millisecond}
}

// Get returns (nil, nil) on a miss. Callers treat any error as a miss: the
// cache is an optimisation and must never fail a request.
func (c *Cache) Get(ctx context.Context, key string) (*api.ChatResponse, error) {
	ctx, span := tracer.Start(ctx, "cache.get")
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	b, err := c.rdb.Get(ctx, key).Bytes()
	span.SetAttributes(attribute.Bool("tollgate.cache_hit", err == nil))
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var resp api.ChatResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Cache) Set(ctx context.Context, key string, resp *api.ChatResponse) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	ctx, span := tracer.Start(ctx, "cache.set")
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.rdb.Set(ctx, key, b, c.ttl).Err()
}

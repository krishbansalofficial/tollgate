package cache

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

func f(v float64) *float64 { return &v }

func parse(t *testing.T, s string) *api.ChatRequest {
	t.Helper()
	var r api.ChatRequest
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestCacheable(t *testing.T) {
	tests := []struct {
		name string
		req  api.ChatRequest
		want bool
	}{
		{"temperature 0", api.ChatRequest{Temperature: f(0)}, true},
		{"temperature absent", api.ChatRequest{}, false},
		{"temperature 0.7", api.ChatRequest{Temperature: f(0.7)}, false},
		{"streaming", api.ChatRequest{Temperature: f(0), Stream: true}, false},
	}
	for _, tt := range tests {
		if got := Cacheable(&tt.req); got != tt.want {
			t.Errorf("%s: got %v", tt.name, got)
		}
	}
}

func TestKeyCanonical(t *testing.T) {
	base := `{"model":"m","temperature":0,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a","parameters":{}}}]}`
	same := []string{
		// whitespace, field order and number formatting differ; meaning doesn't
		`{"temperature":0.0, "model":"m", "messages":[{"content":"hi","role":"user"}], "tools":[{"function":{"parameters":{},"name":"a"},"type":"function"}]}`,
		// fields that don't affect output are ignored
		`{"model":"m","temperature":0,"user":"alice","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a","parameters":{}}}]}`,
	}
	different := []string{
		`{"model":"m2","temperature":0,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a","parameters":{}}}]}`,
		`{"model":"m","temperature":0,"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"a","parameters":{}}}]}`,
		`{"model":"m","temperature":0,"max_tokens":5,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a","parameters":{}}}]}`,
	}
	k := Key(parse(t, base))
	for _, s := range same {
		if Key(parse(t, s)) != k {
			t.Errorf("should match base: %s", s)
		}
	}
	for _, s := range different {
		if Key(parse(t, s)) == k {
			t.Errorf("should differ from base: %s", s)
		}
	}
}

func TestGetSet(t *testing.T) {
	m := miniredis.RunT(t)
	c := New(redis.NewClient(&redis.Options{Addr: m.Addr()}), time.Minute)
	ctx := context.Background()

	if got, err := c.Get(ctx, "k"); got != nil || err != nil {
		t.Fatalf("miss: %v %v", got, err)
	}
	if err := c.Set(ctx, "k", &api.ChatResponse{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Get(ctx, "k"); err != nil || got.ID != "x" {
		t.Fatalf("hit: %v %v", got, err)
	}
	m.FastForward(2 * time.Minute)
	if got, _ := c.Get(ctx, "k"); got != nil {
		t.Fatal("entry outlived TTL")
	}
	m.Close()
	if _, err := c.Get(ctx, "k"); err == nil {
		t.Fatal("want error with Redis down")
	}
}

package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
	"github.com/krishbansalofficial/tollgate/internal/providers"
)

// scripted is a Provider that returns queued errors, then succeeds.
type scripted struct {
	name  string
	mu    sync.Mutex
	errs  []error
	calls atomic.Int64
	delay time.Duration
}

func (s *scripted) Name() string { return s.name }

func (s *scripted) Chat(ctx context.Context, req *api.ChatRequest) (*api.ChatResponse, error) {
	s.calls.Add(1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) > 0 {
		err := s.errs[0]
		if len(s.errs) > 1 {
			s.errs = s.errs[1:]
		}
		if err != nil {
			return nil, err
		}
	}
	return &api.ChatResponse{ID: s.name + ":" + req.Model, Usage: &api.Usage{PromptTokens: 1}}, nil
}

func perr(status int) error {
	return &providers.ProviderError{Status: status, Retryable: status == 429 || status >= 500}
}

var testCatalog = &Catalog{
	Models: map[string]Price{
		"a/big":   {InputPerMTok: 3, OutputPerMTok: 15},
		"b/small": {InputPerMTok: 0.1, OutputPerMTok: 0.5},
		"c/mid":   {InputPerMTok: 1, OutputPerMTok: 5},
	},
	Aliases: map[string]Alias{
		"tollgate/cheapest": {Policy: "cheapest", Models: []string{"a/big", "c/mid", "b/small"}},
		"tollgate/fastest":  {Policy: "fastest", Models: []string{"a/big", "b/small"}},
	},
}

func newRouter(cfg Config, provs ...*scripted) *Router {
	m := map[string]providers.Provider{}
	for _, p := range provs {
		m[p.name] = p
	}
	r := New(cfg, testCatalog, m, nil)
	r.sleep = func(context.Context, time.Duration) error { return nil }
	return r
}

func req(model string) *api.ChatRequest { return &api.ChatRequest{Model: model} }

func TestPlan(t *testing.T) {
	r := newRouter(Config{MaxAttempts: 1, Fallback: "c/mid"}, &scripted{name: "a"}, &scripted{name: "b"}, &scripted{name: "c"})
	tests := []struct {
		model string
		want  []string
	}{
		{"tollgate/cheapest", []string{"b/small", "c/mid", "a/big"}},
		{"a/anything", []string{"a/anything", "c/mid"}}, // pinned, non-catalog model allowed, fallback appended
		{"c/mid", []string{"c/mid"}},                    // fallback not duplicated
	}
	for _, tt := range tests {
		got, err := r.Plan(req(tt.model))
		if err != nil || !equal(got, tt.want) {
			t.Errorf("Plan(%s) = %v, %v; want %v", tt.model, got, err, tt.want)
		}
	}
	if _, err := r.Plan(req("z/unknown-provider")); !errors.Is(err, ErrUnknownModel) {
		t.Errorf("unknown provider: %v", err)
	}

	// fastest: unobserved first, then by observed latency
	r.latency["a/big"].observe(0.5)
	r.latency["b/small"].observe(0.1)
	if got, _ := r.Plan(req("tollgate/fastest")); !equal(got, []string{"b/small", "a/big", "c/mid"}) {
		t.Errorf("fastest: %v", got)
	}
}

func TestChatRetryThenSucceed(t *testing.T) {
	a := &scripted{name: "a", errs: []error{perr(503), nil}}
	r := newRouter(Config{MaxAttempts: 3, AttemptTimeout: time.Second, MaxRetryWait: time.Second, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, a)
	res, err := r.Chat(context.Background(), req("a/big"))
	if err != nil || res.Attempts != 2 || res.Provider != "a" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestChatFallback(t *testing.T) {
	a := &scripted{name: "a", errs: []error{perr(500)}} // always 500
	b := &scripted{name: "b"}
	r := newRouter(Config{MaxAttempts: 2, AttemptTimeout: time.Second, MaxRetryWait: time.Second, Fallback: "b/small"}, a, b)
	res, err := r.Chat(context.Background(), req("a/big"))
	if err != nil || res.Model != "b/small" || a.calls.Load() != 2 || res.Attempts != 3 {
		t.Fatalf("res=%+v err=%v a.calls=%d", res, err, a.calls.Load())
	}
}

func TestChatClientFaultNoFallback(t *testing.T) {
	a := &scripted{name: "a", errs: []error{perr(400)}}
	b := &scripted{name: "b"}
	r := newRouter(Config{MaxAttempts: 2, AttemptTimeout: time.Second, Fallback: "b/small"}, a, b)
	_, err := r.Chat(context.Background(), req("a/big"))
	if pe, ok := providers.AsProviderError(err); !ok || pe.Status != 400 || b.calls.Load() != 0 || a.calls.Load() != 1 {
		t.Fatalf("err=%v b.calls=%d", err, b.calls.Load())
	}
}

func TestChatLongRetryAfterSkipsToFallback(t *testing.T) {
	a := &scripted{name: "a", errs: []error{&providers.ProviderError{Status: 429, Retryable: true, RetryAfter: time.Minute}}}
	b := &scripted{name: "b"}
	r := newRouter(Config{MaxAttempts: 3, AttemptTimeout: time.Second, MaxRetryWait: 5 * time.Second, Fallback: "b/small"}, a, b)
	res, err := r.Chat(context.Background(), req("a/big"))
	if err != nil || res.Model != "b/small" || a.calls.Load() != 1 {
		t.Fatalf("should not wait a minute on a: res=%+v err=%v", res, err)
	}
}

func TestChatAttemptTimeoutFallsBack(t *testing.T) {
	a := &scripted{name: "a", delay: time.Second}
	b := &scripted{name: "b"}
	r := newRouter(Config{MaxAttempts: 1, AttemptTimeout: 20 * time.Millisecond, Fallback: "b/small"}, a, b)
	res, err := r.Chat(context.Background(), req("a/big"))
	if err != nil || res.Model != "b/small" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestChatContextCanceledStops(t *testing.T) {
	a := &scripted{name: "a", delay: time.Second}
	b := &scripted{name: "b"}
	r := newRouter(Config{MaxAttempts: 1, AttemptTimeout: time.Minute, Fallback: "b/small"}, a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.Chat(ctx, req("a/big")); !errors.Is(err, context.DeadlineExceeded) || b.calls.Load() != 0 {
		t.Fatalf("err=%v b.calls=%d", err, b.calls.Load())
	}
}

func TestChatBreakerOpensAndSkips(t *testing.T) {
	a := &scripted{name: "a", errs: []error{perr(500)}}
	b := &scripted{name: "b"}
	r := newRouter(Config{MaxAttempts: 1, AttemptTimeout: time.Second, Fallback: "b/small"}, a, b)
	for range 5 {
		_, _ = r.Chat(context.Background(), req("a/big"))
	}
	if r.BreakerState("a") != Open {
		t.Fatalf("breaker = %v", r.BreakerState("a"))
	}
	before := a.calls.Load()
	if res, err := r.Chat(context.Background(), req("a/big")); err != nil || res.Model != "b/small" || a.calls.Load() != before {
		t.Fatalf("open breaker must skip a without calling it: res=%+v err=%v", res, err)
	}
}

func TestRetryBudget(t *testing.T) {
	b := retryBudget{balance: 1, max: 10, ratio: 0.5}
	if !b.withdraw() || b.withdraw() {
		t.Fatal("balance 1 should allow exactly one retry")
	}
	b.deposit()
	b.deposit()
	if !b.withdraw() {
		t.Fatal("two deposits of 0.5 should fund a retry")
	}
}

func TestFullJitterBounds(t *testing.T) {
	for attempt := range 10 {
		for range 100 {
			d := fullJitter(100*time.Millisecond, time.Second, attempt)
			if d < 0 || d >= min(time.Second, 100*time.Millisecond<<attempt) {
				t.Fatalf("attempt %d: %v out of range", attempt, d)
			}
		}
	}
}

func TestEWMAConcurrent(t *testing.T) {
	var e ewma
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { e.observe(1) })
	}
	wg.Wait()
	if e.value() != 1 {
		t.Fatalf("ewma of constant 1 = %v", e.value())
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

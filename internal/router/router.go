// Package router picks providers for a request and executes it with
// retries, per-provider circuit breakers and fallback.
package router

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/krishbansalofficial/tollgate/internal/api"
	"github.com/krishbansalofficial/tollgate/internal/providers"
)

var tracer = otel.Tracer("github.com/krishbansalofficial/tollgate/internal/router")

var (
	ErrUnknownModel = errors.New("router: unknown model")
	// ErrUnavailable: every candidate was skipped (breaker open, or a
	// fallback provider that isn't configured).
	ErrUnavailable = errors.New("router: no provider available")
)

type Config struct {
	MaxAttempts    int           // attempts per candidate (1 = no retries)
	AttemptTimeout time.Duration // per attempt; a slow provider then falls back
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	MaxRetryWait   time.Duration // Retry-After longer than this: move to the next candidate instead
	Fallback       string        // public model appended last to every plan ("" = none)
}

func DefaultConfig() Config {
	return Config{MaxAttempts: 2, AttemptTimeout: 30 * time.Second, BaseBackoff: 100 * time.Millisecond, MaxBackoff: 2 * time.Second, MaxRetryWait: 5 * time.Second}
}

// Result describes a successful upstream call.
type Result struct {
	Resp     *api.ChatResponse
	Model    string // public name of the model that served it
	Provider string
	Attempts int           // total upstream attempts across all candidates
	Upstream time.Duration // time spent in upstream calls, including backoff
}

type Router struct {
	cfg       Config
	catalog   *Catalog
	providers map[string]providers.Provider
	breakers  map[string]*Breaker
	latency   map[string]*ewma // catalog models only: a fixed set, so no unbounded growth
	budget    retryBudget
	sleep     func(context.Context, time.Duration) error // injectable for tests
}

// New builds a router. onBreaker is called on breaker state changes (metrics).
func New(cfg Config, cat *Catalog, provs map[string]providers.Provider, onBreaker func(provider string, s State)) *Router {
	r := &Router{
		cfg: cfg, catalog: cat, providers: provs,
		breakers: map[string]*Breaker{}, latency: map[string]*ewma{},
		budget: retryBudget{balance: 10, max: 100, ratio: 0.2},
		sleep:  sleepCtx,
	}
	for name := range provs {
		r.breakers[name] = NewBreaker(func(s State) {
			if onBreaker != nil {
				onBreaker(name, s)
			}
		})
	}
	for m := range cat.Models {
		r.latency[m] = &ewma{}
	}
	return r
}

// Known reports whether model can be routed (checked before any rate-limit
// tokens are spent). Pinned models must be in the catalog: an unpriced model
// would be served at a recorded cost of $0, silently bypassing spend
// tracking and budgets, so unknown models fail closed.
func (r *Router) Known(model string) bool {
	if _, ok := r.catalog.Aliases[model]; ok {
		return true
	}
	if _, priced := r.catalog.Models[model]; !priced {
		return false
	}
	p, _, _ := split(model)
	_, configured := r.providers[p]
	return configured
}

// Cost prices a completion for the ledger.
func (r *Router) Cost(model string, u *api.Usage) float64 {
	if u == nil {
		return 0
	}
	return r.catalog.Cost(model, u.PromptTokens, u.CompletionTokens)
}

// Plan returns candidate public model names in the order they will be tried.
func (r *Router) Plan(req *api.ChatRequest) ([]string, error) {
	var plan []string
	if a, ok := r.catalog.Aliases[req.Model]; ok {
		plan = slices.Clone(a.Models)
		switch a.Policy {
		case "cheapest":
			in := api.EstimatePromptTokens(req)
			out := 1000
			if req.MaxTokens != nil {
				out = *req.MaxTokens
			}
			slices.SortStableFunc(plan, func(x, y string) int {
				return cmpFloat(r.catalog.Cost(x, in, out), r.catalog.Cost(y, in, out))
			})
		case "fastest":
			// Unobserved models (EWMA 0) sort first, so each gets tried once
			// and measured instead of being starved forever.
			slices.SortStableFunc(plan, func(x, y string) int {
				return cmpFloat(r.latency[x].value(), r.latency[y].value())
			})
		}
	} else if r.Known(req.Model) {
		plan = []string{req.Model}
	} else {
		return nil, ErrUnknownModel
	}
	if f := r.cfg.Fallback; f != "" && !slices.Contains(plan, f) {
		plan = append(plan, f)
	}
	return plan, nil
}

// Chat executes the plan. Error semantics:
//   - a client-fault ProviderError (400/404/413/422) returns immediately:
//     another provider would reject the same request;
//   - retryable errors retry the same candidate with backoff (budget
//     permitting), then fall back to the next;
//   - other errors (e.g. 401 from a misconfigured upstream key) fall back;
//   - the request's own context ending stops everything.
func (r *Router) Chat(ctx context.Context, req *api.ChatRequest) (res *Result, err error) {
	ctx, span := tracer.Start(ctx, "router")
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetAttributes(attribute.String("tollgate.served_model", res.Model), attribute.Int("tollgate.attempts", res.Attempts))
		}
		span.End()
	}()
	plan, err := r.Plan(req)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.StringSlice("tollgate.plan", plan))
	r.budget.deposit()
	res = &Result{}
	start := time.Now()
	lastErr := ErrUnavailable

	for _, model := range plan {
		provName, native, _ := split(model)
		p, ok := r.providers[provName]
		if !ok {
			continue // e.g. fallback provider not configured in this deployment
		}
		br := r.breakers[provName]
		var retryAfter time.Duration

		for attempt := range r.cfg.MaxAttempts {
			if attempt > 0 {
				wait := fullJitter(r.cfg.BaseBackoff, r.cfg.MaxBackoff, attempt)
				if retryAfter > wait {
					wait = retryAfter
				}
				if wait > r.cfg.MaxRetryWait || !r.budget.withdraw() {
					break // waiting too long or retry budget spent: next candidate
				}
				if err := r.sleep(ctx, wait); err != nil {
					return nil, err
				}
			}
			if !br.Allow() {
				lastErr = ErrUnavailable
				break
			}

			up := *req // shallow copy; only Model differs
			up.Model = native
			actx, cancel := context.WithTimeout(ctx, r.cfg.AttemptTimeout)
			actx, aspan := tracer.Start(actx, "provider.attempt", trace.WithAttributes(
				attribute.String("tollgate.provider", provName), attribute.String("tollgate.model", model), attribute.Int("tollgate.attempt", attempt)))
			t0 := time.Now()
			resp, err := p.Chat(actx, &up)
			elapsed := time.Since(t0)
			endAttempt(aspan, err)
			cancel()
			res.Attempts++

			if err == nil {
				br.Record(true)
				if e := r.latency[model]; e != nil {
					e.observe(elapsed.Seconds())
				}
				res.Resp, res.Model, res.Provider, res.Upstream = resp, model, provName, time.Since(start)
				return res, nil
			}
			if ctx.Err() != nil { // the request itself is over (client gone / overall deadline)
				br.Release()
				return nil, ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) { // per-attempt timeout: provider too slow
				err = &providers.ProviderError{Provider: provName, Retryable: true, Message: "attempt timed out"}
			}
			pe, ok := providers.AsProviderError(err)
			if !ok {
				br.Release()
				return nil, err // not an upstream failure (e.g. encoding bug): don't mask it
			}
			if pe.ClientFault() {
				// Neutral for the breaker: neither health nor failure. Counting it
				// as success would let a client keep a failing provider's breaker
				// closed (or close a half-open one) by sending bad requests.
				br.Release()
				return nil, err
			}
			br.Record(false)
			lastErr = err
			if !pe.Retryable {
				break
			}
			retryAfter = pe.RetryAfter
		}
	}
	return nil, lastErr
}

func endAttempt(span trace.Span, err error) {
	if pe, ok := providers.AsProviderError(err); ok {
		span.SetAttributes(attribute.Int("tollgate.upstream_status", pe.Status), attribute.Bool("tollgate.retryable", pe.Retryable))
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// BreakerState is exposed for metrics and tests.
func (r *Router) BreakerState(provider string) State { return r.breakers[provider].State() }

// fullJitter returns a random duration in [0, min(maxD, base·2^attempt)).
// "Full" jitter spreads clients uniformly over the whole window, so many
// clients retrying after the same outage don't re-synchronise into waves
// (unlike plain exponential backoff, where they all retry at the same time).
func fullJitter(base, maxD time.Duration, attempt int) time.Duration {
	ceiling := min(maxD, base<<attempt)
	if ceiling <= 0 {
		return 0
	}
	return rand.N(ceiling)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// retryBudget caps retries at a fraction of traffic: each request deposits
// `ratio` tokens, each retry withdraws one. Without a budget, a provider
// brown-out multiplies load by the retry count exactly when the provider is
// weakest. The starting balance allows a few retries when traffic is low.
type retryBudget struct {
	mu                  sync.Mutex
	balance, max, ratio float64
}

func (b *retryBudget) deposit() {
	b.mu.Lock()
	b.balance = min(b.max, b.balance+b.ratio)
	b.mu.Unlock()
}

func (b *retryBudget) withdraw() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.balance < 1 {
		return false
	}
	b.balance--
	return true
}

// ewma is an exponentially weighted moving average of latency (seconds),
// lock-free: the float64 lives in a uint64 as raw bits, and observe retries a
// compare-and-swap until no other goroutine raced it. Readers never block
// writers, which matters because "fastest" reads it on every request.
type ewma struct{ bits atomic.Uint64 }

const alpha = 0.2 // weight of the newest sample; ~last 5 samples dominate

func (e *ewma) observe(x float64) {
	for {
		old := e.bits.Load()
		v := math.Float64frombits(old)
		next := x
		if v != 0 {
			next = v + alpha*(x-v)
		}
		if e.bits.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

func (e *ewma) value() float64 { return math.Float64frombits(e.bits.Load()) }

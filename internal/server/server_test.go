package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/krishbansalofficial/tollgate/internal/api"
	"github.com/krishbansalofficial/tollgate/internal/auth"
	"github.com/krishbansalofficial/tollgate/internal/cache"
	"github.com/krishbansalofficial/tollgate/internal/mockupstream"
	"github.com/krishbansalofficial/tollgate/internal/providers"
	"github.com/krishbansalofficial/tollgate/internal/ratelimit"
	"github.com/krishbansalofficial/tollgate/internal/router"
)

type memStore map[[32]byte]auth.Key

func (m memStore) LookupKey(_ context.Context, h []byte) (auth.Key, error) {
	k, ok := m[[32]byte(h)]
	if !ok {
		return auth.Key{}, auth.ErrNotFound
	}
	return k, nil
}

type gwOpts struct {
	primary, backup mockupstream.Config
	withBackup      bool
	timeout         time.Duration
	rpm             int
	failClosed      bool
	redisDown       bool
}

type gateway struct {
	url             string
	key             string
	primary, backup *mockupstream.Server
	mu              sync.Mutex
	outcomes        []Outcome
	posts           int // requests sent via post; last() waits for this many outcomes
}

// last waits for the outcome of the most recent request. The handler flushes
// the response before recording the outcome (bookkeeping is off the
// client's critical path), so the client can finish first.
func (g *gateway) last() Outcome {
	for range 200 {
		g.mu.Lock()
		if n := len(g.outcomes); n >= g.posts {
			o := g.outcomes[n-1]
			g.mu.Unlock()
			return o
		}
		g.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	panic("outcome never recorded")
}

func newGateway(t *testing.T, o gwOpts) *gateway {
	t.Helper()
	if o.timeout == 0 {
		o.timeout = time.Second
	}
	g := &gateway{primary: mockupstream.New(o.primary), backup: mockupstream.New(o.backup)}
	up1 := httptest.NewServer(g.primary.Handler())
	up2 := httptest.NewServer(g.backup.Handler())
	t.Cleanup(up1.Close)
	t.Cleanup(up2.Close)

	provs := map[string]providers.Provider{"mock": providers.NewOpenAICompat("mock", up1.URL+"/v1", "", up1.Client())}
	cfg := router.DefaultConfig()
	cfg.MaxAttempts = 1
	if o.withBackup {
		provs["backup"] = providers.NewOpenAICompat("backup", up2.URL+"/v1", "", up2.Client())
		cfg.Fallback = "backup/m"
	}
	cat := &router.Catalog{Models: map[string]router.Price{"mock/small": {InputPerMTok: 1, OutputPerMTok: 2}, "backup/m": {InputPerMTok: 1, OutputPerMTok: 2}}}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if o.redisDown {
		mr.Close()
	}
	rpm := o.rpm
	if rpm == 0 {
		rpm = 1000
	}

	raw, h, _, _ := auth.Generate()
	g.key = raw
	handler := New(Deps{
		Auth:             auth.NewAuthenticator(memStore{h: {ID: "k1", ProjectID: "p1", ProjectName: "proj", RPMLimit: rpm, TPMLimit: 1_000_000}}, time.Minute),
		Limiter:          ratelimit.New(rdb, !o.failClosed),
		Cache:            cache.New(rdb, time.Minute),
		Router:           router.New(cfg, cat, provs, nil),
		Observer:         ObserverFunc(func(oc Outcome) { g.mu.Lock(); g.outcomes = append(g.outcomes, oc); g.mu.Unlock() }),
		Ready:            func(context.Context) error { return nil },
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		UpstreamTimeout:  o.timeout,
		DefaultMaxTokens: 256,
		RunAfter:         func(f func()) { f() }, // deterministic cache fills
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	g.url = srv.URL
	return g
}

func (g *gateway) post(t *testing.T, body string) *http.Response {
	t.Helper()
	g.posts++
	return postKey(t, g.url, g.key, body)
}

func postKey(t *testing.T, url, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeResp(t *testing.T, resp *http.Response) api.ChatResponse {
	t.Helper()
	var out api.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

const okBody = `{"model":"mock/small","messages":[{"role":"user","content":"hello"}]}`

func TestChatEndToEnd(t *testing.T) {
	g := newGateway(t, gwOpts{})
	resp := g.post(t, okBody)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	out := decodeResp(t, resp)
	if out.Model != "mock/small" || len(out.Choices) != 1 || out.Usage == nil {
		t.Errorf("unexpected response %+v", out)
	}
	for _, h := range []string{"X-Request-ID", "X-RateLimit-Remaining-Requests", "X-RateLimit-Remaining-Tokens"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	if resp.Header.Get("X-Tollgate-Cache") != "BYPASS" || resp.Header.Get("X-Tollgate-Provider") != "mock" {
		t.Errorf("headers: %v", resp.Header)
	}
	o := g.last()
	if o.Status != 200 || o.ProjectID != "p1" || o.TokensOut != 10 || o.CostUSD <= 0 || o.Provider != "mock" || o.Attempts != 1 {
		t.Errorf("outcome: %+v", o)
	}
}

func TestChatErrors(t *testing.T) {
	tests := []struct {
		name       string
		opts       gwOpts
		noKey      bool
		body       string
		wantStatus int
		wantRetry  bool
	}{
		{name: "no key", noKey: true, body: okBody, wantStatus: 401},
		{name: "bad json", body: `{`, wantStatus: 400},
		{name: "no messages", body: `{"model":"mock/x","messages":[]}`, wantStatus: 400},
		{name: "negative max_tokens", body: `{"model":"mock/x","max_tokens":-5,"messages":[{"role":"user","content":"hi"}]}`, wantStatus: 400},
		{name: "stream with unknown model", body: `{"model":"mock/x","stream":true,"messages":[{"role":"user","content":"hi"}]}`, wantStatus: 404},
		{name: "unknown provider", body: `{"model":"nope/x","messages":[{"role":"user","content":"hi"}]}`, wantStatus: 404},
		{name: "no provider prefix", body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, wantStatus: 404},
		{name: "upstream 500", opts: gwOpts{primary: mockupstream.Config{ErrorRate: 1}}, body: okBody, wantStatus: 502},
		{name: "upstream 429", opts: gwOpts{primary: mockupstream.Config{RateLimitRate: 1}}, body: okBody, wantStatus: 503, wantRetry: true},
		{name: "upstream timeout", opts: gwOpts{primary: mockupstream.Config{Latency: time.Second}, timeout: 30 * time.Millisecond}, body: okBody, wantStatus: 504},
		{name: "redis down, fail closed", opts: gwOpts{redisDown: true, failClosed: true}, body: okBody, wantStatus: 503},
		{name: "redis down, fail open", opts: gwOpts{redisDown: true}, body: okBody, wantStatus: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGateway(t, tt.opts)
			key := g.key
			if tt.noKey {
				key = ""
			}
			g.posts++
			resp := postKey(t, g.url, key, tt.body)
			if resp.StatusCode != tt.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tt.wantStatus, b)
			}
			if tt.wantStatus >= 400 {
				var e api.ErrorResponse
				if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Error.Message == "" {
					t.Errorf("not an OpenAI error envelope: %v %+v", err, e)
				}
			}
			if tt.wantRetry && resp.Header.Get("Retry-After") == "" {
				t.Error("missing Retry-After")
			}
		})
	}
}

func TestRateLimited(t *testing.T) {
	g := newGateway(t, gwOpts{rpm: 2})
	for i := range 2 {
		if s := g.post(t, okBody).StatusCode; s != 200 {
			t.Fatalf("request %d: %d", i, s)
		}
	}
	resp := g.post(t, okBody)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "30" {
		t.Fatalf("status %d Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if g.primary.Requests() != 2 {
		t.Fatalf("rate-limited request reached upstream")
	}
	if o := g.last(); o.Status != 429 || o.ErrorClass != "rate_limited" {
		t.Errorf("outcome %+v", o)
	}
}

func TestCacheHitSkipsUpstream(t *testing.T) {
	g := newGateway(t, gwOpts{})
	body := `{"model":"mock/small","temperature":0,"messages":[{"role":"user","content":"cache me"}]}`

	r1 := g.post(t, body)
	first := decodeResp(t, r1)
	r2 := g.post(t, body)
	second := decodeResp(t, r2)

	if r1.Header.Get("X-Tollgate-Cache") != "MISS" || r2.Header.Get("X-Tollgate-Cache") != "HIT" {
		t.Fatalf("cache headers: %q then %q", r1.Header.Get("X-Tollgate-Cache"), r2.Header.Get("X-Tollgate-Cache"))
	}
	if g.primary.Requests() != 1 {
		t.Fatalf("upstream requests = %d, want 1", g.primary.Requests())
	}
	if first.ID == second.ID || first.Choices[0].Message.TextContent() != second.Choices[0].Message.TextContent() {
		t.Errorf("hit must reuse content but not ID: %q %q", first.ID, second.ID)
	}
	if o := g.last(); o.Cache != "HIT" || o.CostSavedUSD <= 0 || o.CostUSD != 0 {
		t.Errorf("outcome %+v", o)
	}
}

func TestFallbackToBackup(t *testing.T) {
	g := newGateway(t, gwOpts{primary: mockupstream.Config{ErrorRate: 1}, withBackup: true})
	resp := g.post(t, okBody)
	if resp.StatusCode != 200 || resp.Header.Get("X-Tollgate-Provider") != "backup" {
		t.Fatalf("status %d provider %q", resp.StatusCode, resp.Header.Get("X-Tollgate-Provider"))
	}
	if out := decodeResp(t, resp); out.Model != "backup/m" {
		t.Errorf("model %q", out.Model)
	}
}

func TestReadyz(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want int
	}{{nil, 200}, {errors.New("db down"), 503}} {
		h := New(Deps{Ready: func(context.Context) error { return tt.err }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != tt.want || strings.Contains(rec.Body.String(), "db down") {
			t.Errorf("readyz with err=%v: %d %q", tt.err, rec.Code, rec.Body.String())
		}
	}
}

func TestLandingPageOnlyAtRoot(t *testing.T) {
	g := newGateway(t, gwOpts{})
	for path, want := range map[string]int{"/": 200, "/nope": 404, "/v1": 404} {
		resp, err := http.Get(g.url + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: status %d, want %d", path, resp.StatusCode, want)
		}
		if path == "/" && (!strings.Contains(string(b), "<title>Tollgate") || resp.Header.Get("Content-Security-Policy") == "") {
			t.Errorf("GET /: missing page or CSP header")
		}
	}
}

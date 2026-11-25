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
	"testing"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
	"github.com/krishbansalofficial/tollgate/internal/auth"
	"github.com/krishbansalofficial/tollgate/internal/mockupstream"
	"github.com/krishbansalofficial/tollgate/internal/providers"
)

type memStore map[[32]byte]auth.Key

func (m memStore) LookupKey(_ context.Context, h []byte) (auth.Key, error) {
	k, ok := m[[32]byte(h)]
	if !ok {
		return auth.Key{}, auth.ErrNotFound
	}
	return k, nil
}

// newGateway runs the real handler against a mock upstream end to end.
func newGateway(t *testing.T, mockCfg mockupstream.Config, timeout time.Duration) (gw *httptest.Server, key string) {
	t.Helper()
	up := httptest.NewServer(mockupstream.New(mockCfg).Handler())
	t.Cleanup(up.Close)

	raw, h, _, _ := auth.Generate()
	h2 := New(Deps{
		Auth:            auth.NewAuthenticator(memStore{h: {ID: "k", ProjectName: "p"}}, time.Minute),
		Providers:       map[string]providers.Provider{"mock": providers.NewOpenAICompat("mock", up.URL+"/v1", "", up.Client())},
		Ready:           func(context.Context) error { return nil },
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		UpstreamTimeout: timeout,
	})
	gw = httptest.NewServer(h2)
	t.Cleanup(gw.Close)
	return gw, raw
}

func post(t *testing.T, url, key, body string) *http.Response {
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

const okBody = `{"model":"mock/small","messages":[{"role":"user","content":"hello"}]}`

func TestChatEndToEnd(t *testing.T) {
	gw, key := newGateway(t, mockupstream.Config{}, time.Second)
	resp := post(t, gw.URL, key, okBody)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var out api.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "mock/small" || len(out.Choices) != 1 || out.Usage == nil {
		t.Errorf("unexpected response %+v", out)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("missing X-Request-ID")
	}
}

func TestChatErrors(t *testing.T) {
	tests := []struct {
		name       string
		mock       mockupstream.Config
		timeout    time.Duration
		noKey      bool
		body       string
		wantStatus int
		wantRetry  bool
	}{
		{name: "no key", noKey: true, body: okBody, wantStatus: 401},
		{name: "bad json", body: `{`, wantStatus: 400},
		{name: "no messages", body: `{"model":"mock/x","messages":[]}`, wantStatus: 400},
		{name: "stream unsupported", body: `{"model":"mock/x","stream":true,"messages":[{"role":"user","content":"hi"}]}`, wantStatus: 400},
		{name: "unknown provider", body: `{"model":"nope/x","messages":[{"role":"user","content":"hi"}]}`, wantStatus: 404},
		{name: "no provider prefix", body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, wantStatus: 404},
		{name: "upstream 500", mock: mockupstream.Config{ErrorRate: 1}, body: okBody, wantStatus: 502},
		{name: "upstream 429", mock: mockupstream.Config{RateLimitRate: 1}, body: okBody, wantStatus: 503, wantRetry: true},
		{name: "upstream timeout", mock: mockupstream.Config{Latency: time.Second}, timeout: 30 * time.Millisecond, body: okBody, wantStatus: 504},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.timeout == 0 {
				tt.timeout = time.Second
			}
			gw, key := newGateway(t, tt.mock, tt.timeout)
			if tt.noKey {
				key = ""
			}
			resp := post(t, gw.URL, key, tt.body)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			var e api.ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Error.Message == "" {
				t.Errorf("not an OpenAI error envelope: %v %+v", err, e)
			}
			if tt.wantRetry && resp.Header.Get("Retry-After") == "" {
				t.Error("missing Retry-After")
			}
		})
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
		if rec.Code != tt.want {
			t.Errorf("readyz with err=%v: %d, want %d", tt.err, rec.Code, tt.want)
		}
	}
}

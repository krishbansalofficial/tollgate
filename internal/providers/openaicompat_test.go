package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
	"github.com/krishbansalofficial/tollgate/internal/mockupstream"
)

func req(model string) *api.ChatRequest {
	return &api.ChatRequest{Model: model, Messages: []api.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
}

func TestOpenAICompatSuccess(t *testing.T) {
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var in api.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotModel = in.Model
		_ = json.NewEncoder(w).Encode(api.ChatResponse{ID: "x", Usage: &api.Usage{TotalTokens: 3}})
	}))
	defer srv.Close()

	p := NewOpenAICompat("groq", srv.URL+"/", "secret", srv.Client())
	resp, err := p.Chat(context.Background(), req("llama"))
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" || gotModel != "llama" || resp.Usage.TotalTokens != 3 {
		t.Errorf("auth=%q model=%q resp=%+v", gotAuth, gotModel, resp)
	}
}

func TestOpenAICompatErrors(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		retryAfter    string
		body          string
		wantRetryable bool
		wantAfter     time.Duration
		wantMsg       string
	}{
		{"server error", 500, "", `{"error":{"message":"boom"}}`, true, 0, "boom"},
		{"rate limited", 429, "7", `{"error":{"message":"slow down"}}`, true, 7 * time.Second, "slow down"},
		{"bad request", 400, "", `{"error":{"message":"bad model"}}`, false, 0, "bad model"},
		{"non-json body", 502, "", "<html>bad gateway</html>", true, 0, "<html>bad gateway</html>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			_, err := NewOpenAICompat("p", srv.URL, "", srv.Client()).Chat(context.Background(), req("m"))
			pe, ok := AsProviderError(err)
			if !ok {
				t.Fatalf("want ProviderError, got %v", err)
			}
			if pe.Status != tt.status || pe.Retryable != tt.wantRetryable || pe.RetryAfter != tt.wantAfter || pe.Message != tt.wantMsg {
				t.Errorf("got %+v", pe)
			}
		})
	}
}

func TestOpenAICompatContextDeadlineIsNotProviderError(t *testing.T) {
	mock := mockupstream.New(mockupstream.Config{Latency: time.Second})
	srv := httptest.NewServer(mock.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := NewOpenAICompat("mock", srv.URL+"/v1", "", srv.Client()).Chat(ctx, req("m"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
}

func TestOpenAICompatConnectionRefusedIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listening now

	_, err := NewOpenAICompat("p", url, "", NewHTTPClient()).Chat(context.Background(), req("m"))
	pe, ok := AsProviderError(err)
	if !ok || !pe.Retryable || pe.Status != 0 {
		t.Fatalf("want retryable transport ProviderError, got %v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"3", 3 * time.Second},
		{"-1", 0},
		{"soon", 0},
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

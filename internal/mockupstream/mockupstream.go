// Package mockupstream is a fake OpenAI-compatible provider with injectable
// latency, errors and 429s. Tests and load tests point Tollgate at it so that
// measurements reflect gateway overhead, not a real provider's latency.
package mockupstream

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

type Config struct {
	Latency       time.Duration // base response latency
	Jitter        time.Duration // uniform extra latency in [0, Jitter)
	ErrorRate     float64       // fraction of requests answered with 500
	RateLimitRate float64       // fraction of requests answered with 429
}

// Server counts requests so tests can assert how many attempts reached it.
type Server struct {
	cfg      Config
	requests atomic.Int64
}

func New(cfg Config) *Server { return &Server{cfg: cfg} }

func (s *Server) Requests() int64 { return s.requests.Load() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	return mux
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	n := s.requests.Add(1)

	delay := s.cfg.Latency
	if s.cfg.Jitter > 0 {
		delay += rand.N(s.cfg.Jitter) // math/rand/v2 top-level funcs are goroutine-safe
	}
	if delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-r.Context().Done(): // caller gave up; don't hold the goroutine
			return
		}
	}

	switch p := rand.Float64(); {
	case p < s.cfg.RateLimitRate:
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, "rate_limit_exceeded", "mock: rate limited")
		return
	case p < s.cfg.RateLimitRate+s.cfg.ErrorRate:
		writeErr(w, http.StatusInternalServerError, "server_error", "mock: injected failure")
		return
	}

	var req api.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "mock: bad request body")
		return
	}
	last := req.Messages[len(req.Messages)-1].TextContent()
	if len(last) > 64 {
		last = last[:64]
	}
	prompt := 0
	for _, m := range req.Messages {
		prompt += len(m.Content)/4 + 1
	}
	const completion = 10
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.ChatResponse{
		ID:      fmt.Sprintf("mock-%d", n),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []api.Choice{{
			Message:      api.Message{Role: "assistant", Content: jsonString("mock reply to: " + last)},
			FinishReason: "stop",
		}},
		Usage: &api.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion},
	})
}

func writeErr(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.ErrorBody{Message: msg, Type: typ}})
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

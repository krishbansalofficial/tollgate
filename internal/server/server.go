// Package server wires HTTP routes, middleware and the chat handler.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
	"github.com/krishbansalofficial/tollgate/internal/auth"
	"github.com/krishbansalofficial/tollgate/internal/providers"
)

const maxRequestBody = 4 << 20 // long contexts are big, but not unbounded

type Deps struct {
	Auth            *auth.Authenticator
	Providers       map[string]providers.Provider // keyed by the "provider/" model prefix
	Ready           func(context.Context) error
	Logger          *slog.Logger
	UpstreamTimeout time.Duration
}

type Server struct{ Deps }

// New returns the root handler.
func New(d Deps) http.Handler {
	s := &Server{d}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("POST /v1/chat/completions", auth.Middleware(d.Auth, writeError)(http.HandlerFunc(s.chat)))
	return s.requestID(s.accessLog(mux))
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Ready(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req api.ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" || len(req.Messages) == 0 {
		writeError(w, r, http.StatusBadRequest, "invalid_request_error", "model and messages are required")
		return
	}
	if req.Stream {
		writeError(w, r, http.StatusBadRequest, "invalid_request_error", "streaming is not supported yet")
		return
	}
	provName, native, ok := strings.Cut(req.Model, "/")
	p, known := s.Providers[provName]
	if !ok || !known || native == "" {
		writeErrorCode(w, r, http.StatusNotFound, "invalid_request_error", "model_not_found",
			"unknown model "+strconv.Quote(req.Model)+`; use "<provider>/<model>"`)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.UpstreamTimeout)
	defer cancel()
	up := req // shallow copy: only Model differs, slices are read-only from here on
	up.Model = native
	resp, err := p.Chat(ctx, &up)
	if err != nil {
		s.upstreamError(w, r, p.Name(), err)
		return
	}
	resp.Model = req.Model // report the name the client asked for
	writeJSON(w, http.StatusOK, resp)
}

// upstreamError maps provider failures to client responses. The rule: the
// client sees its own mistakes verbatim (4xx) but never our upstream
// credentials or quota problems, which surface as 502/503.
func (s *Server) upstreamError(w http.ResponseWriter, r *http.Request, provider string, err error) {
	log := s.Logger.With("request_id", requestIDFrom(r.Context()), "provider", provider)
	switch pe, ok := providers.AsProviderError(err); {
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		log.Info("client canceled request")
		// Nothing useful can be written to a client that hung up.
	case errors.Is(err, context.DeadlineExceeded):
		log.Warn("upstream timeout")
		writeError(w, r, http.StatusGatewayTimeout, "api_error", "upstream timed out")
	case ok && pe.Status == http.StatusTooManyRequests:
		log.Warn("upstream rate limited", "retry_after", pe.RetryAfter)
		if pe.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(pe.RetryAfter.Seconds()+0.5)))
		}
		writeError(w, r, http.StatusServiceUnavailable, "api_error", "upstream capacity exhausted; retry later")
	case ok && pe.Status >= 400 && pe.Status < 500 && pe.Status != http.StatusUnauthorized && pe.Status != http.StatusForbidden:
		writeError(w, r, pe.Status, "invalid_request_error", provider+": "+pe.Message)
	default:
		log.Error("upstream error", "err", err)
		writeError(w, r, http.StatusBadGateway, "api_error", "upstream provider error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, typ, msg string) {
	writeErrorCode(w, r, status, typ, "", msg)
}

func writeErrorCode(w http.ResponseWriter, _ *http.Request, status int, typ, code, msg string) {
	writeJSON(w, status, api.ErrorResponse{Error: api.ErrorBody{Message: msg, Type: typ, Code: code}})
}

// --- middleware ---

type ridKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ridKey{}).(string)
	return id
}

// requestID honours an incoming X-Request-ID (so a caller can correlate its
// own logs) if it's sane, else generates one, and echoes it on the response.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 128 {
			var b [12]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ridKey{}, id)))
	})
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return // probes every few seconds would drown real traffic
		}
		s.Logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

// statusRecorder captures the status code. Unwrap lets http.ResponseController
// reach the underlying writer's Flush/SetWriteDeadline, which streaming needs.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

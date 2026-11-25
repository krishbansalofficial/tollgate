// Package server wires HTTP routes, middleware and the chat pipeline:
// auth → rate limit → cache → router → (respond) → reconcile + cache fill.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/krishbansalofficial/tollgate/internal/api"
	"github.com/krishbansalofficial/tollgate/internal/auth"
	"github.com/krishbansalofficial/tollgate/internal/cache"
	"github.com/krishbansalofficial/tollgate/internal/providers"
	"github.com/krishbansalofficial/tollgate/internal/ratelimit"
	"github.com/krishbansalofficial/tollgate/internal/router"
)

const maxRequestBody = 4 << 20 // long contexts are big, but not unbounded

// Outcome is everything known about one finished chat request. It feeds the
// usage ledger and metrics through Observer, so the handler stays unaware of
// where the data goes.
type Outcome struct {
	RequestID, ProjectID, ProjectName, KeyID string
	Model, Provider                          string // public model that served it
	TokensIn, TokensOut                      int
	UsageEstimated                           bool
	Latency, Upstream, TTFT                  time.Duration
	CostUSD, CostSavedUSD                    float64
	Cache                                    string // HIT | MISS | BYPASS
	Status                                   int
	ErrorClass                               string
	Attempts                                 int
	Stream                                   bool
}

// Observer must not block: it is called on the request goroutine.
type Observer interface{ Observe(Outcome) }

type ObserverFunc func(Outcome)

func (f ObserverFunc) Observe(o Outcome) { f(o) }

type Deps struct {
	Auth             *auth.Authenticator
	Limiter          *ratelimit.Limiter // nil disables rate limiting
	Cache            *cache.Cache       // nil disables caching
	Router           *router.Router
	Observer         Observer
	Ready            func(context.Context) error
	Logger           *slog.Logger
	UpstreamTimeout  time.Duration
	DefaultMaxTokens int
}

type Server struct{ Deps }

// New returns the root handler.
func New(d Deps) http.Handler {
	if d.Observer == nil {
		d.Observer = ObserverFunc(func(Outcome) {})
	}
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
		// Log the cause; never echo it. /readyz is unauthenticated, and a
		// dependency error can contain hostnames or connection details.
		s.Logger.Warn("not ready", "err", err)
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// request carries per-request state through the pipeline helpers.
type request struct {
	w        http.ResponseWriter
	r        *http.Request
	key      auth.Key
	body     api.ChatRequest
	reserved int // TPM tokens reserved by the limiter, to reconcile at the end
	out      Outcome
	start    time.Time
}

func (q *request) fail(status int, class, typ, code, msg string) {
	q.out.Status, q.out.ErrorClass = status, class
	writeErrorCode(q.w, q.r, status, typ, code, msg)
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	key, _ := auth.FromContext(r.Context())
	q := &request{w: w, r: r, key: key, start: time.Now()}
	q.out = Outcome{RequestID: requestIDFrom(r.Context()), ProjectID: key.ProjectID, ProjectName: key.ProjectName, KeyID: key.ID, Cache: "BYPASS"}
	defer func() {
		q.out.Latency = time.Since(q.start)
		s.Observer.Observe(q.out)
	}()

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&q.body); err != nil {
		q.fail(http.StatusBadRequest, "invalid_request", "invalid_request_error", "", "invalid JSON body: "+err.Error())
		return
	}
	req := &q.body
	q.out.Model, q.out.Stream = req.Model, req.Stream
	switch {
	case req.Model == "" || len(req.Messages) == 0:
		q.fail(http.StatusBadRequest, "invalid_request", "invalid_request_error", "", "model and messages are required")
		return
	case req.MaxTokens != nil && *req.MaxTokens < 1:
		q.fail(http.StatusBadRequest, "invalid_request", "invalid_request_error", "", "max_tokens must be at least 1")
		return
	case !s.Router.Known(req.Model):
		// Checked before rate limiting so a typo doesn't spend the caller's quota.
		q.fail(http.StatusNotFound, "invalid_request", "invalid_request_error", "model_not_found",
			"unknown model "+strconv.Quote(req.Model)+`; use "<provider>/<model>" or a tollgate/ alias`)
		return
	}

	if !s.admit(q) {
		return
	}
	// Post-response work (reconcile, cache fill) must survive the client
	// hanging up, but stay bounded: detach from cancellation, keep values
	// (trace context), and rely on each call's own short timeout.
	bg := context.WithoutCancel(r.Context())

	if req.Stream {
		s.serveStream(q, bg) // streams are never cached
		return
	}

	var cacheKey string
	if s.Cache != nil && cache.Cacheable(req) {
		cacheKey = cache.Key(key.ProjectID, req)
		hit, err := s.Cache.Get(r.Context(), cacheKey)
		if err != nil {
			s.Logger.WarnContext(r.Context(), "cache get failed; treating as miss", "request_id", q.out.RequestID, "err", err)
		}
		if hit != nil {
			s.serveCacheHit(q, hit)
			s.reconcile(bg, q, 0) // no upstream tokens were used: refund the whole reservation
			return
		}
		q.out.Cache = "MISS"
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.UpstreamTimeout)
	defer cancel()
	res, err := s.Router.Chat(ctx, req)
	if err != nil {
		s.reconcile(bg, q, 0)
		s.upstreamError(q, err)
		return
	}
	resp := res.Resp
	resp.Model = res.Model // report what actually served it (matters for aliases)
	q.out.Model, q.out.Provider, q.out.Upstream, q.out.Attempts = res.Model, res.Provider, res.Upstream, res.Attempts
	used := s.recordUsage(q, resp.Usage, res.Model)

	w.Header().Set("X-Tollgate-Cache", q.out.Cache)
	w.Header().Set("X-Tollgate-Provider", res.Provider)
	writeJSON(w, http.StatusOK, resp)
	q.out.Status = http.StatusOK
	// Flush now so the client has its answer before the bookkeeping below;
	// otherwise net/http would hold the response until the handler returns.
	_ = http.NewResponseController(w).Flush()

	s.reconcile(bg, q, used)
	if cacheKey != "" {
		if err := s.Cache.Set(bg, cacheKey, resp); err != nil {
			s.Logger.WarnContext(bg, "cache set failed", "request_id", q.out.RequestID, "err", err)
		}
	}
}

// admit applies the rate limit, writing the rejection itself. It reserves
// one request plus the prompt estimate and the maximum possible output.
func (s *Server) admit(q *request) bool {
	if s.Limiter == nil {
		return true
	}
	maxOut := s.DefaultMaxTokens
	if q.body.MaxTokens != nil {
		maxOut = *q.body.MaxTokens
	}
	reserve := api.EstimatePromptTokens(&q.body) + maxOut
	d, err := s.Limiter.Allow(q.r.Context(), q.key.ID, q.key.RPMLimit, q.key.TPMLimit, reserve)
	if err != nil {
		s.Logger.WarnContext(q.r.Context(), "rate limiter degraded", "request_id", q.out.RequestID, "fail_open", d.Allowed, "err", err)
	}
	h := q.w.Header()
	if d.RemainingRequests >= 0 {
		h.Set("X-RateLimit-Remaining-Requests", strconv.Itoa(d.RemainingRequests))
	}
	if d.RemainingTokens >= 0 {
		h.Set("X-RateLimit-Remaining-Tokens", strconv.Itoa(max(0, d.RemainingTokens)))
	}
	switch {
	case d.Allowed:
		q.reserved = d.Reserved
		return true
	case d.Degraded: // fail-closed with Redis down
		q.fail(http.StatusServiceUnavailable, "ratelimit_unavailable", "api_error", "", "rate limiter unavailable")
	default:
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(d.RetryAfter.Seconds()))))
		q.fail(http.StatusTooManyRequests, "rate_limited", "rate_limit_error", "rate_limit_exceeded", "rate limit exceeded for this API key")
	}
	return false
}

// reconcile returns unused reserved tokens (or charges extra) once the real
// usage is known.
func (s *Server) reconcile(ctx context.Context, q *request, used int) {
	if s.Limiter == nil || q.reserved == 0 && used == 0 {
		return
	}
	if err := s.Limiter.Reconcile(ctx, q.key.ID, q.key.TPMLimit, q.reserved-used); err != nil {
		s.Logger.WarnContext(ctx, "rate limit reconcile failed", "request_id", q.out.RequestID, "err", err)
	}
}

func (s *Server) serveCacheHit(q *request, hit *api.ChatResponse) {
	// Fresh ID/Created: two clients receiving the same cached body must not
	// be able to correlate their responses.
	hit.ID, hit.Created = "chatcmpl-"+newID(), time.Now().Unix()
	q.out.Cache, q.out.Model, q.out.Provider = "HIT", hit.Model, "cache"
	if hit.Usage != nil {
		q.out.TokensIn, q.out.TokensOut = hit.Usage.PromptTokens, hit.Usage.CompletionTokens
		q.out.CostSavedUSD = s.Router.Cost(hit.Model, hit.Usage)
	}
	q.w.Header().Set("X-Tollgate-Cache", "HIT")
	writeJSON(q.w, http.StatusOK, hit)
	q.out.Status = http.StatusOK
}

// recordUsage fills tokens and cost, estimating when the upstream sent no
// usage, and returns the total tokens to charge against the TPM bucket.
func (s *Server) recordUsage(q *request, u *api.Usage, model string) int {
	if u == nil {
		u = &api.Usage{PromptTokens: api.EstimatePromptTokens(&q.body)}
		q.out.UsageEstimated = true
	}
	q.out.TokensIn, q.out.TokensOut = u.PromptTokens, u.CompletionTokens
	q.out.CostUSD = s.Router.Cost(model, u)
	return u.PromptTokens + u.CompletionTokens
}

// upstreamError maps router failures to client responses. The rule: the
// client sees its own mistakes (client-fault 4xx) but never our upstream
// credentials or quota problems, which surface as 502/503.
func (s *Server) upstreamError(q *request, err error) {
	log := s.Logger.With("request_id", q.out.RequestID, "model", q.body.Model)
	pe, isPE := providers.AsProviderError(err)
	switch {
	case errors.Is(err, context.Canceled) && q.r.Context().Err() != nil:
		log.InfoContext(q.r.Context(), "client canceled request")
		q.out.Status, q.out.ErrorClass = 499, "client_canceled" // nginx convention; nothing is written
	case errors.Is(err, context.DeadlineExceeded):
		log.WarnContext(q.r.Context(), "upstream timeout")
		q.fail(http.StatusGatewayTimeout, "timeout", "api_error", "", "upstream timed out")
	case errors.Is(err, router.ErrUnavailable):
		log.WarnContext(q.r.Context(), "no provider available")
		q.fail(http.StatusServiceUnavailable, "no_provider", "api_error", "", "no upstream provider available; retry later")
	case isPE && pe.ClientFault():
		msg := "upstream rejected the request"
		if m, ok := pe.ClientMessage(); ok {
			msg = pe.Provider + ": " + m
		}
		log.InfoContext(q.r.Context(), "upstream rejected request", "status", pe.Status, "upstream_message", pe.Message)
		q.fail(pe.Status, "upstream_client_error", "invalid_request_error", "", msg)
	case isPE && pe.Status == http.StatusTooManyRequests:
		log.WarnContext(q.r.Context(), "upstream rate limited", "provider", pe.Provider, "retry_after", pe.RetryAfter)
		if pe.RetryAfter > 0 {
			q.w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(pe.RetryAfter.Seconds()))))
		}
		q.fail(http.StatusServiceUnavailable, "upstream_rate_limited", "api_error", "", "upstream capacity exhausted; retry later")
	default:
		log.ErrorContext(q.r.Context(), "upstream error", "err", err)
		q.fail(http.StatusBadGateway, "upstream_error", "api_error", "", "upstream provider error")
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

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// --- middleware ---

type ridKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ridKey{}).(string)
	return id
}

// requestID honours an incoming X-Request-ID (so a caller can correlate its
// own logs) if it's sane; otherwise it uses the trace ID, so the ID a client
// reports can be pasted straight into the trace viewer. It echoes the ID.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 128 {
			if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
				id = sc.TraceID().String()
			} else {
				id = newID()
			}
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

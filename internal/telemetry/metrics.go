// Package telemetry holds Prometheus metrics, OpenTelemetry tracing setup,
// and a trace-aware slog handler.
package telemetry

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/krishbansalofficial/tollgate/internal/router"
	"github.com/krishbansalofficial/tollgate/internal/server"
)

// Cardinality rule: every label value comes from a bounded set (catalog
// models, configured providers, projects, status codes). Never key IDs,
// request IDs or raw client-supplied strings: each distinct value is a new
// time series, and an attacker-controlled label is a memory DoS on Prometheus.
type Metrics struct {
	reg *prometheus.Registry

	requests     *prometheus.CounterVec
	httpRequests *prometheus.CounterVec
	provErrors   *prometheus.CounterVec
	overhead     prometheus.Histogram
	upstream     *prometheus.HistogramVec
	ttft         *prometheus.HistogramVec
	cacheReqs    *prometheus.CounterVec
	cost         *prometheus.CounterVec
	costSaved    *prometheus.CounterVec
	tokens       *prometheus.CounterVec
	breaker      *prometheus.GaugeVec
	inflight     prometheus.Gauge
}

// Fine-grained low buckets: gateway overhead is expected in the
// sub-millisecond to few-millisecond range, where default buckets are blind.
var overheadBuckets = []float64{.0001, .00025, .0005, .001, .002, .003, .005, .0075, .01, .015, .025, .05, .1, .25, .5}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := func(c prometheus.Collector) { reg.MustRegister(c) }
	m := &Metrics{reg: reg}

	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_requests_total", Help: "Chat requests by outcome.",
	}, []string{"model", "provider", "status", "cache"})
	m.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_http_requests_total", Help: "All HTTP requests, including ones rejected before the chat handler (e.g. 401).",
	}, []string{"route", "code"})
	m.provErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_provider_errors_total", Help: "Failed requests by the provider that last failed and error class.",
	}, []string{"provider", "class"})
	m.overhead = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "tollgate_overhead_seconds", Help: "Gateway time excluding upstream calls: total latency minus upstream time.",
		Buckets: overheadBuckets,
	})
	m.upstream = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "tollgate_upstream_seconds", Help: "Time spent in upstream calls including retries and fallback.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 14), // 10ms .. ~80s
	}, []string{"provider"})
	m.ttft = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "tollgate_ttft_seconds", Help: "Streaming time to first token, from request receipt.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
	}, []string{"provider"})
	m.cacheReqs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_cache_requests_total", Help: "Response cache lookups by result (HIT, MISS); BYPASS when not cacheable.",
	}, []string{"result"})
	m.cost = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_cost_usd_total", Help: "Upstream spend in USD. Per day: increase(...[1d]).",
	}, []string{"project"})
	m.costSaved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_cost_saved_usd_total", Help: "USD not spent thanks to cache hits.",
	}, []string{"project"})
	m.tokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_tokens_total", Help: "Tokens processed.",
	}, []string{"project", "direction"})
	m.breaker = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tollgate_breaker_state", Help: "Circuit breaker state per provider: 0 closed, 1 open, 2 half-open.",
	}, []string{"provider"})
	m.inflight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tollgate_inflight_requests", Help: "Requests currently being served.",
	})
	for _, c := range []prometheus.Collector{m.requests, m.httpRequests, m.provErrors, m.overhead, m.upstream, m.ttft,
		m.cacheReqs, m.cost, m.costSaved, m.tokens, m.breaker, m.inflight} {
		f(c)
	}
	return m
}

// UsageGauges exports the ledger's health from the writer's own counters.
func (m *Metrics) UsageGauges(dropped, written func() int64, buffered func() int) {
	m.reg.MustRegister(
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "tollgate_usage_dropped_total", Help: "Ledger rows dropped (buffer full or DB failure)."},
			func() float64 { return float64(dropped()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "tollgate_usage_written_total", Help: "Ledger rows written."},
			func() float64 { return float64(written()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "tollgate_usage_buffer_len", Help: "Ledger rows waiting in memory."},
			func() float64 { return float64(buffered()) }),
	)
}

// Observe implements server.Observer.
func (m *Metrics) Observe(o server.Outcome) {
	model, provider := o.Model, o.Provider
	if provider == "" { // never routed: o.Model may be any client string
		model, provider = "none", "none"
	}
	m.requests.WithLabelValues(model, provider, strconv.Itoa(o.Status), o.Cache).Inc()
	// Interrupted streams end with status 200 (headers were already sent),
	// but they are provider failures all the same.
	if o.ErrorClass != "" && (o.Status >= 500 || o.ErrorClass == "stream_interrupted") {
		m.provErrors.WithLabelValues(provider, o.ErrorClass).Inc()
	}
	m.overhead.Observe((o.Latency - o.Upstream).Seconds())
	if o.Upstream > 0 {
		m.upstream.WithLabelValues(provider).Observe(o.Upstream.Seconds())
	}
	if o.TTFT > 0 {
		m.ttft.WithLabelValues(provider).Observe(o.TTFT.Seconds())
	}
	if o.Cache != "BYPASS" {
		m.cacheReqs.WithLabelValues(o.Cache).Inc()
	}
	if o.ProjectName != "" {
		m.cost.WithLabelValues(o.ProjectName).Add(o.CostUSD)
		m.costSaved.WithLabelValues(o.ProjectName).Add(o.CostSavedUSD)
		m.tokens.WithLabelValues(o.ProjectName, "in").Add(float64(o.TokensIn))
		m.tokens.WithLabelValues(o.ProjectName, "out").Add(float64(o.TokensOut))
	}
}

func (m *Metrics) BreakerChanged(provider string, s router.State) {
	m.breaker.WithLabelValues(provider).Set(float64(s))
}

// Middleware counts every HTTP request (including auth failures that never
// reach the chat handler) and tracks in-flight requests. The route label is
// the mux pattern, a fixed set, never the raw path.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.inflight.Inc()
		defer m.inflight.Dec()
		rec := &codeRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		route := "other"
		switch r.URL.Path {
		case "/v1/chat/completions", "/healthz", "/readyz":
			route = r.URL.Path
		}
		m.httpRequests.WithLabelValues(route, strconv.Itoa(rec.code)).Inc()
	})
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

type codeRecorder struct {
	http.ResponseWriter
	code int
}

func (r *codeRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *codeRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

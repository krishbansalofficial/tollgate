package telemetry

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/krishbansalofficial/tollgate/internal/server"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestObserve(t *testing.T) {
	m := NewMetrics()
	m.Observe(server.Outcome{Model: "mock/small", Provider: "mock", Status: 200, Cache: "MISS", ProjectName: "p",
		Latency: 52 * time.Millisecond, Upstream: 50 * time.Millisecond, CostUSD: 0.01, TokensIn: 3, TokensOut: 7})
	m.Observe(server.Outcome{Model: "attacker-controlled-string", Status: 404, Cache: "BYPASS", ProjectName: "p"})
	m.Observe(server.Outcome{Model: "mock/small", Provider: "mock", Status: 502, ErrorClass: "upstream_error", Cache: "BYPASS", ProjectName: "p"})

	out := scrape(t, m)
	for _, want := range []string{
		`tollgate_requests_total{cache="MISS",model="mock/small",provider="mock",status="200"} 1`,
		`tollgate_requests_total{cache="BYPASS",model="none",provider="none",status="404"} 1`,
		`tollgate_provider_errors_total{class="upstream_error",provider="mock"} 1`,
		`tollgate_cost_usd_total{project="p"} 0.01`,
		`tollgate_tokens_total{direction="out",project="p"} 7`,
		`tollgate_cache_requests_total{result="MISS"} 1`,
		`tollgate_overhead_seconds_bucket{le="0.001"} 2`, // the two zero-latency outcomes
		`tollgate_overhead_seconds_bucket{le="0.002"} 3`, // + 52ms - 50ms upstream = 2ms (buckets are cumulative)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, "attacker-controlled-string") {
		t.Error("unrouted client model string leaked into a label")
	}
}

func TestMiddlewareRoutesBounded(t *testing.T) {
	m := NewMetrics()
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	for _, p := range []string{"/v1/chat/completions", "/random/123", "/random/456"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, p, nil))
	}
	out := scrape(t, m)
	if !strings.Contains(out, `tollgate_http_requests_total{code="401",route="other"} 2`) || strings.Contains(out, "random") {
		t.Errorf("route label not bounded:\n%s", out)
	}
}

func TestTraceHandlerAddsIDs(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("t").Start(context.Background(), "s")
	defer span.End()
	var buf bytes.Buffer
	log := slog.New(TraceHandler{slog.NewJSONHandler(&buf, nil)}).With("k", "v")
	log.InfoContext(ctx, "hello")
	if !strings.Contains(buf.String(), span.SpanContext().TraceID().String()) || !strings.Contains(buf.String(), `"k":"v"`) {
		t.Fatalf("log line: %s", buf.String())
	}
}

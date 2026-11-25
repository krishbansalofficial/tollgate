package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// SetupTracing installs a global tracer provider exporting OTLP/gRPC to
// endpoint (the in-cluster collector). With an empty endpoint it installs
// nothing: the global no-op tracer makes every span call free.
//
// Sampling: the SDK samples at `ratio` (default 1.0) and the collector's
// tail_sampling keeps all error traces plus a fraction of the rest. Head
// sampling below 1.0 would drop error traces before the collector ever saw
// them, defeating "keep all errors".
func SetupTracing(ctx context.Context, endpoint string, ratio float64, version string) (shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	// Schemaless: merging with resource.Default() fails at runtime if the two
	// carry different semconv schema URLs.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName("tollgate"), semconv.ServiceVersion(version)))
	if err != nil {
		return nil, err
	}
	// BatchSpanProcessor exports from a background goroutine with a bounded
	// queue: like the usage ledger, tracing never blocks a request, and spans
	// are dropped (not queued forever) if the collector is down.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// TraceHandler adds trace_id and span_id to every log record made with a
// context that carries a span, so logs and traces join on trace_id.
type TraceHandler struct{ slog.Handler }

func (h TraceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h TraceHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return TraceHandler{h.Handler.WithAttrs(a)}
}
func (h TraceHandler) WithGroup(n string) slog.Handler { return TraceHandler{h.Handler.WithGroup(n)} }

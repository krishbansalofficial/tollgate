//go:build integration

package usage

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/krishbansalofficial/tollgate/migrations"
)

func TestPGSinkCopy(t *testing.T) {
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("tollgate"), tcpostgres.WithUsername("t"), tcpostgres.WithPassword("t"),
		tcpostgres.BasicWaitStrategies())
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal(err)
	}
	dsn, _ := ctr.ConnectionString(ctx, "sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	prov, err := goose.NewProvider(goose.DialectPostgres, stdlib.OpenDBFromPool(pool), migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Up(ctx); err != nil {
		t.Fatal(err)
	}

	w := NewWriter(NewPGSink(pool), 10_000, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.Start()
	ttft := 42
	const n = 1234
	for i := range n {
		e := Event{
			TS: time.Now(), RequestID: "r", ProjectID: "7f1a9c3e-0000-4000-8000-000000000001", KeyID: "7f1a9c3e-0000-4000-8000-000000000002",
			Model: "mock/small", Provider: "mock", TokensIn: 10, TokensOut: 5, LatencyMS: 3, UpstreamMS: 2,
			CostUSD: 0.000123, Cache: "MISS", Status: 200, Attempts: 1,
		}
		if i == 0 {
			e.TTFTMS, e.ErrorClass, e.Stream = &ttft, "upstream_error", true
		}
		w.Record(e)
	}
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}

	var rows int
	var cost float64
	var ttfts int
	err = pool.QueryRow(ctx, `SELECT count(*), sum(cost_usd)::float8, count(ttft_ms) FROM usage_events`).Scan(&rows, &cost, &ttfts)
	if err != nil {
		t.Fatal(err)
	}
	if rows != n || ttfts != 1 || cost < 0.15 || cost > 0.16 { // 1234 × 0.000123 = 0.151782
		t.Fatalf("rows=%d cost=%f ttfts=%d", rows, cost, ttfts)
	}

	// MonthSpend: only this UTC month counts, per project, and is cached.
	const project = "7f1a9c3e-0000-4000-8000-000000000001"
	if _, err := pool.Exec(ctx, `INSERT INTO usage_events (ts, request_id, project_id, key_id, model, provider, tokens_in, tokens_out,
		usage_estimated, latency_ms, upstream_ms, cost_usd, cost_saved_usd, cache, status, attempts, stream)
		SELECT date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - interval '1 second', 'old', project_id, key_id, model, provider,
		       0, 0, false, 0, 0, 99, 0, 'MISS', 200, 1, false FROM usage_events LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	ms := NewMonthSpend(pool, time.Hour)
	got, err := ms.Get(ctx, project)
	if err != nil || got < 0.15 || got > 0.16 {
		t.Fatalf("month spend = %f, %v; want ~0.1518 (last month's $99 excluded)", got, err)
	}
	if other, err := ms.Get(ctx, "7f1a9c3e-0000-4000-8000-0000000000ff"); err != nil || other != 0 {
		t.Fatalf("other project spend = %f, %v; want 0", other, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE usage_events SET cost_usd = 1 WHERE request_id = 'r'`); err != nil {
		t.Fatal(err)
	}
	if again, _ := ms.Get(ctx, project); again != got {
		t.Fatalf("cached spend changed within ttl: %f -> %f", got, again)
	}
}

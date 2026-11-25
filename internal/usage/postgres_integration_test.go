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
}

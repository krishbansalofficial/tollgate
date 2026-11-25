package usage

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGSink writes batches with COPY (pgx CopyFrom): one round trip streams all
// rows, typically an order of magnitude faster than a multi-row INSERT.
type PGSink struct{ pool *pgxpool.Pool }

func NewPGSink(pool *pgxpool.Pool) *PGSink { return &PGSink{pool: pool} }

var columns = []string{
	"ts", "request_id", "project_id", "key_id", "model", "provider", "tokens_in", "tokens_out",
	"usage_estimated", "latency_ms", "upstream_ms", "ttft_ms", "cost_usd", "cost_saved_usd",
	"cache", "status", "error_class", "attempts", "stream",
}

func (s *PGSink) Insert(ctx context.Context, batch []Event) error {
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"usage_events"}, columns, pgx.CopyFromSlice(len(batch), func(i int) ([]any, error) {
		e := batch[i]
		var ttft, errClass any // nil → SQL NULL
		if e.TTFTMS != nil {
			ttft = *e.TTFTMS
		}
		if e.ErrorClass != "" {
			errClass = e.ErrorClass
		}
		return []any{
			e.TS, e.RequestID, e.ProjectID, e.KeyID, e.Model, e.Provider, e.TokensIn, e.TokensOut,
			e.UsageEstimated, e.LatencyMS, e.UpstreamMS, ttft, e.CostUSD, e.CostSavedUSD,
			e.Cache, e.Status, errClass, e.Attempts, e.Stream,
		}, nil
	}))
	return err
}

package usage

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MonthSpend sums each project's ledger cost for the current UTC calendar
// month, caching the result per project for ttl so the budget check costs
// one query per project per ttl, not one per request.
type MonthSpend struct {
	pool *pgxpool.Pool
	ttl  time.Duration
	mu   sync.Mutex
	m    map[string]spendEntry
}

type spendEntry struct {
	usd     float64
	expires time.Time
}

func NewMonthSpend(pool *pgxpool.Pool, ttl time.Duration) *MonthSpend {
	return &MonthSpend{pool: pool, ttl: ttl, m: map[string]spendEntry{}}
}

func (s *MonthSpend) Get(ctx context.Context, projectID string) (float64, error) {
	now := time.Now()
	s.mu.Lock()
	e, ok := s.m[projectID]
	s.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.usd, nil
	}
	var usd float64
	// Served by usage_events_project_ts_idx.
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(cost_usd), 0)::float8 FROM usage_events
		WHERE project_id = $1 AND ts >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`,
		projectID).Scan(&usd)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.m[projectID] = spendEntry{usd: usd, expires: now.Add(s.ttl)}
	s.mu.Unlock()
	return usd, nil
}

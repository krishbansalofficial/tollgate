package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newMini(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	m := miniredis.RunT(t)
	m.SetTime(time.Unix(1_700_000_000, 0))
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return m, rdb
}

func TestAllowRequestsPerMinute(t *testing.T) {
	m, rdb := newMini(t)
	l := New(rdb, true)
	ctx := context.Background()

	for i := range 3 {
		d, err := l.Allow(ctx, "k", 3, 0, 0)
		if err != nil || !d.Allowed || d.RemainingRequests != 2-i || d.RemainingTokens != -1 {
			t.Fatalf("request %d: %+v %v", i, d, err)
		}
	}
	d, _ := l.Allow(ctx, "k", 3, 0, 0)
	if d.Allowed || d.RetryAfter != 20*time.Second { // 1 request refills every 60s/3
		t.Fatalf("4th request: %+v", d)
	}
	m.SetTime(time.Unix(1_700_000_000, 0).Add(20 * time.Second))
	if d, _ := l.Allow(ctx, "k", 3, 0, 0); !d.Allowed {
		t.Fatalf("after refill: %+v", d)
	}
}

func TestAllowAllOrNothing(t *testing.T) {
	_, rdb := newMini(t)
	l := New(rdb, true)
	ctx := context.Background()

	// Token bucket denies: the request bucket must not be charged.
	if d, _ := l.Allow(ctx, "k", 5, 100, 100); !d.Allowed || d.RemainingRequests != 4 {
		t.Fatalf("first: %+v", d)
	}
	if d, _ := l.Allow(ctx, "k", 5, 100, 50); d.Allowed {
		t.Fatalf("token bucket should deny: %+v", d)
	}
	if d, _ := l.Allow(ctx, "k", 5, 0, 0); d.RemainingRequests != 3 {
		t.Fatalf("denied request consumed a request slot: %+v", d)
	}
}

func TestReservationCappedAtCapacity(t *testing.T) {
	_, rdb := newMini(t)
	d, _ := New(rdb, true).Allow(context.Background(), "k", 0, 100, 10_000)
	if !d.Allowed || d.Reserved != 100 {
		t.Fatalf("oversized reservation should be capped and admitted on a full bucket: %+v", d)
	}
}

func TestReconcileRefundAndDebt(t *testing.T) {
	_, rdb := newMini(t)
	l := New(rdb, true)
	ctx := context.Background()

	d, _ := l.Allow(ctx, "k", 0, 1000, 800) // reserve 800 of 1000
	if d.RemainingTokens != 200 {
		t.Fatalf("%+v", d)
	}
	if err := l.Reconcile(ctx, "k", 1000, 700); err != nil { // only used 100
		t.Fatal(err)
	}
	if d, _ := l.Allow(ctx, "k", 0, 1000, 0); d.RemainingTokens != 900 {
		t.Fatalf("after refund: %+v", d)
	}
	// Under-estimate: charge 1500 more than reserved, bucket goes into debt.
	_ = l.Reconcile(ctx, "k", 1000, -1500)
	d, _ = l.Allow(ctx, "k", 0, 1000, 1)
	if d.Allowed || d.RemainingTokens >= 0 {
		t.Fatalf("bucket in debt should deny: %+v", d)
	}
}

func TestFailModes(t *testing.T) {
	m, rdb := newMini(t)
	m.Close() // Redis is down
	for _, failOpen := range []bool{true, false} {
		d, err := New(rdb, failOpen).Allow(context.Background(), "k", 10, 10, 1)
		if err == nil || !d.Degraded || d.Allowed != failOpen {
			t.Errorf("failOpen=%v: %+v err=%v", failOpen, d, err)
		}
	}
}

// testConcurrentAdmission is the property that matters: with N goroutines
// racing on one key, exactly `limit` are admitted. Shared with the
// testcontainers variant so it runs against real Redis in CI.
func testConcurrentAdmission(t *testing.T, rdb redis.Scripter) {
	t.Helper()
	const limit, workers = 50, 200
	l := New(rdb, false)
	l.timeout = 5 * time.Second // the test measures correctness, not latency
	var admitted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		wg.Go(func() {
			<-start // release all goroutines at once to maximise contention
			d, err := l.Allow(context.Background(), "hot", limit, 0, 0)
			if err != nil {
				t.Error(err)
			}
			if d.Allowed {
				admitted.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := admitted.Load(); got != limit {
		t.Fatalf("admitted %d of %d concurrent requests, want exactly %d", got, workers, limit)
	}
}

func TestConcurrentAdmissionMiniredis(t *testing.T) {
	_, rdb := newMini(t)
	testConcurrentAdmission(t, rdb)
}

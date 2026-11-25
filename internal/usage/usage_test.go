package usage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type memSink struct {
	mu      sync.Mutex
	rows    []Event
	batches int
	failN   int // fail this many Insert calls first
	block   chan struct{}
}

func (m *memSink) Insert(_ context.Context, b []Event) error {
	if m.block != nil {
		<-m.block
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failN > 0 {
		m.failN--
		return errors.New("db down")
	}
	m.rows = append(m.rows, b...)
	m.batches++
	return nil
}

func (m *memSink) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

func newTestWriter(s Sink, buf int) *Writer {
	w := NewWriter(s, buf, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.backoff = time.Millisecond
	return w
}

func TestBatchingAndShutdownFlush(t *testing.T) {
	s := &memSink{}
	w := newTestWriter(s, 10_000)
	w.batchSize = 100
	w.flushEvery = time.Hour // only size and shutdown trigger flushes here
	w.Start()
	for i := range 250 {
		if !w.Record(Event{Status: i}) {
			t.Fatal("record rejected")
		}
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.count() != 250 || s.batches != 3 { // 100 + 100 + 50 on shutdown
		t.Fatalf("rows=%d batches=%d", s.count(), s.batches)
	}
	if w.Record(Event{}) || w.Dropped() != 1 {
		t.Fatal("record after close must be dropped, not panic")
	}
}

func TestTimeFlush(t *testing.T) {
	s := &memSink{}
	w := newTestWriter(s, 100)
	w.flushEvery = 10 * time.Millisecond
	w.Start()
	defer func() { _ = w.Close(context.Background()) }()
	w.Record(Event{})
	for range 100 {
		if s.count() == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("single row never flushed by the ticker")
}

func TestFullBufferDropsWithoutBlocking(t *testing.T) {
	s := &memSink{block: make(chan struct{})} // writer stuck in Insert
	w := newTestWriter(s, 5)
	w.batchSize = 1
	w.Start()
	w.Record(Event{}) // taken by the writer, which then blocks
	time.Sleep(10 * time.Millisecond)

	start := time.Now()
	accepted := 0
	for range 100 {
		if w.Record(Event{}) {
			accepted++
		}
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("Record blocked on a full buffer")
	}
	if accepted != 5 || w.Dropped() != 95 {
		t.Fatalf("accepted=%d dropped=%d", accepted, w.Dropped())
	}
	close(s.block)
	_ = w.Close(context.Background())
}

func TestRetryThenSucceed(t *testing.T) {
	s := &memSink{failN: 2}
	w := newTestWriter(s, 10)
	w.Start()
	w.Record(Event{})
	_ = w.Close(context.Background())
	if s.count() != 1 || w.Dropped() != 0 {
		t.Fatalf("rows=%d dropped=%d", s.count(), w.Dropped())
	}
}

func TestDropAfterRetriesExhausted(t *testing.T) {
	s := &memSink{failN: 100}
	w := newTestWriter(s, 10)
	w.Start()
	w.Record(Event{})
	w.Record(Event{})
	_ = w.Close(context.Background())
	if w.Dropped() != 2 {
		t.Fatalf("dropped=%d, want 2", w.Dropped())
	}
}

// TestConcurrentRecordAndClose: Close racing with many Record calls must
// never panic (send on closed channel). Run with -race in CI.
func TestConcurrentRecordAndClose(t *testing.T) {
	w := newTestWriter(&memSink{}, 1000)
	w.Start()
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 100 {
				w.Record(Event{})
			}
		})
	}
	_ = w.Close(context.Background())
	wg.Wait()
	if got := w.Written() + w.Dropped(); got != 5000 {
		t.Fatalf("written+dropped = %d, want 5000 (every event accounted for)", got)
	}
}

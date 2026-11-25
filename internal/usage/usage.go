// Package usage is the asynchronous usage ledger: one row per request,
// buffered in memory and written to Postgres in COPY batches, so the request
// path never waits on the database.
package usage

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

type Event struct {
	TS                    time.Time
	RequestID             string
	ProjectID, KeyID      string
	Model, Provider       string
	TokensIn, TokensOut   int
	UsageEstimated        bool
	LatencyMS, UpstreamMS int
	TTFTMS                *int // nil unless streaming
	CostUSD, CostSavedUSD float64
	Cache                 string
	Status                int
	ErrorClass            string
	Attempts              int
	Stream                bool
}

// Sink persists a batch. It must be safe to call from one goroutine.
type Sink interface {
	Insert(ctx context.Context, batch []Event) error
}

type Writer struct {
	sink       Sink
	log        *slog.Logger
	batchSize  int
	flushEvery time.Duration
	retries    int
	backoff    time.Duration

	// mu protects the "closed" transition. Record holds the read lock while
	// it does its non-blocking send; Close takes the write lock before
	// closing the channel. That makes "send on closed channel" impossible
	// even if Close runs while a straggling handler is still recording
	// (e.g. http.Server.Shutdown timed out). An RLock is uncontended in the
	// steady state, so the cost on the hot path is a couple of atomics.
	mu     sync.RWMutex
	closed bool
	ch     chan Event
	done   chan struct{}

	dropped atomic.Int64
	written atomic.Int64
}

func NewWriter(sink Sink, buffer int, log *slog.Logger) *Writer {
	return &Writer{
		sink: sink, log: log, batchSize: 500, flushEvery: time.Second, retries: 3, backoff: 200 * time.Millisecond,
		ch: make(chan Event, buffer), done: make(chan struct{}),
	}
}

// Start launches the single writer goroutine. One writer keeps batches
// large and Postgres connections few; COPY of 500 rows takes milliseconds,
// so one goroutine sustains far more rows/s than the gateway serves.
func (w *Writer) Start() { go w.run() }

// Record enqueues an event without ever blocking. When the buffer is full
// (Postgres slow or down for longer than the buffer covers) the event is
// dropped and counted: a lost ledger row is better than adding database
// latency to a user's request. The drop counter is exported and alerted on.
func (w *Writer) Record(e Event) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.dropped.Add(1)
		return false
	}
	select {
	case w.ch <- e:
		return true
	default:
		w.dropped.Add(1)
		return false
	}
}

// Close stops intake, flushes what is buffered, and waits for the writer
// (or ctx). Call it after the HTTP server has drained, so in-flight requests
// get their rows in.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.ch) // the writer drains the remaining buffer, then exits
	}
	w.mu.Unlock()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Writer) Dropped() int64 { return w.dropped.Load() }
func (w *Writer) Written() int64 { return w.written.Load() }
func (w *Writer) Buffered() int  { return len(w.ch) }

func (w *Writer) run() {
	defer close(w.done)
	batch := make([]Event, 0, w.batchSize)
	tick := time.NewTicker(w.flushEvery)
	defer tick.Stop()
	for {
		select {
		case e, ok := <-w.ch:
			if !ok { // closed and fully drained
				w.flush(batch)
				return
			}
			batch = append(batch, e)
			if len(batch) >= w.batchSize {
				batch = w.flush(batch)
			}
		case <-tick.C:
			// Time-based flush bounds how stale the ledger gets at low traffic.
			batch = w.flush(batch)
		}
	}
}

// flush writes the batch with bounded retries and returns the emptied slice
// (reusing its backing array to avoid allocating per batch).
func (w *Writer) flush(batch []Event) []Event {
	if len(batch) == 0 {
		return batch
	}
	var err error
	for attempt := range w.retries {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = w.sink.Insert(ctx, batch)
		cancel()
		if err == nil {
			w.written.Add(int64(len(batch)))
			return batch[:0]
		}
		time.Sleep(w.backoff << attempt)
	}
	w.dropped.Add(int64(len(batch)))
	w.log.Error("usage ledger: dropping batch after retries", "rows", len(batch), "err", err)
	return batch[:0]
}

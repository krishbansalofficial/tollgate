package router

import (
	"sync"
	"time"
)

type State int

const (
	Closed   State = iota // normal: requests flow
	Open                  // failing: requests skip this provider instantly
	HalfOpen              // cooling down: one probe request decides
)

func (s State) String() string { return [...]string{"closed", "open", "half_open"}[s] }

// Breaker is a per-provider circuit breaker.
//
// Closed → Open: within a rolling window, at least minFailures failures
// that make up at least failureRatio of all outcomes.
// Open → HalfOpen: after the cooldown, the next Allow admits one probe.
// HalfOpen → Closed on probe success; → Open (with doubled cooldown, capped)
// on probe failure.
//
// Concurrency: a single mutex guards everything. Every request touches the
// breaker twice (Allow, then Record), each a few comparisons under the lock,
// so contention is negligible next to an upstream call measured in
// milliseconds; a lock-free version would be harder to reason about for no
// measurable gain.
type Breaker struct {
	mu  sync.Mutex
	now func() time.Time

	window       time.Duration
	minFailures  int
	failureRatio float64
	baseCooldown time.Duration
	maxCooldown  time.Duration
	onChange     func(State)

	state       State
	windowStart time.Time
	total, fail int
	cooldown    time.Duration
	openUntil   time.Time
	probing     bool // half-open: a probe is in flight
}

func NewBreaker(onChange func(State)) *Breaker {
	if onChange == nil {
		onChange = func(State) {}
	}
	return &Breaker{
		now: time.Now, window: 10 * time.Second, minFailures: 5, failureRatio: 0.5,
		baseCooldown: 15 * time.Second, maxCooldown: 2 * time.Minute, cooldown: 15 * time.Second,
		onChange: onChange,
	}
}

// Allow reports whether a request may be sent. Every true result must be
// followed by exactly one Record or Release, or a half-open probe slot leaks.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Open:
		if b.now().Before(b.openUntil) {
			return false
		}
		b.setState(HalfOpen)
		b.probing = true
		return true
	case HalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
		return true
	default:
		return true
	}
}

// Record reports the outcome of an allowed request. "Success" means the
// provider is healthy, which includes it correctly rejecting a bad request.
func (b *Breaker) Record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	switch b.state {
	case HalfOpen:
		b.probing = false
		if success {
			b.cooldown = b.baseCooldown
			b.resetWindow(now)
			b.setState(Closed)
		} else {
			b.trip(now)
		}
	case Closed:
		if now.Sub(b.windowStart) >= b.window {
			b.resetWindow(now)
		}
		b.total++
		if !success {
			b.fail++
		}
		if b.fail >= b.minFailures && float64(b.fail) >= b.failureRatio*float64(b.total) {
			b.trip(now)
		}
	case Open:
		// A late result from a request admitted before the trip: ignore.
	}
}

// Release returns an allowed slot without an outcome (e.g. the client hung
// up), so the probe slot isn't held forever and health isn't misjudged.
func (b *Breaker) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == HalfOpen {
		b.probing = false
	}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Breaker) trip(now time.Time) {
	b.openUntil = now.Add(b.cooldown)
	b.cooldown = min(b.maxCooldown, 2*b.cooldown) // the next consecutive trip waits longer
	b.setState(Open)
}

func (b *Breaker) resetWindow(now time.Time) { b.windowStart, b.total, b.fail = now, 0, 0 }

func (b *Breaker) setState(s State) {
	if b.state != s {
		b.state = s
		b.onChange(s)
	}
}

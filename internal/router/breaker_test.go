package router

import (
	"testing"
	"time"
)

func newTestBreaker() (*Breaker, *time.Time, *[]State) {
	now := time.Unix(0, 0)
	var changes []State
	b := NewBreaker(func(s State) { changes = append(changes, s) })
	b.now = func() time.Time { return now }
	return b, &now, &changes
}

func TestBreakerTransitions(t *testing.T) {
	b, now, changes := newTestBreaker()

	// 4 failures: below minFailures, stays closed.
	for range 4 {
		b.Allow()
		b.Record(false)
	}
	if b.State() != Closed {
		t.Fatal("tripped below minFailures")
	}
	b.Allow()
	b.Record(false) // 5th failure, ratio 100%
	if b.State() != Open || b.Allow() {
		t.Fatal("should be open and rejecting")
	}

	// After the cooldown: exactly one probe.
	*now = now.Add(15 * time.Second)
	if !b.Allow() || b.State() != HalfOpen {
		t.Fatal("first request after cooldown should probe")
	}
	if b.Allow() {
		t.Fatal("second concurrent probe admitted")
	}
	b.Record(false) // probe fails: reopen with doubled cooldown
	*now = now.Add(15 * time.Second)
	if b.Allow() {
		t.Fatal("cooldown should have doubled to 30s")
	}
	*now = now.Add(15 * time.Second)
	if !b.Allow() {
		t.Fatal("probe after 30s")
	}
	b.Record(true)
	if b.State() != Closed {
		t.Fatal("successful probe should close")
	}
	want := []State{Open, HalfOpen, Open, HalfOpen, Closed}
	if len(*changes) != len(want) {
		t.Fatalf("changes = %v, want %v", *changes, want)
	}
	for i := range want {
		if (*changes)[i] != want[i] {
			t.Fatalf("changes = %v, want %v", *changes, want)
		}
	}
}

func TestBreakerRatioAndWindow(t *testing.T) {
	b, now, _ := newTestBreaker()
	// 5 failures among 20 outcomes = 25% < 50%: stays closed.
	for i := range 20 {
		b.Allow()
		b.Record(i%4 != 0)
	}
	if b.State() != Closed {
		t.Fatal("tripped below failure ratio")
	}
	// Failures spread across windows never accumulate.
	for range 10 {
		*now = now.Add(11 * time.Second)
		for range 4 {
			b.Allow()
			b.Record(false)
		}
	}
	if b.State() != Closed {
		t.Fatal("failures from old windows were counted")
	}
}

func TestBreakerReleaseFreesProbe(t *testing.T) {
	b, now, _ := newTestBreaker()
	for range 5 {
		b.Allow()
		b.Record(false)
	}
	*now = now.Add(time.Minute)
	b.Allow()
	b.Release() // client hung up mid-probe
	if !b.Allow() {
		t.Fatal("released probe slot not reusable")
	}
}

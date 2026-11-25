package router

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
	"github.com/krishbansalofficial/tollgate/internal/providers"
)

// ChatStream shares Chat's error/delay script for the pre-stream phase, then
// yields three chunks spaced by chunkDelay (zero chunks if emptyStream).
func (s *scripted) ChatStream(ctx context.Context, req *api.ChatRequest) (providers.Stream, error) {
	if _, err := s.Chat(ctx, req); err != nil {
		return nil, err
	}
	n := 3
	if s.emptyStream {
		n = 0
	}
	return &fakeStream{ctx: ctx, left: n, delay: s.chunkDelay, name: s.name}, nil
}

type fakeStream struct {
	ctx    context.Context
	left   int
	delay  time.Duration
	name   string
	closed bool
}

func (f *fakeStream) Recv() (*api.ChatChunk, error) {
	if f.left == 0 {
		return nil, io.EOF
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-f.ctx.Done():
			return nil, f.ctx.Err()
		}
	}
	f.left--
	return api.TextChunk("", f.name, ""), nil
}

func (f *fakeStream) Close() error { f.closed = true; return nil }

func drain(t *testing.T, s providers.Stream) (string, error) {
	t.Helper()
	defer func() { _ = s.Close() }()
	out := ""
	for {
		c, err := s.Recv()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out += c.Choices[0].Delta.Content
	}
}

func TestStreamFallbackBeforeFirstChunk(t *testing.T) {
	a := &scripted{name: "a", errs: []error{perr(500)}}
	b := &scripted{name: "b"}
	r := newRouter(Config{MaxAttempts: 1, AttemptTimeout: time.Second, Fallback: "b/small"}, a, b)
	res, err := r.ChatStream(context.Background(), req("a/big"))
	if err != nil || res.Model != "b/small" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got, err := drain(t, res.Stream); err != nil || got != "bbb" {
		t.Fatalf("got %q %v (first chunk must be replayed)", got, err)
	}
}

func TestStreamEmptyFallsBack(t *testing.T) {
	a := &scripted{name: "a", emptyStream: true}
	b := &scripted{name: "b"}
	r := newRouter(Config{MaxAttempts: 1, AttemptTimeout: time.Second, Fallback: "b/small"}, a, b)
	res, err := r.ChatStream(context.Background(), req("a/big"))
	if err != nil || res.Model != "b/small" {
		t.Fatalf("a stream that ends before any chunk must fall back: res=%+v err=%v", res, err)
	}
	_ = res.Stream.Close()
}

func TestStreamFirstChunkTimeoutFallsBack(t *testing.T) {
	a := &scripted{name: "a", chunkDelay: time.Second}
	b := &scripted{name: "b"}
	r := newRouter(Config{MaxAttempts: 1, AttemptTimeout: 30 * time.Millisecond, Fallback: "b/small"}, a, b)
	res, err := r.ChatStream(context.Background(), req("a/big"))
	if err != nil || res.Model != "b/small" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	_ = res.Stream.Close()
}

// The attempt timeout bounds time-to-first-chunk only: a stream that started
// on time must not be cut off later by the same timer.
func TestStreamOutlivesAttemptTimeout(t *testing.T) {
	a := &scripted{name: "a", chunkDelay: 20 * time.Millisecond} // 3 chunks ≈ 60ms total
	r := newRouter(Config{MaxAttempts: 1, AttemptTimeout: 30 * time.Millisecond}, a)
	res, err := r.ChatStream(context.Background(), req("a/big"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := drain(t, res.Stream); err != nil || got != "aaa" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestStreamCloseCancelsUpstream(t *testing.T) {
	a := &scripted{name: "a", chunkDelay: time.Millisecond}
	r := newRouter(Config{MaxAttempts: 1, AttemptTimeout: time.Second}, a)
	res, err := r.ChatStream(context.Background(), req("a/big"))
	if err != nil {
		t.Fatal(err)
	}
	inner := res.Stream.(*cancelOnClose).Stream.(*primedStream).Stream.(*fakeStream)
	_ = res.Stream.Close()
	if !inner.closed || !errors.Is(inner.ctx.Err(), context.Canceled) {
		t.Fatal("closing the stream must close it and cancel its attempt context")
	}
}

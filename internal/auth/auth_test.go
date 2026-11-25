package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore counts lookups and can block or fail on demand.
type fakeStore struct {
	keys    map[[32]byte]Key
	calls   atomic.Int64
	err     error
	release chan struct{} // if non-nil, LookupKey waits on it
}

func (f *fakeStore) LookupKey(_ context.Context, hash []byte) (Key, error) {
	f.calls.Add(1)
	if f.release != nil {
		<-f.release
	}
	if f.err != nil {
		return Key{}, f.err
	}
	k, ok := f.keys[[32]byte(hash)]
	if !ok {
		return Key{}, ErrNotFound
	}
	return k, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func setup(t *testing.T) (*Authenticator, *fakeStore, *clock, string) {
	t.Helper()
	raw, h, _, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeStore{keys: map[[32]byte]Key{h: {ID: "k1", ProjectName: "p"}}}
	c := &clock{t: time.Unix(0, 0)}
	a := NewAuthenticator(st, 30*time.Second)
	a.now = c.now
	return a, st, c, raw
}

func TestGenerate(t *testing.T) {
	raw, h, display, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !wellFormed(raw) || !strings.HasPrefix(raw, display) || Hash(raw) != h {
		t.Errorf("raw=%q display=%q", raw, display)
	}
	raw2, _, _, _ := Generate()
	if raw == raw2 {
		t.Error("two generated keys are equal")
	}
}

func TestAuthenticateCaching(t *testing.T) {
	a, st, c, raw := setup(t)
	ctx := context.Background()

	for range 3 {
		if k, err := a.Authenticate(ctx, raw); err != nil || k.ID != "k1" {
			t.Fatalf("got %v %v", k, err)
		}
	}
	if st.calls.Load() != 1 {
		t.Fatalf("store calls = %d, want 1 (cache hit)", st.calls.Load())
	}
	c.advance(31 * time.Second)
	if _, err := a.Authenticate(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if st.calls.Load() != 2 {
		t.Fatalf("store calls = %d, want 2 (TTL expired)", st.calls.Load())
	}
}

func TestAuthenticateRejections(t *testing.T) {
	a, st, c, _ := setup(t)
	ctx := context.Background()
	unknown, _, _, _ := Generate()

	tests := []struct{ name, key string }{
		{"empty", ""},
		{"wrong prefix", "sk_" + strings.Repeat("a", 40)},
		{"too long", "tg_" + strings.Repeat("a", 100)},
		{"unknown", unknown},
	}
	for _, tt := range tests {
		if _, err := a.Authenticate(ctx, tt.key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: got %v, want ErrInvalidKey", tt.name, err)
		}
	}
	if st.calls.Load() != 1 {
		t.Fatalf("store calls = %d; malformed keys must not reach the store", st.calls.Load())
	}

	// Unknown key is negatively cached for 5s, then looked up again.
	_, _ = a.Authenticate(ctx, unknown)
	if st.calls.Load() != 1 {
		t.Fatal("negative cache miss")
	}
	c.advance(6 * time.Second)
	_, _ = a.Authenticate(ctx, unknown)
	if st.calls.Load() != 2 {
		t.Fatal("negative entry did not expire")
	}
}

func TestAuthenticateRevoked(t *testing.T) {
	raw, h, _, _ := Generate()
	a := NewAuthenticator(&fakeStore{keys: map[[32]byte]Key{h: {ID: "k", Revoked: true}}}, time.Minute)
	if _, err := a.Authenticate(context.Background(), raw); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("revoked key: got %v", err)
	}
}

func TestAuthenticateStoreErrorNotCached(t *testing.T) {
	a, st, _, raw := setup(t)
	st.err = errors.New("db down")
	_, err := a.Authenticate(context.Background(), raw)
	if err == nil || errors.Is(err, ErrInvalidKey) {
		t.Fatalf("want backend error, got %v", err)
	}
	st.err = nil
	if _, err := a.Authenticate(context.Background(), raw); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
}

// TestSingleflight: a burst of concurrent misses for one key must cause
// exactly one store lookup.
func TestSingleflight(t *testing.T) {
	a, st, _, raw := setup(t)
	st.release = make(chan struct{})

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			_, err := a.Authenticate(context.Background(), raw)
			errs <- err
		})
	}
	// Wait until the one in-flight lookup has started, then let it finish.
	for st.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the other goroutines join the flight
	close(st.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := st.calls.Load(); got != 1 {
		t.Fatalf("store calls = %d, want 1", got)
	}
}

// TestCacheBounded: spraying unknown (but well-formed) keys must not grow the
// cache past maxEntries.
func TestCacheBounded(t *testing.T) {
	a, _, c, _ := setup(t)
	a.maxEntries = 100
	for range 1000 {
		k, _, _, _ := Generate()
		_, _ = a.Authenticate(context.Background(), k)
		if len(a.cache) > a.maxEntries {
			t.Fatalf("cache grew to %d > %d", len(a.cache), a.maxEntries)
		}
	}
	// Once entries expire, the sweep alone frees space.
	c.advance(time.Minute)
	k, _, _, _ := Generate()
	_, _ = a.Authenticate(context.Background(), k)
	if len(a.cache) != 1 {
		t.Fatalf("after expiry sweep: %d entries, want 1", len(a.cache))
	}
}

func TestMiddleware(t *testing.T) {
	a, st, _, raw := setup(t)
	writeErr := func(w http.ResponseWriter, _ *http.Request, status int, _, msg string) { http.Error(w, msg, status) }
	h := Middleware(a, writeErr)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k, ok := FromContext(r.Context())
		if !ok {
			t.Error("no key in context")
		}
		_, _ = w.Write([]byte(k.ID))
	}))

	tests := []struct {
		name, header string
		storeErr     error
		want         int
	}{
		{"valid", "Bearer " + raw, nil, 200},
		{"no header", "", nil, 401},
		{"basic auth", "Basic abc", nil, 401},
		{"bad key", "Bearer tg_nope_nope_nope", nil, 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st.err = tt.storeErr
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != tt.want {
				t.Errorf("status %d, want %d", rec.Code, tt.want)
			}
		})
	}

	t.Run("store down is 503 not 401", func(t *testing.T) {
		fresh, _, _, _ := Generate()
		st.err = errors.New("db down")
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("Authorization", "Bearer "+fresh)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != 503 {
			t.Errorf("status %d, want 503", rec.Code)
		}
	})
}

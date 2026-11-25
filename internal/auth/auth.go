package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Key is the authenticated identity attached to a request.
type Key struct {
	ID          string
	ProjectID   string
	ProjectName string
	RPMLimit    int
	TPMLimit    int
	Revoked     bool
}

var (
	// ErrInvalidKey: malformed, unknown or revoked. Deliberately one error so
	// responses don't reveal whether a key exists.
	ErrInvalidKey = errors.New("auth: invalid API key")
	// ErrNotFound is returned by a Store when no key has the given hash.
	ErrNotFound = errors.New("auth: key not found")
)

type Store interface {
	LookupKey(ctx context.Context, hash []byte) (Key, error)
}

type entry struct {
	key     Key
	found   bool
	expires time.Time
}

// Authenticator resolves raw keys to Keys, caching results in process.
//
// Concurrency: the cache is a plain map behind a sync.RWMutex. Reads (the
// common case, every request) take the shared lock and run in parallel;
// writes happen only on misses. A sync.Map would also work but is tuned for
// keys written once and read forever, and gives no way to bound its size.
type Authenticator struct {
	store      Store
	ttl        time.Duration // positive entries
	negTTL     time.Duration // unknown keys
	maxEntries int
	now        func() time.Time // injectable for tests

	mu    sync.RWMutex
	cache map[[32]byte]entry

	// sf collapses concurrent misses for the same key into one DB query, so
	// a cold cache under a burst doesn't send N identical queries.
	sf singleflight.Group
}

func NewAuthenticator(store Store, ttl time.Duration) *Authenticator {
	return &Authenticator{
		store:      store,
		ttl:        ttl,
		negTTL:     5 * time.Second,
		maxEntries: 10_000,
		now:        time.Now,
		cache:      make(map[[32]byte]entry),
	}
}

// Authenticate returns ErrInvalidKey for bad keys and a wrapped store error
// when the backing store is unavailable (the caller maps that to 503, not
// 401, so clients don't discard a valid key during a DB outage).
func (a *Authenticator) Authenticate(ctx context.Context, raw string) (Key, error) {
	if !wellFormed(raw) {
		return Key{}, ErrInvalidKey
	}
	h := Hash(raw)

	a.mu.RLock()
	e, ok := a.cache[h]
	a.mu.RUnlock()
	if ok && a.now().Before(e.expires) {
		return result(e)
	}

	v, err, _ := a.sf.Do(string(h[:]), func() (any, error) {
		// The shared lookup must not inherit one caller's cancellation:
		// if the first requester hangs up, every other waiter on this flight
		// would fail with its context error. Detach and bound it ourselves.
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()

		k, err := a.store.LookupKey(lctx, h[:])
		now := a.now()
		var ne entry
		switch {
		case errors.Is(err, ErrNotFound):
			ne = entry{expires: now.Add(a.negTTL)}
		case err != nil:
			return nil, err // don't cache outages
		default:
			ne = entry{key: k, found: true, expires: now.Add(a.ttl)}
		}
		a.mu.Lock()
		if len(a.cache) >= a.maxEntries {
			a.evictLocked(now)
		}
		a.cache[h] = ne
		a.mu.Unlock()
		return ne, nil
	})
	if err != nil {
		return Key{}, fmt.Errorf("auth: lookup: %w", err)
	}
	return result(v.(entry))
}

func result(e entry) (Key, error) {
	if !e.found || e.key.Revoked {
		return Key{}, ErrInvalidKey
	}
	return e.key, nil
}

// evictLocked makes room in a full cache. Called with a.mu held for writing,
// only when len(a.cache) >= a.maxEntries.
//
// Why it matters: anyone can send well-formed but random keys, and each one
// creates a negative entry. Without a bound, a key-spraying client grows this
// map until the pod is OOM-killed.
//
// Policy, in order of preference:
//  1. Drop expired entries and all negative entries. Negative entries are the
//     only state an unauthenticated client can create, so a spray evicts its
//     own garbage before it can displace resolved (legitimate) keys.
//  2. Only if still above the low-water mark, drop positive entries at random.
//     Go randomises map iteration order, so ranging is a cheap random sample.
//
// Cost: each call is O(n) under the write lock, but it always frees at least
// 10% of capacity (the low-water mark), so it runs at most once per
// maxEntries/10 inserts: amortised O(10) per miss, never once per miss.
// A true LRU would need a linked list touched on every read; with a 30s TTL
// the extra hit rate isn't worth it.
func (a *Authenticator) evictLocked(now time.Time) {
	lowWater := a.maxEntries - a.maxEntries/10
	for h, e := range a.cache {
		if !e.found || !now.Before(e.expires) {
			delete(a.cache, h)
		}
	}
	for h := range a.cache {
		if len(a.cache) <= lowWater {
			return
		}
		delete(a.cache, h)
	}
}

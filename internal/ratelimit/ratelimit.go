// Package ratelimit enforces per-key requests/min and tokens/min with two
// token buckets in Redis, checked and updated atomically by a Lua script.
//
// Why Lua: a check-then-decrement done as separate commands races across
// gateway replicas (two replicas both read "1 left" and both admit). Redis
// executes a script without interleaving any other command, so the read,
// refill and decrement are one atomic step: no distributed lock, no WATCH
// retry loop, and one network round trip.
package ratelimit

import (
	"context"
	_ "embed"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

var tracer = otel.Tracer("github.com/krishbansalofficial/tollgate/internal/ratelimit")

var (
	//go:embed common.lua
	commonSrc string
	//go:embed allow.lua
	allowSrc string
	//go:embed reconcile.lua
	reconcileSrc string

	allowScript     = redis.NewScript(commonSrc + allowSrc)
	reconcileScript = redis.NewScript(commonSrc + reconcileSrc)
)

// Decision is the result of one Allow call.
type Decision struct {
	Allowed           bool
	RetryAfter        time.Duration
	RemainingRequests int // -1 = unlimited / unknown
	RemainingTokens   int
	Reserved          int  // tokens actually reserved; refund or charge via Reconcile
	Degraded          bool // Redis unavailable; Allowed reflects the fail mode
}

type Limiter struct {
	rdb      redis.Scripter
	failOpen bool
	timeout  time.Duration
}

// New returns a Limiter. failOpen chooses what happens when Redis is
// unreachable: true admits the request (availability over spend control),
// false rejects it (spend control over availability). See ADR 0003.
func New(rdb redis.Scripter, failOpen bool) *Limiter {
	return &Limiter{rdb: rdb, failOpen: failOpen, timeout: 50 * time.Millisecond}
}

// Bucket keys use a {hash tag} so both of a key's buckets land on the same
// Redis Cluster slot; a multi-key script requires that.
func reqKey(id string) string { return "rl:{" + id + "}:req" }
func tokKey(id string) string { return "rl:{" + id + "}:tok" }

// Allow reserves one request and `tokens` tokens for keyID. A non-nil error
// means Redis failed; the Decision is still usable (it follows the fail mode).
func (l *Limiter) Allow(ctx context.Context, keyID string, rpm, tpm, tokens int) (d Decision, err error) {
	ctx, span := tracer.Start(ctx, "ratelimit.allow")
	defer func() {
		span.SetAttributes(attribute.Bool("tollgate.allowed", d.Allowed), attribute.Bool("tollgate.degraded", d.Degraded),
			attribute.Int("tollgate.reserved_tokens", d.Reserved))
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()
	// A reservation larger than the whole bucket could never succeed; cap it
	// so a huge max_tokens waits for a full bucket instead of failing forever.
	if tokens < 0 {
		tokens = 0 // a negative reservation would refill the bucket: never trust the caller here
	}
	if tpm > 0 && tokens > tpm {
		tokens = tpm
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	res, err := allowScript.Run(ctx, l.rdb, []string{reqKey(keyID), tokKey(keyID)}, rpm, tpm, tokens).Int64Slice()
	if err != nil || len(res) != 4 {
		return Decision{Allowed: l.failOpen, Degraded: true, RemainingRequests: -1, RemainingTokens: -1}, errOr(err)
	}
	d = Decision{
		Allowed:           res[0] == 1,
		RetryAfter:        time.Duration(res[1]) * time.Millisecond,
		RemainingRequests: int(res[2]),
		RemainingTokens:   int(res[3]),
	}
	if d.Allowed {
		d.Reserved = tokens
	}
	return d, nil
}

// Reconcile adjusts the token bucket once the real usage is known:
// delta = reserved - actual (positive refunds, negative charges).
func (l *Limiter) Reconcile(ctx context.Context, keyID string, tpm, delta int) error {
	if tpm <= 0 || delta == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	return reconcileScript.Run(ctx, l.rdb, []string{tokKey(keyID)}, tpm, delta).Err()
}

type malformedReply struct{}

func (malformedReply) Error() string { return "ratelimit: malformed script reply" }

func errOr(err error) error {
	if err != nil {
		return err
	}
	return malformedReply{}
}

//go:build integration

package ratelimit

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

func TestConcurrentAdmissionRealRedis(t *testing.T) {
	ctx := context.Background()
	ctr, err := tcredis.Run(ctx, "redis:7-alpine")
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := redis.ParseURL(uri)
	if err != nil {
		t.Fatal(err)
	}
	opts.PoolSize = 64 // real parallelism: many connections hitting Redis at once
	rdb := redis.NewClient(opts)
	defer func() { _ = rdb.Close() }()

	testConcurrentAdmission(t, rdb)
}

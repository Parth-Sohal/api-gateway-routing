package main

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisConnection(t *testing.T) {

	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})

	ctx, cancel := context.WithTimeout(
		context.Background(),
		3*time.Second,
	)

	defer cancel()
	// Ping to verify connection
	err := rdb.Ping(ctx).Err() // ping - pong

	if err != nil {
		t.Fatalf("failed to connect to Redis: %v", err)
	}

	// Write and read a key
	err = rdb.Set(ctx, "rate-limit:client-A", 3, 0).Err()

	if err != nil {
		t.Fatalf("failed to set rate-limit: %v", err)
	}

	tokens, err := rdb.Get(ctx, "rate-limit:client-A").Result()

	if err != nil {
		t.Fatalf("failed to get rate-limit: %v", err)
	}

	t.Logf("tokens: %s", tokens)
}

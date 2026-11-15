package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FallbackOptions wires a Redis limiter to a local fallback.
type FallbackOptions struct {
	Primary    Limiter
	Local      Limiter
	Timeout    time.Duration
	OnError    func(reason string) // "timeout" or "error"
	OnFallback func()
}

// FallbackLimiter enforces the route's fail_open policy when Redis fails.
type FallbackLimiter struct {
	primary    Limiter
	local      Limiter
	timeout    time.Duration
	onError    func(string)
	onFallback func()
}

// NewFallback builds a fallback limiter.
func NewFallback(opts FallbackOptions) *FallbackLimiter {
	return &FallbackLimiter{
		primary:    opts.Primary,
		local:      opts.Local,
		timeout:    opts.Timeout,
		onError:    opts.OnError,
		onFallback: opts.OnFallback,
	}
}

// Allow tries Redis with a bounded per-call timeout. On failure it either
// returns ErrUnavailable (fail_open false) or makes a per-instance local
// decision (fail_open true) marked Degraded.
func (l *FallbackLimiter) Allow(ctx context.Context, key string, rule Rule) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if l.primary == nil {
		return l.useLocal(ctx, key, rule)
	}

	callCtx := ctx
	cancel := func() {}
	if l.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, l.timeout)
	}
	d, err := l.primary.Allow(callCtx, key, rule)
	cancel()
	if err == nil {
		return d, nil
	}
	if ctx.Err() != nil {
		return Decision{}, ctx.Err()
	}

	reason := "error"
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "timeout"
	}
	if l.onError != nil {
		l.onError(reason)
	}
	if !rule.FailOpen {
		return Decision{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	d, lerr := l.useLocal(ctx, key, rule)
	if lerr != nil {
		return Decision{}, fmt.Errorf("%w: %v", ErrUnavailable, lerr)
	}
	return d, nil
}

func (l *FallbackLimiter) useLocal(ctx context.Context, key string, rule Rule) (Decision, error) {
	if l.local == nil {
		return Decision{}, fmt.Errorf("%w: no local fallback configured", ErrUnavailable)
	}
	d, err := l.local.Allow(ctx, key, rule)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	d.Degraded = true
	if l.onFallback != nil {
		l.onFallback()
	}
	return d, nil
}

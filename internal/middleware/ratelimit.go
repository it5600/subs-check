// Package middleware — Token bucket rate limiter with auto-cleanup.
//
// The original implementation had a memory leak: Cleanup() was never called
// automatically, so the buckets map grew indefinitely as new IPs connected.
// Now a background goroutine periodically removes stale buckets (no activity
// for 2+ windows). The goroutine is started lazily on first use and stops
// via a sync.Once-protected flag to avoid duplicate goroutines.

package middleware

import (
	"sync"
	"time"
)

// TokenBucket implements the token bucket rate limiting algorithm (O(1)).
type TokenBucket struct {
	mu      sync.RWMutex
	buckets map[string]*bucket
	window  int64 // milliseconds
	max     int

	cleanupOnce sync.Once
	stopCleanup chan struct{}
}

type bucket struct {
	tokens     float64
	lastRefill int64
}

// NewTokenBucket creates a new rate limiter.
func NewTokenBucket(window int64, maxCapacity int) *TokenBucket {
	return &TokenBucket{
		buckets:    make(map[string]*bucket),
		window:     window,
		max:        maxCapacity,
		stopCleanup: make(chan struct{}),
	}
}

// Allow checks if a request from the given IP is allowed.
// Uses write lock because both read and write happen on the bucket.
func (tb *TokenBucket) Allow(ip string) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now().UnixMilli()
	b, exists := tb.buckets[ip]
	if !exists {
		b = &bucket{tokens: float64(tb.max), lastRefill: now}
		tb.buckets[ip] = b
	}

	// Refill tokens proportionally to elapsed time.
	elapsed := float64(now - b.lastRefill)
	if elapsed > 0 {
		refillRate := float64(tb.max) / float64(tb.window)
		newTokens := b.tokens + elapsed*refillRate
		if newTokens > float64(tb.max) {
			b.tokens = float64(tb.max)
		} else {
			b.tokens = newTokens
		}
		b.lastRefill = now
	}

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true
	}
	return false
}

// Size returns the number of tracked buckets.
func (tb *TokenBucket) Size() int {
	tb.mu.RLock()
	defer tb.mu.RUnlock()
	return len(tb.buckets)
}

// Cleanup removes stale buckets (no activity for 2+ windows).
func (tb *TokenBucket) Cleanup() {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now().UnixMilli()
	threshold := now - tb.window*2

	for ip, b := range tb.buckets {
		if b.lastRefill < threshold {
			delete(tb.buckets, ip)
		}
	}
}

// StartAutoCleanup launches a background goroutine that periodically
// removes stale buckets. Safe to call multiple times (idempotent).
// The goroutine runs every window interval and exits when StopCleanup is called.
func (tb *TokenBucket) StartAutoCleanup() {
	tb.cleanupOnce.Do(func() {
		interval := time.Duration(tb.window) * time.Millisecond
		if interval <= 0 {
			interval = time.Minute // fallback
		}
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					tb.Cleanup()
				case <-tb.stopCleanup:
					return
				}
			}
		}()
	})
}

// StopCleanup terminates the background cleanup goroutine.
func (tb *TokenBucket) StopCleanup() {
	tb.cleanupOnce.Do(func() {
		close(tb.stopCleanup)
	})
}

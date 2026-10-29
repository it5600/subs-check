package middleware

import (
	"sync"
	"time"
)

// TokenBucket implements the token bucket rate limiting algorithm (O(1))
type TokenBucket struct {
	mu      sync.RWMutex
	buckets map[string]*bucket
	window  int64 // milliseconds
	max     int
}

type bucket struct {
	tokens     float64
	lastRefill int64
}

// NewTokenBucket creates a new rate limiter
func NewTokenBucket(window int64, maxCapacity int) *TokenBucket {
	return &TokenBucket{
		buckets: make(map[string]*bucket),
		window:  window,
		max:     maxCapacity,
	}
}

// Allow checks if a request from the given IP is allowed
func (tb *TokenBucket) Allow(ip string) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now().UnixMilli()
	b, exists := tb.buckets[ip]
	if !exists {
		b = &bucket{tokens: float64(tb.max), lastRefill: now}
		tb.buckets[ip] = b
	}

	// Refill tokens proportionally to elapsed time
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

// Size returns the number of tracked buckets
func (tb *TokenBucket) Size() int {
	tb.mu.RLock()
	defer tb.mu.RUnlock()
	return len(tb.buckets)
}

// Cleanup removes stale buckets (no activity for 2+ windows)
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

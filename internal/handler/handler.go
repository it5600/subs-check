// Package handler provides shared application state for the substore-ui-go API.
//
// The actual HTTP routing is handled by the Gin engine in internal/subscheck/app/server.go.
// This package retains the App struct, Stats counter, and session/rate-limit
// infrastructure that the Gin handlers reference via the handlerApp field.
package handler

import (
	"sync"

	"substore-ui-go/internal/config"
	"substore-ui-go/internal/middleware"
	"substore-ui-go/internal/model"
)

// Stats holds request statistics, safe for concurrent access.
// Snapshot uses a struct return to avoid map+interface{} allocation.
type Stats struct {
	mu          sync.RWMutex
	Requests    int64
	Errors      int64
	RateLimited int64
}

// IncRequests increments the total request counter.
func (s *Stats) IncRequests() {
	s.mu.Lock()
	s.Requests++
	s.mu.Unlock()
}

// IncErrors increments the error counter.
func (s *Stats) IncErrors() {
	s.mu.Lock()
	s.Errors++
	s.mu.Unlock()
}

// IncRateLimited increments the rate-limited request counter.
func (s *Stats) IncRateLimited() {
	s.mu.Lock()
	s.RateLimited++
	s.mu.Unlock()
}

// Snapshot returns a copy of current statistics as a struct (no map allocation).
func (s *Stats) Snapshot() model.StatsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return model.StatsSnapshot{
		Requests:    s.Requests,
		Errors:      s.Errors,
		RateLimited: s.RateLimited,
	}
}

// App holds shared application state referenced by the Gin router.
type App struct {
	Config         *config.Config
	Sessions       *middleware.SessionStore
	RateLimiter    *middleware.TokenBucket
	CircuitBreaker *middleware.CircuitBreaker
	Stats          *Stats
}

// NewApp creates a new application handler.
func NewApp(cfg *config.Config) *App {
	return &App{
		Config:         cfg,
		Sessions:       middleware.NewSessionStore(cfg.CSRFTokenTTL),
		RateLimiter:    middleware.NewTokenBucket(cfg.RateLimitWindow, cfg.RateLimitMax),
		CircuitBreaker: middleware.NewCircuitBreaker(),
		Stats:          &Stats{},
	}
}

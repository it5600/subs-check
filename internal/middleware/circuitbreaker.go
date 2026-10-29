// Package middleware provides HTTP middleware components: CSRF, rate limiting,
// circuit breaking, session management, CORS, and security headers.
package middleware

import (
	"sync"
	"time"
)

// CircuitBreaker implements the circuit breaker pattern with metrics tracking.
// States: closed (normal) -> open (failing) -> half-open (probing) -> closed
//
// Added metrics for observability:
// - TotalRequests: total calls to AllowRequest()
// - TotalFailures: total calls to OnFailure()
// - TotalSuccesses: total calls to OnSuccess()
// - LastErrorTime: when the circuit last opened
// - OpenCount: how many times the circuit has opened
type CircuitBreaker struct {
	mu               sync.RWMutex
	state            string
	failureCount     int
	threshold        int
	resetTimeout     int64 // milliseconds
	halfOpenMax      int
	lastFailureTime  int64
	halfOpenInFlight int

	// Metrics
	totalRequests  int64
	totalFailures  int64
	totalSuccesses int64
	openCount      int
}

// NewCircuitBreaker creates a circuit breaker with default parameters
func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		state:        "closed",
		threshold:    5,
		resetTimeout: 30 * 1000,
		halfOpenMax:  1,
	}
}

// AllowRequest checks if a request should be allowed through
func (cb *CircuitBreaker) AllowRequest() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now().UnixMilli()
	cb.totalRequests++

	if cb.state == "open" {
		if now-cb.lastFailureTime > cb.resetTimeout {
			cb.state = "half-open"
			cb.halfOpenInFlight = 0
			return true
		}
		return false
	}

	if cb.state == "half-open" {
		if cb.halfOpenInFlight < cb.halfOpenMax {
			cb.halfOpenInFlight++
			return true
		}
		return false
	}

	return true // closed
}

// OnSuccess records a successful request
func (cb *CircuitBreaker) OnSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = "closed"
	cb.failureCount = 0
	cb.halfOpenInFlight = 0
	cb.totalSuccesses++
}

// OnFailure records a failed request
func (cb *CircuitBreaker) OnFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failureCount++
	cb.totalFailures++
	cb.lastFailureTime = time.Now().UnixMilli()

	if cb.state == "half-open" || cb.failureCount >= cb.threshold {
		if cb.state != "open" {
			cb.openCount++
		}
		cb.state = "open"
		cb.halfOpenInFlight = 0
	}
}

// State returns the current circuit breaker state
func (cb *CircuitBreaker) State() string {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Metrics returns a snapshot of circuit breaker metrics for observability
func (cb *CircuitBreaker) Metrics() CircuitBreakerMetrics {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return CircuitBreakerMetrics{
		State:           cb.state,
		FailureCount:    cb.failureCount,
		TotalRequests:   cb.totalRequests,
		TotalFailures:   cb.totalFailures,
		TotalSuccesses:  cb.totalSuccesses,
		LastErrorTime:   cb.lastFailureTime,
		OpenCount:       cb.openCount,
		Threshold:       cb.threshold,
		ResetTimeout:    cb.resetTimeout,
	}
}

// CircuitBreakerMetrics is a snapshot of circuit breaker state for monitoring.
type CircuitBreakerMetrics struct {
	State          string
	FailureCount   int
	TotalRequests  int64
	TotalFailures  int64
	TotalSuccesses int64
	LastErrorTime  int64
	OpenCount      int
	Threshold      int
	ResetTimeout   int64
}

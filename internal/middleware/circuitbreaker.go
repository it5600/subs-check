package middleware

import (
	"sync"
	"time"
)

// CircuitBreaker implements the circuit breaker pattern
// States: closed (normal) -> open (failing) -> half-open (probing) -> closed
type CircuitBreaker struct {
	mu               sync.RWMutex
	state            string
	failureCount     int
	threshold        int
	resetTimeout     int64 // milliseconds
	halfOpenMax      int
	lastFailureTime  int64
	halfOpenInFlight int
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
}

// OnFailure records a failed request
func (cb *CircuitBreaker) OnFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failureCount++
	cb.lastFailureTime = time.Now().UnixMilli()

	if cb.state == "half-open" || cb.failureCount >= cb.threshold {
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

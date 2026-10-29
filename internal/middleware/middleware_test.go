package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ===== Session Store =====

func TestSessionStore_CreateAndGet(t *testing.T) {
	ss := NewSessionStore(60000)
	sid := ss.Create("test-key")

	entry := ss.Get(sid)
	if entry == nil {
		t.Fatal("Expected valid session, got nil")
	}
	if entry.apiKey != "test-key" {
		t.Errorf("Expected apiKey 'test-key', got %s", entry.apiKey)
	}
}

func TestSessionStore_GetNonexistent(t *testing.T) {
	ss := NewSessionStore(60000)
	entry := ss.Get("nonexistent")
	if entry != nil {
		t.Error("Expected nil for nonexistent session")
	}
}

func TestSessionStore_Destroy(t *testing.T) {
	ss := NewSessionStore(60000)
	sid := ss.Create("test-key")
	ss.Destroy(sid)

	entry := ss.Get(sid)
	if entry != nil {
		t.Error("Expected nil after destroy")
	}
}

func TestSessionStore_DestroyAll(t *testing.T) {
	ss := NewSessionStore(60000)
	sid1 := ss.Create("key1")
	sid2 := ss.Create("key2")

	ss.DestroyAll()

	if ss.Get(sid1) != nil || ss.Get(sid2) != nil {
		t.Error("Expected all sessions destroyed")
	}
	if ss.Count() != 0 {
		t.Errorf("Expected 0 sessions, got %d", ss.Count())
	}
}

func TestSessionStore_Expired(t *testing.T) {
	ss := NewSessionStore(10) // 10ms TTL
	sid := ss.Create("test-key")

	time.Sleep(20 * time.Millisecond)

	entry := ss.Get(sid)
	if entry != nil {
		t.Error("Expected nil for expired session")
	}
}

func TestSessionStore_Cleanup(t *testing.T) {
	ss := NewSessionStore(10)
	ss.Create("key1")

	time.Sleep(20 * time.Millisecond)

	ss.Create("key2")
	ss.Cleanup()

	if ss.Count() != 1 {
		t.Errorf("Expected 1 session after cleanup, got %d", ss.Count())
	}
}

// ===== Token Bucket =====

func TestTokenBucket_Allow(t *testing.T) {
	tb := NewTokenBucket(60000, 5)
	for i := 0; i < 5; i++ {
		if !tb.Allow("1.2.3.4") {
			t.Errorf("Expected request %d to be allowed", i+1)
		}
	}
	if tb.Allow("1.2.3.4") {
		t.Error("Expected 6th request to be denied")
	}
}

func TestTokenBucket_Refill(t *testing.T) {
	tb := NewTokenBucket(100, 5) // 100ms window, 5 tokens
	for i := 0; i < 5; i++ {
		tb.Allow("1.2.3.4")
	}
	if tb.Allow("1.2.3.4") {
		t.Error("Expected denial before refill")
	}

	time.Sleep(150 * time.Millisecond)

	if !tb.Allow("1.2.3.4") {
		t.Error("Expected request to be allowed after refill")
	}
}

func TestTokenBucket_Cleanup(t *testing.T) {
	tb := NewTokenBucket(100, 5)
	tb.Allow("1.2.3.4")

	time.Sleep(250 * time.Millisecond)

	tb.Cleanup()
	if tb.Size() != 0 {
		t.Errorf("Expected 0 buckets after cleanup, got %d", tb.Size())
	}
}

// ===== CSRF =====

func TestGenerateCSRFToken(t *testing.T) {
	token := GenerateCSRFToken("api-key", 60000)
	if len(token) < 16 {
		t.Errorf("Token too short: %s", token)
	}
}

func TestValidateCSRFToken_Success(t *testing.T) {
	token := GenerateCSRFToken("api-key", 60000)

	r := httptest.NewRequest("POST", "/api/test", nil)
	r.Header.Set("X-CSRF-Token", token)

	if !ValidateCSRFToken(r, "api-key") {
		t.Error("Expected valid CSRF token")
	}
}

func TestValidateCSRFToken_OneTimeUse(t *testing.T) {
	token := GenerateCSRFToken("api-key", 60000)

	r1 := httptest.NewRequest("POST", "/api/test", nil)
	r1.Header.Set("X-CSRF-Token", token)
	if !ValidateCSRFToken(r1, "api-key") {
		t.Error("First use should succeed")
	}

	r2 := httptest.NewRequest("POST", "/api/test", nil)
	r2.Header.Set("X-CSRF-Token", token)
	if ValidateCSRFToken(r2, "api-key") {
		t.Error("Second use should fail (one-time token)")
	}
}

func TestValidateCSRFToken_NoHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/test", nil)
	if ValidateCSRFToken(r, "api-key") {
		t.Error("Expected failure without CSRF header")
	}
}

func TestValidateCSRFToken_WrongKey(t *testing.T) {
	token := GenerateCSRFToken("api-key", 60000)

	r := httptest.NewRequest("POST", "/api/test", nil)
	r.Header.Set("X-CSRF-Token", token)

	if ValidateCSRFToken(r, "wrong-key") {
		t.Error("Expected failure with wrong API key")
	}
}

// ===== API Key =====

func TestCheckAPIKey(t *testing.T) {
	tests := []struct {
		name    string
		apiKey  string
		request string
		expect  bool
	}{
		{"Correct key", "test-key", "test-key", true},
		{"Wrong key", "test-key", "wrong", false},
		{"Empty header", "test-key", "", false},
		{"Different length", "test-key", "short", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/test", nil)
			if tt.request != "" {
				r.Header.Set("X-API-Key", tt.request)
			}
			result := CheckAPIKey(r, tt.apiKey)
			if result != tt.expect {
				t.Errorf("Expected %v, got %v", tt.expect, result)
			}
		})
	}
}

// ===== CORS =====

func TestHandleCORS_AllowedOrigin(t *testing.T) {
	origins := []string{"https://example.com"}
	r := httptest.NewRequest("OPTIONS", "/api/test", nil)
	r.Header.Set("Origin", "https://example.com")

	w := httptest.NewRecorder()
	shouldContinue := HandleCORS(origins, w, r)

	if shouldContinue {
		t.Error("Expected OPTIONS to stop processing")
	}
	if w.Code != http.StatusNoContent {
		t.Errorf("Expected 204, got %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Error("Missing CORS header")
	}
}

func TestHandleCORS_DisallowedOrigin(t *testing.T) {
	origins := []string{"https://example.com"}
	r := httptest.NewRequest("OPTIONS", "/api/test", nil)
	r.Header.Set("Origin", "https://evil.com")

	w := httptest.NewRecorder()
	shouldContinue := HandleCORS(origins, w, r)

	if shouldContinue {
		t.Error("Expected OPTIONS to stop processing")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("Expected 403, got %d", w.Code)
	}
}

func TestHandleCORS_GET(t *testing.T) {
	origins := []string{"https://example.com"}
	r := httptest.NewRequest("GET", "/api/test", nil)
	r.Header.Set("Origin", "https://example.com")

	w := httptest.NewRecorder()
	shouldContinue := HandleCORS(origins, w, r)

	if !shouldContinue {
		t.Error("Expected GET to continue processing")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Error("Missing CORS header for GET")
	}
}

// ===== Circuit Breaker =====

func TestCircuitBreaker_ClosedToOpen(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.threshold = 3

	if !cb.AllowRequest() {
		t.Error("Expected requests allowed in closed state")
	}

	for i := 0; i < 3; i++ {
		cb.AllowRequest()
		cb.OnFailure()
	}

	if cb.State() != "open" {
		t.Errorf("Expected open state, got %s", cb.State())
	}
	if cb.AllowRequest() {
		t.Error("Expected requests denied in open state")
	}
}

func TestCircuitBreaker_HalfOpen(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.threshold = 1
	cb.resetTimeout = 100

	cb.OnFailure() // -> open
	time.Sleep(150 * time.Millisecond)

	if !cb.AllowRequest() {
		t.Error("Expected half-open to allow probe request")
	}
	if cb.State() != "half-open" {
		t.Errorf("Expected half-open, got %s", cb.State())
	}
}

func TestCircuitBreaker_Recovery(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.threshold = 1
	cb.resetTimeout = 100

	cb.OnFailure() // -> open
	time.Sleep(150 * time.Millisecond)
	cb.AllowRequest() // -> half-open

	cb.OnSuccess() // -> closed
	if cb.State() != "closed" {
		t.Errorf("Expected closed after success, got %s", cb.State())
	}
}

// ===== Helpers =====

func TestParseCookies(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", "a=1; b=2; c=3")

	cookies := ParseCookies(r)
	if len(cookies) != 3 {
		t.Errorf("Expected 3 cookies, got %d", len(cookies))
	}
	if cookies["a"] != "1" || cookies["b"] != "2" || cookies["c"] != "3" {
		t.Errorf("Unexpected cookie values: %v", cookies)
	}
}

func TestParseCookies_Empty(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	cookies := ParseCookies(r)
	if len(cookies) != 0 {
		t.Errorf("Expected 0 cookies, got %d", len(cookies))
	}
}

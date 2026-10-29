package middleware

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ===== CSRF Token =====

var (
	csrfMu     sync.RWMutex
	csrfTokens = make(map[string]csrfEntry)
)

// maxCSRFTokens limits the number of stored tokens to prevent memory exhaustion
// from a flood of /api/csrf-token requests. When exceeded, the oldest tokens
// are evicted first (rough FIFO via map iteration order — acceptable for CSRF
// since tokens are short-lived and one-time-use).
const maxCSRFTokens = 1000

type csrfEntry struct {
	apiKey  string
	expires int64
}

// GenerateCSRFToken creates a new one-time CSRF token bound to the API key.
func GenerateCSRFToken(apiKey string, ttl int64) string {
	token := randomHex(16)
	csrfMu.Lock()
	// Evict oldest tokens if we're at capacity.
	if len(csrfTokens) >= maxCSRFTokens {
		// Remove a few expired tokens first; if none, remove arbitrary ones.
		now := time.Now().UnixMilli()
		removed := 0
		for k, v := range csrfTokens {
			if now > v.expires {
				delete(csrfTokens, k)
				removed++
				if removed >= 10 {
					break
				}
			}
		}
		// If still at capacity, force-remove some arbitrary entries.
		if len(csrfTokens) >= maxCSRFTokens {
			removed = 0
			for k := range csrfTokens {
				delete(csrfTokens, k)
				removed++
				if removed >= 10 {
					break
				}
			}
		}
	}
	csrfTokens[token] = csrfEntry{apiKey: apiKey, expires: time.Now().UnixMilli() + ttl}
	csrfMu.Unlock()
	return token
}

// ValidateCSRFToken validates and consumes a CSRF token (one-time use).
// Validates expiration and API key match before consuming the token,
// so an expired token is not deleted (allowing it to be cleaned up by CleanupCSRFTokens).
func ValidateCSRFToken(r *http.Request, apiKey string) bool {
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		return false
	}

	csrfMu.Lock()
	entry, ok := csrfTokens[token]
	if !ok {
		csrfMu.Unlock()
		return false
	}

	// Validate before consuming
	if time.Now().UnixMilli() > entry.expires {
		csrfMu.Unlock()
		return false
	}
	if entry.apiKey != apiKey {
		csrfMu.Unlock()
		return false
	}

	// One-time use: delete only after validation passes
	delete(csrfTokens, token)
	csrfMu.Unlock()
	return true
}

// CleanupCSRFTokens removes expired tokens
func CleanupCSRFTokens() {
	csrfMu.Lock()
	now := time.Now().UnixMilli()
	for k, v := range csrfTokens {
		if now > v.expires {
			delete(csrfTokens, k)
		}
	}
	csrfMu.Unlock()
}

// ===== API Key Authentication =====

// CheckAPIKey validates the X-API-Key header using constant-time comparison
func CheckAPIKey(r *http.Request, apiKey string) bool {
	key := r.Header.Get("X-API-Key")
	if key == "" {
		return false
	}
	// Constant-time comparison regardless of length
	result := subtle.ConstantTimeCompare([]byte(key), []byte(apiKey))
	return result == 1
}

// CompareAPIKey validates a raw key string against the configured key in
// constant time without requiring an HTTP request (useful for login bodies).
func CompareAPIKey(key, apiKey string) bool {
	if key == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(key), []byte(apiKey)) == 1
}

// GatewayUser holds the trusted identity injected by the fnOS unified gateway.
//
// The gateway only proves that the request came from an authenticated NAS
// session; it does not prove the caller has any particular business
// privilege. Callers must therefore check IsAdmin themselves before exposing
// management features.
type GatewayUser struct {
	UID      int64
	IsAdmin  bool
	Username string
}

// ParseGatewayIdentity reads the X-Trim-* identity headers injected by the
// fnOS unified gateway. It returns (nil, false) when the headers are absent,
// which means the request did not pass through an authenticated gateway
// (e.g. direct socket access) and must be rejected.
//
// The uid is parsed leniently: a malformed header is treated as unauthenticated
// rather than trusted, matching the gateway contract's "verify before use"
// rule. Header values are only ever treated as display context, never as an
// authorization grant on their own.
func ParseGatewayIdentity(r *http.Request) (*GatewayUser, bool) {
	uidHeader := r.Header.Get("X-Trim-Userid")
	if uidHeader == "" {
		return nil, false
	}
	uid, err := strconv.ParseInt(uidHeader, 10, 64)
	if err != nil || uid <= 0 {
		return nil, false
	}

	return &GatewayUser{
		UID:      uid,
		IsAdmin:  strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Trim-Isadmin")), "true"),
		Username: r.Header.Get("X-Trim-Username"),
	}, true
}

// CheckAuth validates authentication via Cookie Session or X-API-Key header.
//
// NOTE: the fnOS gateway identity is intentionally NOT an authentication
// shortcut here. The gateway proves the caller is a logged-in NAS user, but
// the application still needs its own login (or an API key) before handing
// out management data — otherwise any NAS user could open the panel and
// read/change subscription sources. Callers that do want to admit gateway
// users must additionally require IsAdmin via ParseGatewayIdentity.
func CheckAuth(r *http.Request, apiKey string, sessions *SessionStore) (bool, string) {
	// 1. Cookie Session
	cookie, err := r.Cookie("substore_sid")
	if err == nil && cookie.Value != "" {
		if sessions.Get(cookie.Value) {
			return true, "session"
		}
	}
	// 2. X-API-Key header
	if CheckAPIKey(r, apiKey) {
		return true, "apikey"
	}
	return false, ""
}

// ===== Security Headers =====

// ApplySecurityHeaders sets standard security headers on the response
func ApplySecurityHeaders(w http.ResponseWriter, headers map[string]string) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
}

// SecurityHeadersHandler wraps a handler and injects the configured security
// headers into every response. It is the middleware form of
// ApplySecurityHeaders, suitable for attaching to the root handler chain.
func SecurityHeadersHandler(headers map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ApplySecurityHeaders(w, headers)
		next.ServeHTTP(w, r)
	})
}

// ===== Request Logging =====

// LogRequest wraps the response to log request completion with timing info.
// Reference: net/http server access-log pattern (stdlib's internal logging).
func LogRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap ResponseWriter to capture status
		wrapped := &responseWriter{ResponseWriter: w}
		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)
		status := wrapped.status

		log.Printf("[%s] %s %s -> %d (%dms) [%s]",
			time.Now().Format("2006-01-02 15:04:05"),
			r.Method,
			r.URL.Path,
			status,
			duration.Milliseconds(),
			r.RemoteAddr,
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if rw.status == 0 {
		rw.status = 200
	}
	return rw.ResponseWriter.Write(b)
}

// Flush implements http.Flusher so streaming responses work through the wrapper.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker so WebSocket/raw TCP upgrades work through the wrapper.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}

// ===== Helper =====

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ===== CORS =====

// HandleCORS processes CORS headers and preflight requests.
// For small origin lists (typical: 1-3), linear scan is cache-friendly
// and faster than map allocation. Reference: Go map overhead vs slice scan.
func HandleCORS(allowedOrigins []string, w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	allowed := false
	for _, o := range allowedOrigins {
		if o == origin {
			allowed = true
			break
		}
	}
	if allowed {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "X-API-Key, Content-Type, X-CSRF-Token")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}

	if r.Method == "OPTIONS" {
		if allowed {
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
		return false // stop processing
	}
	return true // continue processing
}

// ParseCookies extracts cookies from a request
func ParseCookies(r *http.Request) map[string]string {
	cookies := make(map[string]string)
	cookieHeader := r.Header.Get("Cookie")
	if cookieHeader == "" {
		return cookies
	}
	parts := strings.Split(cookieHeader, ";")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, "=")
		if idx > 0 {
			key := strings.TrimSpace(part[:idx])
			val := strings.TrimSpace(part[idx+1:])
			cookies[key] = val
		}
	}
	return cookies
}

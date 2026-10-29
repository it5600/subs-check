// Package middleware — gzip compression.
//
// Adds gzip compression to text-based responses (JSON, HTML, CSS, JS) when
// the client accepts it. Already-compressed content (images, fonts, archives)
// is passed through unchanged. The gzip writer wraps the ResponseWriter
// transparently so handlers don't need any changes.
// Reference: Go stdlib compress/gzip.

package middleware

import (
	"compress/gzip"
	"net/http"
)

// gzipWriter wraps http.ResponseWriter to compress output.
type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
	// status records the HTTP status code for the wrapped writer.
	status int
}

func (gw *gzipWriter) WriteHeader(code int) {
	gw.status = code
	gw.ResponseWriter.WriteHeader(code)
}

func (gw *gzipWriter) Write(b []byte) (int, error) {
	if gw.status == 0 {
		gw.status = http.StatusOK
	}
	return gw.gz.Write(b)
}

func (gw *gzipWriter) Close() error {
	return gw.gz.Close()
}

// Flush implements http.Flusher for streaming responses through gzip.
func (gw *gzipWriter) Flush() {
	if f, ok := gw.ResponseWriter.(http.Flusher); ok {
		_ = gw.gz.Flush()
		f.Flush()
	}
}

// GzipCompress wraps a handler with gzip compression.
// Compresses responses only when the client accepts gzip and the
// Content-Type is worth compressing (text/JSON/HTML/CSS/JS).
func GzipCompress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		ct := w.Header().Get("Content-Type")
		if !shouldCompress(ct) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer func() { _ = gz.Close() }()
		next.ServeHTTP(&gzipWriter{ResponseWriter: w, gz: gz}, r)
	})
}

// acceptsGzip checks if the client accepts gzip encoding.
func acceptsGzip(r *http.Request) bool {
	ae := r.Header.Get("Accept-Encoding")
	for _, part := range splitAndTrim(ae) {
		if part == "gzip" || part == "*" {
			return true
		}
		// Also match "gzip;q=1.0" style entries.
		for _, token := range splitAndTrim(part) {
			if token == "gzip" {
				return true
			}
		}
	}
	return false
}

// shouldCompress returns true if the Content-Type is worth compressing.
// Already-compressed types (images, fonts, archives) are skipped.
func shouldCompress(contentType string) bool {
	// If Content-Type is empty, assume it's worth compressing (most
	// responses in this app are JSON or HTML).
	if contentType == "" {
		return true
	}

	// Skip already-compressed content types.
	lower := toLower(contentType)
	skip := []string{
		"image/",
		"font/",
		"application/zip",
		"application/gzip",
		"application/x-gzip",
		"video/",
		"audio/",
	}
	for _, s := range skip {
		if containsStr(lower, s) {
			return false
		}
	}
	return true
}

// --- small string helpers (avoid importing strings for a few calls) ---

func splitAndTrim(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			part := trimSpace(s[start:i])
			if part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func toLower(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func containsStr(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

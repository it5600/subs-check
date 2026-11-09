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

// gzipDecisionWriter defers the compression decision until the first
// WriteHeader/Write, i.e. after the handler has set Content-Type and status.
//
// The previous implementation read Content-Type *before* calling next, when
// the header map was still empty, so shouldCompress("") always returned true:
// images/fonts were compressed for nothing, and http.ServeContent responses
// kept a stale Content-Length while the body was gzip-encoded (Range/206 and
// conditional/304 replies were corrupted). Deciding lazily fixes all of that
// without changing the bytes the client sees after decompression.
type gzipDecisionWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

func (gw *gzipDecisionWriter) WriteHeader(code int) {
	if gw.decided {
		return
	}
	gw.decided = true

	// Statuses with no body, conditional 304 and partial 206 must keep their
	// Content-Length / Content-Range semantics byte-exact.
	if code < 200 || code == http.StatusNoContent ||
		code == http.StatusNotModified || code == http.StatusPartialContent ||
		!shouldCompress(gw.Header().Get("Content-Type")) {
		gw.ResponseWriter.WriteHeader(code)
		return
	}

	gw.Header().Set("Content-Encoding", "gzip")
	gw.Header().Add("Vary", "Accept-Encoding")
	gw.Header().Del("Content-Length") // compressed length differs from the original
	gw.gz = gzip.NewWriter(gw.ResponseWriter)
	gw.ResponseWriter.WriteHeader(code)
}

func (gw *gzipDecisionWriter) Write(b []byte) (int, error) {
	if !gw.decided {
		gw.WriteHeader(http.StatusOK)
	}
	if gw.gz != nil {
		return gw.gz.Write(b)
	}
	return gw.ResponseWriter.Write(b)
}

// Flush implements http.Flusher for streaming responses through gzip.
func (gw *gzipDecisionWriter) Flush() {
	if !gw.decided {
		gw.WriteHeader(http.StatusOK)
	}
	if gw.gz != nil {
		_ = gw.gz.Flush()
	}
	if f, ok := gw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (gw *gzipDecisionWriter) Close() error {
	if gw.gz != nil {
		return gw.gz.Close()
	}
	return nil
}

// GzipCompress wraps a handler with gzip compression.
// Compresses responses only when the client accepts gzip and the actual
// response Content-Type is worth compressing (text/JSON/HTML/CSS/JS).
func GzipCompress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipDecisionWriter{ResponseWriter: w}
		defer func() {
			if !gw.decided {
				// handler 完全没写任何东西：沿用 net/http 原生的「200 + 空体」
				// 语义，不要为一个空响应生成 gzip 流、也不要误加 Content-Encoding。
				gw.decided = true
				gw.ResponseWriter.WriteHeader(http.StatusOK)
				return
			}
			_ = gw.Close()
		}()
		next.ServeHTTP(gw, r)
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

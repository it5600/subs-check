package service

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Sentinel errors for the service package.
// Wrap with fmt.Errorf("%w", ...) at call sites; check with errors.Is().
var (
	ErrUpstreamUnreachable = fmt.Errorf("upstream service unreachable")
	ErrUpstreamTimeout     = fmt.Errorf("upstream service request timeout")
	ErrEmptyResponse       = fmt.Errorf("upstream returned empty response body")
	ErrParseFailure        = fmt.Errorf("failed to parse upstream data")
)

// hopByHopHeaders are headers that should not be forwarded (RFC 7230 §6.1)
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailers":            true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// sharedAgent reuses TCP connections (keep-alive).
// Tuned for a management panel serving multiple concurrent users:
// MaxIdleConnsPerHost raised from 10 to 32 to handle burst traffic
// without connection churn.
var sharedAgent = &http.Transport{
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   32,
	IdleConnTimeout:       90 * time.Second,
	// Reduce connection establishment latency for the first request.
	DialContext: (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	// Enable HTTP/2 for lower latency on repeat requests.
	ForceAttemptHTTP2:     true,
	TLSHandshakeTimeout:   5 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// sharedClient uses the keep-alive agent for all proxy requests.
// Timeout reduced from 30s to 15s — the upstream is a local service
// and should respond well within that window.
var sharedClient = &http.Client{
	Transport: sharedAgent,
	Timeout:   15 * time.Second,
}

// ProxyRequest forwards an HTTP request to the upstream service.
// Response is streamed directly to the client when possible, avoiding
// full buffering in memory for large responses.
func ProxyRequest(w http.ResponseWriter, r *http.Request, targetHost string, targetPort int, targetPath string, apiKey string) {
	// Build upstream URL
	upstreamURL := fmt.Sprintf("http://%s:%d%s", targetHost, targetPort, targetPath)
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	// Create upstream request with caller's context for cancellation propagation.
	// Reference: Go blog "Contexts and goroutines in Go" — context should flow
	// from the inbound request through all downstream calls.
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, FormatError(502001, "代理请求失败: "+err.Error()))
		return
	}

	// Forward headers (excluding hop-by-hop)
	for k, v := range r.Header {
		if !hopByHopHeaders[strings.ToLower(k)] {
			for _, val := range v {
				proxyReq.Header.Add(k, val)
			}
		}
	}
	proxyReq.Header.Set("X-API-Key", apiKey)

	// Execute request
	resp, err := sharedClient.Do(proxyReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, FormatError(502001, "代理请求失败: "+err.Error()))
		return
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			// Body close errors are non-fatal in streaming contexts
			_ = cerr
		}
	}()

	// Forward response headers (excluding hop-by-hop)
	for k, v := range resp.Header {
		if !hopByHopHeaders[strings.ToLower(k)] {
			for _, val := range v {
				w.Header().Add(k, val)
			}
		}
	}

	// Stream response body directly to client — no full buffering.
	// For JSON responses, we still stream as-is since the upstream
	// already returns valid JSON and the frontend parses it.
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// ProxyGet makes a GET request to the upstream and returns the response body.
// The caller's context is propagated so cancellation/timeout reaches the upstream.
func ProxyGet(ctx context.Context, targetHost string, targetPort int, targetPath string, apiKey string) (int, []byte, error) {
	upstreamURL := fmt.Sprintf("http://%s:%d%s", targetHost, targetPort, targetPath)

	req, err := http.NewRequestWithContext(ctx, "GET", upstreamURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-API-Key", apiKey)

	resp, err := sharedClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, fmt.Errorf("%w: %s: %v", ErrUpstreamTimeout, upstreamURL, err)
		}
		return 0, nil, fmt.Errorf("%w: %s: %v", ErrUpstreamUnreachable, upstreamURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read upstream body: %w", err)
	}
	if len(body) == 0 {
		return resp.StatusCode, nil, ErrEmptyResponse
	}

	return resp.StatusCode, body, nil
}

// ReadConfigFile reads a config file
func ReadConfigFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ConfigFileMu guards the read-modify-write cycle used by the "add / update /
// delete subscription source" handlers. Without it two concurrent requests
// can read the same content and the last writer silently discards the other
// change. It is exported so the app layer can wrap the whole cycle in one
// critical section while still using ReadConfigFile/WriteConfigFile.
var ConfigFileMu sync.Mutex

// writeFileMu 串行化 WriteConfigFile 自身的「临时文件 + rename」序列。
// ConfigFileMu 保护的是 handler 层的 read-modify-write；本锁让函数在没有
// 外层锁的调用方处也并发安全（Windows 上并发 rename 到同一目标会
// 返回 "Access is denied"）。两把锁的获取顺序始终一致，不会死锁。
var writeFileMu sync.Mutex

// WriteConfigFile atomically writes a config file using a unique temp file +
// rename. A fixed "path.tmp" name used to be shared by every concurrent
// writer, so two requests could interleave inside the same temp file.
// os.CreateTemp also creates the file with 0600 (the config contains
// github-token / s3-secret-key / webdav-password), instead of the previous
// world-readable 0644.
func WriteConfigFile(path string, content string) error {
	writeFileMu.Lock()
	defer writeFileMu.Unlock()

	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op after a successful rename

	// CreateTemp 在 Unix 上已是 0600，显式声明以免依赖平台默认行为。
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // fsync to ensure durability
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, path)
}

// writeJSON sends a JSON response
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// ComputeETag generates an MD5-based ETag for conditional requests
func ComputeETag(data []byte) string {
	hash := md5.Sum(data)
	return `"` + hex.EncodeToString(hash[:])[:16] + `"`
}

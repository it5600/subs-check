// Package handler provides HTTP request handlers for the substore-ui-go API.
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"substore-ui-go/internal/config"
	"substore-ui-go/internal/middleware"
	"substore-ui-go/internal/model"
	"substore-ui-go/internal/service"
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

// App holds shared application state
type App struct {
	Config         *config.Config
	Sessions       *middleware.SessionStore
	RateLimiter    *middleware.TokenBucket
	CircuitBreaker *middleware.CircuitBreaker
	Stats          *Stats
}

// NewApp creates a new application handler
func NewApp(cfg *config.Config) *App {
	return &App{
		Config:         cfg,
		Sessions:       middleware.NewSessionStore(cfg.CSRFTokenTTL),
		RateLimiter:    middleware.NewTokenBucket(cfg.RateLimitWindow, cfg.RateLimitMax),
		CircuitBreaker: middleware.NewCircuitBreaker(),
		Stats:          &Stats{},
	}
}

// HandleAPI routes API requests to the appropriate handler
func (a *App) HandleAPI(w http.ResponseWriter, r *http.Request, pathname string) {
	// /api/health - no auth required
	if pathname == "/api/health" {
		a.handleHealth(w, r)
		return
	}

	// /api/csrf-token - no auth required
	if pathname == "/api/csrf-token" {
		a.handleCSRFToken(w, r)
		return
	}

	// /api/login - no auth required
	if pathname == "/api/login" {
		a.handleLogin(w, r)
		return
	}

	// /api/logout - no auth required
	if pathname == "/api/logout" {
		a.handleLogout(w, r)
		return
	}

	// /api/dashboard - aggregated data, no auth required (gateway mode handles SSO)
	if pathname == "/api/dashboard" {
		a.handleDashboard(w, r)
		return
	}

	// Authentication check for all other /api/* endpoints
	valid, authType := middleware.CheckAuth(r, a.Config.APIKey, a.Sessions, a.Config.GatewayMode)
	if !valid {
		a.Stats.IncErrors()
		WriteJSON(w, http.StatusUnauthorized, service.FormatError(401001, "未授权"))
		return
	}

	// CSRF check for write operations (POST/PATCH/DELETE)
	// Skip CSRF when authenticated via gateway (gateway handles its own CSRF)
	if authType != "gateway" {
		if r.Method == http.MethodPost || r.Method == http.MethodDelete || r.Method == http.MethodPatch {
			if !middleware.ValidateCSRFToken(r, a.Config.APIKey) {
				a.Stats.IncErrors()
				WriteJSON(w, http.StatusForbidden, service.FormatError(403003, "CSRF 校验失败，请刷新页面获取新的 Token"))
				return
			}
		}
	}

	// Route to specific handlers
	switch {
	case pathname == "/api/status":
		if a.CircuitBreaker.AllowRequest() {
			service.ProxyRequest(w, r, a.Config.SubsCheckHost, a.Config.SubsCheckPort, "/api/status", a.Config.APIKey)
		} else {
			WriteJSON(w, http.StatusServiceUnavailable, service.FormatError(503001, "上游服务不可用（断路器已打开）"))
		}
	case pathname == "/api/logs":
		if a.CircuitBreaker.AllowRequest() {
			service.ProxyRequest(w, r, a.Config.SubsCheckHost, a.Config.SubsCheckPort, "/api/logs", a.Config.APIKey)
		} else {
			WriteJSON(w, http.StatusServiceUnavailable, service.FormatError(503001, "上游服务不可用（断路器已打开）"))
		}
	case pathname == "/api/config":
		if a.CircuitBreaker.AllowRequest() {
			service.ProxyRequest(w, r, a.Config.SubsCheckHost, a.Config.SubsCheckPort, "/api/config", a.Config.APIKey)
		} else {
			WriteJSON(w, http.StatusServiceUnavailable, service.FormatError(503001, "上游服务不可用（断路器已打开）"))
		}
	case pathname == "/api/nodes":
		a.handleNodes(w, r)
	case pathname == "/api/sources":
		a.handleSources(w, r)
	case strings.HasPrefix(pathname, "/api/sources/"):
		if r.Method == http.MethodPatch {
			a.handleUpdateSource(w, r, pathname)
		} else {
			a.handleDeleteSource(w, r, pathname)
		}
	case strings.HasPrefix(pathname, "/api/substore/"):
		subPath := strings.TrimPrefix(pathname, "/api/substore/")
		targetPath := a.Config.SubStorePath + "/" + subPath
		service.ProxyRequest(w, r, a.Config.SubStoreHost, a.Config.SubStorePort, targetPath, a.Config.APIKey)
	default:
		WriteJSON(w, http.StatusNotFound, service.FormatError(404002, fmt.Sprintf("API 不存在: %s", pathname)))
	}
}

// handleHealth returns service health status
func (a *App) handleHealth(w http.ResponseWriter, _ *http.Request) {
	stats := a.Stats.Snapshot()
	data := map[string]interface{}{
		"service": "substore-ui-go",
		"uptime":  int64(time.Since(startTime).Seconds()),
		"stats": map[string]interface{}{
			"requests":     stats.Requests,
			"errors":       stats.Errors,
			"rateLimited":  stats.RateLimited,
			"rateLimitMap": a.RateLimiter.Size(),
		},
		"upstream": map[string]string{
			"subsCheck": fmt.Sprintf("http://%s:%d", a.Config.SubsCheckHost, a.Config.SubsCheckPort),
			"subStore":  fmt.Sprintf("http://%s:%d%s", a.Config.SubStoreHost, a.Config.SubStorePort, a.Config.SubStorePath),
		},
	}
	WriteJSON(w, http.StatusOK, service.FormatOK(data))
}

// handleDashboard aggregates health + status + nodes + sources into a single response.
// Reduces frontend from 3 round-trips to 1, improving load speed.
func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	stats := a.Stats.Snapshot()

	// 1. Health data
	healthData := map[string]interface{}{
		"service": "substore-ui-go",
		"uptime":  int64(time.Since(startTime).Seconds()),
		"stats": map[string]interface{}{
			"requests":     stats.Requests,
			"errors":       stats.Errors,
			"rateLimited":  stats.RateLimited,
			"rateLimitMap": a.RateLimiter.Size(),
		},
		"upstream": map[string]string{
			"subsCheck": fmt.Sprintf("http://%s:%d", a.Config.SubsCheckHost, a.Config.SubsCheckPort),
			"subStore":  fmt.Sprintf("http://%s:%d%s", a.Config.SubStoreHost, a.Config.SubStorePort, a.Config.SubStorePath),
		},
	}

	// 2. Fetch node data (cached)
	nodes := service.GetCachedNodes()
	if nodes == nil {
		statusCode, body, err := service.ProxyGet(r.Context(), a.Config.SubsCheckHost, a.Config.SubsCheckPort, "/sub/all.yaml", a.Config.APIKey)
		if err == nil && statusCode == http.StatusOK {
			nodes = service.ParseNodesFromYaml(string(body))
			service.SetCachedNodes(nodes)
		}
	}
	var nodeSummary model.NodeSummary
	if nodes != nil {
		nodeSummary = model.NodeSummary{
			Total:     len(nodes),
			ByType:    map[string]int{},
			Available: len(nodes), // all parsed nodes are available
		}
		for _, n := range nodes {
			nodeSummary.ByType[n.Type]++
		}
	}

	// 3. Fetch subscription sources
	var sourceSummary model.SourceSummary
	if cfg, err := service.ReadConfigFile(a.Config.ConfigPath); err == nil {
		sources := service.ParseSubUrls(cfg)
		sourceSummary = model.SourceSummary{
			Total:   len(sources),
			Enabled: len(sources),
			List:    sources,
		}
	}

	// 4. Try to fetch upstream status (non-blocking — best effort)
	var pipeline model.PipelineStatus
	if statusCode, body, err := service.ProxyGet(r.Context(), a.Config.SubsCheckHost, a.Config.SubsCheckPort, "/api/status", a.Config.APIKey); err == nil && statusCode == http.StatusOK {
		var statusJSON struct {
			Checking   bool `json:"checking"`
			Available  int  `json:"available"`
			ProxyCount int  `json:"proxyCount"`
			Pipeline   struct {
				SpeedPass int `json:"speedPass"`
			} `json:"pipeline"`
		}
		if json.Unmarshal(body, &statusJSON) == nil {
			pipeline = model.PipelineStatus{
				Checking:   statusJSON.Checking,
				Available:  statusJSON.Available,
				ProxyCount: statusJSON.ProxyCount,
				SpeedPass:  statusJSON.Pipeline.SpeedPass,
			}
		}
	}

	// Build aggregated response
	dashboard := map[string]interface{}{
		"health":  healthData,
		"nodes":   nodeSummary,
		"sources": sourceSummary,
		"status":  pipeline,
	}

	WriteJSON(w, http.StatusOK, service.FormatOK(dashboard))
}

// handleCSRFToken generates a new CSRF token
func (a *App) handleCSRFToken(w http.ResponseWriter, _ *http.Request) {
	token := middleware.GenerateCSRFToken(a.Config.APIKey, a.Config.CSRFTokenTTL)
	WriteJSON(w, http.StatusOK, service.FormatOK(map[string]string{"csrfToken": token}))
}

// handleLogin authenticates via API Key and creates a session cookie
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, service.FormatError(405003, "Method Not Allowed"))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, a.Config.MaxBodySize))
	if err != nil {
		WriteJSON(w, http.StatusRequestEntityTooLarge, service.FormatError(413002, "请求体过大"))
		return
	}

	var loginReq struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(body, &loginReq); err != nil {
		WriteJSON(w, http.StatusBadRequest, service.FormatError(400001, "无效的 JSON"))
		return
	}

	if loginReq.APIKey == "" {
		WriteJSON(w, http.StatusBadRequest, service.FormatError(400003, "请提供 API Key"))
		return
	}

	// Validate API key using constant-time comparison
	if middleware.CompareAPIKey(loginReq.APIKey, a.Config.APIKey) {
		sid := a.Sessions.Create(a.Config.APIKey)
		http.SetCookie(w, &http.Cookie{
			Name:     "substore_sid",
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   int(a.Config.CSRFTokenTTL / 1000),
			Secure:   isHTTPS(r),
		})
		WriteJSON(w, http.StatusOK, service.FormatOK(map[string]string{"message": "登录成功"}))
		return
	}

	WriteJSON(w, http.StatusUnauthorized, service.FormatError(401002, "API Key 无效"))
}

// handleLogout destroys the session and clears the cookie
func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteJSON(w, http.StatusMethodNotAllowed, service.FormatError(405004, "Method Not Allowed"))
		return
	}

	cookie, err := r.Cookie("substore_sid")
	if err == nil && cookie.Value != "" {
		a.Sessions.Destroy(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "substore_sid",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   0,
	})
	WriteJSON(w, http.StatusOK, service.FormatOK(map[string]string{"message": "已登出"}))
}

// handleNodes fetches and returns the node list with filtering and pagination.
// Uses a 30s TTL cache to avoid re-fetching and re-parsing YAML on every request.
func (a *App) handleNodes(w http.ResponseWriter, r *http.Request) {
	// Try cache first
	nodes := service.GetCachedNodes()
	if nodes == nil {
		// Fetch from subs-check (propagate request context for cancellation)
		statusCode, body, err := service.ProxyGet(r.Context(), a.Config.SubsCheckHost, a.Config.SubsCheckPort, "/sub/all.yaml", a.Config.APIKey)
		if err != nil {
			switch {
			case errors.Is(err, service.ErrUpstreamTimeout):
				WriteJSON(w, http.StatusGatewayTimeout, service.FormatError(504001, "上游服务响应超时"))
			case errors.Is(err, service.ErrUpstreamUnreachable):
				WriteJSON(w, http.StatusBadGateway, service.FormatError(502001, "上游服务不可达"))
			default:
				WriteJSON(w, http.StatusBadGateway, service.FormatError(502002, "拉取节点失败"))
			}
			return
		}
		if statusCode != http.StatusOK {
			WriteJSON(w, http.StatusBadGateway, service.FormatError(502002, fmt.Sprintf("拉取节点失败: HTTP %d", statusCode)))
			return
		}
		nodes = service.ParseNodesFromYaml(string(body))
		service.SetCachedNodes(nodes)
	}

	// Parse query parameters
	page := parseIntDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}
	pageSize := parseIntDefault(r.URL.Query().Get("pageSize"), a.Config.NodesPageSize)
	if pageSize < 1 {
		pageSize = a.Config.NodesPageSize
	}
	if pageSize > a.Config.NodesMaxPage {
		pageSize = a.Config.NodesMaxPage
	}
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
	typeFilter := r.URL.Query().Get("type")

	// Single-pass filter: combine search + type in one iteration
	// (was two separate O(n) scans, now one O(n) scan).
	// Skip allocation entirely when no filter is active.
	if search != "" || typeFilter != "" {
		filtered := make([]model.Node, 0, len(nodes))
		for _, n := range nodes {
			if search != "" && !strings.Contains(strings.ToLower(n.Name), search) {
				continue
			}
			if typeFilter != "" && n.Type != typeFilter {
				continue
			}
			filtered = append(filtered, n)
		}
		nodes = filtered
	}

	// Group by type
	byType := map[string]int{}
	for _, n := range nodes {
		byType[n.Type]++
	}

	// Paginate
	total := len(nodes)
	totalPages := (total + pageSize - 1) / pageSize
	offset := (page - 1) * pageSize
	end := offset + pageSize
	if end > total {
		end = total
	}
	if offset >= total {
		offset = total
	}
	pagedNodes := nodes[offset:end]

	// Convert to response format
	respNodes := make([]model.Node, len(pagedNodes))
	for i, n := range pagedNodes {
		ipv6 := n.IPv6 || strings.Contains(n.Server, ":")
		respNodes[i] = model.Node{
			Name:    n.Name,
			Type:    n.Type,
			Server:  n.Server,
			Port:    n.Port,
			TLS:     n.TLS,
			IPv6:    ipv6,
			Network: n.Network,
		}
	}

	WriteJSON(w, http.StatusOK, service.FormatOK(map[string]interface{}{
		"total":      total,
		"page":       page,
		"pageSize":   pageSize,
		"totalPages": totalPages,
		"byType":     byType,
		"nodes":      respNodes,
	}))
}

// handleSources handles GET (list) and POST (add) for subscription sources
func (a *App) handleSources(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, err := service.ReadConfigFile(a.Config.ConfigPath)
		if err != nil {
			WriteJSON(w, http.StatusInternalServerError, service.FormatError(500001, "无法读取配置文件"))
			return
		}
		sources := service.ParseSubUrls(cfg)
		WriteJSON(w, http.StatusOK, service.FormatOK(map[string]interface{}{"sources": sources}))
		return
	}

	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, a.Config.MaxBodySize))
		if err != nil {
			WriteJSON(w, http.StatusRequestEntityTooLarge, service.FormatError(413002, "请求体过大"))
			return
		}

		var addReq struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(body, &addReq); err != nil {
			WriteJSON(w, http.StatusBadRequest, service.FormatError(400001, "无效的 JSON"))
			return
		}

		if !service.ValidateURL(addReq.URL) {
			WriteJSON(w, http.StatusBadRequest, service.FormatError(400002, "URL 无效，需以 http:// 或 https:// 开头"))
			return
		}

		cfg, err := service.ReadConfigFile(a.Config.ConfigPath)
		if err != nil {
			WriteJSON(w, http.StatusInternalServerError, service.FormatError(500002, "无法读取配置文件"))
			return
		}

		// Check duplicates
		existing := service.ParseSubUrls(cfg)
		for _, s := range existing {
			if s.URL == addReq.URL {
				WriteJSON(w, http.StatusBadRequest, service.FormatError(400004, "该订阅源已存在"))
				return
			}
		}

		// Modify config.yaml
		lines := strings.Split(cfg, "\n")
		newLines := modifySubUrls(lines, addReq.URL)

		if err := service.WriteConfigFile(a.Config.ConfigPath, strings.Join(newLines, "\n")); err != nil {
			WriteJSON(w, http.StatusInternalServerError, service.FormatError(500003, "写入配置文件失败"))
			return
		}

		// Invalidate node cache since config changed
		service.InvalidateNodeCache()

		WriteJSON(w, http.StatusOK, service.FormatOK(map[string]string{
			"message": "订阅源已添加，请重启 subs-check 生效",
			"url":     addReq.URL,
		}))
		return
	}

	WriteJSON(w, http.StatusMethodNotAllowed, service.FormatError(405001, "Method Not Allowed"))
}

// handleUpdateSource handles PATCH /api/sources/:index — update a source URL.
// PATCH semantics: partial update, only provided fields change.
func (a *App) handleUpdateSource(w http.ResponseWriter, r *http.Request, pathname string) {
	if r.Method != http.MethodPatch {
		WriteJSON(w, http.StatusMethodNotAllowed, service.FormatError(405005, "Method Not Allowed"))
		return
	}

	indexStr := pathname[strings.LastIndex(pathname, "/")+1:]
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 {
		WriteJSON(w, http.StatusBadRequest, service.FormatError(400005, "订阅源序号无效"))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, a.Config.MaxBodySize))
	if err != nil {
		WriteJSON(w, http.StatusRequestEntityTooLarge, service.FormatError(413002, "请求体过大"))
		return
	}

	var updateReq struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &updateReq); err != nil {
		WriteJSON(w, http.StatusBadRequest, service.FormatError(400001, "无效的 JSON"))
		return
	}

	if !service.ValidateURL(updateReq.URL) {
		WriteJSON(w, http.StatusBadRequest, service.FormatError(400002, "URL 无效，需以 http:// 或 https:// 开头"))
		return
	}

	cfg, err := service.ReadConfigFile(a.Config.ConfigPath)
	if err != nil {
		WriteJSON(w, http.StatusInternalServerError, service.FormatError(500004, "无法读取配置文件"))
		return
	}

	lines := strings.Split(cfg, "\n")
	newLines, found := updateSubUrlsLine(lines, index, updateReq.URL)
	if !found {
		WriteJSON(w, http.StatusNotFound, service.FormatError(404004, "订阅源不存在"))
		return
	}

	if err := service.WriteConfigFile(a.Config.ConfigPath, strings.Join(newLines, "\n")); err != nil {
		WriteJSON(w, http.StatusInternalServerError, service.FormatError(500005, "写入配置文件失败"))
		return
	}

	// Invalidate node cache since config changed
	service.InvalidateNodeCache()

	WriteJSON(w, http.StatusOK, service.FormatOK(map[string]string{
		"message": "订阅源已更新，请重启 subs-check 生效",
	}))
}

// updateSubUrlsLine replaces the URL at the given index in sub-urls section.
// Returns the modified lines and whether the index was found.
func updateSubUrlsLine(lines []string, index int, newURL string) ([]string, bool) {
	inSubUrls := false
	urlIndex := -1

	for i := 0; i < len(lines); i++ {
		if reSubUrls.MatchString(lines[i]) {
			inSubUrls = true
			continue
		}
		if inSubUrls {
			if reNonSpaceStart.MatchString(lines[i]) {
				inSubUrls = false
				continue
			}
			trimmed := strings.TrimSpace(lines[i])
			if strings.HasPrefix(trimmed, "- ") && strings.Contains(trimmed, "http") {
				urlIndex++
				if urlIndex == index {
					lines[i] = `  - "` + newURL + `"`
					return lines, true
				}
			} else if trimmed == "[]" || !strings.HasPrefix(trimmed, "- ") {
				break
			}
		}
	}

	return lines, false
}

// handleDeleteSource handles DELETE /api/sources/:index
func (a *App) handleDeleteSource(w http.ResponseWriter, r *http.Request, pathname string) {
	if r.Method != http.MethodDelete {
		WriteJSON(w, http.StatusMethodNotAllowed, service.FormatError(405002, "Method Not Allowed"))
		return
	}

	indexStr := pathname[strings.LastIndex(pathname, "/")+1:]
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 {
		WriteJSON(w, http.StatusBadRequest, service.FormatError(400005, "订阅源序号无效"))
		return
	}

	cfg, err := service.ReadConfigFile(a.Config.ConfigPath)
	if err != nil {
		WriteJSON(w, http.StatusInternalServerError, service.FormatError(500004, "无法读取配置文件"))
		return
	}

	lines := strings.Split(cfg, "\n")
	newLines := deleteSubUrlsLine(lines, index)

	if err := service.WriteConfigFile(a.Config.ConfigPath, strings.Join(newLines, "\n")); err != nil {
		WriteJSON(w, http.StatusInternalServerError, service.FormatError(500005, "写入配置文件失败"))
		return
	}

	// Invalidate node cache since config changed
	service.InvalidateNodeCache()

	WriteJSON(w, http.StatusOK, service.FormatOK(map[string]string{
		"message": "订阅源已删除，请重启 subs-check 生效",
	}))
}

// modifySubUrls inserts a new URL into the sub-urls section of config.yaml
func modifySubUrls(lines []string, newURL string) []string {
	inSubUrls := false
	insertAt := -1
	foundEmptyArray := false

	for i := 0; i < len(lines); i++ {
		if reSubUrls.MatchString(lines[i]) {
			inSubUrls = true
			insertAt = i + 1
			continue
		}
		if inSubUrls {
			if reNonSpaceStart.MatchString(lines[i]) {
				break
			}
			trimmed := strings.TrimSpace(lines[i])
			if trimmed == "[]" {
				lines[i] = `  - "` + newURL + `"`
				foundEmptyArray = true
				break
			}
		}
	}

	if !foundEmptyArray && insertAt >= 0 {
		lines = append(lines[:insertAt], append([]string{`  - "` + newURL + `"`}, lines[insertAt:]...)...)
	}

	return lines
}

// deleteSubUrlsLine removes the URL at the given index from sub-urls section
func deleteSubUrlsLine(lines []string, index int) []string {
	inSubUrls := false
	urlIndex := -1
	urlLine := -1

	for i := 0; i < len(lines); i++ {
		if reSubUrls.MatchString(lines[i]) {
			inSubUrls = true
			continue
		}
		if inSubUrls {
			if reNonSpaceStart.MatchString(lines[i]) {
				inSubUrls = false
				continue
			}
			trimmed := strings.TrimSpace(lines[i])
			if strings.HasPrefix(trimmed, "- ") && strings.Contains(trimmed, "http") {
				urlIndex++
				if urlIndex == index {
					urlLine = i
					break
				}
			} else if trimmed == "[]" || !strings.HasPrefix(trimmed, "- ") {
				break
			}
		}
	}

	if urlLine >= 0 {
		lines = append(lines[:urlLine], lines[urlLine+1:]...)
	}

	return lines
}

// WriteJSON sends a JSON response
func WriteJSON(w http.ResponseWriter, status int, data *model.Response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func parseIntDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// Package-level pre-compiled regexes for YAML line matching (avoid per-call compilation).
var (
	reSubUrls       = regexp.MustCompile(`^sub-urls:\s*$`)
	reNonSpaceStart = regexp.MustCompile(`^\S`)
)

var startTime = time.Now()

// mimeTypes maps file extensions to MIME types; package-level to avoid per-request map allocation.
var mimeTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "application/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".woff":  "font/woff",
}

func getMimeType(ext string) string {
	if m, ok := mimeTypes[ext]; ok {
		return m
	}
	return "application/octet-stream"
}

// ServeStatic serves static files with path traversal protection and ETag.
// Uses http.ServeContent for streaming (zero-copy io.Copy) instead of
// loading entire file into memory — critical for large JS/CSS bundles.
// Reference: net/http.ServeContent — handles Range, If-Modified-Since, etc.
func ServeStatic(w http.ResponseWriter, r *http.Request, pathname string, publicDir string) {
	var filePath string
	if pathname == "/" || pathname == "" {
		filePath = filepath.Join(publicDir, "index.html")
	} else {
		filePath = filepath.Join(publicDir, pathname)
	}

	// Path traversal protection: ensure the cleaned path is strictly inside publicDir
	cleanPath := filepath.Clean(filePath)
	cleanPublic := filepath.Clean(publicDir)
	rel, err := filepath.Rel(cleanPublic, cleanPath)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		WriteJSON(w, http.StatusForbidden, service.FormatError(403001, "禁止访问"))
		return
	}

	f, err := os.Open(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			WriteJSON(w, http.StatusNotFound, service.FormatError(404003, "页面不存在"))
			return
		}
		WriteJSON(w, http.StatusInternalServerError, service.FormatError(500006, "读取文件失败"))
		return
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			log.Printf("[handler] 关闭文件 %s 失败: %v", cleanPath, cerr)
		}
	}()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		WriteJSON(w, http.StatusNotFound, service.FormatError(404003, "页面不存在"))
		return
	}

	ext := strings.ToLower(filepath.Ext(cleanPath))
	mime := getMimeType(ext)

	// ETag for conditional requests (computed from size + modtime, no full read)
	etag := fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano())
	if etagMatch := r.Header.Get("If-None-Match"); etagMatch == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	cacheControl := "public, max-age=3600"
	if ext == ".html" {
		cacheControl = "no-cache"
	}

	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("ETag", etag)
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

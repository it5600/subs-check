package app

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"substore-ui-go/internal/config"
	"substore-ui-go/internal/handler"
	"substore-ui-go/internal/middleware"
	"substore-ui-go/internal/model"
	"substore-ui-go/internal/service"
	scconfig "substore-ui-go/internal/subscheck/config"
	"substore-ui-go/internal/subscheck/check"
	"substore-ui-go/internal/subscheck/export"
	"substore-ui-go/internal/subscheck/save"
	"substore-ui-go/internal/subscheck/save/method"
	"github.com/gin-contrib/pprof"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// SubStoreApp holds the merged application state
type SubStoreApp struct {
	cfg        *config.Config
	handlerApp *handler.App
	scApp      *App
	configPath string
	version    string
}

// NewMergedApp creates a merged application
func NewMergedApp(cfg *config.Config, scVersion string) *SubStoreApp {
	scApp := New(scVersion)
	return &SubStoreApp{
		cfg:        cfg,
		handlerApp: handler.NewApp(cfg),
		scApp:      scApp,
		version:    scVersion,
	}
}

// Initialize initializes the subs-check core
func (a *SubStoreApp) Initialize() error {
	return a.scApp.Initialize()
}

// Run starts the merged application
func (a *SubStoreApp) Run() {
	a.scApp.Run()
}

// HandlerApp returns the handler app for accessing shared components like rate limiter.
func (a *SubStoreApp) HandlerApp() *handler.App {
	return a.handlerApp
}

// SetupRoutes registers all routes on the Gin engine
func (a *SubStoreApp) SetupRoutes(router *gin.Engine) error {
	gin.SetMode(gin.ReleaseMode)
	gin.DefaultWriter = GinFileLogger
	gin.DefaultErrorWriter = GinFileLogger

	saver, err := method.NewLocalSaver()
	if err != nil {
		return fmt.Errorf("获取http监听目录失败: %w", err)
	}

	// ===== subs-check static file routes =====
	router.StaticFile("/all.yaml", saver.OutputPath+"/all.yaml")
	router.StaticFile("/all.txt", saver.OutputPath+"/all.txt")
	router.StaticFile("/base64.txt", saver.OutputPath+"/base64.txt")
	router.StaticFile("/mihomo.yaml", saver.OutputPath+"/mihomo.yaml")
	router.StaticFile("/ACL4SSR_Online_Full.yaml", saver.OutputPath+"/ACL4SSR_Online_Full.yaml")
	router.StaticFile("/bdg.yaml", saver.OutputPath+"/bdg.yaml")
	router.Static("/sub/", saver.OutputPath)

	// Public export
	router.GET("/export/:target", a.exportHandler)

	// pprof: profiling endpoints expose process internals (heap dumps, goroutine
	// stacks, request stats). They are only registered when explicitly enabled
	// via PPROF_ENABLED=true, and only on a non-gateway listener. Behind the
	// fnOS gateway the socket is reachable by any process on the host, so
	// exposing pprof there would leak credentials and internal state.
	if enablePprof() {
		pprof.Register(router)
		slog.Info("pprof 已启用（调试端口）", "路由", "/debug/pprof")
	} else {
		slog.Info("pprof 未启用（如需调试请设置 PPROF_ENABLED=true）")
	}

	// ===== Embedded static and templates =====
	if scconfig.GlobalConfig.EnableWebUI {
		if scconfig.GlobalConfig.APIKey == "" {
			if apiKey := os.Getenv("API_KEY"); apiKey != "" {
				scconfig.GlobalConfig.APIKey = apiKey
			} else {
				scconfig.GlobalConfig.APIKey = GenerateSimpleKey()
				slog.Warn("未设置api-key，已生成一个随机api-key", "api-key", scconfig.GlobalConfig.APIKey)
			}
		}
		slog.Info("启用Web控制面板", "path", "http://ip:port/admin", "api-key", scconfig.GlobalConfig.APIKey)

		router.SetHTMLTemplate(template.Must(template.New("").ParseFS(configFS, "templates/*.html")))

		staticFS, err := fs.Sub(configFS, "static")
		if err != nil {
			return fmt.Errorf("加载内置静态资源失败: %w", err)
		}
		router.StaticFS("/static", http.FS(staticFS))
	}

	// ===== subs-check API routes (with auth) =====
	api := router.Group("/api")

	// Public endpoints (no auth required)
	api.GET("/health", a.handleHealth)
	api.GET("/csrf-token", a.handleCSRFToken)
	api.POST("/login", a.handleLogin)
	api.POST("/logout", a.handleLogout)

	// subs-check authed routes (X-API-Key)
	api.Use(a.authMiddleware(scconfig.GlobalConfig.APIKey))
	{
		api.GET("/dashboard", a.handleDashboard)
		api.GET("/config", a.getConfig)
		api.POST("/config", a.updateConfig)
		api.GET("/status", a.getStatus)
		api.POST("/trigger-check", a.triggerCheckHandler)
		api.POST("/force-close", a.forceCloseHandler)
		api.GET("/version", a.getVersion)
		api.GET("/logs", a.getLogs)
		api.GET("/results", a.getResults)
		api.GET("/export", a.getExportStatus)
		api.POST("/export/:target", a.generateExport)
		api.DELETE("/export/:target", a.disableExport)
	}

	// ===== substore-ui-go API routes (with session/API-key auth) =====
	subAPI := router.Group("/api")
	subAPI.Use(a.sessionAuthMiddleware())
	{
		subAPI.GET("/nodes", a.handleNodes)
		subAPI.GET("/sources", a.handleSources)
		subAPI.POST("/sources", a.handleAddSource)
		subAPI.PATCH("/sources/:index", a.handleUpdateSource)
		subAPI.DELETE("/sources/:index", a.handleDeleteSource)
		subAPI.Any("/substore/*path", a.handleSubStoreProxy)
	}

	// ===== subs-check admin pages =====
	if scconfig.GlobalConfig.EnableWebUI {
		router.GET("/admin", func(c *gin.Context) {
			c.HTML(http.StatusOK, "admin.html", gin.H{
				"configPath": a.scApp.configPath,
				"nav":        "admin",
			})
		})
		router.GET("/admin/results", func(c *gin.Context) {
			c.HTML(http.StatusOK, "results.html", gin.H{"nav": "results"})
		})
	}

	// ===== substore-ui-go frontend =====
	// NOTE: router.Static("/", ...) cannot be used here. Gin expands it to a
	// catch-all wildcard "/*filepath", which conflicts with the already
	// registered "/api" group and panics at startup:
	//   panic: catch-all wildcard '*filepath' in new path '/*filepath'
	//          conflicts with existing path segment 'a' in existing prefix '/a'
	// Instead the frontend is served through NoRoute, which only fires for
	// paths that matched no route — same behaviour, no route-tree conflict.
	router.NoRoute(a.serveFrontend)

	return nil
}

// mimeTypes maps file extensions to MIME types for the frontend handler.
// Package-level to avoid a per-request map allocation.
var mimeTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "application/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".ttf":   "font/ttf",
	".map":   "application/json; charset=utf-8",
}

// serveFrontend serves the single-page frontend from PublicDir.
//
// It mirrors the behaviour of router.Static("/", dir) — "/" resolves to
// index.html — while keeping path-traversal protection, ETag support and
// streaming via http.ServeContent (zero-copy for large bundles).
func (a *SubStoreApp) serveFrontend(c *gin.Context) {
	// Only GET/HEAD are meaningful for static assets.
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	publicDir := filepath.Clean(a.cfg.PublicDir)

	// Strip the gateway prefix so the same binary works behind the fnOS
	// unified gateway (which forwards the prefixed path) and standalone.
	requestPath := c.Request.URL.Path
	if prefix := strings.TrimSuffix(a.cfg.GatewayPrefix, "/"); prefix != "" {
		if requestPath == prefix {
			requestPath = "/"
		} else if strings.HasPrefix(requestPath, prefix+"/") {
			requestPath = strings.TrimPrefix(requestPath, prefix)
		}
	}

	relPath := strings.TrimPrefix(requestPath, "/")
	target := filepath.Join(publicDir, relPath)

	// The root path must serve index.html rather than the directory itself.
	if relPath == "" {
		target = filepath.Join(publicDir, "index.html")
	}

	// Path traversal protection: the resolved path must stay inside publicDir.
	cleanPublic := filepath.Clean(publicDir)
	cleanTarget := filepath.Clean(target)
	rel, err := filepath.Rel(cleanPublic, cleanTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	f, err := os.Open(cleanTarget)
	if err != nil {
		if os.IsNotExist(err) {
			// Unknown path: fall back to index.html so client-side routing
			// (hash-based views) keeps working on a hard refresh.
			f, err = os.Open(filepath.Join(cleanPublic, "index.html"))
			if err != nil {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			cleanTarget = filepath.Join(cleanPublic, "index.html")
		} else {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	ext := strings.ToLower(filepath.Ext(cleanTarget))
	mime, ok := mimeTypes[ext]
	if !ok {
		mime = "application/octet-stream"
	}

	// ETag computed from size + modtime — no full file read.
	etag := fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano())
	w := c.Writer
	w.Header().Set("Content-Type", mime)
	if ext == ".html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.Header().Set("ETag", etag)

	if match := c.Request.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	http.ServeContent(w, c.Request, info.Name(), info.ModTime(), f)
}

// ===== Auth Middleware =====

func (a *SubStoreApp) authMiddleware(key string) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.GetHeader("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(apiKey), []byte(key)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的API密钥"})
			return
		}
		c.Next()
	}
}

func (a *SubStoreApp) sessionAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Gateway requests must additionally prove administrator privilege:
		// the gateway authenticates the NAS user, but the panel is a
		// management surface that should not be readable by ordinary users.
		if gwUser, ok := middleware.ParseGatewayIdentity(c.Request); ok {
			if !gwUser.IsAdmin {
				a.handlerApp.Stats.IncErrors()
				c.JSON(http.StatusForbidden, gin.H{"code": 403004, "message": "需要管理员权限"})
				c.Abort()
				return
			}
			c.Set("gateway_uid", gwUser.UID)
			c.Set("gateway_username", gwUser.Username)
			c.Next()
			return
		}

		valid, authType := middleware.CheckAuth(c.Request, a.cfg.APIKey, a.handlerApp.Sessions)
		if !valid {
			a.handlerApp.Stats.IncErrors()
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401001, "message": "未授权"})
			c.Abort()
			return
		}

		if authType != "gateway" {
			if c.Request.Method == http.MethodPost || c.Request.Method == http.MethodDelete || c.Request.Method == http.MethodPatch {
				if !middleware.ValidateCSRFToken(c.Request, a.cfg.APIKey) {
					a.handlerApp.Stats.IncErrors()
					c.JSON(http.StatusForbidden, gin.H{"code": 403003, "message": "CSRF 校验失败，请刷新页面获取新的 Token"})
					c.Abort()
					return
				}
			}
		}
		c.Next()
	}
}

// enablePprof reports whether profiling endpoints should be registered.
// Disabled by default so the process does not expose heap dumps and goroutine
// stacks to whoever can reach the listener.
func enablePprof() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("PPROF_ENABLED")))
	return v == "1" || v == "true" || v == "yes"
}

// ===== subs-check API Handlers =====

func (a *SubStoreApp) getConfig(c *gin.Context) {
	configData, err := os.ReadFile(a.scApp.configPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("读取配置文件失败: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"content": string(configData)})
}

func (a *SubStoreApp) updateConfig(c *gin.Context) {
	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的请求格式"})
		return
	}
	var yamlData map[string]any
	if err := yaml.Unmarshal([]byte(req.Content), &yamlData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("YAML格式错误: %v", err)})
		return
	}
	if err := os.WriteFile(a.scApp.configPath, []byte(req.Content), 0644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("保存配置文件失败: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "配置已更新"})
}

func (a *SubStoreApp) getStatus(c *gin.Context) {
	phaseResults := make(map[string]*check.PhaseResult, 3)
	for i := 1; i <= 3; i++ {
		phaseResults[fmt.Sprintf("%d", i)] = check.GetPhaseResult(i)
	}
	pipeline := gin.H{
		"total":      check.ProxyCount.Load(),
		"aliveDone":  check.Progress.Load(),
		"alivePass":  check.Available.Load(),
		"mediaDone":  check.MediaDone.Load(),
		"filterPass": check.FilterPassed.Load(),
		"speedDone":  check.SpeedDone.Load(),
		"speedPass":  check.SpeedOk.Load(),
	}
	c.JSON(http.StatusOK, gin.H{
		"checking":     a.scApp.checking.Load(),
		"proxyCount":   check.ProxyCount.Load(),
		"available":    check.Available.Load(),
		"progress":     check.Progress.Load(),
		"phase":        check.Phase.Load(),
		"phaseResults": phaseResults,
		"pipeline":     pipeline,
		"hasSpeedTest": scconfig.GlobalConfig.SpeedTestUrl != "",
	})
}

func (a *SubStoreApp) triggerCheckHandler(c *gin.Context) {
	a.scApp.TriggerCheck()
	c.JSON(http.StatusOK, gin.H{"message": "已触发检测"})
}

func (a *SubStoreApp) forceCloseHandler(c *gin.Context) {
	check.RequestCancel()
	c.JSON(http.StatusOK, gin.H{"message": "已强制关闭"})
}

func (a *SubStoreApp) getLogs(c *gin.Context) {
	logPath := TempLog()
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		c.JSON(http.StatusOK, gin.H{"logs": []string{}})
		return
	}
	lines, err := ReadLastNLines(logPath, 100)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("读取日志失败: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": lines})
}

func (a *SubStoreApp) getVersion(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"version": a.version})
}

func (a *SubStoreApp) getResults(c *gin.Context) {
	data, err := os.ReadFile(save.ResultsPath())
	if errors.Is(err, fs.ErrNotExist) {
		c.JSON(http.StatusOK, gin.H{"nodes": []any{}})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("读取检测结果失败: %v", err)})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json; charset=utf-8", data)
}

func (a *SubStoreApp) getExportStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"enabled": export.Enabled(),
		"targets": export.Default().Statuses(),
	})
}

func (a *SubStoreApp) generateExport(c *gin.Context) {
	id := c.Param("target")
	t, ok := export.Lookup(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": export.ErrUnknownTarget.Error()})
		return
	}
	if t.Preset {
		c.JSON(http.StatusBadRequest, gin.H{"error": export.ErrPreset.Error()})
		return
	}
	if !export.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未启用 sub-store（sub-store-port 为空），无法导出订阅"})
		return
	}
	status, err := export.Default().Generate(id)
	var cooldown *export.CooldownError
	switch {
	case err == nil:
		c.JSON(http.StatusAccepted, status)
	case errors.As(err, &cooldown):
		c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error(), "status": status})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "status": status})
	}
}

func (a *SubStoreApp) disableExport(c *gin.Context) {
	id := c.Param("target")
	t, ok := export.Lookup(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": export.ErrUnknownTarget.Error()})
		return
	}
	if t.Preset {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该格式每轮检测完成后自动生成，不能停用"})
		return
	}
	status, err := export.Default().Disable(id)
	if err != nil {
		slog.Warn(fmt.Sprintf("停用导出订阅 %s 失败: %v", id, err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "status": status})
		return
	}
	c.JSON(http.StatusOK, status)
}

func (a *SubStoreApp) exportHandler(c *gin.Context) {
	id := c.Param("target")
	if t, ok := export.Lookup(id); !ok || t.Preset {
		c.String(http.StatusNotFound, export.ErrUnknownTarget.Error())
		return
	}
	served, err := export.Default().Open(id)
	if err != nil {
		if errors.Is(err, export.ErrNotGenerated) {
			c.String(http.StatusNotFound, err.Error())
			return
		}
		slog.Error(fmt.Sprintf("读取导出订阅 %s 失败: %v", id, err))
		c.String(http.StatusInternalServerError, "读取导出订阅失败")
		return
	}
	defer served.File.Close()
	c.Header("Content-Type", served.ContentType)
	c.Header("Cache-Control", "no-cache")
	http.ServeContent(c.Writer, c.Request, "", served.ModTime, served.File)
}

// ===== substore-ui-go API Handlers (adapted to Gin) =====

// buildHealthData constructs the shared health response payload used by
// both /api/health and /api/dashboard. Extracted to avoid duplication.
func (a *SubStoreApp) buildHealthData() gin.H {
	stats := a.handlerApp.Stats.Snapshot()
	return gin.H{
		"service": "substore-ui-go",
		"uptime":  int64(time.Since(startTime).Seconds()),
		"stats": gin.H{
			"requests":     stats.Requests,
			"errors":       stats.Errors,
			"rateLimited":  stats.RateLimited,
			"rateLimitMap": a.handlerApp.RateLimiter.Size(),
		},
		"upstream": gin.H{
			"subsCheck": fmt.Sprintf("http://%s:%d", a.cfg.SubsCheckHost, a.cfg.SubsCheckPort),
			"subStore":  fmt.Sprintf("http://%s:%d%s", a.cfg.SubStoreHost, a.cfg.SubStorePort, a.cfg.SubStorePath),
		},
	}
}

func (a *SubStoreApp) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, a.buildHealthData())
}

func (a *SubStoreApp) handleCSRFToken(c *gin.Context) {
	token := middleware.GenerateCSRFToken(a.cfg.APIKey, a.cfg.CSRFTokenTTL)
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": gin.H{"csrfToken": token}})
}

func (a *SubStoreApp) handleLogin(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		c.JSON(http.StatusMethodNotAllowed, gin.H{"code": 405003, "message": "Method Not Allowed"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, a.cfg.MaxBodySize))
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"code": 413002, "message": "请求体过大"})
		return
	}

	var loginReq struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(body, &loginReq); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400001, "message": "无效的 JSON"})
		return
	}

	if loginReq.APIKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400003, "message": "请提供 API Key"})
		return
	}

	if middleware.CompareAPIKey(loginReq.APIKey, a.cfg.APIKey) {
		sid := a.handlerApp.Sessions.Create(a.cfg.APIKey)
		c.SetCookie("substore_sid", sid, int(a.cfg.CSRFTokenTTL/1000), "/", "", isHTTPS(c.Request), true)
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": gin.H{"message": "登录成功"}})
		return
	}

	c.JSON(http.StatusUnauthorized, gin.H{"code": 401002, "message": "API Key 无效"})
}

func (a *SubStoreApp) handleLogout(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		c.JSON(http.StatusMethodNotAllowed, gin.H{"code": 405004, "message": "Method Not Allowed"})
		return
	}

	cookie, err := c.Request.Cookie("substore_sid")
	if err == nil && cookie.Value != "" {
		a.handlerApp.Sessions.Destroy(cookie.Value)
	}

	c.SetCookie("substore_sid", "", 0, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": gin.H{"message": "已登出"}})
}

func (a *SubStoreApp) handleDashboard(c *gin.Context) {
	healthData := a.buildHealthData()

	// Parallelize data gathering: results.json read, config read, and
	// pipeline status collection each run in their own goroutine.
	// The total latency is now bounded by the slowest operation rather
	// than the sum of all three.
	type dashboardResults struct {
		nodeSummary   model.NodeSummary
		sourceSummary model.SourceSummary
		pipeline      model.PipelineStatus
	}

	resCh := make(chan dashboardResults, 1)

	go func() {
		var nodeSummary model.NodeSummary
		if resultsData, err := os.ReadFile(save.ResultsPath()); err == nil {
			var results save.ResultsSnapshot
			if json.Unmarshal(resultsData, &results) == nil && len(results.Nodes) > 0 {
				nodeSummary = model.NodeSummary{
					Total:     len(results.Nodes),
					ByType:    map[string]int{},
					Available: countAvailable(results.Nodes),
				}
				for _, n := range results.Nodes {
					nodeSummary.ByType[n.Type]++
				}
			}
		}

		var sourceSummary model.SourceSummary
		if cfgData, err := service.ReadConfigFile(a.cfg.ConfigPath); err == nil {
			sources := service.ParseSubUrls(cfgData)
			sourceSummary = model.SourceSummary{
				Total:   len(sources),
				Enabled: len(sources),
				List:    sources,
			}
		}

		pipeline := model.PipelineStatus{
			Checking:   a.scApp.checking.Load(),
			Available:  int(check.Available.Load()),
			ProxyCount: int(check.ProxyCount.Load()),
			SpeedPass:  int(check.SpeedOk.Load()),
		}

		resCh <- dashboardResults{nodeSummary, sourceSummary, pipeline}
	}()

	res := <-resCh

	dashboard := gin.H{
		"health":  healthData,
		"nodes":   res.nodeSummary,
		"sources": res.sourceSummary,
		"status":  res.pipeline,
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": dashboard})
}

func (a *SubStoreApp) handleNodes(c *gin.Context) {
	nodes, err := service.FetchNodesWithSingleflight(func() ([]model.Node, error) {
		statusCode, body, err := service.ProxyGet(c.Request.Context(), a.cfg.SubsCheckHost, a.cfg.SubsCheckPort, "/sub/all.yaml", a.cfg.APIKey)
		if err != nil {
			return nil, err
		}
		if statusCode != http.StatusOK {
			return nil, fmt.Errorf("upstream returned HTTP %d", statusCode)
		}
		return service.ParseNodesFromYaml(string(body)), nil
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrUpstreamTimeout):
			c.JSON(http.StatusGatewayTimeout, gin.H{"code": 504001, "message": "上游服务响应超时"})
		case errors.Is(err, service.ErrUpstreamUnreachable):
			c.JSON(http.StatusBadGateway, gin.H{"code": 502001, "message": "上游服务不可达"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"code": 502002, "message": "拉取节点失败"})
		}
		return
	}

	page := parseIntDefault(c.Query("page"), 1)
	if page < 1 {
		page = 1
	}
	pageSize := parseIntDefault(c.Query("pageSize"), a.cfg.NodesPageSize)
	if pageSize < 1 {
		pageSize = a.cfg.NodesPageSize
	}
	if pageSize > a.cfg.NodesMaxPage {
		pageSize = a.cfg.NodesMaxPage
	}
	search := strings.ToLower(strings.TrimSpace(c.Query("search")))
	typeFilter := c.Query("type")

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

	byType := map[string]int{}
	for _, n := range nodes {
		byType[n.Type]++
	}

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

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "ok",
		"data": gin.H{
			"total":      total,
			"page":       page,
			"pageSize":   pageSize,
			"totalPages": totalPages,
			"byType":     byType,
			"nodes":      pagedNodes,
		},
	})
}

func (a *SubStoreApp) handleSources(c *gin.Context) {
	cfg, err := service.ReadConfigFile(a.cfg.ConfigPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500001, "message": "无法读取配置文件"})
		return
	}
	sources := service.ParseSubUrls(cfg)
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": gin.H{"sources": sources}})
}

func (a *SubStoreApp) handleAddSource(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, a.cfg.MaxBodySize))
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"code": 413002, "message": "请求体过大"})
		return
	}

	var addReq struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &addReq); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400001, "message": "无效的 JSON"})
		return
	}

	if !service.ValidateURL(addReq.URL) {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400002, "message": "URL 无效，需以 http:// 或 https:// 开头"})
		return
	}

	cfg, err := service.ReadConfigFile(a.cfg.ConfigPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500002, "message": "无法读取配置文件"})
		return
	}

	existing := service.ParseSubUrls(cfg)
	for _, s := range existing {
		if s.URL == addReq.URL {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400004, "message": "该订阅源已存在"})
			return
		}
	}

	lines := strings.Split(cfg, "\n")
	newLines := modifySubUrls(lines, addReq.URL)

	if err := service.WriteConfigFile(a.cfg.ConfigPath, strings.Join(newLines, "\n")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500003, "message": "写入配置文件失败"})
		return
	}

	service.InvalidateNodeCache()
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": gin.H{
		"message": "订阅源已添加，请重启 subs-check 生效",
		"url":     addReq.URL,
	}})
}

func (a *SubStoreApp) handleUpdateSource(c *gin.Context) {
	indexStr := c.Param("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400005, "message": "订阅源序号无效"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, a.cfg.MaxBodySize))
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"code": 413002, "message": "请求体过大"})
		return
	}

	var updateReq struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &updateReq); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400001, "message": "无效的 JSON"})
		return
	}

	if !service.ValidateURL(updateReq.URL) {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400002, "message": "URL 无效，需以 http:// 或 https:// 开头"})
		return
	}

	cfg, err := service.ReadConfigFile(a.cfg.ConfigPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500004, "message": "无法读取配置文件"})
		return
	}

	lines := strings.Split(cfg, "\n")
	newLines, found := updateSubUrlsLine(lines, index, updateReq.URL)
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"code": 404004, "message": "订阅源不存在"})
		return
	}

	if err := service.WriteConfigFile(a.cfg.ConfigPath, strings.Join(newLines, "\n")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500005, "message": "写入配置文件失败"})
		return
	}

	service.InvalidateNodeCache()
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": gin.H{"message": "订阅源已更新，请重启 subs-check 生效"}})
}

func (a *SubStoreApp) handleDeleteSource(c *gin.Context) {
	indexStr := c.Param("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400005, "message": "订阅源序号无效"})
		return
	}

	cfg, err := service.ReadConfigFile(a.cfg.ConfigPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500004, "message": "无法读取配置文件"})
		return
	}

	lines := strings.Split(cfg, "\n")
	newLines := deleteSubUrlsLine(lines, index)

	if err := service.WriteConfigFile(a.cfg.ConfigPath, strings.Join(newLines, "\n")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500005, "message": "写入配置文件失败"})
		return
	}

	service.InvalidateNodeCache()
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": gin.H{"message": "订阅源已删除，请重启 subs-check 生效"}})
}

func (a *SubStoreApp) handleSubStoreProxy(c *gin.Context) {
	subPath := strings.TrimPrefix(c.Param("path"), "/")
	targetPath := a.cfg.SubStorePath + "/" + subPath
	service.ProxyRequest(c.Writer, c.Request, a.cfg.SubStoreHost, a.cfg.SubStorePort, targetPath, a.cfg.APIKey)
}

// ===== Helpers =====

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
		// Insert in-place: extend, shift right, then place the new element.
		// Single allocation vs. double-append's two allocations.
		lines = append(lines, "")
		copy(lines[insertAt+1:], lines[insertAt:])
		lines[insertAt] = `  - "` + newURL + `"`
	}

	return lines
}

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

func parseIntDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func countAvailable(nodes []save.NodeRecord) int {
	count := 0
	for _, n := range nodes {
		if n.Speed > 0 {
			count++
		} else if n.Country != "" && n.IPRisk != "high" {
			count++
		}
	}
	return count
}

func GenerateSimpleKey() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b[:])
}

func ReadLastNLines(filePath string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}

	const chunkSize int64 = 8192
	var chunks [][]byte
	off := size
	newlineCount := 0

	// Read backwards in chunks, collecting them without O(n²) copies.
	// Each chunk is appended to the chunks slice (O(1) amortized),
	// and newlines are counted per-chunk to avoid scanning the whole buffer.
	for off > 0 {
		readSize := chunkSize
		if off < readSize {
			readSize = off
		}
		off -= readSize

		tmp := make([]byte, readSize)
		if _, err := f.ReadAt(tmp, off); err != nil && err != io.EOF {
			return nil, err
		}
		chunks = append(chunks, tmp)
		newlineCount += bytes.Count(tmp, []byte{'\n'})

		if newlineCount > n {
			break
		}
	}

	// Reassemble chunks in correct order (they were read backwards)
	// using bytes.Buffer, which grows its internal slice geometrically
	// (amortized O(n) total copies vs. the old append-to-front's O(n²)).
	var buf bytes.Buffer
	for i := len(chunks) - 1; i >= 0; i-- {
		buf.Write(chunks[i])
	}

	bufBytes := buf.Bytes()
	bufBytes = bytes.TrimRight(bufBytes, "\n")
	if len(bufBytes) == 0 {
		return nil, nil
	}

	lines := strings.Split(string(bufBytes), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, "\r")
	}
	return lines, nil
}

// Package-level variables
var (
	reSubUrls       = regexp.MustCompile(`^sub-urls:\s*$`)
	reNonSpaceStart = regexp.MustCompile(`^\S`)
	startTime       = time.Now()
)

// substore-ui-go is a unified Sub-Store management panel with integrated subs-check.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"substore-ui-go/internal/config"
	"substore-ui-go/internal/middleware"
	"substore-ui-go/internal/subscheck/assets"
	scapp "substore-ui-go/internal/subscheck/app"
	"substore-ui-go/internal/subscheck/check"
	"github.com/gin-gonic/gin"
)

// version 由构建时 -ldflags "-X main.version=..." 注入，便于在日志中确认运行版本
var version = "1.0.0"

// buildTime 由构建时 -ldflags "-X main.buildTime=..." 注入
var buildTime = "unknown"

func main() {
	cfg := config.Load()

	slog.Info("应用版本", "version", version, "build", buildTime)

	// Create merged application
	mergedApp := scapp.NewMergedApp(cfg, version)

	// Initialize subs-check core (config loading, DNS, watcher, sub-store service)
	if err := mergedApp.Initialize(); err != nil {
		slog.Error(fmt.Sprintf("初始化失败: %v", err))
		os.Exit(1)
	}

	// Start background cleanup goroutines to prevent memory leaks.
	// - Rate limiter buckets: cleaned every window interval
	// - CSRF tokens: cleaned every 5 minutes
	go startBackgroundCleanup(mergedApp)

	// Create Gin router
	router := gin.New()
	router.Use(gin.Recovery())

	// Setup all routes
	if err := mergedApp.SetupRoutes(router); err != nil {
		slog.Error(fmt.Sprintf("路由配置失败: %v", err))
		os.Exit(1)
	}

	// Start HTTP server.
	//
	// Two modes are supported:
	//   - Gateway mode (Native FPK): when SOCKET_PATH is set the server listens
	//     on a Unix domain socket and the fnOS unified gateway proxies
	//     /app/<appname>/* to it. No TCP port is consumed.
	//   - TCP mode (standalone / Docker): fall back to Host:Port.
	//
	// The gateway may or may not strip the /app/<appname> prefix before
	// forwarding, so the handler strips it when present. Doing this outside
	// Gin (before route matching) makes every route work under both
	// conventions.
	handler := http.Handler(router)
	if prefix := cfg.GatewayPrefix; prefix != "" {
		handler = &prefixStripper{prefix: strings.TrimSuffix(prefix, "/"), next: router}
	}

	// Wrap the handler chain so every response carries the security headers,
	// every request is logged, and text responses are gzip-compressed when the
	// client accepts it (frontend bundles shrink 60-80% over NAS LAN).
	handler = middleware.SecurityHeadersHandler(cfg.SecurityHeaders, handler)
	handler = middleware.LogRequest(handler, cfg.AccessLogPath)
	handler = middleware.GzipCompress(handler)

	// Graceful shutdown: register signal handler so that SIGTERM/SIGINT
	// stops the HTTP server gracefully (draining in-flight requests)
	// before the process exits. This prevents the gateway from seeing
	// 502s on the last few requests during a fnOS app upgrade.
	srv := newHTTPServer(handler)
	go signalHandler(srv, mergedApp)

	if cfg.SocketPath != "" {
		go serveUnixSocket(srv, cfg.SocketPath)
	} else {
		go serveTCP(srv, cfg)
	}

	// Run subs-check main loop (triggerCheck, cron/ticker)
	mergedApp.Run()
}

// prefixStripper removes a leading path prefix before the request reaches the
// router.
//
// The fnOS unified gateway forwards requests to the application's Unix socket
// under the /app/<appname> prefix. Depending on the gateway version the
// prefix may or may not already be stripped, so the application removes it
// when it is present. That keeps a single build working both behind the
// gateway and standalone.
type prefixStripper struct {
	prefix string
	next   http.Handler
}

// ServeHTTP implements http.Handler.
func (p *prefixStripper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.prefix == "" {
		p.next.ServeHTTP(w, r)
		return
	}

	path := r.URL.Path
	if path == p.prefix || strings.HasPrefix(path, p.prefix+"/") {
		stripped := strings.TrimPrefix(path, p.prefix)
		if stripped == "" {
			stripped = "/"
		}

		// Clone the request so the original URL stays untouched for logging.
		r2 := r.Clone(r.Context())
		r2.URL.Path = stripped
		r2.RequestURI = stripped
		p.next.ServeHTTP(w, r2)
		return
	}

	p.next.ServeHTTP(w, r)
}

// serveTCP starts the HTTP server on a TCP address.
func serveTCP(srv *http.Server, cfg *config.Config) {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv.Addr = addr
	slog.Info(fmt.Sprintf("HTTP服务器启动，监听 %s", addr))
	if err := srv.ListenAndServe(); err != nil {
		slog.Error(fmt.Sprintf("HTTP服务器启动失败: %v", err))
	}
}

// serveUnixSocket starts the HTTP server on a Unix domain socket.
func serveUnixSocket(srv *http.Server, socketPath string) {
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		slog.Warn(fmt.Sprintf("清理旧 socket 文件失败: %v", err))
	}

	if dir := socketParent(socketPath); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Error(fmt.Sprintf("创建 socket 目录失败: %v", err))
			return
		}
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		slog.Error(fmt.Sprintf("Unix Socket 监听失败: %v", err))
		return
	}

	if err := os.Chmod(socketPath, 0660); err != nil {
		slog.Warn(fmt.Sprintf("设置 socket 权限失败: %v", err))
	}

	slog.Info(fmt.Sprintf("Unix Socket 监听启动: %s", socketPath))

	if err := srv.Serve(listener); err != nil {
		slog.Error(fmt.Sprintf("Unix Socket 服务异常退出: %v", err))
	}
}

// signalHandler listens for SIGTERM/SIGINT and initiates graceful shutdown
// of the HTTP server, then stops the subs-check loop.
func signalHandler(srv *http.Server, _ *scapp.SubStoreApp) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	sig := <-sigChan
	slog.Info(fmt.Sprintf("收到退出信号: %v，开始优雅关闭", sig))

	// Cancel any running check pipeline first.
	check.RequestCancel()

	// Shutdown HTTP server with 10s timeout to drain in-flight requests.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error(fmt.Sprintf("HTTP 服务器关闭超时: %v", err))
	} else {
		slog.Info("HTTP 服务器已优雅关闭")
	}

	// 停止内嵌的 sub-store 进程：不停掉它会占着 8298 端口，
	// 下次启动会因 EADDRINUSE 而崩溃重启循环。
	assets.StopSubStore()

	os.Exit(0)
}

// newHTTPServer builds an http.Server with hardened timeouts applied to both
// TCP and unix-socket listeners. ReadHeaderTimeout is deliberately set (rather
// than ReadTimeout) so that slow-loris style clients that trickle the header
// are rejected, while slow but legitimate bodies (large config uploads) are
// still allowed to finish.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// socketParent returns the parent directory of a socket path, or "" when the
// path has no directory component.
func socketParent(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return ""
}

// startBackgroundCleanup launches periodic cleanup goroutines for
// rate limiter buckets and CSRF tokens. These prevent memory leaks
// from stale entries accumulating over long-running sessions.
func startBackgroundCleanup(app *scapp.SubStoreApp) {
	// Rate limiter cleanup: start auto-cleanup on the handler app's rate limiter
	if rl := app.HandlerApp().RateLimiter; rl != nil {
		rl.StartAutoCleanup()
		defer rl.StopCleanup()
	}

	// CSRF token cleanup: every 5 minutes
	csrfTicker := time.NewTicker(5 * time.Minute)
	defer csrfTicker.Stop()

	for {
		<-csrfTicker.C
		middleware.CleanupCSRFTokens()
		// 1.4.11：过期 Session 此前只在 Get() 时被惰性删除，从未再次访问的
		// session 会永久驻留。复用同一 ticker 主动回收，避免长跑内存增长。
		if sessions := app.HandlerApp().Sessions; sessions != nil {
			sessions.Cleanup()
		}
	}
}

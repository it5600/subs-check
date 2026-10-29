// substore-ui-go is a unified Sub-Store management panel with integrated subs-check.
package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"substore-ui-go/internal/config"
	"substore-ui-go/internal/middleware"
	scapp "substore-ui-go/internal/subscheck/app"
	"github.com/gin-gonic/gin"
)

const version = "1.0.0"

func main() {
	cfg := config.Load()

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

	// Wrap the handler chain so every response carries the security headers
	// and every request is logged. Both were previously defined but never
	// attached to the router, leaving the configuration dead.
	handler = middleware.SecurityHeadersHandler(cfg.SecurityHeaders, handler)
	handler = middleware.LogRequest(handler)

	if cfg.SocketPath != "" {
		go serveUnixSocket(handler, cfg.SocketPath)
	} else {
		go serveTCP(handler, cfg)
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
//
// ReadHeaderTimeout is deliberately set (rather than ReadTimeout) so that
// slow-loris style clients that trickle the header are rejected, while slow
// but legitimate bodies (large config uploads) are still allowed to finish.
func serveTCP(handler http.Handler, cfg *config.Config) {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	slog.Info(fmt.Sprintf("HTTP服务器启动，监听 %s", addr))
	server := &http.Server{
		Addr:    addr,
		Handler: handler,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	if err := server.ListenAndServe(); err != nil {
		slog.Error(fmt.Sprintf("HTTP服务器启动失败: %v", err))
	}
}

// serveUnixSocket starts the HTTP server on a Unix domain socket.
//
// This is the transport the fnOS unified gateway uses for Native FPK
// applications: the gateway owns the public HTTPS endpoint and forwards
// requests to this socket, which means the application needs no dedicated
// TCP port and inherits the gateway's login/SSO handling.
//
// The socket file is removed first because a leftover file from a previous
// (crashed) run would make Listen fail with "address already in use".
func serveUnixSocket(handler http.Handler, socketPath string) {
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		slog.Warn(fmt.Sprintf("清理旧 socket 文件失败: %v", err))
	}

	// Create the parent directory when it does not exist yet.
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

	// The gateway connects as a different user than the one running the
	// application, so the socket has to be group/other accessible.
	if err := os.Chmod(socketPath, 0660); err != nil {
		slog.Warn(fmt.Sprintf("设置 socket 权限失败: %v", err))
	}

	slog.Info(fmt.Sprintf("Unix Socket 监听启动: %s", socketPath))

	server := &http.Server{Handler: handler}
	if err := server.Serve(listener); err != nil {
		slog.Error(fmt.Sprintf("Unix Socket 服务异常退出: %v", err))
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
	}
}

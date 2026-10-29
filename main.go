// substore-ui-go is a unified Sub-Store management panel with integrated subs-check.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"substore-ui-go/internal/config"
	scapp "substore-ui-go/internal/subscheck/app"
	"substore-ui-go/internal/middleware"
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

	// Start HTTP server
	go func() {
		addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
		slog.Info(fmt.Sprintf("HTTP服务器启动，监听 %s", addr))
		if err := router.Run(addr); err != nil {
			slog.Error(fmt.Sprintf("HTTP服务器启动失败: %v", err))
		}
	}()

	// Run subs-check main loop (triggerCheck, cron/ticker)
	mergedApp.Run()
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

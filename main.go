// substore-ui-go is a unified Sub-Store management panel with integrated subs-check.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"substore-ui-go/internal/config"
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

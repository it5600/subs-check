package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"substore-ui-go/internal/config"
	"substore-ui-go/internal/handler"
	"substore-ui-go/internal/middleware"
	"substore-ui-go/internal/service"
)

// Server holds the HTTP server and shared components
type Server struct {
	httpServer *http.Server
	app        *handler.App
	config     *config.Config
}

// New creates a new server instance
func New(cfg *config.Config) *Server {
	app := handler.NewApp(cfg)

	s := &Server{
		config: cfg,
		app:    app,
	}

	mux := http.NewServeMux()
	mux.Handle("/", middleware.LogRequest(http.HandlerFunc(s.handleRequest)))

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return s
}

// Start begins listening and serving requests
func (s *Server) Start() error {
	go s.cleanupLoop()

	log.Printf("[substore-ui-go] 服务已启动")
	log.Printf("  监听: %s:%d", s.config.Host, s.config.Port)
	log.Printf("  subs-check: http://%s:%d", s.config.SubsCheckHost, s.config.SubsCheckPort)
	log.Printf("  Sub-Store:  http://%s:%d%s", s.config.SubStoreHost, s.config.SubStorePort, s.config.SubStorePath)
	log.Printf("  配置路径: %s", s.config.ConfigPath)

	go s.gracefulShutdown()

	return s.httpServer.ListenAndServe()
}

// handleRequest is the main request handler
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	// 1. Security headers
	middleware.ApplySecurityHeaders(w, s.config.SecurityHeaders)

	// 2. CORS handling
	if !middleware.HandleCORS(s.config.CORSAllowedOrigins, w, r) {
		return
	}

	// 3. Rate limiting
	ip := extractIP(r)
	if !s.app.RateLimiter.Allow(ip) {
		s.app.Stats.IncRateLimited()
		handler.WriteJSON(w, http.StatusTooManyRequests, service.FormatError(429001, "请求过于频繁，请稍后再试"))
		return
	}

	// 4. Route to API or static
	pathname := r.URL.Path
	if strings.HasPrefix(pathname, "/api/") {
		s.app.HandleAPI(w, r, pathname)
		return
	}

	handler.ServeStatic(w, r, pathname, s.config.PublicDir)
}

// cleanupLoop periodically cleans expired sessions and rate limit buckets
func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.app.Sessions.Cleanup()
		s.app.RateLimiter.Cleanup()
		middleware.CleanupCSRFTokens()
	}
}

// gracefulShutdown handles SIGINT/SIGTERM with a 30s budget for in-flight requests to drain.
// Reference: Go patterns — "Make the zero value useful" + graceful shutdown budget 30s.
func (s *Server) gracefulShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("[substore-ui-go] 正在关闭...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		log.Printf("[substore-ui-go] 强制关闭: %v", err)
		_ = s.httpServer.Close()
	}
	log.Println("[substore-ui-go] 服务已退出")
}

// extractIP gets the client IP from the request, handling IPv6 addresses
func extractIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		return strings.TrimSpace(ips[0])
	}
	addr := r.RemoteAddr
	// Handle IPv6: "[::1]:8080" → strip brackets + port
	if strings.HasPrefix(addr, "[") {
		if end := strings.Index(addr, "]"); end > 0 {
			return addr[1:end]
		}
	}
	// Handle IPv4: "192.168.1.1:8080" → strip port
	if idx := strings.LastIndex(addr, ":"); idx > 0 {
		return addr[:idx]
	}
	return addr
}

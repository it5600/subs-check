// substore-ui-go is a lightweight Sub-Store management panel written in Go.
package main

import (
	"log"
	"net/http"

	"substore-ui-go/internal/config"
	"substore-ui-go/internal/server"
)

func main() {
	cfg := config.Load()

	srv := server.New(cfg)

	if err := srv.Start(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("服务器启动失败: %v", err)
	}
}

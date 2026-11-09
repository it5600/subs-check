package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"substore-ui-go/internal/config"
	"substore-ui-go/internal/handler"
	"substore-ui-go/internal/middleware"
)

// 参考 beck-8/subs-check app/router_test.go 的思路：把易回归的契约锁成测试。
// 本文件覆盖 1.4.11 新增/修正的三类契约：pprof 暴露边界、分页越界、401 响应体。

func TestEnablePprof_DefaultOff(t *testing.T) {
	t.Setenv("PPROF_ENABLED", "")
	if enablePprof(&config.Config{}) {
		t.Fatal("pprof 默认必须关闭")
	}
}

func TestEnablePprof_ExplicitlyOnInTCPMode(t *testing.T) {
	for _, v := range []string{"1", "true", "YES"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("PPROF_ENABLED", v)
			if !enablePprof(&config.Config{}) {
				t.Fatalf("PPROF_ENABLED=%s 且为 TCP 模式时应启用", v)
			}
		})
	}
}

// Unix socket 对主机上任意进程可连，注释里承诺「只在非网关监听时暴露」，
// 但旧实现完全没检查 SocketPath。
func TestEnablePprof_AlwaysDisabledOnGatewaySocket(t *testing.T) {
	t.Setenv("PPROF_ENABLED", "true")
	if enablePprof(&config.Config{SocketPath: "/tmp/trim-substore-ui-go.sock"}) {
		t.Fatal("网关 socket 模式下即使显式开启也不得注册 pprof")
	}
}

func TestClampPageRange_Boundaries(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	cases := []struct {
		name                                   string
		page, pageSize, total                  int
		wantPage, wantPages, wantOffset, wantEnd int
	}{
		{"常规首页", 1, 100, 250, 1, 3, 0, 100},
		{"常规末页", 3, 100, 250, 3, 3, 200, 250},
		{"页码超界", 99, 100, 250, 3, 3, 200, 250},
		{"页码溢出", maxInt, 100, 250, 3, 3, 200, 250},
		{"空结果集", 1, 100, 0, 1, 1, 0, 0},
		{"空结果集+超大页码", maxInt, 100, 0, 1, 1, 0, 0},
		{"pageSize 非法", 1, 0, 250, 1, 3, 0, 100},
		{"page 非法", -5, 100, 250, 1, 3, 0, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, pages, offset, end := clampPageRange(tc.page, tc.pageSize, tc.total)
			if page != tc.wantPage || pages != tc.wantPages || offset != tc.wantOffset || end != tc.wantEnd {
				t.Fatalf("clampPageRange = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
					page, pages, offset, end, tc.wantPage, tc.wantPages, tc.wantOffset, tc.wantEnd)
			}
			// 真实切片必须不 panic、不越界
			nodes := make([]int, tc.total)
			_ = nodes[offset:end]
		})
	}
}

// 401 响应体必须同时带 code(旧前端) 与 error(原生 admin.html / curl 脚本)。
func TestUnauthorizedBody_KeepsDualContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := &SubStoreApp{handlerApp: &handler.App{Sessions: middleware.NewSessionStore(60000)}}
	r := gin.New()
	r.GET("/probe", a.authMiddleware("test-secret"), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401, got %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("401 响应体必须是 JSON: %v", err)
	}
	if code, _ := body["code"].(float64); int(code) != 401001 {
		t.Fatalf("必须带 code=401001, got %v", body["code"])
	}
	if body["error"] != "无效的API密钥" {
		t.Fatalf("必须保留 error 字段, got %v", body["error"])
	}
	if body["message"] != "未授权" {
		t.Fatalf("必须保留 message 字段, got %v", body["message"])
	}
}

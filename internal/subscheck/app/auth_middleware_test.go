package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"substore-ui-go/internal/handler"
	"substore-ui-go/internal/middleware"
)

// 前端存在两套管理界面：
//   - subs-check 原生 admin.html：X-API-Key header
//   - 合并后的 www/app.js：Cookie Session（登录后由服务端下发）
//
// /api/dashboard、/api/config、/api/logs 等端点同时被这两套界面调用，
// 因此鉴权中间件必须接受两种凭据，且仍然拒绝匿名访问。
func newAuthProbe(t *testing.T, sessions *middleware.SessionStore) (*gin.Engine, *middleware.SessionStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	a := &SubStoreApp{handlerApp: &handler.App{Sessions: sessions}}
	r := gin.New()
	r.GET("/probe", a.authMiddleware("test-secret"), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r, sessions
}

func probe(t *testing.T, r *gin.Engine, mutate func(*http.Request)) int {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if mutate != nil {
		mutate(req)
	}
	r.ServeHTTP(w, req)
	return w.Code
}

func TestAuthMiddleware_RejectsAnonymous(t *testing.T) {
	r, _ := newAuthProbe(t, middleware.NewSessionStore(60000))
	if got := probe(t, r, nil); got != http.StatusUnauthorized {
		t.Fatalf("匿名访问应被拒绝(401), got %d", got)
	}
}

func TestAuthMiddleware_AcceptsAPIKey(t *testing.T) {
	r, _ := newAuthProbe(t, middleware.NewSessionStore(60000))
	got := probe(t, r, func(req *http.Request) {
		req.Header.Set("X-API-Key", "test-secret")
	})
	if got != http.StatusOK {
		t.Fatalf("X-API-Key 应被接受(200), got %d", got)
	}
}

func TestAuthMiddleware_AcceptsSessionCookie(t *testing.T) {
	store := middleware.NewSessionStore(60000)
	r, _ := newAuthProbe(t, store)
	sid := store.Create("test-secret") // 模拟 /api/login 成功后的会话
	got := probe(t, r, func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: "substore_sid", Value: sid})
	})
	if got != http.StatusOK {
		t.Fatalf("有效 Cookie Session 应被接受(200), got %d；合并前端无法访问 /api/dashboard", got)
	}
}

func TestAuthMiddleware_RejectsExpiredSession(t *testing.T) {
	store := middleware.NewSessionStore(-1) // 立即过期
	sid := store.Create("test-secret")
	r, _ := newAuthProbe(t, store)
	got := probe(t, r, func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: "substore_sid", Value: sid})
	})
	if got != http.StatusUnauthorized {
		t.Fatalf("过期 Session 应被拒绝(401), got %d", got)
	}
}

func TestAuthMiddleware_SessionCannotWrite(t *testing.T) {
	// 安全边界：Cookie 会被浏览器自动携带，若允许其执行写操作会引入 CSRF。
	// 写操作必须显式携带 X-API-Key。
	store := middleware.NewSessionStore(60000)
	sid := store.Create("test-secret")

	gin.SetMode(gin.TestMode)
	a := &SubStoreApp{handlerApp: &handler.App{Sessions: store}}
	r := gin.New()
	r.POST("/probe", a.authMiddleware("test-secret"), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/probe", nil)
	req.AddCookie(&http.Cookie{Name: "substore_sid", Value: sid})
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("Session 不应能执行写操作(期望 401), got %d", w.Code)
	}
}

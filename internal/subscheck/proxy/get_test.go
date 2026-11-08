package proxies

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"substore-ui-go/internal/subscheck/config"
)

func TestPermanentHTTPStatus(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{http.StatusOK, false},
		{http.StatusMovedPermanently, false},
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusNotFound, true}, // 运行日志里的失效订阅源
		{http.StatusGone, true},
		{http.StatusUnprocessableEntity, true},
		{http.StatusRequestTimeout, false},  // 408 可恢复
		{http.StatusTooManyRequests, false}, // 429 可恢复
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
		{http.StatusServiceUnavailable, false},
	}
	for _, tc := range cases {
		if got := permanentHTTPStatus(tc.code); got != tc.want {
			t.Errorf("permanentHTTPStatus(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

// setupSubFetchTest 隔离订阅拉取测试：还原全局配置、清空环境代理，
// 并让共享 transport 重新按测试环境初始化。
func setupSubFetchTest(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")

	saved := *config.GlobalConfig
	t.Cleanup(func() {
		*config.GlobalConfig = saved
		subTransportInit = sync.Once{}
		subTransport.Proxy = nil
	})

	config.GlobalConfig.SubUrlsReTry = 3
	config.GlobalConfig.SubUrlsRetryInterval = 0
	config.GlobalConfig.SubUrlsTimeout = 3
	config.GlobalConfig.SubUrlsGetUA = "subs-check-test"
	config.GlobalConfig.Proxy = ""
	config.GlobalConfig.DNS.Enable = false
	subTransportInit = sync.Once{}
	subTransport.Proxy = nil
}

func TestGetDateFromSubs_404NotRetried(t *testing.T) {
	setupSubFetchTest(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := GetDateFromSubs(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "永久失效") {
		t.Fatalf("404 应返回永久失效错误, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("404 不应重试, 期望 1 次请求, 实际 %d 次", got)
	}
}

func TestGetDateFromSubs_5xxStillRetried(t *testing.T) {
	setupSubFetchTest(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "upstream error", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := GetDateFromSubs(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "重试3次后失败") {
		t.Fatalf("502 应重试至上限, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("502 应重试 3 次, 实际 %d 次", got)
	}
}

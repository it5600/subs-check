package proxies

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"substore-ui-go/internal/subscheck/config"
	"substore-ui-go/internal/subscheck/utils"
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

// resetDeadSubs 隔离测试间的进程内黑名单状态。
func resetDeadSubs(t *testing.T) {
	t.Helper()
	deadSubs.mu.Lock()
	deadSubs.m = make(map[string]time.Time)
	deadSubs.mu.Unlock()
	t.Cleanup(func() {
		deadSubs.mu.Lock()
		deadSubs.m = make(map[string]time.Time)
		deadSubs.mu.Unlock()
	})
}

func TestGetDateFromSubs_404CarriesPermanentSentinel(t *testing.T) {
	setupSubFetchTest(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := GetDateFromSubs(srv.URL)
	if !errors.Is(err, errPermanentSub) {
		t.Fatalf("404 错误应携带永久失效哨兵, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "订阅源永久失效(不重试): 返回状态码: 404") {
		t.Fatalf("错误文本应保持兼容, got %v", err)
	}
}

func TestFailedSubCache_BlocksUntilTTLExpires(t *testing.T) {
	resetDeadSubs(t)
	saved := *config.GlobalConfig
	t.Cleanup(func() { *config.GlobalConfig = saved })

	config.GlobalConfig.SubUrlFailTTL = 60
	const raw = "https://example.com/dead.yaml"
	key := utils.WarpUrl(raw)

	if deadSubs.blocked(key) {
		t.Fatal("未登记的源不应被拦截")
	}
	if !deadSubs.mark(key) {
		t.Fatal("首次登记应返回 true")
	}
	if deadSubs.mark(key) {
		t.Fatal("重复登记应返回 false")
	}
	if !deadSubs.blocked(key) {
		t.Fatal("熔断期内应被拦截")
	}

	// 模拟 TTL 到期
	deadSubs.mu.Lock()
	deadSubs.m[key] = time.Now().Add(-time.Second)
	deadSubs.mu.Unlock()
	if deadSubs.blocked(key) {
		t.Fatal("TTL 到期后应自动恢复")
	}
	deadSubs.mu.Lock()
	_, stillThere := deadSubs.m[key]
	deadSubs.mu.Unlock()
	if stillThere {
		t.Fatal("过期条目应被顺带清理")
	}
}

func TestFailedSubCache_ZeroTTLDisables(t *testing.T) {
	resetDeadSubs(t)
	saved := *config.GlobalConfig
	t.Cleanup(func() { *config.GlobalConfig = saved })

	config.GlobalConfig.SubUrlFailTTL = 0
	key := utils.WarpUrl("https://example.com/dead.yaml")
	if deadSubs.mark(key) {
		t.Fatal("TTL=0 时不应登记")
	}
	if deadSubs.blocked(key) {
		t.Fatal("TTL=0 时不应拦截任何源")
	}
}

func TestResolveSubUrls_SkipsBlockedSource(t *testing.T) {
	setupSubFetchTest(t)
	resetDeadSubs(t)

	config.GlobalConfig.SubUrls = []string{
		"https://example.com/good.yaml",
		"https://example.com/dead.yaml",
	}
	config.GlobalConfig.SubUrlsRemote = nil
	config.GlobalConfig.SubUrlFailTTL = 60

	// 模拟上一轮拉取时该源返回 404
	deadSubs.mark(utils.WarpUrl("https://example.com/dead.yaml"))

	entries, _, _ := resolveSubUrls()
	if len(entries) != 1 || entries[0].url != "https://example.com/good.yaml" {
		t.Fatalf("熔断源应被跳过, got %+v", entries)
	}

	// TTL 到期后自动恢复，无需人工改配置
	deadSubs.mu.Lock()
	for k := range deadSubs.m {
		deadSubs.m[k] = time.Now().Add(-time.Second)
	}
	deadSubs.mu.Unlock()
	entries, _, _ = resolveSubUrls()
	if len(entries) != 2 {
		t.Fatalf("TTL 到期后应恢复全部订阅源, got %d", len(entries))
	}
}

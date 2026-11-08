package proxies

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	u "net/url"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/convert"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/samber/lo"
	"gopkg.in/yaml.v3"
	"substore-ui-go/internal/subscheck/config"
	"substore-ui-go/internal/subscheck/utils"
)

type subEntry struct {
	url    string
	source string
}

// errPermanentSub 标记订阅源返回了「重试也不会改变结果」的永久错误（4xx）。
// 用哨兵错误而非字符串匹配，调用方才能可靠地做熔断登记。
var errPermanentSub = errors.New("订阅源永久失效")

// failedSubCache 是进程内的失效订阅源临时黑名单。
// 远程订阅清单常由第三方维护，个别链接会长期 404；每轮重复请求既浪费
// 一次往返，也会持续刷 ERROR 日志。这里按 TTL 临时跳过，到期自动重试：
// 上游修好后无需改配置即可自愈，远程清单的动态更新能力也得以保留。
type failedSubCache struct {
	mu sync.Mutex
	m  map[string]time.Time // WarpUrl 规范化后的 URL -> 熔断到期时间
}

var deadSubs = failedSubCache{m: make(map[string]time.Time)}

// subFailTTL 返回熔断时长；<=0 表示禁用熔断。
func subFailTTL() time.Duration {
	minutes := config.GlobalConfig.SubUrlFailTTL
	if minutes <= 0 {
		return 0
	}
	return time.Duration(minutes) * time.Minute
}

// blocked 判断 URL 是否仍在熔断期，顺带清理已过期条目。
func (c *failedSubCache) blocked(url string) bool {
	if subFailTTL() <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.m[url]
	if !ok {
		return false
	}
	if time.Now().Before(exp) {
		return true
	}
	delete(c.m, url)
	return false
}

// mark 登记一个永久失效订阅源，返回是否为新登记（用于只打一次日志）。
func (c *failedSubCache) mark(url string) bool {
	ttl := subFailTTL()
	if ttl <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, existed := c.m[url]
	c.m[url] = time.Now().Add(ttl)
	return !existed
}

// ShardSpec 描述一次拉取的分片参数：每个订阅内每 Keep 个节点保留 1 个，从 Offset 开始。
type ShardSpec struct {
	Keep   int
	Offset int
}

// applyShard 按分片参数在单个订阅内取样。
// 节点数不超过分母的小订阅整体保留，避免某些轮次整份订阅被跳过。
func applyShard(list []map[string]any, shard ShardSpec) []map[string]any {
	if shard.Keep <= 1 || len(list) <= shard.Keep {
		return list
	}
	out := make([]map[string]any, 0, len(list)/shard.Keep+1)
	for i, p := range list {
		if i%shard.Keep == shard.Offset {
			out = append(out, p)
		}
	}
	return out
}

// ApplyShardForTest 导出的分片实现，供跨包单测覆盖取样逻辑。
func ApplyShardForTest(list []map[string]any, shard ShardSpec) []map[string]any {
	return applyShard(list, shard)
}

// GetProxies 拉取并解析全部订阅节点。
func GetProxies() ([]map[string]any, error) {
	return GetProxiesSharded(ShardSpec{})
}

// GetProxiesSharded 拉取订阅，并在每个订阅内部按分片参数取样后再合并。
// 分片下沉到订阅内部的目的：未选中的节点不会进入节点表，
// 加载阶段的内存峰值随之下降（实测此前加载期峰值 165MB+）。
func GetProxiesSharded(shard ShardSpec) ([]map[string]any, error) {

	// 解析本地与远程订阅清单
	subUrls, localNum, remoteNum := resolveSubUrls()
	slog.Info("订阅链接数量", "本地", localNum, "远程", remoteNum, "总计", len(subUrls))

	if len(config.GlobalConfig.NodeType) > 0 {
		slog.Info("只筛选用户设置的协议", "type", config.GlobalConfig.NodeType)
	}

	var wg sync.WaitGroup
	// Subscription-fetch concurrency is decoupled from the alive-check
	// concurrency: most users want dozens of cheap HTTP fetches in parallel
	// even when the alive-check pool is tuned large. Falls back to 20 if
	// the user hasn't set it (zero value).
	subFetchConcurrency := config.GlobalConfig.SubUrlsConcurrent
	if subFetchConcurrency <= 0 {
		subFetchConcurrency = 20
	}
	concurrentLimit := make(chan struct{}, subFetchConcurrency) // 限制并发数

	// 按订阅顺序预分配槽位,每个 goroutine 只写自己的下标,无竞争
	// 这样即便是并发获取,最终合并时仍能保持 subUrls 的顺序(本地在前,远程在后)
	buckets := make([][]map[string]any, len(subUrls))

	// 启动工作协程
	for idx, subUrl := range subUrls {
		wg.Add(1)
		concurrentLimit <- struct{}{} // 获取令牌

		go func(i int, e subEntry) {
			defer wg.Done()
			defer func() { <-concurrentLimit }() // 释放令牌

			url := e.url
			data, err := GetDateFromSubs(url)
			if err != nil {
				if errors.Is(err, errPermanentSub) {
					// 永久错误：登记熔断，之后 TTL 内由 resolveSubUrls 直接跳过。
					// 只在首次登记时打一条 WARN，避免每轮刷屏。
					if deadSubs.mark(url) {
						slog.Warn("订阅源永久失效，已加入临时熔断", "source", e.source,
							"url", url, "minutes", config.GlobalConfig.SubUrlFailTTL)
					}
				} else {
					slog.Error("获取订阅链接错误跳过", "source", e.source, "url", url, "err", err)
				}
				return
			}

			var tag string
			if d, err := u.Parse(url); err == nil {
				tag = d.Fragment
			}

			var local []map[string]any

			var con map[string]any
			err = yaml.Unmarshal(data, &con)
			if err != nil {
				proxyList, err := convert.ConvertsV2Ray(data)
				if err != nil {
					slog.Error("解析proxy错误", "source", e.source, "url", url, "err", err)
					return
				}
				slog.Debug("获取订阅链接", "source", e.source, "url", url, "count", len(proxyList))
				local = make([]map[string]any, 0, len(proxyList))
				for _, proxy := range proxyList {
					// 只测试指定协议
					if t, ok := proxy["type"].(string); ok {
						if len(config.GlobalConfig.NodeType) > 0 && !lo.Contains(config.GlobalConfig.NodeType, t) {
							continue
						}
					}

					// 为每个节点添加订阅链接来源信息和备注
					proxy["sub_url"] = url
					if tag != "" {
						proxy["sub_tag"] = tag
					}
					local = append(local, proxy)
				}
				buckets[i] = applyShard(local, shard)
				return
			}

			proxyInterface, ok := con["proxies"]
			if !ok || proxyInterface == nil {
				slog.Error("订阅链接没有proxies", "source", e.source, "url", url)
				return
			}

			proxyList, ok := proxyInterface.([]any)
			if !ok {
				return
			}
			slog.Debug("获取订阅链接", "source", e.source, "url", url, "count", len(proxyList))
			local = make([]map[string]any, 0, len(proxyList))
			for _, proxy := range proxyList {
				if proxyMap, ok := proxy.(map[string]any); ok {
					if t, ok := proxyMap["type"].(string); ok {
						// 只测试指定协议
						if len(config.GlobalConfig.NodeType) > 0 && !lo.Contains(config.GlobalConfig.NodeType, t) {
							continue
						}
						// 虽然支持mihomo支持下划线，但是这里为了规范，还是改成横杠
						// todo: 不知道后边还有没有这类问题
						switch t {
						case "hysteria2", "hy2":
							if _, ok := proxyMap["obfs_password"]; ok {
								proxyMap["obfs-password"] = proxyMap["obfs_password"]
								delete(proxyMap, "obfs_password")
							}
						}
					}
					// 为每个节点添加订阅链接来源信息和备注
					proxyMap["sub_url"] = url
					if tag != "" {
						proxyMap["sub_tag"] = tag
					}
					local = append(local, proxyMap)
				}
			}
			buckets[i] = applyShard(local, shard)
		}(idx, subEntry{url: utils.WarpUrl(subUrl.url), source: subUrl.source})
	}

	// 等待所有工作协程完成
	wg.Wait()

	// 按订阅顺序合并,保证本地订阅在前、远程订阅在后,订阅内节点顺序也保留
	total := 0
	for _, b := range buckets {
		total += len(b)
	}
	mihomoProxies := make([]map[string]any, 0, total)
	for _, b := range buckets {
		for _, proxy := range b {
			// 清理 mihomo 不认识的字段（如 client-fingerprint: unsafe），
			// 否则每轮都会刷 "wrong clientFingerprint" 警告，导出的 YAML 里
			// 也会带上这个非法值。
			sanitizeProxy(proxy)
			mihomoProxies = append(mihomoProxies, proxy)
		}
	}

	return mihomoProxies, nil
}

// from 3k
// resolveSubUrls 合并本地与远程订阅清单并去重
func resolveSubUrls() ([]subEntry, int, int) {
	// 计数
	var localNum, remoteNum int
	localNum = len(config.GlobalConfig.SubUrls)

	entries := make([]subEntry, 0, len(config.GlobalConfig.SubUrls))
	// 本地配置
	for _, u := range config.GlobalConfig.SubUrls {
		entries = append(entries, subEntry{url: u, source: "本地配置"})
	}

	// 远程清单
	if len(config.GlobalConfig.SubUrlsRemote) != 0 {
		for _, d := range config.GlobalConfig.SubUrlsRemote {
			if remote, err := fetchRemoteSubUrls(utils.WarpUrl(d)); err != nil {
				slog.Warn("获取远程订阅清单失败，已忽略", "url", d, "err", err)
			} else {
				remoteNum += len(remote)
				for _, u := range remote {
					entries = append(entries, subEntry{url: u, source: d})
				}
			}
		}
	}

	// 规范化、熔断过滤与去重
	seen := make(map[string]struct{}, len(entries))
	out := make([]subEntry, 0, len(entries))
	blocked := 0
	for _, e := range entries {
		s := strings.TrimSpace(e.url)
		if s == "" || strings.HasPrefix(s, "#") { // 跳过空行与注释
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		// key 与拉取协程里实际请求的 URL 保持一致（都经过 WarpUrl）
		if deadSubs.blocked(utils.WarpUrl(s)) {
			blocked++
			continue
		}
		seen[s] = struct{}{}
		out = append(out, subEntry{url: s, source: e.source})
	}
	if blocked > 0 {
		slog.Info("跳过熔断期内的订阅源", "count", blocked, "ttlMinutes", config.GlobalConfig.SubUrlFailTTL)
	}
	return out, localNum, remoteNum
}

// fetchRemoteSubUrls 从远程地址读取订阅URL清单
// 支持两种格式：
// 1) 纯文本，按换行分隔，支持以 # 开头的注释与空行
// 2) YAML/JSON 的字符串数组
func fetchRemoteSubUrls(listURL string) ([]string, error) {
	if listURL == "" {
		return nil, errors.New("empty list url")
	}
	data, err := GetDateFromSubs(listURL)
	if err != nil {
		return nil, err
	}

	// 优先尝试解析为字符串数组（YAML/JSON兼容）
	var arr []string
	if err := yaml.Unmarshal(data, &arr); err == nil && len(arr) > 0 {
		return arr, nil
	}

	// 回退为按行解析
	res := make([]string, 0, 16)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		res = append(res, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// subTransport is a shared http.Transport reused across all subscription
// fetches. All subscription URLs travel through the same network path, so
// sharing one connection pool eliminates redundant TCP+TLS handshakes per
// URL (previously each GetDateFromSubs call built its own Transport).
var subTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
	},
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// initSubTransport configures the shared transport once, based on the current
// config. Called lazily on first use to avoid referencing config before it is
// loaded. Proxy resolution order:
//  1. If Proxy is set → use it as HTTP proxy (typically Fluxor 7890)
//  2. Else fall back to environment proxy (http.ProxyFromEnvironment)
//  3. DNS.Enable still configures DialContext for mihomo DNS resolution
var subTransportInit sync.Once

func initSubTransport(timeoutSec int) {
	subTransportInit.Do(func() {
		// Configure proxy: prefer explicit Proxy config, then env.
		if cfg := config.GlobalConfig; cfg.Proxy != "" {
			proxyURL, err := u.Parse(cfg.Proxy)
			if err == nil {
				subTransport.Proxy = http.ProxyURL(proxyURL)
				slog.Info("订阅拉取代理已启用", "proxy", cfg.Proxy)
			} else {
				slog.Warn("Proxy 配置无效，回退到环境代理", "proxy", cfg.Proxy, "err", err)
			}
		} else {
			subTransport.Proxy = http.ProxyFromEnvironment
		}

		// DNS routing via mihomo if enabled
		if config.GlobalConfig.DNS.Enable {
			subTransport.DialContext = newMihomoDialer(time.Duration(timeoutSec) * time.Second)
		}
	})
}

// permanentHTTPStatus 判断某个 HTTP 状态码是否属于「重试也不会改变结果」。
// 4xx 中 408（请求超时）与 429（限流）是暂时性的，仍可重试；
// 5xx、网络错误、3xx（此处非 200 一律当失败）也不在此列，交给调用方重试。
func permanentHTTPStatus(code int) bool {
	if code < 400 || code >= 500 {
		return false
	}
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	default:
		return true
	}
}

// 订阅链接中获取数据
func GetDateFromSubs(subUrl string) ([]byte, error) {
	maxRetries := config.GlobalConfig.SubUrlsReTry
	// 重试间隔
	retryInterval := config.GlobalConfig.SubUrlsRetryInterval
	if retryInterval == 0 {
		retryInterval = 1
	}
	// 超时时间
	timeout := config.GlobalConfig.SubUrlsTimeout
	if timeout == 0 {
		timeout = 10
	}
	var lastErr error

	// Reuse the shared transport (connection pooling across all subscriptions).
	initSubTransport(timeout)
	client := &http.Client{
		Timeout:   time.Duration(timeout) * time.Second,
		Transport: subTransport,
	}

	for i := range maxRetries {
		if i > 0 {
			time.Sleep(time.Duration(retryInterval) * time.Second)
		}

		req, err := http.NewRequest("GET", subUrl, nil)
		if err != nil {
			lastErr = err
			continue
		}

		if config.GlobalConfig.SubUrlsGetUA == "random" {
			req.Header.Set("User-Agent", convert.RandUserAgent())
		} else {
			req.Header.Set("User-Agent", config.GlobalConfig.SubUrlsGetUA)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("返回状态码: %d", resp.StatusCode)
			// 永久性错误（404/410/403 等）重试多少次都不会变；
			// 运行日志里每轮都有失效订阅源重试 3 次后才报 404，
			// 白白多 2 次请求 + 2 秒退避。这类错误立即返回。
			if permanentHTTPStatus(resp.StatusCode) {
				// 保留原有错误文本，同时挂上哨兵供调用方识别并登记熔断
				return nil, fmt.Errorf("%w(不重试): %w", errPermanentSub, lastErr)
			}
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("读取响应数据错误: %w", err)
			continue
		}
		return body, nil
	}

	return nil, fmt.Errorf("重试%d次后失败: %w", maxRetries, lastErr)
}

// newMihomoDialer returns a DialContext that resolves via mihomo's global resolver
// (DoH when configured) then dials the resulting IP directly, avoiding the OS DNS path.
// dialTimeout is shared with the request-level timeout from SubUrlsTimeout.
func newMihomoDialer(dialTimeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: dialTimeout}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		// If addr is already an IP, skip resolution.
		if ip := net.ParseIP(host); ip != nil {
			return d.DialContext(ctx, network, addr)
		}
		ips, err := resolver.LookupIP(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no ip for %s", host)
		}
		// Try each IP in turn, returning on the first successful connection.
		var dialErr error
		for _, ip := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			dialErr = err
		}
		return nil, fmt.Errorf("dial %s: %w", host, dialErr)
	}
}

// Package fluxor integrates subs-check with the Fluxor/mihomo controller API.
//
// It provides read-only observability (providers, connections, memory, traffic)
// and write operations (provider hot-reload, delay test). The client talks to
// the Fluxor external-controller port (default 9091, bridged via socat to the
// mihomo unix socket /var/apps/Fluxor/target/core.sock).
package fluxor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"substore-ui-go/internal/subscheck/config"
)

// Client is a thin HTTP client for the mihomo controller API.
type Client struct {
	base    string
	secret  string
	http    *http.Client
	enabled bool
}

// Enabled reports whether Fluxor integration is configured.
func Enabled() bool {
	return config.GlobalConfig.FluxorApiUrl != ""
}

// Default returns the process-wide client, initialised from GlobalConfig.
func Default() *Client {
	once.Do(func() {
		base := strings.TrimRight(config.GlobalConfig.FluxorApiUrl, "/")
		defaultClient = &Client{
			base:   base,
			secret: config.GlobalConfig.FluxorApiSecret,
			http:   &http.Client{Timeout: 10 * time.Second},
			enabled: base != "",
		}
	})
	return defaultClient
}

var (
	once        sync.Once
	defaultClient *Client
)

// Reconfigure resets the cached client (e.g. after config change).
func Reconfigure() {
	once = sync.Once{}
	defaultClient = nil
}

// do performs an authenticated request and returns the response body.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	if !c.enabled {
		return nil, fmt.Errorf("Fluxor API 未配置")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Fluxor API 失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Fluxor API 返回 HTTP %d: %s", resp.StatusCode, string(data[:min(len(data), 200)]))
	}
	return data, nil
}

// Health returns a summary of Fluxor controller state.
type Health struct {
	Version   string `json:"version"`
	Uptime    int64  `json:"uptime"`
	Providers int    `json:"providers"`
	Nodes     int    `json:"nodes"`
	Memory    int64  `json:"memory"` // bytes in use
}

// GetHealth fetches version, provider count, node count, memory in parallel.
// mihomo's /memory endpoint can take seconds, so the four endpoints are queried
// concurrently; total latency is bounded by the slowest one rather than the sum.
func GetHealth(ctx context.Context) (*Health, error) {
	c := Default()
	if !c.enabled {
		return &Health{}, nil
	}
	h := &Health{}
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if data, err := c.do(ctx, "GET", "/version", nil); err == nil {
			var v struct{ Version string `json:"version"` }
			if json.Unmarshal(data, &v) == nil {
				h.Version = v.Version
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if data, err := c.do(ctx, "GET", "/providers/proxies", nil); err == nil {
			// Fluxor/mihomo returns each provider's proxies as an array;
			// count them directly. The "now" field is the selected proxy
			// index, not the count, so it must not be summed.
			var p struct {
				Providers map[string]struct {
					Proxies []json.RawMessage `json:"proxies"`
				} `json:"providers"`
			}
			if json.Unmarshal(data, &p) == nil {
				h.Providers = len(p.Providers)
				for _, prov := range p.Providers {
					h.Nodes += len(prov.Proxies)
				}
			}
		}
	}()

	// /memory is intentionally omitted from the health snapshot: mihomo's
	// /memory endpoint takes ~8s and streams multiple JSON objects (one per
	// line), which both exceeds the request timeout and fails json.Unmarshal.
	// Memory info is available via the dedicated /api/fluxor/memory endpoint.

	wg.Wait()
	return h, nil
}

// GetMemory fetches the current memory usage (bytes in use) from /memory.
// mihomo's /memory streams multiple JSON objects; we read the first line only.
func GetMemory(ctx context.Context) (int64, error) {
	c := Default()
	if !c.enabled {
		return 0, fmt.Errorf("Fluxor API 未配置")
	}
	// Use a short timeout — /memory can be slow.
	memCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(memCtx, "GET", c.base+"/memory", nil)
	if err != nil {
		return 0, err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("请求 /memory 失败: %w", err)
	}
	defer resp.Body.Close()

	// /memory streams N lines of JSON; read only the first line.
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return 0, fmt.Errorf("读取 /memory 失败: %w", err)
	}
	var mem struct {
		InUse int64 `json:"inuse"`
	}
	if json.Unmarshal(line, &mem) != nil {
		return 0, fmt.Errorf("解析 /memory 失败")
	}
	return mem.InUse, nil
}

// Connections returns active connections.
type Connection struct {
	Metadata struct {
		Network string `json:"network"`
		Source  struct {
			IP   string `json:"ip"`
			Port int    `json:"port"`
		} `json:"source"`
		Destination struct {
			IP   string `json:"ip"`
			Port int    `json:"port"`
		} `json:"destination"`
		Rule  string `json:"rule"`
		RulePayload string `json:"rulePayload"`
		Chains []string `json:"chains"`
	} `json:"metadata"`
}

// GetConnections returns active TCP/UDP connections.
func GetConnections(ctx context.Context) ([]Connection, error) {
	c := Default()
	if !c.enabled {
		return nil, nil
	}
	data, err := c.do(ctx, "GET", "/connections", nil)
	if err != nil {
		return nil, err
	}
	var resp struct{ Connections []Connection `json:"connections"` }
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Connections, nil
}

// ProviderInfo describes a proxy provider.
type ProviderInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	VehicleType string `json:"vehicleType"`
	NodeCount   int    `json:"nodeCount"`
	Now         int    `json:"now"`
}

// GetProviders lists all proxy providers with their node counts.
func GetProviders(ctx context.Context) ([]ProviderInfo, error) {
	c := Default()
	if !c.enabled {
		return nil, nil
	}
	data, err := c.do(ctx, "GET", "/providers/proxies", nil)
	if err != nil {
		return nil, err
	}
	// Fluxor/mihomo returns each provider's proxies as an array;
	// count them directly rather than looking for a "proxyCount" field.
	var resp struct {
		Providers map[string]struct {
			Type        string              `json:"type"`
			VehicleType string              `json:"vehicleType"`
			Proxies     []json.RawMessage   `json:"proxies"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]ProviderInfo, 0, len(resp.Providers))
	for name, p := range resp.Providers {
		out = append(out, ProviderInfo{
			Name:        name,
			Type:        p.Type,
			VehicleType: p.VehicleType,
			NodeCount:   len(p.Proxies),
			Now:         0,
		})
	}
	return out, nil
}

// ===== Write operations =====

// ReloadProvider triggers a hot-reload of a single provider without restarting
// the mihomo process. This is the zero-disruption node update path.
func ReloadProvider(ctx context.Context, name string) error {
	c := Default()
	if !c.enabled {
		return fmt.Errorf("Fluxor API 未配置")
	}
	_, err := c.do(ctx, "PUT", "/providers/proxies/"+name, nil)
	return err
}

// DelayTest measures the delay of a proxy through Fluxor's own probe.
func DelayTest(ctx context.Context, proxy, url string, timeoutMs int) (int, error) {
	c := Default()
	if !c.enabled {
		return 0, fmt.Errorf("Fluxor API 未配置")
	}
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	// proxy / url 直接来自 /api/fluxor/delay 的请求体。未转义时 "?"/"&"/"#"
	// 可注入额外查询参数，"/" 甚至能把路径指向 controller 的其它端点。
	path := fmt.Sprintf("/proxies/%s/delay?timeout=%d&url=%s",
		neturl.PathEscape(proxy), timeoutMs, neturl.QueryEscape(url))
	data, err := c.do(ctx, "GET", path, nil)
	if err != nil {
		return 0, err
	}
	var resp struct{ Delay int `json:"delay"` }
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Delay, nil
}

// AutoReloadFluxor is called after each check round to push new nodes to
// Fluxor via provider hot-reload. It is safe to call with no config.
func AutoReloadFluxor() {
	if !config.GlobalConfig.FluxorAutoReload {
		return
	}
	if config.GlobalConfig.FluxorProviderName == "" {
		return
	}
	name := config.GlobalConfig.FluxorProviderName
	// The provider URL is served from output/mihomo.yaml. mihomo may re-fetch
	// that file at any moment (provider interval), and a reload that races a
	// write used to answer HTTP 503 "convert v2ray subscribe error". Writes are
	// now atomic, and the reload itself is retried so one transient 5xx does not
	// leave the group on a stale node list.
	const attempts = 3
	var lastErr error
	for i := 1; i <= attempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := ReloadProvider(ctx, name)
		cancel()
		if err == nil {
			if i > 1 {
				slog.Info(fmt.Sprintf("Fluxor provider %s 已热重载（第 %d 次尝试）", name, i))
			} else {
				slog.Info(fmt.Sprintf("Fluxor provider %s 已热重载", name))
			}
			return
		}
		lastErr = err
		if i < attempts {
			slog.Warn(fmt.Sprintf("Fluxor provider %s 第 %d 次热重载失败，%ds 后重试: %v", name, i, i*2, err))
			time.Sleep(time.Duration(i*2) * time.Second)
		}
	}
	slog.Warn(fmt.Sprintf("Fluxor provider %s 热重载失败（已重试 %d 次）: %v", name, attempts, lastErr))
}

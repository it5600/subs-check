package check

import (
	"fmt"
	"log/slog"
	"strings"
)

// infoNodeKeywords marks subscription info entries that are not real proxies:
// remaining-traffic / expiry / official-site / address lines that some
// providers inject into the proxies list. An item whose name contains any of
// these is skipped only when it also lacks a real server+type, so a legit
// node that merely mentions a keyword in its display name survives.
var infoNodeKeywords = []string{
	"剩余流量", "剩余", "套餐到期", "到期", "过期", "流量信息",
	"官网", "地址", "订阅地址", "使用文档", "帮助", "说明", "公告",
	"复制", "二维码", "公众号", "TG群", "电报群", "飞机群",
	"节点订阅", "订阅链接", "节点信息", "使用时长", "有效期",
	"总流量", "已用", "更新于", "服务器信息", "温馨提示", "注意",
	"扫码", "联系", "客服", "条款", "隐私", "免责声明",
	"remaining", "expiry", "expire", "expires", "traffic",
	"official", "address", "subscribe", "update", "notice",
	"telegram", "channel", "group", "contact", "terms", "privacy",
}

// validProxyTypes is the set of proxy protocols a real node can use.
var validProxyTypes = map[string]bool{
	"ss": true, "ssr": true, "vmess": true, "vless": true, "trojan": true,
	"hysteria": true, "hysteria2": true, "hy2": true, "snell": true,
	"tuic": true, "wireguard": true, "http": true, "socks": true,
	"socks5": true, "ssh": true, "anytlnt": true, "guanghong": true,
}

// filterInfoNodes removes non-proxy info entries from the proxies slice.
//
// A node is kept when it has a real server AND a known proxy type. When
// either is missing the name is checked against the info-keyword list;
// matching entries are dropped. Unknown-but-not-info entries are kept to avoid
// false positives (e.g. a custom protocol we don't recognise).
func filterInfoNodes(proxies []map[string]any) []map[string]any {
	dropped := 0
	out := make([]map[string]any, 0, len(proxies))
	for _, p := range proxies {
		if p == nil {
			continue
		}
		server, _ := p["server"].(string)
		typ, _ := p["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))

		// Real proxy: has server + known type → always keep.
		if server != "" && validProxyTypes[typ] {
			out = append(out, p)
			continue
		}
		// Missing both server and type, or unknown type with no server:
		// treat as an info entry candidate.
		name, _ := p["name"].(string)
		if server == "" && isInfoNodeName(name) {
			dropped++
			slog.Debug("跳过订阅信息条目", "name", name, "type", typ)
			continue
		}
		// Unknown type but has a server — keep (custom protocol / future type).
		if server != "" {
			out = append(out, p)
			continue
		}
		// No server at all and not clearly an info entry: drop as malformed
		// (DeduplicateProxies already drops empty-server entries; this is a
		// second safety net for entries that survived with a name but no server).
		dropped++
		slog.Debug("跳过无服务器字段的条目", "name", name, "type", typ)
	}
	if dropped > 0 {
		slog.Info(fmt.Sprintf("过滤订阅信息条目 %d 个", dropped))
	}
	return out
}

// isInfoNodeName reports whether name matches an info keyword.
func isInfoNodeName(name string) bool {
	lower := strings.ToLower(name)
	for _, kw := range infoNodeKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

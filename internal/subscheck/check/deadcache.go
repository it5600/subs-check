package check

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"
)

// 失效节点冷却
//
// 实测（2026-10-31 分片轮）每轮约 60% 的节点在测活阶段超时（黑洞），
// 每个黑洞节点都会占满 alive-timeout（3s）才返回，是整轮耗时的主要来源：
// 测活阶段 139s 中约 120s 花在超时的黑洞节点上。
// 这里用节点指纹记录最近一次测活超时的节点，TTL 内不再重复测活；
// 节点一旦测活成功立即解除记录，保证恢复的节点能被重新发现。

const deadNodeDefaultTTL = 20 * time.Minute
const deadNodeMaxEntries = 50000

type deadNodeStore struct {
	mu  sync.Mutex
	m   map[string]time.Time
	ttl time.Duration
}

var deadNodes = &deadNodeStore{m: make(map[string]time.Time)}

// deadNodeKey 用节点完整配置的哈希作为指纹，避免同名节点互相影响。
func deadNodeKey(proxy map[string]any) string {
	if proxy == nil {
		return ""
	}
	b, err := json.Marshal(proxy)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return string(sum[:16])
}

// Mark 记录一次测活超时。
func (dead *deadNodeStore) Mark(proxy map[string]any) {
	k := deadNodeKey(proxy)
	if k == "" {
		return
	}
	dead.mu.Lock()
	defer dead.mu.Unlock()
	if len(dead.m) >= deadNodeMaxEntries {
		dead.pruneLocked()
	}
	dead.m[k] = time.Now()
}

// Revive 节点测活成功后立即解除记录。
func (dead *deadNodeStore) Revive(proxy map[string]any) {
	k := deadNodeKey(proxy)
	if k == "" {
		return
	}
	dead.mu.Lock()
	delete(dead.m, k)
	dead.mu.Unlock()
}

// SetTTL 设置冷却时长。
func (dead *deadNodeStore) SetTTL(d time.Duration) {
	if d <= 0 {
		d = deadNodeDefaultTTL
	}
	dead.mu.Lock()
	dead.ttl = d
	dead.mu.Unlock()
}

// TTL 返回当前冷却时长。
func (dead *deadNodeStore) TTL() time.Duration {
	dead.mu.Lock()
	defer dead.mu.Unlock()
	if dead.ttl <= 0 {
		return deadNodeDefaultTTL
	}
	return dead.ttl
}

// pruneLocked 清理过期条目，调用方必须已持有锁。
func (dead *deadNodeStore) pruneLocked() {
	now := time.Now()
	ttl := dead.ttl
	if ttl <= 0 {
		ttl = deadNodeDefaultTTL
	}
	for k, t := range dead.m {
		if now.Sub(t) > ttl {
			delete(dead.m, k)
		}
	}
}

// Filter 过滤掉冷却期内的失效节点，返回保留的节点与被跳过的数量。
func (dead *deadNodeStore) Filter(list []map[string]any) ([]map[string]any, int) {
	dead.mu.Lock()
	defer dead.mu.Unlock()
	dead.pruneLocked()
	if len(dead.m) == 0 {
		return list, 0
	}
	kept := make([]map[string]any, 0, len(list))
	skipped := 0
	for _, p := range list {
		if _, ok := dead.m[deadNodeKey(p)]; ok {
			skipped++
			continue
		}
		kept = append(kept, p)
	}
	return kept, skipped
}

// deadNodeTTLOf 推算失效节点冷却时长：显式配置（分钟）优先，
// 否则按检测间隔的 5 倍自动推算，保证覆盖分片轮转周期。
func deadNodeTTLOf(configMinutes, checkIntervalMinutes int) time.Duration {
	if configMinutes > 0 {
		d := time.Duration(configMinutes) * time.Minute
		if d > 24*time.Hour {
			d = 24 * time.Hour
		}
		return d
	}
	if checkIntervalMinutes <= 0 {
		return deadNodeDefaultTTL
	}
	d := time.Duration(checkIntervalMinutes*5) * time.Minute
	if d < 30*time.Minute {
		d = 30 * time.Minute
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	return d
}

// Len 返回当前缓存的失效节点数。
func (dead *deadNodeStore) Len() int {
	dead.mu.Lock()
	defer dead.mu.Unlock()
	return len(dead.m)
}

package check

import (
	"testing"
	"time"
)

func TestDeadNodeCache_MarkFilterRevive(t *testing.T) {
	store := &deadNodeStore{m: make(map[string]time.Time)}
	dead := map[string]any{"name": "a", "server": "1.2.3.4", "port": 443}
	alive := map[string]any{"name": "b", "server": "5.6.7.8", "port": 443}

	store.Mark(dead)
	list := []map[string]any{dead, alive}
	kept, skipped := store.Filter(list)
	if skipped != 1 || len(kept) != 1 {
		t.Fatalf("应跳过 1 个失效节点, got skipped=%d kept=%d", skipped, len(kept))
	}
	if deadNodeKey(kept[0]) != deadNodeKey(alive) {
		t.Fatalf("保留的应是未失效节点")
	}
	if store.Len() != 1 {
		t.Fatalf("缓存数量应为 1, got %d", store.Len())
	}
	store.Revive(dead)
	kept, skipped = store.Filter(list)
	if skipped != 0 || len(kept) != 2 {
		t.Fatalf("解除后不应跳过节点, got skipped=%d kept=%d", skipped, len(kept))
	}
}

func TestDeadNodeCache_Expire(t *testing.T) {
	store := &deadNodeStore{m: make(map[string]time.Time)}
	dead := map[string]any{"name": "a"}
	store.Mark(dead)
	store.m[deadNodeKey(dead)] = time.Now().Add(-deadNodeDefaultTTL - time.Minute)
	kept, skipped := store.Filter([]map[string]any{dead})
	if skipped != 0 || len(kept) != 1 {
		t.Fatalf("过期条目应被清理, got skipped=%d kept=%d", skipped, len(kept))
	}
}

func TestDeadNodeTTLOf(t *testing.T) {
	if got := deadNodeTTLOf(90, 120); got != 90*time.Minute {
		t.Fatalf("显式配置应生效, got %s", got)
	}
	if got := deadNodeTTLOf(0, 120); got != 600*time.Minute {
		t.Fatalf("自动推算应为间隔 5 倍, got %s", got)
	}
	if got := deadNodeTTLOf(0, 1); got != 30*time.Minute {
		t.Fatalf("自动推算下限 30 分钟, got %s", got)
	}
	if got := deadNodeTTLOf(0, 0); got != deadNodeDefaultTTL {
		t.Fatalf("无间隔应回退默认值, got %s", got)
	}
}

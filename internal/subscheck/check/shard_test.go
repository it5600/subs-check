package check

import (
	"testing"

	"substore-ui-go/internal/subscheck/config"
	proxies "substore-ui-go/internal/subscheck/proxy"
)

// TestPlanCheckShare_Disabled 关闭分片时应返回空分片参数
func TestPlanCheckShare_Disabled(t *testing.T) {
	config.GlobalConfig.CheckShare = 0
	config.GlobalConfig.FullCheckEvery = 0
	shard, note := planCheckShare()
	if shard.Keep != 0 {
		t.Fatalf("关闭分片时 Keep 应为 0, got %d", shard.Keep)
	}
	if note != "" {
		t.Fatalf("关闭分片时不应有说明, got %q", note)
	}
}

// TestPlanCheckShare_RotateAndFull 验证分片轮转与每 N 轮全量
func TestPlanCheckShare_RotateAndFull(t *testing.T) {
	config.GlobalConfig.CheckShare = 0.25
	// 每 5 轮一次全量 => 中间 4 个分片轮刚好覆盖全部 4 个分片
	config.GlobalConfig.FullCheckEvery = 5
	defer func() { roundSeq.Store(0); shardSeq.Store(0) }()
	roundSeq.Store(0)
	shardSeq.Store(0)
	offsets := map[int]bool{}
	for seq := 1; seq <= 5; seq++ {
		shard, note := planCheckShare()
		if seq%5 == 0 {
			if shard.Keep != 0 {
				t.Fatalf("第 %d 轮应为全量, got Keep=%d", seq, shard.Keep)
			}
			continue
		}
		if shard.Keep != 4 {
			t.Fatalf("第 %d 轮分母应为 4, got %d", seq, shard.Keep)
		}
		if note == "" {
			t.Fatalf("第 %d 轮应有分片说明", seq)
		}
		offsets[shard.Offset] = true
	}
	if len(offsets) != 4 {
		t.Fatalf("4 个分片轮应覆盖 4 个不同偏移, got %d", len(offsets))
	}
}

// TestApplyShard 验证订阅内分片的取样与全覆盖
func TestApplyShard(t *testing.T) {
	nodes := make([]map[string]any, 0, 40)
	for i := 0; i < 40; i++ {
		nodes = append(nodes, map[string]any{"i": i})
	}
	seen := map[int]bool{}
	for offset := 0; offset < 4; offset++ {
		out := proxies.ApplyShardForTest(nodes, proxies.ShardSpec{Keep: 4, Offset: offset})
		if len(out) != 10 {
			t.Fatalf("偏移 %d 应保留 10 个, got %d", offset, len(out))
		}
		for _, p := range out {
			seen[p["i"].(int)] = true
		}
	}
	if len(seen) != 40 {
		t.Fatalf("4 个偏移应覆盖全部节点, got %d/40", len(seen))
	}
	// 小订阅不参与分片，整体保留
	small := nodes[:3]
	if len(proxies.ApplyShardForTest(small, proxies.ShardSpec{Keep: 4, Offset: 1})) != 3 {
		t.Fatalf("小订阅应整体保留")
	}
	// 未启用分片时原样返回
	if len(proxies.ApplyShardForTest(nodes, proxies.ShardSpec{})) != 40 {
		t.Fatalf("未启用分片时应原样返回")
	}
}

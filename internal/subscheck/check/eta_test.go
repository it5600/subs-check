package check

import (
	"testing"
	"time"

	"substore-ui-go/internal/subscheck/config"
)

// saveETAState 保存/恢复 ETA 用到的包级全局状态，防止测试间互相污染。
// 1.4.11 起检测时间改为 atomic.Int64 存储，这里直接存取包内原子变量。
func saveETAState(t *testing.T) {
	t.Helper()
	oldTotal, oldProgress, oldAvailable := ProxyCount.Load(), Progress.Load(), Available.Load()
	oldCollector := CollectorPassed.Load()
	oldStart, oldEnd, oldDur := checkStartNano.Load(), checkEndNano.Load(), checkDurationNano.Load()
	oldETA := ETASeconds.Load()
	oldLimit := config.GlobalConfig.SuccessLimit
	t.Cleanup(func() {
		ProxyCount.Store(oldTotal)
		Progress.Store(oldProgress)
		Available.Store(oldAvailable)
		CollectorPassed.Store(oldCollector)
		checkStartNano.Store(oldStart)
		checkEndNano.Store(oldEnd)
		checkDurationNano.Store(oldDur)
		ETASeconds.Store(oldETA)
		config.GlobalConfig.SuccessLimit = oldLimit
		SetHistoricalRate(0)
		ResetETA()
	})
	ResetETA()
	SetHistoricalRate(0)
}

func TestUpdateETA_IdleAndComplete(t *testing.T) {
	saveETAState(t)
	config.GlobalConfig.SuccessLimit = 0
	SetCheckStart(time.Now().Add(-10 * time.Second))

	// 空闲：总数为 0
	ProxyCount.Store(0)
	Progress.Store(0)
	UpdateETA()
	if got := ETASeconds.Load(); got != 0 {
		t.Fatalf("空闲状态应返回 0, got %d", got)
	}

	// 完成：进度已到总数
	ProxyCount.Store(100)
	Progress.Store(100)
	UpdateETA()
	if got := ETASeconds.Load(); got != 0 {
		t.Fatalf("完成状态应返回 0, got %d", got)
	}
}

func TestUpdateETA_Calculating(t *testing.T) {
	saveETAState(t)
	config.GlobalConfig.SuccessLimit = 0
	ProxyCount.Store(100)
	Progress.Store(10)
	SetCheckStart(time.Now()) // elapsed < 3s → 计算中
	UpdateETA()
	if got := ETASeconds.Load(); got != -1 {
		t.Fatalf("启动不足 3 秒应返回 -1(计算中), got %d", got)
	}
}

func TestUpdateETA_SuccessLimitProjection(t *testing.T) {
	saveETAState(t)
	// 每 2 个总节点产出 1 个最终通过节点；还差 40 个 → 折算剩余 80 个总节点。
	// 实时速率 = 20 节点 / 10 秒 = 2 节点/秒 → ETA ≈ 40 秒。
	config.GlobalConfig.SuccessLimit = 50
	ProxyCount.Store(100)
	Progress.Store(20)
	CollectorPassed.Store(10)
	SetCheckStart(time.Now().Add(-10 * time.Second))

	UpdateETA()
	got := ETASeconds.Load()
	if got < 38 || got > 42 {
		t.Fatalf("SuccessLimit 折算 ETA 应约等于 40 秒, got %d", got)
	}
}

// 实机回归：2026-11-07 21:25 的日志显示 available=245 但检测仍在跑，
// ETA 却已被判为 0（误把测活通过数当作 SuccessLimit 的触发计数）。
func TestUpdateETA_UsesCollectorPassedNotAvailable(t *testing.T) {
	saveETAState(t)
	config.GlobalConfig.SuccessLimit = 100
	ProxyCount.Store(6300)
	Progress.Store(4982)
	Available.Store(245)      // 测活通过数，远超 SuccessLimit
	CollectorPassed.Store(42) // collector 最终通过数，尚未达标
	SetCheckStart(time.Now().Add(-201 * time.Second))

	UpdateETA()
	if got := ETASeconds.Load(); got <= 0 {
		t.Fatalf("测活通过数超过上限但 collector 未达标时 ETA 不应归零, got %d", got)
	}
}

// 实机回归：SuccessLimit=100 但整轮测速只通过 61 个时，折算出的剩余量
// 会远超实际剩余节点数；ETA 必须钳制到物理剩余量，否则高估数倍。
func TestUpdateETA_ClampsToRemainingNodes(t *testing.T) {
	saveETAState(t)
	config.GlobalConfig.SuccessLimit = 100
	ProxyCount.Store(1000)
	Progress.Store(900)      // 物理剩余 100 个节点
	CollectorPassed.Store(1) // 折算值 (100-1)*900 = 89100，明显不合理
	SetCheckStart(time.Now().Add(-100 * time.Second))

	UpdateETA()
	// 速率兜底 = 900/100 = 9 节点/秒 → 钳制后 ETA ≈ ceil(100/9) = 12 秒
	got := ETASeconds.Load()
	if got < 10 || got > 15 {
		t.Fatalf("ETA 应钳制到剩余节点对应的时长(约 12 秒), got %d", got)
	}
}

func TestUpdateETA_HistoricalRateColdStart(t *testing.T) {
	saveETAState(t)
	config.GlobalConfig.SuccessLimit = 0
	SetHistoricalRate(5) // 上一轮 5 节点/秒
	ProxyCount.Store(100)
	Progress.Store(0) // 冷启动：还没有实时速率
	SetCheckStart(time.Now().Add(-10 * time.Second))

	UpdateETA()
	got := ETASeconds.Load()
	if got < 19 || got > 21 {
		t.Fatalf("冷启动应使用历史速率给出约 20 秒, got %d", got)
	}
}

func TestSnapshotRate_TrimsOldPoints(t *testing.T) {
	saveETAState(t)
	rateHistMu.Lock()
	rateHist = []ratePoint{
		{t: time.Now().Add(-90 * time.Second), n: 1},
		{t: time.Now().Add(-30 * time.Second), n: 2},
	}
	rateHistMu.Unlock()

	Progress.Store(3)
	SnapshotRate()

	rateHistMu.Lock()
	defer rateHistMu.Unlock()
	if len(rateHist) != 2 {
		t.Fatalf("过期快照应被裁剪, 期望 2 个点, got %d", len(rateHist))
	}
	if rateHist[0].n != 2 {
		t.Fatalf("裁剪后首个点应为 n=2, got %d", rateHist[0].n)
	}
}

// 1.4.11 回归：检测时间状态必须能安全地被检测循环写、被 /api/status 读。
func TestCheckTiming_AtomicAccessors(t *testing.T) {
	saveETAState(t)
	checkStartNano.Store(0)
	if got := CheckElapsed(); got != 0 {
		t.Fatalf("尚未开始检测时 elapsed 应为 0, got %v", got)
	}

	start := time.Now().Add(-2 * time.Second)
	SetCheckStart(start)
	if CheckStartNano() != start.UnixNano() {
		t.Fatal("SetCheckStart 未正确写入")
	}
	if elapsed := CheckElapsed(); elapsed < time.Second || elapsed > 5*time.Second {
		t.Fatalf("elapsed 明显不合理: %v", elapsed)
	}

	SetCheckDuration(3 * time.Second)
	if CheckDuration() != 3*time.Second {
		t.Fatalf("CheckDuration 应为 3s, got %v", CheckDuration())
	}
	SetCheckEnd(time.Now())
	if CheckEndNano() <= 0 {
		t.Fatal("SetCheckEnd 未正确写入")
	}
}

package check

// ETA 预估机制（移植自社区项目 subs-check-pro 的 check/eta.go，净室改写适配本项目）：
// 通过滑动窗口实时速率 + 历史速率融合，估算本轮检测剩余耗时。
// 语义约定（与上游一致）：-1=计算中，0=空闲/完成，>0=剩余秒数。

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"substore-ui-go/internal/subscheck/config"
)

// ETASeconds 供 /api/status 读取。-1=计算中，0=空闲/完成，>0=剩余秒数。
var ETASeconds atomic.Int64

type ratePoint struct {
	t time.Time
	n uint32
}

var (
	rateHistMu sync.Mutex
	rateHist   []ratePoint

	histRateMu sync.RWMutex
	histRate   float64 // 上次检测速率（节点/秒），冷启动参考
)

// SetHistoricalRate 在新检测开始前注入参考速率（上次检测的平均速率）。
func SetHistoricalRate(nodesPerSec float64) {
	histRateMu.Lock()
	histRate = nodesPerSec
	histRateMu.Unlock()
}

// ResetETA 新检测开始时清空快照（histRate 保留，供本轮冷启动使用）。
func ResetETA() {
	rateHistMu.Lock()
	rateHist = rateHist[:0]
	rateHistMu.Unlock()
}

// SnapshotRate 每 500ms 由后台 goroutine 调用，记录进度快照。
func SnapshotRate() {
	rateHistMu.Lock()
	defer rateHistMu.Unlock()
	now := time.Now()
	rateHist = append(rateHist, ratePoint{t: now, n: Progress.Load()})
	cutoff := now.Add(-60 * time.Second)
	i := 0
	for i < len(rateHist) && rateHist[i].t.Before(cutoff) {
		i++
	}
	if i > 0 {
		rateHist = append(rateHist[:0], rateHist[i:]...)
	}
}

// UpdateETA 根据实时速率与历史速率融合计算剩余时间。
func UpdateETA() {
	total := int64(ProxyCount.Load())
	done := int64(Progress.Load())
	if total <= 0 || done >= total {
		ETASeconds.Store(0)
		return
	}
	elapsed := CheckElapsed().Seconds()
	if elapsed < 3 {
		ETASeconds.Store(-1)
		return
	}

	var remaining int64
	// SuccessLimit 由 collector 的最终通过数触发，不是 Available（测活通过数）。
	// 用 Available 会因测活通过数远大于最终通过数而提前判定「已完成」，
	// 实机曾出现 eta=0 但检测仍持续数分钟的情况。
	limit := int64(config.GlobalConfig.SuccessLimit)
	if limit > 0 {
		got := int64(CollectorPassed.Load())
		if got >= limit {
			ETASeconds.Store(0)
			return
		}
		if got > 0 {
			// 平均每产出1个可用节点需要处理多少总节点
			nodesPerAvail := float64(done) / float64(got)
			remaining = int64(math.Ceil(float64(limit-got) * nodesPerAvail))
		} else {
			remaining = total - done // 冷启动兜底
		}
	} else {
		remaining = total - done
	}
	// 折算出的剩余量不能超过物理上还没处理的节点数。
	// 实机曾出现 SuccessLimit 始终无法达成（最终通过率过低）导致整轮跑满，
	// 此时折算值会远大于真实剩余量，ETA 被高估数倍。
	if remaining > total-done {
		remaining = total - done
	}

	pct := float64(done) / float64(total) * 100

	// 滑动窗口实时速率
	var rtRate float64
	rateHistMu.Lock()
	if len(rateHist) >= 2 {
		oldest := rateHist[0]
		winSec := time.Since(oldest.t).Seconds()
		winN := int64(Progress.Load()) - int64(oldest.n)
		if winSec > 0 && winN >= 0 {
			rtRate = float64(winN) / winSec
		}
	}
	rateHistMu.Unlock()

	// 全局平均兜底（页面刷新后立即可用）
	if rtRate <= 0 && elapsed > 0 && done > 0 {
		rtRate = float64(done) / elapsed
	}

	histRateMu.RLock()
	hr := histRate
	histRateMu.RUnlock()

	finalRate := rtRate
	if hr > 0 {
		switch {
		case rtRate <= 0:
			finalRate = hr
		default:
			// 线性融合：冷启动历史权重更高（80%→30%），随进度平滑过渡到完全实时
			w := 0.2 + (pct/100)*0.8
			if w > 1 {
				w = 1
			}
			finalRate = rtRate*w + hr*(1-w)
		}
	}

	if finalRate <= 0 {
		ETASeconds.Store(-1)
		return
	}
	ETASeconds.Store(int64(math.Ceil(float64(remaining) / finalRate)))
}

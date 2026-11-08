package check

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juju/ratelimit"
	"github.com/metacubex/mihomo/adapter"
	_ "github.com/metacubex/mihomo/config" // init() sets dns.ParseNameServer, required by ParseProxy
	"github.com/metacubex/mihomo/constant"
	"substore-ui-go/internal/subscheck/check/platform"
	"substore-ui-go/internal/subscheck/config"
	"substore-ui-go/internal/subscheck/monitor"
	proxyutils "substore-ui-go/internal/subscheck/proxy"
)

// Result 存储节点检测结果
type Result struct {
	Proxy      map[string]any
	Openai     *platform.OpenAIResult
	Youtube    string
	Netflix    *platform.NetflixResult
	Google     bool
	Cloudflare bool
	Disney     *platform.DisneyResult
	Gemini     string
	TikTok     string
	Claude     string
	Spotify    string
	IP         string
	IPRisk     string
	Country    string
	Speed      int // KB/s, 0 表示未测速或测速未通过
}

// aliveResult 存活检测通过的中间结果
type aliveResult struct {
	Proxy map[string]any
}

// ProxyChecker 处理代理检测的主要结构体
// Per-stage counts live on package-level atomics (Progress / Available /
// MediaDone / FilterPassed / SpeedDone / SpeedOk) so both the CLI progress
// UI and the web admin API can read them without plumbing through a pointer.
type ProxyChecker struct {
	results    []Result
	proxyCount int
	progress   int32 // alive-stage done count; shared with showProgress
	available  int32 // alive-stage pass count;  shared with showProgress
}

var Progress atomic.Uint32
var Available atomic.Uint32
var ProxyCount atomic.Uint32
var TotalBytes atomic.Uint64
var Phase atomic.Uint32 // 0=idle, 1=pipeline running

// Pipeline-wide live counters. These are 0 when idle and reflect
// how many items have cleared each stage during an active run.
// AliveDone/AliveOk mirror Progress/Available for backward-compat
// with the single-phase progress UI.
var (
	MediaDone    atomic.Uint32 // checkMedia completions (pass-through, never drops)
	FilterPassed atomic.Uint32 // items that matched the filter and entered speed/collector
	SpeedDone    atomic.Uint32 // checkSpeed completions (pass + fail)
	SpeedOk      atomic.Uint32 // checkSpeed passes (also equals collector input when hasSpeedTest)
	// CollectorPassed 是 collector 已收到的最终通过节点数，也是 SuccessLimit
	// 实际比较的计数。它与 Available（测活通过数）不同：测速会淘汰大量节点，
	// 因此不能用 Available 推断「是否已达到成功上限」，否则 ETA 会提前归零。
	CollectorPassed atomic.Uint32
)

// 测活阶段统计：只在本轮结束时汇总输出一行，用来量化超时策略的收益。
// buckets 依次为 <0.5s <1s <2s <3s <5s <8s <10s >=10s。
var (
	aliveStatOK      atomic.Uint32
	aliveStatTimeout atomic.Uint32
	aliveStatFastErr atomic.Uint32
	aliveStatTotalMs atomic.Uint64
	aliveStatBuckets [8]atomic.Uint32
	aliveStageMs     atomic.Int64
)

func resetAliveStats() {
	aliveStatOK.Store(0)
	aliveStatTimeout.Store(0)
	aliveStatFastErr.Store(0)
	aliveStatTotalMs.Store(0)
	aliveStageMs.Store(0)
	for i := range aliveStatBuckets {
		aliveStatBuckets[i].Store(0)
	}
}

// recordAlive 记录一次测活结果：kind 为 ok / timeout / err。
func recordAlive(elapsed time.Duration, kind string) {
	ms := elapsed.Milliseconds()
	aliveStatTotalMs.Add(uint64(ms))
	switch kind {
	case "ok":
		aliveStatOK.Add(1)
	case "timeout":
		aliveStatTimeout.Add(1)
	default:
		aliveStatFastErr.Add(1)
	}
	switch {
	case ms < 500:
		aliveStatBuckets[0].Add(1)
	case ms < 1000:
		aliveStatBuckets[1].Add(1)
	case ms < 2000:
		aliveStatBuckets[2].Add(1)
	case ms < 3000:
		aliveStatBuckets[3].Add(1)
	case ms < 5000:
		aliveStatBuckets[4].Add(1)
	case ms < 8000:
		aliveStatBuckets[5].Add(1)
	case ms < 10000:
		aliveStatBuckets[6].Add(1)
	default:
		aliveStatBuckets[7].Add(1)
	}
}

// aliveStatsSummary 返回一行测活统计摘要。
func aliveStatsSummary() string {
	ok := aliveStatOK.Load()
	timeout := aliveStatTimeout.Load()
	fastErr := aliveStatFastErr.Load()
	total := int64(ok) + int64(timeout) + int64(fastErr)
	avg := int64(0)
	if total > 0 {
		avg = int64(aliveStatTotalMs.Load()) / total
	}
	return fmt.Sprintf("测活统计: 通过=%d 超时=%d 快速失败=%d 平均=%dms 测活阶段=%ds 延迟分桶(<0.5s/<1s/<2s/<3s/<5s/<8s/<10s/>=10s)=%d/%d/%d/%d/%d/%d/%d/%d",
		ok, timeout, fastErr, avg, aliveStageMs.Load()/1000,
		aliveStatBuckets[0].Load(), aliveStatBuckets[1].Load(), aliveStatBuckets[2].Load(), aliveStatBuckets[3].Load(),
		aliveStatBuckets[4].Load(), aliveStatBuckets[5].Load(), aliveStatBuckets[6].Load(), aliveStatBuckets[7].Load())
}

// PhaseResult 保存单个阶段的最终结果
type PhaseResult struct {
	Available uint32 `json:"available"`
	Total     uint32 `json:"total"`
}

// PhaseResults 保存各阶段最终结果，供前端展示历史数据
var PhaseResults [4]atomic.Pointer[PhaseResult] // index 1-3 对应三个阶段

func SavePhaseResult(phase int, available, total uint32) {
	if phase >= 1 && phase <= 3 {
		PhaseResults[phase].Store(&PhaseResult{Available: available, Total: total})
	}
}

func GetPhaseResult(phase int) *PhaseResult {
	if phase >= 1 && phase <= 3 {
		return PhaseResults[phase].Load()
	}
	return nil
}

func ResetPhaseResults() {
	for i := 1; i <= 3; i++ {
		PhaseResults[i].Store(nil)
	}
}

var progressPaused atomic.Bool
var progressRendered atomic.Bool

// activeCancelMu guards activeCancel.
var activeCancelMu sync.Mutex

// activeCancel cancels the currently running phase. nil when idle or
// between phases; the per-phase dispatcher installs and clears it.
var activeCancel context.CancelFunc

// RequestCancel aborts the currently running check phase, if any.
// Safe to call from any goroutine; no-op when idle.
// Phases installed after this call are unaffected (per-phase scope,
// matching the pre-context ForceClose-reset-between-phases behaviour).
//
// Deliberately silent: emitting a log here would land between the
// progress renderer's rows and get overwritten by the next frame's
// cursor-up escape. run() logs the cancellation after pauseProgress
// has parked the renderer.
func RequestCancel() {
	activeCancelMu.Lock()
	defer activeCancelMu.Unlock()
	if activeCancel != nil {
		activeCancel()
	}
}

// installPhaseCancel registers cancel as the active phase canceller
// and returns a cleanup closure that cancels and clears it.
// Call the returned closure from a defer in the phase function.
func installPhaseCancel(cancel context.CancelFunc) func() {
	activeCancelMu.Lock()
	activeCancel = cancel
	activeCancelMu.Unlock()
	return func() {
		activeCancelMu.Lock()
		activeCancel = nil
		activeCancelMu.Unlock()
		cancel()
	}
}

var Bucket *ratelimit.Bucket

// CheckStartTime / CheckEndTime / CheckDuration 记录最近一轮检测的起止时间，
// 供 /api/status 的 elapsed/duration 字段与 ETA 估算使用。
var (
	CheckStartTime time.Time
	CheckEndTime   time.Time
	CheckDuration  time.Duration
)

// effectiveConcurrency 计算阶段实际并发数
func effectiveConcurrency(phaseConcurrency, fallback, itemCount int) int {
	c := phaseConcurrency
	if c <= 0 {
		c = fallback
	}
	if itemCount < c {
		c = itemCount
	}
	if c < 1 {
		c = 1
	}
	return c
}

// Check 执行代理检测的主函数
func Check() ([]Result, error) {
	proxyutils.ResetRenameCounter()

	ProxyCount.Store(0)
	Available.Store(0)
	Progress.Store(0)
	Phase.Store(0)

	TotalBytes.Store(0)

	// keep-days 历史节点前置
	var proxies []map[string]any
	if len(config.GlobalProxies) > 0 {
		slog.Info(fmt.Sprintf("添加历史待测节点，数量: %d", len(config.GlobalProxies)))
		proxies = append(proxies, config.GlobalProxies...)
	}
	// 降载：日常轮只检测一部分新节点，历史节点（上一轮可用节点）仍每轮全测，
	// 每 full-check-every 轮再做一次全量检测，避免新节点漏检。
	// 分片决策必须发生在拉取之前：分片在订阅内部生效，
	// 未选中的节点不会进入节点表，加载期内存峰值也随之下降。
	shard, shardNote := planCheckShare()
	if shardNote != "" {
		slog.Info(shardNote)
	}
	// 加载阶段临时收紧 GC 阈值：几秒内解析上千个订阅会产生上百 MB 临时对象，
	// 堆峰值实测 154MB（活对象只有 13MB）。加载结束后由 ConfigureMemory 恢复配置值。
	monitor.ConfigureMemory(config.GlobalConfig.MemoryLimit, loadGCPercent())
	tmp, err := proxyutils.GetProxiesSharded(shard)
	if err != nil {
		return nil, fmt.Errorf("获取节点失败: %w", err)
	}
	proxies = append(proxies, tmp...)
	slog.Info(fmt.Sprintf("获取节点数量: %d", len(proxies)))

	// 重置全局节点
	config.GlobalProxies = make([]map[string]any, 0)

	proxies = proxyutils.DeduplicateProxies(proxies)
	slog.Info(fmt.Sprintf("去重后节点数量: %d", len(proxies)))

	// Drop subscription info entries (remaining traffic / expiry / official
	// site / address lines) before dispatch. They appear as pseudo-proxies in
	// some subscription formats and would waste a full alive check, often
	// returning a misleading "node" that is just text. Mirrors proxyhub's
	// isInfoNode: only skip when the item has no real server/port, so
	// legitimate nodes whose display name happens to contain an info keyword
	// are preserved.
	proxies = filterInfoNodes(proxies)
	slog.Info(fmt.Sprintf("过滤信息条目后节点数量: %d", len(proxies)))

	// 节点表加载完成，把加载期的临时对象（订阅原文、解析中间态）还给操作系统，
	// 否则常驻内存会一直停在加载峰值附近（实测加载后即占用 197MB）
	monitor.ReleaseMemory()
	slog.Info(monitor.Summary())

	// 订阅来源统计要在轮次开始前算好：轮次中节点表会被逐步释放，
	// 结束后没法再遍历整份节点表。
	subTotals := countSubUrls(proxies)

	// 内存软上限与 GC 阈值（配置改动下一轮生效）
	monitor.ConfigureMemory(config.GlobalConfig.MemoryLimit, config.GlobalConfig.GCPercent)

	checker := &ProxyChecker{
		results: make([]Result, 0),
	}
	return checker.run(proxies, subTotals)
}

// run drives the 4-stage pipeline: dispatch → alive → media+filter → speed → collect.
// Stages run concurrently, connected by channels. SuccessLimit cancels the whole
// pipeline as soon as the collector has gathered N passing items; in-flight work
// is drained and un-dispatched items are discarded.
func (pc *ProxyChecker) run(proxies []map[string]any, subTotals map[string]int) ([]Result, error) {
	if subTotals == nil {
		subTotals = countSubUrls(proxies)
	}
	if config.GlobalConfig.TotalSpeedLimit != 0 {
		Bucket = ratelimit.NewBucketWithRate(float64(config.GlobalConfig.TotalSpeedLimit*1024*1024), int64(config.GlobalConfig.TotalSpeedLimit*1024*1024/10))
	} else {
		Bucket = ratelimit.NewBucketWithRate(float64(math.MaxInt64), int64(math.MaxInt64))
	}

	slog.Info("开始检测节点")
	slog.Info("当前参数", "timeout", config.GlobalConfig.Timeout, "enable-speedtest", config.GlobalConfig.SpeedTestUrl != "", "min-speed", config.GlobalConfig.MinSpeed, "download-timeout", config.GlobalConfig.DownloadTimeout, "download-mb", config.GlobalConfig.DownloadMB, "total-speed-limit", config.GlobalConfig.TotalSpeedLimit)

	ResetPhaseResults()
	resetAliveStats()

	done := make(chan bool)
	if config.GlobalConfig.PrintProgress {
		go pc.showProgress(done)
	}

	// Capture the speed-test URL once at pipeline start so the current run
	// stays consistent even if the user edits config mid-check. Otherwise
	// flipping the URL to empty mid-run would cause every in-flight speed
	// request to fail (no host) and silently drop nearly all results.
	speedTestURL := config.GlobalConfig.SpeedTestUrl
	hasSpeedTest := speedTestURL != ""
	total := len(proxies)

	// 冷却期内已确认超时的节点本轮不再重复测活（黑洞节点是整轮主要耗时来源）。
	// 冷却时长默认按检测间隔推算：需要覆盖分片轮转周期（1/check-share 轮），
	// 否则上一轮的黑洞节点会在本轮再次被完整超时一遍。
	deadNodes.SetTTL(deadNodeTTLOf(config.GlobalConfig.DeadNodeTTL, config.GlobalConfig.CheckInterval))
	if kept, skipped := deadNodes.Filter(proxies); skipped > 0 {
		proxies = kept
		total = len(proxies)
		slog.Info(fmt.Sprintf("跳过冷却期内失效节点: %d, 剩余待测: %d, 失效节点缓存: %d, 冷却时长: %s",
			skipped, total, deadNodes.Len(), deadNodes.TTL()))
	}

	aliveConcurrency := effectiveConcurrency(config.GlobalConfig.Concurrent, config.GlobalConfig.Concurrent, total)
	mediaConcurrency := effectiveConcurrency(config.GlobalConfig.MediaConcurrent, config.GlobalConfig.Concurrent, total)
	speedConcurrency := effectiveConcurrency(config.GlobalConfig.SpeedConcurrent, config.GlobalConfig.Concurrent, total)
	slog.Info(fmt.Sprintf("启动流水线: 输入=%d, 并发(测活/媒体/测速)=%d/%d/%d", total, aliveConcurrency, mediaConcurrency, speedConcurrency))

	// showProgress keeps reading pc.progress / pc.available / pc.proxyCount;
	// the alive stage owns these counters throughout the pipeline run.
	pc.resetPhaseCounters(total)
	Phase.Store(1)
	resumeProgress()

	// Compile filter patterns once; media workers re-use the slice.
	patterns := CompileFilterPatterns()
	if len(patterns) > 0 {
		slog.Info(fmt.Sprintf("应用节点过滤规则，共 %d 个正则表达式", len(patterns)))
	}

	// Whole-pipeline cancellation: collector pulls the trigger on SuccessLimit,
	// RequestCancel pulls it on external SIGHUP / HTTP force-close.
	ctx, cancel := context.WithCancel(context.Background())
	defer installPhaseCancel(cancel)()

	// ETA 预估：每 500ms 快照进度，用滑动窗口实时速率 + 历史速率融合估算剩余时间。
	// ctx 取消时做最后一次估算供 /api/status 读取，goroutine 随整条流水线一起退出。
	CheckStartTime = time.Now()
	ResetETA()
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				UpdateETA()
				return
			case <-ticker.C:
				SnapshotRate()
				UpdateETA()
			}
		}
	}()

	// Channels sized to each stage's concurrency to keep buffering bounded.
	aliveIn := make(chan aliveTask, aliveConcurrency)
	mediaIn := make(chan mediaEntry, mediaConcurrency)
	speedIn := make(chan pipelineItem, speedConcurrency)
	collectIn := make(chan pipelineItem, speedConcurrency)

	if config.GlobalConfig.ShuffleTestOrder {
		slog.Info("已打乱节点测试顺序，输出仍保持订阅原序")
	}

	// Dispatcher
	go pipelineDispatch(ctx, proxies, aliveIn)

	aliveStart := time.Now()
	// Alive workers
	aliveWg := pc.startAliveWorkers(ctx, aliveConcurrency, aliveIn, mediaIn)
	go func() {
		aliveWg.Wait()
		aliveStageMs.Store(int64(time.Since(aliveStart).Milliseconds()))
		close(mediaIn)
	}()

	// Media workers (filter runs inline on each passing item)
	mediaWg := pc.startMediaWorkers(ctx, mediaConcurrency, mediaIn, speedIn, collectIn, hasSpeedTest, patterns)
	go func() {
		mediaWg.Wait()
		close(speedIn)
		if !hasSpeedTest {
			close(collectIn)
		}
	}()

	// Speed workers (optional)
	if hasSpeedTest {
		speedWg := pc.startSpeedWorkers(ctx, speedConcurrency, speedIn, collectIn, speedTestURL)
		go func() { speedWg.Wait(); close(collectIn) }()
	}

	// Collector: place items in pre-allocated slots to preserve subscription order.
	// The SuccessLimit hit notice is *not* logged here: emitting slog output
	// mid-render interleaves with the progress writer and breaks cursor-up
	// positioning. We remember whether we tripped the limit and log it after
	// pauseProgress has parked the renderer.
	out := make([]*Result, total)
	var finalPassed int32
	limitHit := false
	for item := range collectIn {
		r := item.r
		out[item.idx] = &r
		finalPassed++
		CollectorPassed.Add(1)
		if config.GlobalConfig.SuccessLimit > 0 && finalPassed >= config.GlobalConfig.SuccessLimit && !limitHit {
			limitHit = true
			cancel()
		}
	}

	pauseProgress()

	if limitHit {
		slog.Warn(fmt.Sprintf("达到成功数量限制: %d，已停止流水线", config.GlobalConfig.SuccessLimit))
	} else if ctx.Err() != nil {
		// External cancel (RequestCancel via SIGHUP / HTTP force-close).
		// Logged here rather than in RequestCancel because emitting it
		// while the progress renderer is still drawing would let the
		// next frame's cursor-up escape overwrite the warn line.
		slog.Warn("收到取消信号，已停止流水线")
	}

	// Snapshot per-stage results. Totals cascade: alive counts against input,
	// media counts against alive, speed counts against filter-passed.
	aliveOk := Available.Load()
	mediaDone := MediaDone.Load()
	filterPassed := FilterPassed.Load()
	SavePhaseResult(1, aliveOk, uint32(total))
	SavePhaseResult(2, mediaDone, aliveOk)
	if hasSpeedTest {
		SavePhaseResult(3, SpeedOk.Load(), filterPassed)
	}

	// Flatten in subscription order, dropping empty slots.
	pc.results = make([]Result, 0, finalPassed)
	for _, r := range out {
		if r != nil {
			pc.results = append(pc.results, *r)
		}
	}

	if config.GlobalConfig.PrintProgress {
		done <- true
	}
	Phase.Store(0)

	slog.Info(fmt.Sprintf("存活节点数量: %d", aliveOk))
	if len(patterns) > 0 {
		slog.Info(fmt.Sprintf("过滤前节点数量: %d, 过滤后节点数量: %d", mediaDone, filterPassed))
	} else if hasSpeedTest {
		slog.Info(fmt.Sprintf("流媒体阶段通过数量: %d", filterPassed))
	}
	slog.Info(fmt.Sprintf("可用节点数量: %d", len(pc.results)))
	slog.Info(fmt.Sprintf("测试总消耗流量: %.3fGB", float64(TotalBytes.Load())/1024/1024/1024))
	slog.Info(aliveStatsSummary())
	slog.Info(fmt.Sprintf("测速提前结束次数: %d", platform.EarlyStopCount.Load()))
	slog.Info(fmt.Sprintf("慢节点提前结束次数: %d", platform.SlowStopCount.Load()))

	pc.checkSubscriptionSuccessRate(subTotals)

	CheckEndTime = time.Now()
	CheckDuration = time.Since(CheckStartTime)
	ETASeconds.Store(0) // 标记本轮结束：前端看到 eta=0 即表示完成/空闲
	if CheckDuration > 0 {
		// 记录本轮平均速率，供下一轮冷启动前 3 秒的融合估算使用
		SetHistoricalRate(float64(Progress.Load()) / CheckDuration.Seconds())
	}

	// 一轮结束后把临时对象还给操作系统，避免常驻内存停在峰值
	monitor.ReleaseMemory()
	slog.Info(monitor.Summary())

	return pc.results, nil
}

// ======= Pipeline types =======

// aliveTask is an input proxy plus its original index (for order preservation).
type aliveTask struct {
	idx   int
	proxy map[string]any
}

// mediaEntry carries an alive-pass proxy into the media stage.
type mediaEntry struct {
	idx int
	a   aliveResult
}

// pipelineItem flows through filter, speed test, and the collector.
type pipelineItem struct {
	idx int
	r   Result
}

// ======= Pipeline stages =======
//
// The pipeline runs alive / media+filter / speed concurrently, connected
// by channels. An idx field rides along each item so the collector can
// restore original subscription order before emitting the final slice.
//
// Cancellation: a single context.Context covers the entire pipeline.
// SuccessLimit causes the collector to cancel it once N passes have been
// gathered; goroutines then drain their inputs and exit.

// pipelineDispatch feeds proxies into the alive stage. By default it dispatches
// in subscription order; with shuffle-test-order on, it dispatches in a random
// permutation so same-airport / multi-protocol nodes don't cluster in one test
// window. Either way each task carries its original index, so the collector
// restores subscription order on output regardless of dispatch sequence.
// Closes out on exit and honours ctx cancellation.
func pipelineDispatch(ctx context.Context, proxies []map[string]any, out chan<- aliveTask) {
	defer close(out)

	order := make([]int, len(proxies))
	for i := range order {
		order[i] = i
	}
	if config.GlobalConfig.ShuffleTestOrder {
		rand.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
	}

	for _, i := range order {
		select {
		case <-ctx.Done():
			return
		case out <- aliveTask{idx: i, proxy: proxies[i]}:
		}
		// 节点表逐个释放：检测完的节点不再被引用，Go 就能回收它们，
		// 活跃堆不会一直背着上万个节点的配置（实测峰值内存明显下降）
		proxies[i] = nil
	}
}

// startAliveWorkers spawns n alive-check workers.
// Cancellation policy (middle stage): when cancel fires, workers stop
// pulling new items from their input queue *and* allow in-flight items
// to be dropped at the send boundary via a ctx-aware select. This
// prevents items queued at the time of cancel from triggering wasted
// work in the downstream media / speed stages.
// Items already classified as "passed speed" never get dropped — see
// startSpeedWorkers for the asymmetric policy at the last boundary.
func (pc *ProxyChecker) startAliveWorkers(ctx context.Context, n int, in <-chan aliveTask, out chan<- mediaEntry) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range in {
				if ctx.Err() != nil {
					return
				}
				r := pc.checkAlive(t.proxy)
				pc.incrementProgress()
				if r == nil {
					continue
				}
				pc.incrementAvailable()
				select {
				case <-ctx.Done():
					return
				case out <- mediaEntry{idx: t.idx, a: *r}:
				}
			}
		}()
	}
	return &wg
}

// startMediaWorkers spawns n media-check workers.
// checkMedia always produces a Result; the worker applies the filter inline
// and forwards passing items to speedOut (hasSpeed) or collectOut (!hasSpeed).
// Cancellation policy:
//   - hasSpeed: middle stage, ctx-aware select on speedOut (drops on cancel
//     race so cancel-dropped items don't waste a ~10s speed test)
//   - !hasSpeed: last stage, unconditional send to collectOut so items
//     classified as passing the filter are never dropped at the final hop
func (pc *ProxyChecker) startMediaWorkers(
	ctx context.Context,
	n int,
	in <-chan mediaEntry,
	speedOut, collectOut chan<- pipelineItem,
	hasSpeed bool,
	patterns []*regexp.Regexp,
) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range in {
				if ctx.Err() != nil {
					return
				}
				res := pc.checkMedia(entry.a, ctx)
				MediaDone.Add(1)
				if res == nil || !MatchesFilter(*res, patterns) {
					continue
				}
				FilterPassed.Add(1)
				if hasSpeed {
					select {
					case <-ctx.Done():
						return
					case speedOut <- pipelineItem{idx: entry.idx, r: *res}:
					}
				} else {
					// last stage — collector always reads collectIn, so the
					// unconditional send never blocks; guarantees every
					// filter-passed item ends up in the output.
					collectOut <- pipelineItem{idx: entry.idx, r: *res}
				}
			}
		}()
	}
	return &wg
}

// startSpeedWorkers spawns n speed-test workers.
// Last stage before the collector: items that pass min-speed are sent
// unconditionally so an item classified as "good" is never dropped at
// the final hop, even if cancel fires between SpeedOk.Add and the send.
// ctx.Err is only checked at the top of the loop to avoid starting a
// fresh ~10s speed test once we've already tripped SuccessLimit.
//
// speedTestURL is passed through (captured at pipeline start) so the
// run stays self-consistent even if the user edits SpeedTestUrl in
// the config file mid-check.
func (pc *ProxyChecker) startSpeedWorkers(ctx context.Context, n int, in <-chan pipelineItem, out chan<- pipelineItem, speedTestURL string) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range in {
				if ctx.Err() != nil {
					return
				}
				updated := pc.checkSpeed(item.r, speedTestURL)
				SpeedDone.Add(1)
				if updated == nil {
					continue
				}
				SpeedOk.Add(1)
				out <- pipelineItem{idx: item.idx, r: *updated}
			}
		}()
	}
	return &wg
}

// checkAlive 检测单个代理是否存活
// checkAlive 检测单个代理是否存活。
// 超时口径由 alive-timeout 单独控制（毫秒，0 表示沿用 timeout）：
// 测活阶段占整轮 95% 以上时间，绝大多数节点是"连不上"而不是"慢"，
// 把测活超时单独调小可以在不牺牲存活节点的前提下压缩整轮耗时。
func (pc *ProxyChecker) checkAlive(proxy map[string]any) *aliveResult {
	if os.Getenv("SUB_CHECK_SKIP") != "" {
		return &aliveResult{Proxy: proxy}
	}

	timeout := config.GlobalConfig.AliveTimeout
	if timeout <= 0 {
		timeout = config.GlobalConfig.Timeout
	}

	start := time.Now()
	httpClient := CreateClientWithTimeout(proxy, timeout)
	if httpClient == nil {
		aliveStatFastErr.Add(1)
		return nil
	}
	alive, err := platform.CheckAlive(httpClient.Client)
	httpClient.Close()

	if err != nil {
		if isTimeoutError(err) {
			recordAlive(time.Since(start), "timeout")
			// 超时节点进入冷却，避免下一轮继续为黑洞节点付出完整超时时间
			deadNodes.Mark(proxy)
		} else {
			recordAlive(time.Since(start), "err")
		}
		return nil
	}
	if !alive {
		recordAlive(time.Since(start), "err")
		return nil
	}

	recordAlive(time.Since(start), "ok")
	// 恢复的节点立即解除冷却，保证能被重新纳入可用列表
	deadNodes.Revive(proxy)
	return &aliveResult{Proxy: proxy}
}

// isTimeoutError 判断错误是否属于超时类（连接/读写超时或客户端截止时间）。
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// checkSpeed 对已有的 Result 执行测速。
// 通过 min-speed 的节点填充 r.Speed 并返回;未通过的返回 nil。
// 不修改 proxy["name"]。
// speedTestURL 由调用方在流水线启动时冻结的快照,避免 config 热重载
// 把 URL 置空后把当前这一轮的所有测速请求打穿(no host error)。
func (pc *ProxyChecker) checkSpeed(r Result, speedTestURL string) *Result {
	if os.Getenv("SUB_CHECK_SKIP") != "" {
		r.Speed = 0
		return &r
	}

	httpClient := CreateClient(r.Proxy)
	if httpClient == nil {
		return nil
	}
	defer httpClient.Close()

	speed, _, err := platform.CheckSpeed(httpClient.Client, Bucket, httpClient.BytesRead, speedTestURL, config.GlobalConfig.SpeedEarlyStop)
	if err != nil || speed < config.GlobalConfig.MinSpeed {
		return nil
	}

	r.Speed = speed
	return &r
}

// checkMedia 执行流媒体检测和必要的国家查询。
// 不会丢弃节点,不会修改 proxy["name"];检测结果写入 Result 的结构化字段。
// Counter updates are owned by the caller (media pipeline worker).
// ctx 用于媒体重试的取消等待：检测任务一旦出队就完整跑完，但遇到
// SuccessLimit/外部取消时不再等待后续重试退避。
func (pc *ProxyChecker) checkMedia(a aliveResult, ctx context.Context) *Result {
	res := &Result{Proxy: a.Proxy}

	if os.Getenv("SUB_CHECK_SKIP") != "" {
		return res
	}

	httpClient := CreateClient(a.Proxy)
	if httpClient == nil {
		return res
	}
	defer httpClient.Close()

	if config.GlobalConfig.MediaCheck {
		mediaTimeout := config.GlobalConfig.MediaCheckTimeout
		if mediaTimeout <= 0 {
			mediaTimeout = 10
		}
		mediaClient := &http.Client{
			Transport: httpClient.Client.Transport,
			Timeout:   time.Duration(mediaTimeout) * time.Second,
		}

		// 并行检测所有平台
		var mediaWg sync.WaitGroup
		for _, plat := range config.GlobalConfig.Platforms {
			switch plat {
			case "openai":
				mediaWg.Add(1)
				go func() {
					defer mediaWg.Done()
					res.Openai = platform.CheckOpenAI(mediaClient)
				}()
			case "youtube":
				mediaWg.Add(1)
				go func() {
					defer mediaWg.Done()
					var region string
					err := withMediaRetry(ctx, func() error {
						var e error
						region, e = platform.CheckYoutube(mediaClient)
						return e
					})
					if err != nil {
						slog.Debug(fmt.Sprintf("YouTube 检测失败: %v", err))
					}
					if region != "" {
						res.Youtube = region
					}
				}()
			case "netflix":
				mediaWg.Add(1)
				go func() {
					defer mediaWg.Done()
					var nf *platform.NetflixResult
					err := withMediaRetry(ctx, func() error {
						var e error
						nf, e = platform.CheckNetflix(mediaClient)
						return e
					})
					if err != nil {
						slog.Debug(fmt.Sprintf("Netflix 检测失败: %v", err))
					}
					res.Netflix = nf
				}()
			case "disney":
				mediaWg.Add(1)
				go func() {
					defer mediaWg.Done()
					var d *platform.DisneyResult
					err := withMediaRetry(ctx, func() error {
						var e error
						d, e = platform.CheckDisney(mediaClient)
						return e
					})
					if err != nil {
						slog.Debug(fmt.Sprintf("Disney 检测失败: %v", err))
					}
					res.Disney = d
				}()
			case "gemini":
				mediaWg.Add(1)
				go func() {
					defer mediaWg.Done()
					var region string
					err := withMediaRetry(ctx, func() error {
						var e error
						region, e = platform.CheckGemini(mediaClient)
						return e
					})
					if err != nil {
						slog.Debug(fmt.Sprintf("Gemini 检测失败: %v", err))
					}
					if region != "" {
						res.Gemini = region
					}
				}()
			case "claude":
				mediaWg.Add(1)
				go func() {
					defer mediaWg.Done()
					var region string
					err := withMediaRetry(ctx, func() error {
						var e error
						region, e = platform.CheckClaude(mediaClient)
						return e
					})
					if err != nil {
						slog.Debug(fmt.Sprintf("Claude 检测失败: %v", err))
					}
					if region != "" {
						res.Claude = region
					}
				}()
			case "spotify":
				mediaWg.Add(1)
				go func() {
					defer mediaWg.Done()
					var region string
					err := withMediaRetry(ctx, func() error {
						var e error
						region, e = platform.CheckSpotify(mediaClient)
						return e
					})
					if err != nil {
						slog.Debug(fmt.Sprintf("Spotify 检测失败: %v", err))
					}
					if region != "" {
						res.Spotify = region
					}
				}()
			case "iprisk":
				mediaWg.Add(1)
				go func() {
					defer mediaWg.Done()
					country, ip := proxyutils.GetProxyCountry(mediaClient)
					if ip == "" {
						return
					}
					res.IP = ip
					res.Country = country
					var risk string
					err := withMediaRetry(ctx, func() error {
						var e error
						risk, e = platform.CheckIPRisk(mediaClient, ip)
						return e
					})
					if err == nil {
						res.IPRisk = risk
					} else {
						slog.Debug(fmt.Sprintf("查询IP风险失败: %v", err))
					}
				}()
			case "tiktok":
				mediaWg.Add(1)
				go func() {
					defer mediaWg.Done()
					var region string
					err := withMediaRetry(ctx, func() error {
						var e error
						region, e = platform.CheckTikTok(mediaClient)
						return e
					})
					if err != nil {
						slog.Debug(fmt.Sprintf("TikTok 检测失败: %v", err))
					}
					if region != "" {
						res.TikTok = region
					}
				}()
			}
		}
		mediaWg.Wait()
	}

	// 如果没有通过 iprisk 得到 Country，而 RenameNode 开启，则显式查一次国家
	if res.Country == "" && config.GlobalConfig.RenameNode {
		country, _ := proxyutils.GetProxyCountry(httpClient.Client)
		res.Country = country
	}

	return res
}

// pauseProgress 暂停进度条并换行，确保后续日志不会与进度条混在一行
func pauseProgress() {
	progressPaused.Store(true)
	time.Sleep(150 * time.Millisecond) // 等待进度条goroutine停止输出
	if progressRendered.Load() {
		fmt.Println()                 // 仅在进度条实际输出过时才换行
		progressRendered.Store(false) // 标记换行已收尾,避免后续 done 信号重复换行
	}
}

// resumeProgress 恢复进度条显示
func resumeProgress() {
	progressRendered.Store(false)
	progressPaused.Store(false)
}

// 辅助方法
func (pc *ProxyChecker) incrementProgress() {
	atomic.AddInt32(&pc.progress, 1)
	Progress.Add(1)
}

func (pc *ProxyChecker) incrementAvailable() {
	atomic.AddInt32(&pc.available, 1)
	Available.Add(1)
}

func (pc *ProxyChecker) resetPhaseCounters(count int) {
	// Cancellation is scoped per-phase via installPhaseCancel, no reset needed here.
	pc.proxyCount = count
	atomic.StoreInt32(&pc.progress, 0)
	atomic.StoreInt32(&pc.available, 0)
	Progress.Store(0)
	Available.Store(0)
	ProxyCount.Store(uint32(count))

	// Reset downstream pipeline counters as well so a fresh run doesn't
	// inherit totals from a previous one (affects both the web admin UI
	// and the three-line CLI progress renderer).
	MediaDone.Store(0)
	FilterPassed.Store(0)
	SpeedDone.Store(0)
	SpeedOk.Store(0)
	CollectorPassed.Store(0)
}

// roundSeq 记录已启动的检测轮次，用于分片调度。
var (
	roundSeq atomic.Int64
	shardSeq atomic.Int64
)

// loadGCPercent 返回加载阶段使用的 GC 阈值。
// 订阅解析阶段临时对象极多，用更激进的阈值压住堆峰值；
// 加载结束后由 ConfigureMemory 恢复为配置里的阈值。
func loadGCPercent() int {
	const limit = 25
	p := config.GlobalConfig.GCPercent
	if p <= 0 || p > limit {
		return limit
	}
	return p
}

// planCheckShare 决定本轮的分片参数，在拉取订阅之前调用。
// 历史节点（config.GlobalProxies，来自 keep-days 快照，即上一轮可用节点）
// 不参与分片，每轮都测，保证输出的可用节点不会缩水。
// 每 full-check-every 轮做一次全量检测，保证新节点最多 N 轮内被发现一次。
func planCheckShare() (proxyutils.ShardSpec, string) {
	share := config.GlobalConfig.CheckShare
	every := config.GlobalConfig.FullCheckEvery
	if share <= 0 || share >= 1 || every <= 0 {
		return proxyutils.ShardSpec{}, ""
	}
	seq := roundSeq.Add(1)
	if seq%int64(every) == 0 {
		return proxyutils.ShardSpec{}, fmt.Sprintf("分片检测: 本轮为全量检测(第 %d 轮)", seq)
	}
	n := int(math.Round(1 / share))
	if n < 1 {
		n = 1
	}
	// 分片序号只在分片轮递增：全量轮不占用分片序号，
	// 保证任意连续 n 个分片轮里每个节点都会被覆盖一次
	offset := int(shardSeq.Add(1) % int64(n))
	return proxyutils.ShardSpec{Keep: n, Offset: offset},
		fmt.Sprintf("分片检测: 本轮按每订阅 1/%d 取样(第 %d 轮, 偏移 %d), 每 %d 轮全量一次", n, seq, offset, every)
}

// countSubUrls 统计各订阅来源的节点总数。
func countSubUrls(proxies []map[string]any) map[string]int {
	out := make(map[string]int)
	for _, proxy := range proxies {
		if subUrl, ok := proxy["sub_url"].(string); ok {
			out[subUrl]++
		}
	}
	return out
}

// checkSubscriptionSuccessRate 检查订阅成功率并发出警告
func (pc *ProxyChecker) checkSubscriptionSuccessRate(subTotals map[string]int) {
	// 统计每个订阅的节点总数和成功数
	subStats := make(map[string]struct {
		total   int
		success int
	})

	// 各订阅的节点总数在轮次开始前已统计好
	for subUrl, total := range subTotals {
		subStats[subUrl] = struct {
			total   int
			success int
		}{total: total}
	}

	// 统计成功节点的订阅来源
	for _, result := range pc.results {
		if result.Proxy != nil {
			if subUrl, ok := result.Proxy["sub_url"].(string); ok {
				stats := subStats[subUrl]
				stats.success++
				subStats[subUrl] = stats
			}
			delete(result.Proxy, "sub_url")
			// 可以保持127.0.0.1:8199/sub/all.yaml中的节点tag
			if subTag, ok := result.Proxy["sub_tag"].(string); ok {
				if subTag == "" {
					delete(result.Proxy, "sub_tag")
				}
			}
		}
	}

	// 检查成功率并发出警告
	for subUrl, stats := range subStats {
		if stats.total > 0 {
			successRate := float32(stats.success) / float32(stats.total)

			// 如果成功率低于x，发出警告
			if successRate < config.GlobalConfig.SuccessRate {
				slog.Warn(fmt.Sprintf("订阅成功率过低: %s", subUrl),
					"总节点数", stats.total,
					"成功节点数", stats.success,
					"成功占比", fmt.Sprintf("%.2f%%", successRate*100))
			} else {
				slog.Debug(fmt.Sprintf("订阅节点统计: %s", subUrl),
					"总节点数", stats.total,
					"成功节点数", stats.success,
					"成功占比", fmt.Sprintf("%.2f%%", successRate*100))
			}
		}
	}
}

// statsConn wraps net.Conn to count bytes read and apply rate limiting
type statsConn struct {
	net.Conn
	bytesRead *uint64
	bucket    *ratelimit.Bucket
}

func (c *statsConn) Read(b []byte) (n int, err error) {
	// 速度限制（全局）
	if c.bucket != nil {
		c.bucket.Wait(int64(len(b)))
	}

	n, err = c.Conn.Read(b)
	atomic.AddUint64(c.bytesRead, uint64(n))

	return n, err
}

// CreateClient creates and returns an http.Client with a Close function
type ProxyClient struct {
	*http.Client
	proxy     constant.Proxy
	BytesRead *uint64

	ctx    context.Context // done on Close, stops watchdog
	cancel context.CancelFunc
}

func CreateClient(mapping map[string]any) *ProxyClient {
	return CreateClientWithTimeout(mapping, config.GlobalConfig.Timeout)
}

// CreateClientWithTimeout 创建带指定请求超时（毫秒）的代理客户端。
func CreateClientWithTimeout(mapping map[string]any, timeoutMs int) *ProxyClient {
	proxy, err := adapter.ParseProxy(mapping)
	if err != nil {
		slog.Debug("创建mihomo Client失败", "proxy", mapping["name"], "err", err)
		return nil
	}

	var bytesRead uint64
	baseTransport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			var u16Port uint16
			if port, err := strconv.ParseUint(port, 10, 16); err == nil {
				u16Port = uint16(port)
			}
			conn, err := proxy.DialContext(ctx, &constant.Metadata{
				Host:    host,
				DstPort: u16Port,
			})
			if err != nil {
				return nil, err
			}
			return &statsConn{
				Conn:      conn,
				bytesRead: &bytesRead,
				bucket:    Bucket,
			}, nil
		},
		DisableKeepAlives: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	pc := &ProxyClient{
		Client: &http.Client{
			Timeout:   time.Duration(timeoutMs) * time.Millisecond,
			Transport: baseTransport,
		},
		proxy:     proxy,
		BytesRead: &bytesRead,
		ctx:       ctx,
		cancel:    cancel,
	}
	go pc.watchdog(mapping["name"], clientLifetime())
	return pc
}

// clientLifetime is a hang backstop, not a request timeout:
// 10x the largest per-request timeout, at least 10 minutes.
func clientLifetime() time.Duration {
	d := time.Duration(config.GlobalConfig.Timeout) * time.Millisecond
	d = max(d, time.Duration(config.GlobalConfig.MediaCheckTimeout)*time.Second)
	d = max(d, time.Duration(config.GlobalConfig.DownloadTimeout)*time.Second)
	return max(10*d, 10*time.Minute)
}

// watchdog closes the proxy every second after lifetime until Close is called.
// Some transports ignore request timeouts (e.g. mihomo gun stuck in initOnce)
// and gun rebuilds its transport after Close, so one Close is not enough.
func (pc *ProxyClient) watchdog(name any, lifetime time.Duration) {
	timer := time.NewTimer(lifetime)
	defer timer.Stop()
	select {
	case <-pc.ctx.Done():
		return
	case <-timer.C:
	}

	slog.Warn("节点检测超时仍未结束，强制关闭底层连接", "proxy", name)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		pc.proxy.Close()
		select {
		case <-pc.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Close closes the proxy client and cleans up resources
// 防止底层库有一些泄露，所以这里手动关闭
func (pc *ProxyClient) Close() {
	if pc.cancel != nil {
		pc.cancel()
	}

	if pc.Client != nil {
		pc.Client.CloseIdleConnections()
	}

	// 即使这里不关闭，底层GC的时候也会自动关闭
	// 这里及时的关闭，方便内存回收
	// 某些底层传输协议的 Close 可能阻塞，超时后放弃等待交由 GC 回收
	if pc.proxy != nil {
		proxy := pc.proxy
		done := make(chan struct{})
		go func() {
			proxy.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			slog.Debug(fmt.Sprintf("关闭代理连接超时，交由GC回收: %v", proxy))
		}
	}
	pc.Client = nil

	if pc.BytesRead != nil {
		TotalBytes.Add(atomic.LoadUint64(pc.BytesRead))
	}
}

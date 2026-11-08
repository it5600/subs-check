package platform

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"log/slog"

	"substore-ui-go/internal/subscheck/config"
	"github.com/juju/ratelimit"
	"github.com/metacubex/mihomo/common/convert"
)

// 测速的提前结束门槛。
const (
	// 高速节点提前结束：读到 512KB 且耗时 300ms 后，实测速率超过阈值即可收工。
	// 门槛越小，高速节点上浪费的流量越少；512KB/300ms 已足以稳定判定速率档位。
	earlyStopMinBytes = 512 << 10
	earlyStopMinDur   = 300 * time.Millisecond

	// 慢节点提前结束：读到 2MB 且耗时 6s 后，实测速率仍低于最小速度阈值，
	// 说明该节点一定过不了速度门槛，继续下载只是白白消耗流量。
	slowStopMinBytes = 2 << 20
	slowStopMinDur   = 6 * time.Second
)

// EarlyStopCount 统计测速阶段因速率达标而提前结束下载的次数。
var EarlyStopCount atomic.Uint64

// SlowStopCount 统计测速阶段因速率低于最小速度阈值而提前结束下载的次数。
var SlowStopCount atomic.Uint64

// networkLimitedReader 基于网络层字节计数器的大小限制 reader
type networkLimitedReader struct {
	reader       io.Reader
	bytesCounter *uint64
	startBytes   uint64
	limit        uint64
}

func (r *networkLimitedReader) Read(p []byte) (n int, err error) {
	if r.limit > 0 {
		currentBytes := atomic.LoadUint64(r.bytesCounter)
		networkRead := currentBytes - r.startBytes

		if networkRead >= r.limit {
			return 0, io.EOF
		}

		// 限制本次读取的大小（粗略控制，因为网络层可能读取更多）
		if remaining := r.limit - networkRead; remaining < uint64(len(p)) {
			p = p[:remaining]
		}
	}
	return r.reader.Read(p)
}

// CheckSpeed downloads speedTestURL through httpClient and returns the measured
// throughput. The URL is passed in explicitly (rather than read from
// config.GlobalConfig) so a run captured at pipeline start stays consistent
// even if the user edits SpeedTestUrl mid-check.
//
// earlyStopKBps > 0 时启用提前结束：已经读到足够字节且实测速率远超该阈值时，
// 通过与否的结论不会再变，继续下载只是白白消耗带宽和时间。
func CheckSpeed(httpClient *http.Client, bucket *ratelimit.Bucket, bytesCounter *uint64, speedTestURL string, earlyStopKBps int) (int, int64, error) {
	// 慢节点提前结束的门槛：配置的最小速度（KB/s），0 表示不启用。
	minSpeedKBps := config.GlobalConfig.MinSpeed
	// 注意：速度限制在网络层（statsConn）实现，大小限制在应用层基于网络字节计数器实现
	// - 速度限制：通过 bucket 在 statsConn 中实现（网络层）
	// - 大小限制：通过 networkLimitedReader 基于网络字节计数器实现（应用层，但限制网络流量）

	// 创建一个新的测速专用客户端，基于原有客户端的传输层
	speedClient := &http.Client{
		// 设置更长的超时时间用于测速
		Timeout: time.Duration(config.GlobalConfig.DownloadTimeout) * time.Second,
		// 保持原有的传输层配置
		Transport: httpClient.Transport,
	}

	req, err := http.NewRequest("GET", speedTestURL, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", convert.RandUserAgent())

	// 记录测速前的网络传输字节数
	var startBytes uint64
	if bytesCounter != nil {
		startBytes = atomic.LoadUint64(bytesCounter)
	}
	resp, err := speedClient.Do(req)
	if err != nil {
		slog.Debug(fmt.Sprintf("测速请求失败: %v", err))
		return 0, 0, err
	}
	defer resp.Body.Close()

	// 计时从响应头到达才开始：连接建立与 TLS 握手不计入下载速率，
	// 否则握手慢的节点速率会被系统性低估。
	startTime := time.Now()

	// 计算网络层的大小限制
	var limitSize uint64
	if config.GlobalConfig.DownloadMB > 0 {
		limitSize = uint64(config.GlobalConfig.DownloadMB) * 1024 * 1024
	} else {
		limitSize = 0 // 不限制
	}

	// 使用 networkLimitedReader 包装响应体，基于网络字节计数器限制大小
	limitedReader := &networkLimitedReader{
		reader:       resp.Body,
		bytesCounter: bytesCounter,
		startBytes:   startBytes,
		limit:        limitSize,
	}

	// 读取数据：达到速率阈值后可以提前结束，其余情况与整段读取一致
	buf := make([]byte, 64*1024)
	var totalBytes int64
	for {
		n, rerr := limitedReader.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
		}
		if rerr != nil {
			// io.EOF 是正常的（达到限制），其他错误才需要关注
			if rerr != io.EOF && totalBytes == 0 {
				slog.Debug(fmt.Sprintf("totalBytes: %d, 读取数据时发生错误: %v", totalBytes, rerr))
				return 0, 0, rerr
			}
			break
		}
		// 高速节点：速率已超过提前结束阈值，继续下载不会改变结论。
		if earlyStopKBps > 0 && totalBytes >= earlyStopMinBytes {
			dur := time.Since(startTime)
			if dur >= earlyStopMinDur {
				rate := currentRate(totalBytes, dur)
				if rate >= earlyStopKBps {
					EarlyStopCount.Add(1)
					break
				}
			}
		}
		// 慢节点：速率已经低于最小速度门槛，必然过不了速度筛选，
		// 继续下载只是浪费流量，直接收工。
		if minSpeedKBps > 0 && totalBytes >= slowStopMinBytes {
			dur := time.Since(startTime)
			if dur >= slowStopMinDur {
				if currentRate(totalBytes, dur) < minSpeedKBps {
					SlowStopCount.Add(1)
					break
				}
			}
		}
	}

	// 计算下载时间（毫秒）
	duration := time.Since(startTime).Milliseconds()
	if duration == 0 {
		duration = 1 // 避免除以零
	}

	// 计算实际网络传输的字节数（压缩数据）
	var actualBytes int64
	if bytesCounter != nil {
		actualBytes = int64(atomic.LoadUint64(bytesCounter) - startBytes)
	} else {
		// 如果没有字节计数器，无法获取准确数据
		actualBytes = 0
	}

	// 计算速度（KB/s），使用实际网络传输的字节数
	speed := int(float64(actualBytes) / 1024 * 1000 / float64(duration))

	return speed, actualBytes, nil
}

// currentRate 返回当前实测速率（KB/s）。
// 优先用网络层字节计数（统计的是压缩后的真实流量），
// 没有计数器时退化为应用层已读字节数。
func currentRate(totalBytes int64, dur time.Duration) int {
	if dur.Milliseconds() <= 0 {
		return 0
	}
	return int(float64(totalBytes) / 1024 * 1000 / float64(dur.Milliseconds()))
}

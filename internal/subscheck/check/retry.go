package check

// 媒体检测瞬时故障重试（移植自社区项目 subs-check-pro 的 isRetryable/withRetry，
// 净室改写适配本项目：只重试网络层瞬时故障，指数退避 + 随机抖动）。

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"syscall"
	"time"
)

// mediaRetryMax 首次尝试失败后的额外重试次数（总尝试次数 = mediaRetryMax + 1）。
const mediaRetryMax = 2

// isRetryableErr 判断错误是否值得重试（仅网络层瞬时故障）。
func isRetryableErr(err error) bool {
	if err == nil {
		return false
	}
	// 明确不重试：context 被外部取消（超出全局限制等）
	if errors.Is(err, context.Canceled) {
		return false
	}
	// 重试：超时（含客户端自身的 Timeout）
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}

	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		switch sysErr.Err {
		case syscall.ECONNRESET, syscall.EPIPE, syscall.ETIMEDOUT, syscall.ECONNREFUSED:
			return true
		}
	}

	return false
}

// withMediaRetry 包装媒体平台检测调用：仅在瞬时网络错误时重试。
// 退避 = attempt²×200ms + 随机抖动（0~300ms），避免大量任务同一时刻重试。
// ctx 取消时立即放弃等待并返回。
func withMediaRetry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt <= mediaRetryMax; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt*attempt)*200*time.Millisecond +
				time.Duration(rand.Int64N(int64(300*time.Millisecond)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		err = fn()
		if !isRetryableErr(err) {
			return err // 成功或明确失败，不重试
		}
	}
	return err
}

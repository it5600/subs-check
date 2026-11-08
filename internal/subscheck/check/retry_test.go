package check

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "i/o timeout" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return false }

func TestIsRetryableErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"普通错误不重试", errors.New("bad response"), false},
		{"外部取消不重试", context.Canceled, false},
		{"超时可重试", context.DeadlineExceeded, true},
		{"EOF 截断可重试", io.ErrUnexpectedEOF, true},
		{"net.Error 超时可重试", fakeTimeoutErr{}, true},
		{"连接重置可重试", &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}, true},
		{"权限错误不重试", &os.SyscallError{Syscall: "open", Err: syscall.EACCES}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableErr(tc.err); got != tc.want {
				t.Fatalf("isRetryableErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestWithMediaRetry_SucceedsImmediately(t *testing.T) {
	calls := 0
	err := withMediaRetry(context.Background(), func() error {
		calls++
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("成功时不应重试: err=%v calls=%d", err, calls)
	}
}

func TestWithMediaRetry_EventuallySucceeds(t *testing.T) {
	calls := 0
	err := withMediaRetry(context.Background(), func() error {
		calls++
		if calls < 3 {
			return context.DeadlineExceeded
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("瞬时故障应重试至成功: err=%v calls=%d", err, calls)
	}
}

func TestWithMediaRetry_GivesUpAfterLimit(t *testing.T) {
	calls := 0
	err := withMediaRetry(context.Background(), func() error {
		calls++
		return io.ErrUnexpectedEOF
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("应返回最后一次错误, got %v", err)
	}
	if calls != mediaRetryMax+1 {
		t.Fatalf("总尝试次数应为 %d, got %d", mediaRetryMax+1, calls)
	}
}

func TestWithMediaRetry_NonRetryableStopsImmediately(t *testing.T) {
	calls := 0
	want := errors.New("permanent")
	err := withMediaRetry(context.Background(), func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("非瞬时错误不应重试: err=%v calls=%d", err, calls)
	}
}

func TestWithMediaRetry_CancelledContextAbortsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	start := time.Now()
	err := withMediaRetry(ctx, func() error {
		calls++
		cancel() // 第一次失败后立刻取消，退避等待应被中断
		return context.DeadlineExceeded
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应返回 context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("取消后不应再次调用, got %d 次", calls)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("取消应立即放弃退避, took %s", elapsed)
	}
}

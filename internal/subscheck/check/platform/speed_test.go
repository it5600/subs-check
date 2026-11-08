package platform

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"substore-ui-go/internal/subscheck/config"
)

// countingTransport 统计网络层读取的字节数，模拟 mihomo 的流量计数器
type countingTransport struct {
	base    http.RoundTripper
	counter *uint64
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	resp.Body = &countingBody{ReadCloser: resp.Body, counter: t.counter}
	return resp, nil
}

type countingBody struct {
	io.ReadCloser
	counter *uint64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		atomic.AddUint64(b.counter, uint64(n))
	}
	return n, err
}

// chunkServer 按固定节奏输出固定大小的数据块
func chunkServer(total int, chunk int, interval time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "104857600")
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, chunk)
		for sent := 0; sent < total; sent += chunk {
			if _, err := w.Write(buf); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(interval)
		}
	}))
}

// TestCheckSpeed_EarlyStop 高速节点应在达到阈值后提前结束，不下载完整文件
func TestCheckSpeed_EarlyStop(t *testing.T) {
	srv := chunkServer(8<<20, 256<<10, 60*time.Millisecond)
	defer srv.Close()
	var counter uint64
	client := &http.Client{Transport: &countingTransport{base: http.DefaultTransport, counter: &counter}}
	speed, bytes, err := CheckSpeed(client, nil, &counter, srv.URL, 800)
	if err != nil {
		t.Fatalf("测速失败: %v", err)
	}
	if speed < 800 {
		t.Fatalf("高速节点速率应超过阈值, got %d KB/s", speed)
	}
	if bytes >= 8<<20 {
		t.Fatalf("高速节点应提前结束, 下载了 %d 字节", bytes)
	}
	if EarlyStopCount.Load() == 0 {
		t.Fatalf("提前结束计数应递增")
	}
}

// TestCheckSpeed_SlowStop 速率低于最小速度阈值的节点应提前结束下载
func TestCheckSpeed_SlowStop(t *testing.T) {
	old := config.GlobalConfig.MinSpeed
	config.GlobalConfig.MinSpeed = 2000 // 1MB/s 的节点低于该阈值
	defer func() { config.GlobalConfig.MinSpeed = old }()

	srv := chunkServer(16<<20, 1<<20, time.Second)
	defer srv.Close()
	var counter uint64
	client := &http.Client{Transport: &countingTransport{base: http.DefaultTransport, counter: &counter}}
	speed, bytes, err := CheckSpeed(client, nil, &counter, srv.URL, 0)
	if err != nil {
		t.Fatalf("测速失败: %v", err)
	}
	if speed >= config.GlobalConfig.MinSpeed {
		t.Fatalf("慢节点速率应低于阈值, got %d KB/s", speed)
	}
	if bytes >= 16<<20 {
		t.Fatalf("慢节点应提前结束, 下载了 %d 字节", bytes)
	}
	if SlowStopCount.Load() == 0 {
		t.Fatalf("慢节点提前结束计数应递增")
	}
}

// TestCurrentRate 速率换算：KB/s
func TestCurrentRate(t *testing.T) {
	if got := currentRate(1024*1024, time.Second); got != 1024 {
		t.Fatalf("1MB/s 应为 1024 KB/s, got %d", got)
	}
	if got := currentRate(512*1024, 500*time.Millisecond); got != 1024 {
		t.Fatalf("512KB/0.5s 应为 1024 KB/s, got %d", got)
	}
	if got := currentRate(1024, 0); got != 0 {
		t.Fatalf("耗时为 0 时速率应为 0, got %d", got)
	}
}

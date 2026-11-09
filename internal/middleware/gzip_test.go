package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 1.4.11 回归：旧 GzipCompress 在调用 next 之前读 Content-Type（那时头部必为空），
// 于是所有响应都被无条件 gzip，且透传了上游 Content-Length。
// http.ServeContent 的 Range/206、If-None-Match/304 会被破坏，图片字体也白耗 CPU。

func serveGzipped(t *testing.T, accept string, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	w := httptest.NewRecorder()
	GzipCompress(h).ServeHTTP(w, req)
	return w
}

func gunzip(t *testing.T, body []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("响应体不是合法 gzip: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	return string(out)
}

func TestGzip_NotAccepted_PassesThrough(t *testing.T) {
	const payload = `{"ok":true}`
	w := serveGzipped(t, "", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(payload))
	})
	if w.Header().Get("Content-Encoding") != "" {
		t.Fatal("客户端未声明 gzip 时不应压缩")
	}
	if w.Body.String() != payload {
		t.Fatalf("响应体被改动: %q", w.Body.String())
	}
}

func TestGzip_JSONIsCompressedAndLengthDropped(t *testing.T) {
	const payload = `{"nodes":[1,2,3],"total":3}`
	w := serveGzipped(t, "gzip", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.Header().Set("Content-Length", "28")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(payload))
	})
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("JSON 响应应被压缩, got %q", got)
	}
	if got := w.Header().Get("Content-Length"); got != "" {
		t.Fatalf("压缩后必须删除 Content-Length, got %q", got)
	}
	if got := gunzip(t, w.Body.Bytes()); got != payload {
		t.Fatalf("解压后内容不一致: %q", got)
	}
}

func TestGzip_SkipsAlreadyCompressedTypes(t *testing.T) {
	for _, ct := range []string{"image/png", "image/svg+xml", "font/woff2", "application/zip", "video/mp4"} {
		t.Run(ct, func(t *testing.T) {
			const payload = "BINARY-ish-payload"
			w := serveGzipped(t, "gzip", func(rw http.ResponseWriter, _ *http.Request) {
				rw.Header().Set("Content-Type", ct)
				_, _ = rw.Write([]byte(payload))
			})
			if w.Header().Get("Content-Encoding") == "gzip" {
				t.Fatalf("%s 不应被 gzip 压缩", ct)
			}
			if w.Body.String() != payload {
				t.Fatalf("%s 响应体被改动: %q", ct, w.Body.String())
			}
		})
	}
}

func TestGzip_PreservesConditionalAndPartialResponses(t *testing.T) {
	for _, code := range []int{http.StatusNotModified, http.StatusPartialContent, http.StatusNoContent} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			w := serveGzipped(t, "gzip", func(rw http.ResponseWriter, _ *http.Request) {
				rw.Header().Set("Content-Type", "application/json")
				rw.Header().Set("Content-Length", "123")
				rw.Header().Set("ETag", `"abc"`)
				rw.WriteHeader(code)
			})
			if w.Header().Get("Content-Encoding") == "gzip" {
				t.Fatalf("状态 %d 不应被压缩", code)
			}
			if got := w.Header().Get("Content-Length"); got != "123" {
				t.Fatalf("状态 %d 的 Content-Length 必须保留, got %q", code, got)
			}
			if w.Header().Get("ETag") != `"abc"` {
				t.Fatalf("状态 %d 的 ETag 必须保留", code)
			}
		})
	}
}

func TestGzip_EmptyHandlerStillReturns200(t *testing.T) {
	w := serveGzipped(t, "gzip", func(http.ResponseWriter, *http.Request) {})
	if w.Code != http.StatusOK && w.Code != 0 {
		t.Fatalf("空响应应为 200, got %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("空响应不应产生正文, got %q", w.Body.String())
	}
}

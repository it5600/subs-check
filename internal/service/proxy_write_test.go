package service

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// 1.4.11 回归：WriteConfigFile 原先用固定的 path+".tmp" 且权限 0644。
// 并发的「添加/删除订阅源」会写同一个临时文件导致内容交叉，配置文件里的
// github-token / webdav-password 等还会是全局可读。

func TestWriteConfigFile_AtomicUniqueTempAndPrivateMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "sub-urls:\n  - https://example.com/a\n"

	if err := WriteConfigFile(path, content); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取结果失败: %v", err)
	}
	if string(got) != content {
		t.Fatalf("内容不一致: %q", string(got))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 不实现 Unix 权限位，os.Stat().Mode().Perm() 恒为 0666；
	// 只在 Unix 上断言 0600（fnOS/Linux 才是本次权限修复的目标平台）。
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("配置文件权限应为 0600, got %o", info.Mode().Perm())
	}

	// 覆盖写必须成功，且目录里不能留下 .tmp 残留。
	if err := WriteConfigFile(path, content+"  - https://example.com/b\n"); err != nil {
		t.Fatalf("覆盖写失败: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("不应残留临时文件, 目录内容: %v", entries)
	}
}

func TestWriteConfigFile_ConcurrentWritersDoNotInterleave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 每个 writer 写 8KB 由单一两位数字重复组成的内容；
			// 若临时文件被共享，结果必然混入其它数字。
			if err := WriteConfigFile(path, strings.Repeat(fmt.Sprintf("%02d", i), 4096)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发写入报错: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8192 {
		t.Fatalf("最终文件长度异常: %d", len(got))
	}
	for i := 2; i < len(got); i += 2 {
		if got[i] != got[0] || got[i+1] != got[1] {
			t.Fatalf("文件内容发生交叉写入，位置 %d: %q", i, got[i:i+2])
		}
	}
}

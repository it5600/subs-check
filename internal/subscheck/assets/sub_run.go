package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"substore-ui-go/internal/subscheck/config"
	"substore-ui-go/internal/subscheck/save/method"
	"github.com/klauspost/compress/zstd"
	"github.com/shirou/gopsutil/v4/process"
	"gopkg.in/natefinch/lumberjack.v2"
)

// subStoreProcess tracks the running Sub-Store backend so the app can stop
// it on shutdown.
var (
	subStoreMu   sync.Mutex
	subStoreCmd  *exec.Cmd
	subStoreNode string
	subStoreJS   string
	// subStoreStopping 标记应用正在退出：守护循环看到它就安静退出，
	// 不再把"我们自己杀掉的子进程"记成崩溃并触发重启。
	subStoreStopping atomic.Bool
)

// StopSubStore terminates the embedded Sub-Store backend.
//
// The app used to exit without stopping the node process: main.go called
// os.Exit(0) after the HTTP shutdown, so the deferred cleanup never ran and
// the node kept port 8298 occupied. The next start then died with
// EADDRINUSE and the supervisor restarted in a loop (2127 restarts logged
// between 2026-10-29 22:00 and 2026-10-30 19:51).
func StopSubStore() {
	subStoreStopping.Store(true)
	slog.Info("正在停止 sub-store 服务")
	subStoreMu.Lock()
	cmd := subStoreCmd
	node, js := subStoreNode, subStoreJS
	subStoreCmd = nil
	subStoreMu.Unlock()

	if cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Kill(); err != nil {
			slog.Debug("停止 sub-store 子进程失败", "error", err)
		} else {
			slog.Info("已停止 sub-store 子进程", "pid", cmd.Process.Pid)
		}
	}

	// 兜底：清理本安装目录下残留的进程（cluster worker 会继续占用端口）
	if node != "" && js != "" {
		if pids, err := findSubStoreProcesses(node, js); err == nil {
			for _, pid := range pids {
				if err := killProcess(pid); err != nil {
					slog.Debug("清理残留 sub-store 进程失败", "pid", pid, "error", err)
				} else {
					slog.Info("已清理残留 sub-store 进程", "pid", pid)
				}
			}
		}
	}
}

// subStorePaths 返回本安装目录下的 node / bundle / 覆写文件 / 日志路径。
func subStorePaths(outputDir string) (nodePath, jsPath, overYamlPath, logPath string) {
	nodeName := "node"
	if runtime.GOOS == "windows" {
		nodeName += ".exe"
	}
	nodePath = filepath.Join(outputDir, nodeName)
	jsPath = filepath.Join(outputDir, "sub-store.bundle.js")
	overYamlPath = filepath.Join(outputDir, "ACL4SSR_Online_Full.yaml")
	logPath = filepath.Join(outputDir, "sub-store.log")

	// 支持自定义node二进制文件路径，可兼容更多的设备
	if v := os.Getenv("NODEBIN_PATH"); v != "" {
		nodePath = v
	}
	// 支持自定义sub-store脚本路径
	if v := os.Getenv("SUB_STORE_PATH"); v != "" {
		jsPath = v
	}
	return
}

// subStoreOutputDir 返回本安装实例的产出目录。
func subStoreOutputDir() string {
	saver, err := method.NewLocalSaver()
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(saver.OutputPath) {
		saver.OutputPath = filepath.Join(saver.BasePath, saver.OutputPath)
	}
	return saver.OutputPath
}

// RunSubStoreService supervises the embedded Sub-Store backend and restarts
// it whenever it exits.
//
// Two safeguards were added after a crash storm on 2026-10-29/30 (more than
// 1500 restarts in eight hours, one every ~15s):
//
//  1. Before starting, probe the configured port. If an instance is already
//     serving it, reuse it instead of starting a second one. Two app
//     instances used to kill each other's service and never converged.
//  2. Exponential backoff after a crash, so a service that keeps dying
//     cannot spin at a fixed cadence and flood the log.
func RunSubStoreService() {
	// 先记住本安装目录的 node/bundle 路径：即使本实例没有自己拉起 node（
	// 如果端口已有健康实例在服务），退出时也能清理残留进程。
	if dir := subStoreOutputDir(); dir != "" {
		nodePath, jsPath, _, _ := subStorePaths(dir)
		subStoreMu.Lock()
		subStoreNode = nodePath
		subStoreJS = jsPath
		subStoreMu.Unlock()
	}

	delay := 30 * time.Second
	for {
		if subStoreStopping.Load() {
			slog.Info("sub-store 守护循环随应用退出")
			return
		}
		if healthy, err := subStoreHealthy(); err == nil && healthy {
			slog.Debug("检测到已有 sub-store 实例占用端口，跳过启动")
			time.Sleep(delay)
			continue
		}

		started := time.Now()
		err := startSubStore()
		ran := time.Since(started)
		if err != nil {
			if subStoreStopping.Load() {
				slog.Info("sub-store 服务已随应用退出，不再重启")
				return
			}
			slog.Error("Sub-store service crashed, restarting...", "error", err, "retryIn", delay.String())
			if ran > time.Minute {
				// 之前跑得够久，说明不是崩溃循环，退避回到基准值
				delay = 30 * time.Second
			} else {
				delay = min(delay*2, 5*time.Minute)
			}
			time.Sleep(delay)
			continue
		}
		delay = 30 * time.Second
		time.Sleep(delay)
	}
}

// subStoreHealthy reports whether a Sub-Store instance is already serving
// the configured port. Any HTTP response counts as alive.
func subStoreHealthy() (bool, error) {
	port := config.GlobalConfig.SubStorePort
	if port == "" {
		return false, nil
	}
	if idx := strings.LastIndex(port, ":"); idx >= 0 {
		port = port[idx+1:]
	}
	port = strings.TrimSpace(port)
	if port == "" {
		return false, nil
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return true, nil
}

func startSubStore() error {
	saver, err := method.NewLocalSaver()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(saver.OutputPath) {
		// 处理用户写相对路径的问题
		saver.OutputPath = filepath.Join(saver.BasePath, saver.OutputPath)
	}
	os.MkdirAll(saver.OutputPath, 0755)
	nodePath, jsPath, overYamlPath, logPath := subStorePaths(saver.OutputPath)

	killNode := func() {
		// Kill the Sub-Store processes of THIS installation to free the port.
		// Earlier versions killed every sub-store.bundle.js process on the
		// machine, which made two app instances kill each other's service
		// (1500+ crashes in 8 hours on 2026-10-29/30).
		pids, err := findSubStoreProcesses(nodePath, jsPath)
		if err != nil {
			slog.Debug("Sub-store service kill: no processes found", "error", err)
			return
		}
		for _, pid := range pids {
			if err := killProcess(pid); err != nil {
				slog.Debug("Sub-store service kill failed", "error", err, "pid", pid)
			} else {
				slog.Debug("Sub-store service killed", "pid", pid)
			}
		}
	}
	defer killNode()

	// 以前每次启动都删掉再解压 node：内存问题退出会留下损坏的 node，重解压能兜住。
	// 但每次解压 45MB 的嵌入资源会把进程内存峰值顶到 190MB（实测 T+1s 即达 189MB），
	// 机械盘上还要写 120MB。改成：内嵌资源指纹一致且上次解压产物完好时跳过解压。
	if assetsReady(saver.OutputPath, nodePath, jsPath, overYamlPath) {
		slog.Info("sub-store 运行文件与内嵌资源一致，跳过解压")
	} else {
		os.Remove(nodePath)
		os.Remove(jsPath)
		os.Remove(overYamlPath)
		if err := decodeZstd(nodePath, jsPath, overYamlPath); err != nil {
			return err
		}
		if err := writeAssetsStamp(saver.OutputPath, nodePath, jsPath, overYamlPath); err != nil {
			slog.Warn("写入资源指纹失败", "error", err)
		}
	}

	// 配置日志轮转
	logWriter := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    10, // 每个日志文件最大 10MB
		MaxBackups: 3,  // 保留 3 个旧文件
		MaxAge:     7,  // 保留 7 天
	}
	defer logWriter.Close()

	// 运行 JavaScript 文件
	cmd := exec.Command(nodePath, jsPath)
	// js会在运行目录释放依赖文件
	cmd.Dir = saver.OutputPath
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	cmd.Env = os.Environ()

	// 检查MihomoOverwriteUrl是否包含本地IP，如果是则移除代理环境变量
	cleanProxyEnv := false
	if config.GlobalConfig.MihomoOverwriteUrl != "" {
		parsedURL, err := url.Parse(config.GlobalConfig.MihomoOverwriteUrl)
		if err == nil {
			host := parsedURL.Hostname()
			if isLocalIP(host) {
				cleanProxyEnv = true
				slog.Debug("MihomoOverwriteUrl contains local IP, removing proxy environment variables")
			}
		}
	}

	// ipv4/ipv6 都支持
	hostPort := strings.Split(config.GlobalConfig.SubStorePort, ":")
	// host可以为空，port不能为空
	if len(hostPort) == 2 && hostPort[1] != "" {
		cmd.Env = append(cmd.Env,
			fmt.Sprintf("SUB_STORE_BACKEND_API_HOST=%s", hostPort[0]),
			fmt.Sprintf("SUB_STORE_BACKEND_API_PORT=%s", hostPort[1]),
		)
	} else if len(hostPort) == 1 {
		cmd.Env = append(cmd.Env, fmt.Sprintf("SUB_STORE_BACKEND_API_PORT=%s", hostPort[0])) // 设置端口
	} else {
		return fmt.Errorf("sub-store-port invalid port format: %s", config.GlobalConfig.SubStorePort)
	}

	// https://hub.docker.com/r/xream/sub-store
	// 这里有详细的变量说明，可能用NO_PROXY过滤到127.0.0.1更合适
	// 如果MihomoOverwriteUrl包含本地IP，则移除所有代理环境变量
	if cleanProxyEnv {
		filteredEnv := make([]string, 0, len(cmd.Env))
		proxyVars := []string{"http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"}

		for _, env := range cmd.Env {
			isProxyVar := false
			for _, proxyVar := range proxyVars {
				if strings.HasPrefix(strings.ToLower(env), strings.ToLower(proxyVar)+"=") {
					isProxyVar = true
					break
				}
			}
			if !isProxyVar {
				filteredEnv = append(filteredEnv, env)
			}
		}
		cmd.Env = filteredEnv
	}

	// 增加body限制，默认1M
	if os.Getenv("SUB_STORE_BODY_JSON_LIMIT") == "" {
		cmd.Env = append(cmd.Env, "SUB_STORE_BODY_JSON_LIMIT=30mb")
	}
	// 增加自定义访问路径
	if config.GlobalConfig.SubStorePath != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("SUB_STORE_FRONTEND_BACKEND_PATH=%s", config.GlobalConfig.SubStorePath))
		cmd.Env = append(cmd.Env, "SUB_STORE_BACKEND_MERGE=1")
	} else if os.Getenv("SUB_STORE_FRONTEND_BACKEND_PATH") == "" {
		// sub-store 2.38.0 起，Node.js 下未设置 SUB_STORE_FRONTEND_BACKEND_PATH 时脚本操作会直接报错，
		// 而 mihomo 覆写用的就是 Script Operator。未开启 MERGE/PREFIX 时后端不处理 path，设成 / 不影响现有访问路径
		cmd.Env = append(cmd.Env, "SUB_STORE_FRONTEND_BACKEND_PATH=/")
	}
	// sub-store 2.38.0 起 Node.js 下 CORS 默认只放行官方前端，自建前端/其它来源会被拒绝，保持以前的 * 行为
	if os.Getenv("SUB_STORE_CORS_ALLOWED_ORIGINS") == "" {
		cmd.Env = append(cmd.Env, "SUB_STORE_CORS_ALLOWED_ORIGINS=*")
	}

	// sub-store 环境变量: 后端上传文件至 gist
	if config.GlobalConfig.SubStoreSyncCron != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("SUB_STORE_BACKEND_SYNC_CRON=%s", config.GlobalConfig.SubStoreSyncCron))
	}

	// sub-store 环境变量: 自动拉取订阅内容
	if config.GlobalConfig.SubStoreProduceCron != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("SUB_STORE_PRODUCE_CRON=%s", config.GlobalConfig.SubStoreProduceCron))
	}

	// sub-store 环境变量: 当遇到错误时发送通知
	if config.GlobalConfig.SubStorePushService != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("SUB_STORE_PUSH_SERVICE=%s", config.GlobalConfig.SubStorePushService))
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 sub-store 失败: %w", err)
	}

	// 记录当前子进程，退出时（StopSubStore）可以主动结束它
	subStoreMu.Lock()
	subStoreCmd = cmd
	subStoreNode = nodePath
	subStoreJS = jsPath
	subStoreMu.Unlock()
	defer func() {
		subStoreMu.Lock()
		if subStoreCmd == cmd {
			subStoreCmd = nil
		}
		subStoreMu.Unlock()
	}()

	slog.Info("Sub-store service started", "pid", cmd.Process.Pid, "port", config.GlobalConfig.SubStorePort, "log", logPath)

	// 等待程序结束
	return cmd.Wait()
}

// isLocalIP 检查IP是否是本地IP（127.0.0.1或局域网IP）
func isLocalIP(host string) bool {
	// 检查是否是localhost或127.0.0.1
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}

	// 检查IP是否有效
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	// 检查是否是私有IP范围
	privateIPBlocks := []string{
		"10.0.0.0/8",     // 10.0.0.0 - 10.255.255.255
		"172.16.0.0/12",  // 172.16.0.0 - 172.31.255.255
		"192.168.0.0/16", // 192.168.0.0 - 192.168.255.255
		"169.254.0.0/16", // 169.254.0.0 - 169.254.255.255
		"fd00::/8",       // fd00:: - fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff
	}

	for _, block := range privateIPBlocks {
		_, ipNet, err := net.ParseCIDR(block)
		if err != nil {
			continue
		}
		if ipNet.Contains(ip) {
			return true
		}
	}

	return false
}

const (
	// assetsStampName 记录上次解压时的资源指纹，用于跳过重复解压。
	assetsStampName = "assets.stamp"
	// fingerprintChunk 参与指纹计算的头尾字节数。
	fingerprintChunk = 64 << 10
)

// assetsFingerprint 计算内嵌资源的指纹：长度 + 头尾各 64KB 的哈希。
// 只采样头尾，避免为了算指纹把 45MB 的嵌入资源整块读进内存
// —— 那正是启动期内存峰值的主要来源。
func assetsFingerprint() string {
	h := sha256.New()
	fingerprintAsset(h, EmbeddedNode)
	fingerprintAsset(h, EmbeddedSubStore)
	fingerprintAsset(h, EmbeddedOverrideYaml)
	return hex.EncodeToString(h.Sum(nil))
}

// fingerprintAsset 把资源长度与头尾字节写入哈希。
func fingerprintAsset(h hash.Hash, b []byte) {
	h.Write([]byte(strconv.Itoa(len(b))))
	h.Write([]byte{':'})
	if len(b) <= 2*fingerprintChunk {
		h.Write(b)
		return
	}
	h.Write(b[:fingerprintChunk])
	h.Write(b[len(b)-fingerprintChunk:])
}

// fileFingerprint 计算文件头尾各 fingerprintChunk 字节的哈希与文件大小。
func fileFingerprint(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	head := make([]byte, fingerprintChunk)
	n, _ := io.ReadFull(f, head)
	h.Write(head[:n])
	if st.Size() > int64(fingerprintChunk) {
		if _, err := f.Seek(-int64(fingerprintChunk), io.SeekEnd); err == nil {
			tail := make([]byte, fingerprintChunk)
			n, _ := io.ReadFull(f, tail)
			h.Write(tail[:n])
		}
	}
	return hex.EncodeToString(h.Sum(nil)), st.Size(), nil
}

// assetsReady 判断已解压的 node/脚本是否与当前内嵌资源一致且没有损坏。
func assetsReady(outputDir, nodePath, jsPath, overYamlPath string) bool {
	data, err := os.ReadFile(filepath.Join(outputDir, assetsStampName))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	if len(fields) != 5 {
		return false
	}
	if fields[0] != assetsFingerprint() {
		return false
	}
	nodeHash, nodeSize, err := fileFingerprint(nodePath)
	if err != nil || nodeHash != fields[1] {
		return false
	}
	if nodeSize != mustParseInt(fields[2]) {
		return false
	}
	if !isELF(nodePath) {
		return false
	}
	if st, err := os.Stat(jsPath); err != nil || st.Size() != mustParseInt(fields[3]) {
		return false
	}
	if st, err := os.Stat(overYamlPath); err != nil || st.Size() != mustParseInt(fields[4]) {
		return false
	}
	return true
}

// writeAssetsStamp 记录本次解压的资源指纹，供下次启动比对。
func writeAssetsStamp(outputDir, nodePath, jsPath, overYamlPath string) error {
	nodeHash, nodeSize, err := fileFingerprint(nodePath)
	if err != nil {
		return err
	}
	jsSize, err := fileSize(jsPath)
	if err != nil {
		return err
	}
	yamlSize, err := fileSize(overYamlPath)
	if err != nil {
		return err
	}
	stamp := strings.Join([]string{
		assetsFingerprint(),
		nodeHash,
		strconv.FormatInt(nodeSize, 10),
		strconv.FormatInt(jsSize, 10),
		strconv.FormatInt(yamlSize, 10),
	}, " ")
	return os.WriteFile(filepath.Join(outputDir, assetsStampName), []byte(stamp), 0644)
}

func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func mustParseInt(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return v
}

// isELF 检查文件头是否为 ELF 可执行文件（linux 下 node 必须是 ELF）。
func isELF(path string) bool {
	if runtime.GOOS != "linux" {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F'
}

func decodeZstd(nodePath, jsPath, overYamlPath string) error {
	// 创建 zstd 解码器
	zstdDecoder, err := zstd.NewReader(nil)
	if err != nil {
		return fmt.Errorf("创建zstd解码器失败: %w", err)
	}
	defer zstdDecoder.Close()

	// 解压 node 二进制文件
	nodeFile, err := os.OpenFile(nodePath, os.O_CREATE|os.O_WRONLY, 0755)
	if err != nil {
		return fmt.Errorf("创建 node 文件失败: %w", err)
	}
	defer nodeFile.Close()

	zstdDecoder.Reset(bytes.NewReader(EmbeddedNode))
	if _, err := io.Copy(nodeFile, zstdDecoder); err != nil {
		return fmt.Errorf("解压 node 二进制文件失败: %w", err)
	}

	// 解压 sub-store 脚本
	jsFile, err := os.OpenFile(jsPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("创建 sub-store 脚本文件失败: %w", err)
	}
	defer jsFile.Close()

	zstdDecoder.Reset(bytes.NewReader(EmbeddedSubStore))
	if _, err := io.Copy(jsFile, zstdDecoder); err != nil {
		return fmt.Errorf("解压 sub-store 脚本失败: %w", err)
	}

	// 解压 覆写文件
	overYamlFile, err := os.OpenFile(overYamlPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("创建 ACL4SSR_Online_Full.yaml 文件失败: %w", err)
	}
	defer overYamlFile.Close()

	zstdDecoder.Reset(bytes.NewReader(EmbeddedOverrideYaml))
	if _, err := io.Copy(overYamlFile, zstdDecoder); err != nil {
		return fmt.Errorf("解压 ACL4SSR_Online_Full.yaml 失败: %w", err)
	}
	return nil
}

func findProcesses(targetName string) (int32, error) {
	processes, err := process.Processes()
	if err != nil {
		return 0, fmt.Errorf("获取进程列表失败: %v", err)
	}

	for _, p := range processes {
		name, err := p.Exe()
		if err == nil && name == targetName {
			return p.Pid, nil
		}
	}
	return 0, fmt.Errorf("未找到进程")
}

// findSubStoreProcesses finds the Sub-Store processes that belong to this
// installation (the node binary and the bundle are both decoded into the
// output directory). Processes of other installations are left alone.
func findSubStoreProcesses(nodePath, jsPath string) ([]int32, error) {
	processes, err := process.Processes()
	if err != nil {
		return nil, fmt.Errorf("获取进程列表失败: %v", err)
	}

	wantNode := filepath.Clean(nodePath)
	var pids []int32
	for _, p := range processes {
		cmdline, err := p.Cmdline()
		if err != nil || !strings.Contains(cmdline, "sub-store.bundle.js") {
			continue
		}
		if strings.Contains(cmdline, jsPath) {
			pids = append(pids, p.Pid)
			continue
		}
		// 旧版本可能用了不同的 bundle 路径，但只要可执行文件仍是本安装目录下的
		// node，也属于本实例的残留进程。
		if exe, err := p.Exe(); err == nil && filepath.Clean(exe) == wantNode {
			pids = append(pids, p.Pid)
		}
	}
	if len(pids) == 0 {
		return nil, fmt.Errorf("未找到 sub-store 进程")
	}
	return pids, nil
}

func killProcess(pid int32) error {
	p, err := process.NewProcess(pid)
	if err != nil {
		return fmt.Errorf("无法找到进程 %d: %v", pid, err)
	}

	if err := p.Kill(); err != nil {
		return fmt.Errorf("杀死进程 %d 失败: %v", pid, err)
	}
	return nil
}

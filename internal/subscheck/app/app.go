package app

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"

	"substore-ui-go/internal/subscheck/assets"
	"substore-ui-go/internal/subscheck/check"
	"substore-ui-go/internal/subscheck/config"
	"substore-ui-go/internal/subscheck/monitor"
	"substore-ui-go/internal/subscheck/save"
	"substore-ui-go/internal/subscheck/utils"
	"github.com/fsnotify/fsnotify"
	"github.com/robfig/cron/v3"
)

// App 结构体用于管理应用程序状态
type App struct {
	configPath string
	interval   int
	watcher    *fsnotify.Watcher
	checkChan  chan struct{}
	checking   atomic.Bool
	ticker     *time.Ticker
	done       chan struct{}
	cron       *cron.Cron
	version    string
}

// New 创建新的应用实例
func New(version string) *App {
	configPath := flag.String("f", "", "配置文件路径")
	flag.Parse()

	// The -f flag wins; otherwise fall back to the SUBS_CONFIG_PATH
	// environment variable so the merged binary can be configured purely
	// through env vars (as the FPK launcher does).
	resolved := *configPath
	if resolved == "" {
		resolved = os.Getenv("SUBS_CONFIG_PATH")
	}

	return &App{
		configPath: resolved,
		checkChan:  make(chan struct{}),
		done:       make(chan struct{}),
		version:    version,
	}
}

// Initialize 初始化应用程序
func (app *App) Initialize() error {
	if err := app.initConfigPath(); err != nil {
		return fmt.Errorf("初始化配置文件路径失败: %w", err)
	}
	utils.SetCacheDir(app.configPath)

	if err := app.loadConfig(); err != nil {
		return fmt.Errorf("加载配置文件失败: %w", err)
	}

	if err := initResolver(); err != nil {
		return fmt.Errorf("初始化 DNS 失败: %w", err)
	}

	if err := app.initConfigWatcher(); err != nil {
		return fmt.Errorf("初始化配置文件监听失败: %w", err)
	}

	if config.GlobalConfig.Proxy != "" {
		os.Setenv("HTTP_PROXY", config.GlobalConfig.Proxy)
		os.Setenv("HTTPS_PROXY", config.GlobalConfig.Proxy)
	}

	app.interval = func() int {
		if config.GlobalConfig.CheckInterval <= 0 {
			return 1
		}
		return config.GlobalConfig.CheckInterval
	}()

	if config.GlobalConfig.SubStorePort != "" {
		if runtime.GOOS == "linux" && runtime.GOARCH == "386" {
			slog.Warn("node不支持Linux 32位系统，不启动sub-store服务")
		} else {
			go assets.RunSubStoreService()
			time.Sleep(500 * time.Millisecond)
		}
	}

	monitor.StartMemoryMonitor()
	utils.SetupSignalHandler(check.RequestCancel)
	return nil
}

// Run 运行应用程序主循环
func (app *App) Run() {
	defer func() {
		if app.watcher != nil {
			app.watcher.Close()
		}
		if app.ticker != nil {
			app.ticker.Stop()
		}
		if app.cron != nil {
			app.cron.Stop()
		}
	}()

	app.setTimer()

	if config.GlobalConfig.CronExpression != "" {
		slog.Warn("使用cron表达式，首次启动不立即执行检测")
	} else {
		app.triggerCheck()
	}

	for range app.checkChan {
		go app.triggerCheck()
	}
}

// setTimer 根据配置设置定时器
func (app *App) setTimer() {
	if app.ticker != nil {
		close(app.done)
		app.done = make(chan struct{})
		app.ticker.Stop()
		app.ticker = nil
	}

	if app.cron != nil {
		app.cron.Stop()
		app.cron = nil
	}

	if config.GlobalConfig.CronExpression != "" {
		slog.Info(fmt.Sprintf("使用cron表达式: %s", config.GlobalConfig.CronExpression))
		app.cron = cron.New()
		_, err := app.cron.AddFunc(config.GlobalConfig.CronExpression, func() {
			app.triggerCheck()
		})
		if err != nil {
			slog.Error(fmt.Sprintf("cron表达式 '%s' 解析失败: %v，将使用检查间隔时间",
				config.GlobalConfig.CronExpression, err))
			app.useIntervalTimer()
		} else {
			app.cron.Start()
		}
	} else {
		app.useIntervalTimer()
	}
}

func (app *App) useIntervalTimer() {
	app.ticker = time.NewTicker(time.Duration(app.interval) * time.Minute)
	done := app.done
	go func() {
		for {
			select {
			case <-app.ticker.C:
				app.triggerCheck()
			case <-done:
				return
			}
		}
	}()
}

// TriggerCheck 供外部调用的触发检测方法
func (app *App) TriggerCheck() {
	select {
	case app.checkChan <- struct{}{}:
		slog.Info("手动触发检测")
	default:
		slog.Warn("已有检测正在进行，忽略本次触发")
	}
}

func (app *App) triggerCheck() {
	if !app.checking.CompareAndSwap(false, true) {
		slog.Warn("已有检测正在进行，跳过本次检测")
		return
	}
	defer app.checking.Store(false)

	if err := app.checkProxies(); err != nil {
		slog.Error(fmt.Sprintf("检测代理失败: %v", err))
		os.Exit(1)
	}

	if app.ticker != nil {
		app.ticker.Reset(time.Duration(app.interval) * time.Minute)
		nextCheck := time.Now().Add(time.Duration(app.interval) * time.Minute)
		slog.Info(fmt.Sprintf("下次检查时间: %s", nextCheck.Format("2006-01-02 15:04:05")))
	} else if app.cron != nil {
		entries := app.cron.Entries()
		if len(entries) > 0 {
			nextTime := entries[0].Next
			slog.Info(fmt.Sprintf("下次检查时间: %s", nextTime.Format("2006-01-02 15:04:05")))
		}
	}
	debug.FreeOSMemory()
}

func (app *App) checkProxies() error {
	slog.Info("开始准备检测代理", "进度展示", config.GlobalConfig.PrintProgress)

	if config.GlobalConfig.KeepDays > 0 {
		if hp := save.LoadHistoryProxies(); len(hp) > 0 {
			config.GlobalProxies = append(config.GlobalProxies, hp...)
		}
	}

	results, err := check.Check()
	if err != nil {
		return fmt.Errorf("检测代理失败: %w", err)
	}
	slog.Info("检测完成")
	save.SaveConfig(results)
	utils.SendNotify(len(results))
	utils.UpdateSubs()
	utils.ExecuteCallback(len(results))

	return nil
}

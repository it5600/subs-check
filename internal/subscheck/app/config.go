package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"substore-ui-go/internal/subscheck/config"
	"substore-ui-go/internal/subscheck/utils"
	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

func (app *App) initConfigPath() error {
	if app.configPath == "" {
		execPath := utils.GetExecutablePath()
		configDir := filepath.Join(execPath, "config")

		if err := os.MkdirAll(configDir, 0755); err != nil {
			return fmt.Errorf("创建配置目录失败: %w", err)
		}

		app.configPath = filepath.Join(configDir, "config.yaml")
	}
	return nil
}

func (app *App) loadConfig() error {
	yamlFile, err := os.ReadFile(app.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return app.createDefaultConfig()
		}
		return fmt.Errorf("读取配置文件失败: %w", err)
	}

	if err := yaml.Unmarshal(yamlFile, config.GlobalConfig); err != nil {
		return fmt.Errorf("解析配置文件失败: %w", err)
	}

	slog.Info("配置文件读取成功")
	return nil
}

// createDefaultConfig writes the built-in configuration template when no
// config file exists yet.
//
// Upstream subs-check exits the process here (os.Exit(0)) so the user edits
// the file before the first run. That behaviour is wrong for the merged
// application: in FPK/Docker deployments the config lives in a shared volume
// and the process is restarted by a supervisor, so exiting would make the
// app permanently fail to start.
//
// Instead the template is written, loaded into GlobalConfig and the run
// continues with the default values.
func (app *App) createDefaultConfig() error {
	slog.Info("配置文件不存在，创建默认配置文件")

	if err := os.WriteFile(app.configPath, []byte(config.DefaultConfigTemplate), 0644); err != nil {
		return fmt.Errorf("写入默认配置文件失败: %w", err)
	}
	slog.Info("默认配置文件创建成功")
	slog.Info(fmt.Sprintf("配置文件路径: %s", app.configPath))

	// Load the freshly written template so the run continues with valid values.
	if err := yaml.Unmarshal(config.DefaultConfigTemplate, config.GlobalConfig); err != nil {
		return fmt.Errorf("解析默认配置文件失败: %w", err)
	}

	slog.Info("已载入默认配置，可在共享目录中修改后自动生效")
	return nil
}

// reloadConfig reloads the YAML and re-arms the timer if schedule-related
// keys changed. Shared by the inotify watcher and the polling fallback so
// both paths have identical semantics.
func (app *App) reloadConfig() {
	oldCronExpr := config.GlobalConfig.CronExpression
	oldInterval := app.interval

	if err := app.loadConfig(); err != nil {
		slog.Error(fmt.Sprintf("重新加载配置文件失败: %v", err))
		return
	}

	if oldCronExpr != config.GlobalConfig.CronExpression ||
		oldInterval != config.GlobalConfig.CheckInterval {

		app.interval = func() int {
			if config.GlobalConfig.CheckInterval <= 0 {
				return 1
			}
			return config.GlobalConfig.CheckInterval
		}()
		slog.Warn("检测设置发生变化，重新配置定时器")
		app.setTimer()
	}
}

// pollConfigFile is the degraded config watcher used when inotify is
// unavailable. Some NAS systems (Synology/fnOS) cap max_user_instances at
// 8~128, so fsnotify.NewWatcher()/Add() can fail even though the app is
// otherwise healthy. Instead of aborting startup (which put the app into a
// supervisor restart loop), compare mtime+size every 5s.
// Reference: subs-check-pro app/config.go (pollConfigFile).
func (app *App) pollConfigFile() {
	signature := func() (int64, int64) {
		info, err := os.Stat(app.configPath)
		if err != nil {
			return 0, 0
		}
		return info.ModTime().UnixNano(), info.Size()
	}

	lastMod, lastSize := signature()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			mod, size := signature()
			if mod == lastMod && size == lastSize {
				continue
			}
			lastMod, lastSize = mod, size
			slog.Info("配置文件发生变化(轮询)，正在重新加载")
			app.reloadConfig()
		case <-app.done:
			return
		}
	}
}

func (app *App) initConfigWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn(fmt.Sprintf("文件监听器创建失败，降级为 5s 轮询: %v", err))
		go app.pollConfigFile()
		return nil
	}

	app.watcher = watcher

	if err := watcher.Add(filepath.Dir(app.configPath)); err != nil {
		_ = watcher.Close()
		app.watcher = nil
		slog.Warn(fmt.Sprintf("添加配置文件监听失败，降级为 5s 轮询: %v", err))
		go app.pollConfigFile()
		return nil
	}

	var debounceTimer *time.Timer
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if absPath, _ := filepath.Abs(app.configPath); event.Name != absPath {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					if debounceTimer != nil {
						debounceTimer.Stop()
					}

					debounceTimer = time.AfterFunc(100*time.Millisecond, func() {
						slog.Info("配置文件发生变化，正在重新加载")
						app.reloadConfig()
					})
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Error(fmt.Sprintf("配置文件监听错误: %v", err))
			case <-app.done:
				return
			}
		}
	}()

	slog.Info("配置文件监听已启动")
	return nil
}

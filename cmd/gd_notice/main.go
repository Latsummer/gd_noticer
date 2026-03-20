// Package main 是黄金报价通知服务的入口。
// 负责初始化所有模块并启动调度与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gd_notice/internal/config"
	"gd_notice/internal/gold"
	"gd_notice/internal/health"
	"gd_notice/internal/notifier"
	"gd_notice/internal/scheduler"
	"gd_notice/internal/state"
	"gd_notice/internal/strategy"
)

func main() {
	// 解析命令行参数
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	// 初始化结构化日志（使用默认配置，配置加载后重新初始化）
	initLogger(config.LogConfig{Output: "stdout", Level: "info"})

	slog.Info("黄金报价通知服务启动中", "config_path", *configPath)

	// 第一步：加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("加载配置失败", "error", err)
		os.Exit(1)
	}

	// 配置加载完成后，依据配置重新初始化日志
	if err := initLogger(cfg.Log); err != nil {
		slog.Error("初始化日志失败", "error", err)
		os.Exit(1)
	}
	slog.Info("配置加载完成",
		"timezone", cfg.Service.Timezone,
		"poll_interval", cfg.Service.PollIntervalSeconds,
		"window", cfg.Service.WindowStart+"~"+cfg.Service.WindowEnd,
		"device_count", len(cfg.Notify.DeviceKeys),
	)

	// 第二步：加载时区
	loc, err := time.LoadLocation(cfg.Service.Timezone)
	if err != nil {
		slog.Error("加载时区失败", "timezone", cfg.Service.Timezone, "error", err)
		os.Exit(1)
	}

	// 第三步：初始化状态存储
	store, err := state.NewStore(cfg.State.FilePath)
	if err != nil {
		slog.Error("初始化状态存储失败", "error", err)
		os.Exit(1)
	}
	slog.Info("状态存储初始化完成", "file_path", cfg.State.FilePath)

	// 第四步：初始化黄金 API 客户端
	goldCli := gold.NewClient(gold.ClientConfig{
		ApiType:             cfg.GoldAPI.ApiType,
		BaseURL:             cfg.GoldAPI.BaseURL,
		GoldID:              cfg.GoldAPI.GoldID,
		FusionGoldID:        cfg.GoldAPI.FusionGoldID,
		AppKey:              cfg.GoldAPI.AppKey,
		Sign:                cfg.GoldAPI.Sign,
		HTTPTimeoutSeconds:  cfg.Reliability.HTTPTimeoutSeconds,
		MaxRetries:          cfg.Reliability.MaxRetries,
		RetryBackoffSeconds: cfg.Reliability.RetryBackoffSeconds,
	})

	// 第五步：初始化通知策略
	evaluator := strategy.NewEvaluator(strategy.StrategyConfig{
		DedupByUptime:       cfg.Strategy.DedupByUptime,
		MinChangePercent:    cfg.Strategy.MinChangePercent,
		MaxSilentMinutes:    cfg.Strategy.MaxSilentMinutes,
		IsFusion:            cfg.GoldAPI.ApiType == "fusion",
		NotifyChangePercent: cfg.Strategy.NotifyChangePercent,
		TrendReversalCount:  cfg.Strategy.TrendReversalCount,
		PriceHistorySize:    cfg.Strategy.PriceHistorySize,
	})

	// 第六步：初始化 Bark 通知器
	bark := notifier.NewBarkNotifier(notifier.BarkConfig{
		Endpoint:           cfg.Notify.BarkEndpoint,
		DeviceKeys:         cfg.Notify.DeviceKeys,
		Group:              cfg.Notify.Group,
		HTTPTimeoutSeconds: cfg.Reliability.HTTPTimeoutSeconds,
	})

	// 第七步：初始化调度器
	sched := scheduler.NewScheduler(cfg, goldCli, evaluator, bark, store, loc)

	// 创建可取消的上下文，用于优雅退出
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 第八步：启动 HTTP 服务（如果启用）
	if cfg.HTTPServer.Enable {
		mux := http.NewServeMux()
		healthHandler := health.NewHandler(store, sched.InWindow, sched.Trigger)
		healthHandler.RegisterRoutes(mux)

		server := &http.Server{
			Addr:    cfg.HTTPServer.ListenAddr,
			Handler: mux,
		}

		go func() {
			slog.Info("HTTP 服务启动", "listen_addr", cfg.HTTPServer.ListenAddr)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("HTTP 服务异常退出", "error", err)
			}
		}()

		// 注册 HTTP 服务优雅关闭
		go func() {
			<-ctx.Done()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				slog.Error("HTTP 服务关闭失败", "error", err)
			}
		}()
	}

	// 第九步：监听系统信号，实现优雅退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("收到退出信号", "signal", sig)
		cancel()
	}()

	// 第十步：启动调度器（阻塞运行）
	sched.Start(ctx)

	slog.Info("黄金报价通知服务已停止")
}

// initLogger 依据配置初始化结构化日志输出。
// Output 为 "stdout" 时输出到标准输出，其他值将作为文件路径写入日志文件。
// Level 支持 debug/info/warn/error，不配置时默认为 info。
func initLogger(logCfg config.LogConfig) error {
	// 解析日志级别
	var level slog.Level
	switch strings.ToLower(logCfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	// 解析日志输出目标
	var writer io.Writer
	if strings.ToLower(logCfg.Output) == "stdout" || logCfg.Output == "" {
		writer = os.Stdout
	} else {
		f, err := os.OpenFile(logCfg.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("打开日志文件失败 %q: %w", logCfg.Output, err)
		}
		writer = f
	}

	opts := &slog.HandlerOptions{Level: level}
	handler := slog.NewJSONHandler(writer, opts)
	slog.SetDefault(slog.New(handler))
	return nil
}

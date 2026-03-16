// Package scheduler 负责定时触发任务执行。
// 支持时间窗口控制（仅在指定时段内执行），并通过执行锁保证同一时刻仅一个任务在运行。
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gd_notice/internal/config"
	"gd_notice/internal/gold"
	"gd_notice/internal/notifier"
	"gd_notice/internal/state"
	"gd_notice/internal/strategy"
)

// Scheduler 是定时任务调度器。
type Scheduler struct {
	cfg       *config.Config
	goldCli   *gold.Client
	evaluator *strategy.Evaluator
	bark      *notifier.BarkNotifier
	store     *state.Store
	loc       *time.Location

	mu      sync.Mutex // 执行锁，防止并发重入
	running bool
}

// NewScheduler 创建调度器实例。
func NewScheduler(
	cfg *config.Config,
	goldCli *gold.Client,
	evaluator *strategy.Evaluator,
	bark *notifier.BarkNotifier,
	store *state.Store,
	loc *time.Location,
) *Scheduler {
	return &Scheduler{
		cfg:       cfg,
		goldCli:   goldCli,
		evaluator: evaluator,
		bark:      bark,
		store:     store,
		loc:       loc,
	}
}

// Start 启动调度循环，按配置的间隔定时执行任务，直到 ctx 被取消。
func (s *Scheduler) Start(ctx context.Context) {
	interval := time.Duration(s.cfg.Service.PollIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	slog.Info("调度器已启动",
		"interval", interval,
		"window_start", s.cfg.Service.WindowStart,
		"window_end", s.cfg.Service.WindowEnd,
	)

	// 启动时立即执行一次
	s.tryExecute(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("调度器收到停止信号，正在退出")
			return
		case <-ticker.C:
			s.tryExecute(ctx)
		}
	}
}

// Trigger 手动触发一次任务执行（不受时间窗口限制）。
func (s *Scheduler) Trigger() {
	ctx := context.Background()
	s.execute(ctx)
}

// InWindow 判断当前时刻是否在配置的时间窗口内。
func (s *Scheduler) InWindow() bool {
	return s.inWindow(time.Now().In(s.loc))
}

// tryExecute 尝试执行任务，先检查是否在时间窗口内。
func (s *Scheduler) tryExecute(ctx context.Context) {
	now := time.Now().In(s.loc)
	if !s.inWindow(now) {
		slog.Debug("当前不在时间窗口内，跳过执行",
			"now", now.Format("15:04:05"),
			"window_start", s.cfg.Service.WindowStart,
			"window_end", s.cfg.Service.WindowEnd,
		)
		return
	}

	s.execute(ctx)
}

// execute 执行一次完整的拉取-判定-推送流程。
func (s *Scheduler) execute(ctx context.Context) {
	// 获取执行锁，防止并发重入
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		slog.Warn("上一次任务仍在执行，跳过本次")
		return
	}
	s.running = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	slog.Info("开始执行行情拉取任务")

	// 第一步：拉取黄金行情
	quote, err := s.goldCli.FetchQuote(ctx)
	now := time.Now()

	if err != nil {
		slog.Error("拉取黄金行情失败", "error", err)
		s.handleFailure(ctx, err, now)
		return
	}

	slog.Info("行情拉取成功",
		"event", "fetch",
		"gold_id", quote.GoldID,
		"last_price", quote.LastPrice,
		"change_price", quote.ChangePrice,
		"change_margin", quote.ChangeMargin,
		"uptime", quote.Uptime,
	)

	// 第二步：策略判定
	st := s.store.Get()
	decision := s.evaluator.Evaluate(quote, st)

	slog.Info("策略判定完成",
		"event", "decide",
		"should_notify", decision.ShouldNotify,
		"reason", decision.Reason,
	)

	// 第三步：发送通知（如果需要）
	if decision.ShouldNotify {
		title := strategy.FormatNotifyTitle(s.cfg.Notify.TitlePrefix, quote, s.cfg.GoldAPI.IDToName)
		isFusion := s.cfg.GoldAPI.ApiType == "fusion"
		body := strategy.FormatNotifyBody(quote, isFusion)

		results := s.bark.Send(ctx, title, body)

		if notifier.HasAnySuccess(results) {
			slog.Info("通知发送成功", "event", "notify", "title", title)
			// 更新通知状态
			s.store.Update(func(st *state.State) {
				st.LastNotifyAt = now
				st.LastNotifyDigest = fmt.Sprintf("%s_%s", quote.Uptime, quote.LastPrice)
			})
		} else {
			slog.Error("所有设备推送均失败", "event", "notify")
		}
	}

	// 第四步：更新状态
	s.store.Update(func(st *state.State) {
		st.LastSuccessUptime = quote.Uptime
		st.LastSuccessPrice = quote.LastPrice
		st.LastFetchAt = now
		st.ConsecutiveFailures = 0
		st.LastError = ""
	})
}

// handleFailure 处理拉取失败的情况，包括连续失败计数和告警。
func (s *Scheduler) handleFailure(ctx context.Context, fetchErr error, now time.Time) {
	s.store.Update(func(st *state.State) {
		st.ConsecutiveFailures++
		st.LastError = fetchErr.Error()
		st.LastFetchAt = now
	})

	st := s.store.Get()

	// 连续失败达到阈值时发送异常告警
	if st.ConsecutiveFailures >= s.cfg.Reliability.FailureAlertThreshold {
		slog.Warn("连续失败达到告警阈值",
			"consecutive_failures", st.ConsecutiveFailures,
			"threshold", s.cfg.Reliability.FailureAlertThreshold,
		)

		title := fmt.Sprintf("%s 服务异常告警", s.cfg.Notify.TitlePrefix)
		body := fmt.Sprintf(
			"黄金行情拉取连续失败 %d 次\n最近错误: %s\n时间: %s",
			st.ConsecutiveFailures,
			st.LastError,
			now.Format("2006-01-02 15:04:05"),
		)

		s.bark.Send(ctx, title, body)
	}
}

// inWindow 判断指定时刻是否在时间窗口内。
func (s *Scheduler) inWindow(now time.Time) bool {
	start, err := parseTimeOfDay(s.cfg.Service.WindowStart, now, s.loc)
	if err != nil {
		slog.Error("解析 window_start 失败", "error", err)
		return false
	}

	end, err := parseTimeOfDay(s.cfg.Service.WindowEnd, now, s.loc)
	if err != nil {
		slog.Error("解析 window_end 失败", "error", err)
		return false
	}

	// 处理跨天的情况（如 22:00 ~ 02:00）
	if end.Before(start) {
		return now.After(start) || now.Before(end)
	}

	return (now.Equal(start) || now.After(start)) && now.Before(end)
}

// parseTimeOfDay 将 HH:MM:SS 字符串解析为当天对应的 time.Time。
func parseTimeOfDay(hms string, now time.Time, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation("15:04:05", hms, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("时间格式解析失败: %w", err)
	}

	return time.Date(
		now.Year(), now.Month(), now.Day(),
		t.Hour(), t.Minute(), t.Second(), 0, loc,
	), nil
}

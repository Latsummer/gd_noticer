// Package scheduler 负责定时触发任务执行。
// 支持时间窗口控制（仅在指定时段内执行），自适应轮询间隔，并通过执行锁保证同一时刻仅一个任务在运行。
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
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

// Start 启动调度循环。当启用自适应轮询时使用动态间隔的 Timer，否则使用固定 Ticker。
func (s *Scheduler) Start(ctx context.Context) {
	baseInterval := time.Duration(s.cfg.Service.PollIntervalSeconds) * time.Second

	slog.Info("调度器已启动",
		"interval", baseInterval,
		"adaptive_poll", s.cfg.Service.AdaptivePoll,
		"window_start", s.cfg.Service.WindowStart,
		"window_end", s.cfg.Service.WindowEnd,
	)

	// 启动时立即执行一次
	s.tryExecute(ctx)

	for {
		interval := s.calcNextInterval()
		slog.Debug("下次轮询间隔", "interval", interval)

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			slog.Info("调度器收到停止信号，正在退出")
			return
		case <-timer.C:
			s.tryExecute(ctx)
		}
	}
}

// calcNextInterval 根据价格波动率动态计算下次轮询间隔。
// 高波动 → 短间隔（minInterval），低波动 → 长间隔（maxInterval）。
func (s *Scheduler) calcNextInterval() time.Duration {
	baseInterval := time.Duration(s.cfg.Service.PollIntervalSeconds) * time.Second

	if !s.cfg.Service.AdaptivePoll {
		return baseInterval
	}

	history := s.store.Get().PriceHistory
	if len(history) < 2 {
		return baseInterval
	}

	minInterval := time.Duration(s.cfg.Service.MinPollIntervalSeconds) * time.Second
	maxInterval := time.Duration(s.cfg.Service.MaxPollIntervalSeconds) * time.Second

	volatility := calcVolatility(history)

	// 将波动率映射到 [minInterval, maxInterval]
	// 波动率 0 → maxInterval，波动率 >= 1% → minInterval
	// 使用线性插值，波动率阈值 1%
	const highVolatilityThreshold = 1.0 // 百分比
	ratio := volatility / highVolatilityThreshold
	if ratio > 1 {
		ratio = 1
	}

	// 高波动 ratio→1 → minInterval, 低波动 ratio→0 → maxInterval
	interval := maxInterval - time.Duration(float64(maxInterval-minInterval)*ratio)

	slog.Debug("自适应轮询计算",
		"volatility_pct", fmt.Sprintf("%.4f", volatility),
		"ratio", fmt.Sprintf("%.2f", ratio),
		"interval", interval,
		"history_len", len(history),
	)

	return interval
}

// calcVolatility 计算价格历史的波动率（极差 / 均值 * 100，百分比）。
func calcVolatility(history []state.PricePoint) float64 {
	if len(history) < 2 {
		return 0
	}

	minPrice := history[0].Price
	maxPrice := history[0].Price
	sum := 0.0

	for _, p := range history {
		if p.Price < minPrice {
			minPrice = p.Price
		}
		if p.Price > maxPrice {
			maxPrice = p.Price
		}
		sum += p.Price
	}

	avg := sum / float64(len(history))
	if avg == 0 {
		return 0
	}

	return (maxPrice - minPrice) / avg * 100
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

// isSkipDay 判断指定时刻是否为配置中跳过的星期几。
func (s *Scheduler) isSkipDay(now time.Time) bool {
	weekday := strings.ToLower(now.Weekday().String())
	for _, skip := range s.cfg.Service.SkipDays {
		if strings.ToLower(strings.TrimSpace(skip)) == weekday {
			return true
		}
	}
	return false
}

// tryExecute 尝试执行任务，先检查是否在工作日和时间窗口内。
func (s *Scheduler) tryExecute(ctx context.Context) {
	now := time.Now().In(s.loc)

	// 检查是否为跳过的星期几
	if s.isSkipDay(now) {
		slog.Debug("当前为休息日，跳过执行",
			"weekday", now.Weekday().String(),
		)
		return
	}

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

	// 第二步：跨天检查（仅重置日期，不更新高低点）
	currentPrice := parsePrice(quote.LastPrice)
	todayDate := now.In(s.loc).Format("2006-01-02")

	s.store.Update(func(st *state.State) {
		if st.DailyDate != todayDate {
			slog.Info("跨天重置日内高低点", "old_date", st.DailyDate, "new_date", todayDate)
			st.ResetDaily(todayDate)
		}
	})

	// 第三步：策略判定（在更新价格历史和高低点之前，用旧 state 评估）
	st := s.store.Get()
	decision := s.evaluator.Evaluate(quote, st)

	slog.Info("策略判定完成",
		"event", "decide",
		"should_notify", decision.ShouldNotify,
		"notify_type", decision.NotifyType.String(),
		"reason", decision.Reason,
	)

	// 第四步：更新价格历史和日内高低点（评估完成后再写入）
	s.store.Update(func(st *state.State) {
		if currentPrice > 0 {
			st.AddPricePoint(state.PricePoint{
				Price:  currentPrice,
				Time:   now,
				Uptime: quote.Uptime,
			}, s.cfg.Strategy.PriceHistorySize)

			timeStr := now.In(s.loc).Format("15:04:05")
			if st.DailyHigh == 0 || currentPrice > st.DailyHigh {
				st.DailyHigh = currentPrice
				st.DailyHighTime = timeStr
			}
			if st.DailyLow == 0 || currentPrice < st.DailyLow {
				st.DailyLow = currentPrice
				st.DailyLowTime = timeStr
			}
		}
	})

	// 第五步：发送通知（如果需要），用评估时的 state 快照格式化内容
	if decision.ShouldNotify {
		// 通知格式化用评估后、更新后的 state（包含最新高低点，展示更准确）
		formatSt := s.store.Get()
		isFusion := s.cfg.GoldAPI.ApiType == "fusion"
		title, body := strategy.FormatNotification(
			s.cfg.Notify.TitlePrefix, quote, formatSt, decision,
			s.cfg.GoldAPI.IDToName, isFusion,
		)

		results := s.bark.Send(ctx, title, body)

		if notifier.HasAnySuccess(results) {
			slog.Info("通知发送成功",
				"event", "notify",
				"title", title,
				"notify_type", decision.NotifyType.String(),
			)
			s.store.Update(func(st *state.State) {
				st.LastNotifyAt = now
				st.LastNotifyDigest = fmt.Sprintf("%s_%s", quote.Uptime, quote.LastPrice)
				st.LastNotifyPrice = quote.LastPrice
			})
		} else {
			slog.Error("所有设备推送均失败", "event", "notify")
		}
	}

	// 第六步：更新拉取状态
	s.store.Update(func(st *state.State) {
		st.LastSuccessUptime = quote.Uptime
		st.LastSuccessPrice = quote.LastPrice
		st.LastFetchAt = now
		st.ConsecutiveFailures = 0
		st.LastError = ""
	})
}

// parsePrice 解析价格字符串为 float64。
func parsePrice(priceStr string) float64 {
	val, err := strconv.ParseFloat(priceStr, 64)
	if err != nil {
		return 0
	}
	return val
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

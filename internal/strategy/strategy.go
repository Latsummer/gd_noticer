// Package strategy 负责根据行情数据和历史状态判断是否需要发送通知。
// 支持多种智能通知规则：uptime 去重、累计价格变化、趋势反转检测、日内新高/低、静默保底。
package strategy

import (
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"gd_notice/internal/gold"
	"gd_notice/internal/state"
)

// NotifyType 表示通知触发类型。
type NotifyType int

const (
	NotifyNone            NotifyType = iota
	NotifyFirstTime                  // 首次通知
	NotifyPriceChange                // 累计价格变化达阈值
	NotifyTrendReversal              // 趋势反转（拐点）
	NotifyDailyHigh                  // 日内新高
	NotifyDailyLow                   // 日内新低
	NotifyRegularUpdate              // 定期更新（行情有变化 + 距上次通知超过间隔）
	NotifySilentHeartbeat            // 静默保底
)

// String 返回 NotifyType 的可读字符串。
func (t NotifyType) String() string {
	switch t {
	case NotifyNone:
		return "none"
	case NotifyFirstTime:
		return "first_time"
	case NotifyPriceChange:
		return "price_change"
	case NotifyTrendReversal:
		return "trend_reversal"
	case NotifyDailyHigh:
		return "daily_high"
	case NotifyDailyLow:
		return "daily_low"
	case NotifyRegularUpdate:
		return "regular_update"
	case NotifySilentHeartbeat:
		return "silent_heartbeat"
	default:
		return "unknown"
	}
}

// Decision 表示策略判定结果。
type Decision struct {
	ShouldNotify bool       // 是否需要发送通知
	NotifyType   NotifyType // 通知类型
	Reason       string     // 判定原因
}

// StrategyConfig 定义策略参数。
type StrategyConfig struct {
	DedupByUptime          bool    // 是否基于 uptime 去重
	MinChangePercent       float64 // 最小涨跌幅阈值（百分比，0 表示不过滤）—— 保留兼容
	MaxSilentMinutes       int     // 最大静默时间（分钟），超过后强制发送心跳
	IsFusion               bool    // 是否为融合行情模式
	NotifyChangePercent    float64 // 距上次通知的累计变化 % 阈值
	TrendReversalCount     int     // 连续同向 N 次后反转视为拐点
	PriceHistorySize       int     // 价格历史滑动窗口大小
	RegularIntervalMinutes int     // 定期更新间隔（分钟），行情有变化且超过此间隔就通知（0 表示禁用）
}

// Evaluator 是策略评估器。
type Evaluator struct {
	cfg StrategyConfig
}

// NewEvaluator 创建策略评估器实例。
func NewEvaluator(cfg StrategyConfig) *Evaluator {
	return &Evaluator{cfg: cfg}
}

// Evaluate 根据最新行情和历史状态判断是否需要通知。
// 按优先级依次检查：首次通知 → uptime 去重 → 日内新高/低 → 趋势反转 → 累计价格变化 → 静默保底。
func (e *Evaluator) Evaluate(quote *gold.QuoteItem, st state.State) Decision {
	now := time.Now()
	currentPrice := parsePrice(quote.LastPrice)

	// 规则1：首次通知 — LastNotifyAt 为零值，说明从未通知过
	if st.LastNotifyAt.IsZero() {
		return Decision{
			ShouldNotify: true,
			NotifyType:   NotifyFirstTime,
			Reason:       "首次行情通知",
		}
	}

	// 规则2：基于 uptime 去重 — 行情未更新时仅检查静默保底
	if e.cfg.DedupByUptime && quote.Uptime == st.LastSuccessUptime {
		return e.checkSilentHeartbeat(st, now)
	}

	// 规则3：日内新高
	if st.DailyHigh > 0 && currentPrice > st.DailyHigh {
		slog.Info("触发日内新高通知",
			"current_price", currentPrice,
			"previous_high", st.DailyHigh,
		)
		return Decision{
			ShouldNotify: true,
			NotifyType:   NotifyDailyHigh,
			Reason:       fmt.Sprintf("日内新高: %.2f > 前高 %.2f", currentPrice, st.DailyHigh),
		}
	}

	// 规则4：日内新低
	if st.DailyLow > 0 && currentPrice < st.DailyLow {
		slog.Info("触发日内新低通知",
			"current_price", currentPrice,
			"previous_low", st.DailyLow,
		)
		return Decision{
			ShouldNotify: true,
			NotifyType:   NotifyDailyLow,
			Reason:       fmt.Sprintf("日内新低: %.2f < 前低 %.2f", currentPrice, st.DailyLow),
		}
	}

	// 规则5：趋势反转检测
	if e.cfg.TrendReversalCount >= 2 && len(st.PriceHistory) >= e.cfg.TrendReversalCount+1 {
		if reversed, desc := e.detectTrendReversal(st.PriceHistory, currentPrice); reversed {
			slog.Info("触发趋势反转通知", "description", desc)
			return Decision{
				ShouldNotify: true,
				NotifyType:   NotifyTrendReversal,
				Reason:       desc,
			}
		}
	}

	// 规则6：累计价格变化达阈值
	if e.cfg.NotifyChangePercent > 0 && st.LastNotifyPrice != "" {
		lastNotifyPrice := parsePrice(st.LastNotifyPrice)
		if lastNotifyPrice > 0 {
			changePercent := math.Abs(currentPrice-lastNotifyPrice) / lastNotifyPrice * 100
			if changePercent >= e.cfg.NotifyChangePercent {
				direction := "↑"
				if currentPrice < lastNotifyPrice {
					direction = "↓"
				}
				slog.Info("触发累计价格变化通知",
					"current_price", currentPrice,
					"last_notify_price", lastNotifyPrice,
					"change_percent", changePercent,
					"threshold", e.cfg.NotifyChangePercent,
				)
				return Decision{
					ShouldNotify: true,
					NotifyType:   NotifyPriceChange,
					Reason:       fmt.Sprintf("累计变化 %s%.2f%%（阈值 %.2f%%）", direction, changePercent, e.cfg.NotifyChangePercent),
				}
			}
		}
	}

	// 规则7：定期更新 — 行情有变化且距上次通知超过设定间隔
	if e.cfg.RegularIntervalMinutes > 0 {
		sinceLastNotify := now.Sub(st.LastNotifyAt)
		if sinceLastNotify >= time.Duration(e.cfg.RegularIntervalMinutes)*time.Minute {
			slog.Info("触发定期更新通知",
				"since_last_notify_minutes", int(sinceLastNotify.Minutes()),
				"regular_interval_minutes", e.cfg.RegularIntervalMinutes,
			)
			return Decision{
				ShouldNotify: true,
				NotifyType:   NotifyRegularUpdate,
				Reason:       fmt.Sprintf("定期更新: 距上次通知 %d 分钟", int(sinceLastNotify.Minutes())),
			}
		}
	}

	// 规则8：静默保底
	return e.checkSilentHeartbeat(st, now)
}

// checkSilentHeartbeat 检查是否超过最大静默时间，需要发送保底通知。
func (e *Evaluator) checkSilentHeartbeat(st state.State, now time.Time) Decision {
	if e.cfg.MaxSilentMinutes > 0 && !st.LastNotifyAt.IsZero() {
		silentDuration := now.Sub(st.LastNotifyAt)
		if silentDuration >= time.Duration(e.cfg.MaxSilentMinutes)*time.Minute {
			slog.Info("触发静默保底通知",
				"silent_minutes", int(silentDuration.Minutes()),
				"max_silent_minutes", e.cfg.MaxSilentMinutes,
			)
			return Decision{
				ShouldNotify: true,
				NotifyType:   NotifySilentHeartbeat,
				Reason:       fmt.Sprintf("静默保底: 已超过 %d 分钟未通知", e.cfg.MaxSilentMinutes),
			}
		}
	}

	return Decision{
		ShouldNotify: false,
		NotifyType:   NotifyNone,
		Reason:       "未触发任何通知规则",
	}
}

// detectTrendReversal 从价格历史检测趋势反转。
// 寻找连续同向 >= TrendReversalCount 次后出现反向变动。
func (e *Evaluator) detectTrendReversal(history []state.PricePoint, currentPrice float64) (bool, string) {
	// 构建价格序列：历史 + 当前
	prices := make([]float64, 0, len(history)+1)
	for _, p := range history {
		prices = append(prices, p.Price)
	}
	prices = append(prices, currentPrice)

	if len(prices) < e.cfg.TrendReversalCount+2 {
		return false, ""
	}

	// 从倒数第二个变动开始往前看，找连续同向序列
	n := len(prices)
	lastChange := prices[n-1] - prices[n-2] // 最新变动

	if lastChange == 0 {
		return false, ""
	}

	// 往前数连续反向变动（即与最新变动方向相反的连续序列）
	consecutiveCount := 0
	var streakStart, streakEnd float64
	for i := n - 2; i > 0; i-- {
		change := prices[i] - prices[i-1]
		if change == 0 {
			break
		}
		// 检查此变动是否与最新变动方向相反（即构成反转前的趋势）
		if (change > 0 && lastChange < 0) || (change < 0 && lastChange > 0) {
			consecutiveCount++
			streakEnd = prices[i]
			if consecutiveCount == 1 {
				streakStart = prices[i-1]
			} else {
				streakStart = prices[i-1]
			}
		} else {
			break
		}
	}

	if consecutiveCount >= e.cfg.TrendReversalCount {
		direction := "向上"
		if lastChange < 0 {
			direction = "向下"
		}
		prevDirection := "连涨"
		if lastChange > 0 {
			prevDirection = "连跌"
		}
		desc := fmt.Sprintf("趋势反转%s: 前期%s %d 次 (%.2f → %.2f), 当前 %.2f",
			direction, prevDirection, consecutiveCount, streakStart, streakEnd, currentPrice)
		return true, desc
	}

	return false, ""
}

// FormatNotification 根据 NotifyType 生成差异化的通知标题和内容。
func FormatNotification(prefix string, quote *gold.QuoteItem, st state.State, decision Decision, idToName map[string]string, isFusion bool) (title, body string) {
	name := quote.VarietyName
	if idToName != nil {
		if customName, ok := idToName[quote.GoldID]; ok {
			name = customName
		}
	}

	currentPrice := parsePrice(quote.LastPrice)
	dailyRange := fmt.Sprintf("今日: %.2f ~ %.2f", st.DailyLow, st.DailyHigh)
	// 如果 DailyHigh/Low 还没建立好，用当前价
	if st.DailyHigh == 0 || st.DailyLow == 0 {
		dailyRange = fmt.Sprintf("今日: %s", quote.LastPrice)
	}

	switch decision.NotifyType {
	case NotifyPriceChange:
		lastNotifyPrice := parsePrice(st.LastNotifyPrice)
		diff := currentPrice - lastNotifyPrice
		pct := float64(0)
		if lastNotifyPrice > 0 {
			pct = diff / lastNotifyPrice * 100
		}
		arrow := "↑"
		if diff < 0 {
			arrow = "↓"
		}
		title = fmt.Sprintf("%s %s 变动提醒", prefix, name)
		body = fmt.Sprintf("现价: %s %s (%+.2f%%)\n距上次通知: %+.2f\n%s",
			quote.LastPrice, arrow, pct, diff, dailyRange)

	case NotifyTrendReversal:
		direction := "掉头向下 📉"
		if currentPrice > parsePrice(st.LastSuccessPrice) {
			direction = "掉头向上 📈"
		}
		title = fmt.Sprintf("%s %s ⚠️趋势反转", prefix, name)
		// 从历史中取趋势信息
		historyInfo := ""
		if len(st.PriceHistory) >= 2 {
			first := st.PriceHistory[0]
			last := st.PriceHistory[len(st.PriceHistory)-1]
			historyInfo = fmt.Sprintf("\n前期 %.2f → %.2f (%d 个采样点)", first.Price, last.Price, len(st.PriceHistory))
		}
		body = fmt.Sprintf("金价%s\n现价: %s%s\n%s",
			direction, quote.LastPrice, historyInfo, dailyRange)

	case NotifyDailyHigh:
		title = fmt.Sprintf("%s %s 📈今日新高", prefix, name)
		body = fmt.Sprintf("现价: %s（前高 %.2f）\n%s",
			quote.LastPrice, st.DailyHigh, dailyRange)

	case NotifyDailyLow:
		title = fmt.Sprintf("%s %s 📉今日新低", prefix, name)
		body = fmt.Sprintf("现价: %s（前低 %.2f）\n%s",
			quote.LastPrice, st.DailyLow, dailyRange)

	case NotifyRegularUpdate:
		lastNotifyPrice := parsePrice(st.LastNotifyPrice)
		diff := currentPrice - lastNotifyPrice
		pct := float64(0)
		if lastNotifyPrice > 0 {
			pct = diff / lastNotifyPrice * 100
		}
		arrow := "→"
		if diff > 0 {
			arrow = "↑"
		} else if diff < 0 {
			arrow = "↓"
		}
		sinceMin := int(time.Since(st.LastNotifyAt).Minutes())
		title = fmt.Sprintf("%s %s 定期行情", prefix, name)
		body = fmt.Sprintf("现价: %s %s (%+.2f%%)\n%s\n距上次通知: %d 分钟",
			quote.LastPrice, arrow, pct, dailyRange, sinceMin)

	case NotifySilentHeartbeat:
		silentMin := int(time.Since(st.LastNotifyAt).Minutes())
		title = fmt.Sprintf("%s %s 心跳", prefix, name)
		body = fmt.Sprintf("现价: %s\n%s\n已静默 %d 分钟",
			quote.LastPrice, dailyRange, silentMin)

	case NotifyFirstTime:
		// 首次通知使用原有格式
		title = FormatNotifyTitle(prefix, quote, idToName)
		body = FormatNotifyBody(quote, isFusion)
		return title, body

	default:
		title = FormatNotifyTitle(prefix, quote, idToName)
		body = FormatNotifyBody(quote, isFusion)
		return title, body
	}

	// 追加更新时间
	body += fmt.Sprintf("\n更新时间: %s", quote.Uptime)
	return title, body
}

// parsePrice 解析价格字符串为 float64。
func parsePrice(priceStr string) float64 {
	s := strings.TrimSpace(priceStr)
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		slog.Debug("解析价格失败", "price", priceStr, "error", err)
		return 0
	}
	return val
}

// parseChangePercent 解析涨跌幅字符串（如 "1.5%"）为浮点数。
func parseChangePercent(margin string) float64 {
	s := strings.TrimSuffix(strings.TrimSpace(margin), "%")
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		slog.Warn("解析涨跌幅失败", "margin", margin, "error", err)
		return 0
	}
	return val
}

// FormatNotifyBody 根据行情数据格式化通知消息体。
// isFusion 为 true 时使用融合行情格式（不含涨跌额/幅，增加买卖价和高低价）。
func FormatNotifyBody(quote *gold.QuoteItem, isFusion bool) string {
	if isFusion {
		return fmt.Sprintf("现价: %s，买入价: %s，卖出价: %s\n最高价: %s，最低价: %s\n更新时间: %s",
			quote.LastPrice,
			quote.BuyPrice,
			quote.SellPrice,
			quote.HighPrice,
			quote.LowPrice,
			quote.Uptime,
		)
	}
	return fmt.Sprintf("现价: %s\n涨跌: %s (%s)\n更新时间: %s",
		quote.LastPrice,
		quote.ChangePrice,
		quote.ChangeMargin,
		quote.Uptime,
	)
}

// FormatNotifyTitle 根据行情数据格式化通知标题。
// idToName 为品种 ID 到自定义名称的映射，优先使用映射中的名称，否则使用 API 返回的品种名称。
func FormatNotifyTitle(prefix string, quote *gold.QuoteItem, idToName map[string]string) string {
	name := quote.VarietyName
	if idToName != nil {
		if customName, ok := idToName[quote.GoldID]; ok {
			name = customName
		}
	}
	return fmt.Sprintf("%s %s", prefix, name)
}

// Package strategy 负责根据行情数据和历史状态判断是否需要发送通知。
// 支持基于 uptime 去重、价格变化阈值过滤和静默保底机制。
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

// Decision 表示策略判定结果。
type Decision struct {
	ShouldNotify bool   // 是否需要发送通知
	Reason       string // 判定原因
}

// StrategyConfig 定义策略参数。
type StrategyConfig struct {
	DedupByUptime    bool    // 是否基于 uptime 去重
	MinChangePercent float64 // 最小涨跌幅阈值（百分比，0 表示不过滤）
	MaxSilentMinutes int     // 最大静默时间（分钟），超过后强制发送心跳
	IsFusion         bool    // 是否为融合行情模式（融合行情的涨跌额/幅恒为 0，需特殊处理）
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
func (e *Evaluator) Evaluate(quote *gold.QuoteItem, st state.State) Decision {
	now := time.Now()

	// 规则1：基于 uptime 去重——如果行情更新时间与上次相同，说明数据没有变化
	if e.cfg.DedupByUptime && quote.Uptime == st.LastSuccessUptime {
		// 检查静默保底：即使数据没变化，超过最大静默时间也要发一条心跳
		if e.cfg.MaxSilentMinutes > 0 && !st.LastNotifyAt.IsZero() {
			silentDuration := now.Sub(st.LastNotifyAt)
			if silentDuration >= time.Duration(e.cfg.MaxSilentMinutes)*time.Minute {
				slog.Info("触发静默保底通知",
					"silent_minutes", int(silentDuration.Minutes()),
					"max_silent_minutes", e.cfg.MaxSilentMinutes,
				)
				return Decision{
					ShouldNotify: true,
					Reason:       fmt.Sprintf("静默保底: 已超过 %d 分钟未通知", e.cfg.MaxSilentMinutes),
				}
			}
		}

		return Decision{
			ShouldNotify: false,
			Reason:       "行情未更新（uptime 相同）",
		}
	}

	// 规则2：价格变化阈值过滤（融合行情涨跌幅恒为 0，跳过该规则）
	if e.cfg.MinChangePercent > 0 && !e.cfg.IsFusion {
		changePercent := parseChangePercent(quote.ChangeMargin)
		if math.Abs(changePercent) < e.cfg.MinChangePercent {
			slog.Info("涨跌幅未达阈值，不通知",
				"change_percent", changePercent,
				"min_change_percent", e.cfg.MinChangePercent,
			)

			// 即使不满足阈值，也需要检查静默保底
			if e.cfg.MaxSilentMinutes > 0 && !st.LastNotifyAt.IsZero() {
				silentDuration := now.Sub(st.LastNotifyAt)
				if silentDuration >= time.Duration(e.cfg.MaxSilentMinutes)*time.Minute {
					return Decision{
						ShouldNotify: true,
						Reason:       fmt.Sprintf("静默保底: 已超过 %d 分钟未通知", e.cfg.MaxSilentMinutes),
					}
				}
			}

			return Decision{
				ShouldNotify: false,
				Reason:       fmt.Sprintf("涨跌幅 %.2f%% 未达阈值 %.2f%%", math.Abs(changePercent), e.cfg.MinChangePercent),
			}
		}
	}

	// 规则3：首次通知或首次启动（没有历史记录）
	if st.LastNotifyAt.IsZero() {
		return Decision{
			ShouldNotify: true,
			Reason:       "首次行情通知",
		}
	}

	return Decision{
		ShouldNotify: true,
		Reason:       "行情已更新",
	}
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

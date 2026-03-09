// Package health 提供运行状态暴露和手动触发能力。
// 包含 /healthz 健康检查接口和 /trigger 手动触发接口。
package health

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"gd_notice/internal/state"
)

// Status 表示健康检查返回的状态信息。
type Status struct {
	StatusText          string    `json:"status"`
	LastFetchAt         time.Time `json:"last_fetch_at"`
	LastSuccessUptime   string    `json:"last_success_uptime"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	InWindow            bool      `json:"in_window"`
}

// Handler 处理健康检查和手动触发请求。
type Handler struct {
	store      *state.Store
	inWindowFn func() bool // 判断当前是否在时间窗口内的函数
	triggerFn  func()      // 手动触发一次任务的函数
}

// NewHandler 创建 Handler 实例。
func NewHandler(store *state.Store, inWindowFn func() bool, triggerFn func()) *Handler {
	return &Handler{
		store:      store,
		inWindowFn: inWindowFn,
		triggerFn:  triggerFn,
	}
}

// RegisterRoutes 注册 HTTP 路由。
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("POST /trigger", h.handleTrigger)
}

// handleHealthz 处理健康检查请求。
func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	st := h.store.Get()

	status := Status{
		StatusText:          "ok",
		LastFetchAt:         st.LastFetchAt,
		LastSuccessUptime:   st.LastSuccessUptime,
		ConsecutiveFailures: st.ConsecutiveFailures,
		InWindow:            h.inWindowFn(),
	}

	if st.ConsecutiveFailures > 0 {
		status.StatusText = "degraded"
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(status); err != nil {
		slog.Error("写入健康检查响应失败", "error", err)
	}
}

// handleTrigger 处理手动触发请求，立即执行一次完整拉取-判定-推送流程。
func (h *Handler) handleTrigger(w http.ResponseWriter, r *http.Request) {
	slog.Info("收到手动触发请求")

	go h.triggerFn()

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]string{"status": "triggered"}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("写入触发响应失败", "error", err)
	}
}

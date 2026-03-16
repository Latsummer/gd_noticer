// Package notifier 负责通过 Bark API 向苹果设备推送通知。
// 支持多个 device_key，即向多个用户同时推送消息。
package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// barkRequest 表示 Bark 推送 API 的请求体。
type barkRequest struct {
	Title     string `json:"title"`
	Body      string `json:"body"`
	Group     string `json:"group"`
	DeviceKey string `json:"device_key"`
}

// barkResponse 表示 Bark 推送 API 的响应体。
type barkResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// BarkConfig 定义 Bark 推送器的配置参数。
type BarkConfig struct {
	Endpoint           string
	DeviceKeys         []string
	Group              string
	HTTPTimeoutSeconds int
}

// BarkNotifier 是 Bark 推送通知器。
type BarkNotifier struct {
	cfg        BarkConfig
	httpClient *http.Client
}

// NewBarkNotifier 创建 Bark 推送通知器实例。
func NewBarkNotifier(cfg BarkConfig) *BarkNotifier {
	return &BarkNotifier{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.HTTPTimeoutSeconds) * time.Second,
		},
	}
}

// SendResult 表示单个设备推送的结果。
type SendResult struct {
	DeviceKey string // 设备密钥（脱敏显示）
	Success   bool   // 是否成功
	Error     error  // 错误信息（成功时为 nil）
}

// Send 向所有配置的设备推送通知。
// 单个设备推送失败不影响其他设备，返回每个设备的推送结果。
func (b *BarkNotifier) Send(ctx context.Context, title, body string) []SendResult {
	results := make([]SendResult, 0, len(b.cfg.DeviceKeys))

	for _, key := range b.cfg.DeviceKeys {
		maskedKey := maskDeviceKey(key)

		err := b.sendToDevice(ctx, key, title, body)
		if err != nil {
			slog.Error("Bark 推送失败",
				"device_key", maskedKey,
				"error", err,
			)
			results = append(results, SendResult{
				DeviceKey: maskedKey,
				Success:   false,
				Error:     err,
			})
		} else {
			slog.Info("Bark 推送成功", "device_key", maskedKey)
			results = append(results, SendResult{
				DeviceKey: maskedKey,
				Success:   true,
			})
		}
	}

	return results
}

// HasAnySuccess 检查是否至少有一个设备推送成功。
func HasAnySuccess(results []SendResult) bool {
	for _, r := range results {
		if r.Success {
			return true
		}
	}
	return false
}

// sendToDevice 向单个设备发送推送通知。
func (b *BarkNotifier) sendToDevice(ctx context.Context, deviceKey, title, body string) error {
	reqBody := barkRequest{
		Title:     title,
		Body:      body,
		Group:     b.cfg.Group,
		DeviceKey: deviceKey,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("序列化请求体失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.Endpoint, bytes.NewReader(jsonData))
	if err != nil {
		return fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("发送 HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取响应体失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP 响应状态码异常: %d, body: %s", resp.StatusCode, string(respBody))
	}

	var barkResp barkResponse
	if err := json.Unmarshal(respBody, &barkResp); err != nil {
		return fmt.Errorf("解析响应体失败: %w", err)
	}

	if barkResp.Code != 200 {
		return fmt.Errorf("bark API 返回错误: code=%d, message=%s", barkResp.Code, barkResp.Message)
	}

	return nil
}

// maskDeviceKey 对设备密钥进行脱敏处理，仅显示前4位和后4位。
func maskDeviceKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

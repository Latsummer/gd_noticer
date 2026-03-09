// Package gold 负责调用黄金报价 API，完成响应解析与错误归类。
// 支持超时控制和指数退避重试。
package gold

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// QuoteItem 表示单条黄金行情数据。
type QuoteItem struct {
	GoldID       string `json:"goldid"`        // 品种 ID
	Variety      string `json:"variety"`       // 品种代码
	VarietyName  string `json:"varietynm"`     // 品种名称
	LastPrice    string `json:"last_price"`    // 当前价
	BuyPrice     string `json:"buy_price"`     // 买入价
	SellPrice    string `json:"sell_price"`    // 卖出价
	HighPrice    string `json:"high_price"`    // 最高价
	LowPrice     string `json:"low_price"`     // 最低价
	OpenPrice    string `json:"open_price"`    // 开盘价
	YesyPrice    string `json:"yesy_price"`    // 昨收价
	ChangePrice  string `json:"change_price"`  // 涨跌额
	ChangeMargin string `json:"change_margin"` // 涨跌幅
	Uptime       string `json:"uptime"`        // 更新时间
}

// apiResponse 表示黄金 API 的原始响应结构。
type apiResponse struct {
	Success string `json:"success"`
	MsgID   string `json:"msgid"`
	Msg     string `json:"msg"`
	Result  struct {
		DtList map[string]QuoteItem `json:"dtList"`
	} `json:"result"`
}

// ClientConfig 定义客户端初始化参数。
type ClientConfig struct {
	BaseURL             string
	GoldID              string
	AppKey              string
	Sign                string
	HTTPTimeoutSeconds  int
	MaxRetries          int
	RetryBackoffSeconds int
}

// Client 是黄金报价 API 的客户端。
type Client struct {
	cfg        ClientConfig
	httpClient *http.Client
}

// NewClient 创建黄金报价 API 客户端实例。
func NewClient(cfg ClientConfig) *Client {
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.HTTPTimeoutSeconds) * time.Second,
		},
	}
}

// FetchQuote 拉取黄金行情数据。包含重试逻辑，返回解析后的行情数据。
func (c *Client) FetchQuote(ctx context.Context) (*QuoteItem, error) {
	var lastErr error

	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(c.cfg.RetryBackoffSeconds*attempt) * time.Second
			slog.Warn("黄金 API 请求重试",
				"attempt", attempt,
				"backoff", backoff,
				"last_error", lastErr,
			)

			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("请求被取消: %w", ctx.Err())
			case <-time.After(backoff):
			}
		}

		quote, err := c.doFetch(ctx)
		if err == nil {
			return quote, nil
		}
		lastErr = err
	}

	return nil, fmt.Errorf("黄金 API 请求在 %d 次重试后仍然失败: %w", c.cfg.MaxRetries, lastErr)
}

// doFetch 执行一次 API 请求。
func (c *Client) doFetch(ctx context.Context) (*QuoteItem, error) {
	reqURL, err := c.buildURL()
	if err != nil {
		return nil, fmt.Errorf("构建请求 URL 失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送 HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP 响应状态码异常: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %w", err)
	}

	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("解析 JSON 响应失败: %w", err)
	}

	if apiResp.Success != "1" {
		return nil, fmt.Errorf("API 返回错误: msgid=%s, msg=%s", apiResp.MsgID, apiResp.Msg)
	}

	quote, ok := apiResp.Result.DtList[c.cfg.GoldID]
	if !ok {
		return nil, fmt.Errorf("响应中未找到 gold_id=%s 的数据", c.cfg.GoldID)
	}

	return &quote, nil
}

// buildURL 构建完整的 API 请求 URL。
func (c *Client) buildURL() (string, error) {
	base, err := url.Parse(c.cfg.BaseURL)
	if err != nil {
		return "", fmt.Errorf("解析 base_url 失败: %w", err)
	}

	params := url.Values{}
	params.Set("app", "finance.gold_price")
	params.Set("goldid", c.cfg.GoldID)
	params.Set("appkey", c.cfg.AppKey)
	params.Set("sign", c.cfg.Sign)
	params.Set("format", "json")

	base.RawQuery = params.Encode()
	return base.String(), nil
}

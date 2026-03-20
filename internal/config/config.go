// Package config 负责加载、解析和校验应用配置。
// 支持从 YAML 文件读取配置，并通过环境变量覆盖敏感字段。
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 是应用的顶层配置结构。
type Config struct {
	Service     ServiceConfig     `yaml:"service"`
	GoldAPI     GoldAPIConfig     `yaml:"gold_api"`
	Notify      NotifyConfig      `yaml:"notify"`
	Strategy    StrategyConfig    `yaml:"strategy"`
	Reliability ReliabilityConfig `yaml:"reliability"`
	State       StateConfig       `yaml:"state"`
	HTTPServer  HTTPServerConfig  `yaml:"http_server"`
	Log         LogConfig         `yaml:"log"`
}

// ServiceConfig 定义服务运行时参数。
type ServiceConfig struct {
	Timezone               string `yaml:"timezone"`
	PollIntervalSeconds    int    `yaml:"poll_interval_seconds"`
	WindowStart            string `yaml:"window_start"`
	WindowEnd              string `yaml:"window_end"`
	AdaptivePoll           bool   `yaml:"adaptive_poll"`             // 是否启用自适应轮询间隔
	MinPollIntervalSeconds int    `yaml:"min_poll_interval_seconds"` // 自适应轮询最小间隔（秒）
	MaxPollIntervalSeconds int    `yaml:"max_poll_interval_seconds"` // 自适应轮询最大间隔（秒）
}

// GoldAPIConfig 定义黄金报价 API 的连接参数。
type GoldAPIConfig struct {
	ApiType      string            `yaml:"api_type"`       // API 类型：standard（标准行情）或 fusion（融合行情），默认 standard
	BaseURL      string            `yaml:"base_url"`       // API 地址（标准行情与融合行情共用）
	GoldID       string            `yaml:"gold_id"`        // 标准行情品种 ID
	FusionGoldID string            `yaml:"fusion_gold_id"` // 融合行情品种 ID
	AppKey       string            `yaml:"app_key"`
	Sign         string            `yaml:"sign"`
	IDToName     map[string]string `yaml:"id_to_name"` // 品种 ID 到自定义名称的映射（用于通知标题）
}

// NotifyConfig 定义 Bark 推送通知参数，支持多个设备。
// DeviceKeysRaw 为原始配置字符串（多个 key 用英文逗号分隔），解析后存入 DeviceKeys。
type NotifyConfig struct {
	BarkEndpoint  string   `yaml:"bark_endpoint"`
	DeviceKeysRaw string   `yaml:"device_keys"`
	DeviceKeys    []string `yaml:"-"` // 解析后的设备密钥列表，不直接从 YAML 读取
	Group         string   `yaml:"group"`
	TitlePrefix   string   `yaml:"title_prefix"`
}

// StrategyConfig 定义通知策略参数。
type StrategyConfig struct {
	DedupByUptime          bool    `yaml:"dedup_by_uptime"`
	MinChangePercent       float64 `yaml:"min_change_percent"`
	MaxSilentMinutes       int     `yaml:"max_silent_minutes"`
	NotifyChangePercent    float64 `yaml:"notify_change_percent"`    // 距上次通知的累计变化 % 阈值（默认 0.1）
	TrendReversalCount     int     `yaml:"trend_reversal_count"`     // 连续同向 N 次后反转视为拐点（默认 3）
	PriceHistorySize       int     `yaml:"price_history_size"`       // 价格历史滑动窗口大小（默认 10）
	RegularIntervalMinutes int     `yaml:"regular_interval_minutes"` // 定期更新间隔分钟数（默认 30，0 表示禁用）
}

// ReliabilityConfig 定义可靠性相关参数。
type ReliabilityConfig struct {
	HTTPTimeoutSeconds    int `yaml:"http_timeout_seconds"`
	MaxRetries            int `yaml:"max_retries"`
	RetryBackoffSeconds   int `yaml:"retry_backoff_seconds"`
	FailureAlertThreshold int `yaml:"failure_alert_threshold"`
}

// StateConfig 定义状态持久化文件路径。
type StateConfig struct {
	FilePath string `yaml:"file_path"`
}

// HTTPServerConfig 定义内置 HTTP 服务参数。
type HTTPServerConfig struct {
	Enable     bool   `yaml:"enable"`
	ListenAddr string `yaml:"listen_addr"`
}

// LogConfig 定义日志输出参数。
// Output 支持 "stdout"（标准输出）或文件路径（如 "./logs/app.log"）。
// Level 支持 "debug"、"info"、"warn"、"error"，默认为 "info"。
type LogConfig struct {
	Output string `yaml:"output"`
	Level  string `yaml:"level"`
}

// timeFormatRegex 用于校验 HH:MM:SS 格式。
var timeFormatRegex = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}$`)

// Load 从指定路径加载配置文件，展开环境变量后解析为 Config 结构。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	// 展开配置中的环境变量引用（如 ${GOLD_APP_KEY}）
	expanded := os.ExpandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 设置新增配置项的默认值
	if cfg.Strategy.NotifyChangePercent == 0 {
		cfg.Strategy.NotifyChangePercent = 0.1
	}
	if cfg.Strategy.TrendReversalCount == 0 {
		cfg.Strategy.TrendReversalCount = 3
	}
	if cfg.Strategy.PriceHistorySize == 0 {
		cfg.Strategy.PriceHistorySize = 10
	}
	if cfg.Strategy.RegularIntervalMinutes == 0 {
		cfg.Strategy.RegularIntervalMinutes = 30
	}
	if cfg.Service.MinPollIntervalSeconds == 0 {
		cfg.Service.MinPollIntervalSeconds = 180
	}
	if cfg.Service.MaxPollIntervalSeconds == 0 {
		cfg.Service.MaxPollIntervalSeconds = 1200
	}

	// 解析逗号分隔的 device_keys 字符串为列表
	cfg.Notify.DeviceKeys = splitAndTrim(cfg.Notify.DeviceKeysRaw)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置校验失败: %w", err)
	}

	return &cfg, nil
}

// Validate 对配置项进行完整性和合法性校验。
func (c *Config) Validate() error {
	// 服务配置校验
	if c.Service.PollIntervalSeconds < 30 {
		return fmt.Errorf("poll_interval_seconds 必须 >= 30, 当前值: %d", c.Service.PollIntervalSeconds)
	}
	if !timeFormatRegex.MatchString(c.Service.WindowStart) {
		return fmt.Errorf("window_start 格式必须为 HH:MM:SS, 当前值: %q", c.Service.WindowStart)
	}
	if !timeFormatRegex.MatchString(c.Service.WindowEnd) {
		return fmt.Errorf("window_end 格式必须为 HH:MM:SS, 当前值: %q", c.Service.WindowEnd)
	}

	// 黄金 API 配置校验
	if strings.TrimSpace(c.GoldAPI.AppKey) == "" {
		return fmt.Errorf("gold_api.app_key 不能为空")
	}
	if strings.TrimSpace(c.GoldAPI.Sign) == "" {
		return fmt.Errorf("gold_api.sign 不能为空")
	}
	// API 类型校验
	apiType := strings.ToLower(strings.TrimSpace(c.GoldAPI.ApiType))
	if apiType == "" {
		apiType = "standard"
		c.GoldAPI.ApiType = apiType
	}
	if apiType != "standard" && apiType != "fusion" {
		return fmt.Errorf("gold_api.api_type 必须为 standard 或 fusion，当前值: %q", c.GoldAPI.ApiType)
	}
	if apiType == "fusion" {
		if strings.TrimSpace(c.GoldAPI.FusionGoldID) == "" {
			return fmt.Errorf("gold_api.fusion_gold_id 在 api_type 为 fusion 时不能为空")
		}
	}

	// 通知配置校验：至少有一个有效的 device_key
	validKeys := 0
	for _, key := range c.Notify.DeviceKeys {
		if strings.TrimSpace(key) != "" {
			validKeys++
		}
	}
	if validKeys == 0 {
		return fmt.Errorf("notify.device_keys 至少需要一个有效的设备密钥")
	}

	// 策略配置校验
	if c.Strategy.MaxSilentMinutes < 0 {
		return fmt.Errorf("strategy.max_silent_minutes 必须 >= 0, 当前值: %d", c.Strategy.MaxSilentMinutes)
	}
	if c.Strategy.NotifyChangePercent < 0 {
		return fmt.Errorf("strategy.notify_change_percent 必须 >= 0, 当前值: %f", c.Strategy.NotifyChangePercent)
	}
	if c.Strategy.TrendReversalCount < 2 {
		return fmt.Errorf("strategy.trend_reversal_count 必须 >= 2, 当前值: %d", c.Strategy.TrendReversalCount)
	}
	if c.Strategy.PriceHistorySize < 3 {
		return fmt.Errorf("strategy.price_history_size 必须 >= 3, 当前值: %d", c.Strategy.PriceHistorySize)
	}

	// 自适应轮询配置校验
	if c.Service.AdaptivePoll {
		if c.Service.MinPollIntervalSeconds < 30 {
			return fmt.Errorf("service.min_poll_interval_seconds 必须 >= 30, 当前值: %d", c.Service.MinPollIntervalSeconds)
		}
		if c.Service.MaxPollIntervalSeconds <= c.Service.MinPollIntervalSeconds {
			return fmt.Errorf("service.max_poll_interval_seconds 必须 > min_poll_interval_seconds")
		}
	}

	// 可靠性配置校验
	if c.Reliability.HTTPTimeoutSeconds <= 0 {
		return fmt.Errorf("reliability.http_timeout_seconds 必须为正整数, 当前值: %d", c.Reliability.HTTPTimeoutSeconds)
	}
	if c.Reliability.MaxRetries <= 0 {
		return fmt.Errorf("reliability.max_retries 必须为正整数, 当前值: %d", c.Reliability.MaxRetries)
	}

	// 日志配置校验
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true, "": true}
	if !validLevels[strings.ToLower(c.Log.Level)] {
		return fmt.Errorf("log.level 必须为 debug/info/warn/error 之一, 当前值: %q", c.Log.Level)
	}

	return nil
}

// splitAndTrim 将逗号分隔的字符串拆分为列表，去除每项前后空白并过滤空值。
func splitAndTrim(raw string) []string {
	var result []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			result = append(result, s)
		}
	}
	return result
}

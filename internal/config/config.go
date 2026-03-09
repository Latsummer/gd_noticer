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
	Timezone            string `yaml:"timezone"`
	PollIntervalSeconds int    `yaml:"poll_interval_seconds"`
	WindowStart         string `yaml:"window_start"`
	WindowEnd           string `yaml:"window_end"`
}

// GoldAPIConfig 定义黄金报价 API 的连接参数。
type GoldAPIConfig struct {
	BaseURL string `yaml:"base_url"`
	GoldID  string `yaml:"gold_id"`
	AppKey  string `yaml:"app_key"`
	Sign    string `yaml:"sign"`
}

// NotifyConfig 定义 Bark 推送通知参数，支持多个设备。
type NotifyConfig struct {
	BarkEndpoint string   `yaml:"bark_endpoint"`
	DeviceKeys   []string `yaml:"device_keys"`
	Group        string   `yaml:"group"`
	TitlePrefix  string   `yaml:"title_prefix"`
}

// StrategyConfig 定义通知策略参数。
type StrategyConfig struct {
	DedupByUptime    bool    `yaml:"dedup_by_uptime"`
	MinChangePercent float64 `yaml:"min_change_percent"`
	MaxSilentMinutes int     `yaml:"max_silent_minutes"`
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

// Package state 负责读写本地状态文件，用于去重和故障恢复。
// 采用原子写入策略（先写临时文件再重命名），防止半写入导致状态损坏。
package state

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State 表示服务运行时需要持久化的状态信息。
type State struct {
	LastSuccessUptime   string    `json:"last_success_uptime"`  // 最近一次成功拉取的行情更新时间
	LastSuccessPrice    string    `json:"last_success_price"`   // 最近一次成功拉取的价格
	LastNotifyAt        time.Time `json:"last_notify_at"`       // 最近一次通知时间
	LastNotifyDigest    string    `json:"last_notify_digest"`   // 最近一次通知摘要（辅助去重）
	ConsecutiveFailures int       `json:"consecutive_failures"` // 连续失败次数
	LastError           string    `json:"last_error"`           // 最近一次错误信息
	LastFetchAt         time.Time `json:"last_fetch_at"`        // 最近一次拉取时间
}

// Store 管理状态的读写与持久化。
type Store struct {
	filePath string
	mu       sync.RWMutex
	state    State
}

// NewStore 创建 Store 实例。启动时尝试从文件加载状态，文件不存在则使用默认空状态。
func NewStore(filePath string) (*Store, error) {
	s := &Store{
		filePath: filePath,
	}

	if err := s.load(); err != nil {
		slog.Warn("加载状态文件失败，使用默认状态", "error", err, "path", filePath)
	}

	return s, nil
}

// Get 返回当前状态的副本。
func (s *Store) Get() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Update 使用传入的函数修改状态，修改完成后自动落盘。
// 写入失败仅记录错误，不阻塞主流程。
func (s *Store) Update(fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fn(&s.state)

	if err := s.save(); err != nil {
		slog.Error("保存状态文件失败", "error", err, "path", s.filePath)
	}
}

// load 从文件加载状态。
func (s *Store) load() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 文件不存在属于正常情况
		}
		return fmt.Errorf("读取状态文件失败: %w", err)
	}

	if err := json.Unmarshal(data, &s.state); err != nil {
		return fmt.Errorf("解析状态文件失败: %w", err)
	}

	return nil
}

// save 将状态原子写入文件。先写入临时文件，然后重命名覆盖目标文件。
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化状态失败: %w", err)
	}

	dir := filepath.Dir(s.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建状态目录失败: %w", err)
	}

	// 原子写入：先写临时文件再重命名
	tmpFile := s.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return fmt.Errorf("写入临时状态文件失败: %w", err)
	}

	if err := os.Rename(tmpFile, s.filePath); err != nil {
		return fmt.Errorf("重命名状态文件失败: %w", err)
	}

	return nil
}

package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

// FileConfigStore stores AppConfig under $XDG_CONFIG_HOME/michael-auto/config.json with 0600 permissions.
type FileConfigStore struct {
	mu   sync.Mutex
	path string
}

// DefaultXDGConfigPath computes the XDG-compliant config file path.
func DefaultXDGConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, ".config")
		} else {
			base = os.TempDir()
		}
	}
	return filepath.Join(base, "michael-auto", "config.json")
}

// NewFileConfigStore initializes a FileConfigStore at the given path (or XDG default if empty).
func NewFileConfigStore(customPath string) *FileConfigStore {
	if customPath == "" {
		customPath = DefaultXDGConfigPath()
	}
	return &FileConfigStore{path: customPath}
}

// Path returns the configuration file location.
func (s *FileConfigStore) Path() string {
	return s.path
}

// Load reads and validates config from disk, or creates and returns DefaultConfig if missing.
func (s *FileConfigStore) Load() (domain.AppConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.DefaultConfig(), nil
		}
		return domain.DefaultConfig(), err
	}

	cfg := domain.DefaultConfig()
	defaultCua := cfg.CuaDriver
	cfg.Profiles = nil

	if err := json.Unmarshal(data, &cfg); err != nil {
		backupPath := s.backupCorruptLocked(data)
		if backupPath != "" {
			return domain.DefaultConfig(), fmt.Errorf("đọc cấu hình JSON lỗi (đã sao lưu bản cũ tại %s): %w", backupPath, err)
		}
		return domain.DefaultConfig(), fmt.Errorf("đọc cấu hình JSON lỗi: %w", err)
	}

	migrated := false
	// Migrate legacy v1/v2 configs smoothly into v3 (Dedicated "Agent Workspace" + Antigravity 2.34+ safety defaults).
	if cfg.Version < domain.CurrentConfigVersion {
		oldVersion := cfg.Version
		cfg.Version = domain.CurrentConfigVersion
		if oldVersion < 2 {
			cfg.CuaDriver.Enabled = true
			cfg.CuaDriver.EnableWayland = true
			cfg.CuaDriver.AgentWorkspaceEnabled = true
			cfg.CuaDriver.RequireEvidenceBeforeActuate = true
			if cfg.CuaDriver.AgentWorkspaceIndex <= 0 {
				cfg.CuaDriver.AgentWorkspaceIndex = defaultCua.AgentWorkspaceIndex
			}
			if cfg.CuaDriver.EvidenceRetentionRuns <= 0 {
				cfg.CuaDriver.EvidenceRetentionRuns = defaultCua.EvidenceRetentionRuns
			}
			if cfg.CuaDriver.QuotaCheckIntervalMs <= 0 {
				cfg.CuaDriver.QuotaCheckIntervalMs = defaultCua.QuotaCheckIntervalMs
			}
			if cfg.CuaDriver.MinIdleBeforeFocusMs <= 0 {
				cfg.CuaDriver.MinIdleBeforeFocusMs = defaultCua.MinIdleBeforeFocusMs
			}
			if cfg.CuaDriver.SignOutCooldownMs <= 0 {
				cfg.CuaDriver.SignOutCooldownMs = defaultCua.SignOutCooldownMs
			}
			if cfg.CuaDriver.BrowserSettleTimeoutMs <= 0 {
				cfg.CuaDriver.BrowserSettleTimeoutMs = defaultCua.BrowserSettleTimeoutMs
			}
		}
		// Standardize workspace name to canonical "Agent Workspace" (P0-1).
		if strings.TrimSpace(cfg.CuaDriver.AgentWorkspaceName) == "" ||
			strings.EqualFold(strings.TrimSpace(cfg.CuaDriver.AgentWorkspaceName), "Michael-Auto-Agent") {
			cfg.CuaDriver.AgentWorkspaceName = domain.DefaultAgentWorkspaceName
		}
		// Never force same-account relogin when operating a multi-profile pool (>= 2 enabled profiles).
		enabledProfs := 0
		for _, p := range cfg.Profiles {
			if p.Enabled {
				enabledProfs++
			}
		}
		if enabledProfs >= 2 {
			cfg.CuaDriver.AllowSameAccountRelogin = false
		}
		migrated = true
	}

	if err := cfg.Validate(); err != nil {
		backupPath := s.backupCorruptLocked(data)
		if backupPath != "" {
			return domain.DefaultConfig(), fmt.Errorf("cấu hình không hợp lệ (đã sao lưu bản cũ tại %s): %w", backupPath, err)
		}
		return domain.DefaultConfig(), fmt.Errorf("cấu hình không hợp lệ: %w", err)
	}

	if migrated {
		_ = s.saveLocked(cfg)
	}
	return cfg, nil
}

func (s *FileConfigStore) backupCorruptLocked(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	backupPath := fmt.Sprintf("%s.corrupt.%s", s.path, time.Now().UTC().Format("20060102_150405"))
	if err := writeAtomic0600(backupPath, raw); err != nil {
		return ""
	}
	return backupPath
}

// Save validates and atomically writes AppConfig with 0700 directory and 0600 file permissions.
func (s *FileConfigStore) Save(cfg domain.AppConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(cfg)
}

func (s *FileConfigStore) saveLocked(cfg domain.AppConfig) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	tmpPath := s.path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, s.path)
}

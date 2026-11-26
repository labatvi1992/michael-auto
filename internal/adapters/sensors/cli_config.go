package sensors

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

// CLIConfigAdapter implements both Tier-1 StateSensor and CLIConfigSyncer
// for documented CLI settings (~/.gemini/antigravity-cli/settings.json).
type CLIConfigAdapter struct {
	mu sync.Mutex
}

// NewCLIConfigAdapter creates a new Tier-1 CLI settings sensor and atomic writer.
func NewCLIConfigAdapter() *CLIConfigAdapter {
	return &CLIConfigAdapter{}
}

// TierName identifies Tier 1.
func (a *CLIConfigAdapter) TierName() string {
	return "cli_config"
}

// ReadActiveModel reads the configured model from settings.json without modifying the file.
func (a *CLIConfigAdapter) ReadActiveModel(settingsPath string) (string, error) {
	if strings.TrimSpace(settingsPath) == "" {
		return "", errors.New("empty CLI settings path")
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return "", err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", err
	}

	for _, key := range []string{"model", "selectedModel", "defaultModel"} {
		if val, ok := raw[key]; ok {
			if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s), nil
			}
		}
	}
	return "", nil
}

// WriteActiveModel atomically updates the model key in settings.json while preserving all other fields.
func (a *CLIConfigAdapter) WriteActiveModel(settingsPath string, model string, dryRun bool) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model name cannot be empty")
	}
	if strings.TrimSpace(settingsPath) == "" || dryRun {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	raw := make(map[string]any)
	if existing, err := os.ReadFile(settingsPath); err == nil && len(strings.TrimSpace(string(existing))) > 0 {
		if err := json.Unmarshal(existing, &raw); err != nil {
			return err
		}
		// Keep a single backup before atomic replacement.
		_ = os.WriteFile(settingsPath+".bak", existing, 0o600)
	}

	// Preserve existing key naming convention if present, defaulting to "model".
	targetKey := "model"
	for _, k := range []string{"model", "selectedModel", "defaultModel"} {
		if _, exists := raw[k]; exists {
			targetKey = k
			break
		}
	}
	raw[targetKey] = model

	encoded, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if !json.Valid(encoded) {
		return errors.New("encoded CLI settings failed JSON validation")
	}

	dir := filepath.Dir(settingsPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmpPath := settingsPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(encoded); err != nil {
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
	return os.Rename(tmpPath, settingsPath)
}

// Observe implements ports.StateSensor for Tier 1.
func (a *CLIConfigAdapter) Observe(ctx context.Context, cfg domain.AppConfig) (domain.ScreenState, error) {
	_ = ctx
	state := domain.ScreenState{
		ObservedAt:   time.Now(),
		AppAuthState: domain.AuthUnknown,
		SourceTier:   a.TierName(),
	}
	model, err := a.ReadActiveModel(cfg.CLISettingsPath)
	if err == nil && model != "" {
		state.ActiveModel = model
		state.Confidence = 1.0
	}
	return state, nil
}

// Health reports whether the CLI settings directory/file is accessible.
func (a *CLIConfigAdapter) Health(ctx context.Context, cfg domain.AppConfig) domain.SensorHealth {
	_ = ctx
	now := time.Now()
	if strings.TrimSpace(cfg.CLISettingsPath) == "" {
		return domain.SensorHealth{
			TierName:    a.TierName(),
			Healthy:     false,
			Detail:      "Chưa cấu hình đường dẫn settings.json của CLI",
			LastChecked: now,
		}
	}
	if _, err := os.Stat(cfg.CLISettingsPath); err == nil {
		return domain.SensorHealth{
			TierName:    a.TierName(),
			Healthy:     true,
			Detail:      "Đang theo dõi hợp lệ " + filepath.Base(cfg.CLISettingsPath),
			LastChecked: now,
		}
	}
	return domain.SensorHealth{
		TierName:    a.TierName(),
		Healthy:     true,
		Detail:      "Sẵn sàng đồng bộ (tệp settings.json chưa được tạo)",
		LastChecked: now,
	}
}

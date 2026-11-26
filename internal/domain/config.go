package domain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var safeProfileDirRegex = regexp.MustCompile(`^[a-zA-Z0-9._ -]{1,128}$`)

// DefaultAgentWorkspaceName is the canonical dedicated GNOME Wayland workspace name
// reserved exclusively for Michael Auto automation (Antigravity + Chrome OAuth).
const (
	DefaultAgentWorkspaceName = "Agent Workspace"
	CurrentConfigVersion      = 3
)

// BrowserType identifies supported browsers for isolated profile routing.
type BrowserType string

const (
	BrowserChrome   BrowserType = "chrome"
	BrowserChromium BrowserType = "chromium"
	BrowserBrave    BrowserType = "brave"
	BrowserEdge     BrowserType = "edge"
	BrowserFirefox  BrowserType = "firefox"
	BrowserCustom   BrowserType = "custom"
)

// DiscoveredBrowserProfile represents read-only profile metadata discovered from browser Local State.
type DiscoveredBrowserProfile struct {
	BrowserType BrowserType `json:"browserType"`
	ProfileDir  string      `json:"profileDir"`
	DisplayName string      `json:"displayName"`
	EmailHint   string      `json:"emailHint"`
}

// AccountProfile defines an isolated account workspace configuration.
// Per ADR-004 and ADR-005, no passwords, cookies, or OAuth tokens are stored here.
type AccountProfile struct {
	ID                string      `json:"id"`
	Name              string      `json:"name"`
	EmailHint         string      `json:"emailHint"`
	BrowserType       BrowserType `json:"browserType"`
	BrowserBinary     string      `json:"browserBinary,omitempty"`
	BrowserProfileDir string      `json:"browserProfileDir"`
	AuthStartURL      string      `json:"authStartUrl,omitempty"`
	PrimaryModel      string      `json:"primaryModel"`
	FallbackModels    []string    `json:"fallbackModels"`
	Enabled           bool        `json:"enabled"`
	Priority          int         `json:"priority"`
	ExhaustedUntil    *time.Time  `json:"exhaustedUntil,omitempty"`
	ExhaustedModels   []string    `json:"exhaustedModels,omitempty"`
}

// IsExhausted reports whether the entire profile is marked exhausted at time now.
func (p AccountProfile) IsExhausted(now time.Time) bool {
	if p.ExhaustedUntil == nil {
		return false
	}
	return now.Before(*p.ExhaustedUntil)
}

// IsModelExhausted reports whether a specific model on this profile was marked exhausted.
func (p AccountProfile) IsModelExhausted(model string, now time.Time) bool {
	if p.IsExhausted(now) {
		return true
	}
	for _, m := range p.ExhaustedModels {
		if strings.EqualFold(strings.TrimSpace(m), strings.TrimSpace(model)) {
			return true
		}
	}
	return false
}

// NextAvailableFallback returns the next non-exhausted fallback model on this profile.
func (p AccountProfile) NextAvailableFallback(currentModel string, now time.Time) (string, bool) {
	candidates := make([]string, 0, 1+len(p.FallbackModels))
	if p.PrimaryModel != "" {
		candidates = append(candidates, p.PrimaryModel)
	}
	candidates = append(candidates, p.FallbackModels...)

	for _, candidate := range candidates {
		c := strings.TrimSpace(candidate)
		if c == "" || strings.EqualFold(c, strings.TrimSpace(currentModel)) {
			continue
		}
		if !p.IsModelExhausted(c, now) {
			return c, true
		}
	}
	return "", false
}

// CuaDriverConfig configures the trycua/cua (cua-driver) Python sidecar, Fedora 45 Agent Workspace,
// and mandatory Pre-Actuation Screenshot Evidence Vault (ADR-007).
type CuaDriverConfig struct {
	Enabled                      bool   `json:"enabled"`
	PythonBin                    string `json:"pythonBin,omitempty"`
	SessionName                  string `json:"sessionName,omitempty"`
	QuotaCheckIntervalMs         int    `json:"quotaCheckIntervalMs"`
	MinIdleBeforeFocusMs         int    `json:"minIdleBeforeFocusMs"`
	SignOutCooldownMs            int    `json:"signOutCooldownMs"`
	BrowserSettleTimeoutMs       int    `json:"browserSettleTimeoutMs"`
	DefaultModel                 string `json:"defaultModel,omitempty"`
	AllowSameAccountRelogin      bool   `json:"allowSameAccountRelogin"`
	EnableWayland                bool   `json:"enableWayland"`
	DeliveryMode                 string `json:"deliveryMode,omitempty"`
	AgentWorkspaceEnabled        bool   `json:"agentWorkspaceEnabled"`
	AgentWorkspaceIndex          int    `json:"agentWorkspaceIndex"`
	AgentWorkspaceName           string `json:"agentWorkspaceName,omitempty"`
	RequireEvidenceBeforeActuate bool   `json:"requireEvidenceBeforeActuate"`
	MaskEditorInEvidence         bool   `json:"maskEditorInEvidence"`
	EvidenceRetentionRuns        int    `json:"evidenceRetentionRuns"`
}

// AppConfig holds the complete user-editable configuration stored in XDG_CONFIG_HOME.
// Legacy JSON fields (such as deprecated "screenpipe") are safely ignored on load.
type AppConfig struct {
	Version                     int              `json:"version"`
	AutomationEnabled           bool             `json:"automationEnabled"`
	DryRunActuation             bool             `json:"dryRunActuation"`
	YieldPauseSeconds           int              `json:"yieldPauseSeconds"`
	CooldownSeconds             int              `json:"cooldownSeconds"`
	PollIntervalMs              int              `json:"pollIntervalMs"`
	MinConfidence               float64          `json:"minConfidence"`
	ConsensusFrames             int              `json:"consensusFrames"`
	PointerDeadzonePx           int              `json:"pointerDeadzonePx"`
	MaxSwitchesPerWindow        int              `json:"maxSwitchesPerWindow"`
	SwitchWindowMinutes         int              `json:"switchWindowMinutes"`
	ExhaustedCooldownMinutes    int              `json:"exhaustedCooldownMinutes"`
	IntentTTLMinutes            int              `json:"intentTtlMinutes"`
	PreferIntraProfileModelDrop bool             `json:"preferIntraProfileModelDrop"`
	CLISettingsPath             string           `json:"cliSettingsPath"`
	TargetAppKeywords           []string         `json:"targetAppKeywords"`
	ActiveProfileID             string           `json:"activeProfileId"`
	CuaDriver                   CuaDriverConfig  `json:"cuaDriver"`
	Profiles                    []AccountProfile `json:"profiles"`
}

// DefaultConfig returns a safe, Fedora 45 Wayland-ready baseline configuration
// enforcing the 1-minute Quota check ADR, 0% Quota Exhausted Evidence Gate, and Agent Workspace isolation.
func DefaultConfig() AppConfig {
	home, _ := os.UserHomeDir()
	cliPath := ""
	if home != "" {
		cliPath = filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	}
	return AppConfig{
		Version:                     CurrentConfigVersion,
		AutomationEnabled:           true,
		DryRunActuation:             false,
		YieldPauseSeconds:           30,
		CooldownSeconds:             15,
		PollIntervalMs:              2000,
		MinConfidence:               0.95,
		ConsensusFrames:             2,
		PointerDeadzonePx:           4,
		MaxSwitchesPerWindow:        3,
		SwitchWindowMinutes:         30,
		ExhaustedCooldownMinutes:    60,
		IntentTTLMinutes:            10,
		PreferIntraProfileModelDrop: false, // ADR v2.2: Khi Quota chạm 0%, chuyển thẳng sang Account Chrome tiếp theo
		CLISettingsPath:             cliPath,
		TargetAppKeywords:           []string{"Antigravity"},
		ActiveProfileID:             "profile-primary",
		CuaDriver: CuaDriverConfig{
			Enabled:                      true,
			SessionName:                  "michael_auto",
			QuotaCheckIntervalMs:         60000, // ADR bắt buộc: 1 phút bấm nút xem Quota
			MinIdleBeforeFocusMs:         5000,
			SignOutCooldownMs:            60000, // ADR bắt buộc: chờ đúng 1 phút sau SignOut
			BrowserSettleTimeoutMs:       15000,
			DefaultModel:                 "",
			AllowSameAccountRelogin:      false,
			EnableWayland:                true,
			DeliveryMode:                 "auto",
			AgentWorkspaceEnabled:        true,
			AgentWorkspaceIndex:          1, // Workspace #2 ("Agent Workspace") dành riêng cho tự động hóa
			AgentWorkspaceName:           DefaultAgentWorkspaceName,
			RequireEvidenceBeforeActuate: true,
			MaskEditorInEvidence:         true,
			EvidenceRetentionRuns:        20,
		},
		Profiles: []AccountProfile{
			{
				ID:                "profile-primary",
				Name:              "Workspace Chính",
				EmailHint:         "primary@example.com",
				BrowserType:       BrowserChrome,
				BrowserProfileDir: "Default",
				PrimaryModel:      "Gemini 4 Neon Extra High",
				FallbackModels:    []string{"Claude Sonnet 4.6 (Thinking)"},
				Enabled:           true,
				Priority:          1,
			},
			{
				ID:                "profile-backup",
				Name:              "Workspace Dự phòng",
				EmailHint:         "backup@example.com",
				BrowserType:       BrowserChrome,
				BrowserProfileDir: "Profile 1",
				PrimaryModel:      "Gemini 4 Neon Extra High",
				FallbackModels:    []string{"Claude Sonnet 4.6 (Thinking)"},
				Enabled:           false,
				Priority:          2,
			},
		},
	}
}

// Validate enforces safety invariants on user-supplied configuration.
func (c *AppConfig) Validate() error {
	if c.YieldPauseSeconds < 5 || c.YieldPauseSeconds > 3600 {
		return fmt.Errorf("yieldPauseSeconds (%d) phải nằm trong khoảng [5, 3600]", c.YieldPauseSeconds)
	}
	if c.CooldownSeconds < 1 || c.CooldownSeconds > 3600 {
		return fmt.Errorf("cooldownSeconds (%d) phải nằm trong khoảng [1, 3600]", c.CooldownSeconds)
	}
	if c.PollIntervalMs < 250 || c.PollIntervalMs > 60000 {
		return fmt.Errorf("pollIntervalMs (%d) phải nằm trong khoảng [250, 60000]", c.PollIntervalMs)
	}
	if c.MinConfidence < 0.50 || c.MinConfidence > 1.0 {
		return fmt.Errorf("minConfidence (%.2f) phải nằm trong khoảng [0.50, 1.00]", c.MinConfidence)
	}
	if c.ConsensusFrames < 1 || c.ConsensusFrames > 20 {
		return fmt.Errorf("consensusFrames (%d) phải nằm trong khoảng [1, 20]", c.ConsensusFrames)
	}
	if c.PointerDeadzonePx < 0 || c.PointerDeadzonePx > 50 {
		return fmt.Errorf("pointerDeadzonePx (%d) phải nằm trong khoảng [0, 50]", c.PointerDeadzonePx)
	}
	if c.MaxSwitchesPerWindow < 1 || c.MaxSwitchesPerWindow > 50 {
		return fmt.Errorf("maxSwitchesPerWindow (%d) phải nằm trong khoảng [1, 50]", c.MaxSwitchesPerWindow)
	}
	if c.SwitchWindowMinutes < 1 || c.SwitchWindowMinutes > 1440 {
		return fmt.Errorf("switchWindowMinutes (%d) phải nằm trong khoảng [1, 1440]", c.SwitchWindowMinutes)
	}
	if c.ExhaustedCooldownMinutes < 1 {
		c.ExhaustedCooldownMinutes = 60
	}
	if c.IntentTTLMinutes < 1 {
		c.IntentTTLMinutes = 10
	}

	if c.CuaDriver.QuotaCheckIntervalMs < 5000 {
		c.CuaDriver.QuotaCheckIntervalMs = 60000
	}
	if c.CuaDriver.MinIdleBeforeFocusMs < 0 || c.CuaDriver.MinIdleBeforeFocusMs > 300000 {
		c.CuaDriver.MinIdleBeforeFocusMs = 5000
	}
	if c.CuaDriver.SignOutCooldownMs < 1000 {
		c.CuaDriver.SignOutCooldownMs = 60000
	}
	if c.CuaDriver.BrowserSettleTimeoutMs < 2000 {
		c.CuaDriver.BrowserSettleTimeoutMs = 15000
	}
	if c.CuaDriver.SessionName == "" {
		c.CuaDriver.SessionName = "michael_auto"
	}
	if c.CuaDriver.AgentWorkspaceIndex < 0 || c.CuaDriver.AgentWorkspaceIndex > 15 {
		c.CuaDriver.AgentWorkspaceIndex = 1
	}
	if strings.TrimSpace(c.CuaDriver.AgentWorkspaceName) == "" || strings.EqualFold(strings.TrimSpace(c.CuaDriver.AgentWorkspaceName), "Michael-Auto-Agent") {
		c.CuaDriver.AgentWorkspaceName = DefaultAgentWorkspaceName
	}
	if c.CuaDriver.EvidenceRetentionRuns < 1 || c.CuaDriver.EvidenceRetentionRuns > 500 {
		c.CuaDriver.EvidenceRetentionRuns = 20
	}
	switch c.CuaDriver.DeliveryMode {
	case "background", "foreground", "auto":
	default:
		c.CuaDriver.DeliveryMode = "auto"
	}

	seen := make(map[string]bool, len(c.Profiles))
	seenEnabledProfileDirs := make(map[string]string, len(c.Profiles))
	for i, p := range c.Profiles {
		id := strings.TrimSpace(p.ID)
		if id == "" {
			return fmt.Errorf("profiles[%d]: id không được để trống", i)
		}
		if seen[id] {
			return fmt.Errorf("profiles[%d]: trùng lặp id %q", i, id)
		}
		seen[id] = true

		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("profiles[%d] (%s): tên hồ sơ không được để trống", i, id)
		}
		dir := strings.TrimSpace(p.BrowserProfileDir)
		if dir == "" || strings.Contains(dir, "..") || strings.ContainsAny(dir, `/\;|$&`+"`") || !safeProfileDirRegex.MatchString(dir) {
			return fmt.Errorf("profiles[%d] (%s): browserProfileDir %q không hợp lệ", i, id, p.BrowserProfileDir)
		}
		if p.Enabled {
			dirKey := fmt.Sprintf("%s:%s", p.BrowserType, strings.ToLower(dir))
			if prevID, dup := seenEnabledProfileDirs[dirKey]; dup {
				return fmt.Errorf("profiles[%d] (%s): trùng thư mục trình duyệt %q với hồ sơ đang bật %q (mỗi tài khoản cần 1 --profile-directory riêng)", i, id, dir, prevID)
			}
			seenEnabledProfileDirs[dirKey] = id
		}
		if strings.TrimSpace(p.PrimaryModel) == "" {
			return fmt.Errorf("profiles[%d] (%s): primaryModel không được để trống", i, id)
		}
		switch p.BrowserType {
		case BrowserChrome, BrowserChromium, BrowserBrave, BrowserEdge, BrowserFirefox:
		case BrowserCustom:
			if strings.TrimSpace(p.BrowserBinary) == "" {
				return fmt.Errorf("profiles[%d] (%s): browserBinary bắt buộc khi chọn custom", i, id)
			}
		default:
			return fmt.Errorf("profiles[%d] (%s): browserType %q không được hỗ trợ", i, id, p.BrowserType)
		}
	}

	if len(c.Profiles) == 0 {
		return errors.New("cần cấu hình ít nhất một hồ sơ tài khoản")
	}
	return nil
}

// FindProfile returns a pointer to the profile with the given ID, if present.
func (c *AppConfig) FindProfile(id string) *AccountProfile {
	for i := range c.Profiles {
		if c.Profiles[i].ID == id {
			return &c.Profiles[i]
		}
	}
	return nil
}

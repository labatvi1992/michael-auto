package ports

import (
	"context"
	"errors"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

// ErrAuthGateDetected is returned when Password, MFA/2FA, or CAPTCHA is encountered.
var ErrAuthGateDetected = errors.New("phát hiện cổng xác thực bảo mật (Mật khẩu / MFA / CAPTCHA)")

// ErrSecurityViolation is returned when Chrome Omnibox origin, Profile binding, or PID allowlist check fails.
var ErrSecurityViolation = errors.New("vi phạm xác thực an toàn (sai Chrome Profile, sai tên miền Omnibox hoặc sai tiến trình)")

// ErrPostSwitchQuotaExhausted is returned in Step 5 if the newly signed-in account is also already at 0% Quota.
var ErrPostSwitchQuotaExhausted = errors.New("phiên tài khoản mới đăng nhập cũng đang ở trạng thái cạn hạn mức (0% Quota)")

// ErrAccountMismatch is returned in Step 5 if OAuth logged back into a different account than targetProfile.EmailHint.
var ErrAccountMismatch = errors.New("tài khoản sau đăng nhập trên Antigravity không khớp email hồ sơ đích")

// HumanActivityMonitor detects physical human activity and queries Wayland/OS compositor idle time.
type HumanActivityMonitor interface {
	Subscribe(ctx context.Context) (<-chan domain.ActivityKind, error)
	SystemIdleDuration(ctx context.Context) (time.Duration, error)
	AcquireActuationLease(ttl time.Duration, expectedX, expectedY *int) (release func())
	SetActiveExecution(active bool)
	BackendName() string
	Close() error
}

// StateSensor observes quota and UI state from a specific sensing tier.
type StateSensor interface {
	TierName() string
	Observe(ctx context.Context, cfg domain.AppConfig) (domain.ScreenState, error)
	Health(ctx context.Context, cfg domain.AppConfig) domain.SensorHealth
}

// DesktopActuator performs sanctioned UI interactions.
type DesktopActuator interface {
	EnsureSession(ctx context.Context) error
	SessionStatus() (ready bool, detail string)
	TriggerLogout(ctx context.Context, state domain.ScreenState, dryRun bool) error
	SelectModel(ctx context.Context, state domain.ScreenState, model string, dryRun bool) error
	Close() error
}

// CuaDesktopActuator extends DesktopActuator with Verify-Evidence-Execute operations
// across Antigravity and Google Chrome inside the isolated Agent Workspace (ADR-007).
type CuaDesktopActuator interface {
	DesktopActuator
	SetActiveRunID(runID string)
	FocusAppWindow(ctx context.Context, appTarget string, dryRun bool) error
	InspectQuota(ctx context.Context, targetKeywords []string, dryRun bool) (domain.ScreenState, error)
	ClickSignIn(ctx context.Context, dryRun bool) error
	SelectChromeAccount(ctx context.Context, profile domain.AccountProfile, dryRun bool) error
}

// WorkspaceManager manages the isolated Fedora 45 Wayland Agent Workspace separate from User Workspace.
type WorkspaceManager interface {
	Status(ctx context.Context, cfg domain.AppConfig) domain.AgentWorkspaceStatus
	EnsureAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (domain.AgentWorkspaceStatus, error)
	RouteTargetWindowsToAgentWorkspace(ctx context.Context, cfg domain.AppConfig) error
	UserIsOnAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (onAgentWS bool, known bool)
}

// DesktopNotifier sends native OS desktop notifications on critical events (MFA/HardStop or completed switch).
type DesktopNotifier interface {
	Notify(title, body string, critical bool)
}

// EvidenceStore persists pre-actuation screenshot proofs and verification metadata under XDG_STATE_HOME.
type EvidenceStore interface {
	SaveEvidence(runID, step string, pngBytes []byte, meta domain.EvidenceRecord) (domain.EvidenceRecord, error)
	ListRecent(limit int) ([]domain.EvidenceRecord, error)
	ReadDataURL(imagePath string) (string, error)
	Clear() error
	RootDir() string
}

// IntentStore persists PendingIntent ephemerally during a transition and deletes the file immediately on completion.
type IntentStore interface {
	Save(intent domain.PendingIntent) error
	Load() (*domain.PendingIntent, error)
	Clear() error
	Path() string
}

// BrowserProfileRouter opens authentication or workspace URLs inside an isolated browser profile.
type BrowserProfileRouter interface {
	OpenProfileSession(ctx context.Context, profile domain.AccountProfile, dryRun bool) error
}

// CLIConfigSyncer reads and atomically updates documented CLI configuration files.
type CLIConfigSyncer interface {
	ReadActiveModel(settingsPath string) (string, error)
	WriteActiveModel(settingsPath string, model string, dryRun bool) error
}

// ConfigStore persists user configuration safely under XDG_CONFIG_HOME (0600 permissions).
type ConfigStore interface {
	Load() (domain.AppConfig, error)
	Save(cfg domain.AppConfig) error
	Path() string
}

// SecretStore manages sensitive OS tokens via libsecret/keyring.
type SecretStore interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
	Backend() string
}

// AuditLogger writes PII-redacted JSONL operational audit entries with rotation.
type AuditLogger interface {
	Log(entry domain.AuditEntry)
	Recent(limit int) ([]domain.AuditEntry, error)
	Clear() error
	Path() string
	Close() error
}

// StatusPublisher broadcasts RuntimeStatus updates to Wails UI and System Tray.
type StatusPublisher interface {
	PublishStatus(status domain.RuntimeStatus)
}

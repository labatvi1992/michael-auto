package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/labatvi1992/michael-auto/internal/adapters/activity"
	"github.com/labatvi1992/michael-auto/internal/adapters/actuators"
	"github.com/labatvi1992/michael-auto/internal/adapters/sensors"
	"github.com/labatvi1992/michael-auto/internal/adapters/storage"
	"github.com/labatvi1992/michael-auto/internal/adapters/tray"
	"github.com/labatvi1992/michael-auto/internal/adapters/workspace"
	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/orchestrator"
	"github.com/labatvi1992/michael-auto/internal/ports"
)

// App bridges the Go hexagonal orchestrator with the Wails React UI and System Tray.
type App struct {
	mu                 sync.Mutex
	wailsCtx           context.Context
	bgCancel           context.CancelFunc
	forceQuit          bool
	enableTray         bool
	notifiedHiddenOnce bool
	orch               *orchestrator.Orchestrator
	cfgStore           ports.ConfigStore
	intentStore        ports.IntentStore
	evidenceStore      *storage.EvidenceVault
	workspaceMgr       ports.WorkspaceManager
	secretStore        ports.SecretStore
	auditLogger        *storage.JSONLAuditLogger
	notifier           ports.DesktopNotifier
	activityMon        ports.HumanActivityMonitor
	actuator           ports.DesktopActuator
	cuaActuator        ports.CuaDesktopActuator
	cuaImpl            *actuators.CuaDriverActuator
	portalFallback     ports.DesktopActuator
	browserRouter      ports.BrowserProfileRouter
	cliSync            *sensors.CLIConfigAdapter
	trayMgr            *tray.Manager
}

// NewApp wires production adapters for Fedora 45 Wayland with trycua/cua,
// Dedicated Agent Workspace, Ephemeral Intent Store, and Pre-Actuation Evidence Vault.
func NewApp(customConfigPath, customAuditPath string, enableTray bool) (*App, error) {
	cfgStore := storage.NewFileConfigStore(customConfigPath)
	cfg, cfgLoadErr := cfgStore.Load()
	if cfgLoadErr != nil {
		cfg = domain.DefaultConfig()
	}

	auditLogger := storage.NewJSONLAuditLogger(customAuditPath)
	if cfgLoadErr != nil {
		auditLogger.Log(domain.AuditEntry{
			Timestamp: time.Now(),
			Level:     domain.AuditError,
			Event:     "config_load_recovered",
			Message:   "Tệp config.json gặp lỗi và đã được sao lưu an toàn; tạm dùng cấu hình mặc định: " + cfgLoadErr.Error(),
		})
	}
	intentStore := storage.NewEphemeralIntentStore("")
	evidenceStore := storage.NewEvidenceVault("", cfg.CuaDriver.EvidenceRetentionRuns)
	workspaceMgr := workspace.NewWorkspaceManager()
	secretStore := storage.NewSecretStore()
	notifier := tray.NewDesktopNotifier()
	activityMon := activity.NewHumanActivityMonitor()
	browserRouter := actuators.NewOSBrowserRouter()

	cliAdapter := sensors.NewCLIConfigAdapter()
	atspiSensor := sensors.NewATSPI2Sensor()

	redactedStderr := auditLogger.NewRedactingWriter(os.Stderr)
	cua := actuators.NewCuaDriverActuator(cfg.CuaDriver, activityMon, browserRouter, evidenceStore, redactedStderr)
	cuaSensor := sensors.NewCuaDriverSensor(cua)
	portalActuator := actuators.NewDesktopActuator(secretStore, activityMon)

	var primaryActuator ports.DesktopActuator = cua
	if !cfg.CuaDriver.Enabled {
		primaryActuator = portalActuator
	}

	fusion := orchestrator.NewSensorFusion(cliAdapter, cuaSensor, atspiSensor)

	app := &App{
		enableTray:     enableTray,
		cfgStore:       cfgStore,
		intentStore:    intentStore,
		evidenceStore:  evidenceStore,
		workspaceMgr:   workspaceMgr,
		secretStore:    secretStore,
		auditLogger:    auditLogger,
		notifier:       notifier,
		activityMon:    activityMon,
		actuator:       primaryActuator,
		cuaActuator:    cua,
		cuaImpl:        cua,
		portalFallback: portalActuator,
		browserRouter:  browserRouter,
		cliSync:        cliAdapter,
	}

	app.orch = orchestrator.NewOrchestrator(cfg, orchestrator.Dependencies{
		ConfigStore:        cfgStore,
		IntentStore:        intentStore,
		EvidenceStore:      evidenceStore,
		WorkspaceManager:   workspaceMgr,
		ActivityMonitor:    activityMon,
		SensorFusion:       fusion,
		DesktopActuator:    primaryActuator,
		CuaDesktopActuator: cua,
		BrowserRouter:      browserRouter,
		CLISyncer:          cliAdapter,
		AuditLogger:        auditLogger,
		Notifier:           notifier,
		Publisher:          app,
	})
	app.orch.SetAppVersion(AppVersion)
	if cfgLoadErr != nil {
		app.orch.SetConfigWarning("Cấu hình cũ không hợp lệ (đã tự động sao lưu trước khi khôi phục mặc định): " + auditLogger.RedactString(cfgLoadErr.Error()))
	}

	if enableTray {
		app.trayMgr = tray.NewManager(tray.Callbacks{
			OnShowWindow: func() {
				app.ShowMainWindow()
			},
			OnToggleAutomation: func() {
				cur := app.orch.GetConfig()
				_ = app.orch.SetAutomationEnabled(!cur.AutomationEnabled)
			},
			OnResetHardStop: func() {
				app.orch.ResetHardStop()
			},
			OnQuit: func() {
				app.mu.Lock()
				app.forceQuit = true
				ctx := app.wailsCtx
				app.mu.Unlock()
				if ctx != nil {
					wailsruntime.Quit(ctx)
				}
			},
		})
	}

	return app, nil
}

func (a *App) startup(ctx context.Context) {
	bgCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.wailsCtx = ctx
	a.bgCancel = cancel
	a.mu.Unlock()

	if a.trayMgr != nil {
		a.trayMgr.Start()
	}

	a.auditLogger.Log(domain.AuditEntry{
		Timestamp: time.Now(),
		Level:     domain.AuditInfo,
		Event:     "app_started",
		ToState:   a.orch.Status(bgCtx).State,
		Message:   "Khởi chạy Michael Auto v" + AppVersion + " (Dedicated Agent Workspace + Zero-Leak Offscreen Evidence + LockScreen Guard, ActivityBackend=" + a.activityMon.BackendName() + ")",
	})

	_ = a.orch.Start(bgCtx)
}

// ShowMainWindow raises and unminimises the Wails window (invoked by System Tray or Single-Instance Socket wakeup).
func (a *App) ShowMainWindow() {
	a.mu.Lock()
	ctx := a.wailsCtx
	a.mu.Unlock()
	if ctx != nil {
		wailsruntime.WindowShow(ctx)
		wailsruntime.WindowUnminimise(ctx)
	}
}

// QuitApplication terminates the Wails application cleanly bypassing hide-on-close.
func (a *App) QuitApplication() {
	a.mu.Lock()
	a.forceQuit = true
	ctx := a.wailsCtx
	a.mu.Unlock()
	if ctx != nil {
		wailsruntime.Quit(ctx)
	}
}

// HandleControlCommand serves control.sock commands ("status", "pause", "resume", "quit") for GUI and headless daemons.
func (a *App) HandleControlCommand(cmd, mode string, cancelHeadless context.CancelFunc) string {
	switch strings.TrimSpace(cmd) {
	case "status":
		st := a.orch.Status(context.Background())
		return fmt.Sprintf("ok:%s state=%s automation=%v dry_run=%v profile=%s",
			mode, st.State, st.AutomationEnabled, st.DryRunActuation, st.ActiveProfileID)
	case "pause":
		if err := a.orch.SetAutomationEnabled(false); err != nil {
			return "err:" + err.Error()
		}
		return "ok:paused"
	case "resume":
		if err := a.orch.SetAutomationEnabled(true); err != nil {
			return "err:" + err.Error()
		}
		return "ok:resumed"
	case "quit":
		if cancelHeadless != nil {
			go func() {
				time.Sleep(50 * time.Millisecond)
				cancelHeadless()
			}()
		} else {
			go func() {
				time.Sleep(50 * time.Millisecond)
				a.QuitApplication()
			}()
		}
		return "ok:quitting"
	default:
		return "err:unknown-command"
	}
}

func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	a.mu.Lock()
	quit := a.forceQuit || !a.enableTray
	alreadyNotified := a.notifiedHiddenOnce
	if !quit && !alreadyNotified {
		a.notifiedHiddenOnce = true
	}
	a.mu.Unlock()

	if quit {
		return false
	}
	if !alreadyNotified && !tray.IsStatusNotifierHostAvailable() && a.notifier != nil {
		a.notifier.Notify(
			"Michael Auto vẫn đang giám sát nền",
			"Cửa sổ đã thu ẩn an toàn. Bấm lại biểu tượng Michael Auto trong menu ứng dụng bất kỳ lúc nào để mở lại.",
			false,
		)
	}
	wailsruntime.WindowHide(ctx)
	return true
}

func (a *App) shutdown(ctx context.Context) {
	_ = ctx
	a.mu.Lock()
	cancel := a.bgCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if a.trayMgr != nil {
		a.trayMgr.Stop()
	}
	if a.cuaImpl != nil {
		_ = a.cuaImpl.Close()
	}
	if a.portalFallback != nil {
		_ = a.portalFallback.Close()
	}
	_ = a.activityMon.Close()
	_ = a.auditLogger.Close()
}

func (a *App) PublishStatus(status domain.RuntimeStatus) {
	if a.trayMgr != nil {
		a.trayMgr.UpdateStatus(status)
	}
	a.mu.Lock()
	ctx := a.wailsCtx
	a.mu.Unlock()
	if ctx != nil {
		wailsruntime.EventsEmit(ctx, "runtime:status", status)
	}
}

func (a *App) PublishConfig(cfg domain.AppConfig) {
	a.mu.Lock()
	ctx := a.wailsCtx
	a.mu.Unlock()
	if ctx != nil {
		wailsruntime.EventsEmit(ctx, "config:updated", cfg)
	}
}

// --- Wails Bound Frontend Methods ---

func (a *App) GetStatus() domain.RuntimeStatus {
	return a.orch.Status(context.Background())
}

func (a *App) GetConfig() domain.AppConfig {
	return a.orch.GetConfig()
}

func (a *App) SaveConfig(cfg domain.AppConfig) error {
	if a.cuaImpl != nil {
		a.cuaImpl.UpdateSidecarConfig(cfg.CuaDriver)
	}
	if a.evidenceStore != nil {
		a.evidenceStore.SetRetentionRuns(cfg.CuaDriver.EvidenceRetentionRuns)
	}
	var activeAct ports.DesktopActuator = a.portalFallback
	if cfg.CuaDriver.Enabled && a.cuaImpl != nil {
		activeAct = a.cuaImpl
	}
	a.mu.Lock()
	a.actuator = activeAct
	a.mu.Unlock()
	a.orch.SetPrimaryActuator(activeAct)
	return a.orch.UpdateConfig(cfg)
}

func (a *App) SetAutomationEnabled(enabled bool) error {
	return a.orch.SetAutomationEnabled(enabled)
}

func (a *App) SetDryRunActuation(dryRun bool) error {
	cfg := a.orch.GetConfig()
	cfg.DryRunActuation = dryRun
	return a.SaveConfig(cfg)
}

func (a *App) ResetHardStop() {
	if a.cuaImpl != nil {
		a.cuaImpl.ResetBreaker()
	}
	a.orch.ResetHardStop()
	a.orch.InvalidateHealthCache()
}

// DiscoverAntigravityModels opens Antigravity's Model menu briefly and returns live selectable model names.
func (a *App) DiscoverAntigravityModels() ([]string, error) {
	if a.cuaImpl == nil {
		return nil, errors.New("CuaDriverActuator chưa sẵn sàng")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cfg := a.orch.GetConfig()
	models, _, err := a.cuaImpl.ListAvailableModels(ctx, cfg.TargetAppKeywords)
	return models, err
}

func autostartDesktopPath() string {
	base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, ".config")
		} else {
			base = os.TempDir()
		}
	}
	return filepath.Join(base, "autostart", "michael-auto.desktop")
}

// GetAutostartEnabled reports whether XDG desktop autostart (~/.config/autostart/michael-auto.desktop) is enabled.
func (a *App) GetAutostartEnabled() bool {
	_, err := os.Stat(autostartDesktopPath())
	return err == nil
}

// SetAutostartEnabled creates or removes the Fedora XDG autostart desktop entry cleanly (P2-2).
func (a *App) SetAutostartEnabled(enabled bool) error {
	p := autostartDesktopPath()
	if !enabled {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	exePath, err := os.Executable()
	if err != nil || strings.TrimSpace(exePath) == "" {
		exePath = "michael-auto"
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	entry := fmt.Sprintf(`[Desktop Entry]
Type=Application
Version=1.0
Name=Michael Auto
GenericName=AI Quota & Profile Orchestrator
Comment=Tự động giám sát hạn mức và điều phối hồ sơ tài khoản cho Antigravity trên Fedora Wayland
Exec=%s --start-hidden
Icon=michael-auto
Terminal=false
Categories=Utility;Development;
StartupNotify=false
StartupWMClass=michael-auto
X-GNOME-Autostart-enabled=true
`, exePath)
	return os.WriteFile(p, []byte(entry), 0o600)
}

// ExportDiagnostics generates a PII-redacted .tar.gz diagnostics bundle under $XDG_STATE_HOME/michael-auto/diagnostics (P2-3).
func (a *App) ExportDiagnostics() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	stateBase := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if stateBase == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			stateBase = filepath.Join(home, ".local", "state")
		} else {
			stateBase = os.TempDir()
		}
	}
	diagDir := filepath.Join(stateBase, "michael-auto", "diagnostics")
	if err := os.MkdirAll(diagDir, 0o700); err != nil {
		return "", err
	}

	outPath := filepath.Join(diagDir, fmt.Sprintf("michael-auto-diagnostics-%s.tar.gz", time.Now().UTC().Format("20060102_150405")))
	st := a.orch.Status(ctx)
	cfg := a.orch.GetConfig()
	logs, _ := a.auditLogger.Recent(300)

	statusRaw, _ := json.MarshalIndent(st, "", "  ")
	cfgRaw, _ := json.MarshalIndent(cfg, "", "  ")
	logsRaw, _ := json.MarshalIndent(logs, "", "  ")
	inspectStr, inspectErr := a.InspectUISelectors()
	if inspectErr != nil {
		inspectStr = "inspect_ui error: " + inspectErr.Error()
	}

	files := map[string][]byte{
		"runtime_status.json": []byte(a.auditLogger.RedactString(string(statusRaw)) + "\n"),
		"config_redacted.json": []byte(a.auditLogger.RedactString(string(cfgRaw)) + "\n"),
		"audit_recent.json":   []byte(a.auditLogger.RedactString(string(logsRaw)) + "\n"),
		"inspect_ui.json":     []byte(a.auditLogger.RedactString(inspectStr) + "\n"),
		"version.txt":         []byte(fmt.Sprintf("Michael Auto v%s\n", AppVersion)),
	}

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	now := time.Now().UTC()
	fileNames := make([]string, 0, len(files))
	for name := range files {
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)
	for _, name := range fileNames {
		content := files[name]
		hdr := &tar.Header{
			Name:    name,
			Mode:    0o600,
			Size:    int64(len(content)),
			ModTime: now,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = gzw.Close()
			return "", err
		}
		if _, err := tw.Write(content); err != nil {
			_ = tw.Close()
			_ = gzw.Close()
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		_ = gzw.Close()
		return "", err
	}
	if err := gzw.Close(); err != nil {
		return "", err
	}
	if err := os.WriteFile(outPath, buf.Bytes(), 0o600); err != nil {
		return "", err
	}
	a.auditLogger.Log(domain.AuditEntry{
		Timestamp: time.Now(),
		Level:     domain.AuditInfo,
		Event:     "diagnostics_exported",
		Message:   "Đã xuất gói chẩn đoán ẩn danh: " + outPath,
	})
	return outPath, nil
}

func (a *App) GetAuditLogs(limit int) ([]domain.AuditEntry, error) {
	return a.auditLogger.Recent(limit)
}

func (a *App) ClearAuditLogs() error {
	return a.auditLogger.Clear()
}

func (a *App) ListRecentEvidence(limit int) ([]domain.EvidenceRecord, error) {
	if a.evidenceStore == nil {
		return []domain.EvidenceRecord{}, nil
	}
	return a.evidenceStore.ListRecent(limit)
}

func (a *App) ClearEvidenceVault() error {
	if a.evidenceStore == nil {
		return nil
	}
	if err := a.evidenceStore.Clear(); err != nil {
		return err
	}
	a.auditLogger.Log(domain.AuditEntry{
		Timestamp: time.Now(),
		Level:     domain.AuditInfo,
		Event:     "evidence_vault_cleared",
		Message:   "Người dùng đã xóa sạch kho ảnh chụp bằng chứng Evidence Vault",
	})
	return nil
}

func (a *App) GetEvidenceDataURL(imagePath string) (string, error) {
	if a.evidenceStore == nil {
		return "", errors.New("kho lưu trữ Evidence chưa sẵn sàng")
	}
	return a.evidenceStore.ReadDataURL(imagePath)
}

func (a *App) SetupAgentWorkspace() (domain.AgentWorkspaceStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := a.orch.GetConfig()
	st, err := a.workspaceMgr.EnsureAgentWorkspace(ctx, cfg)
	a.orch.InvalidateHealthCache()
	return st, err
}

func (a *App) DiscoverBrowserProfiles() []domain.DiscoveredBrowserProfile {
	return actuators.DiscoverBrowserProfiles("")
}

func (a *App) TriggerManualSwitch(profileID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	return a.orch.TriggerManualSwitch(ctx, profileID)
}

func (a *App) AuthorizeWaylandPortal() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := a.orch.GetConfig()
	target := a.actuator
	if cfg.CuaDriver.Enabled && a.cuaImpl != nil {
		target = a.cuaImpl
	}
	if err := target.EnsureSession(ctx); err != nil {
		return "", err
	}
	a.orch.InvalidateHealthCache()
	_, detail := target.SessionStatus()
	return detail, nil
}

func (a *App) ProvisionCuaSidecar() (string, error) {
	if a.cuaImpl == nil {
		return "", errors.New("CuaDriverActuator chưa được khởi tạo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	msg, err := a.cuaImpl.ProvisionRuntime(ctx)
	a.orch.InvalidateHealthCache()
	return msg, err
}

func (a *App) EnableWaylandAccessibility() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return actuators.EnableWaylandAccessibility(ctx)
}

func (a *App) InspectUISelectors() (string, error) {
	if a.cuaImpl == nil {
		return "", errors.New("CuaDriverActuator chưa sẵn sàng")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg := a.orch.GetConfig()
	raw, err := a.cuaImpl.InspectUISelectors(ctx, cfg.TargetAppKeywords)
	if err != nil {
		return "", err
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") == nil {
		return a.auditLogger.RedactString(pretty.String()), nil
	}
	return a.auditLogger.RedactString(string(raw)), nil
}

func (a *App) TestCuaAction(action string) (string, error) {
	if a.cuaImpl == nil {
		return "", errors.New("CuaDriverActuator chưa sẵn sàng")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := a.orch.GetConfig()

	switch strings.TrimSpace(action) {
	case "focus_antigravity":
		if err := a.cuaImpl.FocusAppWindow(ctx, "Antigravity", false); err != nil {
			return "", err
		}
		return "Đã Focus thành công cửa sổ Antigravity trên Agent Workspace", nil
	case "inspect_quota":
		obs, err := a.cuaImpl.InspectQuota(ctx, cfg.TargetAppKeywords, false)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(
			"Kiểm tra Quota thành công: cửa sổ=%q | tài khoản=%q | model=%q | hết hạn mức=%v (tin cậy %.0f%%)",
			obs.WindowTitle,
			obs.ActiveAccountHint,
			obs.ActiveModel,
			obs.QuotaExhausted,
			obs.Confidence*100,
		), nil
	case "focus_chrome":
		if err := a.cuaImpl.FocusAppWindow(ctx, "Google Chrome", false); err != nil {
			return "", err
		}
		return "Đã Focus thành công cửa sổ Google Chrome qua trycua/cua", nil
	case "select_model":
		model := strings.TrimSpace(cfg.CuaDriver.DefaultModel)
		if model == "" {
			if p := cfg.FindProfile(cfg.ActiveProfileID); p != nil {
				model = p.PrimaryModel
			}
		}
		if model == "" {
			model = "gemini-2.5-pro"
		}
		if err := a.cuaImpl.SelectModel(ctx, domain.ScreenState{}, model, false); err != nil {
			return "", err
		}
		return "Đã chọn thành công Model mặc định: " + model, nil
	default:
		return "", fmt.Errorf("hành động kiểm thử CUA không hợp lệ: %q", action)
	}
}

func (a *App) TestOpenBrowserProfile(profileID string) error {
	cfg := a.orch.GetConfig()
	prof := cfg.FindProfile(profileID)
	if prof == nil {
		return errors.New("không tìm thấy hồ sơ: " + profileID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.browserRouter.OpenProfileSession(ctx, *prof, cfg.DryRunActuation)
}

func (a *App) SimulateHumanInput(kind string) domain.RuntimeStatus {
	actKind := domain.ActivityKind(strings.TrimSpace(kind))
	if actKind == "" {
		actKind = domain.ActivityPointerMove
	}
	a.orch.InjectSimulatedHumanInput(actKind)
	return a.orch.Status(context.Background())
}

func (a *App) SimulateQuotaExhausted() domain.RuntimeStatus {
	cfg := a.orch.GetConfig()
	frames := cfg.ConsensusFrames
	if frames < 1 {
		frames = 2
	}
	for i := 0; i < frames; i++ {
		a.orch.EnqueueSimulatedObservation(domain.ScreenState{
			ObservedAt:     time.Now(),
			WindowDetected: true,
			WindowTitle:    "Antigravity",
			WindowFocused:  true,
			AppAuthState:   domain.AuthAuthenticated,
			QuotaExhausted: true,
			QuotaMessage:   "Simulated 0% remaining — Quota exhausted",
			Confidence:     0.99,
			SourceTier:     "sandbox_simulation",
		})
		_ = a.orch.StepOnce(context.Background())
	}
	return a.orch.Status(context.Background())
}

func (a *App) SimulateAuthChallenge() domain.RuntimeStatus {
	a.orch.EnqueueSimulatedObservation(domain.ScreenState{
		ObservedAt:       time.Now(),
		WindowDetected:   true,
		WindowTitle:      "Sign in - Google Accounts",
		WindowFocused:    true,
		AppAuthState:     domain.AuthChallenge,
		AuthGateDetected: true,
		AuthGateReason:   "Phát hiện yêu cầu xác thực MFA / CAPTCHA (Mô phỏng Sandbox)",
		Confidence:       1.0,
		SourceTier:       "sandbox_simulation",
	})
	_ = a.orch.StepOnce(context.Background())
	return a.orch.Status(context.Background())
}

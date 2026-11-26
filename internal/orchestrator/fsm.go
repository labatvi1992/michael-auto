package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/ports"
)

const (
	defaultPreFlightQuietWindow = 1500 * time.Millisecond
	sensorHealthCacheTTL        = 10 * time.Second
	maxIntentAttempts           = 4
)

// Dependencies groups hexagonal ports injected into the Orchestrator.
type Dependencies struct {
	ConfigStore        ports.ConfigStore
	IntentStore        ports.IntentStore
	EvidenceStore      ports.EvidenceStore
	WorkspaceManager   ports.WorkspaceManager
	ActivityMonitor    ports.HumanActivityMonitor
	SensorFusion       *SensorFusion
	DesktopActuator    ports.DesktopActuator
	CuaDesktopActuator ports.CuaDesktopActuator
	BrowserRouter      ports.BrowserProfileRouter
	CLISyncer          ports.CLIConfigSyncer
	AuditLogger        ports.AuditLogger
	Notifier           ports.DesktopNotifier
	Publisher          ports.StatusPublisher
}

// Orchestrator runs the unattended re-entrant FSM with immediate multi-signal human yield (ADR-002),
// Fedora 45 Agent Workspace isolation, Ephemeral Intent persistence, and Verify-Evidence-Execute gates (ADR-007).
type Orchestrator struct {
	stepExecMu        sync.Mutex
	mu                sync.Mutex
	deps              Dependencies
	cfg               domain.AppConfig
	state             domain.FSMState
	stateReason       string
	hardStopReason    string
	cooldownUntil     time.Time
	pendingIntent     *domain.PendingIntent
	lastObservation   *domain.ScreenState
	simulatedQueue    []domain.ScreenState
	cuaLastQuotaCheck time.Time

	cachedSensors          []domain.SensorHealth
	cachedWorkspace        domain.AgentWorkspaceStatus
	healthCheckedAt        time.Time
	configWarning          string
	quotaChecksTotal       int
	switchesCompletedTotal int
	lastSwitchAt           *time.Time

	yieldCtrl *HumanYieldController
	consensus *MultiFrameConsensus
	breaker   *SafetyBreaker
	policy     *PolicyEngine
	nowFunc    func() time.Time
	display    string
	appVersion string
}

// NewOrchestrator wires all domain engines, restores any non-expired PendingIntent from tmpfs,
// and binds the immediate human-activity interrupt callback.
func NewOrchestrator(cfg domain.AppConfig, deps Dependencies) *Orchestrator {
	o := &Orchestrator{
		deps:        deps,
		cfg:         cfg,
		state:       domain.StateObserving,
		stateReason: "Đang quan sát định kỳ",
		consensus:   NewMultiFrameConsensus(),
		breaker:     NewSafetyBreaker(3),
		policy:      NewPolicyEngine(),
		nowFunc:     time.Now,
		display:     detectDisplayServer(),
		appVersion:  "2.5.0",
	}
	if !cfg.AutomationEnabled {
		o.state = domain.StatePausedByUser
		o.stateReason = "Tự động hóa đang được người dùng tạm tắt"
	}

	if deps.IntentStore != nil {
		if saved, err := deps.IntentStore.Load(); err == nil && saved != nil {
			if !saved.IsExpired(time.Now()) {
				o.pendingIntent = saved
				if deps.CuaDesktopActuator != nil && saved.RunID != "" {
					deps.CuaDesktopActuator.SetActiveRunID(saved.RunID)
				}
			} else {
				_ = deps.IntentStore.Clear()
			}
		}
	}

	o.yieldCtrl = NewHumanYieldController(deps.ActivityMonitor, o.onHumanInterrupt)
	o.yieldCtrl.SetYieldFilter(o.shouldYieldOnActivity)
	return o
}

// shouldYieldOnActivity implements Workspace-Aware Human Yield (#3):
// Explicit synthetic/UI sandbox signals (PointerMove, Keyboard, etc.) ALWAYS trigger 30s yield.
// Global Wayland compositor idle signals (ActivityCompositorAny) trigger 30s yield UNLESS the Agent
// Workspace is enabled in non-foreground mode AND the Wayland bridge proves the User is currently
// viewing another workspace (e.g., User coding on Workspace #1 while Agent operates on Workspace #2).
func (o *Orchestrator) shouldYieldOnActivity(kind domain.ActivityKind) bool {
	if kind != domain.ActivityCompositorAny {
		return true
	}
	o.mu.Lock()
	cfg := o.cfg
	wsMgr := o.deps.WorkspaceManager
	o.mu.Unlock()

	if !cfg.CuaDriver.AgentWorkspaceEnabled || cfg.CuaDriver.DeliveryMode == "foreground" || wsMgr == nil {
		return true
	}
	onAgentWS, known := wsMgr.UserIsOnAgentWorkspace(context.Background(), cfg)
	if known && !onAgentWS {
		return false
	}
	return true
}

func detectDisplayServer() string {
	if st := strings.TrimSpace(os.Getenv("XDG_SESSION_TYPE")); st != "" {
		return st
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return "wayland"
	}
	if os.Getenv("DISPLAY") != "" {
		return "x11"
	}
	return "headless"
}

// SetClock overrides time functions across orchestrator components for deterministic testing.
func (o *Orchestrator) SetClock(nowFunc func() time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if nowFunc == nil {
		return
	}
	o.nowFunc = nowFunc
	o.yieldCtrl.SetClock(nowFunc)
	o.breaker.SetClock(nowFunc)
	if o.deps.SensorFusion != nil {
		o.deps.SensorFusion.SetClock(nowFunc)
	}
}

// SetConfigWarning records a non-fatal startup configuration recovery warning (e.g. corrupt config backed up).
func (o *Orchestrator) SetConfigWarning(msg string) {
	o.mu.Lock()
	o.configWarning = strings.TrimSpace(msg)
	o.mu.Unlock()
}

// ClearConfigWarning dismisses the startup configuration warning once the user saves a valid config.
func (o *Orchestrator) ClearConfigWarning() {
	o.mu.Lock()
	o.configWarning = ""
	o.mu.Unlock()
}

func (o *Orchestrator) isScreenLocked() bool {
	if lockAware, ok := o.deps.ActivityMonitor.(interface{ IsScreenLocked() bool }); ok {
		return lockAware.IsScreenLocked()
	}
	return false
}

// InvalidateHealthCache forces the next Status() snapshot to refresh SensorHealth and AgentWorkspaceStatus.
func (o *Orchestrator) InvalidateHealthCache() {
	o.mu.Lock()
	o.healthCheckedAt = time.Time{}
	o.mu.Unlock()
}

// SetAppVersion sets the application version string exposed in RuntimeStatus telemetry.
func (o *Orchestrator) SetAppVersion(version string) {
	o.mu.Lock()
	if strings.TrimSpace(version) != "" {
		o.appVersion = strings.TrimSpace(version)
	}
	o.mu.Unlock()
}

// SetPrimaryActuator hot-swaps the active DesktopActuator when the user toggles CuaDriver.Enabled at runtime.
func (o *Orchestrator) SetPrimaryActuator(act ports.DesktopActuator) {
	o.mu.Lock()
	o.deps.DesktopActuator = act
	o.healthCheckedAt = time.Time{}
	o.mu.Unlock()
}

// YieldController returns the underlying HumanYieldController.
func (o *Orchestrator) YieldController() *HumanYieldController {
	return o.yieldCtrl
}

// Start launches the activity interrupt listener and background observation/countdown loop.
func (o *Orchestrator) Start(ctx context.Context) error {
	if err := o.yieldCtrl.Start(ctx); err != nil {
		o.logAudit(domain.AuditWarn, "activity_monitor_degraded", o.state, o.state,
			fmt.Sprintf("Không thể đăng ký kênh sự kiện trực tiếp từ ActivityMonitor: %v", err), nil)
	}

	if o.deps.WorkspaceManager != nil && o.cfg.CuaDriver.AgentWorkspaceEnabled {
		_, _ = o.deps.WorkspaceManager.EnsureAgentWorkspace(ctx, o.cfg)
	}

	o.publishCurrentStatus(ctx)

	go o.runLoop(ctx)
	return nil
}

func (o *Orchestrator) runLoop(ctx context.Context) {
	fastTicker := time.NewTicker(250 * time.Millisecond)
	defer fastTicker.Stop()

	lastStepAt := time.Time{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-fastTicker.C:
			o.mu.Lock()
			pollInterval := time.Duration(o.cfg.PollIntervalMs) * time.Millisecond
			if pollInterval < 250*time.Millisecond {
				pollInterval = 250 * time.Millisecond
			}
			curState := o.state
			hasWaitingIntent := o.pendingIntent != nil && o.pendingIntent.NextActionAt != nil
			now := o.nowFunc()
			o.mu.Unlock()

			if curState == domain.StatePausedHumanInput || curState == domain.StateCooldown || hasWaitingIntent {
				_ = o.TryStepOnce(ctx)
				continue
			}

			if lastStepAt.IsZero() || now.Sub(lastStepAt) >= pollInterval {
				lastStepAt = now
				_ = o.TryStepOnce(ctx)
			}
		}
	}
}

// onHumanInterrupt executes immediately when physical mouse/scroll/click/keyboard input is detected.
func (o *Orchestrator) onHumanInterrupt(kind domain.ActivityKind, at time.Time) {
	o.mu.Lock()
	prev := o.state
	pauseSec := o.cfg.YieldPauseSeconds

	if prev == domain.StatePausedByUser || prev == domain.StateHardStopChallenge {
		o.mu.Unlock()
		return
	}

	o.state = domain.StatePausedHumanInput
	o.stateReason = fmt.Sprintf("Phát hiện thao tác người dùng (%s) — tạm dừng trượt %ds", kind, pauseSec)
	o.consensus.Reset()
	o.mu.Unlock()

	if prev != domain.StatePausedHumanInput {
		o.logAudit(domain.AuditInfo, "human_input_yield", prev, domain.StatePausedHumanInput,
			fmt.Sprintf("Ngắt tức thời do thao tác %s; bắt đầu đếm tĩnh lặng %d giây", kind, pauseSec),
			map[string]string{"activity_kind": string(kind), "triggered_at": at.Format(time.RFC3339Nano)})
	}

	o.publishCurrentStatus(context.Background())
}

// InjectSimulatedHumanInput triggers the exact human interrupt pipeline (for tests and UI Sandbox).
func (o *Orchestrator) InjectSimulatedHumanInput(kind domain.ActivityKind) {
	o.yieldCtrl.RecordHumanActivity(kind)
}

// EnqueueSimulatedObservation queues synthetic ScreenStates for deterministic replay/sandbox testing.
func (o *Orchestrator) EnqueueSimulatedObservation(states ...domain.ScreenState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.simulatedQueue = append(o.simulatedQueue, states...)
}

// TriggerManualSwitch initiates the verified 5-step CUA switch to targetProfileID on user request.
func (o *Orchestrator) TriggerManualSwitch(ctx context.Context, targetProfileID string) error {
	o.mu.Lock()
	cfg := o.cfg
	now := o.nowFunc()
	fromID := cfg.ActiveProfileID
	lastModel := ""
	if o.lastObservation != nil {
		lastModel = o.lastObservation.ActiveModel
	}
	o.mu.Unlock()

	if !cfg.AutomationEnabled {
		return errors.New("tự động hóa đang tạm tắt — hãy bật 'Bật Tự động' trước khi thực hiện chuyển hồ sơ qua CUA")
	}

	targetProf := cfg.FindProfile(targetProfileID)
	if targetProf == nil {
		return fmt.Errorf("không tìm thấy hồ sơ đích: %s", targetProfileID)
	}

	targetModel := resolvePreferredModel(&cfg, *targetProf, lastModel)
	runID := fmt.Sprintf("manual_%s_%s", now.UTC().Format("20060102_150405"), targetProf.ID)
	ttlDur := time.Duration(cfg.CuaDriver.SignOutCooldownMs)*time.Millisecond + 10*time.Minute

	subStep := ""
	if cfg.CuaDriver.Enabled && o.deps.CuaDesktopActuator != nil {
		subStep = domain.SubStepSignOutSettings
		o.deps.CuaDesktopActuator.SetActiveRunID(runID)
	}

	newIntent := &domain.PendingIntent{
		RunID:           runID,
		Kind:            domain.IntentSwitchProfile,
		FromProfileID:   fromID,
		TargetProfileID: targetProf.ID,
		TargetModel:     targetModel,
		CreatedAt:       now,
		ExpiresAt:       now.Add(ttlDur),
		SubStep:         subStep,
	}

	o.setPendingIntent(newIntent)
	o.logAudit(domain.AuditInfo, "manual_profile_switch_requested", o.state, domain.StateAutoExecuting,
		fmt.Sprintf("Người dùng yêu cầu chuyển chủ động sang hồ sơ %s", targetProf.Name),
		map[string]string{"target_profile": targetProf.ID, "run_id": runID})

	err := o.StepOnce(ctx)
	o.publishCurrentStatus(ctx)
	return err
}

// UpdateConfig validates, merges runtime profile exhaustion state (#9), applies, and persists configuration.
func (o *Orchestrator) UpdateConfig(newCfg domain.AppConfig) error {
	if err := newCfg.Validate(); err != nil {
		return err
	}

	o.mu.Lock()
	prevState := o.state
	now := o.nowFunc()
	// Preserve live ExhaustedUntil / ExhaustedModels markers from current runtime profiles (#9)
	// so saving Settings from an older UI form snapshot never wipes active cooldown tracking.
	oldByID := make(map[string]domain.AccountProfile, len(o.cfg.Profiles))
	for _, ep := range o.cfg.Profiles {
		oldByID[ep.ID] = ep
	}
	for i := range newCfg.Profiles {
		if prevProf, ok := oldByID[newCfg.Profiles[i].ID]; ok {
			if newCfg.Profiles[i].ExhaustedUntil == nil && prevProf.ExhaustedUntil != nil && now.Before(*prevProf.ExhaustedUntil) {
				t := *prevProf.ExhaustedUntil
				newCfg.Profiles[i].ExhaustedUntil = &t
			}
			if len(newCfg.Profiles[i].ExhaustedModels) == 0 && len(prevProf.ExhaustedModels) > 0 {
				newCfg.Profiles[i].ExhaustedModels = append([]string(nil), prevProf.ExhaustedModels...)
			}
		}
	}

	o.cfg = newCfg
	o.configWarning = ""
	o.healthCheckedAt = time.Time{}
	if !newCfg.AutomationEnabled && o.state != domain.StateHardStopChallenge {
		o.state = domain.StatePausedByUser
		o.stateReason = "Tự động hóa đang được người dùng tạm tắt"
	} else if newCfg.AutomationEnabled && o.state == domain.StatePausedByUser {
		o.state = domain.StateObserving
		o.stateReason = "Đã bật lại tự động hóa — đang quan sát"
	}
	newState := o.state
	cfgSnapshot := o.cfg
	o.mu.Unlock()

	if o.deps.ConfigStore != nil {
		if err := o.deps.ConfigStore.Save(cfgSnapshot); err != nil {
			return err
		}
	}

	if prevState != newState {
		o.logAudit(domain.AuditInfo, "config_automation_toggled", prevState, newState, o.stateReason, nil)
	}
	o.publishConfigUpdate(cfgSnapshot)
	o.publishCurrentStatus(context.Background())
	return nil
}

// SetAutomationEnabled toggles master automation quickly from System Tray or UI header.
func (o *Orchestrator) SetAutomationEnabled(enabled bool) error {
	o.mu.Lock()
	cfgCopy := o.cfg
	o.mu.Unlock()
	cfgCopy.AutomationEnabled = enabled
	return o.UpdateConfig(cfgCopy)
}

// ResetHardStop clears HardStopChallenge, SafetyBreaker, and releases any temporary PendingIntent file.
func (o *Orchestrator) ResetHardStop() {
	o.releasePendingIntent()

	o.mu.Lock()
	prev := o.state
	o.breaker.Reset()
	o.consensus.Reset()
	o.hardStopReason = ""
	for i := range o.cfg.Profiles {
		o.cfg.Profiles[i].ExhaustedUntil = nil
		o.cfg.Profiles[i].ExhaustedModels = nil
	}
	if o.cfg.AutomationEnabled {
		o.state = domain.StateObserving
		o.stateReason = "Đã đặt lại chốt an toàn — tiếp tục quan sát"
	} else {
		o.state = domain.StatePausedByUser
		o.stateReason = "Tự động hóa đang tạm tắt"
	}
	next := o.state
	cfgToSave := o.cfg
	o.mu.Unlock()

	if o.deps.ConfigStore != nil {
		_ = o.deps.ConfigStore.Save(cfgToSave)
	}

	o.logAudit(domain.AuditInfo, "hard_stop_reset", prev, next, "Người dùng đã đặt lại trạng thái bảo vệ & giải phóng intent tạm", nil)
	o.publishCurrentStatus(context.Background())
}

// GetConfig returns a deep copy of current AppConfig.
func (o *Orchestrator) GetConfig() domain.AppConfig {
	o.mu.Lock()
	defer o.mu.Unlock()
	cfg := o.cfg
	cfg.Profiles = append([]domain.AccountProfile(nil), o.cfg.Profiles...)
	return cfg
}

// Status builds a complete RuntimeStatus snapshot, using a 10-second TTL cache for SensorHealth
// and AgentWorkspaceStatus so 4Hz countdown refreshes never generate redundant D-Bus/RPC traffic.
func (o *Orchestrator) Status(ctx context.Context) domain.RuntimeStatus {
	o.mu.Lock()
	cfg := o.cfg
	state := o.state
	reason := o.stateReason
	hardStop := o.hardStopReason
	cooldownUntil := o.cooldownUntil
	lastQuotaCheck := o.cuaLastQuotaCheck
	var intentCopy *domain.PendingIntent
	if o.pendingIntent != nil {
		cp := *o.pendingIntent
		if o.pendingIntent.NextActionAt != nil {
			t := *o.pendingIntent.NextActionAt
			cp.NextActionAt = &t
		}
		intentCopy = &cp
	}
	var obsCopy *domain.ScreenState
	if o.lastObservation != nil {
		cp := *o.lastObservation
		obsCopy = &cp
	}
	now := o.nowFunc()
	display := o.display
	needHealthRefresh := o.healthCheckedAt.IsZero() || now.Sub(o.healthCheckedAt) >= sensorHealthCacheTTL
	sensors := append([]domain.SensorHealth(nil), o.cachedSensors...)
	wsStatus := o.cachedWorkspace
	cfgWarning := o.configWarning
	quotaChecks := o.quotaChecksTotal
	switchesDone := o.switchesCompletedTotal
	var lastSwitchCopy *time.Time
	if o.lastSwitchAt != nil {
		t := *o.lastSwitchAt
		lastSwitchCopy = &t
	}
	o.mu.Unlock()

	if needHealthRefresh {
		if o.deps.SensorFusion != nil {
			sensors = o.deps.SensorFusion.HealthCheck(ctx, cfg)
		}
		if o.deps.WorkspaceManager != nil {
			wsStatus = o.deps.WorkspaceManager.Status(ctx, cfg)
		} else {
			wsStatus = domain.AgentWorkspaceStatus{
				Enabled:        cfg.CuaDriver.AgentWorkspaceEnabled,
				Ready:          cfg.CuaDriver.AgentWorkspaceEnabled,
				WorkspaceIndex: cfg.CuaDriver.AgentWorkspaceIndex,
				WorkspaceName:  cfg.CuaDriver.AgentWorkspaceName,
				Detail:         "Mặc định (không gắn WorkspaceManager)",
			}
		}
		o.mu.Lock()
		o.cachedSensors = append([]domain.SensorHealth(nil), sensors...)
		o.cachedWorkspace = wsStatus
		o.healthCheckedAt = now
		o.mu.Unlock()
	}

	yieldDur := time.Duration(cfg.YieldPauseSeconds) * time.Second
	var yieldRemMs int64
	if state == domain.StatePausedHumanInput {
		rem := o.yieldCtrl.RemainingQuietDuration(ctx, yieldDur)
		yieldRemMs = rem.Milliseconds()
	}

	var cooldownRemMs int64
	if state == domain.StateCooldown && now.Before(cooldownUntil) {
		cooldownRemMs = cooldownUntil.Sub(now).Milliseconds()
	}

	var intentWaitRemMs int64
	if intentCopy != nil && intentCopy.NextActionAt != nil && now.Before(*intentCopy.NextActionAt) {
		intentWaitRemMs = intentCopy.NextActionAt.Sub(now).Milliseconds()
	}

	var nextQuotaRemMs int64
	if cfg.CuaDriver.Enabled && !lastQuotaCheck.IsZero() {
		qInterval := time.Duration(cfg.CuaDriver.QuotaCheckIntervalMs) * time.Millisecond
		if nextAt := lastQuotaCheck.Add(qInterval); now.Before(nextAt) {
			nextQuotaRemMs = nextAt.Sub(now).Milliseconds()
		}
	}

	actKind, actAt := o.yieldCtrl.LastActivity()
	windowDur := time.Duration(cfg.SwitchWindowMinutes) * time.Minute
	tripped, tripReason, switchesUsed := o.breaker.Status(windowDur)

	activeProfName := ""
	activeModel := ""
	if p := cfg.FindProfile(cfg.ActiveProfileID); p != nil {
		activeProfName = p.Name
		activeModel = resolvePreferredModel(&cfg, *p)
	}
	if obsCopy != nil && obsCopy.ActiveModel != "" {
		activeModel = obsCopy.ActiveModel
	}

	portalReady := false
	portalDetail := "Chưa khởi tạo adapter"
	o.mu.Lock()
	appVer := o.appVersion
	primaryAct := o.deps.DesktopActuator
	cuaAct := o.deps.CuaDesktopActuator
	o.mu.Unlock()
	if cfg.CuaDriver.Enabled && cuaAct != nil {
		portalReady, portalDetail = cuaAct.SessionStatus()
	} else if primaryAct != nil {
		portalReady, portalDetail = primaryAct.SessionStatus()
	}

	return domain.RuntimeStatus{
		AppVersion:            appVer,
		State:                 state,
		StateReason:           reason,
		AutomationEnabled:     cfg.AutomationEnabled,
		DryRunActuation:       cfg.DryRunActuation,
		YieldRemainingMs:      yieldRemMs,
		YieldTotalMs:          yieldDur.Milliseconds(),
		LastActivityKind:      actKind,
		LastActivityAt:        actAt,
		CooldownRemainingMs:   cooldownRemMs,
		IntentWaitRemainingMs: intentWaitRemMs,
		NextQuotaCheckRemMs:   nextQuotaRemMs,
		ActiveProfileID:       cfg.ActiveProfileID,
		ActiveProfileName:     activeProfName,
		ActiveModel:           activeModel,
		ConsensusCount:        o.consensus.Count(),
		ConsensusRequired:     cfg.ConsensusFrames,
		SwitchesUsedInWindow:  switchesUsed,
		MaxSwitchesPerWindow:  cfg.MaxSwitchesPerWindow,
		CircuitBreakerTripped: tripped,
		CircuitBreakerReason:  tripReason,
		HardStopReason:        hardStop,
		PendingIntent:         intentCopy,
		LastObservation:       obsCopy,
		AgentWorkspace:        wsStatus,
		WaylandPortalReady:    portalReady,
		WaylandPortalDetail:   portalDetail,
		DisplayServer:          display,
		Sensors:                sensors,
		ScreenLocked:           o.isScreenLocked(),
		ConfigWarning:          cfgWarning,
		QuotaChecksTotal:       quotaChecks,
		SwitchesCompletedTotal: switchesDone,
		LastSwitchAt:           lastSwitchCopy,
		UpdatedAt:              now,
	}
}

// TryStepOnce executes StepOnce if no other goroutine is currently inside StepOnce, returning immediately otherwise.
func (o *Orchestrator) TryStepOnce(ctx context.Context) error {
	if !o.stepExecMu.TryLock() {
		return nil
	}
	defer o.stepExecMu.Unlock()
	return o.stepOnceSerialized(ctx)
}

// StepOnce executes one deterministic, serialized cycle of the re-entrant FSM.
func (o *Orchestrator) StepOnce(ctx context.Context) error {
	o.stepExecMu.Lock()
	defer o.stepExecMu.Unlock()
	return o.stepOnceSerialized(ctx)
}

func (o *Orchestrator) stepOnceSerialized(ctx context.Context) error {
	o.mu.Lock()
	cfg := o.cfg
	curState := o.state
	now := o.nowFunc()
	cooldownUntil := o.cooldownUntil
	o.mu.Unlock()

	if !cfg.AutomationEnabled {
		o.transition(domain.StatePausedByUser, "Tự động hóa đang được người dùng tạm tắt")
		o.publishCurrentStatus(ctx)
		return nil
	}

	if curState == domain.StateHardStopChallenge {
		o.publishCurrentStatus(ctx)
		return nil
	}

	// 0. GNOME Wayland Lock Screen / Suspend-Resume Guard (P1-1):
	// When the desktop session is locked, pause UI actuation cleanly so we never click against GNOME ScreenShield
	// and never falsely trip the Circuit Breaker while the laptop is locked or sleeping!
	if o.isScreenLocked() {
		o.mu.Lock()
		if o.state != domain.StateHardStopChallenge && o.state != domain.StatePausedByUser {
			o.state = domain.StateObserving
			o.stateReason = "Màn hình đang khóa — tạm hoãn thao tác UI cho đến khi mở khóa"
		}
		o.mu.Unlock()
		o.publishCurrentStatus(ctx)
		return nil
	}

	// 1. Reconcile 30-second sliding human-yield pause.
	if curState == domain.StatePausedHumanInput {
		yieldDur := time.Duration(cfg.YieldPauseSeconds) * time.Second
		rem := o.yieldCtrl.RemainingQuietDuration(ctx, yieldDur)
		if rem > 0 {
			o.mu.Lock()
			o.stateReason = fmt.Sprintf("Tạm dừng nhường người dùng — còn %.1fs tĩnh lặng", rem.Seconds())
			o.mu.Unlock()
			o.publishCurrentStatus(ctx)
			return nil
		}
		o.transition(domain.StateObserving, "Đã đủ 30 giây tĩnh lặng — quay lại quan sát và đối soát từ đầu")
		curState = domain.StateObserving
	}

	// 2. Reconcile post-switch stabilization cooldown.
	if curState == domain.StateCooldown {
		if now.Before(cooldownUntil) {
			rem := cooldownUntil.Sub(now)
			o.mu.Lock()
			o.stateReason = fmt.Sprintf("Nghỉ ổn định sau chuyển đổi — còn %.1fs", rem.Seconds())
			o.mu.Unlock()
			o.publishCurrentStatus(ctx)
			return nil
		}
		o.transition(domain.StateObserving, "Hoàn tất Cooldown — tiếp tục giám sát chu kỳ 1 phút")
	}

	// 3. Expire stale PendingIntent and strictly delete its temporary file if expired.
	o.mu.Lock()
	expired := o.pendingIntent != nil && o.pendingIntent.IsExpired(now)
	intent := o.pendingIntent
	hasSimulated := len(o.simulatedQueue) > 0
	o.mu.Unlock()
	if expired {
		o.logAudit(domain.AuditWarn, "pending_intent_expired", o.state, domain.StateObserving,
			"Ý định chuyển đổi trước đó đã hết hạn TTL — hủy bỏ & giải phóng tệp tạm", nil)
		o.releasePendingIntent()
		intent = nil
	}

	// 4. Non-blocking 1-minute SignOut countdown shortcut.
	if intent != nil && intent.SubStep == domain.SubStepWaitBeforeSignIn && intent.NextActionAt != nil && now.Before(*intent.NextActionAt) {
		rem := intent.NextActionAt.Sub(now)
		targetName := intent.TargetProfileID
		if tp := cfg.FindProfile(intent.TargetProfileID); tp != nil {
			targetName = tp.Name
		}
		o.mu.Lock()
		o.state = domain.StateVerifyingState
		o.stateReason = fmt.Sprintf("Đã bấm SignOut — chờ %.0fs trước khi bấm SignIn & chọn %s trên Chrome", rem.Seconds(), targetName)
		o.mu.Unlock()
		o.publishCurrentStatus(ctx)
		return nil
	}

	// 5. Observe current state (with mandatory 1-minute CUA Quota button inspection + 0% Evidence Gate).
	var (
		obs                domain.ScreenState
		fromActiveCuaQuota bool
	)
	if !hasSimulated && intent == nil && cfg.CuaDriver.Enabled && o.deps.CuaDesktopActuator != nil {
		runID := fmt.Sprintf("run_%s", now.UTC().Format("20060102_150405"))
		o.deps.CuaDesktopActuator.SetActiveRunID(runID)
		if quotaObs, ran, err := o.maybeInspectQuotaViaCua(ctx, cfg, now, runID); ran {
			if errors.Is(err, ports.ErrAuthGateDetected) || errors.Is(err, ports.ErrSecurityViolation) {
				o.releasePendingIntent()
				o.enterHardStop(err.Error())
				o.publishCurrentStatus(ctx)
				return nil
			}
			if err == nil {
				obs = quotaObs
				fromActiveCuaQuota = true
			}
		}
	}
	if !fromActiveCuaQuota {
		obs = o.nextObservation(ctx, cfg)
	}

	o.mu.Lock()
	o.lastObservation = &obs
	intent = o.pendingIntent
	o.mu.Unlock()

	// 6. Non-bypassable Hard-Stop Gate: Password / MFA / CAPTCHA / Bot Challenge (ADR-004).
	if obs.AuthGateDetected {
		reason := obs.AuthGateReason
		if reason == "" {
			reason = "Phát hiện yêu cầu xác thực bảo mật (Mật khẩu / MFA / CAPTCHA)"
		}
		o.releasePendingIntent()
		o.enterHardStop(reason)
		o.publishCurrentStatus(ctx)
		return nil
	}

	// 7. Re-entrant reconciliation if a PendingIntent exists.
	if intent != nil {
		err := o.reconcilePendingIntent(ctx, obs, intent)
		o.publishCurrentStatus(ctx)
		return err
	}

	// 8. Evaluate Quota Exhaustion (ADR: strictly requires Quota == 0% / Quota exhausted).
	var confirmed bool
	var count int
	if fromActiveCuaQuota && obs.QuotaExhausted && !obs.IsEditorContent && obs.Confidence >= cfg.MinConfidence {
		confirmed = true
		count = cfg.ConsensusFrames
	} else {
		confirmed, count = o.consensus.Evaluate(obs, cfg.MinConfidence, cfg.ConsensusFrames)
	}

	if !confirmed {
		if count > 0 {
			o.transition(domain.StateObserving, fmt.Sprintf("Phát hiện dấu hiệu chạm hạn mức (%d/%d khung hình đồng thuận)", count, cfg.ConsensusFrames))
		} else {
			o.transition(domain.StateObserving, "Đang giám sát định kỳ — hạn mức ổn định")
		}
		o.publishCurrentStatus(ctx)
		return nil
	}

	// 9. Quota 0% / Exhausted confirmed! Enter PreFlightCheck.
	o.transition(domain.StatePreFlightCheck, "Xác nhận Quota=0% / Quota exhausted — kiểm tra an toàn trước điều phối")
	windowDur := time.Duration(cfg.SwitchWindowMinutes) * time.Minute
	if ok, breakerReason, _ := o.breaker.CanExecute(cfg.MaxSwitchesPerWindow, windowDur); !ok {
		o.enterHardStop(breakerReason)
		o.publishCurrentStatus(ctx)
		return nil
	}

	if quiet, kind := o.yieldCtrl.CheckPreFlightQuiet(ctx, defaultPreFlightQuietWindow); !quiet {
		o.onHumanInterrupt(kind, o.nowFunc())
		return nil
	}

	// 10. Compute policy remediation.
	o.mu.Lock()
	cuaActive := o.cfg.CuaDriver.Enabled && o.deps.CuaDesktopActuator != nil
	decision := o.policy.DecideQuotaRemediation(&o.cfg, obs, now, cuaActive)
	cfgAfterPolicy := o.cfg
	o.mu.Unlock()

	if o.deps.ConfigStore != nil {
		_ = o.deps.ConfigStore.Save(cfgAfterPolicy)
	}

	ttlDur := time.Duration(cfg.IntentTTLMinutes) * time.Minute
	minCuaTTL := time.Duration(cfg.CuaDriver.SignOutCooldownMs)*time.Millisecond + 5*time.Minute
	if ttlDur < minCuaTTL {
		ttlDur = minCuaTTL
	}
	runID := fmt.Sprintf("run_%s", now.UTC().Format("20060102_150405"))
	if o.deps.CuaDesktopActuator != nil {
		o.deps.CuaDesktopActuator.SetActiveRunID(runID)
	}

	meta := map[string]string{
		"from_profile":   decision.CurrentProfile,
		"target_profile": decision.TargetProfileID,
		"target_model":   decision.TargetModel,
		"run_id":         runID,
	}
	if obs.EvidencePath != "" {
		meta["evidence_image"] = obs.EvidencePath
		meta["evidence_sha256"] = obs.EvidenceSHA256
	}

	switch decision.Action {
	case PolicyAllExhausted:
		o.enterHardStop(decision.Reason)
		o.publishCurrentStatus(ctx)
		return nil

	case PolicySwitchModel:
		newIntent := &domain.PendingIntent{
			RunID:           runID,
			Kind:            domain.IntentSwitchModel,
			FromProfileID:   decision.CurrentProfile,
			TargetProfileID: decision.TargetProfileID,
			TargetModel:     decision.TargetModel,
			CreatedAt:       now,
			ExpiresAt:       now.Add(ttlDur),
		}
		o.setPendingIntent(newIntent)
		o.consensus.Reset()
		o.logAudit(domain.AuditInfo, "policy_model_fallback", domain.StatePreFlightCheck, domain.StateAutoExecuting,
			decision.Reason, meta)
		err := o.reconcilePendingIntent(ctx, obs, newIntent)
		o.publishCurrentStatus(ctx)
		return err

	case PolicySwitchProfile:
		initialSubStep := ""
		if cuaActive {
			initialSubStep = domain.SubStepSignOutSettings
		}
		newIntent := &domain.PendingIntent{
			RunID:           runID,
			Kind:            domain.IntentSwitchProfile,
			FromProfileID:   decision.CurrentProfile,
			TargetProfileID: decision.TargetProfileID,
			TargetModel:     decision.TargetModel,
			CreatedAt:       now,
			ExpiresAt:       now.Add(ttlDur),
			SubStep:         initialSubStep,
		}
		o.setPendingIntent(newIntent)
		o.consensus.Reset()
		o.logAudit(domain.AuditInfo, "policy_profile_switch", domain.StatePreFlightCheck, domain.StateAutoExecuting,
			decision.Reason, meta)
		err := o.reconcilePendingIntent(ctx, obs, newIntent)
		o.publishCurrentStatus(ctx)
		return err
	}

	o.publishCurrentStatus(ctx)
	return nil
}

func (o *Orchestrator) maybeInspectQuotaViaCua(
	ctx context.Context,
	cfg domain.AppConfig,
	now time.Time,
	runID string,
) (domain.ScreenState, bool, error) {
	quotaInterval := time.Duration(cfg.CuaDriver.QuotaCheckIntervalMs) * time.Millisecond
	if quotaInterval < 5*time.Second {
		quotaInterval = 60 * time.Second
	}

	o.mu.Lock()
	lastCheck := o.cuaLastQuotaCheck
	o.mu.Unlock()

	if !lastCheck.IsZero() && now.Sub(lastCheck) < quotaInterval {
		return domain.ScreenState{}, false, nil
	}

	// When dedicated Agent Workspace #2 is pinned & ready and delivery is background/auto,
	// run the mandatory 1-minute Quota check on Workspace #2 without stalling while the user types on Workspace #1!
	// Only pause for MinIdleBeforeFocusMs when sharing the User's workspace or forcing foreground focus.
	wsIsolated := false
	if cfg.CuaDriver.AgentWorkspaceEnabled && cfg.CuaDriver.DeliveryMode != "foreground" && o.deps.WorkspaceManager != nil {
		wsIsolated = o.deps.WorkspaceManager.Status(ctx, cfg).Ready
	}
	minIdle := time.Duration(cfg.CuaDriver.MinIdleBeforeFocusMs) * time.Millisecond
	if !wsIsolated && minIdle > 0 && !o.isQuietForPeriodicFocus(ctx, now, minIdle) {
		return domain.ScreenState{}, false, nil
	}

	stepCtx, cleanup := o.yieldCtrl.BeginAtomicStep(ctx)
	obs, err := o.deps.CuaDesktopActuator.InspectQuota(stepCtx, cfg.TargetAppKeywords, cfg.DryRunActuation)
	cancelled := errors.Is(stepCtx.Err(), context.Canceled)
	cleanup()

	if cancelled || errors.Is(err, context.Canceled) {
		return domain.ScreenState{}, true, context.Canceled
	}

	o.mu.Lock()
	o.cuaLastQuotaCheck = now
	o.mu.Unlock()

	if err != nil {
		if !errors.Is(err, ports.ErrAuthGateDetected) {
			o.logAudit(domain.AuditWarn, "cua_quota_inspect_failed", o.state, o.state,
				fmt.Sprintf("CUA kiểm tra nút Quota gặp lỗi tạm thời: %v", err), nil)
		}
		return domain.ScreenState{}, true, err
	}

	meta := map[string]string{
		"quota_exhausted": fmt.Sprintf("%v", obs.QuotaExhausted),
		"active_model":    obs.ActiveModel,
		"run_id":          runID,
	}
	if obs.EvidencePath != "" {
		meta["evidence_image"] = obs.EvidencePath
		meta["evidence_sha256"] = obs.EvidenceSHA256
	}
	o.mu.Lock()
	o.quotaChecksTotal++
	o.mu.Unlock()

	o.logAudit(domain.AuditInfo, "cua_quota_inspected", o.state, o.state,
		"CUA: Đã kiểm tra nút Quota theo chu kỳ 1 phút", meta)
	return obs, true, nil
}

func (o *Orchestrator) isQuietForPeriodicFocus(ctx context.Context, now time.Time, minIdle time.Duration) bool {
	if _, lastAt := o.yieldCtrl.LastActivity(); lastAt != nil && now.Sub(*lastAt) < minIdle {
		return false
	}
	if o.deps.ActivityMonitor != nil {
		if idle, err := o.deps.ActivityMonitor.SystemIdleDuration(ctx); err == nil && idle < minIdle {
			return false
		}
	}
	return true
}

func (o *Orchestrator) reconcilePendingIntent(ctx context.Context, obs domain.ScreenState, intent *domain.PendingIntent) error {
	o.mu.Lock()
	cfg := o.cfg
	o.mu.Unlock()

	targetProf := cfg.FindProfile(intent.TargetProfileID)
	if targetProf == nil {
		o.releasePendingIntent()
		o.enterHardStop("Không tìm thấy hồ sơ đích " + intent.TargetProfileID)
		return nil
	}

	if o.deps.CuaDesktopActuator != nil && intent.RunID != "" {
		o.deps.CuaDesktopActuator.SetActiveRunID(intent.RunID)
	}

	switch intent.Kind {
	case domain.IntentSwitchModel:
		if strings.EqualFold(obs.ActiveModel, intent.TargetModel) && !obs.QuotaExhausted {
			o.completeIntent(targetProf.ID, intent.TargetModel, false, "Đã xác nhận chuyển thành công sang model "+intent.TargetModel)
			return nil
		}
		if intent.Attempts >= maxIntentAttempts {
			o.releasePendingIntent()
			o.enterHardStop("Không thể xác nhận chuyển đổi model " + intent.TargetModel + " sau số lần thử tối đa")
			return nil
		}
		return o.executeAtomicAction(ctx, "apply_model_"+intent.TargetModel, func(stepCtx context.Context) error {
			o.markIntentAttempt(intent, "select_model")
			if o.deps.CLISyncer != nil && cfg.CLISettingsPath != "" {
				if err := o.deps.CLISyncer.WriteActiveModel(cfg.CLISettingsPath, intent.TargetModel, cfg.DryRunActuation); err != nil {
					return err
				}
			}
			if o.deps.DesktopActuator != nil && (obs.WindowDetected || cfg.CuaDriver.Enabled) {
				if err := o.deps.DesktopActuator.SelectModel(stepCtx, obs, intent.TargetModel, cfg.DryRunActuation); err != nil {
					return err
				}
			}
			return nil
		}, fmt.Sprintf("Đang chuyển sang model dự phòng %s", intent.TargetModel))

	case domain.IntentSwitchProfile:
		if cfg.CuaDriver.Enabled && o.deps.CuaDesktopActuator != nil {
			return o.reconcileCuaProfileSwitch(ctx, cfg, obs, intent, *targetProf)
		}
		return o.reconcileFallbackProfileSwitch(ctx, cfg, obs, intent, *targetProf)
	}

	return nil
}

func (o *Orchestrator) reconcileCuaProfileSwitch(
	ctx context.Context,
	cfg domain.AppConfig,
	obs domain.ScreenState,
	intent *domain.PendingIntent,
	targetProf domain.AccountProfile,
) error {
	cua := o.deps.CuaDesktopActuator
	now := o.nowFunc()

	o.mu.Lock()
	if intent.SubStep == "" {
		intent.SubStep = domain.SubStepSignOutSettings
	}
	subStep := intent.SubStep
	attempts := intent.Attempts
	o.mu.Unlock()

	switch subStep {
	case domain.SubStepSignOutSettings:
		if obs.AppAuthState == domain.AuthLoggedOut {
			waitAt := now.Add(time.Duration(cfg.CuaDriver.SignOutCooldownMs) * time.Millisecond)
			o.mu.Lock()
			intent.SubStep = domain.SubStepWaitBeforeSignIn
			intent.NextActionAt = &waitAt
			intent.Attempts = 0
			o.state = domain.StateVerifyingState
			o.stateReason = fmt.Sprintf("Ứng dụng đã ở trạng thái đăng xuất — chờ %ds trước khi bấm SignIn", cfg.CuaDriver.SignOutCooldownMs/1000)
			o.mu.Unlock()
			o.persistPendingIntentSnapshot()
			return nil
		}

		if attempts >= maxIntentAttempts {
			o.releasePendingIntent()
			o.enterHardStop("Vượt quá số lần thử bấm SignOut trên màn hình Settings")
			return nil
		}

		return o.executeAtomicAction(ctx, "cua_signout_"+intent.FromProfileID, func(stepCtx context.Context) error {
			o.markIntentAttempt(intent, domain.SubStepSignOutSettings)
			if err := cua.TriggerLogout(stepCtx, obs, cfg.DryRunActuation); err != nil {
				return err
			}
			waitAt := o.nowFunc().Add(time.Duration(cfg.CuaDriver.SignOutCooldownMs) * time.Millisecond)
			o.mu.Lock()
			intent.SubStep = domain.SubStepWaitBeforeSignIn
			intent.NextActionAt = &waitAt
			intent.Attempts = 0
			o.mu.Unlock()
			o.persistPendingIntentSnapshot()
			o.logAudit(domain.AuditInfo, "cua_signout_completed", domain.StateAutoExecuting, domain.StateVerifyingState,
				fmt.Sprintf("Đã chụp Evidence & bấm SignOut trong Settings — bắt đầu chờ %ds trước khi SignIn", cfg.CuaDriver.SignOutCooldownMs/1000),
				map[string]string{"run_id": intent.RunID})
			return nil
		}, "CUA: Verify + Chụp Evidence #2 & bấm SignOut trong Settings")

	case domain.SubStepWaitBeforeSignIn:
		o.mu.Lock()
		nextAt := intent.NextActionAt
		o.mu.Unlock()
		if nextAt != nil && now.Before(*nextAt) {
			rem := nextAt.Sub(now)
			o.mu.Lock()
			o.state = domain.StateVerifyingState
			o.stateReason = fmt.Sprintf("Đã bấm SignOut — chờ %.0fs trước khi bấm SignIn & chọn %s trên Chrome", rem.Seconds(), targetProf.Name)
			o.mu.Unlock()
			return nil
		}
		o.mu.Lock()
		intent.NextActionAt = nil
		intent.SubStep = domain.SubStepClickSignIn
		intent.Attempts = 0
		attempts = 0
		o.mu.Unlock()
		o.persistPendingIntentSnapshot()
		fallthrough

	case domain.SubStepClickSignIn:
		if o.isTargetProfileAuthenticated(obs, targetProf) && !obs.QuotaExhausted {
			o.mu.Lock()
			intent.SubStep = domain.SubStepVerifyAndSelectModel
			intent.Attempts = 0
			o.mu.Unlock()
			o.persistPendingIntentSnapshot()
			return o.reconcileCuaProfileSwitch(ctx, cfg, obs, intent, targetProf)
		}

		if attempts >= maxIntentAttempts {
			o.releasePendingIntent()
			o.enterHardStop("Vượt quá số lần thử bấm nút SignIn trên Antigravity")
			return nil
		}

		return o.executeAtomicAction(ctx, "cua_signin_"+targetProf.ID, func(stepCtx context.Context) error {
			o.markIntentAttempt(intent, domain.SubStepClickSignIn)
			if prep, ok := cua.(interface{ SetPendingTargetProfile(domain.AccountProfile) }); ok {
				prep.SetPendingTargetProfile(targetProf)
			}
			if err := cua.ClickSignIn(stepCtx, cfg.DryRunActuation); err != nil {
				return err
			}
			o.mu.Lock()
			intent.SubStep = domain.SubStepSelectChromeAccount
			intent.Attempts = 0
			o.mu.Unlock()
			o.persistPendingIntentSnapshot()
			o.logAudit(domain.AuditInfo, "cua_signin_clicked", domain.StateAutoExecuting, domain.StateVerifyingState,
				"CUA: Đã chụp Evidence #3 & bấm SignIn sau 1 phút chờ — chuẩn bị kiểm chứng Chrome Profile",
				map[string]string{"run_id": intent.RunID})
			return nil
		}, "CUA: Verify + Chụp Evidence #3 & bấm nút SignIn")

	case domain.SubStepSelectChromeAccount:
		if o.isTargetProfileAuthenticated(obs, targetProf) && !obs.QuotaExhausted {
			o.mu.Lock()
			intent.SubStep = domain.SubStepVerifyAndSelectModel
			intent.Attempts = 0
			o.mu.Unlock()
			o.persistPendingIntentSnapshot()
			return o.reconcileCuaProfileSwitch(ctx, cfg, obs, intent, targetProf)
		}

		if attempts >= maxIntentAttempts {
			o.releasePendingIntent()
			o.enterHardStop(fmt.Sprintf("Không thể xác minh & chọn tài khoản %s trên Chrome Profile %s sau %d lần thử",
				targetProf.EmailHint, targetProf.BrowserProfileDir, maxIntentAttempts))
			return nil
		}

		return o.executeAtomicAction(ctx, "cua_chrome_account_"+targetProf.ID, func(stepCtx context.Context) error {
			o.markIntentAttempt(intent, domain.SubStepSelectChromeAccount)
			if err := cua.SelectChromeAccount(stepCtx, targetProf, cfg.DryRunActuation); err != nil {
				return err
			}
			o.mu.Lock()
			intent.SubStep = domain.SubStepVerifyAndSelectModel
			intent.Attempts = 0
			o.mu.Unlock()
			o.persistPendingIntentSnapshot()
			o.logAudit(domain.AuditInfo, "cua_chrome_account_selected", domain.StateAutoExecuting, domain.StateVerifyingState,
				fmt.Sprintf("CUA: Đã xác minh đúng Chrome Profile (%s), Omnibox accounts.google.com, lưu Evidence #4 & chọn %s",
					targetProf.BrowserProfileDir, targetProf.Name),
				map[string]string{"run_id": intent.RunID, "browser_profile": targetProf.BrowserProfileDir})
			return nil
		}, fmt.Sprintf("CUA: Verify Profile (%s) + Evidence #4 & chọn tài khoản %s", targetProf.BrowserProfileDir, targetProf.Name))

	case domain.SubStepVerifyAndSelectModel:
		desiredModel := strings.TrimSpace(intent.TargetModel)
		if desiredModel == "" {
			desiredModel = resolvePreferredModel(&cfg, targetProf)
		}

		if attempts >= maxIntentAttempts {
			o.releasePendingIntent()
			o.enterHardStop(fmt.Sprintf("Không thể chọn model mặc định %s sau %d lần thử", desiredModel, maxIntentAttempts))
			return nil
		}

		var stepSucceeded bool
		err := o.executeAtomicAction(ctx, "cua_select_model_"+desiredModel, func(stepCtx context.Context) error {
			o.markIntentAttempt(intent, domain.SubStepVerifyAndSelectModel)
			if o.deps.CLISyncer != nil && cfg.CLISettingsPath != "" && desiredModel != "" {
				_ = o.deps.CLISyncer.WriteActiveModel(cfg.CLISettingsPath, desiredModel, cfg.DryRunActuation)
			}
			if desiredModel != "" {
				if err := cua.SelectModel(stepCtx, obs, desiredModel, cfg.DryRunActuation); err != nil {
					return err
				}
			}
			stepSucceeded = true
			return nil
		}, fmt.Sprintf("CUA: Verify + Evidence #5 & chọn Model mặc định %s", desiredModel))

		// Post-Condition Invariants (#8): handle newly signed-in account already at 0% Quota or mismatched account!
		if errors.Is(err, ports.ErrPostSwitchQuotaExhausted) {
			o.markProfileExhaustedAndRelease(targetProf.ID, desiredModel)
			return nil
		}
		if errors.Is(err, ports.ErrAccountMismatch) {
			o.mu.Lock()
			intent.SubStep = domain.SubStepSignOutSettings
			o.mu.Unlock()
			o.persistPendingIntentSnapshot()
			return err
		}

		if err == nil && stepSucceeded {
			o.completeIntent(targetProf.ID, desiredModel, true,
				fmt.Sprintf("Hoàn tất đăng nhập %s (%s) & chọn model mặc định %s — đã giải phóng tệp trạng thái tạm",
					targetProf.Name, targetProf.BrowserProfileDir, desiredModel))
		}
		return err
	}

	return nil
}

func (o *Orchestrator) reconcileFallbackProfileSwitch(
	ctx context.Context,
	cfg domain.AppConfig,
	obs domain.ScreenState,
	intent *domain.PendingIntent,
	targetProf domain.AccountProfile,
) error {
	o.mu.Lock()
	fLastStep := intent.LastStep
	fAttempts := intent.Attempts
	o.mu.Unlock()

	if o.isTargetProfileAuthenticated(obs, targetProf) && !obs.QuotaExhausted {
		if intent.TargetModel != "" && obs.ActiveModel != "" && !strings.EqualFold(obs.ActiveModel, intent.TargetModel) && fLastStep != "restore_model" {
			return o.executeAtomicAction(ctx, "restore_model_"+intent.TargetModel, func(stepCtx context.Context) error {
				o.markIntentAttempt(intent, "restore_model")
				if o.deps.CLISyncer != nil && cfg.CLISettingsPath != "" {
					_ = o.deps.CLISyncer.WriteActiveModel(cfg.CLISettingsPath, intent.TargetModel, cfg.DryRunActuation)
				}
				if o.deps.DesktopActuator != nil && obs.WindowDetected {
					return o.deps.DesktopActuator.SelectModel(stepCtx, obs, intent.TargetModel, cfg.DryRunActuation)
				}
				return nil
			}, fmt.Sprintf("Đang khôi phục model ưu tiên %s cho hồ sơ %s", intent.TargetModel, targetProf.Name))
		}

		o.completeIntent(targetProf.ID, intent.TargetModel, true,
			fmt.Sprintf("Đã xác minh đăng nhập hồ sơ %s và đồng bộ model %s", targetProf.Name, intent.TargetModel))
		return nil
	}

	if fAttempts >= maxIntentAttempts {
		o.releasePendingIntent()
		o.enterHardStop(fmt.Sprintf("Vượt quá %d bước thử khi chuyển sang hồ sơ %s", maxIntentAttempts, targetProf.Name))
		return nil
	}

	if obs.AppAuthState == domain.AuthLoggedOut || (fLastStep == "trigger_logout" && !obs.WindowDetected) {
		return o.executeAtomicAction(ctx, "open_profile_"+targetProf.ID, func(stepCtx context.Context) error {
			o.markIntentAttempt(intent, "open_isolated_browser_profile")
			if o.deps.BrowserRouter != nil {
				if err := o.deps.BrowserRouter.OpenProfileSession(stepCtx, targetProf, cfg.DryRunActuation); err != nil {
					return err
				}
			}
			if o.deps.CLISyncer != nil && cfg.CLISettingsPath != "" && intent.TargetModel != "" {
				_ = o.deps.CLISyncer.WriteActiveModel(cfg.CLISettingsPath, intent.TargetModel, cfg.DryRunActuation)
			}
			return nil
		}, fmt.Sprintf("Định tuyến đăng nhập qua Browser Profile cô lập (%s)", targetProf.BrowserProfileDir))
	}

	return o.executeAtomicAction(ctx, "logout_from_"+intent.FromProfileID, func(stepCtx context.Context) error {
		o.markIntentAttempt(intent, "trigger_logout")
		if o.deps.DesktopActuator != nil && obs.WindowDetected {
			return o.deps.DesktopActuator.TriggerLogout(stepCtx, obs, cfg.DryRunActuation)
		}
		if o.deps.BrowserRouter != nil {
			o.markIntentStepOnly(intent, "open_isolated_browser_profile")
			if err := o.deps.BrowserRouter.OpenProfileSession(stepCtx, targetProf, cfg.DryRunActuation); err != nil {
				return err
			}
		}
		return nil
	}, fmt.Sprintf("Thực thi đăng xuất phiên cũ để chuyển sang %s", targetProf.Name))
}

func (o *Orchestrator) executeAtomicAction(ctx context.Context, stepName string, fn func(stepCtx context.Context) error, description string) error {
	if quiet, kind := o.yieldCtrl.CheckPreFlightQuiet(ctx, defaultPreFlightQuietWindow); !quiet {
		o.onHumanInterrupt(kind, o.nowFunc())
		return nil
	}

	if o.deps.ActivityMonitor != nil {
		o.deps.ActivityMonitor.SetActiveExecution(true)
		defer o.deps.ActivityMonitor.SetActiveExecution(false)
	}

	o.transition(domain.StateAutoExecuting, description)
	stepCtx, cleanup := o.yieldCtrl.BeginAtomicStep(ctx)
	err := fn(stepCtx)
	stepCancelled := errors.Is(stepCtx.Err(), context.Canceled)
	cleanup()

	if stepCancelled || errors.Is(err, context.Canceled) {
		return nil
	}

	if err != nil {
		if errors.Is(err, ports.ErrAuthGateDetected) || errors.Is(err, ports.ErrSecurityViolation) {
			o.releasePendingIntent()
			o.enterHardStop(err.Error())
			return err
		}
		if errors.Is(err, ports.ErrPostSwitchQuotaExhausted) {
			return err
		}

		tripped, reason := o.breaker.RecordFailure(err.Error())
		o.logAudit(domain.AuditError, "atomic_step_failed", domain.StateAutoExecuting, o.state,
			fmt.Sprintf("Bước %s gặp lỗi: %v", stepName, err), nil)
		if tripped {
			o.releasePendingIntent()
			o.enterHardStop(reason)
		} else {
			o.transition(domain.StateObserving, "Bước thực thi gặp lỗi tạm thời — quay lại quan sát để thử lại")
		}
		return err
	}

	o.transition(domain.StateVerifyingState, "Đã thực thi bước "+stepName+" — đang xác minh trạng thái đích")
	return nil
}

func (o *Orchestrator) isTargetProfileAuthenticated(obs domain.ScreenState, target domain.AccountProfile) bool {
	if obs.AppAuthState != domain.AuthAuthenticated {
		return false
	}
	hint := strings.ToLower(strings.TrimSpace(obs.ActiveAccountHint))
	if hint == "" {
		return false
	}
	return strings.EqualFold(hint, target.ID) ||
		(target.EmailHint != "" && strings.Contains(hint, strings.ToLower(target.EmailHint))) ||
		(target.Name != "" && strings.Contains(hint, strings.ToLower(target.Name)))
}

func (o *Orchestrator) markIntentAttempt(intent *domain.PendingIntent, step string) {
	o.mu.Lock()
	if intent != nil {
		intent.Attempts++
		intent.LastStep = step
	}
	o.mu.Unlock()
	o.persistPendingIntentSnapshot()
}

func (o *Orchestrator) markIntentStepOnly(intent *domain.PendingIntent, step string) {
	o.mu.Lock()
	if intent != nil {
		intent.LastStep = step
	}
	o.mu.Unlock()
	o.persistPendingIntentSnapshot()
}

func (o *Orchestrator) markProfileExhaustedAndRelease(profileID, model string) {
	o.mu.Lock()
	now := o.nowFunc()
	exhaustUntil := now.Add(time.Duration(o.cfg.ExhaustedCooldownMinutes) * time.Minute)
	for i := range o.cfg.Profiles {
		if o.cfg.Profiles[i].ID == profileID {
			o.cfg.Profiles[i].ExhaustedUntil = &exhaustUntil
			if model != "" && !containsFold(o.cfg.Profiles[i].ExhaustedModels, model) {
				o.cfg.Profiles[i].ExhaustedModels = append(o.cfg.Profiles[i].ExhaustedModels, model)
			}
		}
	}
	o.cfg.ActiveProfileID = profileID
	cfgToSave := o.cfg
	o.mu.Unlock()

	o.releasePendingIntent()
	if o.deps.ConfigStore != nil {
		_ = o.deps.ConfigStore.Save(cfgToSave)
	}
	o.publishConfigUpdate(cfgToSave)
	o.logAudit(domain.AuditWarn, "post_switch_profile_exhausted", domain.StateVerifyingState, domain.StateObserving,
		fmt.Sprintf("Tài khoản %s sau khi đăng nhập cũng đang chạm 0%% Quota — đánh dấu cạn hạn mức để chuyển tiếp sang hồ sơ kế tiếp", profileID),
		map[string]string{"profile_id": profileID})
	o.transition(domain.StateObserving, "Tài khoản vừa đăng nhập cũng đã hết hạn mức — đang xét hồ sơ dự phòng tiếp theo")
}

func (o *Orchestrator) setPendingIntent(intent *domain.PendingIntent) {
	o.mu.Lock()
	o.pendingIntent = intent
	var copyToSave domain.PendingIntent
	hasIntent := intent != nil
	if hasIntent {
		copyToSave = *intent
	}
	o.mu.Unlock()

	if o.deps.IntentStore != nil {
		if hasIntent {
			_ = o.deps.IntentStore.Save(copyToSave)
		} else {
			_ = o.deps.IntentStore.Clear()
		}
	}
}

func (o *Orchestrator) persistPendingIntentSnapshot() {
	o.mu.Lock()
	var copyToSave domain.PendingIntent
	hasIntent := o.pendingIntent != nil
	if hasIntent {
		copyToSave = *o.pendingIntent
	}
	o.mu.Unlock()

	if hasIntent && o.deps.IntentStore != nil {
		_ = o.deps.IntentStore.Save(copyToSave)
	}
}

// releasePendingIntent clears in-memory PendingIntent and strictly deletes the temporary pending_intent.json file.
func (o *Orchestrator) releasePendingIntent() {
	o.mu.Lock()
	o.pendingIntent = nil
	o.mu.Unlock()

	if o.deps.CuaDesktopActuator != nil {
		o.deps.CuaDesktopActuator.SetActiveRunID("")
	}
	if o.deps.IntentStore != nil {
		_ = o.deps.IntentStore.Clear()
	}
}

func (o *Orchestrator) completeIntent(profileID, model string, countProfileSwitch bool, msg string) {
	o.mu.Lock()
	prev := o.state
	now := o.nowFunc()
	runID := ""
	if o.pendingIntent != nil {
		runID = o.pendingIntent.RunID
	}
	o.cfg.ActiveProfileID = profileID
	o.pendingIntent = nil
	o.cooldownUntil = now.Add(time.Duration(o.cfg.CooldownSeconds) * time.Second)
	o.state = domain.StateCooldown
	o.stateReason = msg
	if countProfileSwitch {
		o.switchesCompletedTotal++
		ts := now
		o.lastSwitchAt = &ts
	}
	windowDur := time.Duration(o.cfg.SwitchWindowMinutes) * time.Minute
	cfgToSave := o.cfg
	o.mu.Unlock()

	// Guarantee temporary intent file cleanup upon completion!
	if o.deps.CuaDesktopActuator != nil {
		o.deps.CuaDesktopActuator.SetActiveRunID("")
	}
	if o.deps.IntentStore != nil {
		_ = o.deps.IntentStore.Clear()
	}

	if countProfileSwitch {
		o.breaker.RecordSwitch(windowDur)
	} else {
		o.breaker.RecordSuccess()
	}
	o.consensus.Reset()

	if o.deps.ConfigStore != nil {
		_ = o.deps.ConfigStore.Save(cfgToSave)
	}
	o.publishConfigUpdate(cfgToSave)
	if countProfileSwitch && o.deps.Notifier != nil {
		o.deps.Notifier.Notify("Michael Auto — Đã chuyển tài khoản", msg, false)
	}

	o.logAudit(domain.AuditInfo, "transition_completed", prev, domain.StateCooldown, msg, map[string]string{
		"active_profile": profileID,
		"active_model":   model,
		"run_id":         runID,
	})
}

func (o *Orchestrator) enterHardStop(reason string) {
	o.mu.Lock()
	prev := o.state
	o.state = domain.StateHardStopChallenge
	o.stateReason = "Dừng cứng an toàn: " + reason
	o.hardStopReason = reason
	o.breaker.Trip(reason)
	o.mu.Unlock()

	if prev != domain.StateHardStopChallenge {
		o.logAudit(domain.AuditWarn, "hard_stop_engaged", prev, domain.StateHardStopChallenge, reason, nil)
		if o.deps.Notifier != nil {
			o.deps.Notifier.Notify("Michael Auto — Dừng Cứng Bảo Vệ", reason, true)
		}
	}
}

func (o *Orchestrator) transition(next domain.FSMState, reason string) {
	o.mu.Lock()
	prev := o.state
	if prev == domain.StatePausedHumanInput && next != domain.StateObserving && next != domain.StatePausedByUser && next != domain.StateHardStopChallenge {
		o.mu.Unlock()
		return
	}
	o.state = next
	o.stateReason = reason
	o.mu.Unlock()

	if prev != next {
		o.logAudit(domain.AuditInfo, "fsm_transition", prev, next, reason, nil)
	}
}

func (o *Orchestrator) nextObservation(ctx context.Context, cfg domain.AppConfig) domain.ScreenState {
	o.mu.Lock()
	if len(o.simulatedQueue) > 0 {
		obs := o.simulatedQueue[0]
		o.simulatedQueue = o.simulatedQueue[1:]
		if obs.ObservedAt.IsZero() {
			obs.ObservedAt = o.nowFunc()
		}
		o.mu.Unlock()
		return obs
	}
	fusion := o.deps.SensorFusion
	now := o.nowFunc()
	o.mu.Unlock()

	if fusion == nil {
		return domain.ScreenState{ObservedAt: now, AppAuthState: domain.AuthUnknown, SourceTier: "none"}
	}
	return fusion.Observe(ctx, cfg)
}

func (o *Orchestrator) logAudit(level domain.AuditLevel, event string, from, to domain.FSMState, msg string, meta map[string]string) {
	if o.deps.AuditLogger == nil {
		return
	}
	o.mu.Lock()
	ts := o.nowFunc()
	profID := o.cfg.ActiveProfileID
	o.mu.Unlock()

	o.deps.AuditLogger.Log(domain.AuditEntry{
		Timestamp: ts,
		Level:     level,
		Event:     event,
		FromState: from,
		ToState:   to,
		ProfileID: profID,
		Message:   msg,
		Metadata:  meta,
	})
}

func (o *Orchestrator) publishCurrentStatus(ctx context.Context) {
	if o.deps.Publisher == nil {
		return
	}
	o.deps.Publisher.PublishStatus(o.Status(ctx))
}

func (o *Orchestrator) publishConfigUpdate(cfg domain.AppConfig) {
	if cp, ok := o.deps.Publisher.(interface{ PublishConfig(domain.AppConfig) }); ok {
		cp.PublishConfig(cfg)
	}
}

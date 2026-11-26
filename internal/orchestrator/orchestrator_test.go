package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labatvi1992/michael-auto/internal/adapters/storage"
	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/ports"
)

// --- Test Doubles Implementing Hexagonal Ports ---

type mockActivityMonitor struct {
	mu      sync.Mutex
	ch      chan domain.ActivityKind
	idleDur time.Duration
	hasIdle bool
}

func newMockActivityMonitor() *mockActivityMonitor {
	return &mockActivityMonitor{
		ch: make(chan domain.ActivityKind, 32),
	}
}

func (m *mockActivityMonitor) Subscribe(ctx context.Context) (<-chan domain.ActivityKind, error) {
	return m.ch, nil
}

func (m *mockActivityMonitor) SystemIdleDuration(ctx context.Context) (time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasIdle {
		return 0, ErrIdleUnsupported
	}
	return m.idleDur, nil
}

func (m *mockActivityMonitor) setIdle(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idleDur = d
	m.hasIdle = true
}

func (m *mockActivityMonitor) AcquireActuationLease(ttl time.Duration, expectedX, expectedY *int) func() {
	return func() {}
}

func (m *mockActivityMonitor) SetActiveExecution(active bool) {}
func (m *mockActivityMonitor) BackendName() string            { return "mock-wayland-idle" }
func (m *mockActivityMonitor) Close() error                   { return nil }

type mockActuator struct {
	mu           sync.Mutex
	logoutCalls  int
	modelCalls   []string
	blockLogout  chan struct{}
	logoutCancel chan error
	failAlways   bool
}

func (a *mockActuator) EnsureSession(ctx context.Context) error { return nil }
func (a *mockActuator) SessionStatus() (bool, string)          { return true, "mock-ready" }
func (a *mockActuator) Close() error                           { return nil }

func (a *mockActuator) TriggerLogout(ctx context.Context, state domain.ScreenState, dryRun bool) error {
	a.mu.Lock()
	a.logoutCalls++
	blockCh := a.blockLogout
	cancelOut := a.logoutCancel
	fail := a.failAlways
	a.mu.Unlock()

	if fail {
		return errors.New("simulated UI selector mismatch")
	}

	if blockCh != nil {
		select {
		case <-blockCh:
			return nil
		case <-ctx.Done():
			if cancelOut != nil {
				cancelOut <- ctx.Err()
			}
			return ctx.Err()
		}
	}
	return nil
}

func (a *mockActuator) SelectModel(ctx context.Context, state domain.ScreenState, model string, dryRun bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failAlways {
		return errors.New("simulated model selector error")
	}
	a.modelCalls = append(a.modelCalls, model)
	return nil
}

type mockBrowserRouter struct {
	mu             sync.Mutex
	openedProfiles []string
}

func (r *mockBrowserRouter) OpenProfileSession(ctx context.Context, profile domain.AccountProfile, dryRun bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openedProfiles = append(r.openedProfiles, profile.ID+":"+profile.BrowserProfileDir)
	return nil
}

type mockCLISync struct {
	mu    sync.Mutex
	model string
}

func (c *mockCLISync) ReadActiveModel(path string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.model, nil
}

func (c *mockCLISync) WriteActiveModel(path string, model string, dryRun bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.model = model
	return nil
}

// --- Tests ---

func TestMultiSignalInterruptCancelsInFlightStepUnder20msAndSlides30sTimer(t *testing.T) {
	cfg := domain.DefaultConfig()
	cfg.PreferIntraProfileModelDrop = false
	cfg.Profiles[1].Enabled = true

	mon := newMockActivityMonitor()
	mon.setIdle(60 * time.Second)

	act := &mockActuator{
		blockLogout:  make(chan struct{}),
		logoutCancel: make(chan error, 1),
	}
	router := &mockBrowserRouter{}
	cli := &mockCLISync{model: "gemini-2.5-pro"}

	now := time.Date(2026, 11, 24, 21, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	getNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	orch := NewOrchestrator(cfg, Dependencies{
		ActivityMonitor: mon,
		DesktopActuator: act,
		BrowserRouter:   router,
		CLISyncer:       cli,
	})
	orch.SetClock(getNow)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = orch.YieldController().Start(ctx)

	exhaustedObs := domain.ScreenState{
		WindowDetected:    true,
		WindowTitle:       "Antigravity",
		AppAuthState:      domain.AuthAuthenticated,
		ActiveAccountHint: "primary@example.com",
		ActiveModel:       "gemini-2.5-pro",
		QuotaExhausted:    true,
		Confidence:        0.98,
		SourceTier:        "atspi2",
	}
	orch.EnqueueSimulatedObservation(exhaustedObs, exhaustedObs)

	// Frame 1: 1/2 consensus
	_ = orch.StepOnce(ctx)
	if st := orch.Status(ctx); st.State != domain.StateObserving || st.ConsensusCount != 1 {
		t.Fatalf("expected Observing (1/2 consensus), got state=%s count=%d", st.State, st.ConsensusCount)
	}

	// Frame 2 triggers execution in background goroutine (blocked inside TriggerLogout).
	doneStep := make(chan struct{})
	go func() {
		_ = orch.StepOnce(ctx)
		close(doneStep)
	}()

	time.Sleep(15 * time.Millisecond)

	// Human moves mouse -> must cancel step context in << 20ms!
	tStart := time.Now()
	mon.setIdle(0)
	orch.InjectSimulatedHumanInput(domain.ActivityPointerMove)

	select {
	case err := <-act.logoutCancel:
		latency := time.Since(tStart)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if latency > 20*time.Millisecond {
			t.Fatalf("interrupt cancellation exceeded 20ms SLA: %v", latency)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("in-flight step was not cancelled promptly on mouse movement")
	}
	<-doneStep

	if st := orch.Status(ctx); st.State != domain.StatePausedHumanInput {
		t.Fatalf("expected PausedHumanInput, got %s", st.State)
	}

	// Advance 22s, then user scrolls wheel -> must slide quiet window back to full 30s!
	advance(22 * time.Second)
	mon.setIdle(0)
	orch.InjectSimulatedHumanInput(domain.ActivityPointerScroll)

	// Advance 25s, then user presses keyboard key -> slides back to full 30s again!
	advance(25 * time.Second)
	mon.setIdle(0)
	orch.InjectSimulatedHumanInput(domain.ActivityKeyboard)

	// Advance 20s (only 20s since keypress): must remain in PausedHumanInput!
	advance(20 * time.Second)
	mon.setIdle(20 * time.Second)
	_ = orch.StepOnce(ctx)
	if st := orch.Status(ctx); st.State != domain.StatePausedHumanInput {
		t.Fatalf("expected still PausedHumanInput at 20s quiet, got %s", st.State)
	}

	// Advance 11s more (31s total quiet on both clock and compositor) -> returns to Observing and reconciles!
	advance(11 * time.Second)
	mon.setIdle(31 * time.Second)

	// Suppose during the 30s pause, logout had already taken effect (screen is now LoggedOut).
	// Re-entrant reconciliation must NOT call TriggerLogout again; it must open isolated Browser Profile 1!
	orch.EnqueueSimulatedObservation(domain.ScreenState{
		WindowDetected: true,
		WindowTitle:    "Antigravity",
		AppAuthState:   domain.AuthLoggedOut,
		Confidence:     0.95,
	})
	_ = orch.StepOnce(ctx)

	router.mu.Lock()
	opened := append([]string(nil), router.openedProfiles...)
	router.mu.Unlock()
	if len(opened) != 1 || opened[0] != "profile-backup:Profile 1" {
		t.Fatalf("expected isolated browser profile-backup:Profile 1 to open on reconciliation, got %v", opened)
	}

	// Next observation confirms backup@example.com authenticated -> restores model & enters Cooldown!
	orch.EnqueueSimulatedObservation(domain.ScreenState{
		WindowDetected:    true,
		AppAuthState:      domain.AuthAuthenticated,
		ActiveAccountHint: "backup@example.com",
		ActiveModel:       cfg.Profiles[1].PrimaryModel,
		QuotaExhausted:    false,
		Confidence:        0.97,
	})
	_ = orch.StepOnce(ctx)

	st := orch.Status(ctx)
	if st.State != domain.StateCooldown || st.ActiveProfileID != "profile-backup" {
		t.Fatalf("expected Cooldown on profile-backup, got state=%s active=%s", st.State, st.ActiveProfileID)
	}
}

func TestEditorFalsePositiveRejectionAndIntraProfileModelFallback(t *testing.T) {
	cfg := domain.DefaultConfig()
	cfg.PreferIntraProfileModelDrop = true

	mon := newMockActivityMonitor()
	mon.setIdle(120 * time.Second)
	act := &mockActuator{}
	cli := &mockCLISync{model: "gemini-2.5-pro"}

	now := time.Date(2026, 11, 24, 21, 30, 0, 0, time.UTC)
	orch := NewOrchestrator(cfg, Dependencies{
		ActivityMonitor: mon,
		DesktopActuator: act,
		CLISyncer:       cli,
	})
	orch.SetClock(func() time.Time { return now })
	ctx := context.Background()

	// 1. Editor false-positive frames must NEVER count toward consensus.
	editorObs := domain.ScreenState{
		WindowDetected:  true,
		QuotaExhausted:  true,
		IsEditorContent: true,
		Confidence:      0.99,
	}
	orch.EnqueueSimulatedObservation(editorObs, editorObs, editorObs)
	for i := 0; i < 3; i++ {
		_ = orch.StepOnce(ctx)
	}
	if st := orch.Status(ctx); st.State != domain.StateObserving || st.ConsensusCount != 0 {
		t.Fatalf("editor content should be rejected, got state=%s count=%d", st.State, st.ConsensusCount)
	}

	// 2. Genuine UI banner across 2 frames triggers intra-profile fallback to primary profile's fallback model.
	realBanner := domain.ScreenState{
		WindowDetected:    true,
		AppAuthState:      domain.AuthAuthenticated,
		ActiveAccountHint: "primary@example.com",
		ActiveModel:       cfg.Profiles[0].PrimaryModel,
		QuotaExhausted:    true,
		Confidence:        0.97,
	}
	orch.EnqueueSimulatedObservation(realBanner, realBanner)
	_ = orch.StepOnce(ctx)
	_ = orch.StepOnce(ctx)

	if cli.model != cfg.Profiles[0].FallbackModels[0] {
		t.Fatalf("expected CLI model synced to %q, got %q", cfg.Profiles[0].FallbackModels[0], cli.model)
	}
	if act.logoutCalls != 0 {
		t.Fatalf("expected 0 logouts during intra-profile model drop, got %d", act.logoutCalls)
	}
}

func TestHardStopOnCAPTCHAMFAAndAllAccountsExhausted(t *testing.T) {
	cfg := domain.DefaultConfig()
	cfg.Profiles[0].FallbackModels = nil
	cfg.Profiles[1].Enabled = false // Only 1 enabled profile with 1 model!

	mon := newMockActivityMonitor()
	mon.setIdle(120 * time.Second)
	now := time.Date(2026, 11, 24, 22, 0, 0, 0, time.UTC)

	orch := NewOrchestrator(cfg, Dependencies{
		ActivityMonitor: mon,
	})
	orch.SetClock(func() time.Time { return now })
	ctx := context.Background()

	// 1. Immediate Hard-Stop on CAPTCHA (1st frame, no consensus delay).
	orch.EnqueueSimulatedObservation(domain.ScreenState{
		WindowDetected:   true,
		AuthGateDetected: true,
		AuthGateReason:   "Cloudflare Turnstile CAPTCHA detected",
	})
	_ = orch.StepOnce(ctx)
	if st := orch.Status(ctx); st.State != domain.StateHardStopChallenge {
		t.Fatalf("expected HardStopChallenge on CAPTCHA, got %s", st.State)
	}

	// Reset hard stop and test All-Accounts-Exhausted Circuit Breaker.
	orch.ResetHardStop()
	if st := orch.Status(ctx); st.State != domain.StateObserving {
		t.Fatalf("expected Observing after ResetHardStop, got %s", st.State)
	}

	exhaust := domain.ScreenState{
		WindowDetected: true,
		AppAuthState:   domain.AuthAuthenticated,
		ActiveModel:    "gemini-2.5-pro",
		QuotaExhausted: true,
		Confidence:     0.98,
	}
	orch.EnqueueSimulatedObservation(exhaust, exhaust)
	_ = orch.StepOnce(ctx)
	_ = orch.StepOnce(ctx)

	st := orch.Status(ctx)
	if st.State != domain.StateHardStopChallenge || !st.CircuitBreakerTripped {
		t.Fatalf("expected HardStopChallenge when all profiles/models exhausted, got state=%s tripped=%v", st.State, st.CircuitBreakerTripped)
	}
}

type mockCuaActuator struct {
	mu                       sync.Mutex
	activeRunID              string
	inspectCalls             int
	signOutCalls             int
	signInCalls              int
	chromeCalls              int
	chromeAccounts           []string
	selectedModels           []string
	chromeMFAGate            bool
	chromePhishGate          bool
	postSwitchQuotaExhausted bool
}

func (m *mockCuaActuator) EnsureSession(ctx context.Context) error { return nil }
func (m *mockCuaActuator) SessionStatus() (bool, string)          { return true, "mock-cua" }
func (m *mockCuaActuator) Close() error                           { return nil }
func (m *mockCuaActuator) SetActiveRunID(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeRunID = runID
}
func (m *mockCuaActuator) FocusAppWindow(ctx context.Context, appTarget string, dryRun bool) error {
	return nil
}
func (m *mockCuaActuator) InspectQuota(ctx context.Context, targetKeywords []string, dryRun bool) (domain.ScreenState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inspectCalls++
	zero := 0
	return domain.ScreenState{
		WindowDetected:    true,
		WindowTitle:       "Antigravity",
		WindowFocused:     true,
		AppAuthState:      domain.AuthAuthenticated,
		ActiveAccountHint: "primary@example.com",
		ActiveModel:       "gemini-2.5-pro",
		RemainingQuotaPct: &zero,
		QuotaExhausted:    true,
		QuotaMessage:      "0% remaining — Quota exhausted",
		Confidence:        0.99,
		SourceTier:        "cua_driver",
	}, nil
}
func (m *mockCuaActuator) TriggerLogout(ctx context.Context, state domain.ScreenState, dryRun bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signOutCalls++
	return nil
}
func (m *mockCuaActuator) ClickSignIn(ctx context.Context, dryRun bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signInCalls++
	return nil
}
func (m *mockCuaActuator) SelectChromeAccount(ctx context.Context, profile domain.AccountProfile, dryRun bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chromeCalls++
	if m.chromeMFAGate {
		return ports.ErrAuthGateDetected
	}
	if m.chromePhishGate {
		return ports.ErrSecurityViolation
	}
	m.chromeAccounts = append(m.chromeAccounts, profile.EmailHint)
	return nil
}
func (m *mockCuaActuator) SelectModel(ctx context.Context, state domain.ScreenState, model string, dryRun bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.postSwitchQuotaExhausted {
		m.postSwitchQuotaExhausted = false
		return ports.ErrPostSwitchQuotaExhausted
	}
	m.selectedModels = append(m.selectedModels, model)
	return nil
}

func TestCuaFiveStepNonBlockingWorkflowWithHumanInterruptAndChromeHardStop(t *testing.T) {
	cfg := domain.DefaultConfig()
	cfg.PreferIntraProfileModelDrop = false
	cfg.Profiles[1].Enabled = true
	cfg.CuaDriver.Enabled = true
	cfg.CuaDriver.QuotaCheckIntervalMs = 60000
	cfg.CuaDriver.MinIdleBeforeFocusMs = 5000
	cfg.CuaDriver.SignOutCooldownMs = 60000
	cfg.CuaDriver.DefaultModel = "Claude Opus 4.6"

	mon := newMockActivityMonitor()
	// Start with user actively typing (idle=1s < 5s MinIdleBeforeFocusMs) -> CUA must defer auto-focus!
	mon.setIdle(1 * time.Second)

	cua := &mockCuaActuator{}
	cli := &mockCLISync{model: "gemini-2.5-pro"}

	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	getNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	orch := NewOrchestrator(cfg, Dependencies{
		ActivityMonitor:    mon,
		DesktopActuator:    cua,
		CuaDesktopActuator: cua,
		CLISyncer:          cli,
		ConfigStore:        nil,
	})
	orch.SetClock(getNow)
	ctx := context.Background()

	// 1. User idle is only 1s (< 5s MinIdleBeforeFocusMs) -> InspectQuota must NOT steal window focus yet.
	_ = orch.StepOnce(ctx)
	if cua.inspectCalls != 0 {
		t.Fatalf("expected InspectQuota deferred while user idle < 5s, got %d calls", cua.inspectCalls)
	}

	// 2. Once user is quiet >= 5s -> auto-focuses Antigravity, clicks View Quota, sees exhaustion -> clicks SignOut!
	mon.setIdle(10 * time.Second)
	_ = orch.StepOnce(ctx)
	if cua.inspectCalls != 1 || cua.signOutCalls != 1 {
		t.Fatalf("expected 1 InspectQuota and 1 SignOut call, got inspect=%d signout=%d", cua.inspectCalls, cua.signOutCalls)
	}

	stAfterSignOut := orch.Status(ctx)
	if stAfterSignOut.IntentWaitRemainingMs <= 0 || stAfterSignOut.PendingIntent == nil || stAfterSignOut.PendingIntent.SubStep != domain.SubStepWaitBeforeSignIn {
		t.Fatalf("expected non-blocking SubStepWaitBeforeSignIn with countdown > 0, got %+v", stAfterSignOut)
	}

	// 3. Advance 25s into the 60s SignOut wait, then simulate human mouse movement!
	// Must enter 30s PausedHumanInput WITHOUT losing SubStepWaitBeforeSignIn and WITHOUT clicking SignOut again!
	advance(25 * time.Second)
	mon.setIdle(0)
	orch.InjectSimulatedHumanInput(domain.ActivityPointerMove)
	if st := orch.Status(ctx); st.State != domain.StatePausedHumanInput {
		t.Fatalf("expected PausedHumanInput on mouse movement during 60s wait, got %s", st.State)
	}

	// Advance 31s quiet (total 56s since SignOut — 4s still remaining on the 60s SignOut timer!).
	advance(31 * time.Second)
	mon.setIdle(31 * time.Second)
	_ = orch.StepOnce(ctx)
	if cua.signInCalls != 0 || cua.signOutCalls != 1 {
		t.Fatalf("expected 0 SignIn calls at 56s elapsed and still 1 SignOut call, got signIn=%d signOut=%d", cua.signInCalls, cua.signOutCalls)
	}

	// 4. Advance final 5s (total 61s since SignOut) -> executes ClickSignIn -> SelectChromeAccount -> SelectModel!
	advance(5 * time.Second)
	mon.setIdle(36 * time.Second)
	_ = orch.StepOnce(ctx) // ClickSignIn
	_ = orch.StepOnce(ctx) // SelectChromeAccount (backup@example.com)
	_ = orch.StepOnce(ctx) // SelectModel (Claude Opus 4.6) -> Cooldown!

	if cua.signInCalls != 1 {
		t.Fatalf("expected 1 SignIn call after 60s wait, got %d", cua.signInCalls)
	}
	if len(cua.chromeAccounts) != 1 || cua.chromeAccounts[0] != "backup@example.com" {
		t.Fatalf("expected Chrome account backup@example.com selected, got %v", cua.chromeAccounts)
	}
	if len(cua.selectedModels) != 1 || cua.selectedModels[0] != "Claude Opus 4.6" {
		t.Fatalf("expected configured default model 'Claude Opus 4.6' selected, got %v", cua.selectedModels)
	}

	stDone := orch.Status(ctx)
	if stDone.State != domain.StateCooldown || stDone.ActiveProfileID != "profile-backup" {
		t.Fatalf("expected Cooldown on profile-backup, got state=%s profile=%s", stDone.State, stDone.ActiveProfileID)
	}

	// 5. After Cooldown + 1 minute -> simulate Chrome MFA challenge on rotation back to primary -> immediate HardStop!
	advance(65 * time.Second)
	mon.setIdle(100 * time.Second)
	cua.chromeMFAGate = true
	_ = orch.StepOnce(ctx) // Exits Cooldown + runs 1-minute InspectQuota + SignOut
	advance(61 * time.Second)
	_ = orch.StepOnce(ctx) // ClickSignIn
	_ = orch.StepOnce(ctx) // SelectChromeAccount hits ErrAuthGateDetected -> HardStopChallenge on 1st attempt!

	if stMFA := orch.Status(ctx); stMFA.State != domain.StateHardStopChallenge {
		t.Fatalf("expected immediate HardStopChallenge when Chrome OAuth presents MFA, got %s", stMFA.State)
	}
}

type countingSensor struct {
	name  string
	calls atomic.Int64
	obs   domain.ScreenState
}

func (s *countingSensor) TierName() string { return s.name }
func (s *countingSensor) Health(ctx context.Context, cfg domain.AppConfig) domain.SensorHealth {
	return domain.SensorHealth{TierName: s.name, Healthy: true}
}
func (s *countingSensor) Observe(ctx context.Context, cfg domain.AppConfig) (domain.ScreenState, error) {
	s.calls.Add(1)
	return s.obs, nil
}

func TestEphemeralIntentFileLifecycleAndShortCircuitSensorFusion(t *testing.T) {
	// 1. Verify Short-Circuit SensorFusion: Tier 1 (cua_driver) success MUST skip Tier 2 (atspi2) completely!
	tier1 := &countingSensor{
		name: "cua_driver",
		obs: domain.ScreenState{
			WindowDetected: true,
			WindowTitle:    "Antigravity",
			Confidence:     0.96,
		},
	}
	tier2 := &countingSensor{
		name: "atspi2",
		obs: domain.ScreenState{
			WindowDetected: true,
			WindowTitle:    "Antigravity",
			Confidence:     0.95,
		},
	}
	fusion := NewSensorFusion(tier1, tier2)
	cfg := domain.DefaultConfig()
	fusedObs := fusion.Observe(context.Background(), cfg)
	if !fusedObs.WindowDetected {
		t.Fatalf("unexpected SensorFusion observation: %+v", fusedObs)
	}
	if tier1.calls.Load() != 1 || tier2.calls.Load() != 0 {
		t.Fatalf("expected Short-Circuit Cascade (tier1=1, tier2=0), got tier1=%d tier2=%d", tier1.calls.Load(), tier2.calls.Load())
	}

	// 2. Verify Ephemeral PendingIntent File: persisted during 60s SignOut wait, survives crash/restart, and strictly deleted on completion!
	intentPath := filepath.Join(t.TempDir(), "pending_intent.json")
	intentStore := storage.NewEphemeralIntentStore(intentPath)

	cfg.PreferIntraProfileModelDrop = false
	cfg.Profiles[1].Enabled = true
	cfg.CuaDriver.Enabled = true
	cfg.CuaDriver.SignOutCooldownMs = 60000

	mon := newMockActivityMonitor()
	mon.setIdle(20 * time.Second)
	cua := &mockCuaActuator{}

	now := time.Now().UTC().Truncate(time.Second)
	var clockMu sync.Mutex
	getNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	orch1 := NewOrchestrator(cfg, Dependencies{
		ActivityMonitor:    mon,
		DesktopActuator:    cua,
		CuaDesktopActuator: cua,
		IntentStore:        intentStore,
	})
	orch1.SetClock(getNow)
	ctx := context.Background()

	// Trigger Quota 0% -> SignOut -> 60s wait -> file MUST exist on disk!
	_ = orch1.StepOnce(ctx)
	if _, err := os.Stat(intentPath); err != nil {
		t.Fatalf("expected ephemeral pending_intent.json created during 60s SignOut wait: %v", err)
	}

	// Simulate process restart 61s later by constructing a fresh Orchestrator reading the same IntentStore!
	advance(61 * time.Second)
	cuaAfterRestart := &mockCuaActuator{}
	orch2 := NewOrchestrator(cfg, Dependencies{
		ActivityMonitor:    mon,
		DesktopActuator:    cuaAfterRestart,
		CuaDesktopActuator: cuaAfterRestart,
		IntentStore:        intentStore,
	})
	orch2.SetClock(getNow)

	_ = orch2.StepOnce(ctx) // Resumes directly at ClickSignIn (0 new SignOut calls!)
	_ = orch2.StepOnce(ctx) // SelectChromeAccount
	_ = orch2.StepOnce(ctx) // SelectModel -> Complete!

	if cuaAfterRestart.signOutCalls != 0 || cuaAfterRestart.signInCalls != 1 {
		t.Fatalf("expected restarted orchestrator to resume without re-clicking SignOut, got signOut=%d signIn=%d",
			cuaAfterRestart.signOutCalls, cuaAfterRestart.signInCalls)
	}

	// Ephemeral file MUST be strictly deleted immediately upon completion!
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Fatalf("expected pending_intent.json strictly deleted after workflow completion, stat err=%v", err)
	}
}

type stubReadyWorkspaceMgr struct {
	userOnAgent atomic.Bool
}

func (s *stubReadyWorkspaceMgr) Status(ctx context.Context, cfg domain.AppConfig) domain.AgentWorkspaceStatus {
	return domain.AgentWorkspaceStatus{
		Enabled:        true,
		Ready:          true,
		WorkspaceIndex: 1,
		WorkspaceName:  "Michael-Auto-Agent",
		Detail:         "Ready on Fedora 45 Wayland",
	}
}
func (s *stubReadyWorkspaceMgr) EnsureAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (domain.AgentWorkspaceStatus, error) {
	return s.Status(ctx, cfg), nil
}
func (s *stubReadyWorkspaceMgr) RouteTargetWindowsToAgentWorkspace(ctx context.Context, cfg domain.AppConfig) error {
	return nil
}
func (s *stubReadyWorkspaceMgr) UserIsOnAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (bool, bool) {
	return s.userOnAgent.Load(), true
}

type mockNotifier struct {
	mu       sync.Mutex
	messages []string
	critical []bool
}

func (n *mockNotifier) Notify(title, body string, crit bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.messages = append(n.messages, title+": "+body)
	n.critical = append(n.critical, crit)
}

func TestImmediateHardStopOnSecurityViolationAndIsolatedAgentWorkspaceQuotaCadence(t *testing.T) {
	cfg := domain.DefaultConfig()
	cfg.Profiles[1].Enabled = true
	cfg.CuaDriver.Enabled = true
	cfg.CuaDriver.AgentWorkspaceEnabled = true
	cfg.CuaDriver.DeliveryMode = "auto"

	mon := newMockActivityMonitor()
	mon.setIdle(2 * time.Second)

	cua := &mockCuaActuator{chromePhishGate: true}
	notif := &mockNotifier{}
	now := time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)

	orch := NewOrchestrator(cfg, Dependencies{
		ActivityMonitor:    mon,
		WorkspaceManager:   &stubReadyWorkspaceMgr{},
		DesktopActuator:    cua,
		CuaDesktopActuator: cua,
		Notifier:           notif,
	})
	orch.SetClock(func() time.Time { return now })
	ctx := context.Background()

	_ = orch.StepOnce(ctx)
	if cua.inspectCalls != 1 || cua.signOutCalls != 1 {
		t.Fatalf("expected InspectQuota + SignOut on isolated Agent Workspace #2 without starving, got inspect=%d signout=%d",
			cua.inspectCalls, cua.signOutCalls)
	}

	now = now.Add(61 * time.Second)
	_ = orch.StepOnce(ctx) // ClickSignIn
	_ = orch.StepOnce(ctx) // SelectChromeAccount hits ErrSecurityViolation (-32006 phishing Omnibox)!

	st := orch.Status(ctx)
	if st.State != domain.StateHardStopChallenge || cua.chromeCalls != 1 {
		t.Fatalf("expected immediate HardStopChallenge on 1st ErrSecurityViolation (chromeCalls=1), got state=%s calls=%d",
			st.State, cua.chromeCalls)
	}
	if len(notif.critical) == 0 || !notif.critical[0] {
		t.Fatalf("expected critical desktop notification on HardStopChallenge, got %+v", notif.messages)
	}
}

func TestV23WorkspaceAwareYieldPostSwitchExhaustionAndConfigStateMerge(t *testing.T) {
	cfg := domain.DefaultConfig()
	cfg.CuaDriver.AllowSameAccountRelogin = false
	cfg.Profiles[1].Enabled = true
	cfg.Profiles = append(cfg.Profiles, domain.AccountProfile{
		ID:                "profile-third",
		Name:              "Workspace Số 3",
		EmailHint:         "third@example.com",
		BrowserType:       domain.BrowserChrome,
		BrowserProfileDir: "Profile 2",
		PrimaryModel:      "gemini-2.5-pro",
		Enabled:           true,
		Priority:          3,
	})

	wsMgr := &stubReadyWorkspaceMgr{} // userOnAgent = false -> User is on Workspace #1!
	mon := newMockActivityMonitor()
	mon.setIdle(200 * time.Millisecond) // User typing continuously on Workspace #1!
	cua := &mockCuaActuator{postSwitchQuotaExhausted: true}
	notif := &mockNotifier{}

	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	getNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	orch := NewOrchestrator(cfg, Dependencies{
		ActivityMonitor:    mon,
		WorkspaceManager:   wsMgr,
		DesktopActuator:    cua,
		CuaDesktopActuator: cua,
		Notifier:           notif,
	})
	orch.SetClock(getNow)
	ctx := context.Background()

	// 1. User types on Workspace #1 (ActivityCompositorAny while userOnAgent=false):
	// Agent Workspace #2 MUST NOT freeze in PausedHumanInput!
	orch.InjectSimulatedHumanInput(domain.ActivityCompositorAny)
	if st := orch.Status(ctx); st.State == domain.StatePausedHumanInput {
		t.Fatal("expected Workspace #1 compositor input NOT to freeze background Agent Workspace #2")
	}

	// Execute switch toward profile-backup (which will report 0% Quota on Step 5!).
	_ = orch.StepOnce(ctx) // InspectQuota -> SignOut
	advance(61 * time.Second)
	_ = orch.StepOnce(ctx) // ClickSignIn
	_ = orch.StepOnce(ctx) // SelectChromeAccount (profile-backup)
	_ = orch.StepOnce(ctx) // SelectModel -> returns ErrPostSwitchQuotaExhausted (#8)!

	// Verify profile-backup was marked exhausted and NOT falsely marked Cooldown success!
	midCfg := orch.GetConfig()
	if midCfg.FindProfile("profile-backup").ExhaustedUntil == nil {
		t.Fatal("expected profile-backup marked ExhaustedUntil when post-login Quota is 0%")
	}

	// 2. Test #9 Config Merge Protection: saving a stale UI form (where ExhaustedUntil == nil) MUST preserve runtime ExhaustedUntil!
	staleUICfg := cfg
	staleUICfg.CooldownSeconds = 20
	if err := orch.UpdateConfig(staleUICfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	afterMergeCfg := orch.GetConfig()
	if afterMergeCfg.FindProfile("profile-backup").ExhaustedUntil == nil {
		t.Fatal("expected UpdateConfig to preserve runtime ExhaustedUntil marker from stale UI form")
	}

	// 3. Next cycle automatically rotates past exhausted profile-backup to profile-third!
	advance(61 * time.Second)
	_ = orch.StepOnce(ctx) // InspectQuota -> SignOut
	advance(61 * time.Second)
	_ = orch.StepOnce(ctx) // ClickSignIn
	_ = orch.StepOnce(ctx) // SelectChromeAccount (profile-third)
	_ = orch.StepOnce(ctx) // SelectModel -> Completes on profile-third!

	if st := orch.Status(ctx); st.ActiveProfileID != "profile-third" || st.State != domain.StateCooldown {
		t.Fatalf("expected rotation to complete cleanly on profile-third, got profile=%s state=%s", st.ActiveProfileID, st.State)
	}

	// 4. Test concurrent TryStepOnce + Status under -race (#2) AND instant yield when User enters Agent Workspace #2!
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = orch.TryStepOnce(ctx)
			_ = orch.Status(ctx)
		}()
	}
	wg.Wait()

	wsMgr.userOnAgent.Store(true) // User switches view to Agent Workspace #2!
	orch.InjectSimulatedHumanInput(domain.ActivityCompositorAny)
	if st := orch.Status(ctx); st.State != domain.StatePausedHumanInput {
		t.Fatalf("expected immediate 30s PausedHumanInput as soon as User views Agent Workspace #2, got %s", st.State)
	}
}

func TestV24MultiProfileUniqueDirInvariantAndNoSameAccountReloginLoop(t *testing.T) {
	// 1. Validate() MUST reject two enabled profiles sharing the same --profile-directory!
	dupCfg := domain.DefaultConfig()
	dupCfg.Profiles[1].Enabled = true
	dupCfg.Profiles[1].BrowserProfileDir = "Default" // Duplicates profile-primary's "Default"!
	if err := dupCfg.Validate(); err == nil || !strings.Contains(err.Error(), "profile-directory") {
		t.Fatalf("expected Validate() to reject duplicate BrowserProfileDir across enabled profiles, got %v", err)
	}

	// 2. When >=2 profiles are enabled (1 Account = 1 --profile-directory) and ALL profiles are exhausted,
	// PolicyEngine MUST return PolicyAllExhausted even if AllowSameAccountRelogin=true!
	poolCfg := domain.DefaultConfig()
	poolCfg.PreferIntraProfileModelDrop = false
	poolCfg.CuaDriver.AllowSameAccountRelogin = true
	poolCfg.Profiles[1].Enabled = true // Default + Profile 1 both enabled

	now := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)
	until := now.Add(30 * time.Minute)
	poolCfg.Profiles[0].ExhaustedUntil = &until // Profile 0 already exhausted

	pe := NewPolicyEngine()
	dec := pe.DecideQuotaRemediation(&poolCfg, domain.ScreenState{
		WindowDetected:    true,
		AppAuthState:      domain.AuthAuthenticated,
		ActiveAccountHint: "backup@example.com",
		ActiveModel:       "gemini-2.5-pro",
		QuotaExhausted:    true,
	}, now, true)

	if dec.Action != PolicyAllExhausted {
		t.Fatalf("expected PolicyAllExhausted when all profiles in multi-profile pool hit 0%% quota, got %+v", dec)
	}
}

type lockableActivityMonitor struct {
	*mockActivityMonitor
	locked atomic.Bool
}

func (l *lockableActivityMonitor) IsScreenLocked() bool {
	return l.locked.Load()
}

func TestScreenLockPauseManualSwitchGuardAndPreserveActiveModel(t *testing.T) {
	cfg := domain.DefaultConfig()
	cfg.Profiles[1].Enabled = true
	cfg.CuaDriver.DefaultModel = "preserve_active"
	cfg.CuaDriver.Enabled = true

	mon := &lockableActivityMonitor{mockActivityMonitor: newMockActivityMonitor()}
	mon.setIdle(2 * time.Minute)
	mon.locked.Store(true)

	cua := &mockCuaActuator{}
	orch := NewOrchestrator(cfg, Dependencies{
		ActivityMonitor:    mon,
		DesktopActuator:    cua,
		CuaDesktopActuator: cua,
	})
	ctx := context.Background()

	// 1. While GNOME ScreenSaver is locked, StepOnce MUST pause cleanly with ZERO CUA calls and ZERO breaker trips (P1-1)!
	if err := orch.StepOnce(ctx); err != nil {
		t.Fatalf("unexpected StepOnce error while screen locked: %v", err)
	}
	st := orch.Status(ctx)
	if !st.ScreenLocked || st.CircuitBreakerTripped || cua.inspectCalls != 0 {
		t.Fatalf("expected clean screen-lock pause with 0 CUA calls, got locked=%v breaker=%v inspect=%d", st.ScreenLocked, st.CircuitBreakerTripped, cua.inspectCalls)
	}

	// 2. TriggerManualSwitch MUST reject cleanly with explicit error when AutomationEnabled == false (P1-3)!
	_ = orch.SetAutomationEnabled(false)
	if err := orch.TriggerManualSwitch(ctx, "profile-backup"); err == nil || !strings.Contains(err.Error(), "tạm tắt") {
		t.Fatalf("expected explicit error from TriggerManualSwitch when automation disabled, got %v", err)
	}

	// 3. PolicyEngine preserve_active keeps observed active model across profile switch!
	pe := NewPolicyEngine()
	dec := pe.DecideQuotaRemediation(&cfg, domain.ScreenState{
		WindowDetected:    true,
		AppAuthState:      domain.AuthAuthenticated,
		ActiveAccountHint: "primary@example.com",
		ActiveModel:       "Claude Opus 4.6 (Thinking)",
		QuotaExhausted:    true,
	}, time.Now(), true)
	if dec.Action != PolicySwitchProfile || dec.TargetModel != "Claude Opus 4.6 (Thinking)" {
		t.Fatalf("expected preserve_active to retain 'Claude Opus 4.6 (Thinking)', got %+v", dec)
	}

	// 4. Verify AppVersion, ConfigWarning lifecycle, InvalidateHealthCache, and live SetPrimaryActuator hot-swap (R5)!
	orch.SetAppVersion("2.5.0-ga")
	orch.SetConfigWarning("corrupt backup notice")
	orch.InvalidateHealthCache()
	stWarn := orch.Status(ctx)
	if stWarn.AppVersion != "2.5.0-ga" || stWarn.ConfigWarning != "corrupt backup notice" {
		t.Fatalf("expected AppVersion=2.5.0-ga and ConfigWarning set, got %+v", stWarn)
	}
	orch.ClearConfigWarning()
	if orch.Status(ctx).ConfigWarning != "" {
		t.Fatal("expected ConfigWarning cleared")
	}

	fallbackAct := &mockActuator{}
	orch.SetPrimaryActuator(fallbackAct)
	cfgOff := orch.GetConfig()
	cfgOff.CuaDriver.Enabled = false
	_ = orch.UpdateConfig(cfgOff)
	stOff := orch.Status(ctx)
	if !stOff.WaylandPortalReady || stOff.WaylandPortalDetail != "mock-ready" {
		t.Fatalf("expected Status() to reflect fallback actuator after disabling CuaDriver, got ready=%v detail=%q", stOff.WaylandPortalReady, stOff.WaylandPortalDetail)
	}
}

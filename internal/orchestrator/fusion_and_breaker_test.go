package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

type stubSensor struct {
	tier   string
	obs    domain.ScreenState
	health domain.SensorHealth
	err    error
}

func (s *stubSensor) TierName() string { return s.tier }
func (s *stubSensor) Observe(ctx context.Context, cfg domain.AppConfig) (domain.ScreenState, error) {
	return s.obs, s.err
}
func (s *stubSensor) Health(ctx context.Context, cfg domain.AppConfig) domain.SensorHealth {
	return s.health
}

type memAudit struct {
	mu      sync.Mutex
	entries []domain.AuditEntry
}

func (m *memAudit) Log(e domain.AuditEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
}
func (m *memAudit) Recent(limit int) ([]domain.AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.AuditEntry(nil), m.entries...), nil
}
func (m *memAudit) Clear() error  { return nil }
func (m *memAudit) Path() string  { return "mem" }
func (m *memAudit) Close() error  { return nil }

type memPublisher struct {
	mu   sync.Mutex
	last domain.RuntimeStatus
}

func (p *memPublisher) PublishStatus(st domain.RuntimeStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = st
}

func TestSensorFusionPriorityAndHealthCheck(t *testing.T) {
	cfg := domain.DefaultConfig()
	cfg.CuaDriver.Enabled = true

	s1 := &stubSensor{
		tier: "cli_config",
		obs: domain.ScreenState{
			ActiveModel: "gemini-2.5-pro",
			Confidence:  1.0,
		},
		health: domain.SensorHealth{TierName: "cli_config", Healthy: true, Detail: "ok"},
	}
	s2 := &stubSensor{
		tier: "cua_driver",
		obs: domain.ScreenState{
			WindowDetected:    true,
			WindowTitle:       "Antigravity",
			WindowFocused:     true,
			AppAuthState:      domain.AuthAuthenticated,
			ActiveAccountHint: "primary@example.com",
			QuotaExhausted:    true,
			QuotaMessage:      "Quota exceeded",
			Confidence:        0.98,
			Targets: map[string]domain.UIElementTarget{
				"logout_button": {Role: "push button", Name: "Sign Out", ElementToken: "tok:1", HasAction: true},
			},
		},
		health: domain.SensorHealth{TierName: "cua_driver", Healthy: true, Detail: "cua ok"},
	}
	s3 := &stubSensor{
		tier: "atspi2",
		obs: domain.ScreenState{
			WindowDetected:   true,
			AuthGateDetected: true,
			AuthGateReason:   "MFA prompt",
		},
		health: domain.SensorHealth{TierName: "atspi2", Healthy: true, Detail: "atspi fallback ok"},
	}

	fusion := NewSensorFusion(s1, s2, s3)
	now := time.Date(2026, 11, 24, 22, 0, 0, 0, time.UTC)
	fusion.SetClock(func() time.Time { return now })

	merged := fusion.Observe(context.Background(), cfg)
	if !merged.WindowDetected || merged.ActiveModel != "gemini-2.5-pro" || !merged.QuotaExhausted || merged.AppAuthState != domain.AuthAuthenticated {
		t.Fatalf("unexpected short-circuit merged state: %+v", merged)
	}
	if merged.AuthGateDetected {
		t.Fatal("expected atspi2 fallback short-circuited when primary cua_driver succeeded")
	}
	if _, ok := merged.Targets["logout_button"]; !ok {
		t.Fatal("expected logout_button target merged")
	}

	// When cua_driver is unavailable/degraded, SensorFusion must fall back to atspi2 and honor its AuthGate!
	s2.obs = domain.ScreenState{}
	fallbackMerged := fusion.Observe(context.Background(), cfg)
	if !fallbackMerged.AuthGateDetected || fallbackMerged.AppAuthState != domain.AuthChallenge {
		t.Fatalf("expected atspi2 fallback to surface AuthGateDetected when cua_driver misses window, got %+v", fallbackMerged)
	}

	health := fusion.HealthCheck(context.Background(), cfg)
	if len(health) != 3 {
		t.Fatalf("expected 3 health entries, got %d", len(health))
	}
}

func TestSafetyBreakerFailuresBudgetAndDaemonLifecycle(t *testing.T) {
	cfg := domain.DefaultConfig()
	cfg.PollIntervalMs = 250
	cfg.CooldownSeconds = 2
	cfg.MaxSwitchesPerWindow = 1
	cfg.PreferIntraProfileModelDrop = true

	mon := newMockActivityMonitor()
	mon.setIdle(120 * time.Second)

	act := &mockActuator{failAlways: true}
	audit := &memAudit{}
	pub := &memPublisher{}

	s1 := &stubSensor{
		tier: "atspi2",
		obs: domain.ScreenState{
			WindowDetected: true,
			AppAuthState:   domain.AuthAuthenticated,
			ActiveModel:    cfg.Profiles[0].PrimaryModel,
			QuotaExhausted: true,
			Confidence:     0.98,
		},
		health: domain.SensorHealth{TierName: "atspi2", Healthy: true},
	}

	now := time.Date(2026, 11, 24, 22, 10, 0, 0, time.UTC)
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
		SensorFusion:    NewSensorFusion(s1),
		DesktopActuator: act,
		AuditLogger:     audit,
		Publisher:       pub,
	})
	orch.SetClock(getNow)

	ctx, cancel := context.WithCancel(context.Background())
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Test SetAutomationEnabled toggle to PausedByUser and back to Observing.
	if err := orch.SetAutomationEnabled(false); err != nil {
		t.Fatalf("SetAutomationEnabled(false) failed: %v", err)
	}
	if st := orch.Status(ctx); st.State != domain.StatePausedByUser {
		t.Fatalf("expected PausedByUser, got %s", st.State)
	}
	if err := orch.SetAutomationEnabled(true); err != nil {
		t.Fatalf("SetAutomationEnabled(true) failed: %v", err)
	}
	if gotCfg := orch.GetConfig(); !gotCfg.AutomationEnabled {
		t.Fatal("expected GetConfig().AutomationEnabled == true")
	}

	// Trigger 3 consecutive actuator failures -> SafetyBreaker must trip into HardStopChallenge!
	_ = orch.StepOnce(ctx) // Frame 1
	_ = orch.StepOnce(ctx) // Frame 2 -> attempt 1 fails
	_ = orch.StepOnce(ctx) // Reconcile attempt 2 fails
	_ = orch.StepOnce(ctx) // Reconcile attempt 3 fails -> trips breaker!

	if st := orch.Status(ctx); st.State != domain.StateHardStopChallenge || !st.CircuitBreakerTripped {
		t.Fatalf("expected HardStopChallenge after 3 consecutive failures, got %s (tripped=%v)", st.State, st.CircuitBreakerTripped)
	}

	// Reset breaker, fix actuator, verify model completion & Cooldown -> Observing transition!
	act.mu.Lock()
	act.failAlways = false
	act.mu.Unlock()
	orch.ResetHardStop()

	_ = orch.StepOnce(ctx)
	_ = orch.StepOnce(ctx) // Executes SelectModel(cfg.Profiles[0].FallbackModels[0])

	s1.obs = domain.ScreenState{
		WindowDetected: true,
		AppAuthState:   domain.AuthAuthenticated,
		ActiveModel:    cfg.Profiles[0].FallbackModels[0],
		QuotaExhausted: false,
		Confidence:     0.98,
	}
	_ = orch.StepOnce(ctx) // Completes into Cooldown!
	if st := orch.Status(ctx); st.State != domain.StateCooldown {
		t.Fatalf("expected Cooldown, got %s", st.State)
	}

	advance(5 * time.Second)
	_ = orch.StepOnce(ctx) // Cooldown expires -> returns to Observing!
	if st := orch.Status(ctx); st.State != domain.StateObserving {
		t.Fatalf("expected Observing after Cooldown, got %s", st.State)
	}

	// Test CheckPreFlightQuiet catching recent compositor activity (< 1.5s idle).
	mon.setIdle(200 * time.Millisecond)
	s1.obs = domain.ScreenState{
		WindowDetected: true,
		AppAuthState:   domain.AuthAuthenticated,
		ActiveModel:    "gemini-2.5-pro",
		QuotaExhausted: true,
		Confidence:     0.99,
	}
	orch.ResetHardStop()
	_ = orch.StepOnce(ctx)
	_ = orch.StepOnce(ctx) // PreFlight sees compositor idle=200ms -> enters PausedHumanInput!
	if st := orch.Status(ctx); st.State != domain.StatePausedHumanInput {
		t.Fatalf("expected PausedHumanInput when compositor idle < 1.5s, got %s", st.State)
	}

	time.Sleep(280 * time.Millisecond) // Let background runLoop tick at least once
	cancel()
}

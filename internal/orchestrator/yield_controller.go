package orchestrator

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/ports"
)

// ErrIdleUnsupported indicates a monitor backend does not report OS-wide idle duration.
var ErrIdleUnsupported = errors.New("system idle duration unsupported by monitor backend")

// InterruptCallback is invoked immediately (<20ms) when physical human input occurs.
type InterruptCallback func(kind domain.ActivityKind, at time.Time)

// YieldFilterFunc returns true when a physical activity signal should interrupt automation
// (e.g., returns false ONLY when User is proven to be on Workspace #1 while Agent acts in BACKGROUND on Workspace #2).
type YieldFilterFunc func(kind domain.ActivityKind) bool

// HumanYieldController enforces immediate cancellation on physical human input
// (mouse move, click, scroll wheel, or keyboard) and manages the 30-second sliding quiet window.
type HumanYieldController struct {
	mu               sync.Mutex
	monitor          ports.HumanActivityMonitor
	nowFunc          func() time.Time
	yieldFilter      YieldFilterFunc
	lastActivityAt   time.Time
	lastActivityKind domain.ActivityKind
	hasActivity      bool
	stepCancel       context.CancelFunc
	onInterrupt      InterruptCallback
}

// NewHumanYieldController creates a new controller bound to an activity monitor.
func NewHumanYieldController(monitor ports.HumanActivityMonitor, onInterrupt InterruptCallback) *HumanYieldController {
	return &HumanYieldController{
		monitor:     monitor,
		nowFunc:     time.Now,
		onInterrupt: onInterrupt,
	}
}

// SetClock overrides the time source (used for deterministic unit testing).
func (yc *HumanYieldController) SetClock(nowFunc func() time.Time) {
	yc.mu.Lock()
	defer yc.mu.Unlock()
	if nowFunc != nil {
		yc.nowFunc = nowFunc
	}
}

// SetYieldFilter attaches an optional workspace-aware predicate deciding whether global compositor
// input should interrupt background Agent Workspace execution.
func (yc *HumanYieldController) SetYieldFilter(filter YieldFilterFunc) {
	yc.mu.Lock()
	defer yc.mu.Unlock()
	yc.yieldFilter = filter
}

func (yc *HumanYieldController) shouldYieldFor(kind domain.ActivityKind) bool {
	yc.mu.Lock()
	filter := yc.yieldFilter
	yc.mu.Unlock()
	if filter == nil {
		return true
	}
	return filter(kind)
}

// Start begins listening to physical input signals from the HumanActivityMonitor.
func (yc *HumanYieldController) Start(ctx context.Context) error {
	if yc.monitor == nil {
		return nil
	}
	ch, err := yc.monitor.Subscribe(ctx)
	if err != nil {
		return err
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case kind, ok := <-ch:
				if !ok {
					return
				}
				yc.RecordHumanActivity(kind)
			}
		}
	}()
	return nil
}

// RecordHumanActivity registers a physical human signal, cancels any active atomic step
// immediately (unless filtered out by isolated Agent Workspace background execution),
// and resets the sliding quiet-period timer.
func (yc *HumanYieldController) RecordHumanActivity(kind domain.ActivityKind) {
	if !yc.shouldYieldFor(kind) {
		return
	}

	yc.mu.Lock()
	now := yc.nowFunc()
	yc.lastActivityAt = now
	yc.lastActivityKind = kind
	yc.hasActivity = true
	cancel := yc.stepCancel
	yc.stepCancel = nil
	cb := yc.onInterrupt
	yc.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cb != nil {
		cb(kind, now)
	}
}

// BeginAtomicStep wraps parent in a cancellable context that aborts immediately if
// physical human activity arrives while the micro-step is executing.
func (yc *HumanYieldController) BeginAtomicStep(parent context.Context) (context.Context, func()) {
	stepCtx, cancel := context.WithCancel(parent)
	yc.mu.Lock()
	if yc.stepCancel != nil {
		yc.stepCancel()
	}
	yc.stepCancel = cancel
	yc.mu.Unlock()

	cleanup := func() {
		yc.mu.Lock()
		cancel()
		yc.stepCancel = nil
		yc.mu.Unlock()
	}
	return stepCtx, cleanup
}

// LastActivity returns the most recent physical human input kind and timestamp.
func (yc *HumanYieldController) LastActivity() (domain.ActivityKind, *time.Time) {
	yc.mu.Lock()
	defer yc.mu.Unlock()
	if !yc.hasActivity {
		return "", nil
	}
	ts := yc.lastActivityAt
	return yc.lastActivityKind, &ts
}

// CheckPreFlightQuiet verifies no recent human input occurred right before starting execution.
// When isolated Agent Workspace background mode is active and User is confirmed on another workspace,
// global compositor activity on Workspace #1 does not block background actuation on Workspace #2.
func (yc *HumanYieldController) CheckPreFlightQuiet(ctx context.Context, minPreFlightIdle time.Duration) (bool, domain.ActivityKind) {
	if !yc.shouldYieldFor(domain.ActivityCompositorAny) {
		return true, ""
	}

	yc.mu.Lock()
	now := yc.nowFunc()
	if yc.hasActivity && now.Sub(yc.lastActivityAt) < minPreFlightIdle {
		kind := yc.lastActivityKind
		yc.mu.Unlock()
		return false, kind
	}
	mon := yc.monitor
	yc.mu.Unlock()

	if mon != nil {
		idle, err := mon.SystemIdleDuration(ctx)
		if err == nil && idle < minPreFlightIdle {
			yc.RecordHumanActivity(domain.ActivityCompositorAny)
			return false, domain.ActivityCompositorAny
		}
	}
	return true, ""
}

// RemainingQuietDuration calculates how much time remains in the sliding quiet window.
func (yc *HumanYieldController) RemainingQuietDuration(ctx context.Context, pauseDuration time.Duration) time.Duration {
	yc.mu.Lock()
	now := yc.nowFunc()
	hasAct := yc.hasActivity
	lastAt := yc.lastActivityAt
	mon := yc.monitor
	yc.mu.Unlock()

	var remainingLocal time.Duration
	if hasAct {
		elapsed := now.Sub(lastAt)
		if elapsed < pauseDuration {
			remainingLocal = pauseDuration - elapsed
		}
	}

	if mon != nil && yc.shouldYieldFor(domain.ActivityCompositorAny) {
		idle, err := mon.SystemIdleDuration(ctx)
		if err == nil && idle < pauseDuration {
			remainingCompositor := pauseDuration - idle
			if remainingCompositor > remainingLocal {
				yc.mu.Lock()
				inferredAt := now.Add(-idle)
				if !yc.hasActivity || inferredAt.After(yc.lastActivityAt) {
					yc.lastActivityAt = inferredAt
					if yc.lastActivityKind == "" {
						yc.lastActivityKind = domain.ActivityCompositorAny
					}
					yc.hasActivity = true
				}
				yc.mu.Unlock()
				return remainingCompositor
			}
		}
	}

	return remainingLocal
}

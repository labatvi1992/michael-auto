//go:build linux

package activity

import (
	"context"
	"testing"
	"time"
)

func TestActuationLeaseWindowAndLockScreenGuard(t *testing.T) {
	m := &WaylandActivityMonitor{backend: "test"}
	if m.BackendName() != "test" {
		t.Fatalf("expected backend 'test', got %q", m.BackendName())
	}
	if m.inActuationLease() {
		t.Fatal("expected false before acquiring actuation lease")
	}

	x, y := 100, 200
	release := m.AcquireActuationLease(60*time.Millisecond, &x, &y)
	if !m.inActuationLease() {
		t.Fatal("expected true while inside actuation lease")
	}

	release()
	time.Sleep(25 * time.Millisecond)
	if m.inActuationLease() {
		t.Fatal("expected lease expired after release + grace buffer")
	}

	// Verify activeExecution & GNOME Lock Screen state flags (P1-1).
	m.SetActiveExecution(true)
	if !m.activeExecution.Load() {
		t.Fatal("expected activeExecution=true")
	}
	m.SetActiveExecution(false)
	if m.activeExecution.Load() {
		t.Fatal("expected activeExecution=false")
	}

	if m.IsScreenLocked() {
		t.Fatal("expected screenLocked=false initially")
	}
	m.SetScreenLockedForTesting(true)
	if !m.IsScreenLocked() {
		t.Fatal("expected screenLocked=true after lock signal")
	}
	m.SetScreenLockedForTesting(false)
	if m.IsScreenLocked() {
		t.Fatal("expected screenLocked=false after unlock")
	}

	// Verify unavailable backend cleanly returns error on SystemIdleDuration & Close is idempotent.
	unavail := &WaylandActivityMonitor{backend: "unavailable"}
	if _, err := unavail.SystemIdleDuration(context.Background()); err == nil {
		t.Fatal("expected error for unavailable backend SystemIdleDuration")
	}
	if err := unavail.Close(); err != nil {
		t.Fatalf("unexpected Close error: %v", err)
	}
}

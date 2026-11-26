//go:build linux

package activity

import (
	"context"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
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

func TestWaylandActivityMonitorLazyDBusReconnectAndBackoff(t *testing.T) {
	m := &WaylandActivityMonitor{
		backend:         "unavailable",
		backoffSchedule: []time.Duration{15 * time.Millisecond, 25 * time.Millisecond, 40 * time.Millisecond},
	}

	if got := m.nextBackoffDuration(0); got != 15*time.Millisecond {
		t.Fatalf("expected 15ms for attempt 0, got %v", got)
	}
	if got := m.nextBackoffDuration(99); got != 40*time.Millisecond {
		t.Fatalf("expected capped 40ms for attempt 99, got %v", got)
	}

	attempts := 0
	m.probeFn = func(ctx context.Context) (*dbus.Conn, string, error) {
		attempts++
		if attempts < 2 {
			return nil, "unavailable", context.DeadlineExceeded
		}
		return nil, "wayland-freedesktop-screensaver", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := m.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe must never permanently fail when starting early at boot, got: %v", err)
	}

	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		if m.BackendName() == "wayland-freedesktop-screensaver" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m.BackendName() != "wayland-freedesktop-screensaver" {
		t.Fatalf("expected lazy reconnect to recover backend, got %q after %d attempts", m.BackendName(), attempts)
	}

	// Verify markDisconnected resets backend to unavailable cleanly so reconnect loop can recover again.
	m.markDisconnected(nil)
	if m.BackendName() != "unavailable" {
		t.Fatalf("expected unavailable after markDisconnected, got %q", m.BackendName())
	}

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel closed after context cancel")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Subscribe goroutine did not exit cleanly after context cancel")
	}
	_ = m.Close()

	// Smoke test production constructor (read-only D-Bus probe), SystemIdleDuration, Subscribe & clean Close().
	prod := NewHumanActivityMonitor()
	if prod.BackendName() == "" {
		t.Fatal("expected non-empty backend name from NewHumanActivityMonitor")
	}
	qCtx, qCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer qCancel()
	_, _ = prod.SystemIdleDuration(qCtx)
	if subCh, subErr := prod.Subscribe(qCtx); subErr == nil {
		<-qCtx.Done()
		for range subCh {
		}
	}
	_ = prod.Close()
}

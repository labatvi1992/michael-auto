package tray

import (
	"bytes"
	"errors"
	"image/png"
	"testing"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

func TestTrayIconsDecodeAsValid32x32PNG(t *testing.T) {
	icons := map[string][]byte{
		"green": IconGreenObserving,
		"amber": IconAmberPaused30s,
		"blue":  IconBlueExecuting,
		"red":   IconRedHardStop,
		"slate": IconSlateDisabled,
	}

	for name, raw := range icons {
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("icon %s failed PNG decode: %v", name, err)
		}
		b := img.Bounds()
		if b.Dx() != 32 || b.Dy() != 32 {
			t.Fatalf("icon %s expected 32x32, got %dx%d", name, b.Dx(), b.Dy())
		}
	}
}

func TestDesktopNotifierDeduplicationAndTrayManagerLifecycle(t *testing.T) {
	sentCount := 0
	lastCritical := false
	n := NewDesktopNotifier()
	n.minGap = 40 * time.Millisecond
	n.dispatchFn = func(title, body string, critical bool) error {
		sentCount++
		lastCritical = critical
		return nil
	}

	// Blank notification is ignored.
	n.Notify("   ", "  ", false)
	if sentCount != 0 {
		t.Fatalf("expected 0 notifications for blank input, got %d", sentCount)
	}

	// Identical notifications within minGap are deduplicated.
	n.Notify("Quota Alert", "Switched account", false)
	n.Notify("Quota Alert", "Switched account", false)
	if sentCount != 1 {
		t.Fatalf("expected deduplication to keep sentCount=1, got %d", sentCount)
	}

	// Different message fires immediately.
	n.Notify("Hard Stop", "MFA detected", true)
	if sentCount != 2 || !lastCritical {
		t.Fatalf("expected critical alert dispatched, sentCount=%d critical=%v", sentCount, lastCritical)
	}

	// Same message fires again once minGap elapses.
	time.Sleep(50 * time.Millisecond)
	n.Notify("Hard Stop", "MFA detected", true)
	if sentCount != 3 {
		t.Fatalf("expected alert resent after minGap, got sentCount=%d", sentCount)
	}

	// Failed dispatch gracefully falls through without panicking.
	n.dispatchFn = func(title, body string, critical bool) error {
		return errors.New("dbus unavailable")
	}
	n.Notify("Fallback test", "Body", false)

	// Tray Manager UpdateStatus + Stop before Start must be completely safe and race-free.
	mgr := NewManager(Callbacks{})
	mgr.UpdateStatus(domain.RuntimeStatus{
		State:             domain.StateObserving,
		AutomationEnabled: true,
		ActiveProfileName: "Work",
		ActiveModel:       "Gemini 4 Neon Extra High",
	})
	mgr.UpdateStatus(domain.RuntimeStatus{
		State:             domain.StatePausedHumanInput,
		YieldRemainingMs:  15000,
		AutomationEnabled: true,
	})
	mgr.UpdateStatus(domain.RuntimeStatus{State: domain.StateAutoExecuting, AutomationEnabled: true})
	mgr.UpdateStatus(domain.RuntimeStatus{State: domain.StateHardStopChallenge, HardStopReason: "MFA"})
	mgr.UpdateStatus(domain.RuntimeStatus{State: domain.StatePausedByUser, AutomationEnabled: false})
	mgr.Stop()
	_ = IsStatusNotifierHostAvailable()
}

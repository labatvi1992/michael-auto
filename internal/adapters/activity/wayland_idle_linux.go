//go:build linux

package activity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/orchestrator"
)

const (
	mutterIdleDest = "org.gnome.Mutter.IdleMonitor"
	mutterIdlePath = dbus.ObjectPath("/org/gnome/Mutter/IdleMonitor/Core")
	mutterIdleIface = "org.gnome.Mutter.IdleMonitor"

	fdoScreenSaverDest  = "org.freedesktop.ScreenSaver"
	fdoScreenSaverPath  = dbus.ObjectPath("/org/freedesktop/ScreenSaver")
	fdoScreenSaverIface = "org.freedesktop.ScreenSaver"

	gnomeScreenSaverDest  = "org.gnome.ScreenSaver"
	gnomeScreenSaverPath  = dbus.ObjectPath("/org/gnome/ScreenSaver")
	gnomeScreenSaverIface = "org.gnome.ScreenSaver"
)

// WaylandActivityMonitor monitors physical human input (mouse movement, click, scroll wheel, keyboard)
// and GNOME Lock Screen / sleep state on Fedora 45 Wayland without root privileges or keylogging.
type WaylandActivityMonitor struct {
	mu              sync.Mutex
	conn            *dbus.Conn
	backend         string
	leaseUntil      time.Time
	activeWatchID   uint32
	idleWatchID     uint32
	closed          bool
	activeExecution atomic.Bool
	screenLocked    atomic.Bool
}

// IsScreenLocked reports whether the GNOME Wayland session screen shield / lock screen is active (P1-1).
func (m *WaylandActivityMonitor) IsScreenLocked() bool {
	return m.screenLocked.Load()
}

// SetScreenLockedForTesting overrides screen-locked state in unit tests.
func (m *WaylandActivityMonitor) SetScreenLockedForTesting(locked bool) {
	m.screenLocked.Store(locked)
}

// NewHumanActivityMonitor initializes the Fedora Wayland D-Bus activity monitor.
func NewHumanActivityMonitor() *WaylandActivityMonitor {
	m := &WaylandActivityMonitor{
		backend: "unavailable",
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return m
	}
	m.conn = conn

	// Probe GNOME Mutter IdleMonitor first (default on Fedora Workstation 45).
	var idleMs uint64
	if err := conn.Object(mutterIdleDest, mutterIdlePath).Call(mutterIdleIface+".GetIdletime", 0).Store(&idleMs); err == nil {
		m.backend = "wayland-mutter-idle-dbus"
		return m
	}

	// Probe Freedesktop ScreenSaver (KDE Plasma / Sway / wlroots portals).
	var idleSec uint32
	if err := conn.Object(fdoScreenSaverDest, fdoScreenSaverPath).Call(fdoScreenSaverIface+".GetSessionIdleTime", 0).Store(&idleSec); err == nil {
		m.backend = "wayland-freedesktop-screensaver"
		return m
	}

	return m
}

// SetActiveExecution switches high-frequency 80ms idle-regression polling on during active UI steps
// and relaxes polling while idle on GNOME Mutter (where event-driven WatchFired signals handle instant wakeups).
func (m *WaylandActivityMonitor) SetActiveExecution(active bool) {
	m.activeExecution.Store(active)
}

// BackendName returns the detected compositor idle backend.
func (m *WaylandActivityMonitor) BackendName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.backend
}

// AcquireActuationLease marks a short window [now, now+ttl] during which synthetic actuation
// pulses from Michael Auto itself are ignored so the monitor never self-interrupts.
func (m *WaylandActivityMonitor) AcquireActuationLease(ttl time.Duration, expectedX, expectedY *int) func() {
	_ = expectedX
	_ = expectedY
	m.mu.Lock()
	until := time.Now().Add(ttl)
	if until.After(m.leaseUntil) {
		m.leaseUntil = until
	}
	m.mu.Unlock()

	return func() {
		m.mu.Lock()
		// Keep a tiny 15ms tail buffer for asynchronous D-Bus signal delivery.
		grace := time.Now().Add(15 * time.Millisecond)
		if grace.Before(m.leaseUntil) {
			m.leaseUntil = grace
		}
		m.mu.Unlock()
	}
}

func (m *WaylandActivityMonitor) inActuationLease() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return time.Now().Before(m.leaseUntil)
}

// SystemIdleDuration queries the Wayland compositor for exact duration without mouse/scroll/keyboard input.
func (m *WaylandActivityMonitor) SystemIdleDuration(ctx context.Context) (time.Duration, error) {
	m.mu.Lock()
	conn := m.conn
	backend := m.backend
	m.mu.Unlock()

	if conn == nil || backend == "unavailable" {
		return 0, orchestrator.ErrIdleUnsupported
	}

	switch backend {
	case "wayland-mutter-idle-dbus":
		var idleMs uint64
		call := conn.Object(mutterIdleDest, mutterIdlePath).CallWithContext(ctx, mutterIdleIface+".GetIdletime", 0)
		if err := call.Store(&idleMs); err != nil {
			return 0, err
		}
		return time.Duration(idleMs) * time.Millisecond, nil

	case "wayland-freedesktop-screensaver":
		var idleMsOrSec uint32
		call := conn.Object(fdoScreenSaverDest, fdoScreenSaverPath).CallWithContext(ctx, fdoScreenSaverIface+".GetSessionIdleTime", 0)
		if err := call.Store(&idleMsOrSec); err != nil {
			return 0, err
		}
		// Freedesktop ScreenSaver returns milliseconds on KDE/GNOME implementations.
		return time.Duration(idleMsOrSec) * time.Millisecond, nil
	}

	return 0, orchestrator.ErrIdleUnsupported
}

// Subscribe streams immediate ActivityCompositorAny events whenever physical mouse/wheel/click/keyboard input occurs.
func (m *WaylandActivityMonitor) Subscribe(ctx context.Context) (<-chan domain.ActivityKind, error) {
	out := make(chan domain.ActivityKind, 16)

	m.mu.Lock()
	conn := m.conn
	backend := m.backend
	m.mu.Unlock()

	if conn == nil || backend == "unavailable" {
		return out, errors.New("wayland compositor idle D-Bus unavailable in current environment")
	}

	if backend == "wayland-mutter-idle-dbus" {
		// Probe initial GNOME ScreenSaver lock state and subscribe to ActiveChanged(bool) (P1-1).
		var initialLocked bool
		if err := conn.Object(gnomeScreenSaverDest, gnomeScreenSaverPath).Call(gnomeScreenSaverIface+".GetActive", 0).Store(&initialLocked); err == nil {
			m.screenLocked.Store(initialLocked)
		}
		_ = conn.AddMatchSignal(
			dbus.WithMatchObjectPath(gnomeScreenSaverPath),
			dbus.WithMatchInterface(gnomeScreenSaverIface),
			dbus.WithMatchMember("ActiveChanged"),
		)

		_ = conn.AddMatchSignal(
			dbus.WithMatchObjectPath(mutterIdlePath),
			dbus.WithMatchInterface(mutterIdleIface),
			dbus.WithMatchMember("WatchFired"),
		)
		sigCh := make(chan *dbus.Signal, 24)
		conn.Signal(sigCh)
		m.armMutterWatches()

		go func() {
			defer conn.RemoveSignal(sigCh)
			for {
				select {
				case <-ctx.Done():
					return
				case sig, ok := <-sigCh:
					if !ok || sig == nil || len(sig.Body) == 0 {
						continue
					}
					if sig.Name == gnomeScreenSaverIface+".ActiveChanged" {
						if locked, isBool := sig.Body[0].(bool); isBool {
							m.screenLocked.Store(locked)
						}
						continue
					}
					firedID, ok := sig.Body[0].(uint32)
					if !ok {
						continue
					}
					m.mu.Lock()
					activeID := m.activeWatchID
					idleID := m.idleWatchID
					m.mu.Unlock()

					if firedID == activeID {
						if !m.inActuationLease() {
							select {
							case out <- domain.ActivityCompositorAny:
							default:
							}
						}
					} else if firedID == idleID {
						// User paused for 400ms; re-arm one-shot AddUserActiveWatch so next touch fires instantly!
						m.rearmUserActiveWatch()
					}
				}
			}
		}()

		// Best-effort subscribe to systemd-logind PrepareForSleep(bool) on system bus to pause across suspend/resume.
		go m.watchLogindSuspend(ctx)
	}

	// Secondary belt-and-suspenders detector: runs at 80ms during active execution (or on ScreenSaver backends),
	// and relaxes to once per 2.4s while idle on event-driven GNOME Mutter WatchFired.
	go func() {
		defer close(out)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()

		var prevIdle time.Duration
		var tickCounter uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tickCounter++
				if backend == "wayland-mutter-idle-dbus" && !m.activeExecution.Load() && tickCounter%30 != 0 {
					continue
				}
				curIdle, err := m.SystemIdleDuration(ctx)
				if err != nil {
					continue
				}
				// If idle time reset downward (e.g. from 600ms -> 10ms) outside an actuation lease, human interacted!
				if prevIdle >= 120*time.Millisecond && curIdle+40*time.Millisecond < prevIdle && !m.inActuationLease() {
					select {
					case out <- domain.ActivityCompositorAny:
					default:
					}
				}
				prevIdle = curIdle
			}
		}
	}()

	return out, nil
}

func (m *WaylandActivityMonitor) watchLogindSuspend(ctx context.Context) {
	sysConn, err := dbus.ConnectSystemBus()
	if err != nil {
		return
	}
	defer sysConn.Close()

	_ = sysConn.AddMatchSignal(
		dbus.WithMatchObjectPath("/org/freedesktop/login1"),
		dbus.WithMatchInterface("org.freedesktop.login1.Manager"),
		dbus.WithMatchMember("PrepareForSleep"),
	)
	ch := make(chan *dbus.Signal, 8)
	sysConn.Signal(ch)
	defer sysConn.RemoveSignal(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-ch:
			if !ok || sig == nil || len(sig.Body) == 0 {
				continue
			}
			if sleeping, isBool := sig.Body[0].(bool); isBool {
				if sleeping {
					m.screenLocked.Store(true)
				} else {
					// After resume, re-query GNOME ScreenSaver because lock screen usually stays active post-wake.
					m.mu.Lock()
					sConn := m.conn
					m.mu.Unlock()
					if sConn != nil {
						var active bool
						if err := sConn.Object(gnomeScreenSaverDest, gnomeScreenSaverPath).Call(gnomeScreenSaverIface+".GetActive", 0).Store(&active); err == nil {
							m.screenLocked.Store(active)
						} else {
							m.screenLocked.Store(false)
						}
					} else {
						m.screenLocked.Store(false)
					}
				}
			}
		}
	}
}

func (m *WaylandActivityMonitor) armMutterWatches() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil {
		return
	}
	obj := m.conn.Object(mutterIdleDest, mutterIdlePath)
	var idleID uint32
	if err := obj.Call(mutterIdleIface+".AddIdleWatch", 0, uint64(400)).Store(&idleID); err == nil {
		m.idleWatchID = idleID
	}
	var activeID uint32
	if err := obj.Call(mutterIdleIface+".AddUserActiveWatch", 0).Store(&activeID); err == nil {
		m.activeWatchID = activeID
	}
}

func (m *WaylandActivityMonitor) rearmUserActiveWatch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil {
		return
	}
	obj := m.conn.Object(mutterIdleDest, mutterIdlePath)
	var activeID uint32
	if err := obj.Call(mutterIdleIface+".AddUserActiveWatch", 0).Store(&activeID); err == nil {
		m.activeWatchID = activeID
	}
}

// Close releases D-Bus watches and connection.
func (m *WaylandActivityMonitor) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.conn == nil {
		return nil
	}
	m.closed = true
	if m.backend == "wayland-mutter-idle-dbus" && m.idleWatchID != 0 {
		_ = m.conn.Object(mutterIdleDest, mutterIdlePath).Call(mutterIdleIface+".RemoveWatch", 0, m.idleWatchID).Err
	}
	return m.conn.Close()
}

//go:build linux

package activity

import (
	"context"
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

var defaultReconnectBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
}

// WaylandActivityMonitor monitors physical human input (mouse movement, click, scroll wheel, keyboard)
// and GNOME Lock Screen / sleep state on Fedora 45 Wayland without root privileges or keylogging.
type WaylandActivityMonitor struct {
	mu               sync.Mutex
	conn             *dbus.Conn
	backend          string
	leaseUntil       time.Time
	activeWatchID    uint32
	idleWatchID      uint32
	closed           bool
	activeExecution  atomic.Bool
	screenLocked     atomic.Bool
	probeFn          func(ctx context.Context) (*dbus.Conn, string, error)
	backoffSchedule  []time.Duration
	reconnectAttempt int
	lastProbeAttempt time.Time
	logindStarted    bool
	mutterEpoch      uint64
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
// If GNOME Shell / Mutter is not yet ready at early boot, Subscribe() and SystemIdleDuration()
// automatically reconnect lazily using exponential backoff (1s -> 2s -> 5s -> 10s).
func NewHumanActivityMonitor() *WaylandActivityMonitor {
	m := &WaylandActivityMonitor{
		backend:         "unavailable",
		backoffSchedule: defaultReconnectBackoff,
	}
	m.probeFn = m.connectAndProbeSessionBus
	_, _ = m.tryReconnect(context.Background(), true)
	return m
}

func (m *WaylandActivityMonitor) connectAndProbeSessionBus(ctx context.Context) (*dbus.Conn, string, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, "unavailable", err
	}

	// Probe GNOME Mutter IdleMonitor first (default on Fedora Workstation 45).
	var idleMs uint64
	if err := conn.Object(mutterIdleDest, mutterIdlePath).CallWithContext(ctx, mutterIdleIface+".GetIdletime", 0).Store(&idleMs); err == nil {
		return conn, "wayland-mutter-idle-dbus", nil
	}

	// Probe Freedesktop ScreenSaver (KDE Plasma / Sway / wlroots portals).
	var idleSec uint32
	if err := conn.Object(fdoScreenSaverDest, fdoScreenSaverPath).CallWithContext(ctx, fdoScreenSaverIface+".GetSessionIdleTime", 0).Store(&idleSec); err == nil {
		return conn, "wayland-freedesktop-screensaver", nil
	}

	_ = conn.Close()
	return nil, "unavailable", orchestrator.ErrIdleUnsupported
}

func (m *WaylandActivityMonitor) nextBackoffDuration(attempt int) time.Duration {
	sched := m.backoffSchedule
	if len(sched) == 0 {
		sched = defaultReconnectBackoff
	}
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(sched) {
		return sched[len(sched)-1]
	}
	return sched[attempt]
}

// tryReconnect attempts to establish or re-establish the session D-Bus connection respecting exponential backoff.
func (m *WaylandActivityMonitor) tryReconnect(ctx context.Context, force bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tryReconnectLocked(ctx, force)
}

func (m *WaylandActivityMonitor) tryReconnectLocked(ctx context.Context, force bool) (bool, error) {
	if m.closed || m.probeFn == nil {
		return false, orchestrator.ErrIdleUnsupported
	}
	if m.backend != "unavailable" && (m.conn != nil || m.backend != "") {
		return true, nil
	}
	if !force && !m.lastProbeAttempt.IsZero() {
		wait := m.nextBackoffDuration(m.reconnectAttempt)
		if time.Since(m.lastProbeAttempt) < wait {
			return false, orchestrator.ErrIdleUnsupported
		}
	}
	m.lastProbeAttempt = time.Now()
	probe := m.probeFn
	m.mu.Unlock()
	conn, backend, err := probe(ctx)
	m.mu.Lock()

	if m.closed {
		if conn != nil {
			_ = conn.Close()
		}
		return false, orchestrator.ErrIdleUnsupported
	}
	if err != nil || backend == "" || backend == "unavailable" {
		if conn != nil {
			_ = conn.Close()
		}
		m.reconnectAttempt++
		return false, orchestrator.ErrIdleUnsupported
	}

	if m.conn != nil && m.conn != conn {
		_ = m.conn.Close()
	}
	m.conn = conn
	m.backend = backend
	m.reconnectAttempt = 0
	m.activeWatchID = 0
	m.idleWatchID = 0
	m.mutterEpoch++
	return true, nil
}

func (m *WaylandActivityMonitor) markDisconnected(failedConn *dbus.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if failedConn != nil && m.conn != failedConn {
		return
	}
	if m.conn != nil {
		_ = m.conn.Close()
		m.conn = nil
	}
	m.backend = "unavailable"
	m.activeWatchID = 0
	m.idleWatchID = 0
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
// Automatically attempts lazy D-Bus reconnection when the compositor becomes available post-boot.
func (m *WaylandActivityMonitor) SystemIdleDuration(ctx context.Context) (time.Duration, error) {
	m.mu.Lock()
	conn := m.conn
	backend := m.backend
	m.mu.Unlock()

	if conn == nil || backend == "unavailable" {
		if ok, _ := m.tryReconnect(ctx, false); ok {
			m.mu.Lock()
			conn = m.conn
			backend = m.backend
			m.mu.Unlock()
		}
	}

	if conn == nil || backend == "unavailable" {
		return 0, orchestrator.ErrIdleUnsupported
	}

	switch backend {
	case "wayland-mutter-idle-dbus":
		var idleMs uint64
		call := conn.Object(mutterIdleDest, mutterIdlePath).CallWithContext(ctx, mutterIdleIface+".GetIdletime", 0)
		if err := call.Store(&idleMs); err != nil {
			if ctx.Err() == nil {
				m.markDisconnected(conn)
			}
			return 0, err
		}
		return time.Duration(idleMs) * time.Millisecond, nil

	case "wayland-freedesktop-screensaver":
		var idleMsOrSec uint32
		call := conn.Object(fdoScreenSaverDest, fdoScreenSaverPath).CallWithContext(ctx, fdoScreenSaverIface+".GetSessionIdleTime", 0)
		if err := call.Store(&idleMsOrSec); err != nil {
			if ctx.Err() == nil {
				m.markDisconnected(conn)
			}
			return 0, err
		}
		// Freedesktop ScreenSaver returns milliseconds on KDE/GNOME implementations.
		return time.Duration(idleMsOrSec) * time.Millisecond, nil
	}

	return 0, orchestrator.ErrIdleUnsupported
}

// Subscribe streams immediate ActivityCompositorAny events whenever physical mouse/wheel/click/keyboard input occurs.
// Never permanently fails when started early at boot before GNOME Shell exports IdleMonitor; instead lazily
// reconnects with exponential backoff (1s -> 2s -> 5s -> 10s) and arms Mutter + ScreenSaver signals automatically.
func (m *WaylandActivityMonitor) Subscribe(ctx context.Context) (<-chan domain.ActivityKind, error) {
	out := make(chan domain.ActivityKind, 16)

	attachIfMutter := func(attachedEpoch *uint64) {
		m.mu.Lock()
		conn := m.conn
		backend := m.backend
		epoch := m.mutterEpoch
		startLogind := !m.logindStarted
		if backend == "wayland-mutter-idle-dbus" && startLogind {
			m.logindStarted = true
		}
		m.mu.Unlock()

		if backend != "wayland-mutter-idle-dbus" || conn == nil || epoch == *attachedEpoch {
			return
		}
		*attachedEpoch = epoch

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

		go func(c *dbus.Conn, ch chan *dbus.Signal) {
			defer c.RemoveSignal(ch)
			for {
				select {
				case <-ctx.Done():
					return
				case sig, ok := <-ch:
					if !ok {
						m.markDisconnected(c)
						return
					}
					if sig == nil || len(sig.Body) == 0 {
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
		}(conn, sigCh)

		if startLogind {
			// Best-effort subscribe to systemd-logind PrepareForSleep(bool) on system bus to pause across suspend/resume.
			go m.watchLogindSuspend(ctx)
		}
	}

	var attachedEpoch uint64
	attachIfMutter(&attachedEpoch)

	// Secondary belt-and-suspenders detector & lazy D-Bus reconnector:
	// - When unavailable (early boot or compositor restart), retries with exponential backoff (1s->2s->5s->10s)
	//   and automatically arms Mutter WatchFired + GNOME ScreenSaver ActiveChanged once available.
	// - Runs at 80ms during active execution (or ScreenSaver backends), relaxes to 2.4s while idle on Mutter.
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
				m.mu.Lock()
				curBackend := m.backend
				m.mu.Unlock()

				if curBackend == "unavailable" {
					if ok, _ := m.tryReconnect(ctx, false); ok {
						attachIfMutter(&attachedEpoch)
					}
					continue
				}

				if curBackend == "wayland-mutter-idle-dbus" && !m.activeExecution.Load() && tickCounter%30 != 0 {
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
	if m.closed {
		return nil
	}
	m.closed = true
	if m.conn == nil {
		return nil
	}
	if m.backend == "wayland-mutter-idle-dbus" && m.idleWatchID != 0 {
		_ = m.conn.Object(mutterIdleDest, mutterIdlePath).Call(mutterIdleIface+".RemoveWatch", 0, m.idleWatchID).Err
	}
	err := m.conn.Close()
	m.conn = nil
	return err
}

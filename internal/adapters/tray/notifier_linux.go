//go:build linux

package tray

import (
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	fdoNotifyDest  = "org.freedesktop.Notifications"
	fdoNotifyPath  = dbus.ObjectPath("/org/freedesktop/Notifications")
	fdoNotifyIface = "org.freedesktop.Notifications"
)

// DesktopNotifier delivers native GNOME / Freedesktop desktop notifications on Fedora 45 Wayland
// with built-in anti-spam deduplication.
type DesktopNotifier struct {
	mu         sync.Mutex
	lastKey    string
	lastSent   time.Time
	minGap     time.Duration
	dispatchFn func(title, body string, critical bool) error
}

// NewDesktopNotifier constructs a native Linux D-Bus desktop notifier.
func NewDesktopNotifier() *DesktopNotifier {
	return &DesktopNotifier{
		minGap:     20 * time.Second,
		dispatchFn: sendDBusNotification,
	}
}

// Notify dispatches a desktop notification unless an identical alert was sent within minGap.
func (n *DesktopNotifier) Notify(title, body string, critical bool) {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title == "" && body == "" {
		return
	}

	key := title + "|" + body
	now := time.Now()
	n.mu.Lock()
	if n.lastKey == key && now.Sub(n.lastSent) < n.minGap {
		n.mu.Unlock()
		return
	}
	n.lastKey = key
	n.lastSent = now
	dispatch := n.dispatchFn
	n.mu.Unlock()

	if dispatch == nil {
		dispatch = sendDBusNotification
	}
	if dispatch(title, body, critical) == nil {
		return
	}

	if notifySend, err := exec.LookPath("notify-send"); err == nil {
		urgency := "normal"
		if critical {
			urgency = "critical"
		}
		_ = exec.Command(notifySend, "-a", "Michael Auto", "-u", urgency, title, body).Start()
	}
}

func sendDBusNotification(title, body string, critical bool) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	defer conn.Close()

	urgency := byte(1)
	expireMs := int32(6000)
	if critical {
		urgency = 2
		expireMs = 15000
	}

	hints := map[string]dbus.Variant{
		"urgency": dbus.MakeVariant(urgency),
	}
	var outID uint32
	return conn.Object(fdoNotifyDest, fdoNotifyPath).Call(
		fdoNotifyIface+".Notify",
		0,
		"Michael Auto",
		uint32(0),
		"dialog-information",
		title,
		body,
		[]string{},
		hints,
		expireMs,
	).Store(&outID)
}

// IsStatusNotifierHostAvailable checks whether the current Wayland desktop (e.g. GNOME Shell)
// has an active StatusNotifierHost (such as AppIndicator) displaying system tray icons.
func IsStatusNotifierHostAvailable() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()

	var v dbus.Variant
	err = conn.Object("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher").Call(
		"org.freedesktop.DBus.Properties.Get",
		0,
		"org.kde.StatusNotifierWatcher",
		"IsStatusNotifierHostRegistered",
	).Store(&v)
	if err != nil {
		return false
	}
	ok, _ := v.Value().(bool)
	return ok
}

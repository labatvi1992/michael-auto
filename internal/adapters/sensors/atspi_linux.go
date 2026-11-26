//go:build linux

package sensors

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/labatvi1992/michael-auto/internal/domain"
)

const (
	a11yBusDest      = "org.a11y.Bus"
	a11yBusPath      = dbus.ObjectPath("/org/a11y/bus")
	atspiRegistryDst = "org.a11y.atspi.Registry"
	atspiRootPath    = dbus.ObjectPath("/org/a11y/atspi/accessible/root")
	atspiAccIface    = "org.a11y.atspi.Accessible"
	atspiCompIface   = "org.a11y.atspi.Component"
	atspiActIface    = "org.a11y.atspi.Action"

	maxATSPIDepth = 24
	maxATSPINodes = 600
)

type atspiRef struct {
	Name string
	Path dbus.ObjectPath
}

// ATSPI2Sensor queries the Fedora Wayland AT-SPI2 Accessibility D-Bus tree scoped strictly
// to target application windows without capturing screen pixels, caching its D-Bus connection.
type ATSPI2Sensor struct {
	mu   sync.Mutex
	conn *dbus.Conn
}

// NewATSPI2Sensor constructs a Tier-2 AT-SPI2 fallback sensor.
func NewATSPI2Sensor() *ATSPI2Sensor {
	return &ATSPI2Sensor{}
}

// TierName returns "atspi2".
func (s *ATSPI2Sensor) TierName() string {
	return "atspi2"
}

func (s *ATSPI2Sensor) getOrDialConn(ctx context.Context) (*dbus.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		return s.conn, nil
	}
	conn, err := dialAccessibilityBus(ctx)
	if err != nil {
		return nil, err
	}
	s.conn = conn
	return conn, nil
}

func (s *ATSPI2Sensor) resetConn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

// Observe queries the AT-SPI2 bus bounded by context timeout and node budget.
func (s *ATSPI2Sensor) Observe(ctx context.Context, cfg domain.AppConfig) (domain.ScreenState, error) {
	obsCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()

	conn, err := s.getOrDialConn(obsCtx)
	if err != nil {
		return domain.ScreenState{
			ObservedAt:   time.Now(),
			AppAuthState: domain.AuthUnknown,
			SourceTier:   s.TierName(),
		}, err
	}

	roots := s.collectWindows(obsCtx, conn, cfg.TargetAppKeywords)
	if roots == nil && obsCtx.Err() == nil {
		// Reconnect once in case session accessibility bus restarted.
		s.resetConn()
		if retryConn, retryErr := s.getOrDialConn(obsCtx); retryErr == nil {
			roots = s.collectWindows(obsCtx, retryConn, cfg.TargetAppKeywords)
		}
	}
	state := AnalyzeAccessibleTree(roots, cfg.TargetAppKeywords, cfg.Profiles)
	return state, nil
}

// Health checks whether org.a11y.Bus is reachable on the Fedora Wayland session.
func (s *ATSPI2Sensor) Health(ctx context.Context, cfg domain.AppConfig) domain.SensorHealth {
	_ = cfg
	checkCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()

	if _, err := s.getOrDialConn(checkCtx); err != nil {
		return domain.SensorHealth{
			TierName:    s.TierName(),
			Healthy:     false,
			Detail:      "AT-SPI2 Accessibility Bus chưa kết nối: " + err.Error(),
			LastChecked: time.Now(),
		}
	}
	return domain.SensorHealth{
		TierName:    s.TierName(),
		Healthy:     true,
		Detail:      "Kết nối AT-SPI2 D-Bus (org.a11y.Bus) sẵn sàng (Persistent Bus)",
		LastChecked: time.Now(),
	}
}

func dialAccessibilityBus(ctx context.Context) (*dbus.Conn, error) {
	sessionConn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	defer sessionConn.Close()

	var addr string
	call := sessionConn.Object(a11yBusDest, a11yBusPath).CallWithContext(ctx, "org.a11y.Bus.GetAddress", 0)
	if err := call.Store(&addr); err != nil || strings.TrimSpace(addr) == "" {
		return nil, err
	}

	conn, err := dbus.Dial(addr)
	if err != nil {
		return nil, err
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func (s *ATSPI2Sensor) collectWindows(ctx context.Context, conn *dbus.Conn, keywords []string) []AccessibleNode {
	var apps []atspiRef
	rootObj := conn.Object(atspiRegistryDst, atspiRootPath)
	if err := rootObj.CallWithContext(ctx, atspiAccIface+".GetChildren", 0).Store(&apps); err != nil {
		return nil
	}

	if len(keywords) == 0 {
		keywords = []string{"Antigravity"}
	}

	var windows []AccessibleNode
	budget := maxATSPINodes

	for _, app := range apps {
		if ctx.Err() != nil || budget <= 0 {
			break
		}
		if app.Name == "" || app.Path == "" {
			continue
		}
		appName := getStringProp(ctx, conn, app.Name, app.Path, atspiAccIface+".Name")

		var winRefs []atspiRef
		if err := conn.Object(app.Name, app.Path).CallWithContext(ctx, atspiAccIface+".GetChildren", 0).Store(&winRefs); err != nil {
			continue
		}

		for _, wref := range winRefs {
			if ctx.Err() != nil || budget <= 0 {
				break
			}
			winTitle := getStringProp(ctx, conn, wref.Name, wref.Path, atspiAccIface+".Name")
			combined := appName + " " + winTitle
			if !matchesAnyKeyword(combined, keywords) {
				continue
			}
			node := s.walkSubtree(ctx, conn, wref, 0, &budget)
			if node.Name == "" {
				node.Name = combined
			}
			windows = append(windows, node)
		}
	}

	return windows
}

func (s *ATSPI2Sensor) walkSubtree(ctx context.Context, conn *dbus.Conn, ref atspiRef, depth int, budget *int) AccessibleNode {
	if *budget <= 0 || depth > maxATSPIDepth || ctx.Err() != nil {
		return AccessibleNode{}
	}
	*budget--

	obj := conn.Object(ref.Name, ref.Path)
	name := getStringProp(ctx, conn, ref.Name, ref.Path, atspiAccIface+".Name")
	desc := getStringProp(ctx, conn, ref.Name, ref.Path, atspiAccIface+".Description")

	var roleName string
	_ = obj.CallWithContext(ctx, atspiAccIface+".GetRoleName", 0).Store(&roleName)

	var nActions int32
	_ = obj.CallWithContext(ctx, atspiActIface+".NActions", 0).Store(&nActions)

	var x, y, w, h int32
	_ = obj.CallWithContext(ctx, atspiCompIface+".GetExtents", 0, uint32(0)).Store(&x, &y, &w, &h)

	node := AccessibleNode{
		Role:        roleName,
		Name:        name,
		Description: desc,
		BusName:     ref.Name,
		ObjectPath:  string(ref.Path),
		HasAction:   nActions > 0,
		ActionIndex: 0,
		X:           int(x),
		Y:           int(y),
		Width:       int(w),
		Height:      int(h),
	}

	roleLower := strings.ToLower(roleName)
	// Do not descend deeply into massive code editor/document trees (privacy + CPU efficiency).
	if editorContainerRoles[roleLower] {
		return node
	}

	var children []atspiRef
	if depth < maxATSPIDepth && *budget > 0 {
		if err := obj.CallWithContext(ctx, atspiAccIface+".GetChildren", 0).Store(&children); err == nil {
			for _, ch := range children {
				if *budget <= 0 || ctx.Err() != nil {
					break
				}
				node.Children = append(node.Children, s.walkSubtree(ctx, conn, ch, depth+1, budget))
			}
		}
	}

	return node
}

func getStringProp(ctx context.Context, conn *dbus.Conn, dest string, path dbus.ObjectPath, prop string) string {
	parts := strings.SplitN(prop, ".", 4)
	if len(parts) < 4 {
		return ""
	}
	iface := strings.Join(parts[:3], ".")
	member := parts[3]
	var v dbus.Variant
	if err := conn.Object(dest, path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, member).Store(&v); err != nil {
		return ""
	}
	if s, ok := v.Value().(string); ok {
		return s
	}
	return ""
}

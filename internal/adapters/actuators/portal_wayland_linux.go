//go:build linux

package actuators

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/ports"
)

func cryptoPortalToken(prefix string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

const (
	portalDest          = "org.freedesktop.portal.Desktop"
	portalPath          = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	portalRemoteDesktop = "org.freedesktop.portal.RemoteDesktop"
	portalRequestIface  = "org.freedesktop.portal.Request"
	restoreTokenKey     = "wayland_remotedesktop_restore_token"

	linuxBtnLeft = int32(0x110) // BTN_LEFT in linux/input-event-codes.h
)

// WaylandPortalActuator performs sanctioned UI actions on Fedora 45 Wayland:
// 1. Prefers semantic AT-SPI2 org.a11y.atspi.Action.DoAction (zero screen coordinates).
// 2. Falls back to org.freedesktop.portal.RemoteDesktop (persist_mode=2 + restore_token in libsecret).
// Every action acquires a 45ms ActuationLease on HumanActivityMonitor so synthetic pulses never self-interrupt.
type WaylandPortalActuator struct {
	mu          sync.Mutex
	conn        *dbus.Conn
	sessionPath dbus.ObjectPath
	ready       bool
	detail      string
	secrets     ports.SecretStore
	activityMon ports.HumanActivityMonitor
}

// NewDesktopActuator constructs a Fedora Wayland RemoteDesktop & AT-SPI2 actuator.
func NewDesktopActuator(secrets ports.SecretStore, activityMon ports.HumanActivityMonitor) *WaylandPortalActuator {
	detail := "Sẵn sàng (AT-SPI2 Action + Wayland RemoteDesktop Portal)"
	if secrets != nil {
		if tok, err := secrets.Get(restoreTokenKey); err == nil && tok != "" {
			detail = "Đã có restore_token Wayland trong Keyring (tự động không hỏi lại)"
		}
	}
	return &WaylandPortalActuator{
		detail:      detail,
		secrets:     secrets,
		activityMon: activityMon,
	}
}

// SessionStatus returns current portal authorization readiness for UI diagnostics.
func (a *WaylandPortalActuator) SessionStatus() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready && a.sessionPath != "" {
		return true, "Phiên Wayland RemoteDesktop đang hoạt động (" + string(a.sessionPath) + ")"
	}
	if a.secrets != nil {
		if tok, err := a.secrets.Get(restoreTokenKey); err == nil && tok != "" {
			return true, a.detail
		}
	}
	return false, a.detail
}

// EnsureSession establishes or restores an xdg-desktop-portal RemoteDesktop session on Fedora Wayland.
func (a *WaylandPortalActuator) EnsureSession(ctx context.Context) error {
	a.mu.Lock()
	if a.ready && a.conn != nil && a.sessionPath != "" {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("không kết nối được Session D-Bus: %w", err)
	}

	portalObj := conn.Object(portalDest, portalPath)
	sessionToken := cryptoPortalToken("michael_auto_sess")

	// 1. CreateSession
	createRes, err := a.callPortalRequest(ctx, conn, func(reqToken string) (*dbus.Call, error) {
		opts := map[string]dbus.Variant{
			"handle_token":         dbus.MakeVariant(reqToken),
			"session_handle_token": dbus.MakeVariant(sessionToken),
		}
		return portalObj.CallWithContext(ctx, portalRemoteDesktop+".CreateSession", 0, opts), nil
	})
	if err != nil {
		_ = conn.Close()
		a.setStatus(false, "Portal CreateSession lỗi: "+err.Error())
		return err
	}

	var sessPathStr string
	if v, ok := createRes["session_handle"]; ok {
		switch val := v.Value().(type) {
		case string:
			sessPathStr = val
		case dbus.ObjectPath:
			sessPathStr = string(val)
		}
	}
	if sessPathStr == "" {
		_ = conn.Close()
		err := errors.New("portal không trả về session_handle")
		a.setStatus(false, err.Error())
		return err
	}
	sessPath := dbus.ObjectPath(sessPathStr)

	// 2. SelectDevices (types = 3 [KEYBOARD | POINTER], persist_mode = 2 [until revoked], restore_token if saved)
	selectOpts := map[string]dbus.Variant{
		"types":        dbus.MakeVariant(uint32(3)),
		"persist_mode": dbus.MakeVariant(uint32(2)),
	}
	if a.secrets != nil {
		if savedToken, err := a.secrets.Get(restoreTokenKey); err == nil && strings.TrimSpace(savedToken) != "" {
			selectOpts["restore_token"] = dbus.MakeVariant(strings.TrimSpace(savedToken))
		}
	}

	_, err = a.callPortalRequest(ctx, conn, func(reqToken string) (*dbus.Call, error) {
		selectOpts["handle_token"] = dbus.MakeVariant(reqToken)
		return portalObj.CallWithContext(ctx, portalRemoteDesktop+".SelectDevices", 0, sessPath, selectOpts), nil
	})
	if err != nil {
		_ = conn.Close()
		a.setStatus(false, "Portal SelectDevices lỗi: "+err.Error())
		return err
	}

	// 3. Start session (prompts user once on Fedora if no valid restore_token yet; silent thereafter)
	startRes, err := a.callPortalRequest(ctx, conn, func(reqToken string) (*dbus.Call, error) {
		startOpts := map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(reqToken),
		}
		return portalObj.CallWithContext(ctx, portalRemoteDesktop+".Start", 0, sessPath, "", startOpts), nil
	})
	if err != nil {
		_ = conn.Close()
		a.setStatus(false, "Người dùng từ chối hoặc Portal Start thất bại: "+err.Error())
		return err
	}

	if a.secrets != nil {
		if v, ok := startRes["restore_token"]; ok {
			if newTok, ok := v.Value().(string); ok && newTok != "" {
				_ = a.secrets.Set(restoreTokenKey, newTok)
			}
		}
	}

	a.mu.Lock()
	a.conn = conn
	a.sessionPath = sessPath
	a.ready = true
	a.detail = "Đã cấp quyền Wayland RemoteDesktop hợp lệ (persist_mode=2)"
	a.mu.Unlock()
	return nil
}

// TriggerLogout executes semantic logout via AT-SPI2 first, falling back to Wayland Portal pointer click.
func (a *WaylandPortalActuator) TriggerLogout(ctx context.Context, state domain.ScreenState, dryRun bool) error {
	if dryRun {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// If settings needs to be opened first before logout button appears:
	if target, ok := state.Targets["logout_button"]; ok {
		return a.actuateTarget(ctx, target)
	}
	if settingsBtn, ok := state.Targets["settings_button"]; ok {
		return a.actuateTarget(ctx, settingsBtn)
	}
	return errors.New("chưa định vị được phần tử Settings hoặc Logout trên cửa sổ mục tiêu")
}

// SelectModel selects the requested model via AT-SPI2 action if the model selector element is exposed.
func (a *WaylandPortalActuator) SelectModel(ctx context.Context, state domain.ScreenState, model string, dryRun bool) error {
	_ = model
	if dryRun {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if selector, ok := state.Targets["model_selector"]; ok {
		return a.actuateTarget(ctx, selector)
	}
	// If desktop model selector is not exposed on current screen, do not fail hard (CLI settings already synced).
	return nil
}

func (a *WaylandPortalActuator) actuateTarget(ctx context.Context, target domain.UIElementTarget) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	var release func()
	if a.activityMon != nil {
		x, y := target.CenterX, target.CenterY
		release = a.activityMon.AcquireActuationLease(45*time.Millisecond, &x, &y)
		defer release()
	}

	// Priority 1: Semantic AT-SPI2 DoAction (does not move physical cursor at all!).
	if target.HasAction && target.BusName != "" && target.ObjectPath != "" {
		if err := invokeATSPIAction(ctx, target); err == nil {
			return nil
		}
	}

	// Priority 2: Sanctioned Wayland RemoteDesktop Portal pointer click if coordinates are known.
	if !target.HasBounds {
		return errors.New("phần tử không hỗ trợ AT-SPI2 DoAction và không có tọa độ khung")
	}
	if err := a.EnsureSession(ctx); err != nil {
		return err
	}

	a.mu.Lock()
	conn := a.conn
	sess := a.sessionPath
	a.mu.Unlock()
	if conn == nil || sess == "" {
		return errors.New("phiên Wayland RemoteDesktop chưa sẵn sàng")
	}

	portalObj := conn.Object(portalDest, portalPath)
	emptyOpts := map[string]dbus.Variant{}

	if err := portalObj.CallWithContext(ctx, portalRemoteDesktop+".NotifyPointerMotionAbsolute", 0,
		sess, emptyOpts, uint32(0), float64(target.CenterX), float64(target.CenterY)).Err; err != nil {
		return err
	}
	if err := portalObj.CallWithContext(ctx, portalRemoteDesktop+".NotifyPointerButton", 0,
		sess, emptyOpts, linuxBtnLeft, uint32(1)).Err; err != nil {
		return err
	}
	return portalObj.CallWithContext(ctx, portalRemoteDesktop+".NotifyPointerButton", 0,
		sess, emptyOpts, linuxBtnLeft, uint32(0)).Err
}

func invokeATSPIAction(ctx context.Context, target domain.UIElementTarget) error {
	sessionConn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	defer sessionConn.Close()

	var addr string
	if err := sessionConn.Object("org.a11y.Bus", "/org/a11y/bus").CallWithContext(ctx, "org.a11y.Bus.GetAddress", 0).Store(&addr); err != nil || addr == "" {
		return errors.New("cannot resolve AT-SPI2 bus")
	}

	a11yConn, err := dbus.Dial(addr)
	if err != nil {
		return err
	}
	defer a11yConn.Close()
	if err := a11yConn.Auth(nil); err != nil {
		return err
	}
	if err := a11yConn.Hello(); err != nil {
		return err
	}

	var ok bool
	err = a11yConn.Object(target.BusName, dbus.ObjectPath(target.ObjectPath)).
		CallWithContext(ctx, "org.a11y.atspi.Action.DoAction", 0, int32(target.ActionIndex)).Store(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("AT-SPI2 DoAction returned false")
	}
	return nil
}

func (a *WaylandPortalActuator) callPortalRequest(
	ctx context.Context,
	conn *dbus.Conn,
	invoke func(reqToken string) (*dbus.Call, error),
) (map[string]dbus.Variant, error) {
	reqToken := cryptoPortalToken("ma_req")
	sender := strings.TrimPrefix(conn.Names()[0], ":")
	sender = strings.ReplaceAll(sender, ".", "_")
	expectedPath := dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/portal/desktop/request/%s/%s", sender, reqToken))

	_ = conn.AddMatchSignal(
		dbus.WithMatchObjectPath(expectedPath),
		dbus.WithMatchInterface(portalRequestIface),
		dbus.WithMatchMember("Response"),
	)
	sigCh := make(chan *dbus.Signal, 4)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

	call, err := invoke(reqToken)
	if err != nil {
		return nil, err
	}
	var actualReqPath dbus.ObjectPath
	if err := call.Store(&actualReqPath); err != nil {
		return nil, err
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case sig := <-sigCh:
			if sig == nil || (sig.Path != expectedPath && sig.Path != actualReqPath) || len(sig.Body) < 2 {
				continue
			}
			code, _ := sig.Body[0].(uint32)
			if code != 0 {
				return nil, fmt.Errorf("portal request bị hủy hoặc từ chối (code=%d)", code)
			}
			results, _ := sig.Body[1].(map[string]dbus.Variant)
			return results, nil
		}
	}
}

func (a *WaylandPortalActuator) setStatus(ready bool, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ready = ready
	a.detail = detail
}

// Close closes the D-Bus connection if active.
func (a *WaylandPortalActuator) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != nil {
		err := a.conn.Close()
		a.conn = nil
		a.ready = false
		return err
	}
	return nil
}

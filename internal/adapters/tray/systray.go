package tray

import (
	"fmt"
	"sync"

	"fyne.io/systray"
	"github.com/labatvi1992/michael-auto/internal/domain"
)

// Callbacks connects System Tray menu actions back to the application controller.
type Callbacks struct {
	OnShowWindow       func()
	OnToggleAutomation func()
	OnResetHardStop    func()
	OnQuit             func()
}

// Manager drives the StatusNotifierItem D-Bus system tray on Fedora Wayland.
type Manager struct {
	mu          sync.Mutex
	started     bool
	ready       bool
	callbacks   Callbacks
	lastState   domain.FSMState
	statusItem  *systray.MenuItem
	profileItem *systray.MenuItem
	toggleItem  *systray.MenuItem
	resetItem   *systray.MenuItem
	stopFunc    func()
}

// NewManager creates a System Tray manager.
func NewManager(cb Callbacks) *Manager {
	return &Manager{callbacks: cb}
}

// Start initializes the StatusNotifierItem tray asynchronously using systray.RunWithExternalLoop
// so it coexists cleanly with GTK/Wails main thread on Fedora 45 Wayland.
func (m *Manager) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()

	start, end := systray.RunWithExternalLoop(m.onReady, func() {})
	m.mu.Lock()
	m.stopFunc = end
	m.mu.Unlock()

	// Protect against panics if D-Bus session bus is absent in headless CI.
	go func() {
		defer func() { _ = recover() }()
		start()
	}()
}

func (m *Manager) onReady() {
	systray.SetIcon(IconGreenObserving)
	systray.SetTitle("Michael Auto")
	systray.SetTooltip("Michael Auto — Điều phối hạn mức tự động (Fedora Wayland)")

	statusItem := systray.AddMenuItem("🟢 Đang khởi động...", "Trạng thái máy điều phối hiện tại")
	statusItem.Disable()
	profileItem := systray.AddMenuItem("Hồ sơ: --", "Hồ sơ và Model đang kích hoạt")
	profileItem.Disable()

	systray.AddSeparator()
	openItem := systray.AddMenuItem("Mở Bảng điều khiển", "Hiển thị cửa sổ quản trị Michael Auto")
	toggleItem := systray.AddMenuItem("Tạm dừng Tự động hóa", "Bật hoặc tạm dừng toàn bộ điều phối tự động")
	resetItem := systray.AddMenuItem("Đặt lại Chốt An toàn (Hard-Stop)", "Xóa trạng thái dừng cứng sau khi xử lý MFA/CAPTCHA")
	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Thoát Michael Auto", "Đóng hoàn toàn dịch vụ nền và giao diện")

	m.mu.Lock()
	m.ready = true
	m.statusItem = statusItem
	m.profileItem = profileItem
	m.toggleItem = toggleItem
	m.resetItem = resetItem
	m.mu.Unlock()

	go func() {
		for {
			select {
			case <-openItem.ClickedCh:
				if m.callbacks.OnShowWindow != nil {
					m.callbacks.OnShowWindow()
				}
			case <-toggleItem.ClickedCh:
				if m.callbacks.OnToggleAutomation != nil {
					m.callbacks.OnToggleAutomation()
				}
			case <-resetItem.ClickedCh:
				if m.callbacks.OnResetHardStop != nil {
					m.callbacks.OnResetHardStop()
				}
			case <-quitItem.ClickedCh:
				if m.callbacks.OnQuit != nil {
					m.callbacks.OnQuit()
				}
				return
			}
		}
	}()
}

// UpdateStatus refreshes tray icon, tooltip, and live 30s countdown labels.
func (m *Manager) UpdateStatus(st domain.RuntimeStatus) {
	m.mu.Lock()
	if !m.ready {
		m.mu.Unlock()
		return
	}
	prevState := m.lastState
	m.lastState = st.State
	statusItem := m.statusItem
	profileItem := m.profileItem
	toggleItem := m.toggleItem
	m.mu.Unlock()

	if prevState != st.State {
		switch st.State {
		case domain.StateObserving:
			systray.SetIcon(IconGreenObserving)
		case domain.StatePausedHumanInput:
			systray.SetIcon(IconAmberPaused30s)
		case domain.StatePreFlightCheck, domain.StateAutoExecuting, domain.StateVerifyingState, domain.StateCooldown:
			systray.SetIcon(IconBlueExecuting)
		case domain.StateHardStopChallenge:
			systray.SetIcon(IconRedHardStop)
		case domain.StatePausedByUser:
			systray.SetIcon(IconSlateDisabled)
		}
	}

	var label string
	switch st.State {
	case domain.StatePausedHumanInput:
		label = fmt.Sprintf("🟡 Tạm dừng nhường thao tác: còn %.0fs", float64(st.YieldRemainingMs)/1000.0)
	case domain.StateObserving:
		label = "🟢 Đang quan sát tự động"
	case domain.StateAutoExecuting, domain.StateVerifyingState, domain.StatePreFlightCheck:
		if st.IntentWaitRemainingMs > 0 {
			label = fmt.Sprintf("⏳ Đã SignOut — chờ %.0fs để SignIn lại", float64(st.IntentWaitRemainingMs)/1000.0)
		} else {
			label = "🔵 Đang tự động điều phối qua CUA..."
		}
	case domain.StateCooldown:
		label = fmt.Sprintf("🔵 Nghỉ ổn định: còn %.0fs", float64(st.CooldownRemainingMs)/1000.0)
	case domain.StateHardStopChallenge:
		label = "🔴 Dừng cứng: Cần xác thực / Kiểm tra"
	case domain.StatePausedByUser:
		label = "⚪ Đang tạm tắt bởi người dùng"
	default:
		label = string(st.State)
	}

	statusItem.SetTitle(label)
	systray.SetTooltip("Michael Auto — " + label)

	profText := st.ActiveProfileName
	if profText == "" {
		profText = st.ActiveProfileID
	}
	if st.ActiveModel != "" {
		profText += " • " + st.ActiveModel
	}
	profileItem.SetTitle("Hồ sơ: " + profText)

	if st.AutomationEnabled {
		toggleItem.SetTitle("Tạm dừng Tự động hóa")
	} else {
		toggleItem.SetTitle("Tiếp tục Tự động hóa")
	}
}

// Stop shuts down the external systray loop cleanly.
func (m *Manager) Stop() {
	m.mu.Lock()
	stop := m.stopFunc
	m.stopFunc = nil
	m.ready = false
	m.mu.Unlock()

	if stop != nil {
		defer func() { _ = recover() }()
		stop()
	}
}

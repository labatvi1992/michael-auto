//go:build !linux

package actuators

import (
	"context"

	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/ports"
)

type StubDesktopActuator struct{}

func NewDesktopActuator(secrets ports.SecretStore, activityMon ports.HumanActivityMonitor) *StubDesktopActuator {
	return &StubDesktopActuator{}
}

func (a *StubDesktopActuator) EnsureSession(ctx context.Context) error {
	return nil
}

func (a *StubDesktopActuator) SessionStatus() (bool, string) {
	return false, "Wayland Portal chỉ khả dụng trên Linux"
}

func (a *StubDesktopActuator) TriggerLogout(ctx context.Context, state domain.ScreenState, dryRun bool) error {
	return nil
}

func (a *StubDesktopActuator) SelectModel(ctx context.Context, state domain.ScreenState, model string, dryRun bool) error {
	return nil
}

func (a *StubDesktopActuator) Close() error {
	return nil
}

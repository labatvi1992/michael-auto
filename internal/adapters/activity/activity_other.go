//go:build !linux

package activity

import (
	"context"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/orchestrator"
)

type StubActivityMonitor struct{}

func NewHumanActivityMonitor() *StubActivityMonitor {
	return &StubActivityMonitor{}
}

func (m *StubActivityMonitor) BackendName() string {
	return "stub-non-linux"
}

func (m *StubActivityMonitor) AcquireActuationLease(ttl time.Duration, expectedX, expectedY *int) func() {
	return func() {}
}

func (m *StubActivityMonitor) SetActiveExecution(active bool) {
	_ = active
}

func (m *StubActivityMonitor) SystemIdleDuration(ctx context.Context) (time.Duration, error) {
	return 0, orchestrator.ErrIdleUnsupported
}

func (m *StubActivityMonitor) Subscribe(ctx context.Context) (<-chan domain.ActivityKind, error) {
	ch := make(chan domain.ActivityKind)
	return ch, nil
}

func (m *StubActivityMonitor) Close() error {
	return nil
}

package sensors

import (
	"context"
	"fmt"
	"time"

	"github.com/labatvi1992/michael-auto/internal/adapters/actuators"
	"github.com/labatvi1992/michael-auto/internal/domain"
)

// CuaDriverSensor implements ports.StateSensor using the supervised trycua/cua
// Python sidecar (`michael_cua_sidecar`), sharing the single process with CuaDriverActuator.
type CuaDriverSensor struct {
	actuator *actuators.CuaDriverActuator
}

// NewCuaDriverSensor constructs a CUA-backed StateSensor sharing the actuator's sidecar process.
func NewCuaDriverSensor(actuator *actuators.CuaDriverActuator) *CuaDriverSensor {
	return &CuaDriverSensor{actuator: actuator}
}

// TierName returns "cua_driver".
func (s *CuaDriverSensor) TierName() string {
	return "cua_driver"
}

// Observe queries the CUA sidecar's passive window state observer.
func (s *CuaDriverSensor) Observe(ctx context.Context, cfg domain.AppConfig) (domain.ScreenState, error) {
	now := time.Now()
	empty := domain.ScreenState{
		ObservedAt:   now,
		AppAuthState: domain.AuthUnknown,
		SourceTier:   s.TierName(),
		Targets:      make(map[string]domain.UIElementTarget),
	}
	if !cfg.CuaDriver.Enabled || s.actuator == nil {
		return empty, nil
	}

	if err := s.actuator.EnsureSession(ctx); err != nil {
		return empty, fmt.Errorf("cua-driver sidecar chưa sẵn sàng: %w", err)
	}

	raw, err := s.actuator.Call(ctx, "observe", map[string]interface{}{
		"targetKeywords": cfg.TargetAppKeywords,
	})
	if err != nil {
		return empty, fmt.Errorf("cua-driver observe lỗi: %w", err)
	}
	return actuators.DecodeObservedScreenStateForSensor(raw, now)
}

// Health verifies sidecar responsiveness without perturbing window focus.
func (s *CuaDriverSensor) Health(ctx context.Context, cfg domain.AppConfig) domain.SensorHealth {
	now := time.Now()
	if !cfg.CuaDriver.Enabled {
		return domain.SensorHealth{
			TierName:    s.TierName(),
			Healthy:     true,
			Detail:      "Đang tắt theo cấu hình người dùng",
			LastChecked: now,
		}
	}
	if s.actuator == nil {
		return domain.SensorHealth{
			TierName:    s.TierName(),
			Healthy:     false,
			Detail:      "Chưa gắn kết CuaDriverActuator",
			LastChecked: now,
		}
	}

	hCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	if err := s.actuator.EnsureSession(hCtx); err != nil {
		return domain.SensorHealth{
			TierName:    s.TierName(),
			Healthy:     false,
			Detail:      err.Error(),
			LastChecked: now,
		}
	}
	return domain.SensorHealth{
		TierName:    s.TierName(),
		Healthy:     true,
		Detail:      "CUA sidecar hoạt động ổn định (michael_cua_sidecar JSON-RPC)",
		LastChecked: now,
	}
}

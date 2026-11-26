package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/ports"
)

// SensorFusion aggregates observations across Tier 1 (CLI Config), Tier 2a (CUA Driver Sidecar),
// and Tier 2b (Native AT-SPI2 fallback), prioritizing higher-confidence semantic tiers.
type SensorFusion struct {
	sensors []ports.StateSensor
	nowFunc func() time.Time
}

// NewSensorFusion constructs a tiered sensor fusion coordinator.
func NewSensorFusion(sensors ...ports.StateSensor) *SensorFusion {
	return &SensorFusion{
		sensors: sensors,
		nowFunc: time.Now,
	}
}

// SetClock sets a custom clock for testing.
func (sf *SensorFusion) SetClock(nowFunc func() time.Time) {
	if nowFunc != nil {
		sf.nowFunc = nowFunc
	}
}

// Observe queries enabled tiers and merges their findings into a unified ScreenState.
func (sf *SensorFusion) Observe(ctx context.Context, cfg domain.AppConfig) domain.ScreenState {
	merged := domain.ScreenState{
		ObservedAt:   sf.nowFunc(),
		AppAuthState: domain.AuthUnknown,
		SourceTier:   "none",
		Targets:      make(map[string]domain.UIElementTarget),
	}

	cuaSucceeded := false

	for _, sensor := range sf.sensors {
		if sensor == nil {
			continue
		}
		tier := sensor.TierName()
		if tier == "cua_driver" && !cfg.CuaDriver.Enabled {
			continue
		}
		// Performance Short-Circuit: skip native AT-SPI2 D-Bus tree walk when primary CUA engine succeeded!
		if tier == "atspi2" && cuaSucceeded {
			continue
		}

		obs, err := sensor.Observe(ctx, cfg)
		if err != nil {
			continue
		}
		if tier == "cua_driver" && (obs.WindowDetected || obs.AuthGateDetected) {
			cuaSucceeded = true
		}

		// Always honor immediate safety stop gates (Password / MFA / CAPTCHA) from any tier.
		if obs.AuthGateDetected {
			merged.AuthGateDetected = true
			if merged.AuthGateReason == "" {
				merged.AuthGateReason = obs.AuthGateReason
			}
			merged.AppAuthState = domain.AuthChallenge
			merged.WindowDetected = merged.WindowDetected || obs.WindowDetected
			merged.SourceTier = sensor.TierName()
		}

		if obs.ActiveModel != "" && merged.ActiveModel == "" {
			merged.ActiveModel = obs.ActiveModel
		}
		if obs.ActiveAccountHint != "" && merged.ActiveAccountHint == "" {
			merged.ActiveAccountHint = obs.ActiveAccountHint
		}

		if obs.WindowDetected {
			merged.WindowDetected = true
			if merged.WindowTitle == "" {
				merged.WindowTitle = obs.WindowTitle
			}
			merged.WindowFocused = merged.WindowFocused || obs.WindowFocused
		}

		if obs.AppAuthState != "" && obs.AppAuthState != domain.AuthUnknown && merged.AppAuthState == domain.AuthUnknown {
			merged.AppAuthState = obs.AppAuthState
		}

		for k, v := range obs.Targets {
			if _, exists := merged.Targets[k]; !exists {
				merged.Targets[k] = v
			}
		}

		if obs.IsEditorContent {
			merged.IsEditorContent = true
		}

		// Merge quota exhaustion taking the highest-confidence non-editor detection.
		if obs.QuotaExhausted && !obs.IsEditorContent && (!merged.QuotaExhausted || obs.Confidence >= merged.Confidence) {
			merged.QuotaExhausted = true
			merged.Confidence = obs.Confidence
			merged.QuotaMessage = obs.QuotaMessage
			merged.SourceTier = sensor.TierName()
		} else if !merged.QuotaExhausted && merged.SourceTier == "none" && (obs.WindowDetected || obs.ActiveModel != "") {
			merged.SourceTier = sensor.TierName()
			if obs.Confidence > merged.Confidence {
				merged.Confidence = obs.Confidence
			}
		}
	}

	return merged
}

// HealthCheck collects health snapshots from all registered sensor tiers.
func (sf *SensorFusion) HealthCheck(ctx context.Context, cfg domain.AppConfig) []domain.SensorHealth {
	out := make([]domain.SensorHealth, 0, len(sf.sensors))
	for _, s := range sf.sensors {
		if s == nil {
			continue
		}
		h := s.Health(ctx, cfg)
		if h.TierName == "" {
			h.TierName = strings.TrimSpace(s.TierName())
		}
		out = append(out, h)
	}
	return out
}

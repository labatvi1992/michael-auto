//go:build !linux

package sensors

import (
	"context"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

type ATSPI2Sensor struct{}

func NewATSPI2Sensor() *ATSPI2Sensor {
	return &ATSPI2Sensor{}
}

func (s *ATSPI2Sensor) TierName() string {
	return "atspi2"
}

func (s *ATSPI2Sensor) Observe(ctx context.Context, cfg domain.AppConfig) (domain.ScreenState, error) {
	return domain.ScreenState{
		ObservedAt:   time.Now(),
		AppAuthState: domain.AuthUnknown,
		SourceTier:   s.TierName(),
	}, nil
}

func (s *ATSPI2Sensor) Health(ctx context.Context, cfg domain.AppConfig) domain.SensorHealth {
	return domain.SensorHealth{
		TierName:    s.TierName(),
		Healthy:     false,
		Detail:      "AT-SPI2 chỉ hỗ trợ trên Linux",
		LastChecked: time.Now(),
	}
}

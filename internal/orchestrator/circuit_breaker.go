package orchestrator

import (
	"fmt"
	"sync"
	"time"
)

// SafetyBreaker prevents infinite logout/login loops via a sliding switch budget
// and consecutive step failure tracking (ADR-005).
type SafetyBreaker struct {
	mu                  sync.Mutex
	nowFunc             func() time.Time
	switchTimestamps    []time.Time
	consecutiveFailures int
	maxConsecutiveFails int
	tripped             bool
	tripReason          string
}

// NewSafetyBreaker creates a circuit breaker with a default consecutive failure cap of 3.
func NewSafetyBreaker(maxConsecutiveFails int) *SafetyBreaker {
	if maxConsecutiveFails < 1 {
		maxConsecutiveFails = 3
	}
	return &SafetyBreaker{
		nowFunc:             time.Now,
		maxConsecutiveFails: maxConsecutiveFails,
	}
}

// SetClock sets a custom clock function for unit testing.
func (sb *SafetyBreaker) SetClock(nowFunc func() time.Time) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if nowFunc != nil {
		sb.nowFunc = nowFunc
	}
}

func (sb *SafetyBreaker) pruneLocked(now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
	valid := sb.switchTimestamps[:0]
	for _, ts := range sb.switchTimestamps {
		if !ts.Before(cutoff) {
			valid = append(valid, ts)
		}
	}
	sb.switchTimestamps = valid
}

// CanExecute checks if the circuit breaker allows starting an automated transition.
func (sb *SafetyBreaker) CanExecute(maxSwitches int, window time.Duration) (bool, string, int) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	now := sb.nowFunc()
	sb.pruneLocked(now, window)
	used := len(sb.switchTimestamps)

	if sb.tripped {
		return false, sb.tripReason, used
	}
	if used >= maxSwitches {
		sb.tripped = true
		sb.tripReason = fmt.Sprintf("Vượt ngân sách chuyển đổi tự động (%d/%d lần trong %v)", used, maxSwitches, window)
		return false, sb.tripReason, used
	}
	return true, "", used
}

// RecordSwitch logs a completed switch against the sliding rate-limit budget.
func (sb *SafetyBreaker) RecordSwitch(window time.Duration) int {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	now := sb.nowFunc()
	sb.pruneLocked(now, window)
	sb.switchTimestamps = append(sb.switchTimestamps, now)
	sb.consecutiveFailures = 0
	return len(sb.switchTimestamps)
}

// RecordFailure increments consecutive failure count and trips if threshold is reached.
func (sb *SafetyBreaker) RecordFailure(errDetail string) (tripped bool, reason string) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.consecutiveFailures++
	if sb.consecutiveFailures >= sb.maxConsecutiveFails {
		sb.tripped = true
		sb.tripReason = fmt.Sprintf("Thất bại thực thi %d lần liên tiếp: %s", sb.consecutiveFailures, errDetail)
	}
	return sb.tripped, sb.tripReason
}

// RecordSuccess clears consecutive failures without consuming a profile-switch slot.
func (sb *SafetyBreaker) RecordSuccess() {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.consecutiveFailures = 0
}

// Trip forces the breaker open with an explicit safety reason.
func (sb *SafetyBreaker) Trip(reason string) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.tripped = true
	sb.tripReason = reason
}

// Reset clears the tripped state and failure counters when requested by the user.
func (sb *SafetyBreaker) Reset() {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.tripped = false
	sb.tripReason = ""
	sb.consecutiveFailures = 0
	sb.switchTimestamps = nil
}

// Status returns current breaker state and sliding switch count.
func (sb *SafetyBreaker) Status(window time.Duration) (tripped bool, reason string, used int) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.pruneLocked(sb.nowFunc(), window)
	return sb.tripped, sb.tripReason, len(sb.switchTimestamps)
}

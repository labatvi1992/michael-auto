package orchestrator

import (
	"sync"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

// MultiFrameConsensus enforces ADR-001 anti-false-positive rules:
// 1. Editor/document text observations never count toward quota exhaustion.
// 2. Quota exhaustion must appear with Confidence >= minConfidence across >= requiredFrames consecutive checks.
// 3. Auth challenge gates (Password / MFA / CAPTCHA) bypass frame delay for immediate safety stop.
type MultiFrameConsensus struct {
	mu               sync.Mutex
	consecutiveCount int
}

// NewMultiFrameConsensus constructs a consensus filter.
func NewMultiFrameConsensus() *MultiFrameConsensus {
	return &MultiFrameConsensus{}
}

// Evaluate updates the consecutive frame counter and returns whether quota exhaustion is confirmed
// along with the current consecutive frame count.
func (c *MultiFrameConsensus) Evaluate(obs domain.ScreenState, minConfidence float64, requiredFrames int) (confirmed bool, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if requiredFrames < 1 {
		requiredFrames = 1
	}

	// Never trigger on text inside code editors or markdown documents.
	if obs.IsEditorContent {
		c.consecutiveCount = 0
		return false, 0
	}

	if !obs.WindowDetected || !obs.QuotaExhausted || obs.Confidence < minConfidence {
		c.consecutiveCount = 0
		return false, 0
	}

	c.consecutiveCount++
	return c.consecutiveCount >= requiredFrames, c.consecutiveCount
}

// Reset clears accumulated frame counts (e.g. after switching profiles or pausing).
func (c *MultiFrameConsensus) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutiveCount = 0
}

// Count returns the current consecutive frame count.
func (c *MultiFrameConsensus) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consecutiveCount
}

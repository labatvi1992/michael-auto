package domain

import (
	"testing"
	"time"
)

func TestDefaultConfigValidationAndBounds(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected DefaultConfig to validate cleanly, got: %v", err)
	}

	badYield := cfg
	badYield.YieldPauseSeconds = 2
	if err := badYield.Validate(); err == nil {
		t.Fatal("expected error when yieldPauseSeconds < 5")
	}

	badConfidence := cfg
	badConfidence.MinConfidence = 0.2
	if err := badConfidence.Validate(); err == nil {
		t.Fatal("expected error when minConfidence < 0.50")
	}

	badTraversal := DefaultConfig()
	badTraversal.Profiles[0].BrowserProfileDir = "../etc/passwd"
	if err := badTraversal.Validate(); err == nil {
		t.Fatal("expected error on path traversal in browserProfileDir")
	}

	badShell := DefaultConfig()
	badShell.Profiles[0].BrowserProfileDir = "Default; rm -rf /"
	if err := badShell.Validate(); err == nil {
		t.Fatal("expected error on shell metacharacters in browserProfileDir")
	}

	dupID := DefaultConfig()
	dupID.Profiles[1].ID = dupID.Profiles[0].ID
	if err := dupID.Validate(); err == nil {
		t.Fatal("expected error on duplicate profile IDs")
	}
}

func TestProfileFallbackAndExhaustion(t *testing.T) {
	now := time.Date(2026, 11, 24, 21, 0, 0, 0, time.UTC)
	prof := AccountProfile{
		ID:             "p1",
		Name:           "Work",
		BrowserType:    BrowserChrome,
		BrowserProfileDir: "Default",
		PrimaryModel:   "gemini-2.5-pro",
		FallbackModels: []string{"gemini-2.5-flash", "gemini-2.5-flash-lite"},
		Enabled:        true,
	}

	next, ok := prof.NextAvailableFallback("gemini-2.5-pro", now)
	if !ok || next != "gemini-2.5-flash" {
		t.Fatalf("expected gemini-2.5-flash, got %q (%v)", next, ok)
	}

	prof.ExhaustedModels = []string{"gemini-2.5-pro", "gemini-2.5-flash"}
	next, ok = prof.NextAvailableFallback("gemini-2.5-flash", now)
	if !ok || next != "gemini-2.5-flash-lite" {
		t.Fatalf("expected gemini-2.5-flash-lite, got %q (%v)", next, ok)
	}

	until := now.Add(30 * time.Minute)
	prof.ExhaustedUntil = &until
	if !prof.IsExhausted(now) {
		t.Fatal("expected profile to report exhausted before ExhaustedUntil")
	}
	if _, ok := prof.NextAvailableFallback("gemini-2.5-pro", now); ok {
		t.Fatal("expected no fallback available when entire profile is exhausted")
	}
}

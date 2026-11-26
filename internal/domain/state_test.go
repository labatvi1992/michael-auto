package domain

import (
	"testing"
	"time"
)

func TestFindProfileAndIntentExpiry(t *testing.T) {
	cfg := DefaultConfig()
	if p := cfg.FindProfile("profile-primary"); p == nil || p.Name == "" {
		t.Fatal("expected FindProfile to locate profile-primary")
	}
	if p := cfg.FindProfile("non-existent"); p != nil {
		t.Fatal("expected nil for unknown profile ID")
	}

	var nilIntent *PendingIntent
	now := time.Now()
	if !nilIntent.IsExpired(now) {
		t.Fatal("nil PendingIntent must report expired")
	}

	activeIntent := &PendingIntent{
		Kind:      IntentSwitchModel,
		CreatedAt: now,
		ExpiresAt: now.Add(5 * time.Minute),
	}
	if activeIntent.IsExpired(now) {
		t.Fatal("expected non-expired intent")
	}
	if !activeIntent.IsExpired(now.Add(6 * time.Minute)) {
		t.Fatal("expected intent expired after TTL")
	}
}

func TestValidateAdditionalBranches(t *testing.T) {
	c1 := DefaultConfig()
	c1.CooldownSeconds = 0
	if err := c1.Validate(); err == nil {
		t.Fatal("expected error on CooldownSeconds=0")
	}

	c2 := DefaultConfig()
	c2.PollIntervalMs = 100
	if err := c2.Validate(); err == nil {
		t.Fatal("expected error on PollIntervalMs<250")
	}

	c3 := DefaultConfig()
	c3.ConsensusFrames = 0
	if err := c3.Validate(); err == nil {
		t.Fatal("expected error on ConsensusFrames=0")
	}

	c4 := DefaultConfig()
	c4.Profiles = nil
	if err := c4.Validate(); err == nil {
		t.Fatal("expected error when Profiles is empty")
	}

	c5 := DefaultConfig()
	c5.Profiles[0].BrowserType = BrowserCustom
	c5.Profiles[0].BrowserBinary = ""
	if err := c5.Validate(); err == nil {
		t.Fatal("expected error when custom browser has empty BrowserBinary")
	}
}

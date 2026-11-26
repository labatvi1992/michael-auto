package sensors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labatvi1992/michael-auto/internal/adapters/actuators"
	"github.com/labatvi1992/michael-auto/internal/adapters/storage"
	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/ports"
)

func loadSnapshotNodes(t *testing.T, filename string) []AccessibleNode {
	t.Helper()
	path := filepath.Join("..", "..", "..", "testdata", "snapshots", filename)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed reading snapshot %s: %v", path, err)
	}
	var nodes []AccessibleNode
	if err := json.Unmarshal(raw, &nodes); err != nil {
		t.Fatalf("failed unmarshaling snapshot %s: %v", path, err)
	}
	return nodes
}

func TestATSPIReplaySnapshots(t *testing.T) {
	cfg := domain.DefaultConfig()

	// 1. Genuine quota alert banner in Antigravity window
	bannerTree := loadSnapshotNodes(t, "atspi_quota_banner.json")
	st1 := AnalyzeAccessibleTree(bannerTree, cfg.TargetAppKeywords, cfg.Profiles)
	if !st1.WindowDetected || !st1.QuotaExhausted || st1.Confidence < 0.95 {
		t.Fatalf("expected quota banner detection >= 0.95, got %+v", st1)
	}
	if st1.ActiveAccountHint != "primary@example.com" {
		t.Fatalf("expected account hint primary@example.com, got %q", st1.ActiveAccountHint)
	}
	if _, ok := st1.Targets["logout_button"]; !ok {
		t.Fatal("expected semantic logout_button target in parsed map")
	}

	// 2. Adversarial false-positive test: README.md open inside code/document editor
	editorTree := loadSnapshotNodes(t, "atspi_false_positive_editor.json")
	st2 := AnalyzeAccessibleTree(editorTree, cfg.TargetAppKeywords, cfg.Profiles)
	if st2.QuotaExhausted {
		t.Fatalf("CRITICAL: editor document text triggered false-positive quota exhaustion: %+v", st2)
	}
	if !st2.IsEditorContent {
		t.Fatal("expected IsEditorContent=true when quota keywords appear inside editable document")
	}

	// 3. MFA / CAPTCHA challenge window focused
	mfaTree := loadSnapshotNodes(t, "atspi_mfa_captcha_challenge.json")
	st3 := AnalyzeAccessibleTree(mfaTree, cfg.TargetAppKeywords, cfg.Profiles)
	if !st3.AuthGateDetected || st3.AppAuthState != domain.AuthChallenge {
		t.Fatalf("expected immediate AuthGateDetected on MFA/Turnstile dialog, got %+v", st3)
	}

	// 4. Antigravity 2.34+ live Electron DOM: root `document web` + Markdown chat table/list items discussing
	//    2FA/CAPTCHA/Quota/Login must NEVER false-trigger AuthGate or QuotaExhausted or LoggedOut (P0-3)!
	ag234Tree := []AccessibleNode{
		{
			Role: "frame",
			Name: "Antigravity — Workspace",
			Children: []AccessibleNode{
				{
					Role:     "document web",
					Name:     "Antigravity Root",
					Editable: false,
					Children: []AccessibleNode{
						{Role: "table cell", Name: "Dừng cứng khi gặp 2-Step Verification / CAPTCHA / nhập mật khẩu"},
						{Role: "list item", Name: "• Khi đăng nhập bằng role admin, hết hạn mức rate-limit", Y: -2500, Height: 30},
						{Role: "button", Name: "Settings", X: 8, Y: 1008, Width: 240, Height: 32},
						{Role: "button", Name: "Select model, current: Gemini 4 Neon Extra High", X: 735, Y: 989, Width: 174, Height: 28},
					},
				},
			},
		},
	}
	st4 := AnalyzeAccessibleTree(ag234Tree, cfg.TargetAppKeywords, cfg.Profiles)
	if st4.AuthGateDetected || st4.QuotaExhausted {
		t.Fatalf("Antigravity 2.34 chat/bullet content falsely triggered AuthGate/Quota: %+v", st4)
	}
	if st4.AppAuthState != domain.AuthAuthenticated || st4.ActiveModel != "Gemini 4 Neon Extra High" {
		t.Fatalf("expected authenticated + clean model 'Gemini 4 Neon Extra High', got auth=%s model=%q", st4.AppAuthState, st4.ActiveModel)
	}
	if _, ok := st4.Targets["sign_in_button"]; ok {
		t.Fatalf("chat bullet list item falsely matched sign_in_button: %+v", st4.Targets)
	}
}

type leaseCountingMonitor struct {
	leases atomic.Int64
}

func (m *leaseCountingMonitor) Subscribe(ctx context.Context) (<-chan domain.ActivityKind, error) {
	return make(chan domain.ActivityKind), nil
}
func (m *leaseCountingMonitor) SystemIdleDuration(ctx context.Context) (time.Duration, error) {
	return time.Minute, nil
}
func (m *leaseCountingMonitor) AcquireActuationLease(ttl time.Duration, x, y *int) func() {
	m.leases.Add(1)
	return func() {}
}
func (m *leaseCountingMonitor) SetActiveExecution(active bool) {}
func (m *leaseCountingMonitor) BackendName() string            { return "lease-counter" }
func (m *leaseCountingMonitor) Close() error                   { return nil }

func TestCuaSidecarLiveStdioMultiplexerEvidenceVaultAndSecurityMapping(t *testing.T) {
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed in test environment")
	}

	tmpDir := t.TempDir()
	mockPNG := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-mock-sidecar-evidence-png"))
	mockScript := filepath.Join(tmpDir, "mock_sidecar.py")
	scriptContent := `#!/usr/bin/env python3
import json, sys

MOCK_PNG = "` + mockPNG + `"

for line in sys.stdin:
    req = json.loads(line)
    rid = req["id"]
    m = req["method"]
    params = req.get("params") or {}
    if m == "health_check":
        res = {"healthy": True}
    elif m == "observe":
        res = {
            "windowDetected": True,
            "windowTitle": "Antigravity",
            "windowFocused": True,
            "windowPid": 1234,
            "appAuthState": "authenticated",
            "activeAccountHint": "primary@example.com",
            "activeModel": "gemini-2.5-pro",
            "quotaExhausted": False,
            "confidence": 0.96,
        }
    elif m == "inspect_quota":
        res = {
            "windowDetected": True,
            "windowTitle": "Antigravity",
            "windowFocused": True,
            "windowPid": 1234,
            "appAuthState": "authenticated",
            "activeAccountHint": "primary@example.com",
            "activeModel": "gemini-2.5-pro",
            "remainingQuotaPct": 0,
            "quotaExhausted": True,
            "quotaMessage": "0% remaining — Quota exhausted",
            "confidence": 0.99,
            "evidence": {
                "step": "01_verify_quota_zero",
                "windowTitle": "Antigravity",
                "pid": 1234,
                "verifiedDetail": "Verified 0% & Quota exhausted before SignOut",
                "screenshotBase64": MOCK_PNG,
            }
        }
    elif m == "select_chrome_account" and params.get("emailHint") == "mfa@example.com":
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": rid, "error": {"code": -32004, "message": "2-Step Verification on Chrome"}}) + "\n")
        sys.stdout.flush()
        continue
    elif m == "select_chrome_account" and params.get("emailHint") == "untrusted@example.com":
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": rid, "error": {"code": -32006, "message": "Omnibox origin mismatch"}}) + "\n")
        sys.stdout.flush()
        continue
    elif m == "shutdown":
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": rid, "result": {"shutdown": True}}) + "\n")
        sys.stdout.flush()
        sys.exit(0)
    else:
        res = {
            "ok": True,
            "method": m,
            "evidence": {
                "step": "02_pre_" + m,
                "windowTitle": "Antigravity",
                "pid": 1234,
                "verifiedDetail": "Pre-actuation verified",
                "screenshotBase64": MOCK_PNG,
            }
        }
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": rid, "result": res}) + "\n")
    sys.stdout.flush()
`
	if err := os.WriteFile(mockScript, []byte(scriptContent), 0o700); err != nil {
		t.Fatal(err)
	}

	mon := &leaseCountingMonitor{}
	vault := storage.NewEvidenceVault(filepath.Join(tmpDir, "evidence"), 10)
	cua := actuators.NewCustomCuaDriverActuator(pyBin, mockScript, nil, mon, nil)
	cua.SetEvidenceStore(vault)
	cua.SetActiveRunID("run_live_test_01")
	defer cua.Close()

	sensor := NewCuaDriverSensor(cua)
	cfg := domain.DefaultConfig()
	ctx := context.Background()

	if h := sensor.Health(ctx, cfg); !h.Healthy {
		t.Fatalf("expected healthy CUA sidecar, got %+v", h)
	}

	obs, err := sensor.Observe(ctx, cfg)
	if err != nil || !obs.WindowDetected || obs.ActiveAccountHint != "primary@example.com" {
		t.Fatalf("unexpected CUA passive observation: %+v (err=%v)", obs, err)
	}

	qObs, err := cua.InspectQuota(ctx, cfg.TargetAppKeywords, false)
	if err != nil || !qObs.QuotaExhausted || qObs.Confidence < 0.95 || qObs.EvidenceSHA256 == "" {
		t.Fatalf("expected InspectQuota exhaustion >= 0.95 with persisted Evidence SHA-256, got %+v (err=%v)", qObs, err)
	}

	if err := cua.TriggerLogout(ctx, qObs, false); err != nil {
		t.Fatalf("TriggerLogout failed: %v", err)
	}
	if err := cua.ClickSignIn(ctx, false); err != nil {
		t.Fatalf("ClickSignIn failed: %v", err)
	}
	if err := cua.SelectChromeAccount(ctx, cfg.Profiles[1], false); err != nil {
		t.Fatalf("SelectChromeAccount failed: %v", err)
	}
	if err := cua.SelectModel(ctx, qObs, "Claude Opus 4.6", false); err != nil {
		t.Fatalf("SelectModel failed: %v", err)
	}

	savedEv, err := vault.ListRecent(10)
	if err != nil || len(savedEv) < 5 {
		t.Fatalf("expected >= 5 pre-actuation Evidence screenshots persisted in vault, got %d (err=%v)", len(savedEv), err)
	}

	// Verify ActuationLease was acquired on every UI interaction (5 calls above).
	if gotLeases := mon.leases.Load(); gotLeases < 5 {
		t.Fatalf("expected >= 5 ActuationLeases preventing Wayland self-interrupt, got %d", gotLeases)
	}

	// Verify Chrome MFA/CAPTCHA error (-32004) unwraps cleanly to ports.ErrAuthGateDetected!
	mfaProf := domain.AccountProfile{ID: "mfa", EmailHint: "mfa@example.com"}
	if err := cua.SelectChromeAccount(ctx, mfaProf, false); !errors.Is(err, ports.ErrAuthGateDetected) {
		t.Fatalf("expected ports.ErrAuthGateDetected on -32004 RPC error, got %v", err)
	}

	// Verify Omnibox/PID security rejection (-32006) unwraps cleanly to ports.ErrSecurityViolation!
	secProf := domain.AccountProfile{ID: "sec", EmailHint: "untrusted@example.com"}
	if err := cua.SelectChromeAccount(ctx, secProf, false); !errors.Is(err, ports.ErrSecurityViolation) {
		t.Fatalf("expected ports.ErrSecurityViolation on -32006 RPC error, got %v", err)
	}
}

func TestCLIConfigAtomicReadWritePreservesExistingFields(t *testing.T) {
	tmp := t.TempDir()
	settingsFile := filepath.Join(tmp, "settings.json")
	initial := `{"theme":"dark","selectedModel":"gemini-2.5-pro","sandbox":true}`
	if err := os.WriteFile(settingsFile, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := NewCLIConfigAdapter()
	gotModel, err := adapter.ReadActiveModel(settingsFile)
	if err != nil || gotModel != "gemini-2.5-pro" {
		t.Fatalf("expected gemini-2.5-pro, got %q (%v)", gotModel, err)
	}

	if err := adapter.WriteActiveModel(settingsFile, "gemini-2.5-flash", false); err != nil {
		t.Fatalf("WriteActiveModel failed: %v", err)
	}

	updatedModel, err := adapter.ReadActiveModel(settingsFile)
	if err != nil || updatedModel != "gemini-2.5-flash" {
		t.Fatalf("expected gemini-2.5-flash after atomic write, got %q (%v)", updatedModel, err)
	}

	rawBytes, _ := os.ReadFile(settingsFile)
	if !strings.Contains(string(rawBytes), `"theme": "dark"`) || !strings.Contains(string(rawBytes), `"sandbox": true`) {
		t.Fatalf("expected existing settings keys preserved, got: %s", string(rawBytes))
	}
	if _, err := os.Stat(settingsFile + ".bak"); err != nil {
		t.Fatalf("expected atomic backup file .bak to exist: %v", err)
	}
}

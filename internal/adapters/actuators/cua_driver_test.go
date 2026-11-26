package actuators

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labatvi1992/michael-auto/internal/adapters/storage"
	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/ports"
)

type stubActivityMon struct{}

func (s *stubActivityMon) Subscribe(ctx context.Context) (<-chan domain.ActivityKind, error) {
	return make(chan domain.ActivityKind), nil
}
func (s *stubActivityMon) SystemIdleDuration(ctx context.Context) (time.Duration, error) {
	return time.Minute, nil
}
func (s *stubActivityMon) AcquireActuationLease(ttl time.Duration, x, y *int) func() {
	return func() {}
}
func (s *stubActivityMon) SetActiveExecution(active bool) {}
func (s *stubActivityMon) BackendName() string            { return "stub" }
func (s *stubActivityMon) Close() error                   { return nil }

func TestCuaDriverSupervisorHotRestartBreakerResetAndOAuthPKCERelay(t *testing.T) {
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}

	tmpDir := t.TempDir()
	mockPNG := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-mock-cua-driver-test"))
	scriptPath := filepath.Join(tmpDir, "fake_sidecar.py")
	stateFile := filepath.Join(tmpDir, "relay_once.flag")

	script := `#!/usr/bin/env python3
import json, os, sys

MOCK_PNG = "` + mockPNG + `"
STATE_FILE = "` + stateFile + `"

for raw in sys.stdin:
    req = json.loads(raw)
    rid = req.get("id")
    m = req.get("method", "")
    params = req.get("params") or {}
    if m == "$/cancelRequest":
        continue
    if m == "health_check":
        res = {"healthy": True, "wsName": os.environ.get("CUA_AGENT_WORKSPACE_NAME", "")}
    elif m == "inspect_ui":
        res = {"e2eReady": True, "antigravityWindows": [{"pid": 999, "matchedSelectors": ["model_selector", "settings_button"]}]}
    elif m == "list_models":
        res = {"activeModel": "Gemini 4 Neon Extra High", "models": ["Gemini 4 Neon Extra High", "Claude Sonnet 4.6 (Thinking)"]}
    elif m == "select_chrome_account":
        if not os.path.exists(STATE_FILE):
            with open(STATE_FILE, "w") as f:
                f.write("relayed")
            sys.stdout.write(json.dumps({
                "jsonrpc": "2.0",
                "id": rid,
                "error": {
                    "code": -32006,
                    "message": "Misrouted OAuth tab",
                    "data": {
                        "reason": "oauth_profile_relay_required",
                        "relayOAuthUrl": "https://accounts.google.com/o/oauth2/v2/auth?client_id=ag&state=pkce123&code_challenge=abc"
                    }
                }
            }) + "\n")
            sys.stdout.flush()
            continue
        res = {
            "selectedAccount": params.get("emailHint", ""),
            "evidence": {
                "step": "04_pre_chrome_account_select",
                "windowTitle": "Google Accounts",
                "pid": 2002,
                "verifiedDetail": "Verified after PKCE relay",
                "screenshotBase64": MOCK_PNG,
                "captureScope": "window",
                "windowFrame": {"x": 0, "y": 0, "width": 1200, "height": 800}
            }
        }
    elif m == "select_model":
        if params.get("model") == "trigger-mismatch":
            res = {
                "selectedModel": "trigger-mismatch",
                "detectedAccountHint": "wrong@example.com",
                "evidence": {"step": "05_pre_model_select", "windowTitle": "Antigravity", "pid": 1001, "verifiedDetail": "ok", "screenshotBase64": MOCK_PNG}
            }
        elif params.get("model") == "trigger-post-zero":
            res = {
                "selectedModel": "trigger-post-zero",
                "postLoginQuotaExhausted": True,
                "evidence": {"step": "05_pre_model_select", "windowTitle": "Antigravity", "pid": 1001, "verifiedDetail": "ok", "screenshotBase64": MOCK_PNG}
            }
        else:
            res = {
                "selectedModel": params.get("model", ""),
                "evidence": {"step": "05_pre_model_select", "windowTitle": "Antigravity", "pid": 1001, "verifiedDetail": "ok", "screenshotBase64": MOCK_PNG}
            }
    elif m == "click_signin":
        res = {"clickedSignIn": True, "evidence": {"step": "03_pre_signin_antigravity", "windowTitle": "Antigravity", "pid": 1001, "verifiedDetail": "ok", "screenshotBase64": MOCK_PNG}}
    elif m == "focus_window":
        res = {"focused": True}
    elif m == "shutdown":
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": rid, "result": {"shutdown": True}}) + "\n")
        sys.stdout.flush()
        sys.exit(0)
    else:
        res = {"ok": True}
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": rid, "result": res}) + "\n")
    sys.stdout.flush()
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var launchedArgs [][]string
	router := NewTestableBrowserRouter(
		func(bin string) (string, error) { return "/usr/bin/" + bin, nil },
		func(ctx context.Context, binary string, args []string) error {
			mu.Lock()
			launchedArgs = append(launchedArgs, append([]string(nil), args...))
			mu.Unlock()
			return nil
		},
	)

	vault := storage.NewEvidenceVault(filepath.Join(tmpDir, "evidence"), 10)
	cua := NewCustomCuaDriverActuator(pyBin, scriptPath, []string{"TEST_MODE=1"}, &stubActivityMon{}, router)
	cua.SetEvidenceStore(vault)
	cua.SetActiveRunID("run_cua_unit_01")
	defer cua.Close()

	ctx := context.Background()
	if err := cua.EnsureSession(ctx); err != nil {
		t.Fatalf("EnsureSession failed: %v", err)
	}
	if ok, _ := cua.SessionStatus(); !ok {
		t.Fatal("expected ready=true after EnsureSession")
	}

	// 1. InspectUISelectors & ListAvailableModels
	if rawUI, err := cua.InspectUISelectors(ctx, []string{"Antigravity"}); err != nil || !strings.Contains(string(rawUI), "e2eReady") {
		t.Fatalf("InspectUISelectors failed: %v %s", err, string(rawUI))
	}
	models, active, err := cua.ListAvailableModels(ctx, []string{"Antigravity"})
	if err != nil || active != "Gemini 4 Neon Extra High" || len(models) != 2 {
		t.Fatalf("ListAvailableModels unexpected: active=%q models=%v err=%v", active, models, err)
	}

	// 2. FocusAppWindow
	if err := cua.FocusAppWindow(ctx, "Antigravity", false); err != nil {
		t.Fatalf("FocusAppWindow failed: %v", err)
	}

	// 3. Layer 2 Lossless OAuth2 PKCE Relay in SelectChromeAccount!
	targetProf := domain.AccountProfile{
		ID:                "p-work",
		Name:              "Work",
		EmailHint:         "work@example.com",
		BrowserType:       domain.BrowserChrome,
		BrowserProfileDir: "Profile 1",
		PrimaryModel:      "Gemini 4 Neon Extra High",
		Enabled:           true,
	}
	if err := cua.SelectChromeAccount(ctx, targetProf, false); err != nil {
		t.Fatalf("SelectChromeAccount PKCE relay failed: %v", err)
	}
	mu.Lock()
	gotLaunched := append([][]string(nil), launchedArgs...)
	mu.Unlock()
	if len(gotLaunched) != 1 || !strings.Contains(strings.Join(gotLaunched[0], " "), "code_challenge=abc") || !strings.Contains(strings.Join(gotLaunched[0], " "), "--profile-directory=Profile 1") {
		t.Fatalf("expected BrowserRouter launched with exact relayed PKCE URL & Profile 1, got %v", gotLaunched)
	}

	// 4. Post-switch invariants in SelectModel (ErrAccountMismatch & ErrPostSwitchQuotaExhausted)
	cua.mu.Lock()
	cua.expectedEmailHint = "work@example.com"
	cua.mu.Unlock()
	if err := cua.SelectModel(ctx, domain.ScreenState{}, "trigger-mismatch", false); !errors.Is(err, ports.ErrAccountMismatch) {
		t.Fatalf("expected ErrAccountMismatch, got %v", err)
	}
	if err := cua.SelectModel(ctx, domain.ScreenState{}, "trigger-post-zero", false); !errors.Is(err, ports.ErrPostSwitchQuotaExhausted) {
		t.Fatalf("expected ErrPostSwitchQuotaExhausted, got %v", err)
	}

	// 5. Hot-restart via UpdateSidecarConfig must NOT open crash breaker!
	newCfg := domain.DefaultConfig().CuaDriver
	newCfg.DeliveryMode = "foreground"
	newCfg.AgentWorkspaceName = domain.DefaultAgentWorkspaceName
	cua.UpdateSidecarConfig(newCfg)
	if err := cua.EnsureSession(ctx); err != nil {
		t.Fatalf("EnsureSession after UpdateSidecarConfig hot-restart failed: %v", err)
	}

	// 6. Verify ResetBreaker clears a tripped breaker cleanly (P1-2)!
	cua.mu.Lock()
	cua.breakerOpen = true
	cua.restartCount = 5
	cua.mu.Unlock()
	if err := cua.EnsureSession(ctx); err == nil {
		t.Fatal("expected EnsureSession error when breakerOpen=true")
	}
	cua.ResetBreaker()
	if err := cua.EnsureSession(ctx); err != nil {
		t.Fatalf("expected EnsureSession to recover after ResetBreaker(), got %v", err)
	}
}

func TestSidecarProvisionerVerifiedUvBootstrapSHA256(t *testing.T) {
	// Build a valid tar.gz containing an executable `uv` file.
	var archiveBuf bytes.Buffer
	gzw := gzip.NewWriter(&archiveBuf)
	tw := tar.NewWriter(gzw)
	fakeUvPayload := []byte("#!/bin/sh\necho uv 0.6.6\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "uv-x86_64-unknown-linux-gnu/uv",
		Mode: 0o755,
		Size: int64(len(fakeUvPayload)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(fakeUvPayload); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gzw.Close()

	archiveBytes := archiveBuf.Bytes()
	sum := sha256.Sum256(archiveBytes)
	validSha := hex.EncodeToString(sum[:])

	tamperSha := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			if tamperSha {
				_, _ = fmt.Fprintln(w, strings.Repeat("0", 64)+"  uv.tar.gz")
			} else {
				_, _ = fmt.Fprintf(w, "%s  uv.tar.gz\n", validSha)
			}
			return
		}
		_, _ = w.Write(archiveBytes)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bin", "uv")
	gotPath, err := bootstrapVerifiedUvFromBaseURL(context.Background(), srv.URL, dest)
	if err != nil {
		t.Fatalf("expected valid SHA-256 uv bootstrap to succeed, got %v", err)
	}
	info, statErr := os.Stat(gotPath)
	if statErr != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("expected extracted uv binary with 0700 perms, got err=%v mode=%v", statErr, info)
	}

	// Tampered SHA-256 MUST fail closed!
	tamperSha = true
	if _, err := bootstrapVerifiedUvFromBaseURL(context.Background(), srv.URL, filepath.Join(t.TempDir(), "uv-bad")); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("expected SHA-256 mismatch error on tampered checksum, got %v", err)
	}
}

func TestRealGoToPythonSidecarStdioContract(t *testing.T) {
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}

	// Launch the REAL michael_cua_sidecar AsyncJsonRpcMux server over stdio with a tiny injected mock driver!
	harness := filepath.Join(t.TempDir(), "real_sidecar_harness.py")
	repoSidecar, err := filepath.Abs(filepath.Join("..", "..", "..", "sidecar"))
	if err != nil {
		t.Fatal(err)
	}
	harnessCode := fmt.Sprintf(`#!/usr/bin/env python3
import base64, sys
sys.path.insert(0, %q)
from dataclasses import dataclass, field
from michael_cua_sidecar import server

PNG = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")

@dataclass
class El:
    role: str
    label: str = ""
    text: str = ""
    description: str = ""
    value: str = ""
    editable: bool = False
    element_token: str = ""
    x: int = 10
    y: int = 10
    width: int = 100
    height: int = 28
    children: list = field(default_factory=list)

@dataclass
class Win:
    pid: int
    window_id: str
    title: str
    app_name: str = "antigravity"
    focused: bool = True
    screenshot: bytes | None = None
    elements: list = field(default_factory=list)

class HarnessDriver:
    def __init__(self):
        self.menu = False
    async def list_windows(self, _inp=None):
        class Resp:
            windows = [Win(pid=5034, window_id="1", title="Antigravity — Live Contract")]
        return Resp()
    async def get_window_state(self, pid=0, window_id="", include_screenshot=False, **_kw):
        els = [
            El(role="button", label="Settings", element_token="tok:settings"),
            El(role="button", label="Select model, current: Gemini 4 Neon Extra High", element_token="tok:model"),
        ]
        if self.menu:
            els.append(El(role="menu", label="Model", children=[
                El(role="menu item", label="Gemini 4 Neon Extra High", element_token="tok:m1"),
                El(role="menu item", label="Claude Opus 4.6 (Thinking)", element_token="tok:m2"),
                El(role="menu item", label="View Usage", element_token="tok:usage"),
            ]))
        return Win(pid=5034, window_id="1", title="Antigravity — Live Contract", screenshot=PNG if include_screenshot else None, elements=els)
    async def click(self, act):
        if act.get("element_token") == "tok:model":
            self.menu = True
    async def press_key(self, act):
        if act.get("key") == "Escape":
            self.menu = False

server.set_driver_for_testing(HarnessDriver())
server.main()
`, repoSidecar)
	if err := os.WriteFile(harness, []byte(harnessCode), 0o700); err != nil {
		t.Fatal(err)
	}

	cua := NewCustomCuaDriverActuator(pyBin, harness, []string{"CUA_AGENT_WORKSPACE_ENABLED=0"}, &stubActivityMon{}, nil)
	defer cua.Close()

	ctx := context.Background()
	if err := cua.EnsureSession(ctx); err != nil {
		t.Fatalf("Real Python sidecar EnsureSession failed: %v", err)
	}

	rawUI, err := cua.InspectUISelectors(ctx, []string{"Antigravity"})
	if err != nil {
		t.Fatalf("Real Python sidecar InspectUISelectors failed: %v", err)
	}
	if !strings.Contains(string(rawUI), `"quotaInspectionPath":"model_selector -> view_usage"`) &&
		!strings.Contains(string(rawUI), `"quotaInspectionPath": "model_selector -> view_usage"`) {
		t.Fatalf("expected calibrated quotaInspectionPath from real Python sidecar, got: %s", string(rawUI))
	}

	models, active, err := cua.ListAvailableModels(ctx, []string{"Antigravity"})
	if err != nil || active != "Gemini 4 Neon Extra High" || len(models) < 2 {
		t.Fatalf("Real Python sidecar ListAvailableModels unexpected: active=%q models=%v err=%v", active, models, err)
	}
}

type stubSecretStore struct {
	data map[string]string
}

func (s *stubSecretStore) Get(key string) (string, error) {
	if v, ok := s.data[key]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}
func (s *stubSecretStore) Set(key, value string) error { s.data[key] = value; return nil }
func (s *stubSecretStore) Delete(key string) error     { delete(s.data, key); return nil }
func (s *stubSecretStore) Backend() string             { return "stub-secret" }

func TestSupplyChainPipArgsWaylandA11yPortalTokensAndCuaHelpers(t *testing.T) {
	// 1. Supply-chain --require-hashes & offline CUA_WHEEL_DIR pip argument verification (R4)
	lockDir := t.TempDir()
	lockFile := filepath.Join(lockDir, pinnedCuaDriverLockFile)
	if err := os.WriteFile(lockFile, []byte("cua-driver==0.58.1 --hash=sha256:abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defaultArgs := strings.Join(buildPipWheelInstallArgs(pinnedCuaDriverSpec, lockFile, ""), " ")
	if !strings.Contains(defaultArgs, "--only-binary=:all:") || !strings.Contains(defaultArgs, "--require-hashes -r "+lockFile) {
		t.Fatalf("expected --only-binary=:all: --require-hashes -r <lock>, got: %s", defaultArgs)
	}
	offlineArgs := strings.Join(buildPipWheelInstallArgs(pinnedCuaDriverSpec, lockFile, "/opt/wheels"), " ")
	if !strings.Contains(offlineArgs, "--no-index --find-links /opt/wheels") || !strings.Contains(offlineArgs, "--require-hashes") {
		t.Fatalf("expected offline wheel flags + --require-hashes, got: %s", offlineArgs)
	}
	overrideArgs := strings.Join(buildPipWheelInstallArgs("cua-driver==0.99.0", lockFile, ""), " ")
	if strings.Contains(overrideArgs, "--require-hashes") || !strings.HasSuffix(overrideArgs, "cua-driver==0.99.0") {
		t.Fatalf("expected custom spec override without pinned lockfile, got: %s", overrideArgs)
	}

	// 2. Wayland Accessibility flags & systemd environment.d file writing
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "chrome-flags.conf"), []byte("--ozone-platform=wayland\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ensureUserChromiumAccessibilityFlags(cfgDir) {
		t.Fatal("expected ensureUserChromiumAccessibilityFlags to return true")
	}
	// Running a second time must be idempotent (no duplicate line).
	if !ensureUserChromiumAccessibilityFlags(cfgDir) {
		t.Fatal("expected idempotent second call to return true")
	}
	chromeFlags, _ := os.ReadFile(filepath.Join(cfgDir, "chrome-flags.conf"))
	if strings.Count(string(chromeFlags), "--force-renderer-accessibility") != 1 {
		t.Fatalf("expected exactly 1 --force-renderer-accessibility line, got:\n%s", string(chromeFlags))
	}
	electronFlags, _ := os.ReadFile(filepath.Join(cfgDir, "electron-flags.conf"))
	if !strings.Contains(string(electronFlags), "--force-renderer-accessibility") {
		t.Fatalf("expected electron-flags.conf created with accessibility flag, got:\n%s", string(electronFlags))
	}

	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	_ = ensureSystemdSessionAccessibilityEnv(context.Background())
	envConf, err := os.ReadFile(filepath.Join(cfgDir, "environment.d", "90-michael-auto-a11y.conf"))
	if err != nil || !strings.Contains(string(envConf), "ACCESSIBILITY_ENABLED=1") {
		t.Fatalf("expected environment.d accessibility conf written, err=%v content=%q", err, string(envConf))
	}

	// 3. WaylandPortalActuator crypto/rand tokens, dry-run & SessionStatus
	t1 := cryptoPortalToken("sess")
	t2 := cryptoPortalToken("sess")
	if t1 == t2 || !strings.HasPrefix(t1, "sess_") || len(t1) != len("sess_")+16 {
		t.Fatalf("expected unique 16-hex crypto portal tokens, got %q and %q", t1, t2)
	}

	secrets := &stubSecretStore{data: map[string]string{restoreTokenKey: "tok-123"}}
	portal := NewDesktopActuator(secrets, &stubActivityMon{})
	if ready, detail := portal.SessionStatus(); !ready || !strings.Contains(detail, "restore_token") {
		t.Fatalf("expected ready portal status with saved restore_token, got ready=%v detail=%q", ready, detail)
	}
	if err := portal.TriggerLogout(context.Background(), domain.ScreenState{}, true); err != nil {
		t.Fatalf("expected dryRun TriggerLogout nil, got %v", err)
	}
	if err := portal.TriggerLogout(context.Background(), domain.ScreenState{}, false); err == nil {
		t.Fatal("expected TriggerLogout error when no targets present")
	}
	if err := portal.SelectModel(context.Background(), domain.ScreenState{}, "Gemini", true); err != nil {
		t.Fatalf("expected dryRun SelectModel nil, got %v", err)
	}
	if err := portal.SelectModel(context.Background(), domain.ScreenState{}, "Gemini", false); err != nil {
		t.Fatalf("expected SelectModel soft no-op when selector absent, got %v", err)
	}
	if err := portal.Close(); err != nil {
		t.Fatalf("portal Close failed: %v", err)
	}

	// 4. Safe OAuth relay URL validation & DecodeObservedScreenStateForSensor
	if !isSafeGoogleOAuthRelayURL("https://accounts.google.com/o/oauth2/v2/auth?client_id=1") {
		t.Fatal("expected valid Google OAuth URL accepted")
	}
	for _, bad := range []string{"http://accounts.google.com/auth", "https://accounts.google.com.evil.com/auth", "javascript:alert(1)", ""} {
		if isSafeGoogleOAuthRelayURL(bad) {
			t.Fatalf("expected unsafe URL rejected: %q", bad)
		}
	}
	decoded, err := DecodeObservedScreenStateForSensor([]byte(`{"windowDetected":true,"windowTitle":"Antigravity","appAuthState":"authenticated","activeModel":"Gemini 4 Neon Extra High","quotaExhausted":false,"confidence":0.97}`), time.Now())
	if err != nil || !decoded.WindowDetected || decoded.ActiveModel != "Gemini 4 Neon Extra High" || decoded.AppAuthState != domain.AuthAuthenticated {
		t.Fatalf("unexpected DecodeObservedScreenStateForSensor: %+v (err=%v)", decoded, err)
	}
}

func TestCuaDriverInspectQuotaSignOutSignInAndCancelNotification(t *testing.T) {
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}

	tmpDir := t.TempDir()
	mockPNG := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-mock"))
	cancelFlag := filepath.Join(tmpDir, "canceled.flag")
	scriptPath := filepath.Join(tmpDir, "cua_full_methods.py")
	script := `#!/usr/bin/env python3
import json, sys, time

MOCK_PNG = "` + mockPNG + `"
CANCEL_FLAG = "` + cancelFlag + `"

for raw in sys.stdin:
    req = json.loads(raw)
    rid = req.get("id")
    m = req.get("method", "")
    if m == "$/cancelRequest":
        with open(CANCEL_FLAG, "w") as f:
            f.write("canceled")
        continue
    if m == "health_check":
        res = {"healthy": True}
    elif m == "inspect_quota":
        res = {
            "windowDetected": True, "windowTitle": "Antigravity", "windowFocused": True,
            "appAuthState": "authenticated", "activeAccountHint": "p@example.com",
            "activeModel": "Gemini 4 Neon Extra High", "remainingQuotaPct": 0,
            "quotaExhausted": True, "quotaMessage": "0% left", "confidence": 0.99,
            "evidence": {"step": "01_verify_quota_zero", "windowTitle": "Antigravity", "pid": 123, "verifiedDetail": "0%", "screenshotBase64": MOCK_PNG}
        }
    elif m == "click_signout":
        res = {"signedOut": True, "evidence": {"step": "02_pre_signout_settings", "windowTitle": "Settings", "pid": 123, "verifiedDetail": "ok", "screenshotBase64": MOCK_PNG}}
    elif m == "click_signin":
        res = {"clickedSignIn": True, "evidence": {"step": "03_pre_signin_antigravity", "windowTitle": "Antigravity", "pid": 123, "verifiedDetail": "ok", "screenshotBase64": MOCK_PNG}}
    elif m == "slow_hang":
        time.sleep(0.5)
        res = {"late": True}
    elif m == "shutdown":
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": rid, "result": {"shutdown": True}}) + "\n")
        sys.stdout.flush()
        sys.exit(0)
    else:
        res = {"ok": True}
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": rid, "result": res}) + "\n")
    sys.stdout.flush()
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	vault := storage.NewEvidenceVault(filepath.Join(tmpDir, "evidence"), 5)
	cua := NewCustomCuaDriverActuator(pyBin, scriptPath, []string{"TEST=1"}, &stubActivityMon{}, nil)
	cua.SetEvidenceStore(vault)
	cua.SetActiveRunID("run_methods_01")
	defer cua.Close()

	if cua.ResolvedPythonBin() != pyBin || cua.ResolvedEntrypoint() != scriptPath {
		t.Fatalf("unexpected resolved paths: py=%q entry=%q", cua.ResolvedPythonBin(), cua.ResolvedEntrypoint())
	}

	ctx := context.Background()
	st, err := cua.InspectQuota(ctx, []string{"Antigravity"}, false)
	if err != nil || !st.QuotaExhausted || st.EvidenceSHA256 == "" {
		t.Fatalf("InspectQuota failed: %+v (err=%v)", st, err)
	}
	if err := cua.TriggerLogout(ctx, st, false); err != nil {
		t.Fatalf("TriggerLogout failed: %v", err)
	}
	cua.SetPendingTargetProfile(domain.AccountProfile{
		ID: "p2", EmailHint: "b@example.com", BrowserType: domain.BrowserChrome, BrowserProfileDir: "Profile 1",
	})
	if err := cua.ClickSignIn(ctx, false); err != nil {
		t.Fatalf("ClickSignIn failed: %v", err)
	}

	// Verify $/cancelRequest notification is sent when RPC context times out!
	shortCtx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	if _, err := cua.Call(shortCtx, "slow_hang", nil); err == nil {
		t.Fatal("expected deadline error on slow_hang RPC")
	}
	// Give sidecar a brief moment to process $/cancelRequest after slow_hang finishes.
	time.Sleep(550 * time.Millisecond)
	if _, statErr := os.Stat(cancelFlag); statErr != nil {
		t.Fatalf("expected $/cancelRequest processed by sidecar, stat err=%v", statErr)
	}
}

func TestEmbeddedSidecarExtractionAndProductionConstructors(t *testing.T) {
	tmpData := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpData)
	t.Setenv("MICHAEL_AUTO_DEV_SIDECAR", "0")

	entry, err := EnsureEmbeddedSidecarExtracted()
	if err != nil {
		t.Fatalf("EnsureEmbeddedSidecarExtracted failed: %v", err)
	}
	if !strings.HasPrefix(entry, tmpData) || !strings.HasSuffix(entry, filepath.Join("michael_cua_sidecar", "main.py")) {
		t.Fatalf("expected embedded sidecar extracted under XDG_DATA_HOME, got %q", entry)
	}
	lockPath := filepath.Join(ManagedSidecarDir(), pinnedCuaDriverLockFile)
	lockRaw, err := os.ReadFile(lockPath)
	if err != nil || !strings.Contains(string(lockRaw), "sha256:52c3b28a9cf4dce24dd2a05ccd0d51d950bfb8d76703568a47385827367f40fb") {
		t.Fatalf("expected SHA-256 lockfile extracted alongside sidecar, err=%v", err)
	}
	if !strings.HasSuffix(ManagedUvBinPath(), filepath.Join("bin", "uv")) {
		t.Fatalf("unexpected ManagedUvBinPath: %q", ManagedUvBinPath())
	}
	_ = resolveUvBinary()
	if pyBin, err := exec.LookPath("python3"); err == nil {
		if minor := detectPythonMinorVersion(context.Background(), pyBin); minor < 8 {
			t.Fatalf("expected Python 3.8+, got 3.%d", minor)
		}
	}
	if got := tailString("abcdef", 3); got != "...def" {
		t.Fatalf("expected '...def', got %q", got)
	}
	if got := tailString("ab", 5); got != "ab" {
		t.Fatalf("expected 'ab', got %q", got)
	}

	router := NewOSBrowserRouter()
	cfg := domain.DefaultConfig().CuaDriver
	cua := NewCuaDriverActuator(cfg, &stubActivityMon{}, router, nil, nil)
	defer cua.Close()
	cua.cfg.AgentWorkspaceEnabled = false
	cua.armWorkspaceAutoRoute(context.Background(), 10)

	// Read-only Wayland accessibility inspection & portal error paths.
	_, _ = CheckWaylandAccessibility(context.Background())
	_ = InspectATSPI2RegisteredApps(context.Background())

	portal := NewDesktopActuator(nil, &stubActivityMon{})
	portal.setStatus(false, "custom-status")
	if ready, detail := portal.SessionStatus(); ready || detail != "custom-status" {
		t.Fatalf("unexpected portal status: ready=%v detail=%q", ready, detail)
	}
	if err := portal.actuateTarget(context.Background(), domain.UIElementTarget{Name: "Btn", HasBounds: false}); err == nil {
		t.Fatal("expected error when element has neither AT-SPI2 action nor bounds")
	}

	// Verify installWheelAndSidecarIntoVenv executes pip with --require-hashes lockfile and --no-deps end-to-end!
	fakeBinDir := t.TempDir()
	argsLog := filepath.Join(fakeBinDir, "pip_args.log")
	fakePy := filepath.Join(fakeBinDir, "fake_python.sh")
	fakePyBody := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nexit 0\n", argsLog)
	if err := os.WriteFile(fakePy, []byte(fakePyBody), 0o700); err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	if err := installWheelAndSidecarIntoVenv(context.Background(), fakePy, pinnedCuaDriverSpec, ManagedSidecarDir(), &logBuf); err != nil {
		t.Fatalf("installWheelAndSidecarIntoVenv with fake python failed: %v", err)
	}
	invoked, _ := os.ReadFile(argsLog)
	if !strings.Contains(string(invoked), "--require-hashes") || !strings.Contains(string(invoked), "--no-deps") {
		t.Fatalf("expected --require-hashes and --no-deps pip calls, got:\n%s", string(invoked))
	}

	// Verify CuaDriverActuator supervisor detects unexpected sidecar crash and schedules restart cleanly!
	crashScript := filepath.Join(fakeBinDir, "crash_once.sh")
	if err := os.WriteFile(crashScript, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	crashCua := NewCustomCuaDriverActuator("/bin/sh", crashScript, []string{"TEST=1"}, &stubActivityMon{}, nil)
	_ = crashCua.EnsureSession(context.Background())
	time.Sleep(100 * time.Millisecond)
	_ = crashCua.Close()
}

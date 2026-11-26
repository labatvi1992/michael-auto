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

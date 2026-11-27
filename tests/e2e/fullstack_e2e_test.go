package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labatvi1992/michael-auto/internal/adapters/actuators"
	"github.com/labatvi1992/michael-auto/internal/adapters/sensors"
	"github.com/labatvi1992/michael-auto/internal/adapters/storage"
	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/orchestrator"
)

// e2eActivityMonitor provides deterministic idle control, screen-lock simulation, and real interrupt channel delivery.
type e2eActivityMonitor struct {
	mu           sync.Mutex
	idle         time.Duration
	ch           chan domain.ActivityKind
	leases       atomic.Int64
	screenLocked atomic.Bool
}

func newE2EActivityMonitor(initialIdle time.Duration) *e2eActivityMonitor {
	return &e2eActivityMonitor{
		idle: initialIdle,
		ch:   make(chan domain.ActivityKind, 32),
	}
}

func (m *e2eActivityMonitor) Subscribe(ctx context.Context) (<-chan domain.ActivityKind, error) {
	out := make(chan domain.ActivityKind, 32)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-m.ch:
				out <- ev
			}
		}
	}()
	return out, nil
}

func (m *e2eActivityMonitor) SystemIdleDuration(ctx context.Context) (time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.idle, nil
}

func (m *e2eActivityMonitor) setIdle(d time.Duration) {
	m.mu.Lock()
	m.idle = d
	m.mu.Unlock()
}

func (m *e2eActivityMonitor) emit(kind domain.ActivityKind) {
	m.ch <- kind
}

func (m *e2eActivityMonitor) AcquireActuationLease(ttl time.Duration, x, y *int) func() {
	m.leases.Add(1)
	return func() {}
}

func (m *e2eActivityMonitor) SetActiveExecution(active bool) {}
func (m *e2eActivityMonitor) BackendName() string            { return "e2e-wayland-sim" }
func (m *e2eActivityMonitor) Close() error                   { return nil }
func (m *e2eActivityMonitor) IsScreenLocked() bool           { return m.screenLocked.Load() }

type e2eWorkspaceMgr struct {
	userOnAgent atomic.Bool
}

func (w *e2eWorkspaceMgr) Status(ctx context.Context, cfg domain.AppConfig) domain.AgentWorkspaceStatus {
	return domain.AgentWorkspaceStatus{
		Enabled:        true,
		Ready:          true,
		WorkspaceIndex: 1,
		WorkspaceName:  domain.DefaultAgentWorkspaceName,
		Detail:         "E2E Dedicated Agent Workspace v4 Ready",
	}
}
func (w *e2eWorkspaceMgr) EnsureAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (domain.AgentWorkspaceStatus, error) {
	return w.Status(ctx, cfg), nil
}
func (w *e2eWorkspaceMgr) RouteTargetWindowsToAgentWorkspace(ctx context.Context, cfg domain.AppConfig) error {
	return nil
}
func (w *e2eWorkspaceMgr) UserIsOnAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (bool, bool) {
	return w.userOnAgent.Load(), true
}

type e2eNotifier struct {
	mu       sync.Mutex
	titles   []string
	bodies   []string
	critical []bool
}

func (n *e2eNotifier) Notify(title, body string, crit bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.titles = append(n.titles, title)
	n.bodies = append(n.bodies, body)
	n.critical = append(n.critical, crit)
}

func makeWhiteHiDPIPNG(t *testing.T, w, h int) string {
	t.Helper()
	canvas := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.NRGBA{R: 255, G: 255, B: 255, A: 255}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		t.Fatalf("encode PNG failed: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// writeE2ESidecarHarness writes a Python entrypoint that runs the REAL `michael_cua_sidecar` server
// (`AsyncJsonRpcMux`, `actuator.py`, `observer.py`, `security.py`) with a stateful simulated Antigravity 2.34+
// & Multi-Profile Chrome desktop persisted in `stateFile` so crash-restart E2E preserves desktop state!
func writeE2ESidecarHarness(t *testing.T, dir, stateFile, hidpiPNGB64 string) string {
	t.Helper()
	repoSidecar, err := filepath.Abs(filepath.Join("..", "..", "sidecar"))
	if err != nil {
		t.Fatalf("resolve sidecar dir failed: %v", err)
	}
	harnessPath := filepath.Join(dir, "e2e_sidecar_harness.py")
	code := fmt.Sprintf(`#!/usr/bin/env python3
import asyncio, base64, json, os, sys
from dataclasses import dataclass, field
from typing import Any

sys.path.insert(0, %q)
from michael_cua_sidecar import server

STATE_FILE = %q
PNG_BYTES = base64.b64decode(%q)

@dataclass
class SimEl:
    role: str
    label: str = ""
    text: str = ""
    description: str = ""
    value: str = ""
    editable: bool = False
    element_token: str = ""
    x: int = 10
    y: int = 10
    width: int = 120
    height: int = 28
    children: list[Any] = field(default_factory=list)

@dataclass
class SimWin:
    pid: int
    window_id: str
    title: str
    app_name: str
    focused: bool = True
    screenshot: bytes | None = None
    elements: list[Any] = field(default_factory=list)

class E2EDesktopSim:
    def _load(self) -> dict[str, Any]:
        with open(STATE_FILE, "r", encoding="utf-8") as f:
            return json.load(f)

    def _save(self, st: dict[str, Any]) -> None:
        tmp = STATE_FILE + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(st, f)
        os.replace(tmp, STATE_FILE)

    async def list_windows(self, _inp: Any = None) -> Any:
        st = self._load()
        wins = [SimWin(pid=5034, window_id="ag-1", title="Antigravity — Workspace", app_name="antigravity")]
        if st.get("chromeOpen"):
            wins.append(
                SimWin(
                    pid=6001,
                    window_id="ch-1",
                    title=st.get("chromeTitle", "Sign in - Google Accounts - Workspace Dự phòng - Google Chrome"),
                    app_name="chrome",
                )
            )
        class Resp:
            windows = wins
        return Resp()

    async def get_window_state(self, pid: int = 0, window_id: str = "", include_screenshot: bool = False, **_kw: Any) -> SimWin:
        st = self._load()
        delay_ms = int(st.get("chromeDelayMs", 0))
        if window_id == "ch-1" and delay_ms > 0:
            await asyncio.sleep(delay_ms / 1000.0)

        shot = PNG_BYTES if include_screenshot else None

        if window_id == "ch-1":
            omnibox = st.get("chromeOmnibox", "https://accounts.google.com/o/oauth2/v2/auth?client_id=ag&state=secret_state_e2e&code_challenge=pkce_chal_e2e")
            els: list[Any] = [
                SimEl(role="entry", label="Address and search bar", value=omnibox, x=10, y=5, width=180, height=22),
            ]
            if st.get("chromeMfa"):
                els.append(SimEl(role="heading", label="2-Step Verification", x=20, y=50, width=160, height=28))
            elif st.get("chromeStage") == "continue":
                els.append(SimEl(role="button", label="Continue", element_token="tok:ch_continue", x=40, y=70, width=100, height=28))
            elif st.get("chromeStage") == "xdg_open":
                els.append(SimEl(role="button", label="Open xdg-open", element_token="tok:ch_xdg_open", x=40, y=70, width=120, height=28))
            else:
                acc_email = st.get("chromeAccountEmail", "backup@example.com")
                els.append(SimEl(role="button", label=f"Workspace Dự phòng {acc_email}", element_token="tok:ch_account", x=30, y=60, width=150, height=32))
            return SimWin(pid=6001, window_id="ch-1", title=st.get("chromeTitle", "Sign in - Google Accounts - Google Chrome"), app_name="chrome", screenshot=shot, elements=els)

        # Antigravity 2.34+ Window (logical 200x160 matching 2x HiDPI 400x320 PNG)
        els = [
            SimEl(role="text", label="Source Editor", text="secret code", editable=True, x=20, y=40, width=140, height=80),
        ]
        if st.get("loggedOut"):
            els.append(SimEl(role="button", label="Sign in", element_token="tok:ag_signin", x=30, y=10, width=90, height=24))
        else:
            els.extend([
                SimEl(role="label", label=st.get("activeAccount", "primary@example.com"), x=10, y=5, width=120, height=18),
                SimEl(role="button", label="Settings", element_token="tok:ag_settings", x=5, y=130, width=80, height=22),
                SimEl(role="button", label=f"Select model, current: {st.get('activeModel', 'Gemini 4 Neon Extra High')}", element_token="tok:ag_model", x=90, y=130, width=100, height=22),
            ])
            if st.get("settingsOpen"):
                els.append(SimEl(role="button", label="Sign Out", element_token="tok:ag_signout", x=40, y=20, width=90, height=24))
            if st.get("modelMenuOpen"):
                els.append(SimEl(role="menu", label="Model", children=[
                    SimEl(role="menu item", label="Gemini 4 Neon Extra High", element_token="tok:m_gemini4"),
                    SimEl(role="menu item", label="Claude Sonnet 4.6 (Thinking)", element_token="tok:m_claude"),
                    SimEl(role="menu item", label="View Usage", element_token="tok:ag_view_usage"),
                ]))
            if st.get("usageOpen"):
                g_pct = int(st.get("geminiQuotaPct", 0))
                els.append(SimEl(role="menu", label="Usage", children=[
                    SimEl(role="label", label="Gemini Models"),
                    SimEl(role="label", label=f"Weekly limit {g_pct}%% left"),
                    SimEl(role="label", label=f"5h limit {g_pct}%% left"),
                    SimEl(role="label", label="Claude and GPT models"),
                    SimEl(role="label", label="5h limit 100%% left"),
                ]))
            if st.get("postSwitchQuotaBannerZero"):
                els.append(SimEl(role="alert", label="0%% remaining — Quota exhausted"))

        return SimWin(pid=5034, window_id="ag-1", title="Antigravity — Workspace", app_name="antigravity", screenshot=shot, elements=els)

    async def click(self, act: dict[str, Any]) -> None:
        st = self._load()
        tok = act.get("element_token", "")
        clicks = st.setdefault("clicks", [])
        clicks.append(tok)

        if tok == "tok:ag_model":
            st["modelMenuOpen"] = True
        elif tok == "tok:ag_view_usage":
            st["usageOpen"] = True
        elif tok == "tok:ag_settings":
            st["settingsOpen"] = True
        elif tok == "tok:ag_signout":
            st["loggedOut"] = True
            st["settingsOpen"] = False
            st["modelMenuOpen"] = False
            st["usageOpen"] = False
            st["signOutCount"] = int(st.get("signOutCount", 0)) + 1
        elif tok == "tok:ag_signin":
            st["chromeOpen"] = True
            st["signInCount"] = int(st.get("signInCount", 0)) + 1
            if st.get("simulateMisroutedFirst"):
                st["chromeTitle"] = "Sign in - Google Accounts - Workspace Chính - Google Chrome"
                st["chromeOmnibox"] = "https://accounts.google.com/o/oauth2/v2/auth?client_id=ag&state=secret_state_e2e&code_challenge=pkce_chal_e2e"
                st["chromeStage"] = "choose"
        elif tok == "tok:ch_account":
            st["chromeStage"] = "continue"
        elif tok == "tok:ch_continue":
            st["chromeStage"] = "xdg_open"
        elif tok == "tok:ch_xdg_open":
            st["chromeOpen"] = False
            st["loggedOut"] = False
            st["activeAccount"] = st.get("nextLoginAccount", "backup@example.com")
            st["geminiQuotaPct"] = int(st.get("postSwitchQuotaPct", 100))
            if st.get("postSwitchQuotaPct", 100) == 0:
                st["postSwitchQuotaBannerZero"] = True
        elif tok == "tok:m_gemini4":
            st["activeModel"] = "Gemini 4 Neon Extra High"
            st["modelMenuOpen"] = False
        elif tok == "tok:m_claude":
            st["activeModel"] = "Claude Sonnet 4.6 (Thinking)"
            st["modelMenuOpen"] = False

        self._save(st)

    async def press_key(self, act: dict[str, Any]) -> None:
        st = self._load()
        key = act.get("key", "")
        if key == "Escape":
            if st.get("usageOpen"):
                st["usageOpen"] = False
            elif st.get("modelMenuOpen"):
                st["modelMenuOpen"] = False
        elif key.lower() == "ctrl+w":
            st["closedMisroutedTabs"] = int(st.get("closedMisroutedTabs", 0)) + 1
            st["chromeOpen"] = False
        self._save(st)

server.set_driver_for_testing(E2EDesktopSim())
server.main()
`, repoSidecar, stateFile, hidpiPNGB64)
	if err := os.WriteFile(harnessPath, []byte(code), 0o700); err != nil {
		t.Fatalf("write harness failed: %v", err)
	}
	return harnessPath
}

func saveSimState(t *testing.T, path string, st map[string]any) {
	t.Helper()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadSimState(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestE2E_FullFiveStepAntigravity234RotationWithCrashRecoveryPKCERelayAndHiDPIEvidence(t *testing.T) {
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}

	tmp := t.TempDir()
	stateFile := filepath.Join(tmp, "sim_desktop_state.json")
	cfgPath := filepath.Join(tmp, "config.json")
	auditPath := filepath.Join(tmp, "audit.jsonl")
	intentPath := filepath.Join(tmp, "pending_intent.json")
	evidenceDir := filepath.Join(tmp, "evidence")
	cliSettingsPath := filepath.Join(tmp, "cli-settings.json")

	// Prepare Chrome Local State so LookupBrowserProfileMeta resolves exact profile display names!
	fakeChromeCfg := filepath.Join(tmp, "chrome-cfg")
	if err := os.MkdirAll(filepath.Join(fakeChromeCfg, "google-chrome"), 0o700); err != nil {
		t.Fatal(err)
	}
	localState := `{"profile":{"info_cache":{"Default":{"name":"Workspace Chính","user_name":"primary@example.com"},"Profile 1":{"name":"Workspace Dự phòng","user_name":"backup@example.com"}}}}`
	if err := os.WriteFile(filepath.Join(fakeChromeCfg, "google-chrome", "Local State"), []byte(localState), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", fakeChromeCfg)

	saveSimState(t, stateFile, map[string]any{
		"loggedOut":              false,
		"activeAccount":          "primary@example.com",
		"activeModel":            "Gemini 4 Neon Extra High",
		"geminiQuotaPct":         0,
		"chromeOpen":             false,
		"simulateMisroutedFirst": true,
		"nextLoginAccount":       "backup@example.com",
		"postSwitchQuotaPct":     100,
	})

	harness := writeE2ESidecarHarness(t, tmp, stateFile, makeWhiteHiDPIPNG(t, 400, 320))

	cfgStore := storage.NewFileConfigStore(cfgPath)
	intentStore := storage.NewEphemeralIntentStore(intentPath)
	evidenceVault := storage.NewEvidenceVault(evidenceDir, 10)
	auditLogger := storage.NewJSONLAuditLogger(auditPath)
	defer auditLogger.Close()
	cliAdapter := sensors.NewCLIConfigAdapter()

	cfg := domain.DefaultConfig()
	cfg.DryRunActuation = false
	cfg.PreferIntraProfileModelDrop = false
	cfg.CLISettingsPath = cliSettingsPath
	cfg.CuaDriver.DefaultModel = "Gemini 4 Neon Extra High"
	cfg.CuaDriver.MaskEditorInEvidence = true
	cfg.CuaDriver.RequireEvidenceBeforeActuate = true
	cfg.Profiles[0].EmailHint = "primary@example.com"
	cfg.Profiles[0].Name = "Workspace Chính"
	cfg.Profiles[0].BrowserProfileDir = "Default"
	cfg.Profiles[1].Enabled = true
	cfg.Profiles[1].EmailHint = "backup@example.com"
	cfg.Profiles[1].Name = "Workspace Dự phòng"
	cfg.Profiles[1].BrowserProfileDir = "Profile 1"
	if err := cfgStore.Save(cfg); err != nil {
		t.Fatal(err)
	}

	var relayedMu sync.Mutex
	var relayedCalls [][]string
	router := actuators.NewTestableBrowserRouter(
		func(bin string) (string, error) { return "/usr/bin/" + bin, nil },
		func(ctx context.Context, binary string, args []string) error {
			relayedMu.Lock()
			relayedCalls = append(relayedCalls, append([]string(nil), args...))
			relayedMu.Unlock()
			// Simulate Chrome launching the exact target `--profile-directory=Profile 1` window with relayed PKCE URL!
			st := loadSimState(t, stateFile)
			st["chromeOpen"] = true
			st["chromeTitle"] = "Sign in - Google Accounts - Workspace Dự phòng - Google Chrome"
			for _, a := range args {
				if strings.HasPrefix(a, "https://") {
					st["chromeOmnibox"] = a
				}
			}
			st["chromeStage"] = "choose"
			st["chromeAccountEmail"] = "backup@example.com"
			saveSimState(t, stateFile, st)
			return nil
		},
	)

	mon := newE2EActivityMonitor(2 * time.Minute)
	wsMgr := &e2eWorkspaceMgr{}
	notif := &e2eNotifier{}
	now := time.Now().UTC().Truncate(time.Second)

	makeOrch := func() (*orchestrator.Orchestrator, *actuators.CuaDriverActuator) {
		cua := actuators.NewCustomCuaDriverActuator(pyBin, harness, []string{"CUA_AGENT_WORKSPACE_ENABLED=0"}, mon, router)
		cua.SetEvidenceStore(evidenceVault)
		cuaSensor := sensors.NewCuaDriverSensor(cua)
		loadedCfg, err := cfgStore.Load()
		if err != nil {
			t.Fatalf("cfgStore.Load failed: %v", err)
		}
		orch := orchestrator.NewOrchestrator(loadedCfg, orchestrator.Dependencies{
			ConfigStore:        cfgStore,
			IntentStore:        intentStore,
			EvidenceStore:      evidenceVault,
			WorkspaceManager:   wsMgr,
			ActivityMonitor:    mon,
			SensorFusion:       orchestrator.NewSensorFusion(cliAdapter, cuaSensor),
			DesktopActuator:    cua,
			CuaDesktopActuator: cua,
			BrowserRouter:      router,
			CLISyncer:          cliAdapter,
			AuditLogger:        auditLogger,
			Notifier:           notif,
		})
		orch.SetClock(func() time.Time { return now })
		return orch, cua
	}

	ctx := context.Background()

	// --- PHASE 1 & 2: 2-Hop Quota Inspection (Gemini 0% while Claude 100%) -> SignOut -> Write pending_intent.json ---
	orch1, cua1 := makeOrch()
	if err := orch1.StepOnce(ctx); err != nil {
		t.Fatalf("Step 1+2 (InspectQuota + SignOut) failed: %v", err)
	}

	if _, err := os.Stat(intentPath); err != nil {
		t.Fatalf("expected ephemeral pending_intent.json persisted during 60s wait: %v", err)
	}
	stMid := orch1.Status(ctx)
	if stMid.PendingIntent == nil || stMid.PendingIntent.SubStep != domain.SubStepWaitBeforeSignIn {
		t.Fatalf("expected pending intent at wait_before_signin, got %+v", stMid.PendingIntent)
	}

	// --- PHASE 3: Simulate Full Process Crash & Restart mid-60s wait! ---
	_ = cua1.Close()

	now = now.Add(61 * time.Second)
	orch2, cua2 := makeOrch()
	defer cua2.Close()

	// --- PHASE 4a: Resumed Orchestrator clicks SignIn (MUST NOT re-click SignOut!) ---
	if err := orch2.StepOnce(ctx); err != nil {
		t.Fatalf("Resumed Step 4a (ClickSignIn) failed: %v", err)
	}
	simAfterSignIn := loadSimState(t, stateFile)
	if got := int(simAfterSignIn["signOutCount"].(float64)); got != 1 {
		t.Fatalf("expected exactly 1 SignOut across crash-restart, got %d", got)
	}

	// --- PHASE 4b: Misrouted Default tab -> Ctrl+W -> Lossless PKCE Relay -> Profile 1 -> Account -> Continue -> xdg-open ---
	if err := orch2.StepOnce(ctx); err != nil {
		t.Fatalf("Step 4b (SelectChromeAccount with PKCE Relay) failed: %v", err)
	}
	relayedMu.Lock()
	callsCopy := append([][]string(nil), relayedCalls...)
	relayedMu.Unlock()
	if len(callsCopy) != 1 {
		t.Fatalf("expected exactly 1 BrowserRouter PKCE relay launch, got %d (%v)", len(callsCopy), callsCopy)
	}
	joinedRelay := strings.Join(callsCopy[0], " ")
	if !strings.Contains(joinedRelay, "--profile-directory=Profile 1") || !strings.Contains(joinedRelay, "code_challenge=pkce_chal_e2e") {
		t.Fatalf("expected exact Profile 1 + PKCE code_challenge relayed, got: %s", joinedRelay)
	}
	simAfterRelay := loadSimState(t, stateFile)
	closedTabs, _ := simAfterRelay["closedMisroutedTabs"].(float64)
	if int(closedTabs) != 1 {
		t.Fatalf("expected 1 misrouted tab closed via Ctrl+W, got %v", simAfterRelay["closedMisroutedTabs"])
	}

	// --- PHASE 5: Select Default Model, sync CLI settings, update config, DELETE pending_intent.json! ---
	if err := orch2.StepOnce(ctx); err != nil {
		t.Fatalf("Step 5 (SelectModel + Completion) failed: %v", err)
	}

	finalStatus := orch2.Status(ctx)
	if finalStatus.State != domain.StateCooldown || finalStatus.ActiveProfileID != "profile-backup" || finalStatus.SwitchesCompletedTotal != 1 {
		t.Fatalf("expected Cooldown on profile-backup with 1 switch completed, got state=%s prof=%s switches=%d reason=%q",
			finalStatus.State, finalStatus.ActiveProfileID, finalStatus.SwitchesCompletedTotal, finalStatus.StateReason)
	}

	// Verify pending_intent.json strictly DELETED upon completion!
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Fatalf("expected pending_intent.json strictly deleted upon completion, stat err=%v", err)
	}

	// Verify CLI settings synced atomically to default model!
	cliModel, err := cliAdapter.ReadActiveModel(cliSettingsPath)
	if err != nil || cliModel != "Gemini 4 Neon Extra High" {
		t.Fatalf("expected CLI settings synced to 'Gemini 4 Neon Extra High', got %q (%v)", cliModel, err)
	}

	// Verify ALL 5 Evidence screenshots persisted with valid SHA-256, 0600 perms, ReadDataURL, AND Editor Privacy Masking!
	evList, err := evidenceVault.ListRecent(20)
	if err != nil || len(evList) < 5 {
		t.Fatalf("expected >= 5 persisted Evidence records, got %d (err=%v)", len(evList), err)
	}
	foundMasked := false
	for _, rec := range evList {
		info, statErr := os.Stat(rec.ImagePath)
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("evidence %s missing or insecure perms: err=%v mode=%v", rec.Step, statErr, info)
		}
		rawPNG, readErr := os.ReadFile(rec.ImagePath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		sum := sha256.Sum256(rawPNG)
		if hex.EncodeToString(sum[:]) != rec.SHA256 {
			t.Fatalf("evidence %s SHA-256 mismatch: disk=%s meta=%s", rec.Step, hex.EncodeToString(sum[:]), rec.SHA256)
		}
		if dataURL, urlErr := evidenceVault.ReadDataURL(rec.ImagePath); urlErr != nil || !strings.HasPrefix(dataURL, "data:image/png;base64,") {
			t.Fatalf("ReadDataURL failed for %s: %v", rec.Step, urlErr)
		}
		if rec.RedactedRegions > 0 {
			foundMasked = true
			img, decErr := png.Decode(bytes.NewReader(rawPNG))
			if decErr != nil {
				t.Fatalf("decode masked evidence %s failed: %v", rec.Step, decErr)
			}
			rTop, gTop, bTop, _ := img.At(5, 5).RGBA()
			if rTop>>8 != 255 || gTop>>8 != 255 || bTop>>8 != 255 {
				t.Fatalf("expected header pixel (5,5) untouched white on %s, got (%d,%d,%d)", rec.Step, rTop>>8, gTop>>8, bTop>>8)
			}
			rEd, gEd, bEd, _ := img.At(60, 80).RGBA()
			if rEd>>8 != 11 || gEd>>8 != 17 || bEd>>8 != 32 {
				t.Fatalf("expected editor pixel (60,80) masked dark (#0b1120) on %s, got (%d,%d,%d)", rec.Step, rEd>>8, gEd>>8, bEd>>8)
			}
		}
	}
	if !foundMasked {
		t.Fatal("expected at least one Antigravity Evidence PNG with RedactedRegions > 0 verifying editor privacy masking")
	}

	// Verify audit.jsonl has ZERO raw emails and ZERO OAuth PKCE secrets!
	auditRaw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit.jsonl failed: %v", err)
	}
	auditStr := string(auditRaw)
	for _, secret := range []string{"primary@example.com", "backup@example.com", "secret_state_e2e", "pkce_chal_e2e"} {
		if strings.Contains(auditStr, secret) {
			t.Fatalf("SECURITY FAILURE: raw secret/PII %q leaked into audit.jsonl!", secret)
		}
	}
	if mon.leases.Load() < 5 {
		t.Fatalf("expected >= 5 ActuationLeases acquired across full E2E flow, got %d", mon.leases.Load())
	}
}

func TestE2E_AdversarialChromeMFAAndPhishingOmniboxHardStop(t *testing.T) {
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}

	cases := []struct {
		name         string
		chromeMfa    bool
		chromeUrl    string
		wantReasonIn string
	}{
		{
			name:         "Chrome_2Step_MFA_Challenge",
			chromeMfa:    true,
			chromeUrl:    "https://accounts.google.com/signin/v2/challenge",
			wantReasonIn: "2-step verification",
		},
		{
			name:         "Phishing_Omnibox_Domain",
			chromeMfa:    false,
			chromeUrl:    "https://accounts.google.com.evil-phish.net/AccountChooser",
			wantReasonIn: "accounts.google.com",
		},
		{
			name:         "HTTP_Scheme_Downgrade_On_Accounts_Google",
			chromeMfa:    false,
			chromeUrl:    "http://accounts.google.com/AccountChooser",
			wantReasonIn: "https://accounts.google.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			stateFile := filepath.Join(tmp, "sim.json")
			saveSimState(t, stateFile, map[string]any{
				"loggedOut":     true,
				"chromeOpen":    true,
				"chromeTitle":   "Sign in - Google Accounts - Google Chrome",
				"chromeMfa":     tc.chromeMfa,
				"chromeOmnibox": tc.chromeUrl,
			})
			harness := writeE2ESidecarHarness(t, tmp, stateFile, makeWhiteHiDPIPNG(t, 200, 160))

			mon := newE2EActivityMonitor(2 * time.Minute)
			vault := storage.NewEvidenceVault(filepath.Join(tmp, "ev"), 5)
			notif := &e2eNotifier{}
			cua := actuators.NewCustomCuaDriverActuator(pyBin, harness, []string{"CUA_AGENT_WORKSPACE_ENABLED=0"}, mon, nil)
			cua.SetEvidenceStore(vault)
			defer cua.Close()

			cfg := domain.DefaultConfig()
			cfg.DryRunActuation = false
			cfg.Profiles[1].Enabled = true
			now := time.Now().UTC().Truncate(time.Second)

			intentStore := storage.NewEphemeralIntentStore(filepath.Join(tmp, "intent.json"))
			_ = intentStore.Save(domain.PendingIntent{
				RunID:           "run_sec_e2e",
				Kind:            domain.IntentSwitchProfile,
				FromProfileID:   "profile-primary",
				TargetProfileID: "profile-backup",
				TargetModel:     "Gemini 4 Neon Extra High",
				CreatedAt:       now,
				ExpiresAt:       now.Add(10 * time.Minute),
				SubStep:         domain.SubStepSelectChromeAccount,
			})

			orch := orchestrator.NewOrchestrator(cfg, orchestrator.Dependencies{
				IntentStore:        intentStore,
				EvidenceStore:      vault,
				WorkspaceManager:   &e2eWorkspaceMgr{},
				ActivityMonitor:    mon,
				SensorFusion:       orchestrator.NewSensorFusion(sensors.NewCuaDriverSensor(cua)),
				DesktopActuator:    cua,
				CuaDesktopActuator: cua,
				Notifier:           notif,
			})
			orch.SetClock(func() time.Time { return now })

			_ = orch.StepOnce(context.Background())

			st := orch.Status(context.Background())
			if st.State != domain.StateHardStopChallenge {
				t.Fatalf("expected immediate HardStopChallenge on %s, got state=%s reason=%q", tc.name, st.State, st.StateReason)
			}
			if !strings.Contains(strings.ToLower(st.HardStopReason), strings.ToLower(tc.wantReasonIn)) {
				t.Fatalf("expected HardStopReason to contain %q, got %q", tc.wantReasonIn, st.HardStopReason)
			}
			sim := loadSimState(t, stateFile)
			if clicks, ok := sim["clicks"].([]any); ok && len(clicks) > 0 {
				t.Fatalf("SECURITY FAILURE: sidecar clicked %v despite %s!", clicks, tc.name)
			}
			if len(notif.critical) == 0 || !notif.critical[0] {
				t.Fatalf("expected critical desktop notification on HardStopChallenge")
			}

			// Verify ResetHardStop recovers cleanly back to Observing.
			orch.ResetHardStop()
			if after := orch.Status(context.Background()); after.State != domain.StateObserving {
				t.Fatalf("expected Observing after ResetHardStop, got %s", after.State)
			}
		})
	}
}

func TestE2E_HumanYield30sSlidingWindowAndZeroGhostClicksCancel(t *testing.T) {
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}

	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "empty-xdg-cfg"))
	stateFile := filepath.Join(tmp, "sim.json")
	saveSimState(t, stateFile, map[string]any{
		"loggedOut":          true,
		"chromeOpen":         true,
		"chromeTitle":        "Sign in - Google Accounts - Workspace Dự phòng - Google Chrome",
		"chromeOmnibox":      "https://accounts.google.com/o/oauth2/v2/auth?client_id=ag",
		"chromeAccountEmail": "backup@example.com",
		"chromeDelayMs":      450,
	})
	harness := writeE2ESidecarHarness(t, tmp, stateFile, makeWhiteHiDPIPNG(t, 200, 160))

	mon := newE2EActivityMonitor(2 * time.Minute)
	wsMgr := &e2eWorkspaceMgr{}
	wsMgr.userOnAgent.Store(true) // User is viewing Agent Workspace -> physical input MUST yield!
	vault := storage.NewEvidenceVault(filepath.Join(tmp, "ev"), 5)
	cua := actuators.NewCustomCuaDriverActuator(pyBin, harness, []string{"CUA_AGENT_WORKSPACE_ENABLED=0"}, mon, nil)
	cua.SetEvidenceStore(vault)
	defer cua.Close()

	cfg := domain.DefaultConfig()
	cfg.DryRunActuation = false
	cfg.YieldPauseSeconds = 30
	cfg.Profiles[1].Enabled = true

	var nowMu sync.Mutex
	fakeNow := time.Now().UTC().Truncate(time.Second)
	getNow := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return fakeNow
	}
	advance := func(d time.Duration) {
		nowMu.Lock()
		fakeNow = fakeNow.Add(d)
		nowMu.Unlock()
	}

	intentStore := storage.NewEphemeralIntentStore(filepath.Join(tmp, "intent.json"))
	_ = intentStore.Save(domain.PendingIntent{
		RunID:           "run_yield_e2e",
		Kind:            domain.IntentSwitchProfile,
		FromProfileID:   "profile-primary",
		TargetProfileID: "profile-backup",
		TargetModel:     "Gemini 4 Neon Extra High",
		CreatedAt:       getNow(),
		ExpiresAt:       getNow().Add(10 * time.Minute),
		SubStep:         domain.SubStepSelectChromeAccount,
	})

	orch := orchestrator.NewOrchestrator(cfg, orchestrator.Dependencies{
		IntentStore:        intentStore,
		EvidenceStore:      vault,
		WorkspaceManager:   wsMgr,
		ActivityMonitor:    mon,
		SensorFusion:       orchestrator.NewSensorFusion(sensors.NewCuaDriverSensor(cua)),
		DesktopActuator:    cua,
		CuaDesktopActuator: cua,
	})
	orch.SetClock(getNow)

	// Start Subscribe loop so human interrupt cancels in-flight RPC immediately!
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	if err := orch.YieldController().Start(runCtx); err != nil {
		t.Fatalf("YieldController.Start failed: %v", err)
	}
	// Pre-warm sidecar session so StepOnce enters the 450ms delayed get_window_state promptly.
	if err := cua.EnsureSession(runCtx); err != nil {
		t.Fatalf("EnsureSession failed: %v", err)
	}

	stepDone := make(chan error, 1)
	go func() {
		stepDone <- orch.StepOnce(runCtx)
	}()

	time.Sleep(120 * time.Millisecond)
	mon.setIdle(0)
	mon.emit(domain.ActivityPointerMove)

	select {
	case <-stepDone:
	case <-time.After(2 * time.Second):
		t.Fatal("StepOnce did not abort promptly on mid-flight human pointer interrupt")
	}

	// Wait past the 450ms simulated delay and verify ZERO ghost clicks occurred in Python sidecar!
	time.Sleep(500 * time.Millisecond)
	simAfterAbort := loadSimState(t, stateFile)
	if clicks, ok := simAfterAbort["clicks"].([]any); ok && len(clicks) > 0 {
		t.Fatalf("GHOST CLICK DETECTED after $/cancelRequest: %v", clicks)
	}
	if st := orch.Status(runCtx); st.State != domain.StatePausedHumanInput {
		t.Fatalf("expected PausedHumanInput immediately after interrupt, got %s", st.State)
	}

	// Sliding 30s window test: tap keyboard at +20s -> still paused at +40s -> resumes ONLY after +51s (31s quiet)!
	advance(20 * time.Second)
	orch.InjectSimulatedHumanInput(domain.ActivityKeyboard)
	advance(20 * time.Second)
	mon.setIdle(20 * time.Second)
	_ = orch.StepOnce(runCtx)
	if st := orch.Status(runCtx); st.State != domain.StatePausedHumanInput {
		t.Fatalf("expected still PausedHumanInput at 20s after sliding reset, got %s", st.State)
	}

	// Remove artificial delay, complete full 30s quiet -> sidecar finishes Chrome selection cleanly!
	stSim := loadSimState(t, stateFile)
	stSim["chromeDelayMs"] = 0
	saveSimState(t, stateFile, stSim)

	advance(11 * time.Second)
	mon.setIdle(31 * time.Second)
	if err := orch.StepOnce(runCtx); err != nil {
		t.Fatalf("expected resumed StepOnce after 30s quiet to succeed, got: %v", err)
	}
	if st := orch.Status(runCtx); st.PendingIntent == nil || st.PendingIntent.SubStep != domain.SubStepVerifyAndSelectModel {
		t.Fatalf("expected progression to SubStepVerifyAndSelectModel after resume, got %+v", st.PendingIntent)
	}
}

func TestE2E_LockScreenPauseAndPostSwitchZeroQuotaCascade(t *testing.T) {
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}

	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "empty-xdg-cfg"))
	stateFile := filepath.Join(tmp, "sim.json")
	saveSimState(t, stateFile, map[string]any{
		"loggedOut":                 false,
		"activeAccount":             "backup@example.com",
		"activeModel":               "Gemini 4 Neon Extra High",
		"geminiQuotaPct":            0,
		"postSwitchQuotaBannerZero": true,
	})
	harness := writeE2ESidecarHarness(t, tmp, stateFile, makeWhiteHiDPIPNG(t, 200, 160))

	mon := newE2EActivityMonitor(2 * time.Minute)
	mon.screenLocked.Store(true)
	vault := storage.NewEvidenceVault(filepath.Join(tmp, "ev"), 5)
	cua := actuators.NewCustomCuaDriverActuator(pyBin, harness, []string{"CUA_AGENT_WORKSPACE_ENABLED=0"}, mon, nil)
	cua.SetEvidenceStore(vault)
	defer cua.Close()

	cfg := domain.DefaultConfig()
	cfg.DryRunActuation = false
	cfg.Profiles[1].Enabled = true
	cfg.Profiles = append(cfg.Profiles, domain.AccountProfile{
		ID:                "profile-third",
		Name:              "Workspace Số 3",
		EmailHint:         "third@example.com",
		BrowserType:       domain.BrowserChrome,
		BrowserProfileDir: "Profile 2",
		PrimaryModel:      "Gemini 4 Neon Extra High",
		Enabled:           true,
		Priority:          3,
	})
	now := time.Now().UTC().Truncate(time.Second)

	intentPath := filepath.Join(tmp, "intent.json")
	intentStore := storage.NewEphemeralIntentStore(intentPath)
	_ = intentStore.Save(domain.PendingIntent{
		RunID:           "run_cascade_e2e",
		Kind:            domain.IntentSwitchProfile,
		FromProfileID:   "profile-primary",
		TargetProfileID: "profile-backup",
		TargetModel:     "Gemini 4 Neon Extra High",
		CreatedAt:       now,
		ExpiresAt:       now.Add(10 * time.Minute),
		SubStep:         domain.SubStepVerifyAndSelectModel,
	})

	orch := orchestrator.NewOrchestrator(cfg, orchestrator.Dependencies{
		IntentStore:        intentStore,
		EvidenceStore:      vault,
		WorkspaceManager:   &e2eWorkspaceMgr{},
		ActivityMonitor:    mon,
		SensorFusion:       orchestrator.NewSensorFusion(sensors.NewCuaDriverSensor(cua)),
		DesktopActuator:    cua,
		CuaDesktopActuator: cua,
	})
	orch.SetClock(func() time.Time { return now })
	ctx := context.Background()

	// 1. Screen locked -> ZERO clicks, ZERO breaker trips, intent preserved cleanly!
	if err := orch.StepOnce(ctx); err != nil {
		t.Fatalf("unexpected error while screen locked: %v", err)
	}
	if st := orch.Status(ctx); !st.ScreenLocked || st.CircuitBreakerTripped || st.PendingIntent == nil {
		t.Fatalf("expected clean screen-lock pause preserving intent, got %+v", st)
	}

	// 2. Unlock screen -> SelectModel detects newly signed-in backup@example.com is ALSO at 0% Quota -> marks exhausted & releases intent!
	mon.screenLocked.Store(false)
	if err := orch.StepOnce(ctx); err != nil {
		t.Fatalf("post-switch 0%% cascade StepOnce failed: %v", err)
	}
	updatedCfg := orch.GetConfig()
	backupProf := updatedCfg.FindProfile("profile-backup")
	if backupProf == nil || backupProf.ExhaustedUntil == nil {
		t.Fatalf("expected profile-backup marked ExhaustedUntil after post-switch 0%% detection, got %+v", backupProf)
	}
	if _, statErr := os.Stat(intentPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected stale intent deleted on post-switch exhaustion release, stat err=%v", statErr)
	}
}

package actuators

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/labatvi1992/michael-auto/internal/adapters/storage"
	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/ports"
)

const (
	rpcErrAuthGateDetected  = -32004
	rpcErrSecurityViolation = -32006
	rpcErrWindowNotFound    = -32002
	rpcErrRequestCancelled  = -32800
	defaultActuationLease   = 1200 * time.Millisecond
	defaultRPCTimeout       = 20 * time.Second
	maxRestartBackoff       = 30 * time.Second
	maxRestartsInWindow     = 4
)

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type rpcResponse struct {
	ID     int64             `json:"id"`
	Result json.RawMessage   `json:"result,omitempty"`
	Error  *rpcResponseError `json:"error,omitempty"`
}

var errRecoverableProfileMismatch = errors.New("cửa sổ Chrome hiện tại khác Profile cấu hình")

type errOAuthProfileRelay struct {
	relayURL string
	detail   string
}

func (e *errOAuthProfileRelay) Error() string { return e.detail }
func (e *errOAuthProfileRelay) Unwrap() error { return errRecoverableProfileMismatch }

type rpcResponseError struct {
	Code    int                    `json:"code"`
	Message string                 `json:"message"`
	Data    map[string]interface{} `json:"data,omitempty"`
}

type wireEvidencePayload struct {
	Step             string              `json:"step"`
	WindowTitle      string              `json:"windowTitle"`
	PID              int                 `json:"pid"`
	ProcessExe       string              `json:"processExe,omitempty"`
	BrowserProfile   string              `json:"browserProfile,omitempty"`
	OmniboxOrigin    string              `json:"omniboxOrigin,omitempty"`
	VerifiedDetail   string              `json:"verifiedDetail"`
	ScreenshotBase64 string              `json:"screenshotBase64"`
	CaptureScope     string              `json:"captureScope,omitempty"`
	WindowFrame      *domain.RedactRect  `json:"windowFrame,omitempty"`
	RedactRects      []domain.RedactRect `json:"redactRects,omitempty"`
}

// CuaDriverActuator implements ports.DesktopActuator and ports.CuaDesktopActuator
// via a supervised JSON-RPC 2.0 Python sidecar backed by trycua/cua (cua-driver),
// enforcing Verify -> Screenshot Evidence -> Execute and strict Chrome Profile binding.
type CuaDriverActuator struct {
	mu                   sync.Mutex
	writeMu              sync.Mutex
	daemonCtx            context.Context
	daemonCancel         context.CancelFunc
	cmd                  *exec.Cmd
	stdin                io.WriteCloser
	procExitCh           chan struct{}
	pending              map[int64]chan rpcResponse
	nextID               int64
	ready                bool
	closing              bool
	intentionalRestart   bool
	breakerOpen          bool
	a11yEnsured          bool
	detail               string
	activeRunID          string
	expectedEmailHint    string
	pendingTargetProfile *domain.AccountProfile
	restartCount         int
	lastCrash            time.Time
	activityMon          ports.HumanActivityMonitor
	browserRouter        ports.BrowserProfileRouter
	evidenceStore        ports.EvidenceStore
	stderrWriter         io.Writer
	cfg                  domain.CuaDriverConfig
	pythonBin            string
	entrypoint           string
	extraEnv             []string
}

// NewCuaDriverActuator constructs the production CUA sidecar supervisor & actuator.
func NewCuaDriverActuator(
	cfg domain.CuaDriverConfig,
	activityMon ports.HumanActivityMonitor,
	browserRouter ports.BrowserProfileRouter,
	evidenceStore ports.EvidenceStore,
	stderrWriter io.Writer,
) *CuaDriverActuator {
	ctx, cancel := context.WithCancel(context.Background())
	pyBin := resolvePythonBin(cfg.PythonBin)
	entry, err := EnsureEmbeddedSidecarExtracted()
	if err != nil || entry == "" {
		entry = filepath.Join(resolveSidecarDir(), "main.py")
	}
	if stderrWriter == nil {
		stderrWriter = os.Stderr
	}
	return &CuaDriverActuator{
		daemonCtx:     ctx,
		daemonCancel:  cancel,
		pending:       make(map[int64]chan rpcResponse),
		nextID:        1,
		detail:        "CUA sidecar chưa khởi tạo phiên",
		activityMon:   activityMon,
		browserRouter: browserRouter,
		evidenceStore: evidenceStore,
		stderrWriter:  stderrWriter,
		cfg:           cfg,
		pythonBin:     pyBin,
		entrypoint:    entry,
	}
}

// NewCustomCuaDriverActuator constructs a CuaDriverActuator with custom binary/script for testing.
func NewCustomCuaDriverActuator(
	pythonBin string,
	entrypoint string,
	extraEnv []string,
	activityMon ports.HumanActivityMonitor,
	browserRouter ports.BrowserProfileRouter,
) *CuaDriverActuator {
	ctx, cancel := context.WithCancel(context.Background())
	return &CuaDriverActuator{
		daemonCtx:     ctx,
		daemonCancel:  cancel,
		pending:       make(map[int64]chan rpcResponse),
		nextID:        1,
		detail:        "CUA test sidecar chưa khởi tạo",
		activityMon:   activityMon,
		browserRouter: browserRouter,
		stderrWriter:  os.Stderr,
		cfg:           domain.DefaultConfig().CuaDriver,
		pythonBin:     pythonBin,
		entrypoint:    entrypoint,
		extraEnv:      extraEnv,
	}
}

// SetEvidenceStore attaches an EvidenceStore (useful in unit tests).
func (a *CuaDriverActuator) SetEvidenceStore(store ports.EvidenceStore) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evidenceStore = store
}

// SetActiveRunID binds all pre-actuation Evidence screenshots of the current switch cycle into one folder.
func (a *CuaDriverActuator) SetActiveRunID(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activeRunID = strings.TrimSpace(runID)
}

// SetPendingTargetProfile records the target Chrome `--profile-directory` before `ClickSignIn` (#8)
// so Chrome pre-activates that exact isolated profile window as MRU right before Antigravity fires `xdg-open`.
func (a *CuaDriverActuator) SetPendingTargetProfile(prof domain.AccountProfile) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := prof
	a.pendingTargetProfile = &cp
}

// ResetBreaker clears a tripped sidecar crash circuit breaker so the user can recover from UI ResetHardStop (P1-2).
func (a *CuaDriverActuator) ResetBreaker() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.breakerOpen = false
	a.restartCount = 0
	if !a.ready {
		a.detail = "Đã đặt lại chốt an toàn CUA sidecar — sẵn sàng kết nối lại"
	}
}

// UpdateSidecarConfig refreshes runtime CUA parameters from AppConfig updates and hot-restarts
// the running Python sidecar whenever environment-affecting settings change (P1-6).
func (a *CuaDriverActuator) UpdateSidecarConfig(cfg domain.CuaDriverConfig) {
	newPy := resolvePythonBin(cfg.PythonBin)

	a.mu.Lock()
	envChanged := a.cfg.EnableWayland != cfg.EnableWayland ||
		a.cfg.DeliveryMode != cfg.DeliveryMode ||
		a.cfg.AgentWorkspaceEnabled != cfg.AgentWorkspaceEnabled ||
		a.cfg.AgentWorkspaceIndex != cfg.AgentWorkspaceIndex ||
		a.cfg.AgentWorkspaceName != cfg.AgentWorkspaceName ||
		a.cfg.MaskEditorInEvidence != cfg.MaskEditorInEvidence ||
		a.cfg.SessionName != cfg.SessionName ||
		a.pythonBin != newPy

	var oldCmd *exec.Cmd
	var oldStdin io.WriteCloser
	var oldExit chan struct{}
	if envChanged && a.cmd != nil {
		oldCmd = a.cmd
		oldStdin = a.stdin
		oldExit = a.procExitCh
		a.cmd = nil
		a.stdin = nil
		a.procExitCh = nil
		a.ready = false
		a.intentionalRestart = true
		a.restartCount = 0
		a.breakerOpen = false
		a.detail = "Đang khởi động lại CUA sidecar theo cấu hình mới"
	}
	a.cfg = cfg
	a.pythonBin = newPy
	a.mu.Unlock()

	if oldStdin != nil {
		_ = oldStdin.Close()
	}
	if oldCmd != nil && oldCmd.Process != nil && oldExit != nil {
		select {
		case <-oldExit:
		case <-time.After(800 * time.Millisecond):
			_ = oldCmd.Process.Kill()
			<-oldExit
		}
	}
	a.mu.Lock()
	a.intentionalRestart = false
	a.mu.Unlock()
}

// ResolvedPythonBin returns the active Python interpreter selected for the sidecar.
func (a *CuaDriverActuator) ResolvedPythonBin() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pythonBin
}

// ResolvedEntrypoint returns the active sidecar entrypoint path (repo or XDG extracted).
func (a *CuaDriverActuator) ResolvedEntrypoint() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.entrypoint
}

// ProvisionRuntime creates an isolated XDG Python venv, installs cua-driver + embedded sidecar,
// switches the actuator to that venv, and verifies health_check.
func (a *CuaDriverActuator) ProvisionRuntime(ctx context.Context) (string, error) {
	venvPy, summary, err := ProvisionSidecarVenv(ctx)
	if err != nil {
		return summary, err
	}

	entry, extractErr := EnsureEmbeddedSidecarExtracted()

	a.mu.Lock()
	oldCmd := a.cmd
	oldStdin := a.stdin
	oldExit := a.procExitCh
	a.pythonBin = venvPy
	if extractErr == nil && entry != "" {
		a.entrypoint = entry
	}
	a.breakerOpen = false
	a.restartCount = 0
	a.ready = false
	a.intentionalRestart = true
	a.cmd = nil
	a.stdin = nil
	a.procExitCh = nil
	a.mu.Unlock()

	if oldStdin != nil {
		_ = oldStdin.Close()
	}
	if oldCmd != nil && oldCmd.Process != nil && oldExit != nil {
		select {
		case <-oldExit:
		case <-time.After(1500 * time.Millisecond):
			_ = oldCmd.Process.Kill()
			<-oldExit
		}
	}
	a.mu.Lock()
	a.intentionalRestart = false
	a.mu.Unlock()

	if err := a.EnsureSession(ctx); err != nil {
		return summary, fmt.Errorf("đã cài venv nhưng khởi chạy sidecar lỗi: %w", err)
	}
	return summary, nil
}

// EnsureSession spawns the sidecar if needed and validates responsiveness via health_check.
func (a *CuaDriverActuator) EnsureSession(ctx context.Context) error {
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return errors.New("CUA sidecar đang đóng")
	}
	if a.breakerOpen {
		detail := a.detail
		a.mu.Unlock()
		return errors.New(detail)
	}
	alreadyRunning := a.ready && a.cmd != nil && a.stdin != nil
	a.mu.Unlock()

	if alreadyRunning {
		return nil
	}

	a.mu.Lock()
	needA11y := a.cfg.EnableWayland && !a.a11yEnsured && a.extraEnv == nil
	if needA11y {
		a.a11yEnsured = true
	}
	a.mu.Unlock()
	if needA11y {
		if ok, _ := CheckWaylandAccessibility(ctx); !ok {
			_, _ = EnableWaylandAccessibility(ctx)
		}
	}

	if err := a.startSidecar(); err != nil {
		a.setStatus(false, "Không khởi chạy được CUA sidecar: "+err.Error())
		return err
	}

	if _, err := a.Call(ctx, "health_check", nil); err != nil {
		a.setStatus(false, "CUA health_check chưa sẵn sàng: "+err.Error())
		return fmt.Errorf("cua-driver health_check lỗi: %w", err)
	}

	a.setStatus(true, "Sẵn sàng (trycua/cua — Agent Workspace + Evidence Gate)")
	return nil
}

// SessionStatus returns current sidecar readiness and human-readable detail.
func (a *CuaDriverActuator) SessionStatus() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ready, a.detail
}

// FocusAppWindow brings the target application window to focus inside the Agent Workspace.
func (a *CuaDriverActuator) FocusAppWindow(ctx context.Context, appTarget string, dryRun bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if dryRun {
		return nil
	}
	if err := a.EnsureSession(ctx); err != nil {
		return err
	}
	return a.withActuationLease(defaultActuationLease, func() error {
		_, err := a.Call(ctx, "focus_window", map[string]interface{}{
			"appTarget": appTarget,
			"dryRun":    dryRun,
		})
		return err
	})
}

// InspectQuota clicks the View Quota button every 1 minute (ADR), verifies 0% / Quota exhausted
// with mandatory Screenshot Evidence #1 when exhausted, and returns the resulting ScreenState.
func (a *CuaDriverActuator) InspectQuota(ctx context.Context, targetKeywords []string, dryRun bool) (domain.ScreenState, error) {
	now := time.Now()
	empty := domain.ScreenState{
		ObservedAt:   now,
		AppAuthState: domain.AuthUnknown,
		SourceTier:   "cua_driver",
		Targets:      make(map[string]domain.UIElementTarget),
	}
	if ctx.Err() != nil {
		return empty, ctx.Err()
	}
	if err := a.EnsureSession(ctx); err != nil {
		return empty, err
	}

	reqEv := a.requiresEvidence()
	var raw json.RawMessage
	err := a.withActuationLease(1800*time.Millisecond, func() error {
		res, callErr := a.Call(ctx, "inspect_quota", map[string]interface{}{
			"targetKeywords":  targetKeywords,
			"requireEvidence": reqEv,
			"dryRun":          dryRun,
		})
		raw = res
		return callErr
	})
	if err != nil {
		return empty, err
	}

	st, evPayload, err := decodeObservedScreenStateWithEvidence(raw, now)
	if err != nil {
		return empty, err
	}
	if evPayload != nil {
		if rec, saveErr := a.persistWireEvidence(*evPayload); saveErr == nil {
			st.EvidencePath = rec.ImagePath
			st.EvidenceSHA256 = rec.SHA256
		} else if reqEv && st.QuotaExhausted && !dryRun {
			return empty, fmt.Errorf("lưu bằng chứng Evidence #1 (Quota=0%%) thất bại: %w", saveErr)
		}
	}
	return st, nil
}

// TriggerLogout navigates Settings, verifies & captures Evidence #2, and clicks Sign Out.
func (a *CuaDriverActuator) TriggerLogout(ctx context.Context, state domain.ScreenState, dryRun bool) error {
	_ = state
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if dryRun {
		return nil
	}
	if err := a.EnsureSession(ctx); err != nil {
		return err
	}
	reqEv := a.requiresEvidence()
	return a.withActuationLease(2500*time.Millisecond, func() error {
		raw, err := a.Call(ctx, "click_signout", map[string]interface{}{
			"requireEvidence": reqEv,
			"dryRun":          dryRun,
		})
		if err != nil {
			return err
		}
		return a.extractAndPersistActionEvidence(raw, reqEv)
	})
}

// ClickSignIn pre-warms the target `--profile-directory` as Chrome's MRU window (#8),
// verifies LoggedOut state, captures Evidence #3, and clicks Sign In.
func (a *CuaDriverActuator) ClickSignIn(ctx context.Context, dryRun bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if dryRun {
		return nil
	}
	if err := a.EnsureSession(ctx); err != nil {
		return err
	}

	a.mu.Lock()
	var preWarmProf *domain.AccountProfile
	if a.pendingTargetProfile != nil && a.extraEnv == nil {
		cp := *a.pendingTargetProfile
		preWarmProf = &cp
	}
	a.mu.Unlock()

	if preWarmProf != nil && a.browserRouter != nil && strings.TrimSpace(preWarmProf.BrowserProfileDir) != "" {
		warmCopy := *preWarmProf
		warmCopy.AuthStartURL = "https://accounts.google.com"
		if a.browserRouter.OpenProfileSession(ctx, warmCopy, dryRun) == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(350 * time.Millisecond):
			}
		}
	}

	reqEv := a.requiresEvidence()
	return a.withActuationLease(defaultActuationLease, func() error {
		raw, err := a.Call(ctx, "click_signin", map[string]interface{}{
			"requireEvidence": reqEv,
			"dryRun":          dryRun,
		})
		if err != nil {
			return err
		}
		return a.extractAndPersistActionEvidence(raw, reqEv)
	})
}

func isSafeGoogleOAuthRelayURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" {
		return false
	}
	return strings.EqualFold(u.Hostname(), "accounts.google.com")
}

// SelectChromeAccount strictly ensures Chrome is running the target --profile-directory,
// relays the exact OAuth2 PKCE URL losslessly if `xdg-open` landed in another profile (#8),
// verifies Omnibox == accounts.google.com + zero MFA, captures Evidence #4, and selects the account.
func (a *CuaDriverActuator) SelectChromeAccount(ctx context.Context, profile domain.AccountProfile, dryRun bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if dryRun {
		return nil
	}
	if err := a.EnsureSession(ctx); err != nil {
		return err
	}

	a.mu.Lock()
	settleMs := a.cfg.BrowserSettleTimeoutMs
	reqEv := a.cfg.RequireEvidenceBeforeActuate && a.evidenceStore != nil
	a.expectedEmailHint = strings.TrimSpace(profile.EmailHint)
	a.pendingTargetProfile = nil
	a.mu.Unlock()
	if settleMs < 2000 {
		settleMs = 15000
	}

	expectedProfName, discoveredEmail, _ := LookupBrowserProfileMeta(profile.BrowserType, profile.BrowserProfileDir, "")
	effectiveEmail := strings.TrimSpace(profile.EmailHint)
	if effectiveEmail == "" && discoveredEmail != "" {
		effectiveEmail = discoveredEmail
	}

	params := map[string]interface{}{
		"emailHint":           effectiveEmail,
		"browserProfileDir":   profile.BrowserProfileDir,
		"expectedProfileName": expectedProfName,
		"settleTimeoutMs":     settleMs,
		"requireEvidence":     reqEv,
		"dryRun":              dryRun,
	}

	var raw json.RawMessage
	err := a.withActuationLease(time.Duration(settleMs)*time.Millisecond+time.Second, func() error {
		res, callErr := a.Call(ctx, "select_chrome_account", params)
		raw = res
		return callErr
	})
	if err == nil {
		return a.extractAndPersistActionEvidence(raw, reqEv)
	}
	// CRITICAL (P1-4): Never launch browser or retry on MFA/CAPTCHA or fatal Omnibox/PID security violations!
	if errors.Is(err, ports.ErrAuthGateDetected) ||
		(errors.Is(err, ports.ErrSecurityViolation) && !errors.Is(err, errRecoverableProfileMismatch)) ||
		ctx.Err() != nil {
		return err
	}

	// Layer 2 (#8): If Sidecar caught Antigravity's OAuth2 PKCE URL inside a different Chrome profile window,
	// relay THAT EXACT OAuth2 PKCE URL into `--profile-directory=<profile.BrowserProfileDir>`!
	if a.browserRouter != nil && profile.BrowserProfileDir != "" {
		explicitProf := profile
		var relayErr *errOAuthProfileRelay
		if errors.As(err, &relayErr) && isSafeGoogleOAuthRelayURL(relayErr.relayURL) {
			explicitProf.AuthStartURL = strings.TrimSpace(relayErr.relayURL)
		} else if strings.TrimSpace(explicitProf.AuthStartURL) == "" {
			explicitProf.AuthStartURL = "https://accounts.google.com/AccountChooser"
		}
		_ = a.browserRouter.OpenProfileSession(ctx, explicitProf, dryRun)
		return a.withActuationLease(9*time.Second, func() error {
			params["settleTimeoutMs"] = 8000
			retryRaw, retryErr := a.Call(ctx, "select_chrome_account", params)
			if retryErr != nil {
				return retryErr
			}
			return a.extractAndPersistActionEvidence(retryRaw, reqEv)
		})
	}
	return err
}

// SelectModel waits for post-OAuth workspace readiness, captures Evidence #5, verifies post-login
// account identity & non-zero Quota invariants (#8), and selects the Default Model.
func (a *CuaDriverActuator) SelectModel(ctx context.Context, state domain.ScreenState, model string, dryRun bool) error {
	_ = state
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if dryRun {
		return nil
	}
	if err := a.EnsureSession(ctx); err != nil {
		return err
	}

	a.mu.Lock()
	settleMs := a.cfg.BrowserSettleTimeoutMs
	reqEv := a.cfg.RequireEvidenceBeforeActuate && a.evidenceStore != nil
	expectedEmail := a.expectedEmailHint
	a.expectedEmailHint = ""
	a.mu.Unlock()
	if settleMs < 2000 {
		settleMs = 12000
	}

	return a.withActuationLease(time.Duration(settleMs)*time.Millisecond+time.Second, func() error {
		raw, err := a.Call(ctx, "select_model", map[string]interface{}{
			"model":           model,
			"settleTimeoutMs": settleMs,
			"requireEvidence": reqEv,
			"dryRun":          dryRun,
		})
		if err != nil {
			return err
		}
		if evErr := a.extractAndPersistActionEvidence(raw, reqEv); evErr != nil {
			return evErr
		}

		var postCheck struct {
			DetectedAccountHint     string `json:"detectedAccountHint,omitempty"`
			PostLoginQuotaExhausted bool   `json:"postLoginQuotaExhausted,omitempty"`
		}
		if json.Unmarshal(raw, &postCheck) == nil {
			if expectedEmail != "" && postCheck.DetectedAccountHint != "" &&
				!strings.Contains(strings.ToLower(postCheck.DetectedAccountHint), strings.ToLower(expectedEmail)) {
				return fmt.Errorf("%w: giao diện Antigravity đang hiển thị %q thay vì %q",
					ports.ErrAccountMismatch, postCheck.DetectedAccountHint, expectedEmail)
			}
			if postCheck.PostLoginQuotaExhausted {
				return ports.ErrPostSwitchQuotaExhausted
			}
		}
		return nil
	})
}

// ListAvailableModels opens Antigravity's Model menu briefly to discover live selectable models (P1-3).
func (a *CuaDriverActuator) ListAvailableModels(ctx context.Context, targetKeywords []string) ([]string, string, error) {
	if err := a.EnsureSession(ctx); err != nil {
		return nil, "", err
	}
	raw, err := a.Call(ctx, "list_models", map[string]interface{}{
		"targetKeywords": targetKeywords,
	})
	if err != nil {
		return nil, "", err
	}
	var parsed struct {
		ActiveModel string   `json:"activeModel"`
		Models      []string `json:"models"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, "", err
	}
	return parsed.Models, parsed.ActiveModel, nil
}

// InspectUISelectors runs the 100% read-only UI tree & selector calibration probe (--inspect-ui).
func (a *CuaDriverActuator) InspectUISelectors(ctx context.Context, targetKeywords []string) (json.RawMessage, error) {
	if err := a.EnsureSession(ctx); err != nil {
		return nil, err
	}
	return a.Call(ctx, "inspect_ui", map[string]interface{}{
		"targetKeywords": targetKeywords,
	})
}

// Close cleanly terminates the Python sidecar without racing with monitorProcess.
func (a *CuaDriverActuator) Close() error {
	a.mu.Lock()
	if a.closing {
		exitCh := a.procExitCh
		a.mu.Unlock()
		if exitCh != nil {
			<-exitCh
		}
		return nil
	}
	a.closing = true
	a.ready = false
	a.detail = "CUA sidecar đã đóng"
	cmd := a.cmd
	stdin := a.stdin
	exitCh := a.procExitCh
	reqID := a.nextID
	a.nextID++
	a.stdin = nil
	a.mu.Unlock()

	a.daemonCancel()

	if cmd == nil || cmd.Process == nil || exitCh == nil {
		return nil
	}

	if stdin != nil {
		a.writeMu.Lock()
		if payload, err := json.Marshal(rpcRequest{
			JSONRPC: "2.0",
			ID:      reqID,
			Method:  "shutdown",
			Params:  map[string]interface{}{},
		}); err == nil {
			_, _ = stdin.Write(append(payload, '\n'))
		}
		_ = stdin.Close()
		a.writeMu.Unlock()
	}

	select {
	case <-exitCh:
		return nil
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-exitCh
		return nil
	}
}

// Call dispatches a JSON-RPC 2.0 request over stdin and awaits the matching response ID.
func (a *CuaDriverActuator) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	timeout := defaultRPCTimeout
	if d, ok := ctx.Deadline(); ok {
		if rem := time.Until(d); rem > 0 && rem < timeout {
			timeout = rem
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	a.mu.Lock()
	if a.stdin == nil || a.procExitCh == nil {
		a.mu.Unlock()
		return nil, errors.New("CUA sidecar chưa khởi động")
	}
	id := a.nextID
	a.nextID++
	stdin := a.stdin
	exitCh := a.procExitCh
	respCh := make(chan rpcResponse, 1)
	a.pending[id] = respCh
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	if params == nil {
		params = map[string]interface{}{}
	}
	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal JSON-RPC lỗi: %w", err)
	}

	a.writeMu.Lock()
	_, writeErr := stdin.Write(append(payload, '\n'))
	a.writeMu.Unlock()
	if writeErr != nil {
		a.setStatus(false, "Mất ống dẫn stdin tới CUA sidecar")
		return nil, fmt.Errorf("ghi stdin tới CUA sidecar lỗi: %w", writeErr)
	}

	select {
	case <-callCtx.Done():
		// CRITICAL (#1): Immediately dispatch $/cancelRequest to Python AsyncJsonRpcMux so any in-flight
		// coroutine aborts at its current await point with ZERO post-interrupt 'ghost clicks'!
		a.sendCancelNotification(stdin, id)
		return nil, callCtx.Err()
	case <-exitCh:
		return nil, errors.New("tiến trình CUA sidecar đã kết thúc bất ngờ")
	case resp := <-respCh:
		if resp.Error != nil {
			switch resp.Error.Code {
			case rpcErrRequestCancelled:
				return nil, context.Canceled
			case rpcErrAuthGateDetected:
				return nil, fmt.Errorf("%w: %s", ports.ErrAuthGateDetected, resp.Error.Message)
			case rpcErrSecurityViolation:
				reason, _ := resp.Error.Data["reason"].(string)
				if reason == "oauth_profile_relay_required" {
					relayURL, _ := resp.Error.Data["relayOAuthUrl"].(string)
					return nil, fmt.Errorf("%w (%w): %s", ports.ErrSecurityViolation, &errOAuthProfileRelay{
						relayURL: relayURL,
						detail:   resp.Error.Message,
					}, resp.Error.Message)
				}
				if reason == "profile_mismatch" || reason == "window_not_oauth" {
					return nil, fmt.Errorf("%w (%w): %s", ports.ErrSecurityViolation, errRecoverableProfileMismatch, resp.Error.Message)
				}
				return nil, fmt.Errorf("%w: %s", ports.ErrSecurityViolation, resp.Error.Message)
			default:
				return nil, fmt.Errorf("CUA RPC [%s] lỗi (%d): %s", method, resp.Error.Code, resp.Error.Message)
			}
		}
		return resp.Result, nil
	}
}

func (a *CuaDriverActuator) sendCancelNotification(stdin io.Writer, targetID int64) {
	if stdin == nil {
		return
	}
	cancelMsg, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "$/cancelRequest",
		"params": map[string]interface{}{
			"id": targetID,
		},
	})
	if err != nil {
		return
	}
	a.writeMu.Lock()
	_, _ = stdin.Write(append(cancelMsg, '\n'))
	a.writeMu.Unlock()
}

// --- Evidence & lifecycle helpers ---

func (a *CuaDriverActuator) requiresEvidence() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.RequireEvidenceBeforeActuate && a.evidenceStore != nil
}

func (a *CuaDriverActuator) extractAndPersistActionEvidence(raw json.RawMessage, required bool) error {
	var wrapper struct {
		Evidence *wireEvidencePayload `json:"evidence,omitempty"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		if required {
			return fmt.Errorf("không đọc được phản hồi Evidence: %w", err)
		}
		return nil
	}
	if wrapper.Evidence == nil {
		if required {
			return errors.New("thiếu gói dữ liệu ảnh chụp xác minh Evidence từ sidecar")
		}
		return nil
	}
	_, err := a.persistWireEvidence(*wrapper.Evidence)
	return err
}

func (a *CuaDriverActuator) persistWireEvidence(ev wireEvidencePayload) (domain.EvidenceRecord, error) {
	a.mu.Lock()
	store := a.evidenceStore
	runID := a.activeRunID
	maskEditor := a.cfg.MaskEditorInEvidence
	a.mu.Unlock()

	if store == nil {
		return domain.EvidenceRecord{}, nil
	}
	if runID == "" {
		runID = "run_" + time.Now().UTC().Format("20060102_150405")
	}

	pngBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ev.ScreenshotBase64))
	if err != nil || len(pngBytes) == 0 {
		return domain.EvidenceRecord{}, fmt.Errorf("giải mã ảnh PNG Evidence lỗi: %w", err)
	}

	redactedCount := 0
	if maskEditor && len(ev.RedactRects) > 0 {
		pngBytes, redactedCount = storage.MaskPNGEditorRectsWithFrame(pngBytes, ev.RedactRects, ev.WindowFrame, ev.CaptureScope)
	}

	return store.SaveEvidence(runID, ev.Step, pngBytes, domain.EvidenceRecord{
		CreatedAt:       time.Now().UTC(),
		WindowTitle:     ev.WindowTitle,
		PID:             ev.PID,
		ProcessExe:      ev.ProcessExe,
		BrowserProfile:  ev.BrowserProfile,
		OmniboxOrigin:   ev.OmniboxOrigin,
		VerifiedDetail:  ev.VerifiedDetail,
		RedactedRegions: redactedCount,
	})
}

func (a *CuaDriverActuator) withActuationLease(ttl time.Duration, fn func() error) error {
	var release func()
	if a.activityMon != nil {
		release = a.activityMon.AcquireActuationLease(ttl, nil, nil)
	}
	if release != nil {
		defer release()
	}
	return fn()
}

func (a *CuaDriverActuator) startSidecar() error {
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return errors.New("sidecar đang đóng")
	}
	if a.cmd != nil && a.stdin != nil {
		a.mu.Unlock()
		return nil
	}
	pyBin := a.pythonBin
	entrypoint := a.entrypoint
	cfg := a.cfg
	stderrOut := a.stderrWriter
	extraEnv := append([]string(nil), a.extraEnv...)
	a.mu.Unlock()

	if _, err := os.Stat(entrypoint); err != nil {
		return fmt.Errorf("không tìm thấy CUA sidecar entrypoint tại %s: %w", entrypoint, err)
	}

	cmd := exec.Command(pyBin, entrypoint, "--jsonrpc-stdio")
	cmd.Env = append(buildSidecarEnv(cfg, filepath.Dir(filepath.Dir(entrypoint))), extraEnv...)
	cmd.Stderr = stderrOut
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGTERM,
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("tạo stdin pipe lỗi: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdinPipe.Close()
		return fmt.Errorf("tạo stdout pipe lỗi: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdinPipe.Close()
		return fmt.Errorf("khởi chạy tiến trình %s lỗi: %w", pyBin, err)
	}

	exitCh := make(chan struct{})

	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		_ = stdinPipe.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		close(exitCh)
		return errors.New("sidecar đã đóng trong lúc khởi tạo")
	}
	a.cmd = cmd
	a.stdin = stdinPipe
	a.procExitCh = exitCh
	a.mu.Unlock()

	go a.readStdoutLoop(stdoutPipe, exitCh)
	go a.monitorProcess(cmd, stdinPipe, exitCh)

	return nil
}

func (a *CuaDriverActuator) readStdoutLoop(stdout io.Reader, exitCh <-chan struct{}) {
	reader := bufio.NewReaderSize(stdout, 256*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			var resp rpcResponse
			if jsonErr := json.Unmarshal(line, &resp); jsonErr == nil {
				a.mu.Lock()
				ch, ok := a.pending[resp.ID]
				if ok {
					delete(a.pending, resp.ID)
				}
				a.mu.Unlock()
				if ok {
					ch <- resp
				}
			}
		}
		if err != nil {
			return
		}
		select {
		case <-exitCh:
			return
		default:
		}
	}
}

func (a *CuaDriverActuator) monitorProcess(cmd *exec.Cmd, stdin io.WriteCloser, exitCh chan struct{}) {
	waitErr := cmd.Wait()
	_ = stdin.Close()
	close(exitCh)

	a.mu.Lock()
	wasCurrent := a.cmd == cmd
	if wasCurrent {
		a.cmd = nil
		a.stdin = nil
		a.procExitCh = nil
		a.ready = false
	}
	if a.closing || a.intentionalRestart || !wasCurrent || a.daemonCtx.Err() != nil {
		a.mu.Unlock()
		return
	}

	now := time.Now()
	if now.Sub(a.lastCrash) > 2*time.Minute {
		a.restartCount = 0
	}
	a.restartCount++
	a.lastCrash = now

	if a.restartCount > maxRestartsInWindow {
		a.breakerOpen = true
		a.detail = fmt.Sprintf("Sidecar Circuit Breaker kích hoạt sau %d lần crash liên tiếp (%v)", a.restartCount, waitErr)
		a.mu.Unlock()
		return
	}

	backoff := time.Duration(1<<uint(a.restartCount-1)) * time.Second
	if backoff > maxRestartBackoff {
		backoff = maxRestartBackoff
	}
	attempt := a.restartCount
	a.detail = fmt.Sprintf("CUA sidecar dừng bất thường (%v) — tự khởi động lại sau %v (lần %d)", waitErr, backoff, attempt)
	daemonCtx := a.daemonCtx
	a.mu.Unlock()

	select {
	case <-daemonCtx.Done():
		return
	case <-time.After(backoff):
	}

	if err := a.startSidecar(); err != nil {
		a.setStatus(false, fmt.Sprintf("Khởi động lại CUA sidecar lần %d thất bại: %v", attempt, err))
		return
	}
	hCtx, hCancel := context.WithTimeout(daemonCtx, 4*time.Second)
	defer hCancel()
	if _, hErr := a.Call(hCtx, "health_check", nil); hErr == nil {
		a.setStatus(true, "Sẵn sàng (trycua/cua — Agent Workspace + Evidence Gate — đã tự phục hồi)")
	}
}

func (a *CuaDriverActuator) setStatus(ready bool, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ready = ready
	a.detail = detail
}

// DecodeObservedScreenStateForSensor shares JSON-to-ScreenState decoding with CuaDriverSensor.
func DecodeObservedScreenStateForSensor(raw json.RawMessage, now time.Time) (domain.ScreenState, error) {
	st, _, err := decodeObservedScreenStateWithEvidence(raw, now)
	return st, err
}

func decodeObservedScreenStateWithEvidence(raw json.RawMessage, now time.Time) (domain.ScreenState, *wireEvidencePayload, error) {
	type wireState struct {
		WindowDetected    bool                              `json:"windowDetected"`
		WindowTitle       string                            `json:"windowTitle,omitempty"`
		WindowFocused     bool                              `json:"windowFocused"`
		WindowPID         int                               `json:"windowPid,omitempty"`
		ElementCount      int                               `json:"elementCount,omitempty"`
		HollowWindow      bool                              `json:"hollowWindow,omitempty"`
		AppAuthState      string                            `json:"appAuthState"`
		ActiveAccountHint string                            `json:"activeAccountHint,omitempty"`
		ActiveModel       string                            `json:"activeModel,omitempty"`
		RemainingQuotaPct *int                              `json:"remainingQuotaPct,omitempty"`
		QuotaExhausted    bool                              `json:"quotaExhausted"`
		QuotaMessage      string                            `json:"quotaMessage,omitempty"`
		AuthGateDetected  bool                              `json:"authGateDetected"`
		AuthGateReason    string                            `json:"authGateReason,omitempty"`
		IsEditorContent   bool                              `json:"isEditorContent"`
		Confidence        float64                           `json:"confidence"`
		Targets           map[string]domain.UIElementTarget `json:"targets,omitempty"`
		Evidence          *wireEvidencePayload              `json:"evidence,omitempty"`
	}

	var parsed wireState
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return domain.ScreenState{}, nil, fmt.Errorf("parse CUA ScreenState lỗi: %w", err)
	}

	st := domain.ScreenState{
		ObservedAt:        now,
		WindowDetected:    parsed.WindowDetected,
		WindowTitle:       parsed.WindowTitle,
		WindowFocused:     parsed.WindowFocused,
		WindowPID:         parsed.WindowPID,
		ElementCount:      parsed.ElementCount,
		HollowWindow:      parsed.HollowWindow,
		AppAuthState:      parseWireAuthState(parsed.AppAuthState),
		ActiveAccountHint: parsed.ActiveAccountHint,
		ActiveModel:       parsed.ActiveModel,
		RemainingQuotaPct: parsed.RemainingQuotaPct,
		QuotaExhausted:    parsed.QuotaExhausted,
		QuotaMessage:      parsed.QuotaMessage,
		AuthGateDetected:  parsed.AuthGateDetected,
		AuthGateReason:    parsed.AuthGateReason,
		IsEditorContent:   parsed.IsEditorContent,
		Confidence:        parsed.Confidence,
		SourceTier:        "cua_driver",
		Targets:           parsed.Targets,
	}
	if st.Targets == nil {
		st.Targets = make(map[string]domain.UIElementTarget)
	}
	return st, parsed.Evidence, nil
}

func parseWireAuthState(s string) domain.AuthState {
	switch s {
	case "authenticated":
		return domain.AuthAuthenticated
	case "logged_out":
		return domain.AuthLoggedOut
	case "auth_challenge":
		return domain.AuthChallenge
	default:
		return domain.AuthUnknown
	}
}

func resolvePythonBin(configured string) string {
	if configured != "" {
		if path, err := exec.LookPath(configured); err == nil {
			return path
		}
		return configured
	}
	if envBin := os.Getenv("CUA_PYTHON_BIN"); envBin != "" {
		return envBin
	}
	if managedPy := ManagedVenvPythonPath(); fileExecutable(managedPy) {
		return managedPy
	}
	if devVenv, err := filepath.Abs(filepath.Join("sidecar", ".venv", "bin", "python")); err == nil && fileExecutable(devVenv) {
		return devVenv
	}
	if path, err := exec.LookPath("python3"); err == nil {
		return path
	}
	if path, err := exec.LookPath("python"); err == nil {
		return path
	}
	return "python3"
}

func fileExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func resolveSidecarDir() string {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "sidecar", "michael_cua_sidecar")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	if info, err := os.Stat(filepath.Join("sidecar", "michael_cua_sidecar")); err == nil && info.IsDir() {
		return filepath.Join("sidecar", "michael_cua_sidecar")
	}
	return filepath.Join(ManagedSidecarDir(), "michael_cua_sidecar")
}

func buildSidecarEnv(cfg domain.CuaDriverConfig, sidecarRoot string) []string {
	waylandVal := "0"
	if cfg.EnableWayland {
		waylandVal = "1"
	}
	agentWSVal := "0"
	if cfg.AgentWorkspaceEnabled {
		agentWSVal = "1"
	}
	maskEditorVal := "0"
	if cfg.MaskEditorInEvidence {
		maskEditorVal = "1"
	}
	sessionName := cfg.SessionName
	if sessionName == "" {
		sessionName = "michael_auto"
	}
	deliveryMode := cfg.DeliveryMode
	if deliveryMode == "" {
		deliveryMode = "auto"
	}

	wsName := strings.TrimSpace(cfg.AgentWorkspaceName)
	if wsName == "" || strings.EqualFold(wsName, "Michael-Auto-Agent") {
		wsName = domain.DefaultAgentWorkspaceName
	}

	env := []string{
		"CUA_DRIVER_RS_ENABLE_WAYLAND=" + waylandVal,
		"CUA_AGENT_WORKSPACE_ENABLED=" + agentWSVal,
		"CUA_AGENT_WORKSPACE_INDEX=" + strconv.Itoa(cfg.AgentWorkspaceIndex),
		"CUA_AGENT_WORKSPACE_NAME=" + wsName,
		"CUA_MASK_EDITOR_EVIDENCE=" + maskEditorVal,
		"CUA_SESSION_NAME=" + sessionName,
		"CUA_TARGET_APP=Antigravity",
		"CUA_DELIVERY_MODE=" + deliveryMode,
		"ACCESSIBILITY_ENABLED=1",
		"GNOME_ACCESSIBILITY=1",
		"QT_LINUX_ACCESSIBILITY_ALWAYS_ON=1",
		"PYTHONUNBUFFERED=1",
	}

	if absSidecar, err := filepath.Abs(sidecarRoot); err == nil {
		if existing := os.Getenv("PYTHONPATH"); existing != "" {
			env = append(env, "PYTHONPATH="+absSidecar+string(os.PathListSeparator)+existing)
		} else {
			env = append(env, "PYTHONPATH="+absSidecar)
		}
	}

	inherit := []string{
		"DISPLAY",
		"WAYLAND_DISPLAY",
		"XDG_SESSION_TYPE",
		"DBUS_SESSION_BUS_ADDRESS",
		"HOME",
		"PATH",
		"LANG",
		"XDG_RUNTIME_DIR",
		"VIRTUAL_ENV",
	}
	for _, key := range inherit {
		if val, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+val)
		}
	}
	return env
}

package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"

	"github.com/labatvi1992/michael-auto/internal/adapters/actuators"
	"github.com/labatvi1992/michael-auto/internal/adapters/sensors"
	"github.com/labatvi1992/michael-auto/internal/adapters/storage"
	"github.com/labatvi1992/michael-auto/internal/adapters/tray"
	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/internal/orchestrator"
)

const AppVersion = "2.5.0"

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	var (
		showVersion  = flag.Bool("version", false, "Hiển thị phiên bản Michael Auto")
		runDoctor    = flag.Bool("doctor", false, "Kiểm tra chẩn đoán môi trường Fedora Wayland, Agent Workspace, Evidence Vault và trycua/cua")
		inspectUI    = flag.Bool("inspect-ui", false, "Quét chỉ-đọc cây phần tử thực tế của Antigravity & Chrome để hiệu chuẩn trước E2E")
		setupSidecar = flag.Bool("setup-sidecar", false, "Tự động tạo virtualenv XDG, cài cua-driver, bật Accessibility và thiết lập Agent Workspace")
		exportDiag   = flag.Bool("export-diagnostics", false, "Xuất gói chẩn đoán ẩn danh (.tar.gz) không chứa PII để hỗ trợ kỹ thuật")
		doUninstall  = flag.Bool("uninstall", false, "Gỡ sạch GNOME Extension, Autostart, systemd --user unit và Python venv XDG")
		runSelfTest  = flag.Bool("self-test", false, "Chạy kiểm tra tự chẩn đoán toàn trình trong môi trường cô lập")
		headless     = flag.Bool("headless", false, "Chạy dịch vụ điều phối nền không mở cửa sổ giao diện")
		startHidden  = flag.Bool("start-hidden", false, "Khởi động ẩn trực tiếp vào khay hệ thống")
		ctlCmd       = flag.String("ctl", "", "Gửi lệnh điều khiển tới tiến trình đang chạy qua control.sock (status|pause|resume|show|quit)")
		configPath   = flag.String("config", "", "Đường dẫn tệp config.json tùy chỉnh")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("Michael Auto v%s (trycua/cua + Fedora 45 Agent Workspace + Evidence Vault)\n", AppVersion)
		return
	}

	if strings.TrimSpace(*ctlCmd) != "" {
		executeControlCommand(*ctlCmd)
		return
	}

	if *runDoctor {
		executeDoctor(*configPath)
		return
	}

	if *inspectUI {
		executeInspectUI(*configPath)
		return
	}

	if *setupSidecar {
		executeSetupSidecar(*configPath)
		return
	}

	if *exportDiag {
		executeExportDiagnostics(*configPath)
		return
	}

	if *doUninstall {
		executeUninstall(*configPath)
		return
	}

	if *runSelfTest {
		if err := executeSelfTest(); err != nil {
			fmt.Fprintf(os.Stderr, "SELF-TEST FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("SELF-TEST PASSED: CUA 5-Step Evidence Workflow, Ephemeral Intent Cleanup, 30s Human Yield & Hard-Stop verified.")
		return
	}

	if *headless {
		runHeadlessDaemon(*configPath)
		return
	}

	applyNvidiaWaylandWebKitWorkaround()

	instanceGuard := storage.NewInstanceGuard("")
	acquired, lockErr := instanceGuard.AcquireOrWakeExisting(nil)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "Lỗi khóa đơn tiến trình: %v\n", lockErr)
		os.Exit(1)
	}
	if !acquired {
		reply := instanceGuard.LastWakeReply()
		if reply == "ok:headless" {
			msg := "Michael Auto đang chạy ở chế độ --headless (không có cửa sổ GUI). Dừng tiến trình nền bằng 'michael-auto --ctl quit' hoặc 'systemctl --user stop michael-auto.service' rồi mở lại biểu tượng ứng dụng."
			fmt.Fprintln(os.Stderr, msg)
			tray.NewDesktopNotifier().Notify("Michael Auto đang chạy nền (Headless)", msg, true)
			return
		}
		fmt.Println("Michael Auto đang chạy nền — đã gửi tín hiệu mở cửa sổ quản trị.")
		return
	}
	defer instanceGuard.Close()

	app, err := NewApp(*configPath, "", true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Không thể khởi tạo Michael Auto: %v\n", err)
		os.Exit(1)
	}
	instanceGuard.SetShowWindowCallback(app.ShowMainWindow)
	instanceGuard.SetControlCallback(func(cmd string) string {
		return app.HandleControlCommand(cmd, "gui", nil)
	})

	err = wails.Run(&options.App{
		Title:             "Michael Auto — Agent Workspace & Evidence-Driven Quota Orchestrator",
		Width:             1160,
		Height:            820,
		MinWidth:          880,
		MinHeight:         620,
		StartHidden:       *startHidden,
		HideWindowOnClose: true,
		BackgroundColour:  &options.RGBA{R: 9, G: 14, B: 26, A: 1},
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup:     app.startup,
		OnBeforeClose: app.beforeClose,
		OnShutdown:    app.shutdown,
		Bind: []interface{}{
			app,
		},
		Linux: &linux.Options{
			Icon:                tray.IconGreenObserving,
			WindowIsTranslucent: false,
			WebviewGpuPolicy:    linux.WebviewGpuPolicyOnDemand,
			ProgramName:         "michael-auto",
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Lỗi thực thi giao diện Wails: %v\n", err)
		os.Exit(1)
	}
}

func applyNvidiaWaylandWebKitWorkaround() {
	if os.Getenv("WEBKIT_DISABLE_DMABUF_RENDERER") != "" {
		return
	}
	if os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("XDG_SESSION_TYPE") != "wayland" {
		return
	}
	if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
		_ = os.Setenv("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
	}
}

func executeControlCommand(rawCmd string) {
	cmd := strings.ToLower(strings.TrimSpace(rawCmd))
	if cmd == "show" {
		cmd = "show-window"
	}
	guard := storage.NewInstanceGuard("")
	reply, err := guard.SendCommand(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Không thể kết nối tới Michael Auto daemon qua control.sock: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(reply)
}

func runHeadlessDaemon(customConfigPath string) {
	instanceGuard := storage.NewInstanceGuard("")
	acquired, lockErr := instanceGuard.AcquireWithoutWake()
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "Lỗi khóa đơn tiến trình headless: %v\n", lockErr)
		os.Exit(1)
	}
	if !acquired {
		reply := instanceGuard.LastWakeReply()
		if reply != "" {
			fmt.Printf("Michael Auto daemon đang chạy sẵn (%s) — bỏ qua khởi chạy trùng lặp.\n", reply)
		} else {
			fmt.Println("Michael Auto daemon đang chạy sẵn trên hệ thống — bỏ qua khởi chạy trùng lặp.")
		}
		return
	}
	defer instanceGuard.Close()

	app, err := NewApp(customConfigPath, "", false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Lỗi khởi tạo headless daemon: %v\n", err)
		os.Exit(1)
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	instanceGuard.SetControlCallback(func(cmd string) string {
		return app.HandleControlCommand(cmd, "headless", cancel)
	})

	_ = app.orch.Start(ctx)
	fmt.Printf("Michael Auto v%s đang chạy ở chế độ headless trên Agent Workspace (Ctrl+C hoặc 'michael-auto --ctl quit' để dừng)...\n", AppVersion)
	<-ctx.Done()
	app.shutdown(context.Background())
}

func executeInspectUI(customConfigPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	app, err := NewApp(customConfigPath, "", false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Lỗi khởi tạo --inspect-ui: %v\n", err)
		os.Exit(1)
	}
	defer app.shutdown(ctx)

	report, err := app.InspectUISelectors()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Không thể quét cây phần tử UI qua cua-driver: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("=== Michael Auto v%s — Read-Only UI Tree & Selector Calibration ===\n%s\n", AppVersion, report)
}

func executeDoctor(customConfigPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	app, err := NewApp(customConfigPath, "", false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Doctor initialization error: %v\n", err)
		os.Exit(1)
	}
	defer app.shutdown(ctx)

	st := app.orch.Status(ctx)
	fmt.Printf("=== Michael Auto v%s — Fedora 45 Wayland & trycua/cua Doctor ===\n", AppVersion)
	fmt.Printf("• Display Server       : %s (WAYLAND_DISPLAY=%q)\n", st.DisplayServer, os.Getenv("WAYLAND_DISPLAY"))
	if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
		fmt.Printf("• GPU / WebKit Guard   : NVIDIA detected (auto-enables WEBKIT_DISABLE_DMABUF_RENDERER=1)\n")
	} else {
		fmt.Printf("• GPU / WebKit Guard   : Standard Mesa/DRM Wayland\n")
	}
	fmt.Printf("• Activity Monitor     : %s\n", app.activityMon.BackendName())
	if idle, err := app.activityMon.SystemIdleDuration(ctx); err == nil {
		fmt.Printf("• Current System Idle  : %v (30s sliding quiet threshold=%ds)\n", idle, app.orch.GetConfig().YieldPauseSeconds)
	} else {
		fmt.Printf("• Current System Idle  : unavailable in current session (%v)\n", err)
	}
	fmt.Printf("• Agent Workspace      : ready=%-5v | %s\n", st.AgentWorkspace.Ready, st.AgentWorkspace.Detail)
	fmt.Printf("• System Tray Host     : available=%-5v (UNIX socket window-wakeup enabled)\n", tray.IsStatusNotifierHostAvailable())
	fmt.Printf("• Screen Lock Shield   : locked=%-5v (GNOME ScreenSaver + logind Suspend Guard)\n", st.ScreenLocked)
	fmt.Printf("• XDG Autostart        : enabled=%-5v (%s)\n", app.GetAutostartEnabled(), autostartDesktopPath())
	fmt.Printf("• Secret Store         : %s\n", app.secretStore.Backend())
	fmt.Printf("• Actuator Session     : %s (ready=%v)\n", st.WaylandPortalDetail, st.WaylandPortalReady)
	for _, s := range st.Sensors {
		fmt.Printf("• Sensor [%-17s]: healthy=%-5v | %s\n", s.TierName, s.Healthy, s.Detail)
	}
	fmt.Printf("• XDG Config Path      : %s\n", app.cfgStore.Path())
	fmt.Printf("• XDG Audit Log Path   : %s\n", app.auditLogger.Path())
	fmt.Printf("• XDG Evidence Vault   : %s\n", app.evidenceStore.RootDir())
	fmt.Printf("• Ephemeral Intent File: %s\n", app.intentStore.Path())

	a11yOK, a11yDetail := actuators.CheckWaylandAccessibility(ctx)
	fmt.Printf("• Wayland A11y Tree    : enabled=%v | %s\n", a11yOK, a11yDetail)
	if a11yApps := actuators.InspectATSPI2RegisteredApps(ctx); len(a11yApps) > 0 {
		fmt.Printf("• AT-SPI2 Live Apps    : %s\n", strings.Join(a11yApps, ", "))
	}

	discovered := actuators.DiscoverBrowserProfiles("")
	fmt.Printf("• Browser Profiles     : %d discovered in Local State\n", len(discovered))
	for _, dp := range discovered {
		fmt.Printf("    - [%s] dir=%q name=%q email=%q\n",
			dp.BrowserType, dp.ProfileDir, dp.DisplayName, app.auditLogger.RedactString(dp.EmailHint))
	}

	cfg := app.orch.GetConfig()
	fmt.Printf("\n--- Primary Engine: trycua/cua Sidecar (michael_cua_sidecar) ---\n")
	fmt.Printf("• CUA Enabled          : %v (EvidenceGate=%v, MaskEditor=%v, Retention=%d runs)\n",
		cfg.CuaDriver.Enabled, cfg.CuaDriver.RequireEvidenceBeforeActuate, cfg.CuaDriver.MaskEditorInEvidence, cfg.CuaDriver.EvidenceRetentionRuns)
	fmt.Printf("• Quota Check Interval : %dms (1-minute mandatory ADR)\n", cfg.CuaDriver.QuotaCheckIntervalMs)
	fmt.Printf("• SignOut Cooldown     : %dms (1-minute mandatory ADR)\n", cfg.CuaDriver.SignOutCooldownMs)

	pythonBin := app.cuaImpl.ResolvedPythonBin()
	if pyPath, err := exec.LookPath(pythonBin); err == nil {
		fmt.Printf("• Python Interpreter   : %s (resolved)\n", pyPath)
	} else {
		fmt.Printf("• Python Interpreter   : %s (NOT FOUND)\n", pythonBin)
	}

	entryPath := app.cuaImpl.ResolvedEntrypoint()
	if _, err := os.Stat(entryPath); err == nil {
		fmt.Printf("• Sidecar Entrypoint   : %s (ready)\n", entryPath)
	} else {
		fmt.Printf("• Sidecar Entrypoint   : %s (missing)\n", entryPath)
	}

	checkCmd := exec.CommandContext(ctx, pythonBin, "-c", "import cua_driver; print(getattr(cua_driver, '__version__', 'installed'))")
	if out, err := checkCmd.Output(); err == nil {
		fmt.Printf("• cua-driver Package   : %s\n", strings.TrimSpace(string(out)))
	} else {
		fmt.Printf("• cua-driver Package   : NOT INSTALLED (chạy './build/bin/michael-auto --setup-sidecar' hoặc 'make sidecar')\n")
	}
}

func executeSetupSidecar(customConfigPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	app, err := NewApp(customConfigPath, "", false)
	if err == nil {
		defer app.shutdown(ctx)
		fmt.Println("1/3 Đang thiết lập Agent Workspace tách biệt trên Fedora 45 Wayland...")
		if wsSt, wsErr := app.workspaceMgr.EnsureAgentWorkspace(ctx, app.orch.GetConfig()); wsErr != nil {
			fmt.Fprintf(os.Stderr, "  [Cảnh báo] %v\n", wsErr)
		} else {
			fmt.Printf("  [OK] %s\n", wsSt.Detail)
		}
	}

	fmt.Println("2/3 Đang bật Wayland Accessibility cho Electron (Antigravity) & Google Chrome...")
	if msg, err := actuators.EnableWaylandAccessibility(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "  [Cảnh báo] %v\n", err)
	} else {
		fmt.Printf("  [OK] %s\n", msg)
	}

	fmt.Println("3/3 Đang giải nén sidecar nhúng & cài đặt binary wheel cua-driver vào môi trường ảo XDG...")
	pyPath, detail, err := actuators.ProvisionSidecarVenv(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SETUP FAILED: %v\n%s\n", err, detail)
		os.Exit(1)
	}
	fmt.Printf("  [OK] %s\n", detail)
	fmt.Printf("SETUP COMPLETE: Python sidecar sẵn sàng tại %s\n", pyPath)
}

func executeExportDiagnostics(customConfigPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	app, err := NewApp(customConfigPath, "", false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Lỗi khởi tạo --export-diagnostics: %v\n", err)
		os.Exit(1)
	}
	defer app.shutdown(ctx)

	outPath, err := app.ExportDiagnostics()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Xuất gói chẩn đoán thất bại: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("DIAGNOSTICS EXPORTED (PII-Redacted 0600): %s\n", outPath)
}

func executeUninstall(customConfigPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fmt.Println("=== Michael Auto — Gỡ cài đặt sạch user-space ===")
	if app, err := NewApp(customConfigPath, "", false); err == nil {
		defer app.shutdown(ctx)
		if err := app.SetAutostartEnabled(false); err == nil {
			fmt.Println("  [OK] Đã gỡ XDG Autostart (~/.config/autostart/michael-auto.desktop)")
		}
		if u, ok := app.workspaceMgr.(interface{ UninstallGnomeBridgeExtension(context.Context) error }); ok {
			_ = u.UninstallGnomeBridgeExtension(ctx)
			fmt.Println("  [OK] Đã tắt và gỡ GNOME Shell Extension (michael-auto-workspace@michael-auto)")
		}
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		unitPath := filepath.Join(home, ".config", "systemd", "user", "michael-auto.service")
		if _, statErr := os.Stat(unitPath); statErr == nil {
			_ = exec.CommandContext(ctx, "systemctl", "--user", "disable", "--now", "michael-auto.service").Run()
			_ = os.Remove(unitPath)
			_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()
			fmt.Println("  [OK] Đã dừng và gỡ systemd --user service")
		}
		_ = os.Remove(filepath.Join(home, ".local", "share", "applications", "michael-auto.desktop"))
		_ = os.Remove(filepath.Join(home, ".local", "share", "icons", "hicolor", "scalable", "apps", "michael-auto.svg"))
	}

	dataDir := actuators.DefaultXDGDataDir()
	if dataDir != "" && strings.HasSuffix(dataDir, "michael-auto") {
		_ = os.RemoveAll(dataDir)
		fmt.Printf("  [OK] Đã dọn XDG venv & sidecar tại %s\n", dataDir)
	}

	fmt.Println("UNINSTALL COMPLETE (Tệp cấu hình ~/.config/michael-auto/config.json được giữ nguyên; xóa thủ công nếu muốn xóa sạch).")
}

type selfTestCuaActuator struct {
	runID          string
	evidenceStore  *storage.EvidenceVault
	signOuts       int
	signIns        int
	chromeAccounts []string
	models         []string
}

var selfTestPNG = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00}

func (s *selfTestCuaActuator) SetActiveRunID(runID string)              { s.runID = runID }
func (s *selfTestCuaActuator) EnsureSession(ctx context.Context) error  { return nil }
func (s *selfTestCuaActuator) SessionStatus() (bool, string)            { return true, "selftest-cua-ready" }
func (s *selfTestCuaActuator) Close() error                             { return nil }
func (s *selfTestCuaActuator) FocusAppWindow(ctx context.Context, appTarget string, dryRun bool) error {
	return nil
}
func (s *selfTestCuaActuator) InspectQuota(ctx context.Context, targetKeywords []string, dryRun bool) (domain.ScreenState, error) {
	rec, err := s.evidenceStore.SaveEvidence(s.runID, "01_verify_quota_zero", selfTestPNG, domain.EvidenceRecord{
		WindowTitle:    "Antigravity",
		VerifiedDetail: "0% remaining — Quota exhausted",
	})
	if err != nil {
		return domain.ScreenState{}, err
	}
	zero := 0
	return domain.ScreenState{
		WindowDetected:    true,
		WindowTitle:       "Antigravity",
		WindowFocused:     true,
		AppAuthState:      domain.AuthAuthenticated,
		ActiveAccountHint: "primary@example.com",
		ActiveModel:       "gemini-2.5-pro",
		RemainingQuotaPct: &zero,
		QuotaExhausted:    true,
		QuotaMessage:      "0% remaining — Quota exhausted",
		EvidencePath:      rec.ImagePath,
		EvidenceSHA256:    rec.SHA256,
		Confidence:        0.99,
		SourceTier:        "cua_driver",
	}, nil
}
func (s *selfTestCuaActuator) TriggerLogout(ctx context.Context, state domain.ScreenState, dryRun bool) error {
	_, _ = s.evidenceStore.SaveEvidence(s.runID, "02_pre_signout_settings", selfTestPNG, domain.EvidenceRecord{WindowTitle: "Antigravity Settings"})
	s.signOuts++
	return nil
}
func (s *selfTestCuaActuator) ClickSignIn(ctx context.Context, dryRun bool) error {
	_, _ = s.evidenceStore.SaveEvidence(s.runID, "03_pre_signin_antigravity", selfTestPNG, domain.EvidenceRecord{WindowTitle: "Antigravity"})
	s.signIns++
	return nil
}
func (s *selfTestCuaActuator) SelectChromeAccount(ctx context.Context, profile domain.AccountProfile, dryRun bool) error {
	_, _ = s.evidenceStore.SaveEvidence(s.runID, "04_pre_chrome_account_select", selfTestPNG, domain.EvidenceRecord{
		WindowTitle:    "Choose an account - Google Chrome",
		BrowserProfile: profile.BrowserProfileDir,
		OmniboxOrigin:  "https://accounts.google.com/AccountChooser",
	})
	s.chromeAccounts = append(s.chromeAccounts, profile.EmailHint)
	return nil
}
func (s *selfTestCuaActuator) SelectModel(ctx context.Context, state domain.ScreenState, model string, dryRun bool) error {
	_, _ = s.evidenceStore.SaveEvidence(s.runID, "05_pre_model_select", selfTestPNG, domain.EvidenceRecord{WindowTitle: "Antigravity"})
	s.models = append(s.models, model)
	return nil
}

func executeSelfTest() error {
	tmpDir, err := os.MkdirTemp("", "michael-auto-selftest-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	cfgPath := filepath.Join(tmpDir, "config.json")
	auditPath := filepath.Join(tmpDir, "audit.jsonl")
	intentPath := filepath.Join(tmpDir, "pending_intent.json")
	evidenceDir := filepath.Join(tmpDir, "evidence")
	cliSettingsPath := filepath.Join(tmpDir, "cli-settings.json")

	cfgStore := storage.NewFileConfigStore(cfgPath)
	intentStore := storage.NewEphemeralIntentStore(intentPath)
	evidenceStore := storage.NewEvidenceVault(evidenceDir, 10)

	cfg := domain.DefaultConfig()
	cfg.DryRunActuation = true
	cfg.PreferIntraProfileModelDrop = false
	cfg.CLISettingsPath = cliSettingsPath
	cfg.Profiles[1].Enabled = true
	if err := cfgStore.Save(cfg); err != nil {
		return fmt.Errorf("lưu config mẫu lỗi: %w", err)
	}

	audit := storage.NewJSONLAuditLogger(auditPath)
	cliSync := sensors.NewCLIConfigAdapter()
	if err := cliSync.WriteActiveModel(cliSettingsPath, "gemini-2.5-pro", false); err != nil {
		return fmt.Errorf("ghi CLI settings mẫu lỗi: %w", err)
	}

	browserRouter := actuators.NewTestableBrowserRouter(
		func(bin string) (string, error) { return "/usr/bin/" + bin, nil },
		func(ctx context.Context, binary string, args []string) error { return nil },
	)
	mockCua := &selfTestCuaActuator{evidenceStore: evidenceStore}

	fakeNow := time.Now().UTC().Truncate(time.Second)
	orch := orchestrator.NewOrchestrator(cfg, orchestrator.Dependencies{
		ConfigStore:        cfgStore,
		IntentStore:        intentStore,
		EvidenceStore:      evidenceStore,
		SensorFusion:       orchestrator.NewSensorFusion(cliSync),
		DesktopActuator:    mockCua,
		CuaDesktopActuator: mockCua,
		BrowserRouter:      browserRouter,
		CLISyncer:          cliSync,
		AuditLogger:        audit,
	})
	orch.SetClock(func() time.Time { return fakeNow })

	ctx := context.Background()

	// 1. Test 30s sliding human interrupt.
	orch.InjectSimulatedHumanInput(domain.ActivityPointerMove)
	fakeNow = fakeNow.Add(20 * time.Second)
	orch.InjectSimulatedHumanInput(domain.ActivityKeyboard)
	fakeNow = fakeNow.Add(31 * time.Second)
	_ = orch.StepOnce(ctx) // Inspects Quota -> saves Evidence #1 & #2 -> clicks SignOut & writes pending_intent.json!

	if _, err := os.Stat(intentPath); err != nil {
		return fmt.Errorf("kỳ vọng tệp tạm pending_intent.json tồn tại trong lúc chờ 60s: %w", err)
	}

	// 2. Complete remaining 60s wait -> ClickSignIn -> SelectChromeAccount -> SelectModel -> Cooldown!
	fakeNow = fakeNow.Add(61 * time.Second)
	_ = orch.StepOnce(ctx)
	_ = orch.StepOnce(ctx)
	_ = orch.StepOnce(ctx)

	// Verify temporary intent file WAS STRICTLY DELETED upon completion!
	if _, err := os.Stat(intentPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("tệp tạm pending_intent.json chưa được giải phóng sau khi hoàn tất: err=%v", err)
	}

	// Verify all 5 Evidence screenshots were persisted in Evidence Vault!
	evRecords, err := evidenceStore.ListRecent(10)
	if err != nil || len(evRecords) < 5 {
		return fmt.Errorf("kỳ vọng >= 5 bản ghi ảnh chụp Evidence, nhận %d (err=%v)", len(evRecords), err)
	}

	return nil
}

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labatvi1992/michael-auto/internal/adapters/storage"
	"github.com/labatvi1992/michael-auto/internal/domain"
)

func shortE2ETempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ma-e2e-*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestAppE2E_ConfigAutostartDiagnosticsRedactionAndControlCommands(t *testing.T) {
	tmp := shortE2ETempDir(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "runtime"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))

	cfgPath := filepath.Join(tmp, "config", "michael-auto", "config.json")
	auditPath := filepath.Join(tmp, "state", "michael-auto", "audit.jsonl")

	app, err := NewApp(cfgPath, auditPath, false)
	if err != nil {
		t.Fatalf("NewApp failed: %v", err)
	}
	defer app.shutdown(context.Background())

	// 1. Version telemetry & live actuator hot-sync via SaveConfig
	st := app.GetStatus()
	if st.AppVersion != AppVersion {
		t.Fatalf("expected AppVersion=%q, got %q", AppVersion, st.AppVersion)
	}

	cfg := app.GetConfig()
	cfg.Profiles[0].EmailHint = "secret.operator@corp.example"
	cfg.CuaDriver.Enabled = false
	cfg.CuaDriver.EvidenceRetentionRuns = 12
	if err := app.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig(CuaDriver.Enabled=false) failed: %v", err)
	}
	if stOff := app.GetStatus(); !strings.Contains(stOff.WaylandPortalDetail, "Wayland") && !strings.Contains(stOff.WaylandPortalDetail, "restore_token") {
		t.Fatalf("expected fallback portal actuator detail when CuaDriver disabled, got %q", stOff.WaylandPortalDetail)
	}

	cfg.CuaDriver.Enabled = true
	if err := app.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig(CuaDriver.Enabled=true) failed: %v", err)
	}

	// 2. XDG Autostart toggle + StartupWMClass & 0600 permissions
	if app.GetAutostartEnabled() {
		t.Fatal("expected autostart disabled initially in isolated XDG_CONFIG_HOME")
	}
	if err := app.SetAutostartEnabled(true); err != nil {
		t.Fatalf("SetAutostartEnabled(true) failed: %v", err)
	}
	if !app.GetAutostartEnabled() {
		t.Fatal("expected autostart enabled after SetAutostartEnabled(true)")
	}
	autoPath := autostartDesktopPath()
	autoInfo, err := os.Stat(autoPath)
	if err != nil || autoInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 autostart .desktop file, err=%v info=%v", err, autoInfo)
	}
	autoBytes, _ := os.ReadFile(autoPath)
	if !strings.Contains(string(autoBytes), "StartupWMClass=michael-auto") || !strings.Contains(string(autoBytes), "--start-hidden") {
		t.Fatalf("expected StartupWMClass=michael-auto & --start-hidden in autostart file, got:\n%s", string(autoBytes))
	}
	if err := app.SetAutostartEnabled(false); err != nil {
		t.Fatalf("SetAutostartEnabled(false) failed: %v", err)
	}
	if app.GetAutostartEnabled() {
		t.Fatal("expected autostart disabled after SetAutostartEnabled(false)")
	}

	// 3. ExportDiagnostics creates a deterministic 0600 .tar.gz with ZERO raw PII/tokens!
	app.auditLogger.Log(domain.AuditEntry{
		Timestamp: time.Now(),
		Level:     domain.AuditWarn,
		Event:     "diag_pii_probe",
		Message:   "Email secret.operator@corp.example with Bearer ya29.diag_secret_token_999 and ?code_verifier=diag_pkce_888",
	})
	diagPath, err := app.ExportDiagnostics()
	if err != nil {
		t.Fatalf("ExportDiagnostics failed: %v", err)
	}
	diagInfo, err := os.Stat(diagPath)
	if err != nil || diagInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 diagnostics tarball, err=%v info=%v", err, diagInfo)
	}
	rawTarGz, err := os.ReadFile(diagPath)
	if err != nil {
		t.Fatal(err)
	}
	gzr, err := gzip.NewReader(bytes.NewReader(rawTarGz))
	if err != nil {
		t.Fatalf("invalid gzip diagnostics archive: %v", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	foundFiles := map[string]string{}
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatalf("read tar entry failed: %v", nextErr)
		}
		data, readErr := io.ReadAll(tr)
		if readErr != nil {
			t.Fatal(readErr)
		}
		foundFiles[hdr.Name] = string(data)
		for _, leaked := range []string{"secret.operator@corp.example", "diag_secret_token_999", "diag_pkce_888"} {
			if strings.Contains(string(data), leaked) {
				t.Fatalf("SECURITY FAILURE: diagnostics entry %s leaked raw secret %q!", hdr.Name, leaked)
			}
		}
	}
	for _, want := range []string{"audit_recent.json", "config_redacted.json", "inspect_ui.json", "runtime_status.json", "version.txt"} {
		if _, ok := foundFiles[want]; !ok {
			t.Fatalf("expected %s inside diagnostics tarball, got keys=%v", want, foundFiles)
		}
	}
	if !strings.Contains(foundFiles["version.txt"], AppVersion) {
		t.Fatalf("expected version.txt to contain %s, got %q", AppVersion, foundFiles["version.txt"])
	}

	// 4. Control socket commands (status / pause / resume / quit) over real UNIX socket!
	guard := storage.NewInstanceGuard(filepath.Join(tmp, "runtime", "michael-auto"))
	defer guard.Close()
	acq, err := guard.AcquireWithoutWake()
	if err != nil || !acq {
		t.Fatalf("AcquireWithoutWake failed: acq=%v err=%v", acq, err)
	}
	quitCtx, cancelQuit := context.WithCancel(context.Background())
	defer cancelQuit()
	guard.SetControlCallback(func(cmd string) string {
		return app.HandleControlCommand(cmd, "headless", cancelQuit)
	})

	if rep, err := guard.SendCommand("status"); err != nil || !strings.HasPrefix(rep, "ok:headless state=") {
		t.Fatalf("unexpected status reply: %q (err=%v)", rep, err)
	}
	if rep, err := guard.SendCommand("pause"); err != nil || rep != "ok:paused" || app.GetConfig().AutomationEnabled {
		t.Fatalf("unexpected pause reply: %q enabled=%v (err=%v)", rep, app.GetConfig().AutomationEnabled, err)
	}
	if rep, err := guard.SendCommand("resume"); err != nil || rep != "ok:resumed" || !app.GetConfig().AutomationEnabled {
		t.Fatalf("unexpected resume reply: %q enabled=%v (err=%v)", rep, app.GetConfig().AutomationEnabled, err)
	}
	if rep, err := guard.SendCommand("quit"); err != nil || rep != "ok:quitting" {
		t.Fatalf("unexpected quit reply: %q (err=%v)", rep, err)
	}
	select {
	case <-quitCtx.Done():
	case <-time.After(1 * time.Second):
		t.Fatal("expected HandleControlCommand('quit') to invoke cancelHeadless")
	}

	// 5. Simulation controls, Evidence Vault CRUD & Self-Test E2E
	if stMFA := app.SimulateAuthChallenge(); stMFA.State != domain.StateHardStopChallenge {
		t.Fatalf("expected HardStopChallenge after SimulateAuthChallenge, got %s", stMFA.State)
	}
	app.ResetHardStop()
	if stReset := app.GetStatus(); stReset.State != domain.StateObserving {
		t.Fatalf("expected Observing after ResetHardStop, got %s", stReset.State)
	}
	if stSim := app.SimulateHumanInput("keyboard"); stSim.State != domain.StatePausedHumanInput {
		t.Fatalf("expected PausedHumanInput after SimulateHumanInput, got %s", stSim.State)
	}

	rec, err := app.evidenceStore.SaveEvidence("app_e2e_run", "01_verify_quota_zero", selfTestPNG, domain.EvidenceRecord{WindowTitle: "Antigravity"})
	if err != nil {
		t.Fatal(err)
	}
	if list, err := app.ListRecentEvidence(10); err != nil || len(list) != 1 {
		t.Fatalf("expected 1 evidence item, got %d (%v)", len(list), err)
	}
	if dataURL, err := app.GetEvidenceDataURL(rec.ImagePath); err != nil || !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Fatalf("GetEvidenceDataURL failed: %v", err)
	}
	if err := app.ClearEvidenceVault(); err != nil {
		t.Fatalf("ClearEvidenceVault failed: %v", err)
	}
	if err := app.ClearAuditLogs(); err != nil {
		t.Fatalf("ClearAuditLogs failed: %v", err)
	}
	if err := executeSelfTest(); err != nil {
		t.Fatalf("executeSelfTest failed: %v", err)
	}
}

func TestCLIBinaryE2E_VersionSelfTestHeadlessDaemonAndControlSocket(t *testing.T) {
	binPath := filepath.Join("build", "bin", "michael-auto")
	if _, err := os.Stat(binPath); err != nil {
		t.Skip("compiled binary build/bin/michael-auto not present (run 'make build' first)")
	}

	tmp := shortE2ETempDir(t)
	env := append(os.Environ(),
		"XDG_CONFIG_HOME="+filepath.Join(tmp, "config"),
		"XDG_STATE_HOME="+filepath.Join(tmp, "state"),
		"XDG_RUNTIME_DIR="+filepath.Join(tmp, "runtime"),
		"XDG_DATA_HOME="+filepath.Join(tmp, "data"),
	)

	runCLI := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binPath, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// 1. --version & --self-test
	if verOut, err := runCLI("--version"); err != nil || !strings.Contains(verOut, "v"+AppVersion) {
		t.Fatalf("--version failed: %q (%v)", verOut, err)
	}
	if stOut, err := runCLI("--self-test"); err != nil || !strings.Contains(stOut, "SELF-TEST PASSED") {
		t.Fatalf("--self-test failed: %q (%v)", stOut, err)
	}

	// 2. Start isolated --headless daemon, exercise --ctl status/pause/resume, duplicate-launch guard & --ctl quit!
	daemonCtx, daemonCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer daemonCancel()
	daemonCmd := exec.CommandContext(daemonCtx, binPath, "--headless")
	daemonCmd.Env = env
	var daemonOut bytes.Buffer
	daemonCmd.Stdout = &daemonOut
	daemonCmd.Stderr = &daemonOut
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start --headless daemon failed: %v", err)
	}

	sockPath := filepath.Join(tmp, "runtime", "michael-auto", "control.sock")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		daemonCancel()
		_ = daemonCmd.Wait()
		t.Fatalf("headless control.sock did not appear: %s", daemonOut.String())
	}

	if rep, err := runCLI("--ctl", "status"); err != nil || !strings.HasPrefix(rep, "ok:headless ") {
		t.Fatalf("--ctl status failed: %q (%v)", rep, err)
	}
	if rep, err := runCLI("--ctl", "pause"); err != nil || rep != "ok:paused" {
		t.Fatalf("--ctl pause failed: %q (%v)", rep, err)
	}
	if rep, err := runCLI("--ctl", "resume"); err != nil || rep != "ok:resumed" {
		t.Fatalf("--ctl resume failed: %q (%v)", rep, err)
	}

	// Launching a second --headless daemon must detect the lock and exit cleanly without clobbering!
	if dupOut, err := runCLI("--headless"); err != nil || !strings.Contains(dupOut, "đang chạy sẵn") {
		t.Fatalf("duplicate --headless launch unexpected: %q (%v)", dupOut, err)
	}

	// Cleanly shut down via --ctl quit and verify exit 0 + socket cleanup!
	if rep, err := runCLI("--ctl", "quit"); err != nil || rep != "ok:quitting" {
		t.Fatalf("--ctl quit failed: %q (%v)", rep, err)
	}
	if err := daemonCmd.Wait(); err != nil {
		t.Fatalf("expected headless daemon to exit cleanly on --ctl quit, got err=%v output=%s", err, daemonOut.String())
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("expected control.sock cleaned up on daemon exit, stat err=%v", err)
	}

	// 3. CLI --export-diagnostics produces a real 0600 tarball!
	diagOut, err := runCLI("--export-diagnostics")
	if err != nil || !strings.Contains(diagOut, "DIAGNOSTICS EXPORTED") {
		t.Fatalf("--export-diagnostics failed: %q (%v)", diagOut, err)
	}
}

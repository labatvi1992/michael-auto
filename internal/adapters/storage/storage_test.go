package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

func TestFileConfigStorePermissionsAndRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "michael-auto", "config.json")
	store := NewFileConfigStore(cfgPath)

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("initial Load failed: %v", err)
	}
	if loaded.YieldPauseSeconds != 30 {
		t.Fatalf("expected default 30s yield pause, got %d", loaded.YieldPauseSeconds)
	}

	loaded.YieldPauseSeconds = 45
	if err := store.Save(loaded); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("expected config file created on disk after Save: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600 file permissions, got %o", perm)
	}

	reloaded, err := store.Load()
	if err != nil || reloaded.YieldPauseSeconds != 45 {
		t.Fatalf("expected reloaded YieldPauseSeconds=45, got %d (%v)", reloaded.YieldPauseSeconds, err)
	}

	// Verify v1 -> v2 migration upgrades CUA + Agent Workspace + Evidence defaults while ignoring deprecated v1 fields.
	v1Path := filepath.Join(tmp, "v1-config.json")
	v1Raw := `{
		"version": 1,
		"automationEnabled": true,
		"yieldPauseSeconds": 30,
		"cooldownSeconds": 15,
		"pollIntervalMs": 2000,
		"minConfidence": 0.95,
		"consensusFrames": 2,
		"pointerDeadzonePx": 4,
		"maxSwitchesPerWindow": 3,
		"switchWindowMinutes": 30,
		"screenpipe": {"enabled": true, "endpoint": "http://127.0.0.1:3030"},
		"profiles": [{
			"id": "p1",
			"name": "Custom Work",
			"emailHint": "work@company.com",
			"browserType": "chrome",
			"browserProfileDir": "Default",
			"primaryModel": "Claude Sonnet 4.6",
			"enabled": true,
			"priority": 1
		}]
	}`
	if err := os.WriteFile(v1Path, []byte(v1Raw), 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, err := NewFileConfigStore(v1Path).Load()
	if err != nil {
		t.Fatalf("v1 migration Load failed: %v", err)
	}
	if migrated.Version != domain.CurrentConfigVersion ||
		!migrated.CuaDriver.Enabled ||
		!migrated.CuaDriver.EnableWayland ||
		!migrated.CuaDriver.AgentWorkspaceEnabled ||
		migrated.CuaDriver.AgentWorkspaceName != domain.DefaultAgentWorkspaceName ||
		!migrated.CuaDriver.RequireEvidenceBeforeActuate ||
		migrated.CuaDriver.SignOutCooldownMs != 60000 {
		t.Fatalf("expected v1 config migrated to v3 CUA + 'Agent Workspace' + Evidence defaults, got v=%d cua=%+v", migrated.Version, migrated.CuaDriver)
	}
	if len(migrated.Profiles) != 1 || migrated.Profiles[0].EmailHint != "work@company.com" {
		t.Fatalf("expected user profiles preserved across v1->v3 migration, got %+v", migrated.Profiles)
	}
	// Verify migrated v3 config was atomically persisted back to disk!
	persistedBytes, err := os.ReadFile(v1Path)
	if err != nil || !strings.Contains(string(persistedBytes), `"Agent Workspace"`) || !strings.Contains(string(persistedBytes), `"version": 3`) {
		t.Fatalf("expected migrated v3 config persisted on disk, got: %s (%v)", string(persistedBytes), err)
	}

	// Verify corrupt config.json is atomically backed up to config.json.corrupt.* before fallback (P0-4)!
	corruptPath := filepath.Join(tmp, "corrupt-config.json")
	if err := os.WriteFile(corruptPath, []byte(`{"version": 3, INVALID_JSON`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, loadErr := NewFileConfigStore(corruptPath).Load(); loadErr == nil || !strings.Contains(loadErr.Error(), "đã sao lưu") {
		t.Fatalf("expected corrupt config backup error, got %v", loadErr)
	}
	matches, _ := filepath.Glob(corruptPath + ".corrupt.*")
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 corrupt backup file, got %v", matches)
	}
}

func TestJSONLAuditLoggerRedactionRotationAndTailSeek(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "audit.jsonl")
	logger := NewJSONLAuditLogger(logPath)
	logger.SetRotationLimits(450, 2)

	for i := 0; i < 6; i++ {
		logger.Log(domain.AuditEntry{
			Timestamp: time.Now(),
			Level:     domain.AuditInfo,
			Event:     "auth_check",
			Message:   "User danglongvipro@gmail.com with Bearer ya29.super_secret_token_abcdef12345",
			Metadata: map[string]string{
				"account": "danglongvipro@gmail.com",
			},
		})
	}

	entries, err := logger.Recent(10)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected recent entries, got len=%d err=%v", len(entries), err)
	}

	latest := entries[0]
	if strings.Contains(latest.Message, "danglongvipro@gmail.com") || strings.Contains(latest.Message, "super_secret_token") {
		t.Fatalf("PII/token leaked into audit log: %q", latest.Message)
	}
	if !strings.Contains(latest.Message, "d***@gmail.com") || !strings.Contains(latest.Message, "[REDACTED_TOKEN]") {
		t.Fatalf("expected masked email and redacted token marker, got: %q", latest.Message)
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected rotated backup file audit.jsonl.1 to exist: %v", err)
	}

	// Verify O(1) Tail-Seek on a >200KB log file + OAuth URL query parameter redaction.
	largePath := filepath.Join(tmp, "large-audit.jsonl")
	largeLogger := NewJSONLAuditLogger(largePath)
	largeLogger.SetRotationLimits(10*1024*1024, 2)
	padding := strings.Repeat("x", 1024)
	for i := 0; i < 220; i++ {
		largeLogger.Log(domain.AuditEntry{
			Timestamp: time.Now(),
			Level:     domain.AuditInfo,
			Event:     "bulk_pad",
			Message:   padding,
		})
	}
	largeLogger.Log(domain.AuditEntry{
		Timestamp: time.Now(),
		Level:     domain.AuditWarn,
		Event:     "oauth_redirect",
		Message:   "Redirect https://accounts.google.com/o/oauth2/v2/auth?code=4/0AeanS_secret_code_999&state=secretstate123&code_challenge=pkce_chal_777&code_verifier=pkce_ver_888&login_hint=alice@company.com",
	})

	tailEntries, err := largeLogger.Recent(5)
	if err != nil || len(tailEntries) != 5 {
		t.Fatalf("expected 5 tail entries from >200KB log, got len=%d err=%v", len(tailEntries), err)
	}
	top := tailEntries[0]
	if top.Event != "oauth_redirect" {
		t.Fatalf("expected newest entry oauth_redirect first, got %s", top.Event)
	}
	if strings.Contains(top.Message, "0AeanS_secret_code_999") ||
		strings.Contains(top.Message, "secretstate123") ||
		strings.Contains(top.Message, "pkce_chal_777") ||
		strings.Contains(top.Message, "pkce_ver_888") ||
		strings.Contains(top.Message, "alice@company.com") {
		t.Fatalf("OAuth/PKCE query secret or login_hint leaked: %q", top.Message)
	}

	// Verify RedactingWriter sanitizes sidecar stderr streams.
	var buf bytes.Buffer
	rw := largeLogger.NewRedactingWriter(&buf)
	_, _ = rw.Write([]byte("sidecar debug user=bob@company.com?code=secret_oauth_code_abc\n"))
	if strings.Contains(buf.String(), "bob@company.com") || strings.Contains(buf.String(), "secret_oauth_code_abc") {
		t.Fatalf("RedactingWriter leaked stderr secrets: %q", buf.String())
	}
}

func TestEvidenceVaultSaveVerifySHA256PruningAndPathTraversalGuard(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "evidence")
	vault := NewEvidenceVault(baseDir, 2)

	pngBytes := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-mock-png-payload-for-evidence-test")
	wantHash := sha256.Sum256(pngBytes)
	wantHex := hex.EncodeToString(wantHash[:])

	rec1, err := vault.SaveEvidence("run_001", "01_verify_quota_zero", pngBytes, domain.EvidenceRecord{
		CreatedAt:      time.Now().Add(-3 * time.Minute),
		WindowTitle:    "Antigravity",
		PID:            4242,
		VerifiedDetail: "Quota 0% verified",
	})
	if err != nil {
		t.Fatalf("SaveEvidence run_001 failed: %v", err)
	}
	if rec1.SHA256 != wantHex {
		t.Fatalf("expected SHA256 %s, got %s", wantHex, rec1.SHA256)
	}
	info, err := os.Stat(rec1.ImagePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 PNG evidence file, got err=%v mode=%v", err, info)
	}

	// Save run_002 and run_003 -> maxRuns=2 must automatically prune run_001!
	time.Sleep(10 * time.Millisecond)
	_, err = vault.SaveEvidence("run_002", "02_pre_signout_settings", pngBytes, domain.EvidenceRecord{WindowTitle: "Settings"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	rec3, err := vault.SaveEvidence("run_003", "04_pre_chrome_account_select", pngBytes, domain.EvidenceRecord{
		WindowTitle:    "Sign in - Google Accounts",
		BrowserProfile: "Profile 1",
		OmniboxOrigin:  "https://accounts.google.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(baseDir, "run_001")); !os.IsNotExist(err) {
		t.Fatalf("expected oldest run_001 pruned when maxRuns=2, stat err=%v", err)
	}

	dataURL, err := vault.ReadDataURL(rec3.ImagePath)
	if err != nil || !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Fatalf("expected valid PNG data URL, got err=%v prefix=%q", err, dataURL)
	}

	// Path traversal outside baseDir must be strictly blocked!
	outsideFile := filepath.Join(t.TempDir(), "outside.png")
	_ = os.WriteFile(outsideFile, pngBytes, 0o600)
	if _, err := vault.ReadDataURL(outsideFile); err == nil {
		t.Fatal("expected ReadDataURL to reject path outside evidence vault baseDir")
	}

	// Verify MaskPNGEditorRects masks private code editor region while keeping header intact!
	canvas := image.NewNRGBA(image.Rect(0, 0, 200, 160))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.NRGBA{R: 255, G: 255, B: 255, A: 255}), image.Point{}, draw.Src)
	var realPNGBuf bytes.Buffer
	if err := png.Encode(&realPNGBuf, canvas); err != nil {
		t.Fatal(err)
	}
	maskedBytes, count := MaskPNGEditorRects(realPNGBuf.Bytes(), []domain.RedactRect{
		{X: 20, Y: 40, Width: 140, Height: 80},
	})
	if count != 1 {
		t.Fatalf("expected 1 redacted region, got %d", count)
	}
	decodedMasked, err := png.Decode(bytes.NewReader(maskedBytes))
	if err != nil {
		t.Fatalf("decode masked PNG failed: %v", err)
	}
	rTop, gTop, bTop, _ := decodedMasked.At(5, 5).RGBA()
	if rTop>>8 != 255 || gTop>>8 != 255 || bTop>>8 != 255 {
		t.Fatalf("expected header pixel (5,5) untouched white, got (%d,%d,%d)", rTop>>8, gTop>>8, bTop>>8)
	}
	rMid, gMid, bMid, _ := decodedMasked.At(60, 80).RGBA()
	if rMid>>8 != 11 || gMid>>8 != 17 || bMid>>8 != 32 {
		t.Fatalf("expected editor pixel (60,80) masked dark (#0b1120), got (%d,%d,%d)", rMid>>8, gMid>>8, bMid>>8)
	}

	// Verify MaskPNGEditorRectsWithFrame handles 2x HiDPI scaling AND cropped window origin offset (P0-2)!
	hidpiCanvas := image.NewNRGBA(image.Rect(0, 0, 400, 320)) // 2x physical pixels of a 200x160 logical window
	draw.Draw(hidpiCanvas, hidpiCanvas.Bounds(), image.NewUniform(color.NRGBA{R: 255, G: 255, B: 255, A: 255}), image.Point{}, draw.Src)
	var hidpiBuf bytes.Buffer
	if err := png.Encode(&hidpiBuf, hidpiCanvas); err != nil {
		t.Fatal(err)
	}
	// Logical window sits at screen (100, 50) size (200x160); editor sits at screen (120, 90) size (140x80) -> local (20, 40) -> 2x pixels (40..320, 80..240)!
	hidpiMasked, hCount := MaskPNGEditorRectsWithFrame(
		hidpiBuf.Bytes(),
		[]domain.RedactRect{{X: 120, Y: 90, Width: 140, Height: 80}},
		&domain.RedactRect{X: 100, Y: 50, Width: 200, Height: 160},
		"window",
	)
	if hCount != 1 {
		t.Fatalf("expected 1 HiDPI redacted region, got %d", hCount)
	}
	decHiDPI, err := png.Decode(bytes.NewReader(hidpiMasked))
	if err != nil {
		t.Fatalf("decode HiDPI masked PNG failed: %v", err)
	}
	rHi, gHi, bHi, _ := decHiDPI.At(300, 220).RGBA() // Would be UNMASKED at 1x scale! Must be masked at 2x!
	if rHi>>8 != 11 || gHi>>8 != 17 || bHi>>8 != 32 {
		t.Fatalf("expected 2x HiDPI editor pixel (300,220) masked dark (#0b1120), got (%d,%d,%d)", rHi>>8, gHi>>8, bHi>>8)
	}

	// Verify symlink escape rejection in ReadDataURL!
	symLinkPath := filepath.Join(baseDir, "run_003", "evil_link.png")
	if os.Symlink(outsideFile, symLinkPath) == nil {
		if _, err := vault.ReadDataURL(symLinkPath); err == nil {
			t.Fatal("expected ReadDataURL to block symlink pointing outside Evidence Vault")
		}
	}

	// Verify MaxVaultBytes hard disk cap prunes old runs even when maxRuns is high (P1-4)!
	diskVault := NewEvidenceVault(filepath.Join(t.TempDir(), "disk_cap"), 50)
	diskVault.SetMaxVaultBytes(600)
	bigPayload := bytes.Repeat([]byte("A"), 350)
	_, _ = diskVault.SaveEvidence("cap_run_1", "step1", bigPayload, domain.EvidenceRecord{WindowTitle: "W1"})
	time.Sleep(15 * time.Millisecond)
	_, _ = diskVault.SaveEvidence("cap_run_2", "step1", bigPayload, domain.EvidenceRecord{WindowTitle: "W2"})
	if _, err := os.Stat(filepath.Join(diskVault.RootDir(), "cap_run_1")); !os.IsNotExist(err) {
		t.Fatalf("expected cap_run_1 pruned by MaxVaultBytes=600, stat err=%v", err)
	}

	// Verify vault.Clear() wipes all stored Evidence runs cleanly!
	if err := vault.Clear(); err != nil {
		t.Fatalf("vault.Clear() failed: %v", err)
	}
	rem, _ := vault.ListRecent(10)
	if len(rem) != 0 {
		t.Fatalf("expected 0 Evidence records after Clear(), got %d", len(rem))
	}
}

func TestEphemeralIntentStoreAtomicSaveLoadAndGuaranteedFileCleanup(t *testing.T) {
	intentFile := filepath.Join(t.TempDir(), "runtime", "pending_intent.json")
	store := NewEphemeralIntentStore(intentFile)

	now := time.Now().UTC().Truncate(time.Second)
	nextAt := now.Add(60 * time.Second)
	intent := domain.PendingIntent{
		RunID:           "run_ephemeral_01",
		Kind:            domain.IntentSwitchProfile,
		FromProfileID:   "profile-primary",
		TargetProfileID: "profile-backup",
		TargetModel:     "gemini-2.5-pro",
		CreatedAt:       now,
		ExpiresAt:       now.Add(10 * time.Minute),
		SubStep:         domain.SubStepWaitBeforeSignIn,
		NextActionAt:    &nextAt,
	}

	if err := store.Save(intent); err != nil {
		t.Fatalf("Save intent failed: %v", err)
	}
	info, err := os.Stat(intentFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 pending_intent.json on disk during active switch, got err=%v", err)
	}

	loaded, err := store.Load()
	if err != nil || loaded == nil || loaded.RunID != "run_ephemeral_01" || loaded.SubStep != domain.SubStepWaitBeforeSignIn {
		t.Fatalf("expected persisted PendingIntent restored accurately, got %+v (err=%v)", loaded, err)
	}

	// Upon workflow completion, Clear() MUST strictly delete the temporary file from disk!
	if err := store.Clear(); err != nil {
		t.Fatalf("Clear intent failed: %v", err)
	}
	if _, err := os.Stat(intentFile); !os.IsNotExist(err) {
		t.Fatalf("expected pending_intent.json strictly deleted after Clear(), got stat err=%v", err)
	}
}

func TestInstanceGuardSingleInstanceFlockAndUnixSocketWindowWakeup(t *testing.T) {
	rtDir := t.TempDir()
	primary := NewInstanceGuard(rtDir)
	defer primary.Close()

	wokenCh := make(chan struct{}, 1)
	acquired1, err := primary.AcquireOrWakeExisting(func() {
		select {
		case wokenCh <- struct{}{}:
		default:
		}
	})
	if err != nil || !acquired1 {
		t.Fatalf("expected primary InstanceGuard to acquire lock, got acquired=%v err=%v", acquired1, err)
	}

	// Secondary launch must NOT acquire lock and MUST wake primary window over UNIX control socket!
	secondary := NewInstanceGuard(rtDir)
	defer secondary.Close()
	acquired2, err2 := secondary.AcquireOrWakeExisting(nil)
	if err2 != nil || acquired2 {
		t.Fatalf("expected secondary InstanceGuard to detect existing daemon (acquired=false), got acquired=%v err=%v", acquired2, err2)
	}
	if reply := secondary.LastWakeReply(); reply != "ok:gui" {
		t.Fatalf("expected ok:gui reply from GUI primary, got %q", reply)
	}

	select {
	case <-wokenCh:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("expected primary onShowWindow callback woken via UNIX domain socket")
	}

	// Verify headless mode reports ok:headless + control commands (status/pause/resume/quit) cleanly!
	headlessDir := t.TempDir()
	headless := NewInstanceGuard(headlessDir)
	defer headless.Close()
	if acq, err := headless.AcquireWithoutWake(); err != nil || !acq {
		t.Fatalf("headless AcquireWithoutWake failed: acq=%v err=%v", acq, err)
	}
	headless.SetControlCallback(func(cmd string) string {
		return "ok:headless:" + cmd
	})

	guiAttempt := NewInstanceGuard(headlessDir)
	defer guiAttempt.Close()
	if acq, err := guiAttempt.AcquireOrWakeExisting(nil); err != nil || acq {
		t.Fatalf("expected GUI launch to yield to headless daemon, got acq=%v err=%v", acq, err)
	}
	if reply := guiAttempt.LastWakeReply(); reply != "ok:headless" {
		t.Fatalf("expected ok:headless reply so Desktop launcher displays notification instead of silent black hole, got %q", reply)
	}
	if statusReply, err := guiAttempt.SendCommand("status"); err != nil || statusReply != "ok:headless:status" {
		t.Fatalf("expected ok:headless:status, got %q (err=%v)", statusReply, err)
	}
	if unknownReply, err := guiAttempt.SendCommand("bogus"); err != nil || unknownReply != "err:unknown-command" {
		t.Fatalf("expected err:unknown-command, got %q (err=%v)", unknownReply, err)
	}
}

func TestEvidenceDownscaleLargePNGAndAuditClearAndSecretFallback(t *testing.T) {
	// 1. OptimizeEvidencePNG & MaskPNGEditorRects must downscale oversized (>1600px, >=128KB) PNGs cleanly.
	wide := image.NewNRGBA(image.Rect(0, 0, 2000, 1000))
	for y := 0; y < 1000; y++ {
		for x := 0; x < 2000; x++ {
			off := wide.PixOffset(x, y)
			wide.Pix[off+0] = uint8((x*7 + y*13) & 0xff)
			wide.Pix[off+1] = uint8((x*3 ^ y*11) & 0xff)
			wide.Pix[off+2] = uint8((x*17 + y*5) & 0xff)
			wide.Pix[off+3] = 255
		}
	}
	var wideBuf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	if err := enc.Encode(&wideBuf, wide); err != nil {
		t.Fatal(err)
	}
	optimized := OptimizeEvidencePNG(wideBuf.Bytes())
	dec, err := png.Decode(bytes.NewReader(optimized))
	if err != nil {
		t.Fatalf("decode optimized PNG failed: %v", err)
	}
	if dec.Bounds().Dx() != 1600 || dec.Bounds().Dy() != 800 {
		t.Fatalf("expected downscaled 1600x800 PNG, got %dx%d", dec.Bounds().Dx(), dec.Bounds().Dy())
	}

	// 2. AuditLogger.Clear() truncates the JSONL file cleanly.
	logPath := filepath.Join(t.TempDir(), "clear-audit.jsonl")
	al := NewJSONLAuditLogger(logPath)
	al.Log(domain.AuditEntry{Timestamp: time.Now(), Level: domain.AuditInfo, Event: "ev1", Message: "hello"})
	if err := al.Clear(); err != nil {
		t.Fatalf("AuditLogger.Clear failed: %v", err)
	}
	recs, err := al.Recent(10)
	if err != nil || len(recs) != 0 {
		t.Fatalf("expected 0 logs after Clear, got %d (err=%v)", len(recs), err)
	}
	_ = al.Close()

	// 3. FreedesktopSecretStore falls back safely to memory when Secret Service is unavailable.
	ss := NewSecretStore()
	ss.getFn = func(key string) (string, error) { return "", os.ErrNotExist }
	ss.setFn = func(key, value string) error { return os.ErrPermission }
	ss.delFn = func(key string) error { return nil }

	if err := ss.Set("tok_key", "secret_val"); err != nil {
		t.Fatalf("Set fallback failed: %v", err)
	}
	if ss.Backend() != "memory-fallback" {
		t.Fatalf("expected memory-fallback backend, got %q", ss.Backend())
	}
	got, err := ss.Get("tok_key")
	if err != nil || got != "secret_val" {
		t.Fatalf("expected fallback secret_val, got %q (err=%v)", got, err)
	}
	_ = ss.Delete("tok_key")
	if _, err := ss.Get("tok_key"); err == nil {
		t.Fatal("expected error after Delete")
	}

	// 4. Verify XDG path helpers, Path() getters, SetRetentionRuns & SetShowWindowCallback.
	tmpXDG := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpXDG, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpXDG, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmpXDG, "run"))

	if p := DefaultXDGConfigPath(); !strings.Contains(p, filepath.Join("cfg", "michael-auto", "config.json")) {
		t.Fatalf("unexpected DefaultXDGConfigPath: %q", p)
	}
	if p := DefaultXDGEvidenceRoot(); !strings.Contains(p, filepath.Join("state", "michael-auto", "evidence")) {
		t.Fatalf("unexpected DefaultXDGEvidenceRoot: %q", p)
	}
	if p := DefaultEphemeralIntentPath(); !strings.Contains(p, filepath.Join("run", "michael-auto", "pending_intent.json")) {
		t.Fatalf("unexpected DefaultEphemeralIntentPath: %q", p)
	}
	if NewFileConfigStore("").Path() == "" || NewJSONLAuditLogger("").Path() == "" || NewEphemeralIntentStore("").Path() == "" {
		t.Fatal("expected non-empty default store paths")
	}
	ev := NewEvidenceVault("", 5)
	ev.SetRetentionRuns(15)
	_, _ = ev.SaveEvidence("r_list_1", "step1", []byte("png1"), domain.EvidenceRecord{WindowTitle: "W1"})
	_, _ = ev.SaveEvidence("r_list_2", "step2", []byte("png2"), domain.EvidenceRecord{WindowTitle: "W2"})
	if list, err := ev.ListRecent(10); err != nil || len(list) != 2 {
		t.Fatalf("expected 2 listed evidence records, got %d (err=%v)", len(list), err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if DefaultXDGConfigPath() == "" || DefaultXDGEvidenceRoot() == "" || DefaultEphemeralIntentPath() == "" {
		t.Fatal("expected valid home fallback XDG paths")
	}
	ig := NewInstanceGuard(filepath.Join(tmpXDG, "ig"))
	ig.SetShowWindowCallback(func() {})
	_ = ig.Close()
}

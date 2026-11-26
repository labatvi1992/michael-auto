package actuators

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labatvi1992/michael-auto/internal/domain"
	"github.com/labatvi1992/michael-auto/sidecar"
)

func TestBrowserProfileIsolationCommands(t *testing.T) {
	var executedBin string
	var executedArgs []string

	router := NewTestableBrowserRouter(
		func(file string) (string, error) {
			return "/usr/bin/" + file, nil
		},
		func(ctx context.Context, binary string, args []string) error {
			executedBin = binary
			executedArgs = args
			return nil
		},
	)

	chromeProf := domain.AccountProfile{
		ID:                "p1",
		Name:              "Work",
		BrowserType:       domain.BrowserChrome,
		BrowserProfileDir: "Profile 2",
		PrimaryModel:      "gemini-2.5-pro",
	}

	if err := router.OpenProfileSession(context.Background(), chromeProf, false); err != nil {
		t.Fatalf("OpenProfileSession Chrome failed: %v", err)
	}
	if executedBin != "/usr/bin/google-chrome-stable" {
		t.Fatalf("unexpected chrome binary: %s", executedBin)
	}
	joined := strings.Join(executedArgs, " ")
	if !strings.Contains(joined, "--profile-directory=Profile 2") ||
		!strings.Contains(joined, "--ozone-platform-hint=auto") ||
		!strings.Contains(joined, "--force-renderer-accessibility") {
		t.Fatalf("expected --profile-directory=Profile 2, Wayland ozone, and --force-renderer-accessibility flags, got: %v", executedArgs)
	}

	ffProf := domain.AccountProfile{
		ID:                "p2",
		Name:              "Personal",
		BrowserType:       domain.BrowserFirefox,
		BrowserProfileDir: "personal-ff",
		PrimaryModel:      "gemini-2.5-pro",
	}
	bin, args, err := router.BuildLaunchCommand(ffProf)
	if err != nil || bin != "/usr/bin/firefox" || args[0] != "-P" || args[1] != "personal-ff" {
		t.Fatalf("unexpected Firefox command: bin=%s args=%v err=%v", bin, args, err)
	}

	unsafeProf := chromeProf
	unsafeProf.BrowserProfileDir = "../../bad"
	if _, _, err := router.BuildLaunchCommand(unsafeProf); err == nil {
		t.Fatal("expected unsafe profile dir to be rejected")
	}
}

func TestDiscoverBrowserProfilesAndStrictProfileLookupFromLocalState(t *testing.T) {
	cfgRoot := t.TempDir()
	chromeDir := filepath.Join(cfgRoot, "google-chrome")
	if err := os.MkdirAll(chromeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	localStateJSON := `{
		"profile": {
			"info_cache": {
				"Profile 2": {
					"name": "Team Backup",
					"user_name": "backup@company.com",
					"gaia_name": "Backup User"
				},
				"Default": {
					"name": "Michael Primary",
					"user_name": "primary@company.com",
					"gaia_name": "Michael"
				},
				"Profile 3": {
					"name": "Web-Only No-Sync",
					"user_name": "",
					"gaia_name": ""
				}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(chromeDir, "Local State"), []byte(localStateJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(chromeDir, "Profile 3"), 0o700); err != nil {
		t.Fatal(err)
	}
	prefsJSON := `{"account_info":[{"email":"websignon@company.com","full_name":"Web SignOn"}]}`
	if err := os.WriteFile(filepath.Join(chromeDir, "Profile 3", "Preferences"), []byte(prefsJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	profiles := DiscoverBrowserProfiles(cfgRoot)
	if len(profiles) != 3 {
		t.Fatalf("expected 3 discovered Chrome profiles, got %+v", profiles)
	}
	if profiles[0].ProfileDir != "Default" || profiles[0].EmailHint != "primary@company.com" || profiles[0].DisplayName != "Michael Primary" {
		t.Fatalf("expected Default profile sorted first with exact metadata, got %+v", profiles[0])
	}
	if profiles[1].ProfileDir != "Profile 2" || profiles[1].EmailHint != "backup@company.com" {
		t.Fatalf("expected Profile 2 second, got %+v", profiles[1])
	}
	if profiles[2].ProfileDir != "Profile 3" || profiles[2].EmailHint != "websignon@company.com" {
		t.Fatalf("expected Profile 3 email fallback from Preferences when Sync is off, got %+v", profiles[2])
	}

	disp, email, ok := LookupBrowserProfileMeta(domain.BrowserChrome, "Profile 2", cfgRoot)
	if !ok || disp != "Team Backup" || email != "backup@company.com" {
		t.Fatalf("expected LookupBrowserProfileMeta match for Profile 2, got disp=%q email=%q ok=%v", disp, email, ok)
	}
}

func TestEmbeddedSidecarSingleBinaryExtractionAndIdempotence(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "sidecar")

	entry1, err := sidecar.ExtractTo(dest)
	if err != nil {
		t.Fatalf("ExtractTo failed: %v", err)
	}
	if _, err := os.Stat(entry1); err != nil {
		t.Fatalf("expected extracted entrypoint %s to exist: %v", entry1, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "michael_cua_sidecar", "security.py")); err != nil {
		t.Fatalf("expected embedded security.py extracted to disk: %v", err)
	}

	// Second call must hit SHA-256 fast path cleanly.
	entry2, err := sidecar.ExtractTo(dest)
	if err != nil || entry2 != entry1 {
		t.Fatalf("expected idempotent fast-path extraction, got %q vs %q (%v)", entry2, entry1, err)
	}
}

func buildMockUvTarGz(t *testing.T, payload []byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:     "uv-x86_64-unknown-linux-gnu/uv",
		Mode:     0o755,
		Size:     int64(len(payload)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return raw.Bytes()
}

func TestUvBinarySHA256VerifiedBootstrapAndTamperRejection(t *testing.T) {
	fakeUv := []byte("#!/bin/sh\necho uv 0.6.6\n")
	archive := buildMockUvTarGz(t, fakeUv)
	sum := sha256.Sum256(archive)
	validHex := hex.EncodeToString(sum[:])

	var serveTampered bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			if serveTampered {
				_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  uv.tar.gz\n"))
				return
			}
			_, _ = w.Write([]byte(validHex + "  uv.tar.gz\n"))
			return
		}
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bin", "uv")
	gotPath, err := bootstrapVerifiedUvFromBaseURL(context.Background(), srv.URL, dest)
	if err != nil || gotPath != dest {
		t.Fatalf("expected verified uv bootstrap to succeed, got path=%q err=%v", gotPath, err)
	}
	info, err := os.Stat(dest)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("expected extracted 0700 uv binary, got err=%v mode=%v", err, info)
	}

	// Tampered SHA-256 checksum MUST be strictly rejected!
	serveTampered = true
	destBad := filepath.Join(t.TempDir(), "bin", "uv-bad")
	if _, err := bootstrapVerifiedUvFromBaseURL(context.Background(), srv.URL, destBad); err == nil {
		t.Fatal("expected tampered SHA-256 archive to be rejected")
	}
}

func TestCuaDriverActuatorLosslessOAuthPKCERelayAcrossChromeProfiles(t *testing.T) {
	const pkceURL = "https://accounts.google.com/o/oauth2/v2/auth?client_id=ag&state=pkce_secret_42&redirect_uri=http://127.0.0.1:41999/cb"

	// Create a lightweight mock JSON-RPC Python script simulating first-attempt oauth_profile_relay_required,
	// then succeeding once BrowserRouter launches --profile-directory=Profile 1 with the exact pkceURL!
	scriptPath := filepath.Join(t.TempDir(), "mock_relay_sidecar.py")
	scriptSrc := `import json, sys
calls = 0
for line in sys.stdin:
    req = json.loads(line)
    mid, method = req.get("id"), req.get("method")
    if method == "health_check":
        print(json.dumps({"jsonrpc": "2.0", "id": mid, "result": {"healthy": True}}), flush=True)
    elif method == "select_chrome_account":
        calls += 1
        if calls == 1:
            print(json.dumps({
                "jsonrpc": "2.0",
                "id": mid,
                "error": {
                    "code": -32006,
                    "message": "Misrouted to Default",
                    "data": {
                        "reason": "oauth_profile_relay_required",
                        "relayOAuthUrl": "` + pkceURL + `",
                        "browserProfileDir": "Profile 1"
                    }
                }
            }), flush=True)
        else:
            print(json.dumps({
                "jsonrpc": "2.0",
                "id": mid,
                "result": {"selectedAccount": "backup@company.com"}
            }), flush=True)
    elif method == "shutdown":
        print(json.dumps({"jsonrpc": "2.0", "id": mid, "result": {"shutdown": True}}), flush=True)
        break
`
	if err := os.WriteFile(scriptPath, []byte(scriptSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	var launchedArgs []string
	router := NewTestableBrowserRouter(
		func(file string) (string, error) { return "/usr/bin/" + file, nil },
		func(ctx context.Context, binary string, args []string) error {
			launchedArgs = append([]string(nil), args...)
			return nil
		},
	)

	cua := NewCustomCuaDriverActuator("python3", scriptPath, []string{"TEST=1"}, nil, router)
	defer cua.Close()

	prof := domain.AccountProfile{
		ID:                "profile-backup",
		Name:              "Work Backup",
		EmailHint:         "backup@company.com",
		BrowserType:       domain.BrowserChrome,
		BrowserProfileDir: "Profile 1",
		PrimaryModel:      "gemini-2.5-pro",
	}

	if err := cua.SelectChromeAccount(context.Background(), prof, false); err != nil {
		t.Fatalf("expected SelectChromeAccount to succeed after lossless OAuth PKCE relay, got %v", err)
	}
	joined := strings.Join(launchedArgs, " ")
	if !strings.Contains(joined, "--profile-directory=Profile 1") || !strings.Contains(joined, pkceURL) {
		t.Fatalf("expected BrowserRouter launched --profile-directory=Profile 1 with exact PKCE URL, got %v", launchedArgs)
	}
}

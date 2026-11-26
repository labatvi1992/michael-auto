package actuators

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/labatvi1992/michael-auto/sidecar"
)

const (
	pinnedCuaDriverSpec = "cua-driver==0.58.1"
	pinnedUvVersion     = "0.6.6"
	maxUvArchiveLen     = 64 * 1024 * 1024 // 64 MB safety cap
)

// DefaultXDGDataDir returns $XDG_DATA_HOME/michael-auto (default ~/.local/share/michael-auto).
func DefaultXDGDataDir() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "michael-auto")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "michael-auto")
	}
	return filepath.Join(os.TempDir(), "michael-auto")
}

// ManagedVenvPythonPath returns the isolated virtualenv python path inside XDG_DATA_HOME.
func ManagedVenvPythonPath() string {
	return filepath.Join(DefaultXDGDataDir(), "venv", "bin", "python")
}

// ManagedUvBinPath returns the isolated uv binary path inside XDG_DATA_HOME/michael-auto/bin/uv.
func ManagedUvBinPath() string {
	return filepath.Join(DefaultXDGDataDir(), "bin", "uv")
}

// ManagedSidecarDir returns the extracted sidecar directory inside XDG_DATA_HOME.
func ManagedSidecarDir() string {
	return filepath.Join(DefaultXDGDataDir(), "sidecar")
}

// EnsureEmbeddedSidecarExtracted extracts the SHA-256-verified embedded sidecar bundle into XDG_DATA_HOME
// by default (preventing CWD hijack in Production builds). Local `./sidecar` is only used when
// `MICHAEL_AUTO_DEV_SIDECAR=1` is explicitly set for live sidecar development (P1-2).
func EnsureEmbeddedSidecarExtracted() (string, error) {
	if os.Getenv("MICHAEL_AUTO_DEV_SIDECAR") == "1" {
		devCandidate := filepath.Join("sidecar", "michael_cua_sidecar", "main.py")
		if _, err := os.Stat(devCandidate); err == nil {
			if abs, absErr := filepath.Abs(devCandidate); absErr == nil {
				return abs, nil
			}
			return devCandidate, nil
		}
	}

	if entry, err := sidecar.ExtractTo(ManagedSidecarDir()); err == nil && entry != "" {
		return entry, nil
	}

	if exe, err := os.Executable(); err == nil {
		adj := filepath.Join(filepath.Dir(exe), "sidecar", "michael_cua_sidecar", "main.py")
		if _, statErr := os.Stat(adj); statErr == nil {
			return adj, nil
		}
	}
	devCandidate := filepath.Join("sidecar", "michael_cua_sidecar", "main.py")
	if _, err := os.Stat(devCandidate); err == nil {
		if abs, absErr := filepath.Abs(devCandidate); absErr == nil {
			return abs, nil
		}
		return devCandidate, nil
	}
	return "", fmt.Errorf("không thể giải nén hoặc định vị CUA sidecar")
}

// ProvisionSidecarVenv creates an isolated Python virtual environment under $XDG_DATA_HOME/michael-auto/venv
// and installs binary wheels for `cua-driver` plus the embedded `michael-auto-cua-sidecar` package.
// On bleeding-edge Fedora 45 hosts running Python 3.15+ without `cp315` wheels, it automatically bootstraps
// a SHA-256-verified user-space `uv` binary to provision a standalone Python 3.12 runtime!
func ProvisionSidecarVenv(ctx context.Context) (string, string, error) {
	sidecarRoot := ManagedSidecarDir()
	if _, err := sidecar.ExtractTo(sidecarRoot); err != nil {
		return "", "", fmt.Errorf("giải nén sidecar nhúng lỗi: %w", err)
	}

	venvDir := filepath.Join(DefaultXDGDataDir(), "venv")
	venvPy := filepath.Join(venvDir, "bin", "python")

	if err := os.MkdirAll(filepath.Dir(venvDir), 0o700); err != nil {
		return "", "", err
	}

	cuaPkgSpec := strings.TrimSpace(os.Getenv("CUA_DRIVER_PKG_SPEC"))
	if cuaPkgSpec == "" {
		cuaPkgSpec = pinnedCuaDriverSpec
	}

	var logBuf bytes.Buffer
	uvBin := resolveUvBinary()

	// Step 1: Create initial venv (preferring uv + Python 3.12 when already available, else stdlib python).
	if _, err := os.Stat(venvPy); err != nil {
		createdWithUv := false
		if uvBin != "" {
			if err := createUvPython312Venv(ctx, uvBin, venvDir, &logBuf); err == nil {
				createdWithUv = true
			} else {
				logBuf.Reset()
			}
		}
		if !createdWithUv {
			if err := createStdlibVenv(ctx, venvDir, &logBuf); err != nil {
				return "", logBuf.String(), err
			}
		}
	}

	installErr := installWheelAndSidecarIntoVenv(ctx, venvPy, cuaPkgSpec, sidecarRoot, &logBuf)
	if installErr == nil {
		return venvPy, "Đã khởi tạo môi trường ảo và cài đặt cua-driver thành công tại " + venvPy, nil
	}

	// Step 2 (Fedora 45 Python 3.14+/3.15+ Auto-Recovery):
	// If binary wheel installation failed on bleeding-edge system Python (e.g. Python 3.15.2 lacking cp315 wheels),
	// automatically bootstrap SHA-256-verified `uv` into $XDG_DATA_HOME/michael-auto/bin/uv and rebuild venv with Python 3.12!
	if minor := detectPythonMinorVersion(ctx, venvPy); minor >= 14 {
		if uvBin == "" {
			bootstrappedUv, dlErr := bootstrapVerifiedUvBinary(ctx, ManagedUvBinPath())
			if dlErr != nil {
				return venvPy, tailString(logBuf.String(), 600), fmt.Errorf(
					"Python 3.%d chưa có wheel nhị phân cho %s và tải tự động uv lỗi (%v) — hãy chạy 'sudo dnf install python3.12'",
					minor, cuaPkgSpec, dlErr,
				)
			}
			uvBin = bootstrappedUv
		}

		_ = os.RemoveAll(venvDir)
		logBuf.Reset()
		if err := createUvPython312Venv(ctx, uvBin, venvDir, &logBuf); err != nil {
			return "", tailString(logBuf.String(), 600), fmt.Errorf("dựng Python 3.12 qua uv lỗi: %w", err)
		}
		if err := installWheelAndSidecarIntoVenv(ctx, venvPy, cuaPkgSpec, sidecarRoot, &logBuf); err != nil {
			return venvPy, tailString(logBuf.String(), 600), err
		}
		return venvPy, "Đã tự động bootstrap Python 3.12 qua uv (SHA-256) và cài đặt cua-driver tại " + venvPy, nil
	}

	return venvPy, tailString(logBuf.String(), 600), installErr
}

func resolveUvBinary() string {
	if p, err := exec.LookPath("uv"); err == nil {
		return p
	}
	managed := ManagedUvBinPath()
	if fileExecutable(managed) {
		return managed
	}
	return ""
}

func createUvPython312Venv(ctx context.Context, uvBin, venvDir string, out *bytes.Buffer) error {
	cmd := exec.CommandContext(ctx, uvBin, "venv", "--python", "3.12", "--seed", venvDir)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

func installWheelAndSidecarIntoVenv(
	ctx context.Context,
	venvPy string,
	cuaPkgSpec string,
	sidecarRoot string,
	logBuf *bytes.Buffer,
) error {
	ensurePipCmd := exec.CommandContext(ctx, venvPy, "-m", "ensurepip", "--upgrade")
	ensurePipCmd.Stdout = logBuf
	ensurePipCmd.Stderr = logBuf
	_ = ensurePipCmd.Run()

	installCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	wheelCmd := exec.CommandContext(installCtx, venvPy, "-m", "pip", "install", "--only-binary=:all:", cuaPkgSpec)
	wheelCmd.Stdout = logBuf
	wheelCmd.Stderr = logBuf
	if err := wheelCmd.Run(); err != nil {
		return fmt.Errorf("cài đặt binary wheel %s thất bại: %w", cuaPkgSpec, err)
	}

	sidecarCmd := exec.CommandContext(installCtx, venvPy, "-m", "pip", "install", "--no-deps", sidecarRoot)
	sidecarCmd.Stdout = logBuf
	sidecarCmd.Stderr = logBuf
	if err := sidecarCmd.Run(); err != nil {
		return fmt.Errorf("cài đặt gói michael_cua_sidecar vào venv thất bại: %w", err)
	}
	return nil
}

func detectPythonMinorVersion(ctx context.Context, pyBin string) int {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, pyBin, "-c", "import sys; print(sys.version_info.minor)").Output()
	if err != nil {
		return 0
	}
	minor, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return minor
}

// bootstrapVerifiedUvBinary downloads the official Astral uv release tarball for Linux, verifies its
// SHA-256 digest against the official .sha256 checksum file, and extracts `uv` with 0700 permissions.
func bootstrapVerifiedUvBinary(ctx context.Context, destBin string) (string, error) {
	return bootstrapVerifiedUvFromBaseURL(ctx, "https://github.com/astral-sh/uv/releases/download/"+pinnedUvVersion, destBin)
}

func bootstrapVerifiedUvFromBaseURL(ctx context.Context, baseURL string, destBin string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("chỉ hỗ trợ tự tải uv trên Linux (hiện tại: %s)", runtime.GOOS)
	}

	var targetTriple string
	switch runtime.GOARCH {
	case "amd64":
		targetTriple = "x86_64-unknown-linux-gnu"
	case "arm64":
		targetTriple = "aarch64-unknown-linux-gnu"
	default:
		return "", fmt.Errorf("kiến trúc CPU %s chưa hỗ trợ tải tự động uv", runtime.GOARCH)
	}

	archiveName := fmt.Sprintf("uv-%s.tar.gz", targetTriple)
	archiveURL := strings.TrimRight(baseURL, "/") + "/" + archiveName
	shaURL := archiveURL + ".sha256"

	dlCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	expectedHex, err := fetchExpectedSHA256(dlCtx, shaURL)
	if err != nil {
		return "", err
	}

	archiveBytes, err := fetchBytesLimited(dlCtx, archiveURL, maxUvArchiveLen)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(archiveBytes)
	actualHex := hex.EncodeToString(sum[:])
	if !strings.EqualFold(actualHex, expectedHex) {
		return "", fmt.Errorf("kiểm tra SHA-256 gói uv thất bại: mong đợi %s, nhận được %s", expectedHex, actualHex)
	}

	uvBytes, err := extractUvBinaryFromTarGz(archiveBytes)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(destBin), 0o700); err != nil {
		return "", err
	}
	tmpPath := destBin + ".tmp"
	if err := os.WriteFile(tmpPath, uvBytes, 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, destBin); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return destBin, nil
}

func fetchExpectedSHA256(ctx context.Context, url string) (string, error) {
	raw, err := fetchBytesLimited(ctx, url, 4096)
	if err != nil {
		return "", fmt.Errorf("tải tệp kiểm tra SHA-256 lỗi: %w", err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", fmt.Errorf("nội dung SHA-256 không hợp lệ từ %s", url)
	}
	return strings.ToLower(fields[0]), nil
}

func fetchBytesLimited(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d khi tải %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}

func extractUvBinaryFromTarGz(archiveBytes []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == "uv" {
			return io.ReadAll(io.LimitReader(tr, maxUvArchiveLen))
		}
	}
	return nil, fmt.Errorf("không tìm thấy thực thi 'uv' bên trong gói nén")
}

func createStdlibVenv(ctx context.Context, venvDir string, out *bytes.Buffer) error {
	candidates := []string{"python3.12", "python3.13", "python3.14", "python3", "python"}
	var basePy string
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			basePy = p
			break
		}
	}
	if basePy == "" {
		return fmt.Errorf("không tìm thấy trình thông dịch python3 trên hệ thống")
	}

	cmd := exec.CommandContext(ctx, basePy, "-m", "venv", venvDir)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tạo virtualenv bằng %s lỗi: %w", basePy, err)
	}
	return nil
}

func tailString(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return "..." + s[len(s)-maxLen:]
}

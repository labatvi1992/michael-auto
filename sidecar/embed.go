package sidecar

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Bundle embeds the Python CUA sidecar sources directly into the Go executable
// so `michael-auto` works as a self-contained single binary from ~/.local/bin or autostart.
//
//go:embed pyproject.toml michael_cua_sidecar/*.py
var Bundle embed.FS

const hashMarkerFile = ".bundle_sha256"

// ExtractTo ensures the embedded Python sidecar is extracted and up-to-date inside destRoot
// (typically $XDG_DATA_HOME/michael-auto/sidecar) and returns the absolute path to main.py.
func ExtractTo(destRoot string) (string, error) {
	if strings.TrimSpace(destRoot) == "" {
		return "", fmt.Errorf("đường dẫn giải nén sidecar không hợp lệ")
	}

	expectedHash, files, err := computeBundleDigest()
	if err != nil {
		return "", err
	}

	entrypoint := filepath.Join(destRoot, "michael_cua_sidecar", "main.py")
	markerPath := filepath.Join(destRoot, hashMarkerFile)

	if existingHash, err := os.ReadFile(markerPath); err == nil {
		if strings.TrimSpace(string(existingHash)) == expectedHash {
			if _, statErr := os.Stat(entrypoint); statErr == nil {
				return entrypoint, nil
			}
		}
	}

	if err := os.MkdirAll(filepath.Join(destRoot, "michael_cua_sidecar"), 0o700); err != nil {
		return "", fmt.Errorf("không tạo được thư mục sidecar %s: %w", destRoot, err)
	}

	for _, relPath := range files {
		raw, readErr := Bundle.ReadFile(relPath)
		if readErr != nil {
			return "", fmt.Errorf("đọc tệp nhúng %s lỗi: %w", relPath, readErr)
		}
		outPath := filepath.Join(destRoot, filepath.FromSlash(relPath))
		if mkErr := os.MkdirAll(filepath.Dir(outPath), 0o700); mkErr != nil {
			return "", mkErr
		}
		perm := os.FileMode(0o600)
		if strings.HasSuffix(relPath, "main.py") {
			perm = 0o700
		}
		tmpPath := outPath + ".tmp"
		if writeErr := os.WriteFile(tmpPath, raw, perm); writeErr != nil {
			return "", fmt.Errorf("ghi tệp sidecar %s lỗi: %w", outPath, writeErr)
		}
		if renErr := os.Rename(tmpPath, outPath); renErr != nil {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("cập nhật nguyên tử %s lỗi: %w", outPath, renErr)
		}
	}

	_ = os.WriteFile(markerPath, []byte(expectedHash+"\n"), 0o600)
	return entrypoint, nil
}

func computeBundleDigest() (string, []string, error) {
	var files []string
	err := fs.WalkDir(Bundle, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	sort.Strings(files)

	h := sha256.New()
	for _, f := range files {
		raw, err := Bundle.ReadFile(f)
		if err != nil {
			return "", nil, err
		}
		_, _ = h.Write([]byte(f))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(raw)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), files, nil
}

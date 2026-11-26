package actuators

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

const defaultAuthTargetURL = "https://accounts.google.com"

// OSBrowserRouter launches an isolated browser profile directory (--profile-directory or -P)
// without shell interpolation, explicitly enabling renderer accessibility so trycua/cua
// can inspect and select pre-logged-in Google Accounts on Linux Wayland (ADR-004, ADR-007).
type OSBrowserRouter struct {
	lookPathFunc func(file string) (string, error)
	execRunner   func(ctx context.Context, binary string, args []string) error
}

// NewOSBrowserRouter creates a production BrowserProfileRouter with Wayland accessibility env vars.
func NewOSBrowserRouter() *OSBrowserRouter {
	return &OSBrowserRouter{
		lookPathFunc: exec.LookPath,
		execRunner: func(ctx context.Context, binary string, args []string) error {
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Env = append(
				os.Environ(),
				"ACCESSIBILITY_ENABLED=1",
				"GNOME_ACCESSIBILITY=1",
			)
			return cmd.Start()
		},
	}
}

// NewTestableBrowserRouter injects custom LookPath and runner callbacks for unit testing.
func NewTestableBrowserRouter(
	lookPath func(string) (string, error),
	runner func(context.Context, string, []string) error,
) *OSBrowserRouter {
	return &OSBrowserRouter{
		lookPathFunc: lookPath,
		execRunner:   runner,
	}
}

// BuildLaunchCommand validates profile arguments and returns the resolved binary and CLI arguments.
func (r *OSBrowserRouter) BuildLaunchCommand(profile domain.AccountProfile) (string, []string, error) {
	dir := strings.TrimSpace(profile.BrowserProfileDir)
	if dir == "" || strings.Contains(dir, "..") || strings.ContainsAny(dir, `/\;|$&`+"`") {
		return "", nil, fmt.Errorf("browserProfileDir không an toàn: %q", profile.BrowserProfileDir)
	}

	targetURL := strings.TrimSpace(profile.AuthStartURL)
	if targetURL == "" {
		targetURL = defaultAuthTargetURL
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil || (parsedURL.Scheme != "https" && parsedURL.Scheme != "http") {
		return "", nil, fmt.Errorf("authStartUrl không hợp lệ: %q", targetURL)
	}

	bin, err := r.resolveBinary(profile)
	if err != nil {
		return "", nil, err
	}

	if profile.BrowserType == domain.BrowserFirefox {
		return bin, []string{"-P", dir, "-new-tab", targetURL}, nil
	}

	// Chromium family on Fedora 45 Wayland:
	// --ozone-platform-hint=auto ensures clean native Wayland windowing;
	// --force-renderer-accessibility exposes DOM elements (Google Account rows) to cua-driver.
	args := []string{
		"--profile-directory=" + dir,
		"--ozone-platform-hint=auto",
		"--force-renderer-accessibility",
		"--new-window",
		targetURL,
	}
	return bin, args, nil
}

func (r *OSBrowserRouter) resolveBinary(profile domain.AccountProfile) (string, error) {
	if custom := strings.TrimSpace(profile.BrowserBinary); custom != "" {
		return r.lookPathFunc(custom)
	}

	var candidates []string
	switch profile.BrowserType {
	case domain.BrowserChrome:
		candidates = []string{"google-chrome-stable", "google-chrome"}
	case domain.BrowserChromium:
		candidates = []string{"chromium-browser", "chromium"}
	case domain.BrowserBrave:
		candidates = []string{"brave-browser", "brave"}
	case domain.BrowserEdge:
		candidates = []string{"microsoft-edge-stable", "microsoft-edge"}
	case domain.BrowserFirefox:
		candidates = []string{"firefox"}
	default:
		return "", fmt.Errorf("loại trình duyệt không hỗ trợ: %s", profile.BrowserType)
	}

	for _, c := range candidates {
		if path, err := r.lookPathFunc(c); err == nil && path != "" {
			return path, nil
		}
	}
	return "", errors.New("không tìm thấy tệp thực thi trình duyệt trên hệ thống (" + strings.Join(candidates, ", ") + ")")
}

// OpenProfileSession opens the target profile session cleanly unless dryRun is enabled.
func (r *OSBrowserRouter) OpenProfileSession(ctx context.Context, profile domain.AccountProfile, dryRun bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	bin, args, err := r.BuildLaunchCommand(profile)
	if err != nil {
		if dryRun && strings.Contains(err.Error(), "không tìm thấy tệp thực thi") {
			return nil
		}
		return err
	}
	if dryRun {
		return nil
	}
	return r.execRunner(ctx, bin, args)
}

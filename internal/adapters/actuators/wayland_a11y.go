package actuators

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	a11yAddrRegex      = regexp.MustCompile(`'([^']+)'`)
	a11yChildBusRegex  = regexp.MustCompile(`'(:[0-9.]+)'`)
	a11yIntRegex       = regexp.MustCompile(`(\d+)`)
	a11yQuotedStrRegex = regexp.MustCompile(`<'([^']*)'>`)
)

// CheckWaylandAccessibility inspects BOTH GNOME toolkit-accessibility AND live D-Bus
// `org.a11y.Status.ScreenReaderEnabled` so Electron (Antigravity) and multi-profile Chromium
// (Google Chrome) actively export their internal DOM trees over AT-SPI2 on Fedora 45 Wayland.
func CheckWaylandAccessibility(ctx context.Context) (enabled bool, detail string) {
	gsettingsBin, err := exec.LookPath("gsettings")
	if err != nil {
		return true, "Không dùng gsettings (dựa trên biến môi trường ACCESSIBILITY_ENABLED=1)"
	}

	out, err := exec.CommandContext(ctx, gsettingsBin, "get", "org.gnome.desktop.interface", "toolkit-accessibility").Output()
	if err != nil {
		return true, "Không đọc được khóa toolkit-accessibility (" + err.Error() + ")"
	}

	val := strings.TrimSpace(strings.ToLower(string(out)))
	if val != "true" {
		return false, "GNOME toolkit-accessibility=false (cần bật để Electron/Chrome xuất cây phần tử bên trong cửa sổ)"
	}

	// Crucial for Fedora 45 Wayland (#6): Electron & Chrome running under --ozone-platform=wayland
	// keep WebContents AXMode dormant unless `org.a11y.Status.ScreenReaderEnabled` is ALSO true!
	if gdbusBin, gErr := exec.LookPath("gdbus"); gErr == nil {
		srOut, srErr := exec.CommandContext(
			ctx,
			gdbusBin,
			"call", "--session",
			"--dest", "org.a11y.Bus",
			"--object-path", "/org/a11y/bus",
			"--method", "org.freedesktop.DBus.Properties.Get",
			"org.a11y.Status",
			"ScreenReaderEnabled",
		).Output()
		if srErr == nil && !strings.Contains(strings.ToLower(string(srOut)), "true") {
			return false, "GNOME toolkit-accessibility=true nhưng org.a11y.Status.ScreenReaderEnabled=false (chạy --setup-sidecar để đánh thức DOM Electron/Chrome)"
		}
	}

	return true, "GNOME toolkit-accessibility=true + ScreenReaderEnabled=true (Antigravity & Chrome xuất đầy đủ cây UI)"
}

// InspectATSPI2RegisteredApps returns the names of applications currently registered on the live AT-SPI2 bus.
func InspectATSPI2RegisteredApps(ctx context.Context) []string {
	gdbusBin, err := exec.LookPath("gdbus")
	if err != nil {
		return nil
	}

	addrOut, err := exec.CommandContext(
		ctx,
		gdbusBin,
		"call", "--session",
		"--dest", "org.a11y.Bus",
		"--object-path", "/org/a11y/bus",
		"--method", "org.a11y.Bus.GetAddress",
	).Output()
	if err != nil {
		return nil
	}
	m := a11yAddrRegex.FindStringSubmatch(string(addrOut))
	if len(m) < 2 || m[1] == "" {
		return nil
	}
	busAddr := m[1]

	cntOut, err := exec.CommandContext(
		ctx,
		gdbusBin,
		"call", "--address", busAddr,
		"--dest", "org.a11y.atspi.Registry",
		"--object-path", "/org/a11y/atspi/accessible/root",
		"--method", "org.freedesktop.DBus.Properties.Get",
		"org.a11y.atspi.Accessible",
		"ChildCount",
	).Output()
	if err != nil {
		return nil
	}
	cm := a11yIntRegex.FindString(string(cntOut))
	count, _ := strconv.Atoi(cm)
	if count <= 0 {
		return []string{}
	}
	if count > 32 {
		count = 32
	}

	var apps []string
	for i := 0; i < count; i++ {
		childOut, cErr := exec.CommandContext(
			ctx,
			gdbusBin,
			"call", "--address", busAddr,
			"--dest", "org.a11y.atspi.Registry",
			"--object-path", "/org/a11y/atspi/accessible/root",
			"--method", "org.a11y.atspi.Accessible.GetChildAtIndex",
			strconv.Itoa(i),
		).Output()
		if cErr != nil {
			continue
		}
		bm := a11yChildBusRegex.FindStringSubmatch(string(childOut))
		if len(bm) < 2 || bm[1] == "" {
			continue
		}
		nameOut, nErr := exec.CommandContext(
			ctx,
			gdbusBin,
			"call", "--address", busAddr,
			"--dest", bm[1],
			"--object-path", "/org/a11y/atspi/accessible/root",
			"--method", "org.freedesktop.DBus.Properties.Get",
			"org.a11y.atspi.Accessible",
			"Name",
		).Output()
		if nErr != nil {
			continue
		}
		if nm := a11yQuotedStrRegex.FindStringSubmatch(string(nameOut)); len(nm) == 2 && strings.TrimSpace(nm[1]) != "" {
			apps = append(apps, strings.TrimSpace(nm[1]))
		}
	}
	return apps
}

// EnableWaylandAccessibility turns on GNOME toolkit-accessibility, dynamically activates live AT-SPI
// IsEnabled + ScreenReaderEnabled on BOTH Session and Accessibility Buses (waking WebContents DOM trees
// in already-running Electron & Chrome processes without restart!), and ensures user-level flags files.
func EnableWaylandAccessibility(ctx context.Context) (string, error) {
	var actions []string

	if gsettingsBin, err := exec.LookPath("gsettings"); err == nil {
		if out, runErr := exec.CommandContext(
			ctx,
			gsettingsBin,
			"set",
			"org.gnome.desktop.interface",
			"toolkit-accessibility",
			"true",
		).CombinedOutput(); runErr != nil {
			return "", fmt.Errorf("gsettings set toolkit-accessibility lỗi: %w (%s)", runErr, strings.TrimSpace(string(out)))
		}
		actions = append(actions, "toolkit-accessibility=true")
	}

	if gdbusBin, err := exec.LookPath("gdbus"); err == nil {
		for _, prop := range []string{"IsEnabled", "ScreenReaderEnabled"} {
			_ = exec.CommandContext(
				ctx,
				gdbusBin,
				"call", "--session",
				"--dest", "org.a11y.Bus",
				"--object-path", "/org/a11y/bus",
				"--method", "org.freedesktop.DBus.Properties.Set",
				"org.a11y.Status",
				prop,
				"<true>",
			).Run()
		}

		// Also broadcast on the Accessibility Bus socket if it exposes org.a11y.Status.
		if addrOut, err := exec.CommandContext(
			ctx,
			gdbusBin,
			"call", "--session",
			"--dest", "org.a11y.Bus",
			"--object-path", "/org/a11y/bus",
			"--method", "org.a11y.Bus.GetAddress",
		).Output(); err == nil {
			if m := a11yAddrRegex.FindStringSubmatch(string(addrOut)); len(m) == 2 && m[1] != "" {
				for _, prop := range []string{"IsEnabled", "ScreenReaderEnabled"} {
					_ = exec.CommandContext(
						ctx,
						gdbusBin,
						"call", "--address", m[1],
						"--dest", "org.a11y.Bus",
						"--object-path", "/org/a11y/bus",
						"--method", "org.freedesktop.DBus.Properties.Set",
						"org.a11y.Status",
						prop,
						"<true>",
					).Run()
				}
			}
		}
		actions = append(actions, "org.a11y.Status(IsEnabled+ScreenReaderEnabled)=true")
	}

	if ensureUserChromiumAccessibilityFlags("") {
		actions = append(actions, "chrome/electron-flags.conf(--force-renderer-accessibility)")
	}
	if ensureSystemdSessionAccessibilityEnv(ctx) {
		actions = append(actions, "systemd/dbus(ACCESSIBILITY_ENABLED=1)")
	}

	if len(actions) == 0 {
		return "Đã cấu hình biến môi trường ACCESSIBILITY_ENABLED=1 cho phiên làm việc", nil
	}
	return "Đã bật nóng thành công Accessibility trên Wayland (" + strings.Join(actions, ", ") + ")", nil
}

func ensureUserChromiumAccessibilityFlags(customConfigDir string) bool {
	base := strings.TrimSpace(customConfigDir)
	if base == "" {
		base = strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
		if base == "" {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				base = filepath.Join(home, ".config")
			}
		}
	}
	if base == "" {
		return false
	}

	updated := false
	const flagLine = "--force-renderer-accessibility"
	for _, name := range []string{"chrome-flags.conf", "electron-flags.conf"} {
		p := filepath.Join(base, name)
		existing, _ := os.ReadFile(p)
		if strings.Contains(string(existing), flagLine) {
			updated = true
			continue
		}
		var newContent string
		if len(existing) == 0 {
			newContent = flagLine + "\n"
		} else {
			trimmed := strings.TrimRight(string(existing), "\n")
			newContent = trimmed + "\n" + flagLine + "\n"
		}
		if err := os.MkdirAll(base, 0o700); err == nil {
			if os.WriteFile(p, []byte(newContent), 0o600) == nil {
				updated = true
			}
		}
	}
	return updated
}

func ensureSystemdSessionAccessibilityEnv(ctx context.Context) bool {
	updated := false
	base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, ".config")
		}
	}
	if base != "" {
		envDir := filepath.Join(base, "environment.d")
		if os.MkdirAll(envDir, 0o700) == nil {
			envContent := "ACCESSIBILITY_ENABLED=1\nGNOME_ACCESSIBILITY=1\nQT_LINUX_ACCESSIBILITY_ALWAYS_ON=1\n"
			if os.WriteFile(filepath.Join(envDir, "90-michael-auto-a11y.conf"), []byte(envContent), 0o600) == nil {
				updated = true
			}
		}
	}

	if dbusUpd, err := exec.LookPath("dbus-update-activation-environment"); err == nil {
		if exec.CommandContext(ctx, dbusUpd, "--systemd", "ACCESSIBILITY_ENABLED=1", "GNOME_ACCESSIBILITY=1").Run() == nil {
			updated = true
		}
	} else if sysctl, sErr := exec.LookPath("systemctl"); sErr == nil {
		if exec.CommandContext(ctx, sysctl, "--user", "set-environment", "ACCESSIBILITY_ENABLED=1", "GNOME_ACCESSIBILITY=1").Run() == nil {
			updated = true
		}
	}
	return updated
}

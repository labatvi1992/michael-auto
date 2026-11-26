//go:build linux

package workspace

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

const (
	GnomeBridgeExtensionUUID = "michael-auto-workspace@michael-auto"
	GnomeBridgeDBusObject    = "/io/michael_auto/WorkspaceBridge"
	GnomeBridgeDBusInterface = "io.michael_auto.WorkspaceBridge"
)

var (
	gnomeVerDigits    = regexp.MustCompile(`\b(\d{2})\b`)
	gdbusInt32Regex   = regexp.MustCompile(`(-?\d+)`)
	gsettingsQuoteReg = regexp.MustCompile(`'([^']*)'`)
)

const gnomeShellExtensionJS = `import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import Shell from 'gi://Shell';
import {Extension} from 'resource:///org/gnome/shell/extensions/extension.js';

const IFACE_XML = ` + "`" + `
<node>
  <interface name="io.michael_auto.WorkspaceBridge">
    <method name="MoveByPid">
      <arg type="u" direction="in" name="pid"/>
      <arg type="u" direction="in" name="workspaceIndex"/>
      <arg type="u" direction="out" name="movedCount"/>
    </method>
    <method name="MoveByTitle">
      <arg type="s" direction="in" name="keyword"/>
      <arg type="u" direction="in" name="workspaceIndex"/>
      <arg type="u" direction="out" name="movedCount"/>
    </method>
    <method name="ActivateByPid">
      <arg type="u" direction="in" name="pid"/>
      <arg type="b" direction="out" name="activated"/>
    </method>
    <method name="CaptureWindowPng">
      <arg type="u" direction="in" name="pid"/>
      <arg type="s" direction="in" name="outPath"/>
      <arg type="b" direction="out" name="captured"/>
    </method>
    <method name="GetWindowFrame">
      <arg type="u" direction="in" name="pid"/>
      <arg type="i" direction="out" name="x"/>
      <arg type="i" direction="out" name="y"/>
      <arg type="i" direction="out" name="width"/>
      <arg type="i" direction="out" name="height"/>
      <arg type="i" direction="out" name="windowWorkspace"/>
      <arg type="i" direction="out" name="activeWorkspace"/>
    </method>
    <method name="ResolveAgentWorkspace">
      <arg type="s" direction="in" name="expectedName"/>
      <arg type="i" direction="out" name="workspaceIndex"/>
      <arg type="b" direction="out" name="found"/>
    </method>
    <method name="GetActiveWorkspace">
      <arg type="i" direction="out" name="activeIndex"/>
    </method>
    <method name="ArmAutoRoute">
      <arg type="u" direction="in" name="ttlSeconds"/>
      <arg type="u" direction="in" name="workspaceIndex"/>
      <arg type="b" direction="out" name="armed"/>
    </method>
  </interface>
</node>` + "`" + `;

const AUTO_ROUTE_TITLE_MARKERS = [
  'google accounts',
  'sign in',
  'choose an account',
  'chọn tài khoản',
  'accounts.google.com',
  'antigravity',
];

const AUTO_ROUTE_WMCLASS_MARKERS = [
  'google-chrome',
  'chromium',
  'brave-browser',
  'microsoft-edge',
  'antigravity',
];

function _isAllowedEvidencePath(outPath) {
  if (!outPath || typeof outPath !== 'string') return false;
  if (outPath.includes('..') || !outPath.endsWith('.png')) return false;
  const runtimeDir = GLib.get_user_runtime_dir();
  if (!runtimeDir) return false;
  const allowedPrefix = runtimeDir.replace(/\/+$/, '') + '/michael-auto/';
  if (!outPath.startsWith(allowedPrefix)) return false;
  const tail = outPath.slice(allowedPrefix.length);
  if (!tail || tail.includes('/')) return false;
  return true;
}

class WorkspaceBridgeService {
  constructor() {
    this._autoRouteUntilMs = 0;
    this._autoRouteWsIdx = -1;
  }

  dispose() {
    this._autoRouteUntilMs = 0;
    this._autoRouteWsIdx = -1;
  }

  ArmAutoRoute(ttlSeconds, workspaceIndex) {
    if (ttlSeconds <= 0) {
      this._autoRouteUntilMs = 0;
      this._autoRouteWsIdx = -1;
      return true;
    }
    const clampedTtl = Math.min(Math.max(ttlSeconds, 1), 60);
    const nWs = global.workspace_manager.get_n_workspaces();
    if (workspaceIndex < 0 || (nWs > 0 && workspaceIndex >= nWs)) {
      return false;
    }
    this._autoRouteWsIdx = workspaceIndex;
    this._autoRouteUntilMs = Date.now() + clampedTtl * 1000;
    return true;
  }

  _matchesAutoRouteWindow(win) {
    if (!win) return false;
    const title = (win.get_title() || '').toLowerCase();
    const wmClass = (win.get_wm_class() || '').toLowerCase();
    for (const marker of AUTO_ROUTE_TITLE_MARKERS) {
      if (title.includes(marker)) return true;
    }
    for (const cls of AUTO_ROUTE_WMCLASS_MARKERS) {
      if (wmClass.includes(cls)) return true;
    }
    return false;
  }

  handleWindowCreated(win) {
    if (!win || this._autoRouteWsIdx < 0 || Date.now() >= this._autoRouteUntilMs) {
      return;
    }
    const targetWs = this._autoRouteWsIdx;
    if (this._matchesAutoRouteWindow(win)) {
      try {
        win.change_workspace_by_index(targetWs, false);
      } catch (_e) {}
      return;
    }
    // Fallback if title/wm_class populates right after initial MetaWindow creation.
    const sigId = win.connect('notify::title', () => {
      if (Date.now() >= this._autoRouteUntilMs) {
        try { win.disconnect(sigId); } catch (_e) {}
        return;
      }
      if (this._matchesAutoRouteWindow(win)) {
        try {
          win.change_workspace_by_index(targetWs, false);
          win.disconnect(sigId);
        } catch (_e) {}
      }
    });
    win.connect('unmanaged', () => {
      try { win.disconnect(sigId); } catch (_e) {}
    });
  }

  ResolveAgentWorkspace(expectedName) {
    const wanted = (expectedName || 'Agent Workspace').trim().toLowerCase();
    try {
      const prefs = new Gio.Settings({schema_id: 'org.gnome.desktop.wm.preferences'});
      const names = prefs.get_strv('workspace-names') || [];
      for (let i = 0; i < names.length; i++) {
        if ((names[i] || '').trim().toLowerCase() === wanted) {
          return [i, true];
        }
      }
    } catch (_e) {
      // Fall through if schema unavailable.
    }
    return [-1, false];
  }

  MoveByPid(pid, workspaceIndex) {
    let moved = 0;
    const actors = global.get_window_actors();
    for (const actor of actors) {
      const win = actor.get_meta_window();
      if (win && win.get_pid() === pid) {
        win.change_workspace_by_index(workspaceIndex, false);
        moved++;
      }
    }
    return moved;
  }

  MoveByTitle(keyword, workspaceIndex) {
    const needle = (keyword || '').trim().toLowerCase();
    if (!needle) return 0;
    let moved = 0;
    const actors = global.get_window_actors();
    for (const actor of actors) {
      const win = actor.get_meta_window();
      if (!win) continue;
      const title = (win.get_title() || '').toLowerCase();
      const wmClass = (win.get_wm_class() || '').toLowerCase();
      if (title.includes(needle) || wmClass.includes(needle)) {
        win.change_workspace_by_index(workspaceIndex, false);
        moved++;
      }
    }
    return moved;
  }

  ActivateByPid(pid) {
    const actors = global.get_window_actors();
    for (const actor of actors) {
      const win = actor.get_meta_window();
      if (win && win.get_pid() === pid) {
        win.activate(global.get_current_time());
        return true;
      }
    }
    return false;
  }

  GetWindowFrame(pid) {
    const activeIdx = global.workspace_manager.get_active_workspace_index();
    if (pid > 0) {
      for (const actor of global.get_window_actors()) {
        const win = actor.get_meta_window();
        if (win && win.get_pid() === pid) {
          const rect = win.get_frame_rect();
          const ws = win.get_workspace();
          const winWs = ws ? ws.index() : -1;
          return [rect.x, rect.y, rect.width, rect.height, winWs, activeIdx];
        }
      }
    }
    return [0, 0, 0, 0, -1, activeIdx];
  }

  async CaptureWindowPngAsync(params, invocation) {
    const [pid, outPath] = params;
    let stream = null;
    try {
      // P0-1 Security Hardening: restrict writes strictly to $XDG_RUNTIME_DIR/michael-auto/*.png and reject symlinks.
      if (!_isAllowedEvidencePath(outPath) || pid <= 0) {
        invocation.return_value(new GLib.Variant('(b)', [false]));
        return;
      }

      const file = Gio.File.new_for_path(outPath);
      if (file.query_exists(null)) {
        const info = file.query_info('standard::is-symlink', Gio.FileQueryInfoFlags.NOFOLLOW_SYMLINKS, null);
        if (info && info.get_is_symlink()) {
          invocation.return_value(new GLib.Variant('(b)', [false]));
          return;
        }
      }

      let targetActor = null;
      let targetWin = null;
      for (const actor of global.get_window_actors()) {
        const win = actor.get_meta_window();
        if (win && win.get_pid() === pid) {
          targetActor = actor;
          targetWin = win;
          break;
        }
      }
      if (!targetActor || !targetWin) {
        invocation.return_value(new GLib.Variant('(b)', [false]));
        return;
      }

      stream = file.replace(null, false, Gio.FileCreateFlags.PRIVATE, null);
      let captured = false;

      // Primary P0-1 Isolation Path: render target Meta.WindowActor directly to an offscreen Cogl.Texture
      // so windows inside dedicated "Agent Workspace" are captured cleanly WITHOUT EVER capturing User Workspace #1!
      if (typeof targetActor.paint_to_content === 'function' && typeof Shell.Screenshot.composite_to_stream === 'function') {
        try {
          const content = targetActor.paint_to_content(null);
          const texture = content && typeof content.get_texture === 'function' ? content.get_texture() : null;
          if (texture) {
            const texW = texture.get_width();
            const texH = texture.get_height();
            if (texW > 0 && texH > 0) {
              const scale = global.display.get_monitor_scale(targetWin.get_monitor()) || 1.0;
              await new Promise((resolve, reject) => {
                Shell.Screenshot.composite_to_stream(
                  texture, 0, 0, texW, texH, scale,
                  null, 0, 0, 1.0,
                  stream,
                  (_src, res) => {
                    try {
                      Shell.Screenshot.composite_to_stream_finish(res);
                      resolve(true);
                    } catch (err) {
                      reject(err);
                    }
                  }
                );
              });
              captured = true;
            }
          }
        } catch (_offscreenErr) {
          captured = false;
        }
      }

      // Safe Fallback: ONLY permit compositor stage screenshot_area when the target window IS on the
      // currently active workspace. Fail-closed otherwise so User Workspace #1 pixels can NEVER leak!
      if (!captured) {
        const winWs = targetWin.get_workspace();
        const winWsIdx = winWs ? winWs.index() : -1;
        const activeWsIdx = global.workspace_manager.get_active_workspace_index();
        const rect = targetWin.get_frame_rect();
        if (winWsIdx >= 0 && winWsIdx === activeWsIdx && rect && rect.width > 0 && rect.height > 0) {
          const shooter = new Shell.Screenshot();
          await shooter.screenshot_area(rect.x, rect.y, rect.width, rect.height, stream);
          captured = true;
        }
      }

      if (stream) {
        stream.close(null);
        stream = null;
      }
      if (!captured) {
        try { file.delete(null); } catch (_delErr) {}
      }
      invocation.return_value(new GLib.Variant('(b)', [captured]));
    } catch (_e) {
      if (stream) {
        try { stream.close(null); } catch (_ce) {}
      }
      invocation.return_value(new GLib.Variant('(b)', [false]));
    }
  }

  GetActiveWorkspace() {
    return global.workspace_manager.get_active_workspace_index();
  }
}

export default class MichaelAutoWorkspaceExtension extends Extension {
  enable() {
    this._service = new WorkspaceBridgeService();
    this._dbus = Gio.DBusExportedObject.wrapJSObject(IFACE_XML, this._service);
    this._dbus.export(Gio.DBus.session, '/io/michael_auto/WorkspaceBridge');
    this._winCreatedId = global.display.connect('window-created', (_display, win) => {
      if (this._service) {
        this._service.handleWindowCreated(win);
      }
    });
  }

  disable() {
    if (this._winCreatedId) {
      global.display.disconnect(this._winCreatedId);
      this._winCreatedId = 0;
    }
    if (this._service) {
      this._service.dispose();
    }
    if (this._dbus) {
      this._dbus.unexport();
      this._dbus = null;
    }
    this._service = null;
  }
}
`

// WaylandWorkspaceManager provisions and routes automation windows onto a dedicated, named
// "Agent Workspace" on Fedora 45 Wayland (GNOME Mutter native Wayland D-Bus bridge + XWayland fallbacks)
// so periodic 1-minute Quota checks and Chrome OAuth selections stay strictly isolated from the User's workspace.
type WaylandWorkspaceManager struct {
	mu              sync.Mutex
	cachedActiveIdx int
	cachedKnown     bool
	cachedAt        time.Time
	extBaseDir      string
	lookPath        func(string) (string, error)
	runCmd          func(ctx context.Context, name string, args ...string) (string, error)
}

// NewWorkspaceManager constructs a production Fedora 45 Wayland workspace manager.
func NewWorkspaceManager() *WaylandWorkspaceManager {
	return &WaylandWorkspaceManager{
		extBaseDir: defaultGnomeExtensionsDir(),
		lookPath:   exec.LookPath,
		runCmd: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return strings.TrimSpace(string(out)), err
		},
	}
}

// NewTestableWorkspaceManager constructs a WaylandWorkspaceManager with injected command runners and extension dir.
func NewTestableWorkspaceManager(
	lookPath func(string) (string, error),
	runCmd func(ctx context.Context, name string, args ...string) (string, error),
) *WaylandWorkspaceManager {
	return &WaylandWorkspaceManager{
		extBaseDir: filepath.Join(os.TempDir(), "michael-auto-test-gnome-ext"),
		lookPath:   lookPath,
		runCmd:     runCmd,
	}
}

// SetExtensionBaseDir overrides where the user-space GNOME Shell extension is written (used in tests).
func (m *WaylandWorkspaceManager) SetExtensionBaseDir(dir string) {
	if strings.TrimSpace(dir) != "" {
		m.extBaseDir = dir
	}
}

func defaultGnomeExtensionsDir() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "gnome-shell", "extensions")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "gnome-shell", "extensions")
	}
	return filepath.Join(os.TempDir(), "gnome-shell-extensions")
}

func canonicalWorkspaceName(cfg domain.AppConfig) string {
	name := strings.TrimSpace(cfg.CuaDriver.AgentWorkspaceName)
	if name == "" || strings.EqualFold(name, "Michael-Auto-Agent") {
		return domain.DefaultAgentWorkspaceName
	}
	return name
}

// resolveAgentWorkspaceIndex resolves the exact 0-based workspace index matching canonicalWorkspaceName
// from GNOME workspace-names (or D-Bus ResolveAgentWorkspace), falling back to cfg.CuaDriver.AgentWorkspaceIndex.
func (m *WaylandWorkspaceManager) resolveAgentWorkspaceIndex(ctx context.Context, cfg domain.AppConfig) (int, bool) {
	name := canonicalWorkspaceName(cfg)

	// Tier 1: Direct GNOME Shell D-Bus ResolveAgentWorkspace (most authoritative live view).
	if gdbusBin, err := m.lookPath("gdbus"); err == nil {
		qCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
		out, callErr := m.runCmd(
			qCtx,
			gdbusBin,
			"call", "--session",
			"--dest", "org.gnome.Shell",
			"--object-path", GnomeBridgeDBusObject,
			"--method", GnomeBridgeDBusInterface+".ResolveAgentWorkspace",
			name,
		)
		cancel()
		if callErr == nil && strings.Contains(out, "true") {
			if match := gdbusInt32Regex.FindString(out); match != "" {
				if idx, convErr := strconv.Atoi(match); convErr == nil && idx >= 0 {
					return idx, true
				}
			}
		}
	}

	// Tier 2: gsettings org.gnome.desktop.wm.preferences workspace-names exact name lookup.
	if gsettingsBin, err := m.lookPath("gsettings"); err == nil {
		if namesOut, nErr := m.runCmd(ctx, gsettingsBin, "get", "org.gnome.desktop.wm.preferences", "workspace-names"); nErr == nil {
			names := parseGSettingsWorkspaceNames(namesOut)
			for i, item := range names {
				if strings.EqualFold(strings.TrimSpace(item), name) {
					return i, true
				}
			}
		}
	}

	fallbackIdx := cfg.CuaDriver.AgentWorkspaceIndex
	if fallbackIdx < 1 {
		fallbackIdx = 1
	}
	return fallbackIdx, false
}

// Status inspects current workspace isolation readiness truthfully:
// Ready is true ONLY when static workspaces include the dedicated named "Agent Workspace" AND a live
// window-routing mechanism (Native GNOME Wayland D-Bus bridge, wmctrl, or xdotool) is active.
func (m *WaylandWorkspaceManager) Status(ctx context.Context, cfg domain.AppConfig) domain.AgentWorkspaceStatus {
	name := canonicalWorkspaceName(cfg)
	idx, matchedByName := m.resolveAgentWorkspaceIndex(ctx, cfg)

	st := domain.AgentWorkspaceStatus{
		Enabled:        cfg.CuaDriver.AgentWorkspaceEnabled,
		WorkspaceIndex: idx,
		WorkspaceName:  name,
	}
	if !st.Enabled {
		st.Ready = false
		st.Detail = "Agent Workspace đang tắt theo cấu hình"
		return st
	}

	gsettingsBin, err := m.lookPath("gsettings")
	if err != nil {
		st.Ready = true
		st.Detail = fmt.Sprintf("Chế độ %q (#%d) sẵn sàng (compositor không dùng gsettings)", name, idx+1)
		return st
	}

	dynOut, dynErr := m.runCmd(ctx, gsettingsBin, "get", "org.gnome.mutter", "dynamic-workspaces")
	numOut, numErr := m.runCmd(ctx, gsettingsBin, "get", "org.gnome.desktop.wm.preferences", "num-workspaces")
	namesOut, _ := m.runCmd(ctx, gsettingsBin, "get", "org.gnome.desktop.wm.preferences", "workspace-names")

	numWS, _ := strconv.Atoi(strings.TrimSpace(numOut))
	isStatic := dynErr == nil && strings.TrimSpace(dynOut) == "false"
	hasEnough := numErr == nil && numWS >= idx+1
	hasLabel := matchedByName || containsWorkspaceName(parseGSettingsWorkspaceNames(namesOut), name)
	bridgeLive, bridgeOffscreen, bridgeAutoRoute := m.inspectGnomeWaylandBridge(ctx)
	_, hasWmctrl := m.lookPath("wmctrl")
	_, hasXdotool := m.lookPath("xdotool")
	extInstalled := m.isExtensionWritten()
	canRouteWindows := bridgeLive || hasWmctrl == nil || hasXdotool == nil

	if isStatic && hasEnough && hasLabel {
		switch {
		case bridgeLive:
			st.Ready = true
			if bridgeOffscreen && bridgeAutoRoute {
				st.Detail = fmt.Sprintf("Đã dành riêng %q (Workspace #%d) + Cầu nối Native Wayland v4 (Offscreen + ArmAutoRoute) đang hoạt động", name, idx+1)
			} else {
				st.Detail = fmt.Sprintf("Đã dành riêng %q (Workspace #%d) + Cầu nối Wayland cũ đang chạy — đăng xuất/đăng nhập lại GNOME 1 lần để nạp v4 Offscreen & chống nháy OAuth", name, idx+1)
			}
		case hasWmctrl == nil || hasXdotool == nil:
			st.Ready = true
			st.Detail = fmt.Sprintf("Đã dành riêng %q (Workspace #%d) tách biệt User Workspace (XWayland router sẵn sàng)", name, idx+1)
		case extInstalled:
			st.Ready = canRouteWindows
			st.Detail = fmt.Sprintf(
				"Đã gắn tên %q (Workspace #%d) & cài Extension GNOME 51 — cần đăng xuất/đăng nhập lại GNOME 1 lần để nạp cầu nối Wayland",
				name,
				idx+1,
			)
		default:
			st.Ready = false
			st.Detail = fmt.Sprintf("Đã gắn tên %q (Workspace #%d) — bấm 'Thiết lập Agent Workspace' để cài cầu nối Native Wayland", name, idx+1)
		}
		return st
	}

	st.Ready = false
	st.Detail = fmt.Sprintf(
		"Chưa định danh tĩnh %q trên GNOME Fedora 45 (dynamic=%s, num=%d, named=%v) — bấm 'Thiết lập Agent Workspace' để kích hoạt",
		name,
		dynOut,
		numWS,
		hasLabel,
	)
	return st
}

// EnsureAgentWorkspace configures GNOME Mutter on Fedora 45 to maintain a dedicated static workspace named
// "Agent Workspace" (preserving existing user workspace names), sets focus-new-windows=smart, installs/enables
// the hardened GNOME Shell 51 bridge extension, and routes target automation windows exclusively to it.
func (m *WaylandWorkspaceManager) EnsureAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (domain.AgentWorkspaceStatus, error) {
	name := canonicalWorkspaceName(cfg)
	if !cfg.CuaDriver.AgentWorkspaceEnabled {
		return m.Status(ctx, cfg), nil
	}

	if gsettingsBin, err := m.lookPath("gsettings"); err == nil {
		curNamesOut, _ := m.runCmd(ctx, gsettingsBin, "get", "org.gnome.desktop.wm.preferences", "workspace-names")
		curNames := parseGSettingsWorkspaceNames(curNamesOut)

		curNum := 0
		if numOut, nErr := m.runCmd(ctx, gsettingsBin, "get", "org.gnome.desktop.wm.preferences", "num-workspaces"); nErr == nil {
			curNum, _ = strconv.Atoi(strings.TrimSpace(numOut))
		}

		mergedNames, resolvedIdx := mergeGSettingsWorkspaceNames(curNames, cfg.CuaDriver.AgentWorkspaceIndex, name)
		minRequired := resolvedIdx + 1
		if minRequired < 2 {
			minRequired = 2
		}
		targetNum := curNum
		if targetNum < minRequired {
			targetNum = minRequired
		}
		if len(mergedNames) > targetNum {
			targetNum = len(mergedNames)
		}

		if out, err := m.runCmd(ctx, gsettingsBin, "set", "org.gnome.mutter", "dynamic-workspaces", "false"); err != nil {
			return domain.AgentWorkspaceStatus{}, fmt.Errorf("không đặt được dynamic-workspaces=false: %w (%s)", err, out)
		}
		if out, err := m.runCmd(ctx, gsettingsBin, "set", "org.gnome.desktop.wm.preferences", "num-workspaces", strconv.Itoa(targetNum)); err != nil {
			return domain.AgentWorkspaceStatus{}, fmt.Errorf("không đặt được num-workspaces=%d: %w (%s)", targetNum, err, out)
		}

		// Prevent newly spawned OAuth windows on Agent Workspace from forcing viewport switch on User Workspace.
		_, _ = m.runCmd(ctx, gsettingsBin, "set", "org.gnome.desktop.wm.preferences", "focus-new-windows", "'smart'")

		namesVariant := formatGSettingsStringList(mergedNames)
		_, _ = m.runCmd(ctx, gsettingsBin, "set", "org.gnome.desktop.wm.preferences", "workspace-names", namesVariant)
	}

	_ = m.ensureGnomeWaylandBridgeExtension(ctx)
	_ = m.RouteTargetWindowsToAgentWorkspace(ctx, cfg)
	return m.Status(ctx, cfg), nil
}

// RouteTargetWindowsToAgentWorkspace moves matching Antigravity automation windows exclusively onto the
// name-resolved "Agent Workspace" without switching the User's active desktop viewport.
func (m *WaylandWorkspaceManager) RouteTargetWindowsToAgentWorkspace(ctx context.Context, cfg domain.AppConfig) error {
	if !cfg.CuaDriver.AgentWorkspaceEnabled {
		return nil
	}
	idx, _ := m.resolveAgentWorkspaceIndex(ctx, cfg)
	idxStr := strconv.Itoa(idx)
	keywords := cfg.TargetAppKeywords
	if len(keywords) == 0 {
		keywords = []string{"Antigravity"}
	}

	// Tier 1: Native Fedora 45 GNOME Shell Wayland Bridge over session D-Bus.
	if gdbusBin, err := m.lookPath("gdbus"); err == nil && m.isGnomeWaylandBridgeLive(ctx) {
		for _, kw := range keywords {
			kw = strings.TrimSpace(kw)
			if kw == "" {
				continue
			}
			_, _ = m.runCmd(
				ctx,
				gdbusBin,
				"call", "--session",
				"--dest", "org.gnome.Shell",
				"--object-path", GnomeBridgeDBusObject,
				"--method", GnomeBridgeDBusInterface+".MoveByTitle",
				kw,
				strconv.FormatUint(uint64(idx), 10),
			)
		}
		return nil
	}

	// Tier 2: EWMH wmctrl fallback (XWayland / X11 sessions).
	if wmctrlBin, err := m.lookPath("wmctrl"); err == nil {
		for _, kw := range keywords {
			kw = strings.TrimSpace(kw)
			if kw != "" {
				_, _ = m.runCmd(ctx, wmctrlBin, "-r", kw, "-t", idxStr)
			}
		}
		return nil
	}

	// Tier 3: xdotool fallback.
	if xdotoolBin, err := m.lookPath("xdotool"); err == nil {
		for _, kw := range keywords {
			kw = strings.TrimSpace(kw)
			if kw == "" {
				continue
			}
			if idsOut, sErr := m.runCmd(ctx, xdotoolBin, "search", "--name", kw); sErr == nil {
				for _, wid := range strings.Fields(idsOut) {
					_, _ = m.runCmd(ctx, xdotoolBin, "set_desktop_for_window", wid, idxStr)
				}
			}
		}
	}
	return nil
}

// UserIsOnAgentWorkspace checks whether the user's currently visible desktop viewport is the named "Agent Workspace".
// If the active workspace cannot be determined reliably, it fails closed (onAgentWS=true, known=false)
// so 30-second Human Yield protection remains strictly active.
func (m *WaylandWorkspaceManager) UserIsOnAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (bool, bool) {
	if !cfg.CuaDriver.AgentWorkspaceEnabled {
		return true, false
	}

	agentIdx, _ := m.resolveAgentWorkspaceIndex(ctx, cfg)
	now := time.Now()
	m.mu.Lock()
	if !m.cachedAt.IsZero() && now.Sub(m.cachedAt) < 400*time.Millisecond {
		idx, known := m.cachedActiveIdx, m.cachedKnown
		m.mu.Unlock()
		if !known {
			return true, false
		}
		return idx == agentIdx, true
	}
	m.mu.Unlock()

	activeIdx, known := m.queryActiveWorkspaceIndex(ctx)

	m.mu.Lock()
	m.cachedActiveIdx = activeIdx
	m.cachedKnown = known
	m.cachedAt = now
	m.mu.Unlock()

	if !known {
		return true, false
	}
	return activeIdx == agentIdx, true
}

// UninstallGnomeBridgeExtension cleanly disables and removes the user-space GNOME Shell extension (--uninstall).
func (m *WaylandWorkspaceManager) UninstallGnomeBridgeExtension(ctx context.Context) error {
	if geBin, err := m.lookPath("gnome-extensions"); err == nil {
		_, _ = m.runCmd(ctx, geBin, "disable", GnomeBridgeExtensionUUID)
		_, _ = m.runCmd(ctx, geBin, "uninstall", GnomeBridgeExtensionUUID)
	}
	if gsettingsBin, err := m.lookPath("gsettings"); err == nil {
		if curExts, gErr := m.runCmd(ctx, gsettingsBin, "get", "org.gnome.shell", "enabled-extensions"); gErr == nil {
			if updatedExts, changed := removeGSettingsStringList(curExts, GnomeBridgeExtensionUUID); changed {
				_, _ = m.runCmd(ctx, gsettingsBin, "set", "org.gnome.shell", "enabled-extensions", updatedExts)
			}
		}
	}
	extDir := filepath.Join(m.extBaseDir, GnomeBridgeExtensionUUID)
	return os.RemoveAll(extDir)
}

func (m *WaylandWorkspaceManager) queryActiveWorkspaceIndex(ctx context.Context) (int, bool) {
	qCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	// Tier 1: Native GNOME Shell Wayland Bridge (GetActiveWorkspace -> "(0,)" or "(int32 0,)").
	if gdbusBin, err := m.lookPath("gdbus"); err == nil {
		out, callErr := m.runCmd(
			qCtx,
			gdbusBin,
			"call", "--session",
			"--dest", "org.gnome.Shell",
			"--object-path", GnomeBridgeDBusObject,
			"--method", GnomeBridgeDBusInterface+".GetActiveWorkspace",
		)
		if callErr == nil {
			if match := gdbusInt32Regex.FindString(out); match != "" {
				if idx, convErr := strconv.Atoi(match); convErr == nil && idx >= 0 {
					return idx, true
				}
			}
		}
	}

	// Tier 2: wmctrl -d fallback (active workspace row has '*' in column 2).
	if wmctrlBin, err := m.lookPath("wmctrl"); err == nil {
		if out, wErr := m.runCmd(qCtx, wmctrlBin, "-d"); wErr == nil {
			for _, line := range strings.Split(out, "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[1] == "*" {
					if idx, convErr := strconv.Atoi(fields[0]); convErr == nil && idx >= 0 {
						return idx, true
					}
				}
			}
		}
	}

	return -1, false
}

func (m *WaylandWorkspaceManager) isExtensionWritten() bool {
	extJS := filepath.Join(m.extBaseDir, GnomeBridgeExtensionUUID, "extension.js")
	_, err := os.Stat(extJS)
	return err == nil
}

func (m *WaylandWorkspaceManager) inspectGnomeWaylandBridge(ctx context.Context) (live bool, hasOffscreen bool, hasAutoRoute bool) {
	gdbusBin, err := m.lookPath("gdbus")
	if err != nil {
		return false, false, false
	}
	out, err := m.runCmd(
		ctx,
		gdbusBin,
		"introspect", "--session",
		"--dest", "org.gnome.Shell",
		"--object-path", GnomeBridgeDBusObject,
	)
	if err != nil || !strings.Contains(out, GnomeBridgeDBusInterface) {
		return false, false, false
	}
	return true, strings.Contains(out, "GetWindowFrame"), strings.Contains(out, "ArmAutoRoute")
}

func (m *WaylandWorkspaceManager) isGnomeWaylandBridgeLive(ctx context.Context) bool {
	live, _, _ := m.inspectGnomeWaylandBridge(ctx)
	return live
}

// ArmAutoRoute asks the GNOME Shell Wayland bridge to pre-route newly created Chrome/OAuth/Antigravity
// windows straight onto the named "Agent Workspace" before first frame paint (zero flash on User Workspace #1).
func (m *WaylandWorkspaceManager) ArmAutoRoute(ctx context.Context, cfg domain.AppConfig, ttlSeconds int) bool {
	if !cfg.CuaDriver.AgentWorkspaceEnabled {
		return false
	}
	if ttlSeconds < 1 {
		ttlSeconds = 15
	}
	gdbusBin, err := m.lookPath("gdbus")
	if err != nil {
		return false
	}
	idx, _ := m.resolveAgentWorkspaceIndex(ctx, cfg)
	qCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	out, callErr := m.runCmd(
		qCtx,
		gdbusBin,
		"call", "--session",
		"--dest", "org.gnome.Shell",
		"--object-path", GnomeBridgeDBusObject,
		"--method", GnomeBridgeDBusInterface+".ArmAutoRoute",
		strconv.Itoa(ttlSeconds),
		strconv.Itoa(idx),
	)
	return callErr == nil && strings.Contains(strings.ToLower(out), "true")
}

func (m *WaylandWorkspaceManager) ensureGnomeWaylandBridgeExtension(ctx context.Context) error {
	extDir := filepath.Join(m.extBaseDir, GnomeBridgeExtensionUUID)
	if err := os.MkdirAll(extDir, 0o700); err != nil {
		return err
	}

	versions := []string{"45", "46", "47", "48", "49", "50", "51", "52", "53", "54", "55"}
	if gsBin, err := m.lookPath("gnome-shell"); err == nil {
		if verOut, vErr := m.runCmd(ctx, gsBin, "--version"); vErr == nil {
			if match := gnomeVerDigits.FindString(verOut); match != "" && !containsStr(versions, match) {
				versions = append(versions, match)
			}
		}
	}

	meta := map[string]interface{}{
		"uuid":          GnomeBridgeExtensionUUID,
		"name":          "Michael Auto Agent Workspace Bridge",
		"description":   "Dedicated Agent Workspace routing, zero-flash ArmAutoRoute & offscreen window capture on GNOME Wayland.",
		"shell-version": versions,
		"version":       4,
	}
	metaRaw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	metaBytes := append(metaRaw, '\n')
	jsBytes := []byte(gnomeShellExtensionJS)

	if err := os.WriteFile(filepath.Join(extDir, "metadata.json"), metaBytes, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(extDir, "extension.js"), jsBytes, 0o600); err != nil {
		return err
	}

	if geBin, err := m.lookPath("gnome-extensions"); err == nil {
		if bundleZip, zErr := buildExtensionZipBundle(metaBytes, jsBytes); zErr == nil {
			if tmpFile, tErr := os.CreateTemp("", "michael-auto-gnome-ext-*.zip"); tErr == nil {
				tmpName := tmpFile.Name()
				_, _ = tmpFile.Write(bundleZip)
				_ = tmpFile.Close()
				_, _ = m.runCmd(ctx, geBin, "install", "--force", tmpName)
				_ = os.Remove(tmpName)
			}
		}
		_, _ = m.runCmd(ctx, geBin, "enable", GnomeBridgeExtensionUUID)
	}

	if gsettingsBin, err := m.lookPath("gsettings"); err == nil {
		if curExts, gErr := m.runCmd(ctx, gsettingsBin, "get", "org.gnome.shell", "enabled-extensions"); gErr == nil {
			if updatedExts, changed := appendGSettingsStringList(curExts, GnomeBridgeExtensionUUID); changed {
				_, _ = m.runCmd(ctx, gsettingsBin, "set", "org.gnome.shell", "enabled-extensions", updatedExts)
			}
		}
	}

	if gdbusBin, err := m.lookPath("gdbus"); err == nil {
		_, _ = m.runCmd(
			ctx,
			gdbusBin,
			"call", "--session",
			"--dest", "org.gnome.Shell.Extensions",
			"--object-path", "/org/gnome/Shell/Extensions",
			"--method", "org.gnome.Shell.Extensions.EnableExtension",
			GnomeBridgeExtensionUUID,
		)
	}
	return nil
}

func appendGSettingsStringList(rawList string, item string) (string, bool) {
	safeItem := strings.ReplaceAll(strings.TrimSpace(item), "'", "")
	if safeItem == "" {
		return rawList, false
	}
	if strings.Contains(rawList, "'"+safeItem+"'") {
		return rawList, false
	}
	trimmed := strings.TrimSpace(rawList)
	trimmed = strings.TrimPrefix(trimmed, "@as ")
	if trimmed == "" || trimmed == "[]" {
		return "['" + safeItem + "']", true
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		if inner == "" {
			return "['" + safeItem + "']", true
		}
		return "[" + inner + ", '" + safeItem + "']", true
	}
	return "['" + safeItem + "']", true
}

func removeGSettingsStringList(rawList string, item string) (string, bool) {
	items := parseGSettingsWorkspaceNames(rawList)
	var kept []string
	removed := false
	for _, s := range items {
		if s == item {
			removed = true
			continue
		}
		kept = append(kept, s)
	}
	if !removed {
		return rawList, false
	}
	return formatGSettingsStringList(kept), true
}

func parseGSettingsWorkspaceNames(raw string) []string {
	matches := gsettingsQuoteReg.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 {
			out = append(out, m[1])
		}
	}
	return out
}

func containsWorkspaceName(names []string, target string) bool {
	for _, n := range names {
		if strings.EqualFold(strings.TrimSpace(n), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}

// mergeGSettingsWorkspaceNames preserves existing user workspace names while guaranteeing that
// the canonical "Agent Workspace" exists (renaming legacy "Michael-Auto-Agent" in-place if found).
func mergeGSettingsWorkspaceNames(existing []string, preferredIdx int, agentName string) ([]string, int) {
	safeName := strings.ReplaceAll(strings.TrimSpace(agentName), "'", "")
	if safeName == "" {
		safeName = domain.DefaultAgentWorkspaceName
	}
	if preferredIdx < 1 {
		preferredIdx = 1
	}

	out := append([]string(nil), existing...)
	for i, n := range out {
		trimmed := strings.TrimSpace(n)
		if strings.EqualFold(trimmed, safeName) {
			out[i] = safeName
			return out, i
		}
		if strings.EqualFold(trimmed, "Michael-Auto-Agent") {
			out[i] = safeName
			return out, i
		}
	}

	for len(out) <= preferredIdx {
		if len(out) == 0 {
			out = append(out, "User Workspace")
		} else if len(out) == preferredIdx {
			out = append(out, safeName)
		} else {
			out = append(out, fmt.Sprintf("Workspace %d", len(out)+1))
		}
	}
	// If preferredIdx was already occupied by a non-default user workspace name, append "Agent Workspace" cleanly
	// so we never hijack or rename the user's personal workspace!
	if !strings.EqualFold(out[preferredIdx], safeName) {
		if out[preferredIdx] == "" || strings.HasPrefix(out[preferredIdx], "Workspace ") {
			out[preferredIdx] = safeName
			return out, preferredIdx
		}
		out = append(out, safeName)
		return out, len(out) - 1
	}
	return out, preferredIdx
}

func formatGSettingsStringList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "'" + strings.ReplaceAll(s, "'", "") + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func buildExtensionZipBundle(metaBytes, jsBytes []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"metadata.json", metaBytes},
		{"extension.js", jsBytes},
	} {
		w, err := zw.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func containsStr(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

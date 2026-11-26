//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

func TestWaylandWorkspaceManagerFedora45DedicatedAgentWorkspaceAndHardenedBridge(t *testing.T) {
	cfg := domain.DefaultConfig()
	cfg.CuaDriver.AgentWorkspaceEnabled = true
	cfg.CuaDriver.AgentWorkspaceIndex = 1
	cfg.CuaDriver.AgentWorkspaceName = domain.DefaultAgentWorkspaceName

	state := map[string]string{
		"org.gnome.mutter:dynamic-workspaces":             "true",
		"org.gnome.desktop.wm.preferences:num-workspaces": "1",
		"org.gnome.desktop.wm.preferences:workspace-names": "['Work', 'Michael-Auto-Agent']",
	}
	var gdbusCalls []string
	var extensionEnabled bool

	mgr := NewTestableWorkspaceManager(
		func(bin string) (string, error) {
			if bin == "wmctrl" || bin == "xdotool" {
				return "", errors.New("not installed")
			}
			return "/usr/bin/" + bin, nil
		},
		func(ctx context.Context, name string, args ...string) (string, error) {
			switch {
			case strings.HasSuffix(name, "gnome-shell"):
				return "GNOME Shell 51.1", nil
			case strings.HasSuffix(name, "gnome-extensions"):
				if len(args) >= 2 && args[0] == "enable" && args[1] == GnomeBridgeExtensionUUID {
					extensionEnabled = true
				}
				return "", nil
			case strings.HasSuffix(name, "gsettings") && len(args) >= 3:
				key := args[1] + ":" + args[2]
				if args[0] == "get" {
					return state[key], nil
				}
				if args[0] == "set" && len(args) >= 4 {
					state[key] = args[3]
					return "", nil
				}
			case strings.HasSuffix(name, "gdbus"):
				joined := strings.Join(args, " ")
				if strings.Contains(joined, "introspect") && extensionEnabled {
					return "interface " + GnomeBridgeDBusInterface + " { methods: GetWindowFrame(); ArmAutoRoute(); }", nil
				}
				if strings.Contains(joined, "ResolveAgentWorkspace") && extensionEnabled {
					return "(1, true)", nil
				}
				if strings.Contains(joined, "ArmAutoRoute") && extensionEnabled {
					gdbusCalls = append(gdbusCalls, joined)
					return "(true,)", nil
				}
				if strings.Contains(joined, "MoveByTitle") {
					gdbusCalls = append(gdbusCalls, joined)
					return "(uint32 1,)", nil
				}
				if strings.Contains(joined, "GetActiveWorkspace") {
					return "(int32 0,)", nil
				}
			}
			return "", errors.New("not active yet")
		},
	)
	extBase := t.TempDir()
	mgr.SetExtensionBaseDir(extBase)

	ctx := context.Background()
	if stBefore := mgr.Status(ctx, cfg); stBefore.Ready {
		t.Fatalf("expected unready status before EnsureAgentWorkspace, got %+v", stBefore)
	}

	stAfter, err := mgr.EnsureAgentWorkspace(ctx, cfg)
	if err != nil {
		t.Fatalf("EnsureAgentWorkspace failed: %v", err)
	}
	if !stAfter.Ready || stAfter.WorkspaceName != domain.DefaultAgentWorkspaceName || !strings.Contains(stAfter.Detail, "Agent Workspace") {
		t.Fatalf("expected Ready=true with canonical 'Agent Workspace', got %+v", stAfter)
	}

	// Legacy 'Michael-Auto-Agent' must be cleanly migrated in-place to 'Agent Workspace' while preserving 'Work'!
	wsNames := state["org.gnome.desktop.wm.preferences:workspace-names"]
	if wsNames != "['Work', 'Agent Workspace']" {
		t.Fatalf("expected preserved user workspace 'Work' + canonical 'Agent Workspace', got %q", wsNames)
	}

	// Verify GNOME Shell 51.1 extension contains P0-1 path confinement + offscreen Meta.WindowActor capture!
	extJSBytes, err := os.ReadFile(filepath.Join(extBase, GnomeBridgeExtensionUUID, "extension.js"))
	if err != nil {
		t.Fatalf("read extension.js failed: %v", err)
	}
	extJS := string(extJSBytes)
	for _, marker := range []string{"_isAllowedEvidencePath", "NOFOLLOW_SYMLINKS", "paint_to_content", "composite_to_stream", "ResolveAgentWorkspace", "GetWindowFrame", "ArmAutoRoute", "window-created"} {
		if !strings.Contains(extJS, marker) {
			t.Fatalf("expected hardened extension.js to contain %q", marker)
		}
	}

	if !mgr.ArmAutoRoute(ctx, cfg, 15) {
		t.Fatal("expected ArmAutoRoute(15) to return true when v4 bridge is live")
	}

	if !strings.Contains(state["org.gnome.shell:enabled-extensions"], GnomeBridgeExtensionUUID) {
		t.Fatalf("expected %s added to enabled-extensions, got %q", GnomeBridgeExtensionUUID, state["org.gnome.shell:enabled-extensions"])
	}
	if len(gdbusCalls) == 0 || !strings.Contains(gdbusCalls[0], "Antigravity 1") {
		t.Fatalf("expected Antigravity routed over pure Wayland gdbus bridge to index 1, got %v", gdbusCalls)
	}

	onAgentWS, known := mgr.UserIsOnAgentWorkspace(ctx, cfg)
	if !known || onAgentWS {
		t.Fatalf("expected onAgentWS=false, known=true when active workspace is 0, got onAgentWS=%v known=%v", onAgentWS, known)
	}

	// Verify name-first workspace merging preserves 2 user workspaces and appends 'Agent Workspace' at index 2!
	merged, idx := mergeGSettingsWorkspaceNames([]string{"Coding", "Browser"}, 1, domain.DefaultAgentWorkspaceName)
	if idx != 2 || len(merged) != 3 || merged[2] != domain.DefaultAgentWorkspaceName {
		t.Fatalf("expected 'Agent Workspace' appended at index 2 without overwriting 'Browser', got idx=%d merged=%v", idx, merged)
	}

	// Verify UninstallGnomeBridgeExtension removes extension files & gsettings entry cleanly!
	if err := mgr.UninstallGnomeBridgeExtension(ctx); err != nil {
		t.Fatalf("UninstallGnomeBridgeExtension failed: %v", err)
	}
	if strings.Contains(state["org.gnome.shell:enabled-extensions"], GnomeBridgeExtensionUUID) {
		t.Fatalf("expected UUID removed from enabled-extensions, got %q", state["org.gnome.shell:enabled-extensions"])
	}
	if _, statErr := os.Stat(filepath.Join(extBase, GnomeBridgeExtensionUUID)); !os.IsNotExist(statErr) {
		t.Fatalf("expected extension dir removed, stat err=%v", statErr)
	}

	// Verify legacy v2 bridge detection surfaces explicit GNOME relogin upgrade guidance in Status().Detail!
	legacyMgr := NewTestableWorkspaceManager(
		func(bin string) (string, error) { return "/usr/bin/" + bin, nil },
		func(ctx context.Context, name string, args ...string) (string, error) {
			switch {
			case strings.HasSuffix(name, "gsettings") && len(args) >= 3:
				switch args[2] {
				case "dynamic-workspaces":
					return "false", nil
				case "num-workspaces":
					return "2", nil
				case "workspace-names":
					return "['User-Workspace', 'Agent Workspace']", nil
				}
			case strings.HasSuffix(name, "gdbus"):
				if strings.Contains(strings.Join(args, " "), "introspect") {
					return "interface " + GnomeBridgeDBusInterface + " { methods: MoveByPid(); MoveByTitle(); }", nil
				}
			}
			return "", errors.New("n/a")
		},
	)
	legacySt := legacyMgr.Status(ctx, cfg)
	if !legacySt.Ready || !strings.Contains(legacySt.Detail, "đăng xuất/đăng nhập lại GNOME 1 lần") {
		t.Fatalf("expected legacy v2 bridge relogin upgrade notice, got %+v", legacySt)
	}

	// Verify production constructor & XDG extension directory resolution.
	tmpData := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpData)
	prodMgr := NewWorkspaceManager()
	if !strings.HasPrefix(prodMgr.extBaseDir, tmpData) {
		t.Fatalf("expected extBaseDir under XDG_DATA_HOME, got %q", prodMgr.extBaseDir)
	}
}

"""Actuator module — Verify -> Screenshot Evidence -> Execute multi-window automation via trycua/cua (v2.4).

Implements all mandatory ADR invariants + real `cua-driver 0.58+` UniFFI binary compatibility:
- Strict UniFFI keyword-only constructor hydration (`_safe_construct` fills `None` for Optional params).
- `on_screen_only=False` window enumeration so Agent Workspace #2 windows stay visible while User is on Workspace #1.
- Flat `WindowElement` (`parent_index` + `ElementFrame`) hierarchy reconstruction & `SnapshotImage` extraction.
- 3-Layer OAuth Routing for '1 Google Account = 1 Chrome --profile-directory':
  1) Pre-warmed MRU `--profile-directory` window before `ClickSignIn`.
  2) Lossless OAuth2 PKCE URL Relay (`oauth_profile_relay_required`) if `xdg-open` lands in another profile.
  3) Single-Account Auto-Consent / `Open xdg-open` Fast-Path + 8s adaptive convergence loop.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
import os
import re
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from typing import Any

from michael_cua_sidecar.config import get_delivery_mode, get_target_app
from michael_cua_sidecar.errors import (
    ERR_ACTION_FAILED,
    ERR_AUTH_GATE_DETECTED,
    ERR_ELEMENT_NOT_FOUND,
    ERR_SECURITY_VIOLATION,
    ERR_WINDOW_NOT_FOUND,
    SidecarRpcError,
)
from michael_cua_sidecar.observer import (
    NormalizedElement,
    analyze_window_state,
    build_target_dict,
    count_element_nodes,
    is_valid_ide_control,
    normalize_elements_hierarchy,
    scan_elements_for_auth_gate,
)
from michael_cua_sidecar.phrases import (
    ACCOUNT_TAB_PHRASES,
    CANCEL_DIALOG_PHRASES,
    CHROME_EXCLUDED_ACCOUNT_PHRASES,
    CHROME_WINDOW_KEYWORDS,
    CLICKABLE_ROLES,
    CONFIRM_SIGNOUT_PHRASES,
    CONVERSATION_CONTENT_ROLES,
    EDITOR_ROLES,
    MODEL_SELECTOR_PHRASES,
    SETTINGS_PHRASES,
    SIGNIN_PHRASES,
    SIGNOUT_PHRASES,
    SYSTEM_SURFACE_ROLES,
    VIEW_QUOTA_PHRASES,
)
from michael_cua_sidecar.security import (
    collect_editor_redaction_rects,
    extract_and_verify_screenshot_b64,
    extract_chrome_omnibox_url,
    is_antigravity_oauth_authorize_url,
    normalize_https_url,
    pid_matches_browser_profile_dir,
    verify_chrome_omnibox_and_profile,
    verify_pid_allowed,
)

logger = logging.getLogger("michael_cua_sidecar.actuator")

_EMAIL_RE = re.compile(r"[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}")
_STATUS_PCT_RE = re.compile(r"\b\d{1,3}\s*%\s*(?:used|remaining|quota|left|còn lại)\b", re.IGNORECASE)

OAUTH_COMPLETED_TAB_MARKERS = (
    "authentication complete",
    "login successful",
    "you can close this window",
    "you may close this tab",
    "authorization successful",
    "đăng nhập thành công",
)

POST_OAUTH_CONTINUE_PHRASES = [
    "continue",
    "allow",
    "tiếp tục",
    "cho phép",
    "open antigravity",
    "open google antigravity",
    "open xdg-open",
    "mở antigravity",
    "mở xdg-open",
]

POST_OAUTH_NEGATIVE_PHRASES = (
    "cancel",
    "deny",
    "block",
    "don't allow",
    "hủy",
    "từ chối",
)


@dataclass
class WindowHandle:
    pid: int
    window_id: Any
    title: str
    app_name: str = ""


def _safe_construct(cls: Any, **kwargs: Any) -> Any:
    """Construct UniFFI or Python input types safely.

    Crucial for `cua_driver >= 0.58` (UniFFI): generated `__init__` signatures declare
    `typing.Optional[T]` fields as required keyword-only arguments with NO `= None` default!
    Any parameter without a default value that is omitted from `kwargs` is explicitly set to `None`.
    """
    try:
        sig = inspect.signature(cls)
        params = sig.parameters
        accepts_var_kw = any(p.kind == inspect.Parameter.VAR_KEYWORD for p in params.values())
        if accepts_var_kw:
            return cls(**{k: v for k, v in kwargs.items() if v is not None})

        filtered: dict[str, Any] = {}
        for name, p in params.items():
            if name == "self":
                continue
            if p.kind in (inspect.Parameter.VAR_POSITIONAL, inspect.Parameter.VAR_KEYWORD):
                continue
            if name in kwargs:
                filtered[name] = kwargs[name]
            elif p.default is inspect.Parameter.empty:
                filtered[name] = None
        return cls(**filtered)
    except (TypeError, ValueError):
        return cls(**{k: v for k, v in kwargs.items() if v is not None})


def _coerce_window_id(wid: Any) -> Any:
    """Ensure numeric window IDs are passed as `int` to UniFFI FFI converters while keeping mock string IDs."""
    if isinstance(wid, int):
        return wid
    if isinstance(wid, str):
        s = wid.strip()
        if s.isdigit():
            return int(s)
        if s.lower().startswith("0x"):
            try:
                return int(s, 16)
            except ValueError:
                return wid
    return wid


def _import_cua_symbol(name: str) -> Any | None:
    try:
        import cua_driver  # type: ignore[import-untyped]

        return getattr(cua_driver, name, None)
    except ImportError:
        return None


def _is_native_cua_driver(driver: Any) -> bool:
    cua_cls = _import_cua_symbol("CuaDriver")
    return cua_cls is not None and isinstance(driver, cua_cls)


def _agent_workspace_enabled() -> tuple[bool, int]:
    enabled = os.environ.get("CUA_AGENT_WORKSPACE_ENABLED", "1") == "1"
    try:
        idx = int(os.environ.get("CUA_AGENT_WORKSPACE_INDEX", "1"))
    except ValueError:
        idx = 1
    if enabled and sys.platform.startswith("linux"):
        resolved = _resolve_agent_workspace_index_by_name()
        if resolved is not None:
            idx = resolved
    return enabled, max(0, idx)


def _agent_workspace_name() -> str:
    raw = (os.environ.get("CUA_AGENT_WORKSPACE_NAME") or "Agent Workspace").strip()
    if not raw or raw.lower() == "michael-auto-agent":
        return "Agent Workspace"
    return raw


def _resolve_agent_workspace_index_by_name() -> int | None:
    """Resolve the exact 0-based index of the dedicated 'Agent Workspace' by name via GNOME 51 bridge."""
    gdbus_bin = shutil.which("gdbus")
    if not gdbus_bin:
        return None
    try:
        res = subprocess.run(
            [
                gdbus_bin,
                "call",
                "--session",
                "--dest",
                "org.gnome.Shell",
                "--object-path",
                "/io/michael_auto/WorkspaceBridge",
                "--method",
                "io.michael_auto.WorkspaceBridge.ResolveAgentWorkspace",
                _agent_workspace_name(),
            ],
            timeout=0.35,
            check=False,
            capture_output=True,
            text=True,
        )
        if res.returncode == 0 and "true" in (res.stdout or "").lower():
            m = re.search(r"(-?\d+)", res.stdout or "")
            if m:
                val = int(m.group(1))
                if val >= 0:
                    return val
    except Exception:
        pass
    return None


def _query_gnome51_active_workspace() -> int:
    if not sys.platform.startswith("linux"):
        return -1
    gdbus_bin = shutil.which("gdbus")
    if not gdbus_bin:
        return -1
    try:
        res = subprocess.run(
            [
                gdbus_bin,
                "call",
                "--session",
                "--dest",
                "org.gnome.Shell",
                "--object-path",
                "/io/michael_auto/WorkspaceBridge",
                "--method",
                "io.michael_auto.WorkspaceBridge.GetActiveWorkspace",
            ],
            timeout=0.35,
            check=False,
            capture_output=True,
            text=True,
        )
        if res.returncode == 0 and res.stdout:
            m = re.search(r"(-?\d+)", res.stdout)
            if m:
                return int(m.group(1))
    except Exception:
        pass
    return -1


def _query_gnome51_window_frame(pid: int) -> tuple[dict[str, int] | None, int, int, bool]:
    """Return (windowFrameDict, windowWorkspaceIndex, activeWorkspaceIndex, hasV3Bridge) via GNOME 51 bridge."""
    if pid <= 0 or not sys.platform.startswith("linux"):
        return None, -1, -1, False
    gdbus_bin = shutil.which("gdbus")
    if not gdbus_bin:
        return None, -1, -1, False
    try:
        res = subprocess.run(
            [
                gdbus_bin,
                "call",
                "--session",
                "--dest",
                "org.gnome.Shell",
                "--object-path",
                "/io/michael_auto/WorkspaceBridge",
                "--method",
                "io.michael_auto.WorkspaceBridge.GetWindowFrame",
                str(pid),
            ],
            timeout=0.4,
            check=False,
            capture_output=True,
            text=True,
        )
        if res.returncode == 0 and res.stdout:
            nums = [int(n) for n in re.findall(r"-?\d+", res.stdout)]
            if len(nums) >= 6:
                x, y, w, h, win_ws, active_ws = nums[:6]
                frame = {"x": x, "y": y, "width": w, "height": h} if w > 0 and h > 0 else None
                return frame, win_ws, active_ws, True
    except Exception:
        pass
    return None, -1, _query_gnome51_active_workspace(), False


def _mask_editor_enabled() -> bool:
    return os.environ.get("CUA_MASK_EDITOR_EVIDENCE", "1") == "1"


def _arm_gnome51_auto_route(ttl_seconds: int = 15) -> bool:
    """Arm GNOME Shell WorkspaceBridge v4 window-created auto-routing to dedicated 'Agent Workspace' (R3)."""
    enabled, ws_idx = _agent_workspace_enabled()
    if not enabled or not sys.platform.startswith("linux"):
        return False
    gdbus_bin = shutil.which("gdbus")
    if not gdbus_bin:
        return False
    try:
        res = subprocess.run(
            [
                gdbus_bin,
                "call",
                "--session",
                "--dest",
                "org.gnome.Shell",
                "--object-path",
                "/io/michael_auto/WorkspaceBridge",
                "--method",
                "io.michael_auto.WorkspaceBridge.ArmAutoRoute",
                str(max(1, min(int(ttl_seconds), 60))),
                str(ws_idx),
            ],
            timeout=0.35,
            check=False,
            capture_output=True,
            text=True,
        )
        return res.returncode == 0 and "true" in (res.stdout or "").lower()
    except Exception:
        return False


def _pin_window_to_agent_workspace(handle: WindowHandle) -> None:
    """Move target window silently to the Fedora 45 Agent Workspace without switching the User's active view."""
    enabled, ws_idx = _agent_workspace_enabled()
    if not enabled or not sys.platform.startswith("linux"):
        return

    # Tier 1: Native Fedora 45 GNOME Shell Wayland D-Bus Bridge (works for native Wayland & XWayland windows).
    gdbus_bin = shutil.which("gdbus")
    if gdbus_bin and handle.pid > 0:
        try:
            res = subprocess.run(
                [
                    gdbus_bin,
                    "call",
                    "--session",
                    "--dest",
                    "org.gnome.Shell",
                    "--object-path",
                    "/io/michael_auto/WorkspaceBridge",
                    "--method",
                    "io.michael_auto.WorkspaceBridge.MoveByPid",
                    str(handle.pid),
                    str(ws_idx),
                ],
                timeout=0.5,
                check=False,
                capture_output=True,
            )
            if res.returncode == 0:
                return
        except Exception:
            pass

    # Tier 2: EWMH wmctrl fallback for XWayland / X11.
    wid_str = str(handle.window_id or "").strip()
    wmctrl_bin = shutil.which("wmctrl")
    if wmctrl_bin:
        try:
            if wid_str.startswith("0x") or wid_str.isdigit():
                subprocess.run([wmctrl_bin, "-i", "-r", wid_str, "-t", str(ws_idx)], timeout=0.6, check=False, capture_output=True)
            elif handle.title:
                subprocess.run([wmctrl_bin, "-r", handle.title[:48], "-t", str(ws_idx)], timeout=0.6, check=False, capture_output=True)
            return
        except Exception:
            pass

    # Tier 3: xdotool fallback.
    xdotool_bin = shutil.which("xdotool")
    if xdotool_bin and wid_str.isdigit():
        try:
            subprocess.run([xdotool_bin, "set_desktop_for_window", wid_str, str(ws_idx)], timeout=0.6, check=False, capture_output=True)
        except Exception:
            pass


async def find_all_windows_by_keywords(driver: Any, keywords: list[str]) -> list[WindowHandle]:
    """Return all matching windows across ALL workspaces (`on_screen_only=False`), sorted by relevance."""
    lowered = [kw.strip().lower() for kw in keywords if kw and kw.strip()]
    if not lowered:
        lowered = [get_target_app().lower()]

    list_windows_cls = _import_cua_symbol("ListWindowsInput")
    list_apps_cls = _import_cua_symbol("ListAppsInput")

    # CRITICAL (#4): Must pass on_screen_only=False so windows placed on Agent Workspace #2 remain visible
    # while the User works uninterrupted on Workspace #1!
    win_inp = _safe_construct(list_windows_cls, pid=None, on_screen_only=False) if list_windows_cls else None
    windows_resp = await driver.list_windows(win_inp) if win_inp is not None else await driver.list_windows()
    windows = getattr(windows_resp, "windows", None) or []

    scored: list[tuple[int, WindowHandle]] = []
    seen_keys: set[str] = set()

    for idx, w in enumerate(windows):
        title = (getattr(w, "title", None) or getattr(w, "window_title", None) or "").strip()
        app_name = (getattr(w, "app_name", None) or getattr(w, "owner_name", None) or "").strip()
        haystack = f"{title} {app_name}".lower()
        for rank, kw in enumerate(lowered):
            if kw in haystack:
                pid = int(getattr(w, "pid", 0) or 0)
                wid = _coerce_window_id(getattr(w, "window_id", None))
                dedup_key = f"{pid}:{wid}:{title}"
                if dedup_key not in seen_keys:
                    seen_keys.add(dedup_key)
                    score = (len(lowered) - rank) * 100 - idx
                    scored.append((score, WindowHandle(pid=pid, window_id=wid, title=title, app_name=app_name)))
                break

    if scored:
        scored.sort(key=lambda pair: pair[0], reverse=True)
        return [pair[1] for pair in scored]

    if hasattr(driver, "list_apps") and not sys.platform.startswith("linux"):
        apps_inp = _safe_construct(list_apps_cls) if list_apps_cls else None
        apps_resp = await driver.list_apps(apps_inp) if apps_inp is not None else await driver.list_apps()
        out_apps: list[WindowHandle] = []
        for app in getattr(apps_resp, "apps", None) or []:
            name = (getattr(app, "name", "") or "").strip()
            if not getattr(app, "running", True):
                continue
            if any(kw in name.lower() for kw in lowered):
                pid = int(getattr(app, "pid", 0) or 0)
                by_pid_inp = (
                    _safe_construct(list_windows_cls, pid=pid, on_screen_only=False)
                    if list_windows_cls
                    else None
                )
                w_resp = (
                    await driver.list_windows(by_pid_inp)
                    if by_pid_inp is not None
                    else await driver.list_windows()
                )
                for w0 in getattr(w_resp, "windows", None) or []:
                    if int(getattr(w0, "pid", pid) or pid) == pid:
                        out_apps.append(
                            WindowHandle(
                                pid=pid,
                                window_id=_coerce_window_id(getattr(w0, "window_id", None)),
                                title=(getattr(w0, "title", "") or name).strip(),
                                app_name=name,
                            )
                        )
        return out_apps

    return []


async def find_window_by_keywords(driver: Any, keywords: list[str]) -> WindowHandle | None:
    matches = await find_all_windows_by_keywords(driver, keywords)
    return matches[0] if matches else None


async def capture_window_state(driver: Any, handle: WindowHandle, include_screenshot: bool = False) -> Any:
    """Capture structured element-token snapshot (and screenshot when capturing pre-actuation Evidence)."""
    gws_cls = _import_cua_symbol("GetWindowStateInput")
    coerced_wid = _coerce_window_id(handle.window_id)

    if gws_cls is not None:
        inp = _safe_construct(
            gws_cls,
            pid=handle.pid,
            window_id=coerced_wid,
            include_accessibility_tree=True,
            include_screenshot=include_screenshot,
            max_elements=2500,
            max_depth=48,
            tree_format="elements",
            full_output=True,
        )
        return await driver.get_window_state(inp)

    return await driver.get_window_state(
        pid=handle.pid,
        window_id=coerced_wid,
        include_screenshot=include_screenshot,
    )


async def _capture_or_fallback_evidence_b64(
    driver: Any,
    window_state: Any,
    handle: WindowHandle,
    require_evidence: bool,
    step_name: str,
) -> tuple[str, str, dict[str, int] | None]:
    """Extract screenshot from `WindowStateOutput.images`, falling back on GNOME Shell 51+ to our
    offscreen `Meta.WindowActor` capture bridge. NEVER captures User Workspace #1 via full-desktop
    fallback when the target window lives on an inactive dedicated `Agent Workspace` (P0-1)."""
    win_frame, win_ws, active_ws, has_v3_bridge = (
        _query_gnome51_window_frame(handle.pid)
        if _is_native_cua_driver(driver) and sys.platform.startswith("linux")
        else (None, -1, -1, False)
    )

    try:
        b64 = extract_and_verify_screenshot_b64(window_state, require_evidence=False, step_name=step_name)
        if b64:
            return b64, "window", win_frame
    except SidecarRpcError:
        pass

    fallback_bytes: bytes | None = None
    capture_scope = "window"
    if _is_native_cua_driver(driver) and sys.platform.startswith("linux"):
        ws_enabled, agent_ws_idx = _agent_workspace_enabled()
        # Defense-in-depth (P0-1): If GNOME Shell v3 offscreen bridge is live, CaptureWindowPng renders
        # Meta.WindowActor offscreen cleanly; if legacy v2 bridge is still cached in RAM before relogin,
        # ONLY call CaptureWindowPng when the user is viewing Agent Workspace (never leak User Workspace #1!).
        allow_bridge_capture = has_v3_bridge or (not ws_enabled) or (active_ws >= 0 and active_ws == agent_ws_idx)
        if allow_bridge_capture:
            fallback_bytes = _capture_gnome51_bridge_png(handle.pid)

        same_workspace_confirmed = (not ws_enabled) or (win_ws >= 0 and win_ws == active_ws) or (
            not has_v3_bridge and active_ws >= 0 and active_ws == agent_ws_idx
        )
        if not fallback_bytes and same_workspace_confirmed and hasattr(driver, "get_desktop_state"):
            gds_cls = _import_cua_symbol("GetDesktopStateInput")
            if gds_cls is not None:
                runtime_dir = os.environ.get("XDG_RUNTIME_DIR") or tempfile.gettempdir()
                ev_tmp_dir = os.path.join(runtime_dir, "michael-auto")
                tmp_desk_png = ""
                try:
                    os.makedirs(ev_tmp_dir, mode=0o700, exist_ok=True)
                    fd, tmp_desk_png = tempfile.mkstemp(prefix="ev_desk_", suffix=".png", dir=ev_tmp_dir)
                    os.close(fd)
                    os.chmod(tmp_desk_png, 0o600)
                    gds_inp = _safe_construct(gds_cls, session=None, screenshot_out_file=tmp_desk_png)
                    desk_st = await driver.get_desktop_state(gds_inp)
                    if os.path.isfile(tmp_desk_png) and os.path.getsize(tmp_desk_png) > 0:
                        with open(tmp_desk_png, "rb") as f:
                            fallback_bytes = f.read()
                        capture_scope = "desktop"
                    else:
                        desk_b64 = extract_and_verify_screenshot_b64(desk_st, require_evidence=False, step_name=step_name)
                        if desk_b64:
                            return desk_b64, "desktop", win_frame
                except Exception:
                    pass
                finally:
                    if tmp_desk_png and os.path.exists(tmp_desk_png):
                        try:
                            os.remove(tmp_desk_png)
                        except OSError:
                            pass

    verified_b64 = extract_and_verify_screenshot_b64(
        window_state,
        require_evidence=require_evidence,
        step_name=step_name,
        fallback_png_bytes=fallback_bytes,
    )
    return verified_b64, capture_scope, win_frame


def _capture_gnome51_bridge_png(pid: int) -> bytes | None:
    """Attempt offscreen window PNG capture via `io.michael_auto.WorkspaceBridge.CaptureWindowPng` on GNOME 51+."""
    gdbus_bin = shutil.which("gdbus")
    if not gdbus_bin or pid <= 0:
        return None
    runtime_dir = os.environ.get("XDG_RUNTIME_DIR") or tempfile.gettempdir()
    ev_tmp_dir = os.path.join(runtime_dir, "michael-auto")
    try:
        os.makedirs(ev_tmp_dir, mode=0o700, exist_ok=True)
        fd, tmp_path = tempfile.mkstemp(prefix="ev_cap_", suffix=".png", dir=ev_tmp_dir)
        os.close(fd)
        os.chmod(tmp_path, 0o600)
    except OSError:
        return None

    try:
        res = subprocess.run(
            [
                gdbus_bin,
                "call",
                "--session",
                "--dest",
                "org.gnome.Shell",
                "--object-path",
                "/io/michael_auto/WorkspaceBridge",
                "--method",
                "io.michael_auto.WorkspaceBridge.CaptureWindowPng",
                str(max(0, pid)),
                tmp_path,
            ],
            timeout=1.5,
            check=False,
            capture_output=True,
        )
        if res.returncode == 0 and os.path.isfile(tmp_path) and os.path.getsize(tmp_path) > 0:
            with open(tmp_path, "rb") as f:
                return f.read()
    except Exception:
        pass
    finally:
        try:
            if os.path.exists(tmp_path):
                os.remove(tmp_path)
        except OSError:
            pass
    return None


def _build_evidence_payload(
    step: str,
    handle: WindowHandle,
    proc_exe: str,
    screenshot_b64: str,
    verified_detail: str,
    elements: list[Any] | None = None,
    browser_profile: str = "",
    omnibox_origin: str = "",
    capture_scope: str = "window",
    window_frame: dict[str, int] | None = None,
) -> dict[str, Any] | None:
    if not screenshot_b64:
        return None
    payload: dict[str, Any] = {
        "step": step,
        "windowTitle": handle.title,
        "pid": handle.pid,
        "processExe": proc_exe,
        "browserProfile": browser_profile,
        "omniboxOrigin": omnibox_origin,
        "verifiedDetail": verified_detail,
        "screenshotBase64": screenshot_b64,
        "captureScope": capture_scope or "window",
    }
    if window_frame and window_frame.get("width", 0) > 0 and window_frame.get("height", 0) > 0:
        payload["windowFrame"] = window_frame
    if _mask_editor_enabled() and elements:
        rects = collect_editor_redaction_rects(elements)
        if rects:
            payload["redactRects"] = rects
    return payload


async def focus_app_window(driver: Any, app_target: str, dry_run: bool = False) -> dict[str, Any]:
    is_browser = any(k in app_target.lower() for k in ("chrome", "chromium", "browser", "google account"))
    keywords = CHROME_WINDOW_KEYWORDS if is_browser else [app_target]
    handle = await find_window_by_keywords(driver, keywords)
    if handle is None:
        raise SidecarRpcError(
            ERR_WINDOW_NOT_FOUND,
            f"Không tìm thấy cửa sổ mục tiêu '{app_target}' trên màn hình",
            {"appTarget": app_target},
        )

    verify_pid_allowed(handle.pid, "browser" if is_browser else "antigravity", _is_native_cua_driver(driver))
    if dry_run:
        return {"focused": True, "dryRun": True, "windowTitle": handle.title, "pid": handle.pid}

    if _is_native_cua_driver(driver):
        _pin_window_to_agent_workspace(handle)
    await _focus_handle(driver, handle)
    return {
        "focused": True,
        "windowTitle": handle.title,
        "pid": handle.pid,
        "windowId": str(handle.window_id),
    }


async def _focus_handle(driver: Any, handle: WindowHandle) -> None:
    ws_enabled, _ = _agent_workspace_enabled()

    focus_win_cls = _import_cua_symbol("FocusWindowInput")
    focus_app_cls = _import_cua_symbol("FocusAppInput")

    focused_ok = False
    last_err: Exception | None = None

    if hasattr(driver, "focus_window"):
        try:
            if focus_win_cls is not None:
                inp = _safe_construct(focus_win_cls, pid=handle.pid, window_id=_coerce_window_id(handle.window_id))
                await driver.focus_window(inp)
            else:
                await driver.focus_window(pid=handle.pid, window_id=handle.window_id)
            focused_ok = True
        except Exception as e:
            last_err = e

    if not focused_ok and hasattr(driver, "focus_app"):
        try:
            if focus_app_cls is not None:
                inp = _safe_construct(focus_app_cls, pid=handle.pid)
                await driver.focus_app(inp)
            else:
                await driver.focus_app(pid=handle.pid)
            focused_ok = True
        except Exception as e:
            last_err = e

    # Note: Real CuaDriver 0.58+ performs target activation directly inside `driver.click(..., FOREGROUND)`
    # rather than exposing a standalone `focus_window` method. Only run desktop activate fallback when
    # Agent Workspace isolation is disabled so User Workspace #1 never switches viewport!
    if not ws_enabled and sys.platform.startswith("linux") and _is_native_cua_driver(driver):
        _linux_desktop_activate_fallback(handle)
        focused_ok = True

    if not focused_ok and last_err is not None:
        raise SidecarRpcError(
            ERR_ACTION_FAILED,
            f"Không thể focus cửa sổ '{handle.title}': {last_err}",
            {"pid": handle.pid, "windowId": str(handle.window_id)},
        )


def _linux_desktop_activate_fallback(handle: WindowHandle) -> None:
    gdbus_bin = shutil.which("gdbus")
    if gdbus_bin and handle.pid > 0:
        try:
            res = subprocess.run(
                [
                    gdbus_bin,
                    "call",
                    "--session",
                    "--dest",
                    "org.gnome.Shell",
                    "--object-path",
                    "/io/michael_auto/WorkspaceBridge",
                    "--method",
                    "io.michael_auto.WorkspaceBridge.ActivateByPid",
                    str(handle.pid),
                ],
                timeout=0.6,
                check=False,
                capture_output=True,
            )
            if res.returncode == 0:
                return
        except Exception:
            pass

    title_sub = handle.title[:48].strip()
    wid_str = str(handle.window_id or "").strip()

    wmctrl_bin = shutil.which("wmctrl")
    if wmctrl_bin and title_sub:
        try:
            subprocess.run([wmctrl_bin, "-a", title_sub], timeout=0.8, check=False, capture_output=True)
            return
        except Exception:
            pass

    xdotool_bin = shutil.which("xdotool")
    if xdotool_bin and wid_str.isdigit():
        try:
            subprocess.run([xdotool_bin, "windowactivate", wid_str], timeout=0.8, check=False, capture_output=True)
        except Exception:
            pass


async def _click_target_on_window(
    driver: Any,
    handle: WindowHandle,
    target: dict[str, Any],
    prefer_foreground: bool = False,
) -> None:
    action_target_cls = _import_cua_symbol("ActionTarget")
    click_pos_cls = _import_cua_symbol("ClickPosition")
    click_btn_cls = _import_cua_symbol("ClickButton")
    click_inp_cls = _import_cua_symbol("ClickInput")
    delivery_cls = _import_cua_symbol("InputDeliveryMode")

    cfg_mode = get_delivery_mode()
    ws_enabled, _ = _agent_workspace_enabled()
    # On isolated Agent Workspace, attempt BACKGROUND semantic AT-SPI click first so User Workspace #1 never jumps!
    if ws_enabled and cfg_mode == "auto":
        modes = ["background", "foreground"]
    elif prefer_foreground or cfg_mode == "foreground":
        modes = ["foreground"]
    elif cfg_mode == "background":
        modes = ["background"]
    else:
        modes = ["background", "foreground"]

    token = target.get("elementToken", "")
    cx = int(target.get("centerX", 0) or 0)
    cy = int(target.get("centerY", 0) or 0)

    if not token and not (cx > 0 and cy > 0):
        raise SidecarRpcError(ERR_ELEMENT_NOT_FOUND, "Phần tử không có element_token hay tọa độ hợp lệ", target)

    if click_inp_cls is None:
        await driver.click(
            {
                "pid": handle.pid,
                "window_id": handle.window_id,
                "element_token": token,
                "x": cx,
                "y": cy,
                "delivery_mode": modes[0],
            }
        )
        return

    coerced_wid = _coerce_window_id(handle.window_id)
    last_exc: Exception | None = None

    for mode_name in modes:
        try:
            if mode_name == "foreground" and not ws_enabled:
                await _focus_handle(driver, handle)

            win_target = (
                action_target_cls.WINDOW(pid=handle.pid, window_id=coerced_wid)
                if action_target_cls is not None and hasattr(action_target_cls, "WINDOW")
                else _safe_construct(action_target_cls, pid=handle.pid, window_id=coerced_wid)
            )

            position: Any = None
            if token and click_pos_cls is not None and hasattr(click_pos_cls, "ELEMENT"):
                position = click_pos_cls.ELEMENT(element_token=token)
            elif cx > 0 and cy > 0 and click_pos_cls is not None:
                if hasattr(click_pos_cls, "COORDINATES"):
                    position = click_pos_cls.COORDINATES(x=float(cx), y=float(cy))
                elif hasattr(click_pos_cls, "WINDOW_COORDINATES"):
                    position = click_pos_cls.WINDOW_COORDINATES(x=cx, y=cy)
                elif hasattr(click_pos_cls, "COORDINATE"):
                    position = click_pos_cls.COORDINATE(x=cx, y=cy)

            delivery = None
            if delivery_cls is not None:
                delivery = (
                    getattr(delivery_cls, "FOREGROUND", None)
                    if mode_name == "foreground"
                    else getattr(delivery_cls, "BACKGROUND", None)
                )

            button = getattr(click_btn_cls, "LEFT", None) if click_btn_cls is not None else None
            inp = _safe_construct(
                click_inp_cls,
                target=win_target,
                position=position,
                delivery_mode=delivery,
                button=button,
                count=1,
            )
            res = await driver.click(inp)

            # CRITICAL (#5): Real `cua_driver` returns `ActionResult(error=ActionError(code=...))` on soft
            # background refusals (`background_unavailable`) without raising an exception!
            if res is not None:
                act_err = getattr(res, "error", None)
                if act_err is not None:
                    code_s = str(getattr(act_err, "code", "") or "")
                    hint_s = str(getattr(act_err, "hint", "") or "")
                    raise RuntimeError(f"cua_action_error:{code_s}:{hint_s}")
                eff_s = str(getattr(res, "effect", "") or "").lower()
                if "none" in eff_s and getattr(res, "escalation", None) is not None:
                    raise RuntimeError("cua_action_escalation:background_unavailable")
            return
        except Exception as e:
            last_exc = e
            err_text = str(e).lower()
            if mode_name == "background" and len(modes) > 1 and (
                "background" in err_text or "unavailable" in err_text or "unsupported" in err_text
            ):
                continue
            break

    raise SidecarRpcError(
        ERR_ACTION_FAILED,
        f"Click phần tử '{target.get('name', '')}' thất bại: {last_exc}",
        target,
    )


async def _press_key_on_window(driver: Any, handle: WindowHandle, key_str: str) -> None:
    if not hasattr(driver, "press_key"):
        return
    try:
        press_cls = _import_cua_symbol("PressKeyInput")
        action_target_cls = _import_cua_symbol("ActionTarget")
        if press_cls is not None and action_target_cls is not None and hasattr(action_target_cls, "WINDOW"):
            inp = _safe_construct(
                press_cls,
                target=action_target_cls.WINDOW(pid=handle.pid, window_id=_coerce_window_id(handle.window_id)),
                key=key_str,
            )
            await driver.press_key(inp)
        else:
            await driver.press_key({"pid": handle.pid, "window_id": handle.window_id, "key": key_str})
    except Exception:
        pass


async def _press_hotkey_on_window(driver: Any, handle: WindowHandle, keys: list[str]) -> None:
    """Send multi-key chord (e.g. `['ctrl', 'w']`) using UniFFI `HotkeyInput` when available,
    falling back cleanly to `press_key` for mock drivers."""
    hotkey_cls = _import_cua_symbol("HotkeyInput")
    action_target_cls = _import_cua_symbol("ActionTarget")
    if hotkey_cls is not None and action_target_cls is not None and hasattr(driver, "hotkey"):
        try:
            inp = _safe_construct(
                hotkey_cls,
                keys=keys,
                target=action_target_cls.WINDOW(pid=handle.pid, window_id=_coerce_window_id(handle.window_id)),
            )
            await driver.hotkey(inp)
            return
        except Exception:
            pass
    await _press_key_on_window(driver, handle, "+".join(keys))


async def _dismiss_popover_escape(driver: Any, handle: WindowHandle) -> None:
    await _press_key_on_window(driver, handle, "Escape")


async def _cleanup_completed_oauth_browser_tabs(driver: Any) -> int:
    """Safely close stale 'Authentication complete' tabs left open in Chrome after successful OAuth."""
    closed = 0
    try:
        chrome_windows = await find_all_windows_by_keywords(driver, CHROME_WINDOW_KEYWORDS)
        for ch_win in chrome_windows:
            t_low = ch_win.title.lower()
            if any(m in t_low for m in OAUTH_COMPLETED_TAB_MARKERS):
                await _press_hotkey_on_window(driver, ch_win, ["ctrl", "w"])
                closed += 1
    except Exception:
        pass
    return closed


async def inspect_quota(
    driver: Any,
    target_keywords: list[str] | None = None,
    require_evidence: bool = True,
    dry_run: bool = False,
) -> dict[str, Any]:
    """Steps 1 & 2 (1-Minute ADR): Inspect Quota on Agent Workspace without stealing User Workspace #1 viewport.
    When Quota == 0% / Quota exhausted is detected, immediately captures Screenshot Evidence #1."""
    kws = target_keywords or [get_target_app()]
    handle = await find_window_by_keywords(driver, kws)
    if handle is None:
        return {
            "windowDetected": False,
            "appAuthState": "unknown",
            "sourceTier": "cua_driver",
            "confidence": 0.0,
            "targets": {},
        }

    is_native = _is_native_cua_driver(driver)
    proc_exe = verify_pid_allowed(handle.pid, "antigravity", is_native)
    ws_enabled, _ = _agent_workspace_enabled()

    if not dry_run and is_native:
        _pin_window_to_agent_workspace(handle)

    if not dry_run and not ws_enabled:
        await _focus_handle(driver, handle)

    win_state = await capture_window_state(driver, handle, include_screenshot=False)
    parsed = analyze_window_state(win_state, kws)

    if parsed.get("authGateDetected"):
        return parsed

    if parsed.get("quotaExhausted") and not dry_run:
        ev_state = await capture_window_state(driver, handle, include_screenshot=True)
        ev_parsed = analyze_window_state(ev_state, kws)
        if ev_parsed.get("quotaExhausted"):
            b64, cap_scope, win_frame = await _capture_or_fallback_evidence_b64(
                driver, ev_state, handle, require_evidence, "01_verify_quota_zero"
            )
            ev_els = getattr(ev_state, "elements", None) or []
            ev_parsed["evidence"] = _build_evidence_payload(
                "01_verify_quota_zero",
                handle,
                proc_exe,
                b64,
                f"Xác minh Quota=0% / Quota exhausted: {ev_parsed.get('quotaMessage', '')}",
                elements=ev_els,
                capture_scope=cap_scope,
                window_frame=win_frame,
            )
            return ev_parsed

    if dry_run:
        return parsed

    elements = normalize_elements_hierarchy(getattr(win_state, "elements", None) or [])
    quota_el = _find_quota_trigger_element(elements)
    if quota_el is not None:
        await _click_target_on_window(driver, handle, build_target_dict(quota_el), prefer_foreground=False)
        await asyncio.sleep(0.45)

        refreshed = await capture_window_state(driver, handle, include_screenshot=False)
        parsed_after = analyze_window_state(refreshed, kws)

        if parsed_after.get("quotaExhausted"):
            ev_state = await capture_window_state(driver, handle, include_screenshot=True)
            b64, cap_scope, win_frame = await _capture_or_fallback_evidence_b64(
                driver, ev_state, handle, require_evidence, "01_verify_quota_zero"
            )
            ev_els = getattr(ev_state, "elements", None) or []
            parsed_after["evidence"] = _build_evidence_payload(
                "01_verify_quota_zero",
                handle,
                proc_exe,
                b64,
                f"Xác minh bảng Quota về 0% & báo lỗi hết hạn mức: {parsed_after.get('quotaMessage', '')}",
                elements=ev_els,
                capture_scope=cap_scope,
                window_frame=win_frame,
            )

        await _dismiss_popover_escape(driver, handle)
        return parsed_after

    # Antigravity 2.34+ Calibrated 2-Hop Quota Flow:
    # Click 'Select model, current: ...' -> Click 'View Usage' -> Parse group-scoped Usage popover -> Escape x2!
    model_el = _find_model_selector_element(elements)
    if model_el is not None:
        await _click_target_on_window(driver, handle, build_target_dict(model_el), prefer_foreground=False)
        await asyncio.sleep(0.35)

        menu_state = await capture_window_state(driver, handle, include_screenshot=False)
        menu_els = normalize_elements_hierarchy(getattr(menu_state, "elements", None) or [])
        view_usage_el = _find_quota_trigger_element(menu_els)
        if view_usage_el is not None:
            await _click_target_on_window(driver, handle, build_target_dict(view_usage_el), prefer_foreground=False)
            await asyncio.sleep(0.45)

            usage_state = await capture_window_state(driver, handle, include_screenshot=False)
            parsed_usage = analyze_window_state(usage_state, kws)
            if not parsed_usage.get("activeModel") and parsed.get("activeModel"):
                parsed_usage["activeModel"] = parsed["activeModel"]

            if parsed_usage.get("quotaExhausted"):
                ev_state = await capture_window_state(driver, handle, include_screenshot=True)
                b64, cap_scope, win_frame = await _capture_or_fallback_evidence_b64(
                    driver, ev_state, handle, require_evidence, "01_verify_quota_zero"
                )
                ev_els = getattr(ev_state, "elements", None) or []
                parsed_usage["evidence"] = _build_evidence_payload(
                    "01_verify_quota_zero",
                    handle,
                    proc_exe,
                    b64,
                    f"Xác minh Model -> View Usage chạm 0%: {parsed_usage.get('quotaMessage', '')}",
                    elements=ev_els,
                    capture_scope=cap_scope,
                    window_frame=win_frame,
                )

            await _dismiss_popover_escape(driver, handle)
            await asyncio.sleep(0.12)
            await _dismiss_popover_escape(driver, handle)
            return parsed_usage

        await _dismiss_popover_escape(driver, handle)

    return parsed


async def signout_from_settings(
    driver: Any,
    require_evidence: bool = True,
    dry_run: bool = False,
) -> dict[str, Any]:
    """Step 3: Navigate Settings -> Verify Sign Out button -> Capture Evidence #2 -> Click Sign Out -> Confirm."""
    handle = await find_window_by_keywords(driver, [get_target_app()])
    if handle is None:
        raise SidecarRpcError(ERR_WINDOW_NOT_FOUND, f"Không tìm thấy cửa sổ {get_target_app()}")

    is_native = _is_native_cua_driver(driver)
    proc_exe = verify_pid_allowed(handle.pid, "antigravity", is_native)
    ws_enabled, _ = _agent_workspace_enabled()

    if dry_run:
        return {"signedOut": True, "dryRun": True}

    if is_native:
        _pin_window_to_agent_workspace(handle)
    if not ws_enabled:
        await _focus_handle(driver, handle)

    win_state = await capture_window_state(driver, handle)
    elements = normalize_elements_hierarchy(getattr(win_state, "elements", None) or [])

    gate, reason = scan_elements_for_auth_gate(elements, ide_mode=True)
    if gate:
        raise SidecarRpcError(ERR_AUTH_GATE_DETECTED, reason)

    step_used = "direct_signout"
    active_handle = handle

    logout_el = _find_clickable_by_phrases(elements, SIGNOUT_PHRASES)
    if logout_el is None:
        settings_el = _find_clickable_by_phrases(elements, SETTINGS_PHRASES)
        if settings_el is None:
            raise SidecarRpcError(
                ERR_ELEMENT_NOT_FOUND,
                "Không tìm thấy nút Settings hoặc Sign Out trên màn hình Antigravity",
            )

        await _click_target_on_window(driver, handle, build_target_dict(settings_el))
        await asyncio.sleep(0.75)

        # Prefer the Antigravity app window (same PID) first so an unrelated GNOME Settings window is never selected!
        active_handle = await find_window_by_keywords(driver, [get_target_app()]) or handle
        settings_state = await capture_window_state(driver, active_handle)
        settings_elements = normalize_elements_hierarchy(getattr(settings_state, "elements", None) or [])

        logout_el = _find_clickable_by_phrases(settings_elements, SIGNOUT_PHRASES)
        step_used = "settings_then_signout"

        if logout_el is None:
            account_tab = _find_clickable_by_phrases(settings_elements, ACCOUNT_TAB_PHRASES)
            if account_tab is not None:
                await _click_target_on_window(driver, active_handle, build_target_dict(account_tab))
                await asyncio.sleep(0.6)
                tab_state = await capture_window_state(driver, active_handle)
                tab_elements = normalize_elements_hierarchy(getattr(tab_state, "elements", None) or [])
                logout_el = _find_clickable_by_phrases(tab_elements, SIGNOUT_PHRASES)
                step_used = "settings_account_tab_then_signout"

    if logout_el is None:
        raise SidecarRpcError(
            ERR_ELEMENT_NOT_FOUND,
            "Đã mở màn hình Settings nhưng không tìm thấy nút Sign Out",
        )

    ev_state = await capture_window_state(driver, active_handle, include_screenshot=True)
    b64, cap_scope, win_frame = await _capture_or_fallback_evidence_b64(
        driver, ev_state, active_handle, require_evidence, "02_pre_signout_settings"
    )
    ev_elements = normalize_elements_hierarchy(getattr(ev_state, "elements", None) or [])
    verified_logout_el = _find_clickable_by_phrases(ev_elements, SIGNOUT_PHRASES) or logout_el

    await _click_target_on_window(driver, active_handle, build_target_dict(verified_logout_el))

    verified_logged_out = False
    for _ in range(5):
        await asyncio.sleep(0.5)
        cur_handle = await find_window_by_keywords(driver, [get_target_app()]) or active_handle
        cur_state = await capture_window_state(driver, cur_handle)
        cur_elements = normalize_elements_hierarchy(getattr(cur_state, "elements", None) or [])

        if _find_clickable_by_phrases(cur_elements, SIGNIN_PHRASES) is not None:
            verified_logged_out = True
            break

        confirm_btn = _find_confirm_dialog_button(cur_elements)
        if confirm_btn is not None:
            await _click_target_on_window(driver, cur_handle, build_target_dict(confirm_btn))
            step_used += "_confirmed"
            continue

        if _find_clickable_by_phrases(cur_elements, SIGNOUT_PHRASES) is None:
            verified_logged_out = True
            break

    return {
        "signedOut": True,
        "verifiedLoggedOut": verified_logged_out,
        "step": step_used,
        "evidence": _build_evidence_payload(
            "02_pre_signout_settings",
            active_handle,
            proc_exe,
            b64,
            f"Xác minh màn hình Settings & nút Sign Out ({step_used}) trước khi đăng xuất",
            elements=ev_elements,
            capture_scope=cap_scope,
            window_frame=win_frame,
        ),
    }


async def click_signin(
    driver: Any,
    require_evidence: bool = True,
    dry_run: bool = False,
) -> dict[str, Any]:
    """Step 4a: After full 1-minute wait, verify LoggedOut + capture Evidence #3 -> Click Sign In."""
    handle = await find_window_by_keywords(driver, [get_target_app()])
    if handle is None:
        raise SidecarRpcError(ERR_WINDOW_NOT_FOUND, f"Không tìm thấy cửa sổ {get_target_app()}")

    is_native = _is_native_cua_driver(driver)
    proc_exe = verify_pid_allowed(handle.pid, "antigravity", is_native)
    ws_enabled, _ = _agent_workspace_enabled()

    if dry_run:
        return {"clickedSignIn": True, "dryRun": True}

    if is_native:
        _pin_window_to_agent_workspace(handle)
    if not ws_enabled:
        await _focus_handle(driver, handle)

    ev_state = await capture_window_state(driver, handle, include_screenshot=True)
    elements = normalize_elements_hierarchy(getattr(ev_state, "elements", None) or [])

    gate, reason = scan_elements_for_auth_gate(elements, ide_mode=True)
    if gate:
        raise SidecarRpcError(ERR_AUTH_GATE_DETECTED, reason)

    signin_el = _find_clickable_by_phrases(elements, SIGNIN_PHRASES)
    if signin_el is None:
        raise SidecarRpcError(ERR_ELEMENT_NOT_FOUND, "Không tìm thấy nút Sign In trên Antigravity")

    b64, cap_scope, win_frame = await _capture_or_fallback_evidence_b64(
        driver, ev_state, handle, require_evidence, "03_pre_signin_antigravity"
    )
    if is_native and ws_enabled:
        _arm_gnome51_auto_route(15)
    await _click_target_on_window(driver, handle, build_target_dict(signin_el))

    return {
        "clickedSignIn": True,
        "evidence": _build_evidence_payload(
            "03_pre_signin_antigravity",
            handle,
            proc_exe,
            b64,
            "Xác minh trạng thái LoggedOut sau 1 phút chờ & nút Sign In hợp lệ trước khi bấm",
            elements=elements,
            capture_scope=cap_scope,
            window_frame=win_frame,
        ),
    }


def _prioritize_chrome_candidates(
    candidates: list[WindowHandle],
    browser_profile_dir: str,
    expected_profile_name: str,
) -> list[WindowHandle]:
    """Sort candidate Chrome windows so windows matching target --profile-directory or expected profile badge come first."""
    exp_name = expected_profile_name.strip().lower()

    def _score(w: WindowHandle) -> int:
        s = 0
        t_low = w.title.lower()
        if pid_matches_browser_profile_dir(w.pid, browser_profile_dir):
            s += 100
        if exp_name and exp_name in t_low:
            s += 80
        if any(k in t_low for k in ("choose an account", "google accounts", "chọn tài khoản", "sign in")):
            s += 40
        return s

    return sorted(candidates, key=_score, reverse=True)


async def _run_post_oauth_convergence(
    driver: Any,
    ws_enabled: bool,
    settle_timeout_ms: int,
    initial_clicked: set[str] | None = None,
) -> set[str]:
    """Adaptive up-to-8s post-OAuth loop handling Google Consent ('Continue') + Chrome protocol prompt ('Open xdg-open')."""
    conv_budget_s = min(8.0, max(1.6, settle_timeout_ms / 2000.0))
    conv_deadline = asyncio.get_event_loop().time() + conv_budget_s
    clicked_post_steps: set[str] = set(initial_clicked or set())

    while True:
        await asyncio.sleep(0.45)

        ag_win = await find_window_by_keywords(driver, [get_target_app()])
        if ag_win is not None:
            ag_st = await capture_window_state(driver, ag_win)
            ag_els = normalize_elements_hierarchy(getattr(ag_st, "elements", None) or [])
            if ag_els and _find_clickable_by_phrases(ag_els, SIGNIN_PHRASES) is None:
                break

        cur_ch = await find_window_by_keywords(driver, CHROME_WINDOW_KEYWORDS)
        if cur_ch is None or any(m in cur_ch.title.lower() for m in OAUTH_COMPLETED_TAB_MARKERS):
            break

        post_state = await capture_window_state(driver, cur_ch)
        post_elements = normalize_elements_hierarchy(getattr(post_state, "elements", None) or [])
        gate2, reason2 = scan_elements_for_auth_gate(post_elements)
        if gate2:
            raise SidecarRpcError(ERR_AUTH_GATE_DETECTED, f"Chrome OAuth yêu cầu xác minh: {reason2}")

        continue_el = _find_post_oauth_confirm_element(post_elements, clicked_post_steps)
        if continue_el is not None:
            step_sig = continue_el.element_token or _combined_text(continue_el).lower()
            clicked_post_steps.add(step_sig)
            await _click_target_on_window(
                driver, cur_ch, build_target_dict(continue_el), prefer_foreground=not ws_enabled
            )
            continue

        if asyncio.get_event_loop().time() >= conv_deadline:
            break

    return clicked_post_steps


async def select_chrome_account(
    driver: Any,
    email_hint: str,
    browser_profile_dir: str = "",
    expected_profile_name: str = "",
    settle_timeout_ms: int = 12000,
    require_evidence: bool = True,
    dry_run: bool = False,
) -> dict[str, Any]:
    """Step 4b (1 Account = 1 --profile-directory):
    - Scans all open Chrome windows; aborts immediately on phishing Omnibox or MFA/CAPTCHA.
    - If `xdg-open` misrouted Antigravity's OAuth PKCE URL into a DIFFERENT Chrome profile window,
      extracts the exact full OAuth PKCE URL, closes the misrouted tab (`Ctrl+W`), and raises
      `oauth_profile_relay_required` so Go immediately opens that exact PKCE URL in `--profile-directory=<target>`.
    - Supports both standard Account Chooser row clicks AND 1-Account-Per-Profile direct Consent / `Open xdg-open` fast-path!
    """
    if dry_run:
        return {"selectedAccount": email_hint, "dryRun": True}

    is_native = _is_native_cua_driver(driver)
    ws_enabled, _ = _agent_workspace_enabled()
    deadline = asyncio.get_event_loop().time() + max(1.5, settle_timeout_ms / 1000.0)
    email_needle = email_hint.strip().lower()
    last_missing_reason = "Chưa tìm thấy cửa sổ Google Chrome chứa trang chọn tài khoản"
    last_non_fatal_sec_err: SidecarRpcError | None = None

    while True:
        raw_candidates = await find_all_windows_by_keywords(driver, CHROME_WINDOW_KEYWORDS)
        candidates = _prioritize_chrome_candidates(raw_candidates, browser_profile_dir, expected_profile_name)
        misrouted_oauth_tab: tuple[WindowHandle, str, str] | None = None

        for chrome_handle in candidates:
            proc_exe = verify_pid_allowed(chrome_handle.pid, "browser", is_native)

            chrome_state = await capture_window_state(driver, chrome_handle, include_screenshot=True)
            elements = normalize_elements_hierarchy(getattr(chrome_state, "elements", None) or [])

            gate, reason = scan_elements_for_auth_gate(elements)
            if gate:
                raise SidecarRpcError(
                    ERR_AUTH_GATE_DETECTED,
                    f"Chrome OAuth yêu cầu xác thực bảo mật: {reason}",
                    {"windowTitle": chrome_handle.title},
                )

            raw_omnibox = extract_chrome_omnibox_url(elements)

            try:
                verified_origin, verified_prof = verify_chrome_omnibox_and_profile(
                    chrome_handle.title,
                    elements,
                    expected_profile_dir=browser_profile_dir,
                    expected_profile_name=expected_profile_name,
                )
            except SidecarRpcError as sec_err:
                reason_code = (sec_err.data or {}).get("reason", "")
                if sec_err.code == ERR_SECURITY_VIOLATION and reason_code == "omnibox_origin_violation":
                    raise

                # Lossless OAuth PKCE URL Relay (#8): If Antigravity's xdg-open landed in another Chrome Profile
                # window (e.g. "Personal" instead of "Work"), capture the exact OAuth2 PKCE URL for relay!
                if (
                    reason_code == "profile_mismatch"
                    and browser_profile_dir
                    and is_antigravity_oauth_authorize_url(raw_omnibox)
                ):
                    misrouted_oauth_tab = (
                        chrome_handle,
                        normalize_https_url(raw_omnibox),
                        str(sec_err.message),
                    )

                last_non_fatal_sec_err = sec_err
                continue

            if is_native:
                _pin_window_to_agent_workspace(chrome_handle)
            if not ws_enabled:
                await _focus_handle(driver, chrome_handle)

            # Path A: Click matching Google Account row in this --profile-directory!
            account_el = _find_chrome_account_element(elements, email_needle)
            if account_el is not None:
                b64, cap_scope, win_frame = await _capture_or_fallback_evidence_b64(
                    driver, chrome_state, chrome_handle, require_evidence, "04_pre_chrome_account_select"
                )
                target = build_target_dict(account_el)
                await _click_target_on_window(driver, chrome_handle, target, prefer_foreground=not ws_enabled)

                clicked_post = await _run_post_oauth_convergence(driver, ws_enabled, settle_timeout_ms)
                return {
                    "selectedAccount": email_hint or target.get("name", ""),
                    "chromeWindow": chrome_handle.title,
                    "postOAuthStepsClicked": len(clicked_post),
                    "evidence": _build_evidence_payload(
                        "04_pre_chrome_account_select",
                        chrome_handle,
                        proc_exe,
                        b64,
                        f"Xác minh Chrome Profile={verified_prof}, Origin={verified_origin}, không MFA trước khi chọn Account",
                        elements=elements,
                        browser_profile=verified_prof,
                        omnibox_origin=verified_origin,
                        capture_scope=cap_scope,
                        window_frame=win_frame,
                    ),
                }

            # Path B (1-Account-Per-Profile Direct Consent / Protocol Dialog Fast-Path):
            # When a dedicated `--profile-directory` has only 1 Google Account signed in, Google OAuth may skip
            # the chooser row and display 'Continue' / 'Allow' / 'Open xdg-open' directly!
            has_other_email = _page_contains_conflicting_email(elements, email_needle)
            if not has_other_email and is_antigravity_oauth_authorize_url(raw_omnibox):
                direct_confirm_el = _find_post_oauth_confirm_element(elements, set())
                if direct_confirm_el is not None:
                    b64, cap_scope, win_frame = await _capture_or_fallback_evidence_b64(
                        driver, chrome_state, chrome_handle, require_evidence, "04_pre_chrome_account_select"
                    )
                    direct_target = build_target_dict(direct_confirm_el)
                    first_sig = direct_confirm_el.element_token or _combined_text(direct_confirm_el).lower()
                    await _click_target_on_window(
                        driver, chrome_handle, direct_target, prefer_foreground=not ws_enabled
                    )
                    clicked_post = await _run_post_oauth_convergence(
                        driver, ws_enabled, settle_timeout_ms, initial_clicked={first_sig}
                    )
                    return {
                        "selectedAccount": email_hint or verified_prof,
                        "chromeWindow": chrome_handle.title,
                        "fastPathDirectConfirm": True,
                        "postOAuthStepsClicked": len(clicked_post),
                        "evidence": _build_evidence_payload(
                            "04_pre_chrome_account_select",
                            chrome_handle,
                            proc_exe,
                            b64,
                            f"Xác minh Chrome Profile={verified_prof} (1-Account Direct Consent), Origin={verified_origin}",
                            elements=elements,
                            browser_profile=verified_prof,
                            omnibox_origin=verified_origin,
                            capture_scope=cap_scope,
                            window_frame=win_frame,
                        ),
                    }

            # If this window is an OAuth authorize tab showing ONLY a DIFFERENT account email (e.g. Chrome didn't
            # show a profile badge in the title, yet xdg-open landed in Default with acc1@ instead of Profile 1 with acc2@),
            # mark it for lossless OAuth PKCE URL Relay into `browser_profile_dir`!
            if has_other_email and browser_profile_dir and is_antigravity_oauth_authorize_url(raw_omnibox):
                misrouted_oauth_tab = (
                    chrome_handle,
                    normalize_https_url(raw_omnibox),
                    f"Tab OAuth mở nhầm vào Profile chứa tài khoản khác (cần '{email_hint}' trên '{browser_profile_dir}')",
                )

            last_missing_reason = (
                f"Đã xác minh Chrome ('{chrome_handle.title}') nhưng không thấy tài khoản khớp '{email_hint}' "
                f"trên Profile '{browser_profile_dir or 'Default'}'"
            )

        # Immediately relay misrouted OAuth PKCE URL to Go's BrowserRouter (closing the misrouted tab cleanly)!
        if misrouted_oauth_tab is not None:
            bad_handle, relay_url, relay_detail = misrouted_oauth_tab
            await _press_hotkey_on_window(driver, bad_handle, ["ctrl", "w"])
            raise SidecarRpcError(
                ERR_SECURITY_VIOLATION,
                f"Chuyển tiếp URL OAuth PKCE sang đúng Chrome Profile '{browser_profile_dir}': {relay_detail}",
                {
                    "reason": "oauth_profile_relay_required",
                    "relayOAuthUrl": relay_url,
                    "browserProfileDir": browser_profile_dir,
                },
            )

        if asyncio.get_event_loop().time() >= deadline:
            break
        await asyncio.sleep(0.6)

    if last_non_fatal_sec_err is not None:
        raise last_non_fatal_sec_err
    raise SidecarRpcError(ERR_ELEMENT_NOT_FOUND, last_missing_reason, {"emailHint": email_hint})


async def select_default_model(
    driver: Any,
    model_name: str,
    settle_timeout_ms: int = 10000,
    require_evidence: bool = True,
    dry_run: bool = False,
) -> dict[str, Any]:
    """Step 5: Wait for post-OAuth workspace -> Verify + Capture Evidence #5 -> Select Default Model -> Close stale OAuth tab."""
    wanted = model_name.strip()
    if not wanted:
        raise SidecarRpcError(ERR_ELEMENT_NOT_FOUND, "Tên model mặc định không được để trống")

    if dry_run:
        return {"selectedModel": wanted, "dryRun": True}

    is_native = _is_native_cua_driver(driver)
    ws_enabled, _ = _agent_workspace_enabled()
    deadline = asyncio.get_event_loop().time() + max(1.5, settle_timeout_ms / 1000.0)
    handle: WindowHandle | None = None
    selector_el: NormalizedElement | None = None
    win_state: Any = None
    elements: list[NormalizedElement] = []
    proc_exe = ""

    while True:
        handle = await find_window_by_keywords(driver, [get_target_app()])
        if handle is not None:
            proc_exe = verify_pid_allowed(handle.pid, "antigravity", is_native)
            if is_native:
                _pin_window_to_agent_workspace(handle)
            if not ws_enabled:
                await _focus_handle(driver, handle)

            win_state = await capture_window_state(driver, handle, include_screenshot=True)
            elements = normalize_elements_hierarchy(getattr(win_state, "elements", None) or [])

            gate, reason = scan_elements_for_auth_gate(elements, ide_mode=True)
            if gate:
                raise SidecarRpcError(ERR_AUTH_GATE_DETECTED, reason)

            selector_el = _find_model_selector_element(elements)
            if selector_el is not None:
                break

        if asyncio.get_event_loop().time() >= deadline:
            break
        await asyncio.sleep(0.6)

    if handle is None:
        raise SidecarRpcError(ERR_WINDOW_NOT_FOUND, f"Không tìm thấy cửa sổ {get_target_app()}")
    if selector_el is None:
        raise SidecarRpcError(
            ERR_ELEMENT_NOT_FOUND,
            "Không tìm thấy bộ chọn Model trên Antigravity sau khi đăng nhập (quá hạn chờ OAuth callback)",
        )

    b64, cap_scope, win_frame = await _capture_or_fallback_evidence_b64(
        driver, win_state, handle, require_evidence, "05_pre_model_select"
    )
    post_login_parsed = analyze_window_state(win_state, [get_target_app()])
    detected_account = post_login_parsed.get("activeAccountHint", "")
    post_login_exhausted = bool(post_login_parsed.get("quotaExhausted", False))

    selector_text = _combined_text(selector_el)
    if _model_matches(selector_text, wanted):
        closed_tabs = await _cleanup_completed_oauth_browser_tabs(driver)
        return {
            "selectedModel": wanted,
            "alreadyActive": True,
            "closedOAuthTabs": closed_tabs,
            "detectedAccountHint": detected_account,
            "postLoginQuotaExhausted": post_login_exhausted,
            "evidence": _build_evidence_payload(
                "05_pre_model_select",
                handle,
                proc_exe,
                b64,
                f"Xác minh phiên mới trên Antigravity đã sẵn sàng với model mặc định '{wanted}'",
                elements=elements,
                capture_scope=cap_scope,
                window_frame=win_frame,
            ),
        }

    await _click_target_on_window(driver, handle, build_target_dict(selector_el))
    await asyncio.sleep(0.6)

    new_state = await capture_window_state(driver, handle)
    new_elements = normalize_elements_hierarchy(getattr(new_state, "elements", None) or [])
    option_el = _find_model_option_element(new_elements, wanted)
    if option_el is None:
        await _dismiss_popover_escape(driver, handle)
        raise SidecarRpcError(
            ERR_ELEMENT_NOT_FOUND,
            f"Không tìm thấy model '{wanted}' trong danh sách Model của Antigravity",
            {"model": wanted},
        )

    await _click_target_on_window(driver, handle, build_target_dict(option_el))
    closed_tabs = await _cleanup_completed_oauth_browser_tabs(driver)

    return {
        "selectedModel": wanted,
        "alreadyActive": False,
        "closedOAuthTabs": closed_tabs,
        "detectedAccountHint": detected_account,
        "postLoginQuotaExhausted": post_login_exhausted,
        "evidence": _build_evidence_payload(
            "05_pre_model_select",
            handle,
            proc_exe,
            b64,
            f"Xác minh Antigravity trước khi chọn model mặc định '{wanted}'",
            elements=elements,
            capture_scope=cap_scope,
            window_frame=win_frame,
        ),
    }


async def list_available_models(driver: Any, target_keywords: list[str] | None = None) -> dict[str, Any]:
    """Open Antigravity's Model menu briefly, extract all selectable model names, and dismiss with Escape."""
    kws = target_keywords or [get_target_app()]
    handle = await find_window_by_keywords(driver, kws)
    if handle is None:
        raise SidecarRpcError(ERR_WINDOW_NOT_FOUND, f"Không tìm thấy cửa sổ {get_target_app()}")

    is_native = _is_native_cua_driver(driver)
    verify_pid_allowed(handle.pid, "antigravity", is_native)
    st = await capture_window_state(driver, handle, include_screenshot=False)
    parsed = analyze_window_state(st, kws)
    els = normalize_elements_hierarchy(getattr(st, "elements", None) or [])
    active_model = parsed.get("activeModel", "")

    models: list[str] = []
    if active_model:
        models.append(active_model)

    selector_el = _find_model_selector_element(els)
    if selector_el is not None:
        await _click_target_on_window(driver, handle, build_target_dict(selector_el), prefer_foreground=False)
        await asyncio.sleep(0.35)
        menu_state = await capture_window_state(driver, handle, include_screenshot=False)
        menu_els = normalize_elements_hierarchy(getattr(menu_state, "elements", None) or [])
        from michael_cua_sidecar.phrases import MODEL_FAMILY_HINTS

        def _collect(nodes: list[NormalizedElement], in_sys: bool) -> None:
            for n in nodes:
                r = n.role.lower()
                now_sys = in_sys or r in SYSTEM_SURFACE_ROLES
                cleaned = _combined_text(n).replace("\ufffc", " ").strip()
                low = cleaned.lower()
                if (
                    now_sys
                    and cleaned
                    and len(cleaned) <= 80
                    and "view usage" not in low
                    and "select model" not in low
                    and any(h in low for h in MODEL_FAMILY_HINTS)
                    and r in CLICKABLE_ROLES
                ):
                    if cleaned not in models:
                        models.append(cleaned)
                if n.children:
                    _collect(n.children, now_sys)

        _collect(menu_els, False)
        await _dismiss_popover_escape(driver, handle)

    return {"activeModel": active_model, "models": models}


async def inspect_ui_selectors(driver: Any, target_keywords: list[str] | None = None) -> dict[str, Any]:
    """Read-only E2E calibration inspector (--inspect-ui): inspects live Antigravity & Chrome trees safely."""
    kws = target_keywords or [get_target_app()]
    ag_windows = await find_all_windows_by_keywords(driver, kws)
    chrome_windows = await find_all_windows_by_keywords(driver, CHROME_WINDOW_KEYWORDS)

    ag_reports: list[dict[str, Any]] = []
    for w in ag_windows:
        st = await capture_window_state(driver, w, include_screenshot=False)
        parsed = analyze_window_state(st, kws)
        els = normalize_elements_hierarchy(getattr(st, "elements", None) or [])
        quota_el = _find_quota_trigger_element(els)
        if quota_el is not None and "quota_button" not in parsed["targets"]:
            parsed["targets"]["quota_button"] = build_target_dict(quota_el)
        matched_keys = sorted(list((parsed.get("targets") or {}).keys()))
        quota_path = "direct_quota_button" if "quota_button" in matched_keys else (
            "model_selector -> view_usage" if "model_selector" in matched_keys else "unavailable"
        )
        ag_reports.append(
            {
                "pid": w.pid,
                "windowId": str(w.window_id),
                "title": parsed.get("windowTitle") or w.title,
                "elementCount": parsed.get("elementCount", 0),
                "hollowWindow": parsed.get("hollowWindow", False),
                "appAuthState": parsed.get("appAuthState", "unknown"),
                "activeModel": parsed.get("activeModel", ""),
                "remainingQuotaPct": parsed.get("remainingQuotaPct"),
                "quotaExhausted": parsed.get("quotaExhausted", False),
                "authGateDetected": parsed.get("authGateDetected", False),
                "quotaInspectionPath": quota_path,
                "matchedSelectors": matched_keys,
            }
        )

    chrome_reports: list[dict[str, Any]] = []
    for cw in chrome_windows:
        cst = await capture_window_state(driver, cw, include_screenshot=False)
        cels = normalize_elements_hierarchy(getattr(cst, "elements", None) or [])
        total = max(
            count_element_nodes(cels),
            int(getattr(cst, "total_element_count", 0) or getattr(cst, "element_count", 0) or 0),
        )
        omnibox = extract_chrome_omnibox_url(cels)
        acc_el = _find_chrome_account_element(cels, "")
        chrome_reports.append(
            {
                "pid": cw.pid,
                "windowId": str(cw.window_id),
                "title": cw.title,
                "elementCount": total,
                "hollowWindow": total == 0,
                "omniboxDetected": bool(omnibox),
                "accountRowDetected": acc_el is not None,
            }
        )

    remediation_hints: list[str] = []
    if sys.platform.startswith("linux") and _is_native_cua_driver(driver) and not ag_reports:
        dormant_pids = _detect_running_unregistered_target_pids("antigravity")
        if dormant_pids:
            remediation_hints.append(
                f"Phát hiện tiến trình Antigravity đang chạy (PID {dormant_pids[0]}) nhưng được mở TRƯỚC KHI bật Accessibility nên chưa đăng ký vào bus AT-SPI2. "
                "Hãy đóng và mở lại Antigravity (hoặc chạy: ACCESSIBILITY_ENABLED=1 /opt/antigravity/antigravity --force-renderer-accessibility &) rồi chạy lại --inspect-ui."
            )

    return {
        "e2eReady": len(ag_reports) > 0 and not any(r.get("hollowWindow") for r in ag_reports),
        "antigravityWindows": ag_reports,
        "chromeWindows": chrome_reports,
        "remediationHints": remediation_hints,
    }


def _detect_running_unregistered_target_pids(exe_keyword: str) -> list[int]:
    """Check /proc for running main target processes (excluding --type=renderer/zygote/gpu-process helpers)."""
    if not sys.platform.startswith("linux") or not os.path.isdir("/proc"):
        return []
    found: list[int] = []
    kw = exe_keyword.lower()
    try:
        for entry in os.listdir("/proc"):
            if not entry.isdigit():
                continue
            pid = int(entry)
            try:
                with open(f"/proc/{pid}/cmdline", "rb") as f:
                    raw = f.read(512).replace(b"\x00", b" ").decode("utf-8", errors="replace").strip()
            except OSError:
                continue
            low = raw.lower()
            if kw in low and "--type=" not in low and "michael-auto" not in low:
                found.append(pid)
    except OSError:
        pass
    return sorted(found)


def _combined_text(el: NormalizedElement) -> str:
    return f"{el.label} {el.text} {el.value} {el.description}".strip()


def _normalize_model_key(raw: str) -> str:
    return re.sub(r"[^a-z0-9]+", "", raw.lower())


def _model_matches(candidate_text: str, wanted_model: str) -> bool:
    cand_lower = candidate_text.lower()
    want_lower = wanted_model.lower()
    if want_lower in cand_lower:
        return True
    norm_cand = _normalize_model_key(candidate_text)
    norm_want = _normalize_model_key(wanted_model)
    return bool(norm_want) and norm_want in norm_cand


def _find_model_selector_element(elements: list[NormalizedElement]) -> NormalizedElement | None:
    """Prefer explicit 'Select model' / 'Model selection' control over generic buttons mentioning model names."""
    explicit = _find_clickable_by_phrases(
        elements, ["select model", "model selection", "choose model", "chọn model", "chọn mô hình"]
    )
    if explicit is not None:
        return explicit
    return _find_clickable_by_phrases(elements, MODEL_SELECTOR_PHRASES)


def _find_quota_trigger_element(
    elements: list[NormalizedElement],
    *,
    inside_editor: bool = False,
    inside_chat: bool = False,
    inside_system: bool = False,
) -> NormalizedElement | None:
    explicit = _find_clickable_by_phrases(
        elements,
        VIEW_QUOTA_PHRASES,
        inside_editor=inside_editor,
        inside_chat=inside_chat,
        inside_system=inside_system,
    )
    if explicit is not None:
        return explicit

    for el in elements:
        role = el.role.lower()
        now_system = inside_system or role in SYSTEM_SURFACE_ROLES or (
            role == "landmark" and "notification" in el.label.lower()
        )
        now_editor = inside_editor or el.editable or role in EDITOR_ROLES
        now_chat = inside_chat or ((not now_system) and (role in CONVERSATION_CONTENT_ROLES or role == "list item"))
        if now_editor or now_chat:
            continue

        cleaned = _combined_text(el).replace("\ufffc", " ").strip()
        if is_valid_ide_control(el, cleaned, inside_system=now_system) and _STATUS_PCT_RE.search(cleaned):
            return el
        if el.children:
            found = _find_quota_trigger_element(
                el.children,
                inside_editor=now_editor,
                inside_chat=now_chat,
                inside_system=now_system,
            )
            if found is not None:
                return found
    return None


def _find_confirm_dialog_button(elements: list[NormalizedElement], inside_dialog: bool = False) -> NormalizedElement | None:
    for el in elements:
        role = el.role.lower()
        now_dialog = inside_dialog or role in ("dialog", "alert dialog", "alert")
        combined = _combined_text(el).lower()

        if now_dialog and role in CLICKABLE_ROLES and combined:
            if not any(c in combined for c in CANCEL_DIALOG_PHRASES) and any(
                p in combined for p in CONFIRM_SIGNOUT_PHRASES
            ):
                return el

        if el.children:
            found = _find_confirm_dialog_button(el.children, now_dialog)
            if found is not None:
                return found
    return None


def _find_post_oauth_confirm_element(elements: list[NormalizedElement], already_clicked: set[str]) -> NormalizedElement | None:
    for el in elements:
        role = el.role.lower()
        combined = _combined_text(el).lower()
        sig = el.element_token or combined
        if (
            role in CLICKABLE_ROLES
            and combined
            and sig not in already_clicked
            and not any(neg in combined for neg in POST_OAUTH_NEGATIVE_PHRASES)
            and any(pos in combined for pos in POST_OAUTH_CONTINUE_PHRASES)
        ):
            return el
        if el.children:
            found = _find_post_oauth_confirm_element(el.children, already_clicked)
            if found is not None:
                return found
    return None


def _find_clickable_by_phrases(
    elements: list[NormalizedElement],
    phrases: list[str],
    *,
    inside_editor: bool = False,
    inside_chat: bool = False,
    inside_system: bool = False,
) -> NormalizedElement | None:
    for el in elements:
        role = el.role.lower()
        now_system = inside_system or role in SYSTEM_SURFACE_ROLES or (
            role == "landmark" and "notification" in el.label.lower()
        )
        now_editor = inside_editor or el.editable or role in EDITOR_ROLES
        now_chat = inside_chat or ((not now_system) and (role in CONVERSATION_CONTENT_ROLES or role == "list item"))
        if now_editor or now_chat:
            continue

        cleaned = _combined_text(el).replace("\ufffc", " ").strip()
        combined = cleaned.lower()
        if is_valid_ide_control(el, cleaned, inside_system=now_system) and any(p in combined for p in phrases):
            return el
        if el.children:
            found = _find_clickable_by_phrases(
                el.children,
                phrases,
                inside_editor=now_editor,
                inside_chat=now_chat,
                inside_system=now_system,
            )
            if found is not None:
                return found
    return None


def _find_chrome_account_element(elements: list[NormalizedElement], email_needle: str) -> NormalizedElement | None:
    for el in elements:
        role = el.role.lower()
        combined = _combined_text(el).lower()

        if combined and not any(excl in combined for excl in CHROME_EXCLUDED_ACCOUNT_PHRASES):
            if email_needle:
                if email_needle in combined and (role in CLICKABLE_ROLES or bool(el.element_token)):
                    return el
            else:
                if _EMAIL_RE.search(combined) and (role in CLICKABLE_ROLES or bool(el.element_token)):
                    return el

        if el.children:
            found = _find_chrome_account_element(el.children, email_needle)
            if found is not None:
                return found
    return None


def _page_contains_conflicting_email(elements: list[NormalizedElement], expected_email_needle: str) -> bool:
    """Return True if the page displays an `@` email address that does NOT match `expected_email_needle`."""
    needle = expected_email_needle.strip().lower()
    if not needle:
        return False

    found_emails: set[str] = set()

    def _walk(nodes: list[NormalizedElement]) -> None:
        for n in nodes:
            txt = _combined_text(n).lower()
            for m in _EMAIL_RE.findall(txt):
                found_emails.add(m.lower())
            if n.children:
                _walk(n.children)

    _walk(elements)
    if not found_emails:
        return False
    if any(needle in em for em in found_emails):
        return False
    return True


def _find_model_option_element(
    elements: list[NormalizedElement],
    wanted_model: str,
    *,
    inside_editor: bool = False,
    inside_chat: bool = False,
    inside_system: bool = False,
) -> NormalizedElement | None:
    for el in elements:
        role = el.role.lower()
        now_system = inside_system or role in SYSTEM_SURFACE_ROLES or (
            role == "landmark" and "notification" in el.label.lower()
        )
        now_editor = inside_editor or el.editable or role in EDITOR_ROLES
        now_chat = inside_chat or ((not now_system) and (role in CONVERSATION_CONTENT_ROLES or role == "list item"))
        if now_editor or now_chat:
            continue

        if el.children:
            found = _find_model_option_element(
                el.children,
                wanted_model,
                inside_editor=now_editor,
                inside_chat=now_chat,
                inside_system=now_system,
            )
            if found is not None:
                return found

        if el.height > 0 and (el.y + el.height) <= 0:
            continue

        cleaned = _combined_text(el).replace("\ufffc", " ").strip()
        if not cleaned or len(cleaned) > 90 or cleaned.startswith(("•", "▪", "◦")):
            continue
        if _model_matches(cleaned, wanted_model) and (
            role in CLICKABLE_ROLES or role == "label" or bool(el.element_token)
        ):
            return el
    return None

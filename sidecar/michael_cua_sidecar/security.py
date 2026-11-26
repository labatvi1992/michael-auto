"""Security & Pre-Actuation Evidence Verification guards for michael_cua_sidecar (v2.4).

Enforces:
1. Strict Linux /proc/<pid> executable allowlist (only Antigravity & configured browsers).
2. Strict Chrome Omnibox origin verification (must be accounts.google.com), Profile badge check,
   and lossless OAuth2 PKCE URL recognition for 1-Account-Per-Profile-Dir relay.
3. Non-empty Screenshot Evidence extraction across UniFFI `WindowStateOutput.images[0].data_base64`,
   `screenshot_file_path`, and GNOME 51 fallback capture, plus editor bounding-box privacy masking.
"""

from __future__ import annotations

import base64
import os
import sys
from typing import Any
from urllib.parse import urlparse

from michael_cua_sidecar.errors import (
    ERR_EVIDENCE_MISSING,
    ERR_SECURITY_VIOLATION,
    SidecarRpcError,
)
from michael_cua_sidecar.observer import NormalizedElement, normalize_elements_hierarchy
from michael_cua_sidecar.phrases import EDITOR_ROLES

ANTIGRAVITY_ALLOWED_PROCS = ("antigravity", "agy", "electron", "code")
BROWSER_ALLOWED_PROCS = ("chrome", "chromium", "brave", "msedge", "microsoft-edge", "firefox")

OMNIBOX_LABEL_HINTS = (
    "address and search bar",
    "thanh địa chỉ",
    "search or type web address",
    "search or enter address",
    "omnibox",
    "url bar",
    "location",
)

ALLOWED_GOOGLE_OAUTH_HOSTS = {
    "accounts.google.com",
    "myaccount.google.com",
}

NON_FATAL_DEGRADED_REASONS = {
    "accessibility_window_identity_unproven",
}


def inspect_process_identity(pid: int) -> str:
    """Read /proc/<pid> metadata on Linux for audit evidence and allowlist enforcement."""
    if pid <= 0 or not sys.platform.startswith("linux"):
        return ""
    proc_dir = f"/proc/{pid}"
    if not os.path.exists(proc_dir):
        return ""

    parts: list[str] = []
    try:
        exe = os.readlink(f"{proc_dir}/exe")
        if exe:
            parts.append(exe)
    except OSError:
        pass
    try:
        with open(f"{proc_dir}/comm", "r", encoding="utf-8", errors="replace") as f:
            comm = f.read().strip()
            if comm:
                parts.append(comm)
    except OSError:
        pass
    try:
        with open(f"{proc_dir}/cmdline", "rb") as f:
            raw = f.read(512).replace(b"\x00", b" ").decode("utf-8", errors="replace").strip()
            if raw:
                parts.append(raw)
    except OSError:
        pass
    return " | ".join(parts)


def verify_pid_allowed(pid: int, role: str, is_native_driver: bool) -> str:
    """Ensure the target window PID belongs strictly to Antigravity or an allowed browser."""
    if not is_native_driver or pid <= 0 or not sys.platform.startswith("linux"):
        return f"mock_or_virtual_pid:{pid}"

    identity = inspect_process_identity(pid)
    if not identity:
        raise SidecarRpcError(
            ERR_SECURITY_VIOLATION,
            f"Không xác minh được tiến trình đích PID={pid} trong /proc",
            {"pid": pid, "role": role, "reason": "pid_violation"},
        )

    ident_lower = identity.lower()
    allowed = ANTIGRAVITY_ALLOWED_PROCS if role == "antigravity" else BROWSER_ALLOWED_PROCS
    if not any(token in ident_lower for token in allowed):
        raise SidecarRpcError(
            ERR_SECURITY_VIOLATION,
            f"Từ chối truy cập cửa sổ PID={pid}: tiến trình '{identity[:80]}' không nằm trong danh sách cho phép ({role})",
            {"pid": pid, "role": role, "reason": "pid_violation"},
        )
    return identity[:160]


def pid_matches_browser_profile_dir(pid: int, expected_profile_dir: str) -> bool:
    """Check if /proc/<pid>/cmdline explicitly specifies --profile-directory=<expected_profile_dir>."""
    if pid <= 0 or not expected_profile_dir or not sys.platform.startswith("linux"):
        return False
    identity = inspect_process_identity(pid).lower()
    needle = f"--profile-directory={expected_profile_dir.strip().lower()}"
    return needle in identity


def normalize_https_url(raw_url: str) -> str:
    s = (raw_url or "").strip()
    if not s:
        return ""
    if "://" not in s:
        s = "https://" + s
    return s


def is_antigravity_oauth_authorize_url(raw_url: str) -> bool:
    """Return True if raw_url is a genuine Google OAuth2 authorization/PKCE URL (carrying callback params),
    distinguishing a live `xdg-open` Antigravity OAuth tab from a plain AccountChooser page."""
    candidate = normalize_https_url(raw_url)
    if not candidate:
        return False
    parsed = urlparse(candidate)
    host = (parsed.hostname or "").lower()
    if host != "accounts.google.com":
        return False
    combined = f"{parsed.path}?{parsed.query}".lower()
    return any(
        marker in combined
        for marker in (
            "/o/oauth2",
            "/signin/oauth",
            "client_id=",
            "redirect_uri=",
            "code_challenge=",
            "response_type=",
        )
    )


def extract_chrome_omnibox_url(elements: list[Any]) -> str:
    norm = normalize_elements_hierarchy(elements)
    return _find_omnibox_value(norm)


def verify_chrome_omnibox_and_profile(
    window_title: str,
    elements: list[Any],
    expected_profile_dir: str = "",
    expected_profile_name: str = "",
) -> tuple[str, str]:
    """Verify Chrome's Omnibox points strictly to accounts.google.com and check Profile badge consistency.

    Returns:
        (verified_origin, detected_profile_info)
    """
    omnibox_val = extract_chrome_omnibox_url(elements)
    verified_origin = "accounts.google.com (title-verified)"

    if omnibox_val:
        candidate_url = normalize_https_url(omnibox_val)
        parsed = urlparse(candidate_url)
        host = (parsed.hostname or "").lower()
        if host not in ALLOWED_GOOGLE_OAUTH_HOSTS:
            raise SidecarRpcError(
                ERR_SECURITY_VIOLATION,
                f"Chặn bảo mật chống click nhầm/phishing: thanh địa chỉ Chrome đang ở '{host or omnibox_val}', không phải accounts.google.com",
                {"omnibox": f"{parsed.scheme}://{host}{parsed.path}" if host else "", "windowTitle": window_title, "reason": "omnibox_origin_violation"},
            )
        verified_origin = f"https://{host}{parsed.path}"
    else:
        title_lower = window_title.lower()
        if not any(k in title_lower for k in ("google accounts", "choose an account", "chọn tài khoản", "sign in")):
            raise SidecarRpcError(
                ERR_SECURITY_VIOLATION,
                f"Cửa sổ trình duyệt '{window_title}' không phải trang xác thực Google Accounts hợp lệ",
                {"windowTitle": window_title, "reason": "window_not_oauth"},
            )

    # Check if window title explicitly advertises a DIFFERENT Chrome profile badge (e.g. "- Personal - Google Chrome").
    detected_profile = expected_profile_dir or "Default"
    exp_name = expected_profile_name.strip()
    if exp_name and " - " in window_title:
        segments = [s.strip() for s in window_title.split(" - ") if s.strip()]
        if len(segments) >= 3:
            badge_candidate = segments[-2]
            if (
                badge_candidate.lower() not in ("google accounts", "sign in", "choose an account", "chọn tài khoản")
                and exp_name.lower() not in badge_candidate.lower()
            ):
                safe_omnibox = verified_origin if omnibox_val else ""
                raise SidecarRpcError(
                    ERR_SECURITY_VIOLATION,
                    f"Cửa sổ Chrome đang mở ở Profile '{badge_candidate}', không khớp Profile cấu hình '{exp_name}' ({expected_profile_dir})",
                    {
                        "detectedProfile": badge_candidate,
                        "expectedProfile": exp_name,
                        "omnibox": safe_omnibox,
                        "reason": "profile_mismatch",
                    },
                )
            detected_profile = f"{expected_profile_dir} ({badge_candidate})"

    return verified_origin, detected_profile


def collect_editor_redaction_rects(elements: list[Any], out: list[dict[str, int]] | None = None) -> list[dict[str, int]]:
    """Collect bounding boxes of code/document/chat editor panes (from both flat UniFFI `ElementFrame`
    and nested mock elements) so EvidenceVault masks private project text."""
    if out is None:
        out = []
    norm = normalize_elements_hierarchy(elements)
    _collect_normalized_editor_rects(norm, out)
    return out


def _collect_normalized_editor_rects(nodes: list[NormalizedElement], out: list[dict[str, int]]) -> None:
    for el in nodes:
        role = el.role.lower()
        # Never mask an entire full-screen root Electron `document web` container (which would black out
        # Settings, Model/Usage popovers, and Quota alerts); mask only real editable/code/chat content panes!
        is_full_root_webview = role == "document web" and not el.editable
        is_large_editor = (
            not is_full_root_webview
            and (role in EDITOR_ROLES or (el.editable and role not in ("entry", "combo box")))
            and el.width >= 120
            and el.height >= 60
        )
        if is_large_editor:
            out.append({"x": max(0, el.x), "y": max(0, el.y), "width": el.width, "height": el.height})
            continue

        if el.children:
            _collect_normalized_editor_rects(el.children, out)


def extract_and_verify_screenshot_b64(
    window_state: Any,
    require_evidence: bool,
    step_name: str,
    fallback_png_bytes: bytes | None = None,
) -> str:
    """Extract base64-encoded image from UniFFI `WindowStateOutput.images` (`SnapshotImage.data_base64`),
    `screenshot_file_path`, legacy mock attributes, or GNOME 51 fallback capture."""
    if getattr(window_state, "screenshot_frame_valid", None) is False and not fallback_png_bytes:
        if require_evidence:
            raise SidecarRpcError(
                ERR_EVIDENCE_MISSING,
                f"Khung ảnh chụp ở bước '{step_name}' không hợp lệ (screenshot_frame_valid=false) — từ chối thực thi",
                {"step": step_name},
            )

    degraded = bool(getattr(window_state, "degraded", False))
    degraded_reason = str(getattr(window_state, "degraded_reason", "") or "").strip().lower()
    if degraded and degraded_reason not in NON_FATAL_DEGRADED_REASONS and require_evidence and not fallback_png_bytes:
        # Still permit if non-empty UniFFI SnapshotImage is present and degraded_reason is merely informational;
        # reject strictly if degraded without an explicit non-fatal reason and no SnapshotImage exists.
        if not (getattr(window_state, "images", None) or []):
            raise SidecarRpcError(
                ERR_EVIDENCE_MISSING,
                f"Ảnh chụp xác minh ở bước '{step_name}' bị đánh dấu suy giảm ({degraded_reason or 'degraded'}) — từ chối thực thi",
                {"step": step_name, "degradedReason": degraded_reason},
            )

    # Tier 1: Real UniFFI WindowStateOutput.images: List[SnapshotImage(mime_type, data_base64)]
    for img in getattr(window_state, "images", None) or []:
        for attr_name in ("data_base64", "base64", "data"):
            val = getattr(img, attr_name, None)
            if isinstance(val, (bytes, bytearray)) and len(val) > 0:
                return base64.b64encode(bytes(val)).decode("ascii")
            if isinstance(val, str) and val.strip():
                s = val.strip()
                if s.startswith("data:image/") and "," in s:
                    s = s.split(",", 1)[1]
                return s

    # Tier 2: UniFFI WindowStateOutput.screenshot_file_path
    file_path = str(getattr(window_state, "screenshot_file_path", "") or "").strip()
    if file_path and os.path.isfile(file_path):
        try:
            with open(file_path, "rb") as f:
                raw_file = f.read()
            if raw_file:
                return base64.b64encode(raw_file).decode("ascii")
        except OSError:
            pass

    # Tier 3: Mock/test candidates
    raw_candidates = [
        getattr(window_state, "screenshot_base64", None),
        getattr(window_state, "screenshot", None),
        getattr(window_state, "image_bytes", None),
        getattr(window_state, "screenshot_png", None),
    ]
    for cand in raw_candidates:
        if cand is None:
            continue
        if isinstance(cand, (bytes, bytearray)) and len(cand) > 0:
            return base64.b64encode(bytes(cand)).decode("ascii")
        if isinstance(cand, str) and cand.strip():
            s = cand.strip()
            if s.startswith("data:image/") and "," in s:
                s = s.split(",", 1)[1]
            return s
        for attr in ("data_base64", "base64", "data", "bytes"):
            nested = getattr(cand, attr, None)
            if isinstance(nested, (bytes, bytearray)) and len(nested) > 0:
                return base64.b64encode(bytes(nested)).decode("ascii")
            if isinstance(nested, str) and nested.strip():
                return nested.strip()

    # Tier 4: GNOME Shell 51 / Portal fallback PNG bytes
    if fallback_png_bytes and len(fallback_png_bytes) > 0:
        return base64.b64encode(fallback_png_bytes).decode("ascii")

    if require_evidence:
        raise SidecarRpcError(
            ERR_EVIDENCE_MISSING,
            f"Không thu được ảnh chụp màn hình Evidence bắt buộc tại bước '{step_name}' — dừng thực thi an toàn",
            {"step": step_name},
        )
    return ""


def _find_omnibox_value(elements: list[NormalizedElement]) -> str:
    for el in elements:
        role = el.role.lower()
        meta = f"{el.label.lower()} {el.description.lower()}"

        if role in ("entry", "text", "autocomplete", "combo box") and any(h in meta for h in OMNIBOX_LABEL_HINTS):
            val = (el.value or el.text).strip()
            if val:
                return val

        if el.children:
            found = _find_omnibox_value(el.children)
            if found:
                return found
    return ""

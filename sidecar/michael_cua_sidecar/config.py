"""Environment configuration for Michael Auto CUA sidecar."""

from __future__ import annotations

import os


def ensure_wayland_env() -> None:
    """Enable Wayland support in cua-driver on Linux Wayland sessions unless explicitly disabled."""
    if os.environ.get("CUA_DRIVER_RS_ENABLE_WAYLAND") is not None:
        return
    session_type = os.environ.get("XDG_SESSION_TYPE", "").lower()
    if session_type == "wayland" or bool(os.environ.get("WAYLAND_DISPLAY")):
        os.environ["CUA_DRIVER_RS_ENABLE_WAYLAND"] = "1"


def get_session_name() -> str:
    return os.environ.get("CUA_SESSION_NAME", "michael_auto").strip() or "michael_auto"


def get_target_app() -> str:
    return os.environ.get("CUA_TARGET_APP", "Antigravity").strip() or "Antigravity"


def get_delivery_mode() -> str:
    mode = os.environ.get("CUA_DELIVERY_MODE", "auto").strip().lower()
    if mode in ("background", "foreground", "auto"):
        return mode
    return "auto"

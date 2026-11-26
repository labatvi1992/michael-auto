"""Standardized JSON-RPC 2.0 domain error codes for Michael Auto CUA Sidecar."""

from __future__ import annotations

from typing import Any

ERR_ELEMENT_NOT_FOUND = -32001
ERR_WINDOW_NOT_FOUND = -32002
ERR_DRIVER_UNAVAILABLE = -32003
ERR_AUTH_GATE_DETECTED = -32004
ERR_ACTION_FAILED = -32005
ERR_SECURITY_VIOLATION = -32006
ERR_EVIDENCE_MISSING = -32007
ERR_REQUEST_CANCELLED = -32800


class SidecarRpcError(Exception):
    """Domain exception serialized cleanly into a JSON-RPC 2.0 error object."""

    def __init__(self, code: int, message: str, data: dict[str, Any] | None = None) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.data = data or {}

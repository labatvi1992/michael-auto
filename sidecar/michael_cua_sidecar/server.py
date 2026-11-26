"""Multiplexed Asyncio JSON-RPC 2.0 stdio server for Michael Auto CUA Sidecar (v2.3).

Supports concurrent non-blocking RPC execution AND instant `$/cancelRequest` cancellation
so <20ms human mouse/keyboard interrupts in Go immediately abort in-flight Python coroutines
with zero post-interrupt 'ghost clicks'.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import signal
import sys
import threading
import time
import traceback
from typing import Any

from michael_cua_sidecar.actuator import (
    capture_window_state,
    click_signin,
    find_window_by_keywords,
    focus_app_window,
    inspect_quota,
    inspect_ui_selectors,
    list_available_models,
    select_chrome_account,
    select_default_model,
    signout_from_settings,
)
from michael_cua_sidecar.config import ensure_wayland_env, get_session_name, get_target_app
from michael_cua_sidecar.errors import (
    ERR_DRIVER_UNAVAILABLE,
    ERR_REQUEST_CANCELLED,
    SidecarRpcError,
)
from michael_cua_sidecar.observer import analyze_window_state

ensure_wayland_env()

logger = logging.getLogger("michael_cua_sidecar")

_driver: Any = None
_driver_error: str | None = None


def set_driver_for_testing(mock_driver: Any) -> None:
    """Inject a mock CuaDriver instance for deterministic unit & integration tests."""
    global _driver, _driver_error
    _driver = mock_driver
    _driver_error = None


async def get_driver() -> Any:
    """Lazy-initialize the native CuaDriver singleton."""
    global _driver, _driver_error
    if _driver is not None:
        return _driver

    try:
        from cua_driver import CuaDriver  # type: ignore[import-untyped]

        created = CuaDriver.create()
        if asyncio.iscoroutine(created):
            created = await created
        _driver = created
        _driver_error = None
        logger.info("Khởi tạo CuaDriver thành công (session=%s)", get_session_name())
        return _driver
    except ImportError as e:
        _driver_error = f"Gói Python 'cua-driver' chưa được cài đặt ({e})"
        raise SidecarRpcError(ERR_DRIVER_UNAVAILABLE, _driver_error) from e
    except Exception as e:
        _driver_error = f"Khởi tạo CuaDriver thất bại: {e}"
        raise SidecarRpcError(ERR_DRIVER_UNAVAILABLE, _driver_error) from e


async def _handle_observe(params: dict[str, Any]) -> dict[str, Any]:
    driver = await get_driver()
    keywords = params.get("targetKeywords") or [get_target_app()]
    handle = await find_window_by_keywords(driver, keywords)
    if handle is None:
        return {
            "windowDetected": False,
            "appAuthState": "unknown",
            "sourceTier": "cua_driver",
            "confidence": 0.0,
            "targets": {},
        }
    win_state = await capture_window_state(driver, handle, include_screenshot=False)
    return analyze_window_state(win_state, keywords)


async def _handle_inspect_ui(params: dict[str, Any]) -> dict[str, Any]:
    driver = await get_driver()
    keywords = params.get("targetKeywords") or [get_target_app()]
    return await inspect_ui_selectors(driver, target_keywords=keywords)


async def _handle_inspect_quota(params: dict[str, Any]) -> dict[str, Any]:
    driver = await get_driver()
    keywords = params.get("targetKeywords") or [get_target_app()]
    require_ev = bool(params.get("requireEvidence", True))
    dry_run = bool(params.get("dryRun", False))
    return await inspect_quota(
        driver,
        target_keywords=keywords,
        require_evidence=require_ev,
        dry_run=dry_run,
    )


async def _handle_focus_window(params: dict[str, Any]) -> dict[str, Any]:
    driver = await get_driver()
    app_target = (params.get("appTarget") or get_target_app()).strip()
    dry_run = bool(params.get("dryRun", False))
    return await focus_app_window(driver, app_target=app_target, dry_run=dry_run)


async def _handle_click_signout(params: dict[str, Any]) -> dict[str, Any]:
    driver = await get_driver()
    require_ev = bool(params.get("requireEvidence", True))
    dry_run = bool(params.get("dryRun", False))
    return await signout_from_settings(driver, require_evidence=require_ev, dry_run=dry_run)


async def _handle_click_signin(params: dict[str, Any]) -> dict[str, Any]:
    driver = await get_driver()
    require_ev = bool(params.get("requireEvidence", True))
    dry_run = bool(params.get("dryRun", False))
    return await click_signin(driver, require_evidence=require_ev, dry_run=dry_run)


async def _handle_select_chrome_account(params: dict[str, Any]) -> dict[str, Any]:
    driver = await get_driver()
    email_hint = (params.get("emailHint") or "").strip()
    profile_dir = (params.get("browserProfileDir") or "").strip()
    expected_prof_name = (params.get("expectedProfileName") or "").strip()
    settle_ms = int(params.get("settleTimeoutMs") or 12000)
    require_ev = bool(params.get("requireEvidence", True))
    dry_run = bool(params.get("dryRun", False))
    return await select_chrome_account(
        driver,
        email_hint=email_hint,
        browser_profile_dir=profile_dir,
        expected_profile_name=expected_prof_name,
        settle_timeout_ms=settle_ms,
        require_evidence=require_ev,
        dry_run=dry_run,
    )


async def _handle_select_model(params: dict[str, Any]) -> dict[str, Any]:
    driver = await get_driver()
    model = (params.get("model") or params.get("modelName") or "").strip()
    settle_ms = int(params.get("settleTimeoutMs") or 10000)
    require_ev = bool(params.get("requireEvidence", True))
    dry_run = bool(params.get("dryRun", False))
    return await select_default_model(
        driver,
        model_name=model,
        settle_timeout_ms=settle_ms,
        require_evidence=require_ev,
        dry_run=dry_run,
    )


async def _handle_list_models(params: dict[str, Any]) -> dict[str, Any]:
    driver = await get_driver()
    keywords = params.get("targetKeywords") or [get_target_app()]
    return await list_available_models(driver, target_keywords=keywords)


async def _handle_health_check(params: dict[str, Any]) -> dict[str, Any]:
    _ = params
    driver = await get_driver()
    if hasattr(driver, "list_windows"):
        from michael_cua_sidecar.actuator import _import_cua_symbol, _safe_construct

        lw_cls = _import_cua_symbol("ListWindowsInput")
        inp = _safe_construct(lw_cls, pid=None, on_screen_only=False) if lw_cls else None
        if inp is not None:
            await driver.list_windows(inp)
        else:
            await driver.list_windows()

    return {
        "healthy": True,
        "sessionName": get_session_name(),
        "targetApp": get_target_app(),
        "waylandEnabled": os.environ.get("CUA_DRIVER_RS_ENABLE_WAYLAND", "0") == "1",
        "agentWorkspaceEnabled": os.environ.get("CUA_AGENT_WORKSPACE_ENABLED", "1") == "1",
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }


async def _handle_shutdown(params: dict[str, Any]) -> dict[str, Any]:
    _ = params
    global _driver
    if _driver is not None and hasattr(_driver, "shutdown"):
        try:
            res = _driver.shutdown()
            if asyncio.iscoroutine(res):
                await res
        except Exception as e:
            logger.warning("Lỗi khi đóng CuaDriver: %s", e)
        _driver = None

    asyncio.get_running_loop().call_later(0.05, lambda: os._exit(0))
    return {"shutdown": True}


_METHODS = {
    "observe": _handle_observe,
    "inspect_ui": _handle_inspect_ui,
    "inspect_quota": _handle_inspect_quota,
    "focus_window": _handle_focus_window,
    "click_signout": _handle_click_signout,
    "click_signin": _handle_click_signin,
    "select_chrome_account": _handle_select_chrome_account,
    "select_account": _handle_select_chrome_account,
    "select_model": _handle_select_model,
    "list_models": _handle_list_models,
    "health_check": _handle_health_check,
    "shutdown": _handle_shutdown,
}


class MethodNotFoundError(Exception):
    pass


async def dispatch_rpc(method: str, params: dict[str, Any] | None) -> Any:
    handler = _METHODS.get(method)
    if handler is None:
        raise MethodNotFoundError(f"Phương thức JSON-RPC không tồn tại: {method}")
    return await handler(params or {})


class AsyncJsonRpcMux:
    """Multiplexes concurrent JSON-RPC 2.0 requests and processes instant `$/cancelRequest` cancellations."""

    def __init__(self, write_line_fn: Any) -> None:
        self._write_line = write_line_fn
        self._write_lock = threading.Lock()
        self.inflight: dict[Any, asyncio.Task[None]] = {}
        self.shutdown_event = asyncio.Event()

    def emit(self, raw_json: str) -> None:
        with self._write_lock:
            self._write_line(raw_json)

    async def process_line(self, raw_line: str) -> None:
        stripped = raw_line.strip()
        if not stripped:
            return

        req_id: Any = None
        try:
            req = json.loads(stripped)
            req_id = req.get("id")
            method = (req.get("method") or "").strip()
            params = req.get("params") or {}
        except json.JSONDecodeError as e:
            self.emit(_encode_resp(req_id, error={"code": -32700, "message": f"JSON không hợp lệ: {e}"}))
            return

        if not method:
            self.emit(_encode_resp(req_id, error={"code": -32600, "message": "Thiếu tên method JSON-RPC"}))
            return

        if method in ("$/cancelRequest", "cancel"):
            target_id = params.get("id")
            cancelled = self.cancel_request(target_id)
            if req_id is not None:
                self.emit(_encode_resp(req_id, result={"cancelled": cancelled, "targetId": target_id}))
            return

        task = asyncio.create_task(self._execute_rpc(req_id, method, params))
        if req_id is not None:
            self.inflight[req_id] = task

    def cancel_request(self, target_id: Any) -> bool:
        task = self.inflight.get(target_id)
        if task is not None and not task.done():
            task.cancel()
            logger.info("Đã hủy tức thời tác vụ CUA đang chạy (id=%s)", target_id)
            return True
        return False

    def cancel_all(self) -> None:
        for task in list(self.inflight.values()):
            if not task.done():
                task.cancel()

    async def _execute_rpc(self, req_id: Any, method: str, params: dict[str, Any]) -> None:
        try:
            if method == "shutdown":
                self.cancel_all()
            result = await dispatch_rpc(method, params)
            if req_id is not None:
                self.emit(_encode_resp(req_id, result=result))
            if method == "shutdown":
                self.shutdown_event.set()
        except asyncio.CancelledError:
            if req_id is not None:
                self.emit(
                    _encode_resp(
                        req_id,
                        error={
                            "code": ERR_REQUEST_CANCELLED,
                            "message": "Đã hủy tác vụ CUA tức thời do tín hiệu ngắt từ người dùng",
                        },
                    )
                )
        except SidecarRpcError as e:
            if req_id is not None:
                self.emit(_encode_resp(req_id, error={"code": e.code, "message": e.message, "data": e.data}))
        except MethodNotFoundError as e:
            if req_id is not None:
                self.emit(_encode_resp(req_id, error={"code": -32601, "message": str(e)}))
        except Exception as e:
            logger.error("Lỗi nội bộ sidecar: %s", e, exc_info=True)
            if req_id is not None:
                self.emit(_encode_resp(req_id, error={"code": -32603, "message": f"Lỗi nội bộ sidecar: {e}"}))
        finally:
            if req_id is not None:
                self.inflight.pop(req_id, None)


def handle_jsonrpc_line(loop: asyncio.AbstractEventLoop, raw_line: str) -> str:
    """Synchronous helper preserved for single-call deterministic unit tests."""
    req_id: Any = None
    try:
        req = json.loads(raw_line)
        req_id = req.get("id")
        method = (req.get("method") or "").strip()
        params = req.get("params") or {}
        if not method:
            return _encode_resp(req_id, error={"code": -32600, "message": "Thiếu tên method JSON-RPC"})

        result = loop.run_until_complete(dispatch_rpc(method, params))
        return _encode_resp(req_id, result=result)

    except SidecarRpcError as e:
        return _encode_resp(req_id, error={"code": e.code, "message": e.message, "data": e.data})
    except MethodNotFoundError as e:
        return _encode_resp(req_id, error={"code": -32601, "message": str(e)})
    except json.JSONDecodeError as e:
        return _encode_resp(req_id, error={"code": -32700, "message": f"JSON không hợp lệ: {e}"})
    except SystemExit:
        raise
    except Exception as e:
        logger.error("Lỗi nội bộ sidecar: %s", e, exc_info=True)
        return _encode_resp(req_id, error={"code": -32603, "message": f"Lỗi nội bộ sidecar: {e}"})


def _encode_resp(req_id: Any, result: Any = None, error: dict[str, Any] | None = None) -> str:
    payload: dict[str, Any] = {"jsonrpc": "2.0", "id": req_id}
    if error is not None:
        payload["error"] = error
    else:
        payload["result"] = result
    return json.dumps(payload, ensure_ascii=False, default=str)


def _setup_logging() -> None:
    handler = logging.StreamHandler(sys.stderr)
    formatter = logging.Formatter(
        fmt='{"ts":"%(asctime)s","level":"%(levelname)s","msg":"%(message)s"}',
        datefmt="%Y-%m-%dT%H:%M:%SZ",
    )
    handler.setFormatter(formatter)
    logger.handlers.clear()
    logger.addHandler(handler)
    logger.setLevel(logging.DEBUG if os.environ.get("CUA_DEBUG") else logging.INFO)


async def _serve_stdio_async() -> None:
    loop = asyncio.get_running_loop()
    in_queue: asyncio.Queue[str | None] = asyncio.Queue()

    def _write_stdout(raw: str) -> None:
        sys.stdout.write(raw + "\n")
        sys.stdout.flush()

    mux = AsyncJsonRpcMux(_write_stdout)

    def _stdin_reader_thread() -> None:
        try:
            for line in sys.stdin:
                loop.call_soon_threadsafe(in_queue.put_nowait, line)
        except Exception:
            pass
        finally:
            loop.call_soon_threadsafe(in_queue.put_nowait, None)

    reader_thread = threading.Thread(target=_stdin_reader_thread, name="cua-stdin-reader", daemon=True)
    reader_thread.start()

    while not mux.shutdown_event.is_set():
        line = await in_queue.get()
        if line is None:
            break
        await mux.process_line(line)

    mux.cancel_all()
    if mux.inflight:
        await asyncio.gather(*mux.inflight.values(), return_exceptions=True)


def main() -> None:
    _setup_logging()
    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)

    def _on_sig(signum: int, _frame: Any) -> None:
        logger.info("Nhận tín hiệu %d, dừng CUA sidecar", signum)
        sys.exit(0)

    signal.signal(signal.SIGTERM, _on_sig)
    signal.signal(signal.SIGINT, _on_sig)

    try:
        loop.run_until_complete(_serve_stdio_async())
    except SystemExit:
        pass
    except Exception:
        traceback.print_exc(file=sys.stderr)
    finally:
        if _driver is not None and hasattr(_driver, "shutdown"):
            try:
                res = _driver.shutdown()
                if asyncio.iscoroutine(res):
                    loop.run_until_complete(res)
            except Exception:
                pass
        loop.close()


if __name__ == "__main__":
    main()

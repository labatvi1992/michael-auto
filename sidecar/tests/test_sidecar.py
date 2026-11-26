"""Comprehensive unit tests for michael_cua_sidecar v2.2:
- Verify -> Evidence -> Execute across all 5 gates
- Zero-viewport-steal inspect_quota on Agent Workspace
- Multi-window Chrome Profile matcher (skipping User's Personal window to pick Work/Backup window)
- Phishing Omnibox immediate rejection (-32006) & MFA HardStop (-32004)
- Editor bounding-box privacy redaction metadata & stale OAuth tab cleanup
"""

from __future__ import annotations

import asyncio
import base64
import json
import os
import sys
import unittest
from dataclasses import dataclass, field
from typing import Any

_SIDECAR_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if _SIDECAR_ROOT not in sys.path:
    sys.path.insert(0, _SIDECAR_ROOT)

from michael_cua_sidecar.server import AsyncJsonRpcMux, handle_jsonrpc_line, set_driver_for_testing  # noqa: E402

MINIMAL_PNG_BYTES = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
)


@dataclass
class FakeElement:
    role: str
    label: str = ""
    text: str = ""
    description: str = ""
    value: str = ""
    editable: bool = False
    element_token: str = ""
    x: int = 10
    y: int = 10
    width: int = 80
    height: int = 28
    children: list["FakeElement"] = field(default_factory=list)


@dataclass
class FakeWindow:
    pid: int
    window_id: str
    title: str
    app_name: str = ""
    focused: bool = True
    screenshot: bytes | None = None
    degraded: bool = False
    elements: list[FakeElement] = field(default_factory=list)


@dataclass
class FakeWindowsResponse:
    windows: list[FakeWindow]


class FakeCuaDriver:
    """Simulates Antigravity + multi-window Google Chrome with Screenshot Evidence & Omnibox URL."""

    def __init__(self) -> None:
        self.focused_windows: list[str] = []
        self.clicked_tokens: list[str] = []
        self.pressed_keys: list[str] = []
        self.quota_popover_open = False
        self.settings_open = False
        self.logged_out = False
        self.chrome_open = False
        self.extra_personal_chrome_open = False
        self.stale_oauth_tab_open = False
        self.chrome_mfa_gate = False
        self.simulate_delayed_multi_stage_oauth = False
        self._oauth_stage = 0
        self.hollow_antigravity = False
        self.chrome_omnibox_url = "https://accounts.google.com/o/oauth2/v2/auth"
        self.chrome_profile_badge = "Workspace Dự phòng"
        self.return_screenshots = True
        self.active_model = "Gemini 2.5 Pro"
        self.model_dropdown_open = False

    async def list_windows(self, _inp: Any = None) -> FakeWindowsResponse:
        wins = [self._build_antigravity_window(False)]
        if self.extra_personal_chrome_open:
            wins.append(self._build_personal_chrome_window(False))
        if self.chrome_open:
            wins.append(self._build_chrome_window(False))
        if self.stale_oauth_tab_open:
            wins.append(
                FakeWindow(
                    pid=2099,
                    window_id="win-chrome-done",
                    title="Authentication complete - You may close this tab - Google Chrome",
                    app_name="Google Chrome",
                )
            )
        return FakeWindowsResponse(windows=wins)

    async def focus_window(self, pid: int = 0, window_id: str = "", *_args: Any, **_kwargs: Any) -> None:
        self.focused_windows.append(f"{pid}:{window_id}")

    async def get_window_state(
        self,
        pid: int = 0,
        window_id: str = "",
        include_screenshot: bool = False,
        *_args: Any,
        **_kwargs: Any,
    ) -> FakeWindow:
        if window_id == "win-chrome-personal":
            return self._build_personal_chrome_window(include_screenshot)
        if window_id == "win-chrome":
            return self._build_chrome_window(include_screenshot)
        return self._build_antigravity_window(include_screenshot)

    async def click(self, action: dict[str, Any]) -> None:
        tok = action["element_token"]
        self.clicked_tokens.append(tok)
        if tok == "tok:view_quota":
            self.quota_popover_open = True
        elif tok == "tok:settings":
            self.settings_open = True
        elif tok == "tok:signout":
            self.logged_out = True
            self.settings_open = False
        elif tok == "tok:signin":
            self.chrome_open = True
        elif tok == "tok:chrome_acc_backup":
            if self.simulate_delayed_multi_stage_oauth:
                self._oauth_stage = 1
            else:
                self.logged_out = False
                self.chrome_open = False
                self.stale_oauth_tab_open = True
        elif tok == "tok:oauth_continue":
            self._oauth_stage = 2
        elif tok == "tok:xdg_open_antigravity":
            self._oauth_stage = 3
            self.logged_out = False
            self.chrome_open = False
            self.stale_oauth_tab_open = True
        elif tok == "tok:model_selector":
            self.model_dropdown_open = True
        elif tok == "tok:model_claude_opus":
            self.active_model = "Claude Opus 4.6"
            self.model_dropdown_open = False

    async def press_key(self, action: dict[str, Any]) -> None:
        self.pressed_keys.append(f"{action.get('window_id')}:{action['key']}")
        if action["key"] == "Escape":
            self.quota_popover_open = False
        if action["key"] == "ctrl+w" and action.get("window_id") == "win-chrome-done":
            self.stale_oauth_tab_open = False

    def _png_if_requested(self, include_screenshot: bool) -> bytes | None:
        if include_screenshot and self.return_screenshots:
            return MINIMAL_PNG_BYTES
        return None

    def _build_antigravity_window(self, include_screenshot: bool) -> FakeWindow:
        png = self._png_if_requested(include_screenshot)
        if self.hollow_antigravity:
            return FakeWindow(
                pid=1001,
                window_id="win-ag",
                title="Antigravity — Hollow Shell",
                app_name="Antigravity",
                screenshot=png,
                elements=[],
            )
        if self.logged_out:
            return FakeWindow(
                pid=1001,
                window_id="win-ag",
                title="Antigravity",
                app_name="Antigravity",
                screenshot=png,
                elements=[FakeElement(role="push button", label="Sign In", element_token="tok:signin")],
            )

        els: list[FakeElement] = [
            FakeElement(role="label", text="primary@example.com"),
            FakeElement(role="push button", label="View Quota", element_token="tok:view_quota"),
            FakeElement(role="push button", label="Settings", element_token="tok:settings"),
            FakeElement(role="combo box", label=self.active_model, element_token="tok:model_selector"),
            # Large code editor pane that must be reported in redactRects for privacy masking!
            FakeElement(
                role="document frame",
                label="secret_project.go",
                editable=True,
                x=40,
                y=80,
                width=800,
                height=500,
            ),
        ]
        if self.quota_popover_open:
            els.append(FakeElement(role="dialog", label="Plan Quota: 0% remaining — Quota exhausted"))
        if self.settings_open:
            els.append(FakeElement(role="push button", label="Sign Out", element_token="tok:signout"))
        if self.model_dropdown_open:
            els.append(
                FakeElement(
                    role="menu item",
                    label="Claude Opus 4.6",
                    element_token="tok:model_claude_opus",
                )
            )
        return FakeWindow(
            pid=1001,
            window_id="win-ag",
            title="Antigravity — Workspace",
            app_name="Antigravity",
            screenshot=png,
            elements=els,
        )

    def _build_personal_chrome_window(self, include_screenshot: bool) -> FakeWindow:
        return FakeWindow(
            pid=2001,
            window_id="win-chrome-personal",
            title="Choose an account - Sign in - Google Accounts - Personal - Google Chrome",
            app_name="Google Chrome",
            screenshot=self._png_if_requested(include_screenshot),
            elements=[
                FakeElement(
                    role="entry",
                    label="Address and search bar",
                    value="https://accounts.google.com/AccountChooser",
                ),
                FakeElement(
                    role="list item",
                    label="Personal Account personal@example.com",
                    element_token="tok:chrome_personal",
                ),
            ],
        )

    def _build_chrome_window(self, include_screenshot: bool) -> FakeWindow:
        png = self._png_if_requested(include_screenshot)
        els: list[FakeElement] = [
            FakeElement(
                role="entry",
                label="Address and search bar",
                value=self.chrome_omnibox_url,
            )
        ]
        if self.chrome_mfa_gate:
            els.append(FakeElement(role="dialog", text="2-Step Verification — Enter authenticator code"))
        if self._oauth_stage == 1:
            els.append(FakeElement(role="push button", label="Continue", element_token="tok:oauth_continue"))
        elif self._oauth_stage == 2:
            els.append(
                FakeElement(
                    role="push button",
                    label="Open xdg-open",
                    element_token="tok:xdg_open_antigravity",
                )
            )
        else:
            els.extend(
                [
                    FakeElement(
                        role="list item",
                        label="Backup Account backup@example.com",
                        element_token="tok:chrome_acc_backup",
                    ),
                    FakeElement(
                        role="list item",
                        label="Use another account",
                        element_token="tok:chrome_other",
                    ),
                ]
            )
        return FakeWindow(
            pid=2002,
            window_id="win-chrome",
            title=f"Choose an account - Sign in - Google Accounts - {self.chrome_profile_badge} - Google Chrome",
            app_name="Google Chrome",
            screenshot=png,
            elements=els,
        )


class SidecarWorkflowTest(unittest.TestCase):
    def setUp(self) -> None:
        os.environ["CUA_AGENT_WORKSPACE_ENABLED"] = "1"
        os.environ["CUA_MASK_EDITOR_EVIDENCE"] = "1"
        self.loop = asyncio.new_event_loop()
        asyncio.set_event_loop(self.loop)
        self.fake_driver = FakeCuaDriver()
        set_driver_for_testing(self.fake_driver)

    def tearDown(self) -> None:
        set_driver_for_testing(None)
        self.loop.close()

    def _rpc(self, method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
        raw = handle_jsonrpc_line(
            self.loop,
            json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}}),
        )
        return json.loads(raw)

    def test_full_five_step_workflow_multi_chrome_no_viewport_steal_and_tab_cleanup(self) -> None:
        # Simulate User having their Personal Chrome window open on Workspace #1 the whole time!
        self.fake_driver.extra_personal_chrome_open = True

        # Step 1 & 2: Inspect Quota -> must NOT call focus_window when Agent Workspace is enabled!
        r1 = self._rpc("inspect_quota", {"targetKeywords": ["Antigravity"], "requireEvidence": True})
        self.assertNotIn("error", r1)
        self.assertEqual(self.fake_driver.focused_windows, [], "inspect_quota must not steal focus on Agent Workspace")
        self.assertTrue(r1["result"]["quotaExhausted"])
        self.assertEqual(r1["result"]["remainingQuotaPct"], 0)
        self.assertEqual(r1["result"]["evidence"]["step"], "01_verify_quota_zero")
        # Verify editor privacy redaction bounding box was extracted!
        self.assertEqual(len(r1["result"]["evidence"]["redactRects"]), 1)
        self.assertEqual(r1["result"]["evidence"]["redactRects"][0]["width"], 800)

        # Step 3: Navigate Settings -> Verify & capture Evidence #2 -> Sign Out!
        r2 = self._rpc("click_signout", {"requireEvidence": True})
        self.assertNotIn("error", r2)
        self.assertEqual(r2["result"]["evidence"]["step"], "02_pre_signout_settings")
        self.assertTrue(self.fake_driver.logged_out)

        # Step 4a: Verify LoggedOut & capture Evidence #3 -> Click Sign In!
        r3 = self._rpc("click_signin", {"requireEvidence": True})
        self.assertNotIn("error", r3)

        # Step 4b: Both Personal Chrome AND Backup Chrome are open -> must skip Personal and pick Workspace Dự phòng!
        r4 = self._rpc(
            "select_chrome_account",
            {
                "emailHint": "backup@example.com",
                "browserProfileDir": "Profile 1",
                "expectedProfileName": "Workspace Dự phòng",
                "settleTimeoutMs": 2000,
                "requireEvidence": True,
            },
        )
        self.assertNotIn("error", r4)
        self.assertIn("accounts.google.com", r4["result"]["evidence"]["omniboxOrigin"])
        self.assertIn("Workspace Dự phòng", r4["result"]["evidence"]["browserProfile"])
        self.assertTrue(self.fake_driver.stale_oauth_tab_open)

        # Step 5: Verify Antigravity + Evidence #5 -> Select Default Model -> Auto-close stale OAuth tab!
        r5 = self._rpc("select_model", {"model": "claude-opus-4-6", "requireEvidence": True})
        self.assertNotIn("error", r5)
        self.assertEqual(self.fake_driver.active_model, "Claude Opus 4.6")
        self.assertEqual(r5["result"]["closedOAuthTabs"], 1)
        self.assertFalse(self.fake_driver.stale_oauth_tab_open)

    def test_rejects_phishing_omnibox_and_wrong_chrome_profile_and_missing_evidence(self) -> None:
        self.fake_driver.chrome_open = True
        self.fake_driver.chrome_omnibox_url = "https://phishing-login.evil.example/accounts.google.com"

        r_phish = self._rpc(
            "select_chrome_account",
            {"emailHint": "backup@example.com", "settleTimeoutMs": 1500, "requireEvidence": True},
        )
        self.assertIn("error", r_phish)
        self.assertEqual(r_phish["error"]["code"], -32006)
        self.assertEqual(r_phish["error"]["data"]["reason"], "omnibox_origin_violation")

        # Restore valid Google domain, but ONLY wrong Chrome Profile ("Personal" vs "Work") is open.
        self.fake_driver.chrome_omnibox_url = "https://accounts.google.com/AccountChooser"
        self.fake_driver.chrome_profile_badge = "Personal"
        r_wrong_prof = self._rpc(
            "select_chrome_account",
            {
                "emailHint": "backup@example.com",
                "browserProfileDir": "Profile 2",
                "expectedProfileName": "Work",
                "settleTimeoutMs": 1500,
            },
        )
        self.assertIn("error", r_wrong_prof)
        self.assertEqual(r_wrong_prof["error"]["code"], -32006)
        self.assertEqual(r_wrong_prof["error"]["data"]["reason"], "profile_mismatch")

        # Missing screenshot when requireEvidence=True must abort before clicking (-32007)!
        self.fake_driver.chrome_profile_badge = "Work"
        self.fake_driver.return_screenshots = False
        r_no_ev = self._rpc(
            "select_chrome_account",
            {
                "emailHint": "backup@example.com",
                "browserProfileDir": "Profile 2",
                "expectedProfileName": "Work",
                "settleTimeoutMs": 1500,
                "requireEvidence": True,
            },
        )
        self.assertIn("error", r_no_ev)
        self.assertEqual(r_no_ev["error"]["code"], -32007)

    def test_chrome_mfa_challenge_returns_hardstop_error_code(self) -> None:
        self.fake_driver.chrome_open = True
        self.fake_driver.chrome_mfa_gate = True

        resp = self._rpc("select_chrome_account", {"emailHint": "backup@example.com", "settleTimeoutMs": 1500})
        self.assertIn("error", resp)
        self.assertEqual(resp["error"]["code"], -32004)

    def test_async_mux_cancel_request_aborts_inflight_coroutine_with_zero_ghost_clicks(self) -> None:
        # Chrome is NOT open yet -> select_chrome_account will poll/await for up to 5s.
        self.fake_driver.chrome_open = False
        emitted: list[dict[str, Any]] = []

        async def _run_cancel_scenario() -> None:
            mux = AsyncJsonRpcMux(lambda line: emitted.append(json.loads(line)))
            await mux.process_line(
                json.dumps(
                    {
                        "jsonrpc": "2.0",
                        "id": 42,
                        "method": "select_chrome_account",
                        "params": {"emailHint": "backup@example.com", "settleTimeoutMs": 5000},
                    }
                )
            )
            await asyncio.sleep(0.08)
            # Human touches mouse -> Go sends $/cancelRequest!
            await mux.process_line(
                json.dumps({"jsonrpc": "2.0", "method": "$/cancelRequest", "params": {"id": 42}})
            )
            # Open Chrome right after cancel -> cancelled task MUST NOT perform a ghost click!
            self.fake_driver.chrome_open = True
            await asyncio.sleep(0.15)

        self.loop.run_until_complete(_run_cancel_scenario())
        self.assertEqual(self.fake_driver.clicked_tokens, [], "Cancelled RPC must never emit ghost clicks!")
        self.assertEqual(len(emitted), 1)
        self.assertEqual(emitted[0]["id"], 42)
        self.assertEqual(emitted[0]["error"]["code"], -32800)

    def test_multi_stage_oauth_continue_and_xdg_open_plus_inspect_ui_hollow_detection(self) -> None:
        # 1. Verify multi-stage OAuth convergence: Account -> Continue -> Open xdg-open -> Antigravity Authenticated!
        self.fake_driver.logged_out = True
        self.fake_driver.chrome_open = True
        self.fake_driver.simulate_delayed_multi_stage_oauth = True

        r_oauth = self._rpc(
            "select_chrome_account",
            {
                "emailHint": "backup@example.com",
                "browserProfileDir": "Profile 1",
                "expectedProfileName": "Workspace Dự phòng",
                "settleTimeoutMs": 4000,
                "requireEvidence": True,
            },
        )
        self.assertNotIn("error", r_oauth)
        self.assertEqual(r_oauth["result"]["postOAuthStepsClicked"], 2)
        self.assertFalse(self.fake_driver.logged_out)

        # 2. Verify read-only --inspect-ui reports matched selectors AND detects hollow Electron trees!
        r_ui = self._rpc("inspect_ui", {"targetKeywords": ["Antigravity"]})
        self.assertNotIn("error", r_ui)
        ag_win = r_ui["result"]["antigravityWindows"][0]
        self.assertFalse(ag_win["hollowWindow"])
        self.assertIn("quota_button", ag_win["matchedSelectors"])

        self.fake_driver.hollow_antigravity = True
        r_hollow = self._rpc("inspect_ui", {"targetKeywords": ["Antigravity"]})
        self.assertTrue(r_hollow["result"]["antigravityWindows"][0]["hollowWindow"])

    def test_uniffi_058_strict_contract_flat_elements_action_escalation_and_oauth_pkce_relay(self) -> None:
        import types

        fake_mod = types.ModuleType("cua_driver")

        class StrictListWindowsInput:
            # Exact UniFFI 0.58.1 signature: NO default=None!
            def __init__(self, *, pid: Any, on_screen_only: Any) -> None:
                self.pid = pid
                self.on_screen_only = on_screen_only

        class StrictElementFrame:
            def __init__(self, *, x: float, y: float, w: float, h: float) -> None:
                self.x = x
                self.y = y
                self.w = w
                self.h = h

        class StrictWindowElement:
            def __init__(
                self,
                *,
                element_index: int,
                role: str,
                depth: int,
                element_token: Any,
                label: Any,
                value: Any,
                value_description: Any,
                enabled: Any,
                selected: Any,
                is_password: Any,
                in_web_content: Any,
                actions: Any,
                custom_actions: Any,
                parent_index: Any,
                frame: Any,
                min: Any,
                max: Any,
            ) -> None:
                self.element_index = element_index
                self.role = role
                self.depth = depth
                self.element_token = element_token
                self.label = label
                self.value = value
                self.value_description = value_description
                self.enabled = enabled
                self.selected = selected
                self.is_password = is_password
                self.in_web_content = in_web_content
                self.actions = actions
                self.custom_actions = custom_actions
                self.parent_index = parent_index
                self.frame = frame
                self.min = min
                self.max = max

        class StrictSnapshotImage:
            def __init__(self, *, mime_type: str, data_base64: str) -> None:
                self.mime_type = mime_type
                self.data_base64 = data_base64

        class StrictGetWindowStateInput:
            def __init__(
                self,
                *,
                pid: int,
                window_id: int,
                session: Any,
                query: Any,
                include_accessibility_tree: Any,
                include_screenshot: Any,
                screenshot_out_file: Any,
                max_elements: Any,
                max_depth: Any,
                max_dimension: Any,
                max_image_dimension: Any,
                timeout_ms: Any = None,
                tree_format: Any = None,
            ) -> None:
                self.pid = pid
                self.window_id = window_id
                self.include_screenshot = include_screenshot
                self.tree_format = tree_format

        class StrictWindowStateOutput:
            def __init__(
                self,
                *,
                pid: int,
                window_id: int,
                app_name: str,
                window_title: str,
                elements: list[StrictWindowElement],
                degraded: bool,
                degraded_reason: str,
                images: list[StrictSnapshotImage],
            ) -> None:
                self.pid = pid
                self.window_id = window_id
                self.app_name = app_name
                self.window_title = window_title
                self.elements = elements
                self.degraded = degraded
                self.degraded_reason = degraded_reason
                self.images = images

        class StrictActionTarget:
            class WINDOW:
                def __init__(self, pid: int, window_id: int) -> None:
                    self.pid = pid
                    self.window_id = window_id

        class StrictClickPosition:
            class ELEMENT:
                def __init__(self, element_token: str) -> None:
                    self.element_token = element_token

            class COORDINATES:
                def __init__(self, x: float, y: float) -> None:
                    self.x = x
                    self.y = y

        class StrictInputDeliveryMode:
            BACKGROUND = "background"
            FOREGROUND = "foreground"

        class StrictClickButton:
            LEFT = "left"

        class StrictClickInput:
            def __init__(
                self,
                *,
                target: Any,
                position: Any,
                delivery_mode: Any,
                session: Any,
                button: Any,
                count: Any,
            ) -> None:
                self.target = target
                self.position = position
                self.delivery_mode = delivery_mode
                self.session = session
                self.button = button
                self.count = count

        class StrictHotkeyInput:
            def __init__(self, *, keys: list[str], target: Any, scope: Any, session: Any) -> None:
                self.keys = keys
                self.target = target
                self.scope = scope
                self.session = session

        class StrictActionError:
            def __init__(self, code: str, hint: str = "") -> None:
                self.code = code
                self.hint = hint

        class StrictActionResult:
            def __init__(self, error: Any = None) -> None:
                self.error = error
                self.effect = "none" if error else "dispatched"
                self.escalation = None

        class StrictUniffiDriver:
            def __init__(self) -> None:
                self.last_on_screen_only: Any = None
                self.delivery_attempts: list[str] = []
                self.hotkeys_sent: list[tuple[int, list[str]]] = []
                self.misrouted_personal_open = True
                self.work_direct_consent_open = False
                self.ag_logged_out = True
                self.pkce_url = (
                    "https://accounts.google.com/o/oauth2/v2/auth"
                    "?client_id=antigravity-ide&state=pkce_xyz_999&redirect_uri=http://127.0.0.1:43999/oauth"
                )

            async def list_windows(self, inp: StrictListWindowsInput) -> Any:
                self.last_on_screen_only = inp.on_screen_only
                wins: list[Any] = [
                    types.SimpleNamespace(pid=3001, window_id=11, title="Antigravity", app_name="Antigravity")
                ]
                if self.misrouted_personal_open:
                    wins.append(
                        types.SimpleNamespace(
                            pid=4001,
                            window_id=22,
                            title="Sign in - Google Accounts - Personal - Google Chrome",
                            app_name="Google Chrome",
                        )
                    )
                if self.work_direct_consent_open:
                    wins.append(
                        types.SimpleNamespace(
                            pid=4001,
                            window_id=33,
                            title="Sign in - Google Accounts - Work - Google Chrome",
                            app_name="Google Chrome",
                        )
                    )
                return types.SimpleNamespace(windows=wins)

            async def get_window_state(self, inp: StrictGetWindowStateInput) -> StrictWindowStateOutput:
                b64_png = base64.b64encode(MINIMAL_PNG_BYTES).decode("ascii")
                imgs = [StrictSnapshotImage(mime_type="image/png", data_base64=b64_png)] if inp.include_screenshot else []

                if inp.window_id == 22:
                    return StrictWindowStateOutput(
                        pid=4001,
                        window_id=22,
                        app_name="Google Chrome",
                        window_title="Sign in - Google Accounts - Personal - Google Chrome",
                        degraded=True,
                        degraded_reason="accessibility_window_identity_unproven",
                        images=imgs,
                        elements=[
                            StrictWindowElement(
                                element_index=0,
                                role="entry",
                                depth=0,
                                element_token="tok:omnibox_22",
                                label="Address and search bar",
                                value=self.pkce_url,
                                value_description=None,
                                enabled=True,
                                selected=False,
                                is_password=False,
                                in_web_content=False,
                                actions=None,
                                custom_actions=None,
                                parent_index=None,
                                frame=StrictElementFrame(x=80, y=40, w=600, h=32),
                                min=None,
                                max=None,
                            )
                        ],
                    )

                if inp.window_id == 33:
                    # Dedicated 1-Account Profile ("Work"): skips Account Chooser straight to "Continue"!
                    return StrictWindowStateOutput(
                        pid=4001,
                        window_id=33,
                        app_name="Google Chrome",
                        window_title="Sign in - Google Accounts - Work - Google Chrome",
                        degraded=True,
                        degraded_reason="accessibility_window_identity_unproven",
                        images=imgs,
                        elements=[
                            StrictWindowElement(
                                element_index=0,
                                role="entry",
                                depth=0,
                                element_token="tok:omnibox_33",
                                label="Address and search bar",
                                value=self.pkce_url,
                                value_description=None,
                                enabled=True,
                                selected=False,
                                is_password=False,
                                in_web_content=False,
                                actions=None,
                                custom_actions=None,
                                parent_index=None,
                                frame=StrictElementFrame(x=80, y=40, w=600, h=32),
                                min=None,
                                max=None,
                            ),
                            StrictWindowElement(
                                element_index=1,
                                role="push button",
                                depth=0,
                                element_token="tok:direct_continue",
                                label="Continue",
                                value=None,
                                value_description=None,
                                enabled=True,
                                selected=False,
                                is_password=False,
                                in_web_content=True,
                                actions=["click"],
                                custom_actions=None,
                                parent_index=None,
                                frame=StrictElementFrame(x=320, y=420, w=120, h=36),
                                min=None,
                                max=None,
                            ),
                        ],
                    )

                # Antigravity window: flat parent_index tree with Editor pane + child comment mentioning "Quota exhausted"!
                ag_els = [
                    StrictWindowElement(
                        element_index=0,
                        role="document web",
                        depth=0,
                        element_token=None,
                        label="editor.go",
                        value=None,
                        value_description=None,
                        enabled=True,
                        selected=False,
                        is_password=False,
                        in_web_content=True,
                        actions=["set_value"],
                        custom_actions=None,
                        parent_index=None,
                        frame=StrictElementFrame(x=30, y=90, w=720, h=440),
                        min=None,
                        max=None,
                    ),
                    StrictWindowElement(
                        element_index=1,
                        role="label",
                        depth=1,
                        element_token=None,
                        label="// Quota exhausted inside user source code must NOT false-trigger!",
                        value=None,
                        value_description=None,
                        enabled=True,
                        selected=False,
                        is_password=False,
                        in_web_content=True,
                        actions=None,
                        custom_actions=None,
                        parent_index=0,
                        frame=StrictElementFrame(x=40, y=110, w=400, h=20),
                        min=None,
                        max=None,
                    ),
                ]
                if not self.ag_logged_out:
                    ag_els.append(
                        StrictWindowElement(
                            element_index=2,
                            role="combo box",
                            depth=0,
                            element_token="tok:ag_model",
                            label="Gemini 2.5 Pro",
                            value=None,
                            value_description=None,
                            enabled=True,
                            selected=False,
                            is_password=False,
                            in_web_content=True,
                            actions=["click"],
                            custom_actions=None,
                            parent_index=None,
                            frame=StrictElementFrame(x=500, y=20, w=160, h=28),
                            min=None,
                            max=None,
                        )
                    )
                return StrictWindowStateOutput(
                    pid=3001,
                    window_id=11,
                    app_name="Antigravity",
                    window_title="Antigravity — Project",
                    degraded=False,
                    degraded_reason="",
                    images=imgs,
                    elements=ag_els,
                )

            async def click(self, inp: StrictClickInput) -> StrictActionResult:
                self.delivery_attempts.append(str(inp.delivery_mode))
                # Simulate Electron/Chrome returning soft ActionResult.error on BACKGROUND mode, requiring escalation!
                if inp.delivery_mode == "background":
                    return StrictActionResult(error=StrictActionError("background_unavailable", "escalate"))
                if getattr(inp.position, "element_token", "") == "tok:direct_continue":
                    self.ag_logged_out = False
                    self.work_direct_consent_open = False
                return StrictActionResult(error=None)

            async def hotkey(self, inp: StrictHotkeyInput) -> StrictActionResult:
                self.hotkeys_sent.append((inp.target.window_id, inp.keys))
                if inp.target.window_id == 22 and inp.keys == ["ctrl", "w"]:
                    self.misrouted_personal_open = False
                return StrictActionResult(error=None)

        fake_mod.ListWindowsInput = StrictListWindowsInput  # type: ignore[attr-defined]
        fake_mod.GetWindowStateInput = StrictGetWindowStateInput  # type: ignore[attr-defined]
        fake_mod.ActionTarget = StrictActionTarget  # type: ignore[attr-defined]
        fake_mod.ClickPosition = StrictClickPosition  # type: ignore[attr-defined]
        fake_mod.InputDeliveryMode = StrictInputDeliveryMode  # type: ignore[attr-defined]
        fake_mod.ClickButton = StrictClickButton  # type: ignore[attr-defined]
        fake_mod.ClickInput = StrictClickInput  # type: ignore[attr-defined]
        fake_mod.HotkeyInput = StrictHotkeyInput  # type: ignore[attr-defined]

        strict_driver = StrictUniffiDriver()
        prev_mod = sys.modules.get("cua_driver")
        sys.modules["cua_driver"] = fake_mod
        set_driver_for_testing(strict_driver)
        try:
            # 1. Flat parent_index editor hierarchy prevents false Quota trigger & verifies on_screen_only=False!
            r_obs = self._rpc("observe", {"targetKeywords": ["Antigravity"]})
            self.assertNotIn("error", r_obs)
            self.assertIs(strict_driver.last_on_screen_only, False)
            self.assertEqual(r_obs["result"]["windowTitle"], "Antigravity — Project")
            self.assertFalse(r_obs["result"]["quotaExhausted"])
            self.assertTrue(r_obs["result"]["isEditorContent"])

            # 2. Misrouted OAuth tab in "Personal" triggers instant Ctrl+W cleanup + lossless PKCE URL relay!
            r_relay = self._rpc(
                "select_chrome_account",
                {
                    "emailHint": "work@example.com",
                    "browserProfileDir": "Profile 1",
                    "expectedProfileName": "Work",
                    "settleTimeoutMs": 2000,
                    "requireEvidence": True,
                },
            )
            self.assertIn("error", r_relay)
            self.assertEqual(r_relay["error"]["code"], -32006)
            self.assertEqual(r_relay["error"]["data"]["reason"], "oauth_profile_relay_required")
            self.assertEqual(r_relay["error"]["data"]["relayOAuthUrl"], strict_driver.pkce_url)
            self.assertIn((22, ["ctrl", "w"]), strict_driver.hotkeys_sent)

            # 3. Once BrowserRouter opens the relayed PKCE URL in "Profile 1" ("Work"),
            #    1-Account Direct Consent + BACKGROUND->FOREGROUND escalation + SnapshotImage all succeed!
            strict_driver.work_direct_consent_open = True
            r_direct = self._rpc(
                "select_chrome_account",
                {
                    "emailHint": "work@example.com",
                    "browserProfileDir": "Profile 1",
                    "expectedProfileName": "Work",
                    "settleTimeoutMs": 2500,
                    "requireEvidence": True,
                },
            )
            self.assertNotIn("error", r_direct)
            self.assertTrue(r_direct["result"]["fastPathDirectConfirm"])
            self.assertEqual(strict_driver.delivery_attempts, ["background", "foreground"])
            self.assertFalse(strict_driver.ag_logged_out)

            # 4. Step 5 verifies ElementFrame editor redaction rect extraction on real UniFFI elements!
            r_mod = self._rpc("select_model", {"model": "gemini-2.5-pro", "requireEvidence": True})
            self.assertNotIn("error", r_mod)
            self.assertEqual(len(r_mod["result"]["evidence"]["redactRects"]), 1)
            self.assertEqual(r_mod["result"]["evidence"]["redactRects"][0]["width"], 720)
        finally:
            if prev_mod is None:
                sys.modules.pop("cua_driver", None)
            else:
                sys.modules["cua_driver"] = prev_mod
            set_driver_for_testing(self.fake_driver)

    def test_antigravity_234_two_hop_model_view_usage_group_scoped_quota_and_chat_shield(self) -> None:
        class Antigravity234Driver:
            """Reproduces live Antigravity 2.34.0 Electron DOM (`document web` root + chat tables + Model -> View Usage)."""

            def __init__(self) -> None:
                self.model_menu_open = False
                self.usage_submenu_open = False
                self.gemini_5h_pct = 87
                self.clicks: list[str] = []
                self.escapes = 0

            async def list_windows(self, _inp: Any = None) -> FakeWindowsResponse:
                return FakeWindowsResponse(
                    windows=[
                        FakeWindow(
                            pid=5034,
                            window_id="329908224",
                            title="Tự Động Hóa Cua Cho Antig...",
                            app_name="antigravity",
                        )
                    ]
                )

            async def get_window_state(
                self,
                pid: int = 0,
                window_id: str = "",
                include_screenshot: bool = False,
                *_args: Any,
                **_kwargs: Any,
            ) -> FakeWindow:
                children: list[FakeElement] = [
                    # Chat content discussing CAPTCHA / 2FA / Quota exhausted / Login in Markdown tables, paragraphs & bullets:
                    # MUST NEVER false-trigger authGateDetected, quotaExhausted, sign_in_button, or quota_button!
                    FakeElement(role="table cell", label="Dừng cứng khi gặp 2-Step Verification / CAPTCHA / nhập mật khẩu"),
                    FakeElement(
                        role="paragraph",
                        label="Khi Quota exhausted hoặc 0% left thì tự chuyển",
                        children=[
                            FakeElement(role="link", label="Sign In", element_token="tok:chat_link_signin", x=500, y=300, width=60, height=20),
                        ],
                    ),
                    FakeElement(role="block quote", label="Verify you are human"),
                    # Exact live PID 5034 Markdown bullet list items (both scrolled offscreen & onscreen):
                    FakeElement(
                        role="list item",
                        label="• Khi chạy HPA pod trên K8s, hạn mức rate-limit bị nhân nhiều lần.",
                        element_token="tok:chat_bullet_quota",
                        x=1112,
                        y=-4694,
                        width=520,
                        height=30,
                    ),
                    FakeElement(
                        role="list item",
                        label="• Thêm hành động trên card của KB khi đăng nhập bằng role admin.",
                        element_token="tok:chat_bullet_signin",
                        x=1132,
                        y=-2523,
                        width=520,
                        height=30,
                    ),
                    FakeElement(
                        role="list item",
                        label="• Xem hạn mức hoặc đăng nhập",
                        element_token="tok:chat_bullet_onscreen",
                        x=420,
                        y=380,
                        width=260,
                        height=24,
                    ),
                    # Real bottom-left Settings & bottom Model selector buttons (just like live PID 5034):
                    FakeElement(role="button", label="Settings", element_token="tok:ag234_settings", x=8, y=1008, width=240, height=32),
                    FakeElement(
                        role="button",
                        label="Select model, current: Gemini 4 Neon Extra High",
                        element_token="tok:ag234_model",
                        x=735,
                        y=989,
                        width=174,
                        height=28,
                    ),
                ]
                if self.model_menu_open:
                    children.append(
                        FakeElement(
                            role="menu",
                            label="Model",
                            children=[
                                FakeElement(role="menu item", label="Gemini 4 Neon Extra High", element_token="tok:m_neon"),
                                FakeElement(role="menu item", label="Claude Opus 4.6 (Thinking)", element_token="tok:m_opus"),
                                FakeElement(role="menu item", label="View Usage", element_token="tok:ag234_view_usage"),
                            ],
                        )
                    )
                if self.usage_submenu_open:
                    children.append(
                        FakeElement(
                            role="dialog",
                            label="Usage",
                            children=[
                                FakeElement(role="label", label="Gemini Models"),
                                FakeElement(role="label", label="Weekly limit 29% left"),
                                FakeElement(role="label", label="Resets in 5d 11h"),
                                FakeElement(role="label", label=f"5h limit {self.gemini_5h_pct}% left"),
                                FakeElement(role="label", label="Resets in 3h 16m"),
                                FakeElement(role="label", label="Claude and GPT models"),
                                FakeElement(role="label", label="Weekly limit 77% left"),
                                FakeElement(role="label", label="5h limit 100% left"),
                            ],
                        )
                    )
                return FakeWindow(
                    pid=5034,
                    window_id="329908224",
                    title="Tự Động Hóa Cua Cho Antig...",
                    app_name="antigravity",
                    screenshot=MINIMAL_PNG_BYTES if include_screenshot else None,
                    elements=[
                        # Root full-screen Electron `document web` container (1920x1048, editable=False):
                        FakeElement(
                            role="document web",
                            label="Antigravity Root",
                            editable=False,
                            x=0,
                            y=0,
                            width=1920,
                            height=1048,
                            children=children,
                        )
                    ],
                )

            async def click(self, action: dict[str, Any]) -> None:
                tok = action["element_token"]
                self.clicks.append(tok)
                if tok == "tok:ag234_model":
                    self.model_menu_open = True
                elif tok == "tok:ag234_view_usage":
                    self.usage_submenu_open = True

            async def press_key(self, action: dict[str, Any]) -> None:
                if action["key"] == "Escape":
                    self.escapes += 1
                    if self.usage_submenu_open:
                        self.usage_submenu_open = False
                    else:
                        self.model_menu_open = False

        ag_drv = Antigravity234Driver()
        set_driver_for_testing(ag_drv)
        try:
            # 1. Read-only --inspect-ui must see clean activeModel, appAuthState="authenticated", ZERO false
            #    HardStop/Quota/SignIn/QuotaButton from chat tables or Markdown bullet `list item`s!
            r_ui = self._rpc("inspect_ui", {"targetKeywords": ["Antigravity"]})
            self.assertNotIn("error", r_ui)
            win_rep = r_ui["result"]["antigravityWindows"][0]
            self.assertFalse(win_rep["authGateDetected"])
            self.assertFalse(win_rep["quotaExhausted"])
            self.assertEqual(win_rep["appAuthState"], "authenticated")
            self.assertEqual(win_rep["activeModel"], "Gemini 4 Neon Extra High")
            self.assertEqual(win_rep["quotaInspectionPath"], "model_selector -> view_usage")
            self.assertEqual(win_rep["matchedSelectors"], ["model_selector", "settings_button"])

            # 1b. Verify Chrome OAuth <h1> heading / <p> paragraph MFA prompts STILL trigger HardStop strictly!
            from michael_cua_sidecar.observer import scan_elements_for_auth_gate

            chrome_gate, chrome_reason = scan_elements_for_auth_gate(
                [FakeElement(role="heading", label="2-Step Verification")],
                ide_mode=False,
            )
            self.assertTrue(chrome_gate)
            self.assertIn("2-step verification", chrome_reason.lower())

            # 2. Healthy 2-hop check (Gemini 29% weekly / 87% 5h; Claude 77% / 100%):
            #    Must report 29% (min of Gemini Models) and NOT be overwritten by Claude's 100%!
            r_ok = self._rpc("inspect_quota", {"targetKeywords": ["Antigravity"], "requireEvidence": True})
            self.assertNotIn("error", r_ok)
            self.assertFalse(r_ok["result"]["quotaExhausted"])
            self.assertEqual(r_ok["result"]["remainingQuotaPct"], 29)
            self.assertEqual(ag_drv.clicks, ["tok:ag234_model", "tok:ag234_view_usage"])
            self.assertFalse(ag_drv.model_menu_open)
            self.assertFalse(ag_drv.usage_submenu_open)

            # 3. Exhausted Gemini 5h limit (0% left) while Claude is STILL 100% left:
            #    Must detect quotaExhausted=True (0%), capture Evidence #1 with NO full-screen 1920x1048 black mask!
            ag_drv.gemini_5h_pct = 0
            r_zero = self._rpc("inspect_quota", {"targetKeywords": ["Antigravity"], "requireEvidence": True})
            self.assertNotIn("error", r_zero)
            self.assertTrue(r_zero["result"]["quotaExhausted"])
            self.assertEqual(r_zero["result"]["remainingQuotaPct"], 0)
            self.assertIn("Gemini Models: 0% left", r_zero["result"]["quotaMessage"])
            self.assertEqual(r_zero["result"]["evidence"]["step"], "01_verify_quota_zero")
            self.assertEqual(r_zero["result"]["evidence"]["captureScope"], "window")
            self.assertNotIn("redactRects", r_zero["result"]["evidence"])

            # 4. list_models RPC opens Model menu, reads selectable models, and cleanly dismisses with Escape!
            r_models = self._rpc("list_models", {"targetKeywords": ["Antigravity"]})
            self.assertNotIn("error", r_models)
            self.assertEqual(r_models["result"]["activeModel"], "Gemini 4 Neon Extra High")
            self.assertIn("Claude Opus 4.6 (Thinking)", r_models["result"]["models"])
            self.assertFalse(ag_drv.model_menu_open)
        finally:
            set_driver_for_testing(self.fake_driver)

    def test_arm_gnome51_auto_route_invokes_v4_bridge_with_clamped_ttl(self) -> None:
        from unittest.mock import MagicMock, patch
        from michael_cua_sidecar import actuator

        calls: list[list[str]] = []

        def fake_run(cmd: list[str], **_kw: Any) -> Any:
            calls.append(cmd)
            m = MagicMock()
            m.returncode = 0
            m.stdout = "(true,)"
            return m

        with (
            patch.dict(os.environ, {"CUA_AGENT_WORKSPACE_ENABLED": "1", "CUA_AGENT_WORKSPACE_INDEX": "1"}),
            patch.object(actuator.shutil, "which", return_value="/usr/bin/gdbus"),
            patch.object(actuator, "_resolve_agent_workspace_index_by_name", return_value=2),
            patch.object(actuator.subprocess, "run", side_effect=fake_run),
        ):
            self.assertTrue(actuator._arm_gnome51_auto_route(999))
            self.assertEqual(len(calls), 1)
            self.assertIn("io.michael_auto.WorkspaceBridge.ArmAutoRoute", calls[0])
            self.assertEqual(calls[0][-2:], ["60", "2"])


if __name__ == "__main__":
    unittest.main()

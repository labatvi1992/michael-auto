"""Observer module — normalizes CuaDriver 0.58+ UniFFI WindowState snapshots into ScreenState dicts (v2.4.1).

Calibrated against live Antigravity 2.34.0 Electron AX-tree on Fedora 45 GNOME 51.1:
- Handles flat UniFFI `WindowElement` (`parent_index`, `ElementFrame(x, y, w, h)`, `value_description`, `is_password`).
- Shields Antigravity IDE chat content (`paragraph`, `table cell`, `block quote`, `list item`, `heading`) from
  false-triggering `HardStopChallenge` or `QuotaExhausted` while keeping full strict scanning on Chrome OAuth.
- Parses Antigravity 2.34.0's two-bucket `Usage` popover (`Gemini Models` vs `Claude and GPT models`,
  evaluating `min(Weekly limit, 5h limit)` for the active model's family so Claude 100% never masks Gemini 0%).
"""

from __future__ import annotations

import re
import time
from dataclasses import dataclass, field
from typing import Any

from michael_cua_sidecar.phrases import (
    ALERT_ROLES,
    AUTH_CHALLENGE_PHRASES,
    CLICKABLE_ROLES,
    CONVERSATION_CONTENT_ROLES,
    EDITOR_ROLES,
    MODEL_FAMILY_HINTS,
    MODEL_SELECTOR_PHRASES,
    QUOTA_PHRASES,
    SETTINGS_PHRASES,
    SIGNIN_PHRASES,
    SIGNOUT_PHRASES,
    SYSTEM_SURFACE_ROLES,
    VIEW_QUOTA_PHRASES,
)

_EMAIL_RE = re.compile(r"[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}")
_PCT_REMAINING_RE = re.compile(r"\b(\d{1,3})(?:\.\d+)?\s*%\s*(?:remaining|left|còn lại)\b", re.IGNORECASE)
_PCT_USED_RE = re.compile(r"\b(\d{1,3})(?:\.\d+)?\s*%\s*(?:used|đã dùng)\b", re.IGNORECASE)
_ZERO_PCT_LEFT_RE = re.compile(r"(?<!\d)0(?:\.0+)?\s*%\s*(?:remaining|left|còn lại)\b", re.IGNORECASE)
_RESETS_IN_RE = re.compile(r"resets?\s+in\s+[0-9a-zA-Z\s]{1,24}", re.IGNORECASE)
_CURRENT_MODEL_PREFIX_RE = re.compile(r"current\s*:\s*(.+)$", re.IGNORECASE)


@dataclass
class NormalizedElement:
    """Unified UI element representation bridging flat UniFFI WindowElement and nested test trees."""

    role: str
    label: str = ""
    text: str = ""
    description: str = ""
    value: str = ""
    editable: bool = False
    is_password: bool = False
    element_token: str = ""
    x: int = 0
    y: int = 0
    width: int = 0
    height: int = 0
    element_index: int | None = None
    parent_index: int | None = None
    children: list["NormalizedElement"] = field(default_factory=list)


def _extract_single_normalized_node(el: Any) -> NormalizedElement:
    if isinstance(el, NormalizedElement):
        return el

    role = (getattr(el, "role", None) or "").strip()
    label = (getattr(el, "label", None) or "").strip()
    text = (getattr(el, "text", None) or "").strip()
    desc_raw = getattr(el, "description", None)
    if not desc_raw:
        desc_raw = getattr(el, "value_description", None)
    description = (desc_raw or "").strip()
    value = (getattr(el, "value", None) or "").strip()
    token = (getattr(el, "element_token", None) or "").strip()

    is_pwd = bool(getattr(el, "is_password", False)) or role.lower() == "password text"
    actions = getattr(el, "actions", None) or []
    editable = bool(getattr(el, "editable", False)) or any(
        "set_value" in str(a).lower() or "insert" in str(a).lower() for a in actions
    )

    frame = getattr(el, "frame", None)
    if frame is not None:
        x = int(round(float(getattr(frame, "x", 0) or 0)))
        y = int(round(float(getattr(frame, "y", 0) or 0)))
        width = int(round(float(getattr(frame, "w", getattr(frame, "width", 0)) or 0)))
        height = int(round(float(getattr(frame, "h", getattr(frame, "height", 0)) or 0)))
    else:
        x = int(getattr(el, "x", 0) or 0)
        y = int(getattr(el, "y", 0) or 0)
        width = int(getattr(el, "width", 0) or 0)
        height = int(getattr(el, "height", 0) or 0)

    el_idx_raw = getattr(el, "element_index", None)
    par_idx_raw = getattr(el, "parent_index", None)
    el_idx = int(el_idx_raw) if el_idx_raw is not None else None
    par_idx = int(par_idx_raw) if par_idx_raw is not None else None

    return NormalizedElement(
        role=role,
        label=label,
        text=text,
        description=description,
        value=value,
        editable=editable,
        is_password=is_pwd,
        element_token=token,
        x=x,
        y=y,
        width=width,
        height=height,
        element_index=el_idx,
        parent_index=par_idx,
        children=[],
    )


def normalize_elements_hierarchy(elements: list[Any] | None) -> list[NormalizedElement]:
    """Convert either a flat UniFFI `WindowElement` list (linked via `parent_index`) or a nested
    element list (`.children`) into a proper hierarchical tree of `NormalizedElement` nodes."""
    if not elements:
        return []
    if all(isinstance(e, NormalizedElement) for e in elements):
        return elements  # type: ignore[return-value]

    is_flat_uniffi = any(getattr(e, "element_index", None) is not None for e in elements)
    if is_flat_uniffi:
        nodes: list[NormalizedElement] = [_extract_single_normalized_node(e) for e in elements]
        by_index: dict[int, NormalizedElement] = {}
        for idx, n in enumerate(nodes):
            key = n.element_index if n.element_index is not None else idx
            by_index[key] = n

        roots: list[NormalizedElement] = []
        for idx, n in enumerate(nodes):
            own_key = n.element_index if n.element_index is not None else idx
            p_idx = n.parent_index
            if p_idx is not None and p_idx != own_key and p_idx in by_index:
                by_index[p_idx].children.append(n)
            else:
                roots.append(n)
        return roots

    nested_roots: list[NormalizedElement] = []
    for raw in elements:
        node = _extract_single_normalized_node(raw)
        raw_children = getattr(raw, "children", None) or []
        if raw_children:
            node.children = normalize_elements_hierarchy(raw_children)
        nested_roots.append(node)
    return nested_roots


def count_element_nodes(elements: list[Any]) -> int:
    norm = normalize_elements_hierarchy(elements)
    return _count_normalized_nodes(norm)


def _count_normalized_nodes(nodes: list[NormalizedElement]) -> int:
    total = 0
    for n in nodes:
        total += 1
        if n.children:
            total += _count_normalized_nodes(n.children)
    return total


def extract_window_title(window_state: Any) -> str:
    """Extract window title across both UniFFI WindowStateOutput (.window_title) and WindowInfo (.title)."""
    if window_state is None:
        return ""
    wt = getattr(window_state, "window_title", None)
    if wt and str(wt).strip():
        return str(wt).strip()
    t = getattr(window_state, "title", None)
    if t and str(t).strip():
        return str(t).strip()
    return ""


def extract_clean_model_name(raw_label: str) -> str:
    """Extract clean model name from Antigravity 2.34+ labels like 'Select model, current: Gemini 4 Neon Extra High'."""
    s = (raw_label or "").strip()
    if not s:
        return ""
    m = _CURRENT_MODEL_PREFIX_RE.search(s)
    if m:
        return m.group(1).strip()
    return s


def _model_family_bucket(model_name: str) -> str:
    low = (model_name or "").strip().lower()
    if any(tok in low for tok in ("claude", "opus", "sonnet", "haiku", "gpt", "o3", "o4")):
        return "claude_gpt"
    return "gemini"


def is_valid_ide_control(el: NormalizedElement, cleaned_text: str, inside_system: bool = False) -> bool:
    """Validate that an Antigravity IDE node is a genuine interactive UI control rather than
    a Markdown chat bullet (`list item`), offscreen scrolled transcript item, or prose sentence."""
    role = el.role.lower()
    if el.editable or role in EDITOR_ROLES or role in CONVERSATION_CONTENT_ROLES:
        return False
    if role == "list item" and not inside_system:
        return False
    if role not in CLICKABLE_ROLES:
        return False
    s = (cleaned_text or "").strip()
    if not s or len(s) > 90:
        return False
    if s.startswith(("•", "▪", "◦", "- ", "* ")):
        return False
    if el.height > 0 and (el.y + el.height) <= 0:
        return False
    return True


def parse_usage_popover(elements: list[Any], active_model: str = "") -> dict[str, Any] | None:
    """Parse Antigravity 2.34.0's `Model -> View Usage` popover separated by model family:
    - `Gemini Models` (`Weekly limit XX% left`, `5h limit XX% left`, `Resets in ...`)
    - `Claude and GPT models` (`Weekly limit XX% left`, `5h limit XX% left`, `Resets in ...`)

    Returns None if no Usage percentages are present; otherwise returns group-scoped min remaining %
    so `Claude 100% left` never masks `Gemini 0% left` (and vice versa)!
    """
    norm = normalize_elements_hierarchy(elements)
    groups: dict[str, list[int]] = {"gemini": [], "claude_gpt": [], "general": []}
    reset_notes: dict[str, list[str]] = {"gemini": [], "claude_gpt": [], "general": []}
    current_group = "general"
    saw_usage_marker = False

    def _walk(nodes: list[NormalizedElement], inside_editor: bool, inside_chat: bool, inside_system: bool) -> None:
        nonlocal current_group, saw_usage_marker
        for n in nodes:
            role = n.role.lower()
            now_system = inside_system or role in SYSTEM_SURFACE_ROLES or (
                role == "landmark" and "notification" in n.label.lower()
            )
            is_ed = inside_editor or n.editable or role in EDITOR_ROLES
            is_chat = inside_chat or ((not now_system) and (role in CONVERSATION_CONTENT_ROLES or role == "list item"))
            if is_ed or is_chat:
                continue

            if n.height > 0 and (n.y + n.height) <= 0:
                continue

            # Ignore generic object-replacement placeholder characters (\ufffc) emitted by Chromium AX containers.
            raw_parts = [n.label, n.text, n.value, n.description]
            cleaned = " ".join(p.replace("\ufffc", " ").strip() for p in raw_parts if p).strip()
            low = cleaned.lower()

            if low and len(cleaned) <= 120 and not cleaned.startswith(("•", "▪", "◦")):
                if "gemini models" in low or ("gemini" in low and "model" in low and "%" not in low):
                    current_group = "gemini"
                    saw_usage_marker = True
                elif "claude and gpt" in low or ("claude" in low and "gpt" in low):
                    current_group = "claude_gpt"
                    saw_usage_marker = True
                elif "weekly limit" in low or "5h limit" in low or low == "usage":
                    saw_usage_marker = True

                rem_matches = _PCT_REMAINING_RE.findall(low)
                if rem_matches:
                    for m_str in rem_matches:
                        pct = max(0, min(100, int(m_str)))
                        groups[current_group].append(pct)
                else:
                    used_matches = _PCT_USED_RE.findall(low)
                    for m_str in used_matches:
                        used = max(0, min(100, int(m_str)))
                        groups[current_group].append(100 - used)

                r_match = _RESETS_IN_RE.search(cleaned)
                if r_match:
                    note = r_match.group(0).strip()
                    if note not in reset_notes[current_group]:
                        reset_notes[current_group].append(note)

            if n.children:
                _walk(n.children, is_ed, is_chat, now_system)

    _walk(norm, False, False, False)

    if not saw_usage_marker and not groups["gemini"] and not groups["claude_gpt"]:
        return None

    preferred = _model_family_bucket(active_model)
    chosen_group = preferred
    if not groups[chosen_group]:
        if groups["gemini"]:
            chosen_group = "gemini"
        elif groups["claude_gpt"]:
            chosen_group = "claude_gpt"
        elif groups["general"]:
            chosen_group = "general"
        else:
            return None

    vals = groups[chosen_group]
    min_pct = min(vals)
    group_label = (
        "Gemini Models"
        if chosen_group == "gemini"
        else ("Claude and GPT models" if chosen_group == "claude_gpt" else "Usage")
    )
    resets = ", ".join(reset_notes[chosen_group][:2])
    detail = f"{group_label}: {min_pct}% left"
    if resets:
        detail += f" ({resets})"

    return {
        "group": chosen_group,
        "groupLabel": group_label,
        "remainingQuotaPct": min_pct,
        "quotaExhausted": min_pct == 0,
        "quotaMessage": detail,
        "allGroups": {k: v for k, v in groups.items() if v},
    }


def analyze_window_state(window_state: Any, target_keywords: list[str] | None = None) -> dict[str, Any]:
    """Convert a CuaDriver WindowStateOutput into Go's domain.ScreenState JSON contract."""
    if not target_keywords:
        target_keywords = ["antigravity"]

    state: dict[str, Any] = {
        "observedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "windowDetected": False,
        "windowTitle": "",
        "windowFocused": False,
        "windowPid": int(getattr(window_state, "pid", 0) or 0) if window_state is not None else 0,
        "elementCount": 0,
        "hollowWindow": False,
        "appAuthState": "unknown",
        "activeAccountHint": "",
        "activeModel": "",
        "remainingQuotaPct": None,
        "quotaExhausted": False,
        "quotaMessage": "",
        "authGateDetected": False,
        "authGateReason": "",
        "isEditorContent": False,
        "confidence": 0.0,
        "sourceTier": "cua_driver",
        "targets": {},
    }

    if window_state is None:
        return state

    title = extract_window_title(window_state)
    focused = bool(getattr(window_state, "focused", False))
    state["windowTitle"] = title
    state["windowFocused"] = focused

    raw_elements = getattr(window_state, "elements", None) or []
    elements = normalize_elements_hierarchy(raw_elements)
    reported_total = int(
        getattr(window_state, "total_element_count", 0)
        or getattr(window_state, "element_count", 0)
        or 0
    )
    total_nodes = max(_count_normalized_nodes(elements), reported_total)
    state["elementCount"] = total_nodes

    title_lower = title.lower()
    app_name_lower = (getattr(window_state, "app_name", "") or "").lower()
    combined_meta = f"{title_lower} {app_name_lower}"

    if not any(kw.lower() in combined_meta for kw in target_keywords):
        if focused:
            gate, reason = scan_elements_for_auth_gate(elements, ide_mode=False)
            if gate:
                state["authGateDetected"] = True
                state["authGateReason"] = reason
                state["appAuthState"] = "auth_challenge"
                state["confidence"] = 1.0
        return state

    state["windowDetected"] = True
    if total_nodes == 0:
        state["hollowWindow"] = True
        state["confidence"] = 0.50
        return state

    state["confidence"] = 0.92

    for el in elements:
        _inspect_element(el, False, False, False, state)
        if state["authGateDetected"]:
            state["confidence"] = 1.0
            return state

    # Apply group-scoped Antigravity 2.34+ `Usage` popover parsing when Usage buckets are visible!
    usage_info = parse_usage_popover(elements, state.get("activeModel", ""))
    if usage_info is not None:
        state["remainingQuotaPct"] = usage_info["remainingQuotaPct"]
        state["quotaMessage"] = usage_info["quotaMessage"]
        if usage_info["quotaExhausted"]:
            state["quotaExhausted"] = True
            state["confidence"] = 0.99
        elif not any(qp in (state.get("quotaMessage", "").lower()) for qp in QUOTA_PHRASES):
            # When the active model's Usage bucket still has > 0% left, ignore a 0% in an unrelated model family.
            state["quotaExhausted"] = False

    return state


def scan_elements_for_auth_gate(elements: list[Any], ide_mode: bool = False) -> tuple[bool, str]:
    """Scan an element tree for Password/MFA/CAPTCHA.

    - `ide_mode=False` (Chrome OAuth windows): scans all non-editor nodes at maximum sensitivity.
    - `ide_mode=True` (Antigravity IDE window): ALWAYS flags `is_password` anywhere, and flags
      `AUTH_CHALLENGE_PHRASES` strictly inside system modal/alert/notification surfaces so user
      chat discussions about CAPTCHA/2FA never false-trigger HardStopChallenge!
    """
    norm = normalize_elements_hierarchy(elements)
    return _scan_normalized_for_auth_gate(norm, ide_mode=ide_mode, inside_system=False)


def _scan_normalized_for_auth_gate(
    elements: list[NormalizedElement],
    ide_mode: bool,
    inside_system: bool,
) -> tuple[bool, str]:
    for el in elements:
        role = el.role.lower()
        now_system = inside_system or role in SYSTEM_SURFACE_ROLES or (
            role == "landmark" and "notification" in el.label.lower()
        )
        combined = f"{el.label} {el.description} {el.text}".replace("\ufffc", " ").strip()
        combined_lower = combined.lower()

        if el.is_password or role == "password text":
            return True, f"Phát hiện ô nhập mật khẩu xác thực ({el.label or 'password'})"

        if ide_mode:
            # In Antigravity IDE: shield chat/editor content and scan phrases only inside system modals/alerts.
            skip_node = (not now_system) and (
                role in CONVERSATION_CONTENT_ROLES or role == "list item" or el.editable or role in EDITOR_ROLES
            )
            allow_phrase_scan = (not skip_node) and now_system
        else:
            # In Chrome OAuth windows: maximum sensitivity (including <h1> heading and <p> paragraph MFA prompts!).
            allow_phrase_scan = not el.editable

        if allow_phrase_scan and combined_lower:
            for phrase in AUTH_CHALLENGE_PHRASES:
                if phrase in combined_lower:
                    return True, f"Phát hiện cổng xác thực bảo mật / CAPTCHA: {phrase}"

        if el.children:
            ok, reason = _scan_normalized_for_auth_gate(el.children, ide_mode=ide_mode, inside_system=now_system)
            if ok:
                return True, reason

    return False, ""


def _inspect_element(
    el: NormalizedElement,
    inside_editor: bool,
    inside_chat: bool,
    inside_system: bool,
    state: dict[str, Any],
) -> None:
    role = el.role.lower()
    now_system = inside_system or role in SYSTEM_SURFACE_ROLES or (
        role == "landmark" and "notification" in el.label.lower()
    )
    combined = f"{el.label} {el.description} {el.text} {el.value}".replace("\ufffc", " ").strip()
    combined_lower = combined.lower()

    if el.is_password or role == "password text":
        state["authGateDetected"] = True
        state["authGateReason"] = f"Phát hiện ô nhập mật khẩu xác thực ({el.label or 'password'})"
        state["appAuthState"] = "auth_challenge"
        return

    is_editor_node = inside_editor or el.editable or role in EDITOR_ROLES
    is_chat_content = inside_chat or (
        (not now_system) and (role in CONVERSATION_CONTENT_ROLES or role == "list item")
    )

    if not is_editor_node and not is_chat_content and combined_lower:
        # In Antigravity IDE, only scan AUTH_CHALLENGE_PHRASES inside explicit system modals/alerts/notifications
        # so normal chat text, headings, or thought-summary buttons never false-trigger HardStopChallenge!
        if now_system:
            for phrase in AUTH_CHALLENGE_PHRASES:
                if phrase in combined_lower:
                    state["authGateDetected"] = True
                    state["authGateReason"] = f"Phát hiện cổng bảo mật / xác minh: {phrase}"
                    state["appAuthState"] = "auth_challenge"
                    return

        email_match = _EMAIL_RE.search(combined_lower)
        if email_match and not state["activeAccountHint"]:
            state["activeAccountHint"] = email_match.group(0)
            if state["appAuthState"] == "unknown":
                state["appAuthState"] = "authenticated"

        if is_valid_ide_control(el, combined, inside_system=now_system):
            target = build_target_dict(el, el.element_token)
            if _contains_any(combined_lower, SIGNOUT_PHRASES):
                state["targets"]["logout_button"] = target
                if state["appAuthState"] == "unknown":
                    state["appAuthState"] = "authenticated"
            elif _contains_any(combined_lower, SIGNIN_PHRASES):
                state["targets"]["sign_in_button"] = target
                state["appAuthState"] = "logged_out"
            elif _contains_any(combined_lower, SETTINGS_PHRASES):
                state["targets"]["settings_button"] = target
                if state["appAuthState"] == "unknown":
                    state["appAuthState"] = "authenticated"
            elif _contains_any(combined_lower, VIEW_QUOTA_PHRASES):
                state["targets"]["quota_button"] = target
            elif _contains_any(combined_lower, MODEL_SELECTOR_PHRASES):
                # Prefer explicit "select model" control over generic conversation buttons containing model names.
                is_explicit_selector = any(
                    k in combined_lower for k in ("select model", "model selection", "choose model", "chọn model")
                )
                if is_explicit_selector or "model_selector" not in state["targets"]:
                    state["targets"]["model_selector"] = target
                if (is_explicit_selector or not state["activeModel"]) and any(
                    h in combined_lower for h in MODEL_FAMILY_HINTS
                ):
                    state["activeModel"] = extract_clean_model_name(el.label or el.text or el.value)

        rem_match = _PCT_REMAINING_RE.search(combined_lower)
        if rem_match:
            pct = max(0, min(100, int(rem_match.group(1))))
            if state["remainingQuotaPct"] is None or pct < state["remainingQuotaPct"]:
                state["remainingQuotaPct"] = pct
        else:
            used_match = _PCT_USED_RE.search(combined_lower)
            if used_match:
                used = max(0, min(100, int(used_match.group(1))))
                rem = 100 - used
                if state["remainingQuotaPct"] is None or rem < state["remainingQuotaPct"]:
                    state["remainingQuotaPct"] = rem

        if role in ALERT_ROLES or now_system or role == "":
            matched_quota = _ZERO_PCT_LEFT_RE.search(combined_lower) is not None
            if not matched_quota:
                for qp in QUOTA_PHRASES:
                    if qp in combined_lower:
                        matched_quota = True
                        break
            if matched_quota:
                state["quotaExhausted"] = True
                state["remainingQuotaPct"] = 0
                state["quotaMessage"] = combined
                state["confidence"] = 0.99

    elif (is_editor_node or is_chat_content) and combined_lower:
        for qp in QUOTA_PHRASES:
            if qp in combined_lower and not state["quotaExhausted"]:
                state["isEditorContent"] = True

    for child in el.children:
        _inspect_element(child, is_editor_node, is_chat_content, now_system, state)
        if state["authGateDetected"]:
            return


def build_target_dict(el: Any, element_token: str = "") -> dict[str, Any]:
    node = _extract_single_normalized_node(el)
    token = element_token or node.element_token
    return {
        "role": node.role,
        "name": (node.label or node.text or node.value).strip(),
        "elementToken": token,
        "centerX": node.x + node.width // 2,
        "centerY": node.y + node.height // 2,
        "hasBounds": node.width > 0 and node.height > 0,
        "hasAction": bool(token),
    }


def _contains_any(text: str, needles: list[str]) -> bool:
    return any(n in text for n in needles)

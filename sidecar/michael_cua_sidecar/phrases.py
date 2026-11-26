"""Shared phrase dictionaries for Antigravity 2.34+ UI, Model -> View Usage popovers, Chrome OAuth, and Model families."""

from __future__ import annotations

QUOTA_PHRASES = [
    "quota exceeded",
    "quota exhausted",
    "usage limit reached",
    "rate limit exceeded",
    "out of quota",
    "you've reached your usage limit",
    "you have reached your usage limit",
    "model quota reached",
    "100% used",
    "0 credits remaining",
    "no credits remaining",
    "limit reached",
    "hết hạn mức",
    "đã chạm hạn mức",
    "vượt quá hạn mức",
]

VIEW_QUOTA_PHRASES = [
    "view usage",
    "view quota",
    "check quota",
    "quota",
    "plan usage",
    "usage",
    "credits",
    "credit usage",
    "xem hạn mức",
    "kiểm tra hạn mức",
    "hạn mức",
]

AUTH_CHALLENGE_PHRASES = [
    "verify you are human",
    "recaptcha",
    "hcaptcha",
    "cloudflare turnstile",
    "captcha",
    "2-step verification",
    "two-factor authentication",
    "enter your password",
    "authenticator code",
    "verify it's you",
    "xác minh 2 bước",
    "xác minh bạn là con người",
    "xác minh danh tính",
    "nhập mật khẩu",
]

SIGNOUT_PHRASES = ["sign out", "log out", "logout", "đăng xuất"]
SIGNIN_PHRASES = ["sign in", "log in", "login", "sign in with google", "đăng nhập"]
SETTINGS_PHRASES = ["settings", "preferences", "account settings", "antigravity settings", "cài đặt"]

ACCOUNT_TAB_PHRASES = [
    "account",
    "accounts",
    "profile",
    "authentication",
    "subscription",
    "plan",
    "tài khoản",
    "hồ sơ",
]

CONFIRM_SIGNOUT_PHRASES = [
    "sign out",
    "log out",
    "confirm",
    "yes",
    "continue",
    "đăng xuất",
    "xác nhận",
    "đồng ý",
]

CANCEL_DIALOG_PHRASES = [
    "cancel",
    "no",
    "stay signed in",
    "back",
    "dismiss",
    "hủy",
    "không",
    "quay lại",
]

MODEL_SELECTOR_PHRASES = [
    "select model",
    "model selection",
    "choose model",
    "chọn model",
    "chọn mô hình",
    "gemini",
    "orchestrator",
    "claude",
    "opus",
    "sonnet",
    "haiku",
    "gpt-",
    "gpt-oss",
]

MODEL_FAMILY_HINTS = (
    "gemini",
    "orchestrator",
    "claude",
    "opus",
    "sonnet",
    "haiku",
    "gpt-",
    "gpt-oss",
    "o3",
    "o4",
)

CHROME_WINDOW_KEYWORDS = [
    "sign in - google accounts",
    "choose an account",
    "chọn tài khoản",
    "google accounts",
    "google chrome",
    "chromium",
    "brave",
    "microsoft edge",
]

CHROME_EXCLUDED_ACCOUNT_PHRASES = [
    "use another account",
    "sử dụng tài khoản khác",
    "add another account",
    "thêm tài khoản khác",
    "remove an account",
]

# NOTE (v2.4.1 live calibration): NEVER put "document web" in EDITOR_ROLES!
# In Electron (Antigravity) and Chrome, the root `document web` container (1920x1048) wraps 100% of the app UI.
EDITOR_ROLES = {
    "document text",
    "document frame",
    "source code",
    "terminal",
    "text",
    "entry",
    "textbox",
    "text box",
    "paragraph",
}

# Passive conversation/document content roles inside Antigravity IDE that must NOT false-trigger
# HardStopChallenge or QuotaExhausted when discussing security/quota topics in chat messages.
CONVERSATION_CONTENT_ROLES = {
    "paragraph",
    "block quote",
    "table",
    "table row",
    "table cell",
    "column header",
    "heading",
    "code",
}

# Explicit system/modal surfaces where Quota alerts and Auth/MFA challenges legitimately appear in the IDE.
SYSTEM_SURFACE_ROLES = {
    "dialog",
    "alert dialog",
    "alert",
    "notification",
    "info bar",
    "status bar",
    "banner",
    "menu",
    "popup menu",
    "list box",
}

ALERT_ROLES = {
    "alert",
    "notification",
    "info bar",
    "status bar",
    "dialog",
    "alert dialog",
    "banner",
    "label",
    "panel",
    "progress bar",
}

CLICKABLE_ROLES = {
    "push button",
    "menu item",
    "combo box",
    "button",
    "link",
    "list item",
    "radio menu item",
    "check menu item",
    "toggle button",
    "tab",
    "page tab",
    "tree item",
}

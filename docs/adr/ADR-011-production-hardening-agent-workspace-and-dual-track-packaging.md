# ADR-011: Chuẩn Hóa Dedicated `"Agent Workspace"`, Cách Ly Chụp Ảnh Offscreen Không Lộ Workspace `#1`, Bù HiDPI, Bảo Vệ Lock Screen & Đóng Gói Dual-Track Production (`v2.5.0`)

- **Trạng thái:** Đã chấp thuận & Đã triển khai (`v2.5.0 Production Ready`)
- **Ngày:** 2026-11-26
- **Liên quan:** Mở rộng cho [ADR-008](ADR-008-agent-workspace-and-evidence-vault.md), [ADR-009](ADR-009-async-ipc-workspace-yield-and-daemon-guard.md), [ADR-010](ADR-010-uniffi-contract-and-isolated-profile-oauth-relay.md)

## 1. Bối cảnh & Quyết định Nền tảng

Qua vòng kiểm toán **Production Readiness Review (PRR)** trước phát hành GA, hệ thống chốt các bất biến kiến trúc bắt buộc để đạt chuẩn Production 24/7 trên Fedora 45 Wayland:

1. **Tên chuẩn duy nhất & Chuyên dụng 100%: `"Agent Workspace"`**
   - Thay thế tên cũ `"Michael-Auto-Agent"` bằng định danh chuẩn **`"Agent Workspace"`** (`domain.DefaultAgentWorkspaceName`).
   - Tra cứu động chỉ số Workspace theo tên qua `io.michael_auto.WorkspaceBridge.ResolveAgentWorkspace("Agent Workspace")` và `gsettings org.gnome.desktop.wm.preferences workspace-names` (bảo toàn tên các Workspace cá nhân sẵn có của người dùng, tự đổi tên `"Michael-Auto-Agent"` tại chỗ, và không bao giờ chiếm đè Workspace người dùng).
2. **Cách ly tuyệt đối khi chụp Bằng chứng Evidence (`Zero Workspace #1 Leakage` — P0-1):**
   - Trong GNOME Shell 51 Extension v3 (`michael-auto-workspace@michael-auto`), `CaptureWindowPng` sử dụng kết xuất trực tiếp **`Meta.WindowActor.paint_to_content()` → `Shell.Screenshot.composite_to_stream()`** để chụp bề mặt offscreen của chính cửa sổ trong `"Agent Workspace"` mà không chạm vào compositor stage của Workspace `#1`.
   - Khóa cứng đường dẫn D-Bus `outPath` chỉ cho phép tệp `.png` bên trong `$XDG_RUNTIME_DIR/michael-auto/` và từ chối symlink (`NOFOLLOW_SYMLINKS`).
   - Áp dụng phòng thủ chiều sâu (*defense-in-depth*) ở `actuator.py`: nếu GNOME Shell phiên hiện tại vẫn giữ cache module v2 trước khi relogin, không bao giờ gọi `CaptureWindowPng` hay `get_desktop_state` khi người dùng đang xem Workspace khác `"Agent Workspace"`.
3. **Bù Tỷ lệ HiDPI & Gốc Tọa độ Cửa sổ Trong `MaskPNGEditorRectsWithFrame` (P0-2) & Trần Dung lượng `150 MB` (P1-4):**
   - Truyền kèm `windowFrame` (`GetWindowFrame`) và `captureScope` (`"window"` / `"desktop"`) trong `wireEvidencePayload`; `storage.MaskPNGEditorRectsWithFrame` tự tính tỷ lệ `scaleX/scaleY` và trừ offset `(frame.X, frame.Y)` trước khi che vùng Editor.
   - Tối ưu nén PNG (`png.BestCompression`, max width `1600px`), chặn symlink khi xem lại (`ReadDataURL`), và áp trần dung lượng đĩa cứng `MaxVaultBytes = 150 MB`.
4. **Đồng bộ Cảm biến Tier-2 (`atspi_parser.go`), An toàn `config.json` v3 & Che Tham số PKCE (P0-3 / P0-4):**
   - Đồng bộ `atspi_parser.go` & `atspi_linux.go` (`maxATSPIDepth = 24`, bỏ `"document web"`, lá chắn chat `paragraph`/`table cell`/`list item`).
   - Sao lưu nguyên tử tệp cấu hình hỏng sang `config.json.corrupt.<timestamp>` (`0600`) và hiện cảnh báo trên UI; tự động migrate `v1/v2 → v3` bền vững lên đĩa; mở rộng bộ che log cho `code_challenge`, `code_verifier`, `nonce`, `client_id`, `flowName`.
5. **Độ Tin Cậy 24/7 (Lock Screen, Sidecar Pinning & Phục Hồi Breaker — P1):**
   - Lắng nghe D-Bus `org.gnome.ScreenSaver.ActiveChanged` & `org.freedesktop.login1.Manager.PrepareForSleep` để tạm hoãn sạch thao tác UI khi màn hình khóa (không làm nổ Circuit Breaker).
   - Pin cứng `cua-driver==0.58.1`; ưu tiên mã sidecar nhúng `embed.FS` đã xác thực SHA-256 (chống CWD hijack, chỉ đọc `./sidecar` khi đặt `MICHAEL_AUTO_DEV_SIDECAR=1`); nối `cuaImpl.ResetBreaker()` vào `ResetHardStop()` và tự kiểm chứng `health_check` sau auto-restart.
   - Hỗ trợ model Antigravity 2.34+ (`Gemini 4 Neon Extra High`, `Claude Sonnet 4.6 (Thinking)`), chế độ `preserve_active`, nút *"Quét Model từ Antigravity"* (`list_models`), và chặn lỗi im lặng ở `TriggerManualSwitch` khi tự động hóa đang tắt.
6. **UX Production, Đóng Gói Dual-Track & Diagnostics (P2):**
   - Bảo vệ form đang chỉnh sửa (`draftConfig` tách biệt `serverConfig` + banner *"Lưu / Hủy"* chống `config:updated` ghi đè), danh mục khởi tạo 3 bước, cảnh báo khi `< 2` profile bật, thu gọn Developer Sandbox, nút bật/tắt XDG Autostart.
   - Đóng gói Dual-Track: [michael-auto.spec](../../packaging/fedora/michael-auto.spec) + [michael-auto.service](../../packaging/michael-auto.service) + icon [michael-auto.svg](../../packaging/icons/michael-auto.svg) + `--uninstall` + `--export-diagnostics` (xuất `.tar.gz` ẩn danh PII).

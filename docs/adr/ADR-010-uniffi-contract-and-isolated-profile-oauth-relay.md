# ADR-010: Khớp Hợp Đồng Nhị Phân Rust UniFFI (`cua-driver 0.58+`), Tương Thích GNOME Shell 51 & Định Tuyến OAuth PKCE 3 Lớp Cho Mô Hình "1 Tài Khoản = 1 Chrome `--profile-directory`"

- **Trạng thái:** Đã chấp thuận & Đã triển khai (`v2.4.1 Live-Calibrated E2E-Ready`)
- **Ngày:** 2026-11-26
- **Liên quan:** Mở rộng cho [ADR-004](ADR-004-browser-profile-isolation-hardstop.md), [ADR-007](ADR-007-cua-driver-sidecar-integration.md), [ADR-008](ADR-008-agent-workspace-and-evidence-vault.md), [ADR-009](ADR-009-async-ipc-workspace-yield-and-daemon-guard.md)

## 1. Bối cảnh Thực Địa Trước Khi Bước Vào E2E

Qua giải phẫu nhị phân thực tế gói `cua_driver-0.58.1-py3-none-manylinux_2_31_x86_64.whl` (`_native_contract.py`, `libcua_driver_sdk.so` và tài liệu `LINUX.md` nhúng) kết hợp kiểm tra trực tiếp các tiến trình/bus trên máy **Fedora 45 (`GNOME Shell 51.1` Wayland, `Python 3.15.2`)**, hệ thống ghi nhận 3 nhóm đặc tính kỹ thuật cốt tử:

1. **Hợp đồng kiểu dữ liệu Rust UniFFI trong `cua-driver 0.58+`:**
   - Các lớp đầu vào (`ListWindowsInput`, `GetWindowStateInput`, `ClickInput`, `PressKeyInput`, `HotkeyInput`) yêu cầu truyền tường minh `None` cho mọi tham số keyword-only `Optional[...]` không có giá trị mặc định.
   - `WindowStateOutput` xuất tiêu đề tại `.window_title`, ảnh chụp tại `.images` (`list[SnapshotImage(mime_type, data_base64)]`), và cây phần tử tại `.elements` dưới dạng **danh sách phẳng** liên kết qua `parent_index` kèm `frame: ElementFrame(x, y, w, h)` và `is_password`.
   - `ListWindowsInput(on_screen_only=True)` ẩn các cửa sổ nằm trên **Agent Workspace `#2`** khi người dùng đang xem **Workspace `#1`**; `driver.click(...)` trả về `ActionResult.error` mềm (`background_unavailable`) khi cần leo thang lên `FOREGROUND`.
2. **Đặc thù `GNOME Shell 51.1` (`>= 50`) & AT-SPI2 trên Fedora 45:**
   - Helper `org.cua.WinRects` của `cua-driver` tắt trên GNOME Shell `>= 50`, nên cần cơ chế chụp ảnh cửa sổ dự phòng (`CaptureWindowPng` trên cầu nối GNOME 51 / `GetDesktopStateInput(screenshot_out_file=...)`) và trạng thái `AgentWorkspace.Ready` trung thực.
   - Electron (`/opt/antigravity/antigravity`) và Chrome chạy thuần Wayland cần kích hoạt đồng thời `org.a11y.Status.ScreenReaderEnabled=true` và biến môi trường phiên `ACCESSIBILITY_ENABLED=1` (`systemd`/`dbus-broker` + `environment.d`).
3. **Bất biến Mô hình Tài khoản của Người dùng: `1 Tài khoản Google = 1 Chrome --profile-directory riêng` (`Default`, `Profile 1`, `Profile 2`, ...):**
   - Trên Linux, Chrome gộp mọi cửa sổ `--profile-directory` vào chung 1 PID gốc qua `SingletonSocket`, và lệnh `xdg-open` từ Antigravity mặc định mở tab OAuth PKCE vào cửa sổ Chrome dùng gần nhất (dễ rơi nhầm vào Profile cũ vừa hết Quota). Nếu fallback bằng URL tĩnh `AccountChooser`, toàn bộ tham số OAuth2 PKCE (`client_id`, `state`, `code_challenge`, `redirect_uri=127.0.0.1:PORT`) sẽ bị mất.

## 2. Quyết định Kiến trúc

1. **Tương thích Tuyệt đối Hợp đồng UniFFI `cua-driver 0.58+`:**
   - `_safe_construct` tự động điền `None` cho mọi tham số `inspect.Parameter.empty`; luôn gọi `ListWindowsInput(..., on_screen_only=False)`.
   - `normalize_elements_hierarchy` dựng lại cây phân cấp từ `element_index`/`parent_index`, ánh xạ `ElementFrame(x, y, w, h)` cho bộ che vùng code Editor (`MaskPNGEditorRects`) và bắt cứng `is_password == True`.
   - Đọc `WindowStateOutput.images[0].data_base64` kèm fallback chụp ảnh trên GNOME 51; kiểm tra `ActionResult.error` để leo thang `BACKGROUND -> FOREGROUND` và dùng `HotkeyInput(["ctrl", "w"])`.
2. **Định tuyến OAuth2 PKCE 3 Lớp Cho Mô hình `1 Tài khoản = 1 --profile-directory`:**
   - **Lớp 1 — Pre-Warm MRU `--profile-directory` (Bước 4a):** Ngay trước `ClickSignIn`, mở/nâng nhẹ cửa sổ `--profile-directory=<Target>` để Chrome nhận trực tiếp tab `xdg-open` từ Antigravity vào đúng Profile đích.
   - **Lớp 2 — Lossless OAuth PKCE URL Relay (Bước 4b):** Nếu `xdg-open` lọt nhầm vào cửa sổ Profile khác, trích xuất nguyên vẹn URL OAuth2 PKCE từ Omnibox, đóng tab nhầm (`Ctrl+W`), rồi khởi chạy `--profile-directory=<Target>` với đúng URL OAuth2 PKCE đó (`oauth_profile_relay_required`).
   - **Lớp 3 — Single-Account Fast-Path:** Hỗ trợ cả trường hợp chọn dòng email lẫn trường hợp Profile đơn tài khoản nhảy thẳng sang màn hình `Continue` / `Open xdg-open`.
3. **Bảo vệ Bể Đa Profile & Chẩn đoán Trực tiếp (`v2.4.0`):**
   - Bổ sung quét email fallback từ `<ProfileDir>/Preferences` (`account_info[0].email`) khi tắt Chrome Sync; chặn cấu hình trùng `BrowserProfileDir` giữa các hồ sơ đang bật; tắt tự đăng nhập lặp lại cùng tài khoản khi `enabledProfiles >= 2`.
   - Bổ sung trường `e2eReady` & `remediationHints` vào `--inspect-ui` để phát hiện tức thì tiến trình Antigravity chạy trước khi bật AT-SPI2.
4. **Hiệu chuẩn Thực địa Trực tiếp Trên `Antigravity 2.34.0` & `GNOME Shell 51.1` (`v2.4.1`):**
   - `capture_window_state` truyền tường minh `max_depth=48, max_elements=2500, full_output=True` để đọc trọn toàn bộ DOM Electron (thay vì bị `cua-driver` cắt cụt ở độ sâu `8`).
   - Loại bỏ `"document web"` khỏi `EDITOR_ROLES` để không làm mù cây UI Electron và không bôi đen toàn bộ ảnh chụp Evidence; bổ sung lá chắn chống báo giả theo cây con cho nội dung chat Markdown (`paragraph`, `table cell`, `block quote`, `heading`, `list item` ngoài `menu`/`dialog`, và nút cuộn khuất `y + height <= 0` qua `is_valid_ide_control`) trong cửa sổ IDE (`ide_mode=True`), đồng thời giữ nguyên độ nhạy 100% (kể cả `<h1> heading` / `<p> paragraph` 2FA/CAPTCHA) trên cửa sổ Google Chrome OAuth (`ide_mode=False`).
   - Hỗ trợ chu trình đọc Quota 2 chặng thực tế của Antigravity `2.34.0` (**`Select model, current: ...` → `View Usage`**), tách riêng 2 nhóm hạn mức (**`Gemini Models`** vs **`Claude and GPT models`**, xét `min(Weekly limit, 5h limit)` và khớp `0% left` bằng regex biên từ `(?<!\d)0%` tránh nhầm với `100% left`), và đóng gọn 2 lớp bằng `Escape x2`.
   - Tự động bổ sung `michael-auto-workspace@michael-auto` vào `gsettings org.gnome.shell enabled-extensions` để kích hoạt sống cầu nối `Agent Workspace #2` trên GNOME Shell `51.1`.

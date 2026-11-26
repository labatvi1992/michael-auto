# Changelog

Tất cả các thay đổi quan trọng của **Michael Auto** được ghi nhận theo chuẩn [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) và đánh số phiên bản theo [Semantic Versioning](https://semver.org/).

## [2.5.0] — 2026-11-26 (Production GA Candidate)

### Added
- **Dedicated `"Agent Workspace"` & GNOME Shell 51 Extension v4 (`ArmAutoRoute` + Offscreen Capture):**
  - Định danh tĩnh không gian làm việc dành riêng `"Agent Workspace"` (`ADR-011`), giữ nguyên các workspace làm việc hiện hữu của người dùng và tự di trú tên cũ (`Michael-Auto-Agent`).
  - Phương thức D-Bus `CaptureWindowPng` vẽ trực tiếp `Meta.WindowActor.paint_to_content` + `Shell.Screenshot.composite_to_stream` offscreen, ngăn hoàn toàn rò rỉ pixel từ User Workspace `#1` và chặn ghi tệp ngoài `$XDG_RUNTIME_DIR/michael-auto/*.png` hoặc qua symlink.
  - Phương thức D-Bus `ArmAutoRoute(ttlSeconds, workspaceIndex)` lắng nghe `global.display::window-created` để định tuyến cửa sổ Chrome OAuth sang `"Agent Workspace"` ngay trước khung hình đầu tiên (0 ms nhấp nháy trên Workspace `#1`).
  - `WaylandWorkspaceManager.Status()` phát hiện và cảnh báo rõ ràng khi cầu nối GNOME v2 cũ vẫn đang nạp trong RAM và cần đăng xuất/đăng nhập GNOME 1 lần để nạp v4.
- **Control Socket Protocol v2 & CLI `--ctl`:**
  - Giao thức `control.sock` phân biệt `"ok:gui"` vs `"ok:headless"`, đồng thời hỗ trợ điều khiển tiến trình đang chạy qua `--ctl status|pause|resume|show|quit`.
  - Khi tiến trình `--headless` đang giữ khóa mà người dùng bấm icon Desktop, hiển thị ngay thông báo Desktop GNOME + hướng dẫn dừng sạch thay vì im lặng.
- **Lazy D-Bus Reconnect cho `WaylandActivityMonitor`:**
  - Tự động kết nối lại `org.gnome.Mutter.IdleMonitor` / `org.freedesktop.ScreenSaver` theo exponential backoff (`1s → 2s → 5s → 10s`) khi khởi động sớm cùng phiên GNOME Wayland, tự động vũ trang lại watch hoạt động người dùng và khóa màn hình.
- **Supply-Chain & Crypto Hardening:**
  - Khóa cứng mã băm SHA-256 (`--require-hashes`) trong `sidecar/cua-driver-0.58.1.requirements.lock` cho cả 5 binary wheel của `cua-driver==0.58.1` (0 gói phụ thuộc bắc cầu) + hỗ trợ thư mục cài offline `CUA_WHEEL_DIR`.
  - Chuyển toàn bộ token phiên `xdg-desktop-portal` trong `portal_wayland_linux.go` sang `crypto/rand`.
- **GNOME Lock Screen & Suspend Guard:**
  - Giám sát `org.gnome.ScreenSaver.ActiveChanged` và `org.freedesktop.login1.Manager.PrepareForSleep` để tạm dừng tự động hóa ngay khi khóa màn hình hoặc ngủ đông.
- **Dual-Track Packaging & Operations:**
  - Offline-ready Fedora `.rpm` (`make rpm-source` đóng gói sẵn `frontend/dist` + Go `vendor/`), `systemd --user` chạy `--start-hidden` với `ConditionEnvironment=WAYLAND_DISPLAY`, `StartupWMClass=michael-auto`, và làm mới `gtk-update-icon-cache` / `update-desktop-database`.
  - CLI `--export-diagnostics` (nén `.tar.gz` đã ẩn danh PII/token, quyền `0600`) và `--uninstall` dọn sạch user-space.
  - Quản trị phát hành: `CHANGELOG.md`, `SECURITY.md`, `THIRD_PARTY_NOTICES.md`, hiển thị huy hiệu phiên bản `v2.5.0` trên Topbar Dashboard.

### Changed
- **HiDPI-Aware Evidence Redaction:** Khung che vùng mã nguồn/chat (`MaskPNGEditorRectsWithFrame`) tính đúng tỷ lệ scale vật lý/logic (1x, 1.25x, 1.5x, 2x) và độ lệch gốc cửa sổ.
- **Config Schema v3 & Corruption Protection:** Tự động sao lưu tệp cấu hình hỏng sang `config.json.corrupt.<timestamp>` trước khi khôi phục mặc định, đồng thời hiển thị banner cảnh báo trên UI.
- **Dirty-Form Guard:** Các sự kiện nền `config:updated` không ghi đè biểu mẫu đang được người dùng chỉnh sửa trên tab Hồ sơ / Cấu hình.

### Fixed
- Khắc phục xung đột âm thầm giữa `michael-auto.service` (`--headless`) và biểu tượng ứng dụng trên GNOME App Grid.
- Khắc phục lỗi kẹt `backend == "unavailable"` vĩnh viễn của `WaylandActivityMonitor` khi ứng dụng khởi động sớm hơn GNOME Shell.
- Khắc phục lỗi `CuaDriver.Enabled` không đồng bộ trạng thái actuator ngay trong `Status()` sau khi người dùng lưu cấu hình nóng.
- Đồng bộ đầy đủ 4 binding Wails mới trong `frontend/wailsjs/go/main/App.js`.

---

## [2.4.1] — 2026-11-26

### Added
- Hiệu chuẩn thực địa với **Antigravity 2.34.0** trên Fedora 45 Wayland: mở rộng độ sâu duyệt AT-SPI2 (`max_depth=24`), lộ trình kiểm tra Quota chính xác `Select model → View Usage` (hỗ trợ nhóm *Gemini Models* + *Claude and GPT models*, *Weekly limit* & *5h limit*).
- Cơ chế chống báo giả cho nội dung chat/tài liệu Markdown bên trong `document web` của Electron.

---

## [2.3.0] — 2026-11-25

### Added
- **Lossless OAuth2 PKCE Relay:** Nếu `xdg-open` mở nhầm cửa sổ Chrome profile khác, sidecar trích xuất đúng URL PKCE `https://accounts.google.com/...`, đóng tab lạc bằng `Ctrl+W`, và Go khởi chạy đúng `--profile-directory=<target>` với URL đó.
- Tự động bootstrap `uv` (xác thực SHA-256) cài Python 3.12 độc lập trên máy Fedora 45 dùng Python 3.15 chưa có wheel `cp315`.
- Tự động bật nóng `org.a11y.Status.ScreenReaderEnabled` trên AT-SPI2 Bus để đánh thức DOM của Electron/Chrome đang chạy.

---

## [2.1.0] — 2026-11-25

### Added
- Kiến trúc Hexagonal Go + Wails v2 + React + Python `trycua/cua` (`cua-driver`) thay thế hoàn toàn các giải pháp OCR bên ngoài.
- Quy trình 5 bước có cổng bằng chứng **Verify → Screenshot Evidence (SHA-256) → Execute** với chu kỳ kiểm tra Quota 60 giây và thời gian nghỉ 60 giây sau `Sign Out`.
- Cơ chế **30-second Sliding Human Yield** qua GNOME Mutter `IdleMonitor` và **Hard-Stop** tức thì khi phát hiện MFA / CAPTCHA / yêu cầu nhập mật khẩu.

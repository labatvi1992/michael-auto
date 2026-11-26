# ADR-008: Không gian Agent Cô lập trên Fedora 45 Wayland, Giao thức Bằng chứng Trước Thực thi & Khóa Cứng Chrome Profile

- **Trạng thái:** Đã chấp thuận & Đã triển khai (`v2.2.0`)
- **Ngày:** 2026-11-25
- **Liên quan:** Bổ sung & siết chặt cho [ADR-003](ADR-003-fedora-wayland-platform-architecture.md), [ADR-004](ADR-004-browser-profile-isolation-and-model-policy.md), [ADR-007](ADR-007-cua-driver-sidecar-integration.md)

## 1. Bối cảnh & Các Bất biến Kiến trúc Bắt buộc

Trong quá trình vận hành tự động hóa không giám sát qua `trycua/cua` (`cua-driver`) trên **Fedora 45 (GNOME Mutter Wayland)**, hệ thống phải tuân thủ đồng thời 5 yêu cầu kiến trúc bắt buộc:

1. **Chu kỳ kiểm tra Quota 1 phút (`60,000ms`):** Bắt buộc giữ nguyên giãn cách mặc định 1 phút để phát hiện kịp thời hạn mức mà không làm gián đoạn cửa sổ làm việc hiện tại của người dùng.
2. **Điều kiện kích hoạt chuyển tài khoản tại đúng ngưỡng `0%` & `Quota exhausted`:** Không chuyển sớm khi còn hạn mức; chỉ thực thi khi bảng Quota hoặc thông báo hệ thống xác nhận về `0%` / `Quota exhausted`, chụp ảnh bằng chứng xác minh trước khi bấm `SignOut` và chờ đúng **1 phút (`60,000ms`)**.
3. **Đảm bảo mở đúng tiến trình & Profile Google Chrome (không cần Pre-Warming):** Chrome khởi động đủ nhanh trên Linux hiện đại; yêu cầu trọng tâm là đảm bảo cửa sổ Chrome mở đúng `--profile-directory` đã cấu hình và đang ở đúng tên miền xác thực hợp lệ.
4. **Lưu tạm `PendingIntent` vào tệp và giải phóng ngay sau khi hoàn tất:** Tránh mất dấu chu trình nếu ứng dụng khởi động lại giữa thời gian chờ 1 phút, nhưng tuyệt đối không để lại tệp trạng thái dư thừa sau khi xong việc.
5. **Giao thức An toàn `Verify -> Screenshot Evidence -> Execute`:** Mọi bước nhạy cảm đều phải xác minh ngữ cảnh tiến trình/cửa sổ và lưu bằng chứng ảnh chụp màn hình cục bộ trước khi phát lệnh click.

## 2. Quyết định Kiến trúc

### 2.1. Cô lập Không gian Làm việc (`Agent Workspace`) Chuẩn Native Wayland trên Fedora 45
- Triển khai `WaylandWorkspaceManager` (`internal/adapters/workspace/wayland_workspace_linux.go`) cấu hình `org.gnome.mutter dynamic-workspaces=false`, `focus-new-windows='smart'`, đảm bảo tối thiểu `num-workspaces >= 2` và đặt nhãn Workspace `#2` (`index = 1`) là `"Michael-Auto-Agent"` tách biệt hoàn toàn với **User Workspace `#1` (`index = 0`)**.
- Tự động cài đặt cầu nối không cần `root` **GNOME Shell Mini-Extension** (`michael-auto-workspace@michael-auto`, hỗ trợ GNOME Shell 45–55/51.1) gọi trực tiếp `Meta.Window.change_workspace_by_index(idx, false)` qua `gdbus` cho cửa sổ **Native Wayland** (kèm fallback `wmctrl`/`xdotool` cho XWayland), kết hợp tương tác `BACKGROUND` (`element_token`) không gọi `focus_window` ở chu kỳ 1 phút để không bao giờ giật góc nhìn của User.

### 2.2. Giao thức `Verify -> Screenshot Evidence -> Execute`, Che Vùng Code & Kho Bằng chứng `EvidenceVault`
- Mở rộng Python sidecar (`sidecar/michael_cua_sidecar/security.py`, `actuator.py`) và Go `EvidenceVault` (`internal/adapters/storage/evidence_store.go`):
  - Kiểm chứng danh sách trắng tiến trình qua `/proc/<pid>/exe` & `/proc/<pid>/cmdline`.
  - Chụp ảnh cửa sổ mục tiêu qua `cua-driver`, tự động **che mờ vùng khung soạn thảo code/chat (`MaskPNGEditorRects`)** để bảo vệ quyền riêng tư dự án, tính mã băm **SHA-256** và lưu cặp `<step>.png` + `<step>.meta.json` với quyền `0600` tại `$XDG_STATE_HOME/michael-auto/evidence/<run_id>/` trước cả 5 cổng thực thi (`01_verify_quota_zero` .. `05_pre_model_select`).
  - Mọi lỗi `-32004` (`ErrAuthGateDetected`) hoặc `-32006` (`ErrSecurityViolation` — sai Omnibox / sai tiến trình) đều kích hoạt `HardStopChallenge` ngay ở lần phát hiện đầu tiên, không thử lại.

### 2.3. Duyệt Đa Cửa sổ & Khóa Cứng Hồ sơ Google Chrome (`Local State` + Omnibox + Tự đóng Tab OAuth)
- `chrome_profiles.go` đọc chỉ-đọc (`read-only`) tệp `Local State` (`profile.info_cache`) của Chrome/Chromium/Brave/Edge để quét danh sách `--profile-directory`, tên hiển thị hồ sơ và `emailHint`.
- `select_chrome_account` duyệt qua **toàn bộ danh sách cửa sổ Chrome đang mở**, tự động bỏ qua cửa sổ Chrome cá nhân khác của User để chọn đúng cửa sổ khớp `expectedProfileName` + `accounts.google.com`, và tự động đóng các tab `Authentication complete` dư thừa sau khi hoàn tất OAuth.
- Trên các bản Fedora 45 dùng Python mới (`3.15+`) chưa có wheel `cp315`, `sidecar_provisioner.go` tự động bootstrap `uv` (xác minh SHA-256) vào `$XDG_DATA_HOME/michael-auto/bin/uv` để khởi tạo Python `3.12` cô lập.

### 2.4. Tệp Trạng thái Tạm thời Tự Hủy (`EphemeralIntentStore`)
- `internal/adapters/storage/intent_store.go` ghi nguyên tử `pending_intent.json` (quyền `0600` trong `$XDG_RUNTIME_DIR/michael-auto/` hoặc `$XDG_STATE_HOME/michael-auto/`) trong suốt phiên chuyển đổi.
- Ngay khi hoàn tất Bước 5 (`completeIntent`), hết hạn TTL, hoặc người dùng bấm `ResetHardStop`, hệ thống gọi `os.Remove` **xóa sạch tệp ngay lập tức**.

### 2.5. Tối ưu Hiệu năng Thực thi
- **Short-Circuit `SensorFusion`:** Khi `cua_driver` quan sát thành công, bỏ qua lượt quét cây `AT-SPI2` dự phòng trong cùng chu kỳ, đồng thời tái sử dụng kết nối `org.a11y.Bus` bền vững.
- **Event-Driven Idle & Health Cache:** Giãn nhịp thăm dò nền trên `WaylandActivityMonitor` khi không thực thi và lưu đệm `SensorHealth` 10 giây.
- **`O(1)` Tail-Seek `audit.jsonl`:** Chỉ đọc cửa sổ 128 KB cuối tệp nhật ký kết hợp bộ lọc che tham số OAuth (`code=`, `state=`, `login_hint=`) trên cả luồng `stderr` của sidecar.

## 3. Hệ quả
- Người dùng làm việc liên tục trên Workspace `#1` không bị cướp tiêu điểm mỗi 1 phút kiểm tra Quota.
- Mọi quyết định SignOut, chọn tài khoản Chrome và chọn Model đều có bằng chứng hình ảnh SHA-256 kiểm chứng được trực tiếp ngay trên giao diện Wails Dashboard.

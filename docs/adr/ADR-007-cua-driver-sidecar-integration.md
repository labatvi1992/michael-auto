# ADR-007: Kiến trúc Động cơ Chính `trycua/cua` (`cua-driver`) Thay thế Screenpipe cho Chu trình 5 Bước Tự động

- **Trạng thái:** Đã chấp thuận (Accepted — Primary Engine)
- **Ngày:** 2026-11-25

## Bối cảnh

Hệ thống cần thực hiện trọn vẹn chu trình giám sát và khôi phục phiên làm việc hoàn toàn tự động mà **không sử dụng Screenpipe**:

1. Tự động Focus màn hình của **Antigravity** (tôn trọng độ tĩnh tối thiểu của người dùng).
2. Bấm nút xem **Quota** sau khoảng thời gian giãn cách cấu hình (mặc định **1 phút**).
3. Khi phát hiện **Quota hết**, mở màn hình **Settings** của ứng dụng và bấm nút **SignOut**.
4. Chờ đúng **1 phút** (phi chặn), sau đó bấm nút **SignIn** và **Focus sang cửa sổ Google Chrome** để chọn **Account đã đăng nhập sẵn**.
5. Quay lại chọn **Model mặc định** đã cấu hình từ trước và tiếp tục vòng lặp giám sát.

## Quyết định

1. **Loại bỏ 100% Screenpipe & Đặt `cua-driver` (`trycua/cua`) làm Động cơ Chính (`Enabled: true` mặc định):**
   - Sử dụng trực tiếp SDK Rust/Python gốc `cua-driver` ở chế độ Deterministic UI Automation (`GetWindowStateInput(..., include_accessibility_tree=True, tree_format="elements")`, `ActionTarget.WINDOW`, `ClickPosition.ELEMENT`), chi phí LLM bằng `0` và không gửi ảnh màn hình ra ngoài máy.
2. **Định danh gói Sidecar độc lập (`sidecar/michael_cua_sidecar/`):**
   - Đặt tên package nội bộ là `michael_cua_sidecar` tách biệt hoàn toàn với tên gói PyPI `cua_driver` để loại trừ lỗi `Module Shadowing`.
   - Giao tiếp qua JSON-RPC 2.0 trên `stdin/stdout` với mã lỗi miền chuẩn hóa (`-32001..32005`, trong đó `-32004` ánh xạ trực tiếp sang `ports.ErrAuthGateDetected` kích hoạt `HardStopChallenge` tức thì khi gặp Password/MFA/CAPTCHA trên Antigravity hoặc Chrome).
3. **Kiến trúc `Single-Reader Multiplexer` & `Automatic ActuationLease` bên Go:**
   - Tách riêng `daemonCtx` của sidecar khỏi `stepCtx` ngắn hạn của từng bước FSM; duy nhất một goroutine đọc `stdout` định tuyến phản hồi theo `ID` và duy nhất `monitorProcess` gọi `cmd.Wait()`.
   - Mọi lệnh Focus/Click từ `CuaDriverActuator` tự động bọc trong `AcquireActuationLease` để `WaylandActivityMonitor` (`Mutter.IdleMonitor`) không nhầm xung tự động của CUA với thao tác chuột/phím của con người.
4. **Chu trình 5 Bước Phi chặn (`Non-Blocking Sub-Step FSM`):**
   - Lưu trạng thái con trong `PendingIntent.SubStep` (`signout_settings` → `wait_before_signin` → `click_signin` → `select_chrome_account` → `verify_and_select_model`) cùng mốc `NextActionAt` thay vì dùng `time.Sleep`/`time.After` chặn luồng. Nhờ đó, nếu người dùng di chuột giữa lúc đang đếm ngược 60 giây sau SignOut, hệ thống nhường quyền 30 giây mượt mà và tiếp tục chính xác bước kế tiếp mà không mất dấu trạng thái.
5. **Đóng gói Nhị phân Đơn (`//go:embed`) & Môi trường Ảo XDG Tự động (`Provisioner`):**
   - Nhúng trực tiếp mã nguồn `michael_cua_sidecar` vào binary Go qua `sidecar/embed.go`, tự động giải nén kiểm tra băm SHA-256 ra `$XDG_DATA_HOME/michael-auto/sidecar/` để hoạt động độc lập khi chạy từ `~/.local/bin` hoặc GNOME Autostart.
   - Hỗ trợ lệnh `--setup-sidecar` (và nút bấm 1 chạm trên UI) tự tạo virtualenv cô lập tại `$XDG_DATA_HOME/michael-auto/venv` (ưu tiên `uv` Python 3.12 cho wheel nhị phân Rust hoặc `python3 -m venv` kèm `ensurepip`), đồng thời bật `toolkit-accessibility` trên GNOME Wayland và cờ `--force-renderer-accessibility` khi định tuyến Chrome để xuất đầy đủ cây phần tử.

## Hệ quả

- Đáp ứng trọn vẹn chu trình 5 bước xuyên suốt hai ứng dụng **Antigravity ↔ Google Chrome** trên Fedora 45 Wayland (`CUA_DRIVER_RS_ENABLE_WAYLAND=1`).
- Không còn phụ thuộc Screenpipe; kiến trúc gọn nhẹ, an toàn tương tranh (`go test -race` sạch 100%) và có kiểm thử tự động đầu-cuối cho cả Go, Python sidecar và giao diện React.

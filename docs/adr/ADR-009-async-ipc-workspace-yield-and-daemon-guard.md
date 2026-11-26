# ADR-009: IPC Bất Đồng Bộ Chống Ghost-Click (`$/cancelRequest`), Nhường Quyền Nhận Biết Không Gian (`Workspace-Aware Yield`) & Khóa Đơn Tiến Trình (`flock`)

- **Trạng thái:** Đã chấp thuận & Đã triển khai (`v2.3.0`)
- **Ngày:** 2026-11-25
- **Liên quan:** Mở rộng cho [ADR-002](ADR-002-unattended-fsm-human-yield.md), [ADR-007](ADR-007-cua-driver-sidecar-integration.md), [ADR-008](ADR-008-agent-workspace-and-evidence-vault.md)

## 1. Bối cảnh Kiến trúc Trước Kiểm thử E2E

Qua rà soát toàn diện kiến trúc liên tầng trước khi bước vào kiểm thử End-to-End (E2E) trên **Fedora 45 (`GNOME Shell 51.1` Wayland)**, hệ thống bộc lộ 4 thách thức cốt lõi của ứng dụng điều phối thời gian thực chạy nền dài ngày:

1. **Hiện tượng "Ghost Click" sau khi chạm chuột:** Dù phía Go hủy `stepCtx` trong `< 20ms`, nếu tiến trình Python sidecar đọc `stdin` đồng bộ, tác vụ đang `await` trong Python vẫn tiếp tục click trễ sau khi người dùng đã giành lại chuột.
2. **Nghịch lý `Global Idle` vs `Agent Workspace #2`:** `Mutter.IdleMonitor` đo thời gian tĩnh toàn cục; nếu người dùng gõ code liên tục ở **Workspace `#1`**, bộ ngắt 30s toàn cục sẽ khóa cứng khả năng xoay vòng tài khoản ngầm trên **Agent Workspace `#2`**.
3. **Nguy cơ chạy trùng 2 Daemon (`Split-Brain`) & Cửa sổ ẩn trên Vanilla GNOME 51:** GNOME mặc định không hiển thị khay icon `StatusNotifierItem`; nếu đóng cửa sổ ẩn mà không có kênh đánh thức đơn tiến trình, người dùng không thể mở lại giao diện và dễ khởi chạy nhầm tiến trình thứ hai.
4. **Độ tin cậy thực địa trên Electron / Google Chrome OAuth:** Cần vòng hội tụ đa chặng chờ màn hình *OAuth Consent ("Continue")* + hộp thoại *Protocol Handler ("Open xdg-open")*, kích hoạt nóng `ScreenReaderEnabled` cho cửa sổ đang chạy, đối soát hậu đăng nhập Bước 5 và chống ghi đè trạng thái hồ sơ động.

## 2. Quyết định Kiến trúc

1. **Phân kênh JSON-RPC Bất đồng bộ & Tín hiệu Hủy Tức thời `$/cancelRequest` (`AsyncJsonRpcMux`):**
   - `sidecar/michael_cua_sidecar/server.py` tách luồng đọc `stdin` khỏi luồng thực thi `asyncio.Task`, lưu bảng `inflight[req_id]`.
   - Ngay khi `callCtx.Done()` kích hoạt phía Go (`cua_driver.go`), Go gửi `{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":id}}` để Python gọi `task.cancel()` tức thì (`-32800`), cam kết **0 thao tác click thừa** sau khi ngắt chuột.
2. **Khóa Thực thi Đơn luồng `StepOnce` (`stepExecMu`) & Chính sách `Workspace-Aware Yield`:**
   - `Orchestrator` tuần tự hóa `StepOnce` qua `stepExecMu` (`TryStepOnce` cho vòng lặp nền) và khóa chặt mọi cập nhật `PendingIntent`.
   - Khi cầu nối Wayland (`io.michael_auto.WorkspaceBridge.GetActiveWorkspace`) xác nhận người dùng đang ở **Workspace `#1`** (`!= AgentWorkspaceIndex`) và chế độ truyền không ép `foreground`, thao tác gõ phím ở Workspace `#1` không làm đóng băng Agent ở Workspace `#2`; ngay khoảnh khắc người dùng chuyển sang xem Workspace `#2`, bộ ngắt `< 20ms` + trượt `30s` lập tức kích hoạt.
3. **Khóa Đơn Tiến trình `InstanceGuard` (`flock` + UNIX Socket) & Thông báo GNOME (`DesktopNotifier`):**
   - Khóa độc quyền `$XDG_RUNTIME_DIR/michael-auto/daemon.lock` kết hợp socket cục bộ `control.sock` (`0600`): lần gọi thứ hai tự động gửi lệnh `"show-window"` bật sáng cửa sổ đang ẩn rồi thoát êm.
   - Phát thông báo gốc `org.freedesktop.Notifications` khi vào `HardStopChallenge` (`critical`) hoặc khi hoàn tất chuyển tài khoản (`normal`).
4. **Vòng Hội tụ Hậu-OAuth 8s, Kích hoạt Nóng `ScreenReaderEnabled`, Đối soát Bước 5 & `--inspect-ui`:**
   - Theo dõi đa chặng sau khi chọn tài khoản Chrome (`Continue` → `Allow` → `Open xdg-open`), kiểm tra `ErrPostSwitchQuotaExhausted` / `ErrAccountMismatch` ở Bước 5, bảo toàn `ExhaustedUntil` khi lưu Settings kèm sự kiện `config:updated`, và cung cấp lệnh chỉ-đọc `--inspect-ui`.

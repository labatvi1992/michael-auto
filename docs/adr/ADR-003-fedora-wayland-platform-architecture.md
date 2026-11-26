# ADR-003: Kiến trúc Ưu tiên Linux Wayland trên Fedora 45

- **Trạng thái:** Đã chấp thuận (Accepted)
- **Ngày:** 2026-11-24

## Bối cảnh

Môi trường triển khai ưu tiên hàng đầu là **Fedora 45 (Linux Wayland)**. Trên Fedora hiện đại, phiên làm việc mặc định thuần Wayland; các công cụ giả lập đầu vào và theo dõi sự kiện thế hệ X11 (`xdotool`, `XTest`, `XInput2` qua XWayland) không thể quan sát hay điều khiển các cửa sổ Wayland gốc. Đồng thời, ứng dụng không nên yêu cầu quyền `root` (`sudo` / truy cập trực tiếp `/dev/input/event*`) để tránh rủi ro bảo mật.

## Quyết định

Thiết kế stack nền tảng ưu tiên gốc Wayland trên Fedora 45 (vận hành 100% trong không gian người dùng):

1. **Theo dõi thao tác & Thời gian tĩnh lặng:** Sử dụng D-Bus `org.gnome.Mutter.IdleMonitor` (`AddUserActiveWatch`, `GetIdletime`) trên GNOME và giao thức Wayland `ext-idle-notify-v1` / `org.freedesktop.ScreenSaver` trên KDE/wlroots, kết hợp cơ chế `ActuationLease` ngắn (`45ms`) để tách xung điều khiển tự động khỏi thao tác vật lý mà không cần ghi lén nội dung phím gõ.
2. **Thực thi thao tác đầu vào hợp chuẩn:** Sử dụng `org.freedesktop.portal.RemoteDesktop` kết hợp `libei` (`ConnectToEIS`) với `persist_mode = 2`. Chuỗi `restore_token` sau lần cấp quyền đầu tiên được bảo vệ trong `org.freedesktop.secrets` (`libsecret`), cho phép các phiên sau chạy tự động không bật lại hộp thoại xin quyền.
3. **Giao diện & Khay hệ thống:** Sử dụng Go + Wails biên dịch với cờ `-tags webkit2_41` (tương thích `webkit2gtk4.1-devel` trên Fedora 45) và giao thức khay hệ thống `StatusNotifierItem` qua D-Bus.
4. **Đa nền tảng về sau:** Cô lập mã phụ thuộc hệ điều hành bằng Go build tags để dễ dàng bổ sung adapter cho X11, macOS và Windows ở các giai đoạn tiếp theo.

## Hệ quả

- Chạy ổn định, chuẩn bảo mật trên Fedora 45 Wayland mà không cần hạ cấp phiên về X11 hay cấp quyền `root`.

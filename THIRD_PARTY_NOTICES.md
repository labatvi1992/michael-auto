# Third-Party Open Source Notices

**Michael Auto** được phát hành theo giấy phép [MIT License](LICENSE) và sử dụng các thành phần mã nguồn mở sau. Toàn bộ bản quyền thuộc về các tác giả tương ứng.

## Go Runtime & Backend Dependencies

| Thư viện | Phiên bản / Nhánh | Giấy phép | Mô tả |
| :--- | :--- | :--- | :--- |
| [Go Standard Library](https://go.dev/) | Go 1.24+ | BSD-3-Clause | Nền tảng biên dịch, mật mã học (`crypto/rand`, `crypto/sha256`), ảnh PNG & mạng cục bộ. |
| [Wails v2](https://github.com/wailsapp/wails) | v2.11+ | MIT | Khung ứng dụng desktop Go + WebKitGTK (`webkit2gtk4.1`). |
| [godbus/dbus](https://github.com/godbus/dbus) | v5 | BSD-2-Clause | Giao tiếp D-Bus thuần Go với GNOME Mutter, ScreenSaver, Secret Service, Notifications & logind. |
| [fyne.io/systray](https://github.com/fyne-io/systray) | v1.11+ | Apache-2.0 | Tích hợp biểu tượng khay hệ thống (StatusNotifierItem). |

## Python Sidecar & Desktop Automation

| Thư viện | Phiên bản | Giấy phép | Mô tả |
| :--- | :--- | :--- | :--- |
| [trycua/cua (`cua-driver`)](https://github.com/trycua/cua) | `0.58.1` (pinned SHA-256) | MIT | Trình điều khiển desktop cục bộ hỗ trợ cây phần tử AT-SPI2 / Wayland (`0` phụ thuộc bắc cầu). |
| [Astral `uv`](https://github.com/astral-sh/uv) *(tùy chọn dự phòng)* | `0.6.6` (SHA-256 verified) | MIT / Apache-2.0 | Công cụ bootstrap Python 3.12 độc lập khi hệ điều hành dùng Python quá mới chưa có wheel. |

## Frontend (Desktop Dashboard UI)

| Thư viện | Phiên bản | Giấy phép | Mô tả |
| :--- | :--- | :--- | :--- |
| [React & React DOM](https://react.dev/) | v18 | MIT | Giao diện điều khiển bảng quản trị desktop. |
| [Vite](https://vite.dev/) & `@vitejs/plugin-react` | v5 / v4 | MIT | Bộ đóng gói mã nguồn frontend tĩnh nhúng vào binary Go. |
| [TypeScript](https://www.typescriptlang.org/) | v5 | Apache-2.0 | Kiểm tra kiểu tĩnh cho giao diện React. |
| [Vitest](https://vitest.dev/) & [Testing Library](https://testing-library.com/) | v2 / v16 | MIT | Kiểm thử tự động giao diện người dùng (chỉ dùng lúc phát triển/CI). |

## Hệ Sinh Thái Hệ Điều Hành (Fedora Workstation / GNOME Wayland)

Michael Auto tương tác tại thời điểm chạy qua giao diện công khai (D-Bus / CLI) với các thành phần hệ thống cài sẵn trên Fedora Workstation:
- **GTK3 & WebKitGTK 4.1** (LGPL-2.1)
- **GNOME Shell / Mutter / AT-SPI2 / GLib / Gio** (GPL-2.0+ / LGPL-2.1+)
- **systemd (`systemd --user`, `logind`)** (LGPL-2.1+)

# Michael Auto

Michael Auto là ứng dụng desktop chạy trên khay hệ thống, tự động giám sát hạn mức sử dụng trên **Agent Workspace cô lập**, xác minh bằng chứng hình ảnh (**SHA-256 Evidence Vault**), điều phối xoay vòng tài khoản đã đăng nhập sẵn trên đúng **Google Chrome Profile** và khôi phục cấu hình **Model AI mặc định** khi làm việc với **Antigravity** — sử dụng **`trycua/cua` (`cua-driver`)** làm động cơ tự động hóa chính trên **Linux Wayland (Fedora 45)** *(không sử dụng Screenpipe)*.

> **Trạng thái dự án:** **v2.5.0 — Production Ready trên Fedora 45 Native Wayland (`cua-driver==0.58.1` + Dedicated `"Agent Workspace"` + Offscreen Zero-Leak Evidence + HiDPI Redaction + GNOME LockScreen Guard + Dual-Track RPM/systemd Packaging).** Đã xác thực thực địa trực tiếp trên `Fedora 45 (GNOME Shell 51.1, Python 3.15, Antigravity 2.34.0)` với đầy đủ kiểm định tự động (`go vet`, `go test -count=1 -race`, `Python unittest`, `Vitest`, `--self-test`, `--doctor`, `--inspect-ui`, `--export-diagnostics`).

## Chu trình Tự động hóa 5 Bước qua `trycua/cua` (ADR-007..011)

1. **Cô lập trên Dedicated `"Agent Workspace"` & Kiểm tra Quota mỗi 1 phút (`60,000ms`):** Dành riêng không gian **`"Agent Workspace"`** (nhận diện động theo tên trên GNOME 51) cho Antigravity và Chrome OAuth; định kỳ 1 phút mở **`Select model → View Usage`** đọc đúng nhóm hạn mức (`Gemini Models` vs `Claude and GPT models`) rồi đóng gọn `Escape x2`. Tự động tạm hoãn khi màn hình GNOME khóa hoặc ngủ đông.
2. **Xác minh đúng ngưỡng `0%` / `Quota exhausted` & Chụp Evidence trước SignOut:** Chỉ kích hoạt chuyển đổi khi hạn mức về **`0%`** và hiện lỗi **`Quota exhausted`**. Ngay lập tức chụp ảnh màn hình lưu bằng chứng SHA-256 (`01_verify_quota_zero`, `02_pre_signout_settings`), mở **Settings** và bấm **SignOut**.
3. **Lưu Trạng thái Tạm & Chờ phi chặn đúng 1 phút (`60,000ms`):** Ghi nguyên tử tiến độ vào tệp tạm `pending_intent.json` (`0600`) và đếm ngược phi chặn đúng **1 phút** trên Dashboard trước khi bấm **SignIn**.
4. **Khóa cứng Chrome `--profile-directory`, Xác minh Omnibox & Chọn Account:** Chụp bằng chứng (`03_pre_signin_antigravity`), bấm **SignIn**, đảm bảo tiến trình Google Chrome bật đúng `--profile-directory` cấu hình (đối chiếu từ `Local State`), xác minh thanh địa chỉ Omnibox là `accounts.google.com`, chụp bằng chứng (`04_pre_chrome_account_select`) rồi chọn Account đăng nhập sẵn.
5. **Chọn Model Mặc định & Giải phóng Tệp Trạng thái Tạm:** Chụp bằng chứng (`05_pre_model_select`), chọn **Model mặc định** trên Antigravity, **xóa sạch tệp `pending_intent.json`** ngay sau khi hoàn tất và tiếp tục vòng lặp giám sát.

## Tính năng An toàn, Bảo mật & Hiệu năng

- **Giao thức `Verify → Screenshot Evidence → Execute`:** Kiểm tra danh sách trắng tiến trình `/proc/<pid>`, xác minh Omnibox `accounts.google.com` + huy hiệu Chrome Profile, chụp PNG + băm SHA-256 quyền `0600` trước mọi thao tác nhạy cảm và cho phép xem trực tiếp ảnh Evidence trên Dashboard.
- **Nhường quyền tức thời & Tạm dừng trượt 30 giây:** Bất kỳ thao tác vật lý thật nào của con người (di chuột, nhấp chuột, cuộn con lăn, gõ phím) đều ngắt bước đang chạy trong `< 20ms` và đợi **đủ 30 giây tĩnh lặng liên tục** trước khi tiếp tục.
- **Chốt chặn bảo mật cứng (`HardStopChallenge`):** Dừng cứng ngay lập tức nếu phát hiện yêu cầu Mật khẩu, MFA/2FA, CAPTCHA (`-32004`) hoặc sai lệch tên miền/hồ sơ trình duyệt (`-32006`).
- **Tối ưu hiệu năng nhẹ tải:** `SensorFusion` ngắt mạch sớm (*Short-Circuit*), kết nối `AT-SPI2` D-Bus bền vững, bộ đệm `HealthCheck` 10 giây, đọc đuôi nhật ký `audit.jsonl` `O(1)` (`Tail-Seek` 128 KB) và lọc tham số nhạy cảm OAuth (`code=`, `state=`, `login_hint=`).

## Cấu trúc Thư mục

```text
michael-auto/
├── main.go                           # Entrypoint Wails Desktop, CLI (--doctor, --setup-sidecar, --self-test, --headless)
├── app.go                            # Cầu nối IPC Wails <-> Orchestrator, WorkspaceManager, EvidenceVault & Tray
├── docs/
│   ├── ARCHITECTURE.md               # Đặc tả kiến trúc kỹ thuật v2.5.0
│   └── adr/                          # Hồ sơ quyết định kiến trúc ADR-001..011
├── tests/e2e/                        # Kiểm thử E2E toàn stack (Go Orchestrator <-> Python Sidecar thật)
├── internal/
│   ├── domain/                       # AppConfig, CuaDriverConfig, AgentWorkspaceStatus, EvidenceRecord, PendingIntent
│   ├── ports/                        # Hexagonal Go Interfaces (CuaDesktopActuator, WorkspaceManager, EvidenceStore, IntentStore)
│   ├── orchestrator/                 # Non-Blocking FSM, Short-Circuit SensorFusion, HumanYieldController (30s), SafetyBreaker
│   └── adapters/
│       ├── workspace/                # WaylandWorkspaceManager (Fedora 45 GNOME static Agent Workspace #2)
│       ├── activity/                 # Event-driven Wayland Mutter.IdleMonitor + ActuationLease
│       ├── sensors/                  # CLI Config, CUA Driver Sensor (Primary), Persistent AT-SPI2 Fallback
│       ├── actuators/                # Supervised CUA Multiplexer, Chrome Local State Scanner, OSBrowserRouter
│       ├── storage/                  # EvidenceVault (PNG+SHA256), EphemeralIntentStore, Tail-Seek Redacted JSONL
│       └── tray/                     # StatusNotifierItem D-Bus System Tray & Dynamic Status Icons
├── sidecar/
│   ├── embed.go                      # Nhúng toàn bộ sidecar Python vào nhị phân đơn (//go:embed)
│   ├── pyproject.toml                # Cấu hình gói Python michael-auto-cua-sidecar
│   ├── michael_cua_sidecar/          # Gói Python sidecar độc lập
│   │   ├── server.py                 # JSON-RPC 2.0 router trên stdin/stdout
│   │   ├── security.py               # Kiểm tra /proc/<pid>, Omnibox accounts.google.com, Chrome Profile & PNG Gate
│   │   ├── observer.py               # Phân tích ngưỡng 0% / Quota exhausted & quét cổng MFA/CAPTCHA
│   │   ├── actuator.py               # Thực thi 5 cổng Evidence trên Antigravity & Google Chrome
│   │   ├── phrases.py                # Từ điển nút Quota, Settings, Chrome Account Chooser & đa họ Model
│   │   └── errors.py                 # Mã lỗi miền JSON-RPC chuẩn hóa (-32001..-32007)
│   └── tests/
│       └── test_sidecar.py           # Kiểm thử tự động 5 cổng Evidence, chống Phishing Omnibox, sai Profile & MFA
├── frontend/                         # Giao diện React 18 + TypeScript + Vite + Vitest (Pipeline, Scanner, Evidence Viewer)
├── testdata/snapshots/               # Dữ liệu mẫu kiểm thử chống dương tính giả vùng soạn thảo
└── packaging/michael-auto.desktop    # Tệp tích hợp menu & autostart chuẩn XDG cho Fedora 45
```

Xem chi tiết tại [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) và bộ [ADR-001..011](docs/adr/).

## Yêu cầu & Hướng dẫn Biên dịch (Fedora 45 — Linux Wayland)

### 1. Cài đặt gói phụ thuộc hệ thống

```bash
sudo dnf install -y git golang nodejs npm gcc pkgconf-pkg-config \
  gtk3-devel webkit2gtk4.1-devel at-spi2-core python3
```

### 2. Thiết lập tự động Agent Workspace, Python Venv (`trycua/cua`) & Accessibility

```bash
# Cách 1 (Khuyến nghị sau khi build): Tự động ghim Agent Workspace #2, tạo XDG venv (--only-binary) & bật Accessibility
./build/bin/michael-auto --setup-sidecar

# Cách 2 (Chế độ phát triển tại thư mục mã nguồn): Tạo sidecar/.venv và cài gói cục bộ
make sidecar
```

### 3. Kiểm thử & Biên dịch

```bash
# Chạy toàn bộ Unit Test Go, Python Sidecar Test & Vitest Frontend
make test

# Kiểm thử Go với Race Detector
make test-race

# Biên dịch nhị phân gốc cho Fedora 45 Wayland (-tags webkit2_41)
make build

# Chạy riêng bộ E2E toàn stack (Go Orchestrator <-> Python Sidecar + CLI Binary E2E)
make e2e

# Kiểm định toàn trình (Frontend + Go Race + Build + Full-Stack E2E + Python Test + Vitest + Self-Test)
make verify
```

### 4. Chế độ Vận hành

```bash
# Giao diện quản trị Desktop (đóng cửa sổ sẽ thu gọn xuống khay hệ thống)
./build/bin/michael-auto

# Khởi động ẩn vào khay hệ thống
./build/bin/michael-auto --start-hidden

# Chẩn đoán môi trường Fedora 45 Wayland, Agent Workspace, Chrome Profiles & Evidence Vault
./build/bin/michael-auto --doctor

# Quét chỉ-đọc cây phần tử thực tế của Antigravity & Chrome trước khi chạy E2E
./build/bin/michael-auto --inspect-ui

# Tự kiểm định chu trình 5 bước Evidence & tự hủy tệp trạng thái tạm trong môi trường cô lập
./build/bin/michael-auto --self-test

# Chạy dịch vụ điều phối nền không giao diện (Headless Daemon)
./build/bin/michael-auto --headless

# Điều khiển nhanh tiến trình đang chạy qua control.sock
./build/bin/michael-auto --ctl status
./build/bin/michael-auto --ctl pause
./build/bin/michael-auto --ctl resume
./build/bin/michael-auto --ctl quit

# Xuất gói chẩn đoán ẩn danh (.tar.gz 0600, đã che toàn bộ PII/email/token)
./build/bin/michael-auto --export-diagnostics

# Cài đặt user-space (Desktop + Icon + systemd --user service, không cần sudo)
make install-desktop
make install-systemd

# Đóng gói nguồn offline cho Fedora RPM (kèm frontend/dist + Go vendor/) & release tarball
make release-tarball
make rpm-source

# Gỡ cài đặt sạch user-space (GNOME extension, autostart, systemd unit, XDG venv)
./build/bin/michael-auto --uninstall
```

## Tài liệu Pháp lý, Bảo mật & Phát hành

- [CHANGELOG.md](CHANGELOG.md) — Lịch sử phát hành chuẩn Keep a Changelog
- [SECURITY.md](SECURITY.md) — Chính sách bảo mật, cam kết Zero-Credential & quy trình báo cáo lỗ hổng
- [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) — Ghi nhận bản quyền các thư viện mã nguồn mở

## Quyền riêng tư và bảo mật

- **Không sử dụng công cụ ghi hình liên tục:** Đã loại bỏ 100% `Screenpipe`; chỉ chụp ảnh cửa sổ cục bộ làm bằng chứng kiểm toán tại thời điểm chuyển đổi với quyền `0600` và tự động xoay vòng xóa cũ.
- **Không lưu thông tin xác thực thô:** Không đọc `Cookies` hay `Login Data`, không lưu mật khẩu hay OAuth token. Tệp `pending_intent.json` được tự động xóa ngay khi chu trình hoàn tất.
- **Ranh giới dừng cứng (`HardStopChallenge`):** Dừng tự động hóa ngay lập tức khi gặp yêu cầu mật khẩu, MFA/2FA, CAPTCHA hoặc sai lệch tên miền xác thực.

## Giấy phép

Phân phối theo [Giấy phép MIT](LICENSE).

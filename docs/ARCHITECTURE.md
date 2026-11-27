# Đặc tả Kiến trúc Kỹ thuật — Michael Auto v2.5.0 (`Production Ready`: Dedicated `"Agent Workspace"` + Offscreen Zero-Leak Evidence + LockScreen Guard + Dual-Track Fedora Packaging)

Tài liệu này quy định kiến trúc phần mềm, hợp đồng giao diện (Hexagonal Ports & Adapters), máy trạng thái điều phối tái nhập phi chặn (Non-Blocking Re-entrant FSM), cơ chế cô lập **`Agent Workspace`** trên **Fedora 45 Wayland**, giao thức an toàn **`Verify → Screenshot Evidence → Execute`** và phối hợp đa ứng dụng **Antigravity ↔ Google Chrome** qua **`trycua/cua` (`cua-driver==0.58.1`)**.

> **Lưu ý kiến trúc v2.5.0 (Production Ready):** Hệ thống **không sử dụng Screenpipe** và **không cần nối LLM/API Key**. Toàn bộ chức năng quan sát hạn mức (`Select model → View Usage` tách nhóm `Gemini Models` vs `Claude and GPT models`, `max_depth=48`, lá chắn nội dung chat & gạch đầu dòng Markdown đồng bộ cả Python sidecar lẫn Go Tier-2 AT-SPI2) và điều khiển giao diện được thực hiện 100% cục bộ qua **`trycua/cua` (`cua-driver==0.58.1` pin cứng, ưu tiên `embed.FS` SHA-256)** trong không gian dành riêng **`"Agent Workspace"`** (tra cứu động theo tên, chụp Evidence offscreen từ `Meta.WindowActor` không bao giờ lộ Workspace `#1`, bù tỷ lệ HiDPI `MaskPNGEditorRectsWithFrame`, trần dung lượng `150 MB`, tạm hoãn sạch khi GNOME Lock Screen/Suspend, sao lưu cấu hình hỏng, đóng gói Dual-Track `.rpm` + `systemd --user` + `--uninstall` + `--export-diagnostics`) — xem [ADR-007](adr/ADR-007-cua-driver-sidecar-integration.md) đến [ADR-011](adr/ADR-011-production-hardening-agent-workspace-and-dual-track-packaging.md).

---

## 1. Mục tiêu Kiến trúc & Quy trình 5 Bước Cốt lõi

1. **Cô lập Không gian Làm việc Dành Riêng trên Fedora 45 (`"Agent Workspace"`):**
   - Ghim tĩnh Workspace dành riêng mang tên chuẩn **`"Agent Workspace"`** (tra cứu động theo tên qua `ResolveAgentWorkspace`, bảo toàn nguyên vẹn tên các Workspace cá nhân sẵn có của người dùng) để mọi thao tác kiểm tra Quota 1 phút và OAuth Chrome hoàn toàn tách biệt với Workspace làm việc của người dùng.
2. **Chu trình Tự động hóa 5 Bước qua `trycua/cua` (`Verify → Screenshot Evidence → Execute`):**
   - **Bước 1 — Kiểm tra Quota định kỳ 1 phút (`60,000ms`):** Trên Agent Workspace (khi máy tĩnh `>= minIdleBeforeFocusMs`), bấm mở bảng **Quota** của Antigravity, đọc chính xác hạn mức rồi đóng bảng gọn gàng.
   - **Bước 2 — Xác minh `0%` / `Quota exhausted` & SignOut:** Chỉ kích hoạt chuyển đổi khi hạn mức về đúng **`0%`** và báo lỗi **`Quota exhausted`**. Chụp ảnh màn hình lưu bằng chứng (`01_verify_quota_zero`, `02_pre_signout_settings`) trước khi mở **Settings** và bấm **SignOut**.
   - **Bước 3 — Chờ phi chặn đúng 1 phút (`60,000ms`):** Lưu trạng thái tạm vào `pending_intent.json` (`0600`) và đếm ngược phi chặn đúng 1 phút mà không khóa vòng lặp điều phối.
   - **Bước 4 — Xác minh Chrome Profile + Omnibox & Chọn Account:** Chụp bằng chứng trước khi bấm **SignIn** (`03_pre_signin_antigravity`), đảm bảo trình duyệt mở đúng `--profile-directory` (đối chiếu từ `Local State`), kiểm tra Omnibox thuộc đúng `accounts.google.com`, quét không có MFA/CAPTCHA, chụp bằng chứng (`04_pre_chrome_account_select`) rồi click chọn Account đăng nhập sẵn.
   - **Bước 5 — Chọn Model Mặc định & Giải phóng Tệp Trạng thái Tạm:** Chụp bằng chứng (`05_pre_model_select`), chọn **Model mặc định** trên Antigravity, **xóa sạch tệp `pending_intent.json`** ngay lập tức, vào `Cooldown` rồi tiếp tục giám sát.
3. **Nhường Quyền Tức thời Đa Tín hiệu & Tạm dừng Trượt 30 Giây (Multi-Signal Human Yield):**
   - Mọi thao tác vật lý thật từ con người (di chuột, nhấp chuột, lăn chuột, gõ bàn phím) ngoài cửa sổ `ActuationLease` đều kích hoạt ngắt ưu tiên cao (`< 20ms`), tạm dừng cho đến khi đủ **30 giây tĩnh lặng liên tục**, sau đó đối soát tiếp từ đúng vi bước đang thực hiện.
4. **Chốt Chặn Bảo mật Cứng (`HardStopChallenge` & `ErrSecurityViolation`):**
   - Dừng cứng ngay lập tức nếu gặp yêu cầu Mật khẩu, MFA/2FA, CAPTCHA (`-32004`), sai tên miền Omnibox / sai tiến trình `/proc/<pid>` (`-32006`), hoặc thiếu ảnh bằng chứng trước thực thi (`-32007`).

---

## 2. Kiến trúc Phân lớp Lục giác (Hexagonal Architecture)

```text
┌───────────────────────────────────────────────────────────────────────────────────┐
│                 UI Layer (Wails v2 webkit2_41 + React 18 + SNI Tray)              │
│ 5-Step Pipeline │ Agent Workspace Setup │ Chrome Profile Scanner │ Evidence Vault │
└─────────────────────────────────────────┬─────────────────────────────────────────┘
                                          │ Wails Event-Driven IPC
┌─────────────────────────────────────────▼─────────────────────────────────────────┐
│                          Core Domain & Orchestrator (Go)                          │
│  ┌──────────────────────────┐  ┌───────────────────────┐  ┌────────────────────┐  │
│  │ Non-Blocking FSM         │  │ HumanYieldController  │  │ Short-Circuit      │  │
│  │ + Ephemeral PendingIntent│◄─┤ (Interrupt <20ms +    │  │ SensorFusion +     │  │
│  │ + 10s Health Cache       │  │  30s Sliding Quiet)   │  │ SafetyBreaker      │  │
│  └────────────┬─────────────┘  └───────────▲───────────┘  └─────────▲──────────┘  │
└───────▲───────┴─────────────────▲──────────┴─────────────┬──────────┴─────────┬───┘
        │                         │                        │                    │
┌───────┴──────────┐   ┌──────────┴───────────┐  ┌─────────▼──────────┐ ┌───────▼───────────┐
│ Workspace & Idle │   │ Quota & UI Sensors   │  │ CUA & Chrome       │ │ XDG Storage &     │
│ Adapters         │   │ (Short-Circuit)      │  │ Profile Actuators  │ │ Evidence Vault    │
├──────────────────┤   ├──────────────────────┤  ├────────────────────┤ ├───────────────────┤
│ - AgentWorkspace │   │ - Tier 1: CLI Config │  │ - CuaDriverActuator│ │ - EvidenceVault   │
│   (GNOME Static) │   │ - Tier 2a: CUA Driver│  │ - ChromeProfiles   │ │   (PNG + SHA-256) │
│ - Event-Driven   │   │   (Primary Engine)   │  │   (Local State)    │ │ - EphemeralIntent │
│   Mutter Idle    │   │ - Tier 2b: AT-SPI2   │  │ - OSBrowserRouter  │ │ - Tail-Seek JSONL │
└──────────────────┘   └──────────────────────┘  └─────────┬──────────┘ └───────────────────┘
                                                           │ JSON-RPC 2.0 Single-Reader stdio
                                            ┌──────────────▼───────────────────────────┐
                                            │ Python Sidecar (michael_cua_sidecar/)    │
                                            │  ├── server.py   (JSON-RPC 2.0 Router)   │
                                            │  ├── security.py (/proc PID + Omnibox +  │
                                            │  │                Profile + PNG Gate)    │
                                            │  ├── observer.py (0% Quota & Gate Scan)  │
                                            │  └── actuator.py (5-Gate Evidence Exec)  │
                                            └──────────────┬───────────────────────────┘
                                                           │ Native Rust UniFFI (cua-driver)
                                            ┌──────────────▼───────────────────────────┐
                                            │ trycua/cua (Wayland + Agent Workspace)   │
                                            └──────────────────────────────────────────┘
```

---

## 3. Hợp đồng Giao diện Cốt lõi (`internal/ports/ports.go`)

```go
type WorkspaceManager interface {
	Status(ctx context.Context, cfg domain.AppConfig) domain.AgentWorkspaceStatus
	EnsureAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (domain.AgentWorkspaceStatus, error)
	RouteTargetWindowsToAgentWorkspace(ctx context.Context, cfg domain.AppConfig) error
	UserIsOnAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (onAgentWS bool, known bool)
}

type EvidenceStore interface {
	SaveEvidence(runID string, step string, pngBytes []byte, meta domain.EvidenceRecord) (domain.EvidenceRecord, error)
	ListRecent(limit int) ([]domain.EvidenceRecord, error)
	ReadDataURL(imagePath string) (string, error)
}

type IntentStore interface {
	Save(intent domain.PendingIntent) error
	Load() (*domain.PendingIntent, error)
	Clear() error
}
```

---

## 4. Các Cơ chế Kỹ thuật Trọng tâm trong `v2.5.0`

### 4.1. Cổng Kiểm chứng & Lưu Bằng chứng 5 Bước (`Verify → Screenshot Evidence → Execute`)
Mỗi thao tác tự động hóa qua `michael_cua_sidecar` đều trải qua `security.py`:
1. Kiểm tra `/proc/<pid>/exe` & `/proc/<pid>/cmdline` của cửa sổ đích thuộc danh sách tiến trình cho phép (`antigravity`, `chrome`, `chromium`, `brave`, `msedge`, `firefox`).
2. Trên bước chọn tài khoản Chrome (`04_pre_chrome_account_select`), bắt buộc xác minh thanh địa chỉ Omnibox thuộc `accounts.google.com` và huy hiệu hồ sơ khớp với `--profile-directory` mục tiêu.
3. Chụp ảnh PNG cửa sổ đích, đối chiếu chữ ký PNG + tính SHA-256 trong `EvidenceVault` trước khi thực thi click (`01_verify_quota_zero`, `02_pre_signout_settings`, `03_pre_signin_antigravity`, `04_pre_chrome_account_select`, `05_pre_model_select`).

### 4.2. Khóa Cứng `--profile-directory` qua Tệp `Local State`
`DiscoverBrowserProfiles` và `LookupBrowserProfileMeta` (`internal/adapters/actuators/chrome_profiles.go`) đọc an toàn siêu dữ liệu `profile.info_cache` từ tệp `Local State` của trình duyệt để:
- Hỗ trợ nút **"Quét tự động Profile Chrome"** ngay trên giao diện mà không cần gõ tay tên thư mục `Profile 1`, `Profile 2`.
- Cung cấp tên hiển thị chính xác của Profile cho sidecar kiểm chứng; nếu cửa sổ Chrome hiện tại đang mở nhầm Profile khác, `CuaDriverActuator` tự động khởi chạy đúng `--profile-directory` rồi thử lại an toàn.

### 4.3. Hiệu năng Hệ thống & Đóng gói Nhị phân Đơn
- **Đóng gói nhị phân đơn (`sidecar/embed.go`):** Nhúng toàn bộ `pyproject.toml` và `michael_cua_sidecar/*.py` (bao gồm `security.py`), cài đặt wheel nhị phân qua `--only-binary=:all:`.
- **Short-Circuit `SensorFusion` & Kết nối `AT-SPI2` bền vững:** Giảm tải D-Bus khi `cua_driver` hoạt động ổn định.
- **Đọc đuôi tệp `O(1)` (`Tail-Seek` 128 KB) & Lọc OAuth Query:** Giữ độ trễ đọc nhật ký dưới vài mili-giây kể cả khi tệp `audit.jsonl` lên tới `10 MB`, tự động che `code=`, `state=`, `login_hint=`, Bearer token và email.

---

## 5. Lưu trữ Chuẩn XDG & Quyền Riêng tư

- **Tệp cấu hình:** `$XDG_CONFIG_HOME/michael-auto/config.json` (`0600`).
- **Tệp trạng thái chuyển đổi tạm thời:** `$XDG_RUNTIME_DIR/michael-auto/pending_intent.json` (`0600` — **tự động xóa ngay khi hoàn tất**).
- **Kho bằng chứng hình ảnh:** `$XDG_STATE_HOME/michael-auto/evidence/<run_id>/` (`0700`/`0600`, xoay vòng tự động theo `evidenceRetentionRuns`).
- **Nhật ký kiểm toán:** `$XDG_STATE_HOME/michael-auto/audit.jsonl` (`0600`, xoay vòng `10MB x 3`, đọc `Tail-Seek` 128 KB).

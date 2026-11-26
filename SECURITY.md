# Security Policy — Michael Auto

## 1. Cam Kết Bảo Mật Cốt Lõi (Zero-Credential Architecture)

Michael Auto được thiết kế theo nguyên tắc **không lưu trữ thông tin đăng nhập** (*Zero-Credential*):
- **Không bao giờ** lưu, đọc, ghi log hoặc tự điền mật khẩu, cookie phiên, OAuth access token / refresh token, hay mã OTP/2FA.
- Toàn bộ xác thực Google được thực hiện trực tiếp bởi trình duyệt Google Chrome gốc thông qua các hồ sơ cách ly **`1 Tài khoản = 1 --profile-directory`** đã đăng nhập sẵn của chính người dùng.
- **Không cần LLM API key** và **không gửi dữ liệu màn hình ra dịch vụ đám mây bên thứ ba**; mọi phân tích cây phần tử AT-SPI2 và che mờ ảnh bằng chứng chạy 100% cục bộ trên máy.

## 2. Các Hàng Rào Phòng Thủ Chủ Động (Defense-in-Depth)

| Mối đe dọa | Cơ chế chặn cứng trong Michael Auto |
| :--- | :--- |
| **Gặp MFA / 2FA / CAPTCHA / Phishing** | Phát hiện từ khóa/widget xác thực hoặc bất kỳ tên miền Omnibox nào khác `https://accounts.google.com` → chuyển ngay sang trạng thái **`HardStopChallenge`**, không bao giờ tự bấm thử lại. |
| **Chụp nhầm màn hình làm việc (Workspace `#1`)** | Dành riêng **`"Agent Workspace"`**; cầu nối GNOME Shell v4 chụp trực tiếp texture offscreen của `Meta.WindowActor` (`paint_to_content` + `composite_to_stream`) và fail-closed nếu cửa sổ không ở đúng workspace. |
| **Ghi tệp tùy ý qua D-Bus `CaptureWindowPng`** | `WorkspaceBridge` giới hạn đường dẫn đầu ra tuyệt đối bên trong `$XDG_RUNTIME_DIR/michael-auto/*.png`, chặn `..`, chặn thư mục con và từ chối symlink (`NOFOLLOW_SYMLINKS`). |
| **Lộ mã nguồn / hội thoại trong ảnh bằng chứng** | Tự động che vùng soạn thảo (Editor/Chat) bằng khung tối `#0b1120` có bù tỷ lệ HiDPI và offset cửa sổ trước khi tính SHA-256 và ghi quyền `0600` vào Evidence Vault. |
| **Lộ email / OAuth PKCE secret qua log & diagnostics** | `JSONLAuditLogger` & `RedactingWriter` che email (`u***@domain`), Bearer/JWT token và tham số URL OAuth (`code`, `state`, `code_challenge`, `code_verifier`, `login_hint`) trước khi ghi đĩa hoặc xuất `--export-diagnostics`. |
| **Tấn công chuỗi cung ứng gói Python (`cua-driver`)** | Khóa cứng phiên bản `cua-driver==0.58.1` (0 gói phụ thuộc bắc cầu) với `--only-binary=:all:` và `--require-hashes` kiểm chứng SHA-256 (trong `sidecar/cua-driver-0.58.1.requirements.lock`); quá trình tải `uv` dự phòng cũng kiểm chứng SHA-256 nghiêm ngặt. |
| **Tệp cấu hình / socket / intent bị tiến trình khác đọc** | Toàn bộ tệp trong `$XDG_CONFIG_HOME/michael-auto`, `$XDG_STATE_HOME/michael-auto` và `$XDG_RUNTIME_DIR/michael-auto` (`daemon.lock`, `control.sock`, `pending_intent.json`) được tạo với quyền thư mục `0700` và quyền tệp `0600`. |

## 3. Khuyến Nghị Vận Hành An Toàn

1. Luôn giữ **Dry-Run Mode** khi hiệu chuẩn mới (`--inspect-ui` + `--doctor`), chỉ bật **Live Mode** sau khi xác minh đúng các Chrome Profile và `"Agent Workspace"`.
2. Sau khi cài hoặc nâng cấp GNOME Shell Extension v4, đăng xuất/đăng nhập GNOME 1 lần để nạp cơ chế chụp offscreen & `ArmAutoRoute` mới nhất vào `gnome-shell`.
3. Kiểm tra định kỳ bằng `./build/bin/michael-auto --doctor`; sử dụng `--uninstall` nếu muốn gỡ bỏ hoàn toàn extension, autostart, systemd unit và Python venv user-space.

## 4. Báo Cáo Lỗ Hổng Bảo Mật

Nếu bạn phát hiện lỗ hổng bảo mật (ví dụ: rò rỉ đường dẫn D-Bus, bypass kiểm tra Omnibox, rò rỉ PII trong nhật ký hoặc gói diagnostics):
- **Không** đăng khai thác công khai trên Issue trước khi có bản vá.
- Gửi báo cáo chi tiết (phiên bản `michael-auto --version`, môi trường Fedora/GNOME, bước tái hiện, và gói `--export-diagnostics` nếu phù hợp) tới nhóm duy trì dự án qua kênh bảo mật riêng (GitHub Security Advisories của kho lưu trữ).
- Chúng tôi cam kết phản hồi xác nhận trong vòng **48 giờ** và phát hành bản vá khẩn cho mọi lỗ hổng mức P0/P1.

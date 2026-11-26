# ADR-004: Cô lập Hồ sơ Trình duyệt, Chuỗi Fallback Model & Ranh giới Dừng cứng Xác thực

- **Trạng thái:** Đã chấp thuận (Accepted)
- **Ngày:** 2026-11-24

## Bối cảnh

Việc dùng OCR/tọa độ để tìm và nhấp vào hình đại diện (avatar) tài khoản Google trên một trình duyệt đăng nhập chung nhiều tài khoản rất dễ thất bại khi thứ tự tài khoản thay đổi hoặc cookie bị xung đột. Ngoài ra, việc chuyển đổi tài khoản tốn thời gian hơn so với chuyển sang model dự phòng ngay trên cùng tài khoản, và luồng OAuth có thể phát sinh yêu cầu xác thực bảo mật bất kỳ lúc nào.

## Quyết định

1. **Cô lập Hồ sơ Trình duyệt (`Browser Profile Isolation`):** Mỗi hồ sơ tài khoản trong Michael Auto được ánh xạ cố định tới một thư mục hồ sơ trình duyệt riêng biệt (`--profile-directory` trên Chromium/Chrome/Brave hoặc `-P` trên Firefox), nơi người dùng đã đăng nhập sẵn duy nhất tài khoản tương ứng.
2. **Chính sách Ưu tiên Model (`Ordered Model Policy`):** Mỗi hồ sơ cấu hình một `primary_model` và danh sách `fallback_models` có thứ tự. Tùy theo cấu hình chính sách của người dùng, hệ thống có thể ưu tiên hạ xuống model dự phòng trên cùng hồ sơ trước khi chuyển sang hồ sơ tài khoản kế tiếp, và tự động khôi phục `primary_model` sau khi chuyển đổi thành công.
3. **Ranh giới Dừng cứng Không Vượt qua (`HardStopChallenge`):** Nếu bất kỳ bước nào gặp màn hình yêu cầu mật khẩu, MFA/2FA, CAPTCHA hoặc kiểm tra chống bot, hệ thống bắt buộc dừng cứng ngay lập tức, chuyển trạng thái khay hệ thống sang cảnh báo và chờ người dùng tự hoàn tất xác thực trên giao diện gốc. Không can thiệp vào cơ sở dữ liệu nội bộ (SQLite/LevelDB) của ứng dụng đang mở.

## Hệ quả

- Loại bỏ lỗi chọn nhầm tài khoản Google và giữ ranh giới bảo mật/tuân thủ rõ ràng.

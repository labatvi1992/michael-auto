# ADR-005: Circuit Breaker Chống Lặp Vô tận, Lưu trữ Khóa & Kiểm thử Replay

- **Trạng thái:** Đã chấp thuận (Accepted)
- **Ngày:** 2026-11-24

## Bối cảnh

Trong hệ thống vận hành tự động không có người bấm xác nhận, nếu tất cả các hồ sơ tài khoản đều đã cạn hạn mức hoặc giao diện ứng dụng mục tiêu thay đổi sau một bản cập nhật, hệ thống có nguy cơ rơi vào vòng lặp đăng xuất/đăng nhập liên tục không điểm dừng.

## Quyết định

1. **All-Accounts-Exhausted Guard & Switch Budget:** Ghi nhận trạng thái chạm ngưỡng theo từng hồ sơ; nếu toàn bộ hồ sơ trong danh sách đều đang hết hạn mức hoặc số lần chuyển đổi vượt ngân sách cho phép (mặc định tối đa `3 lần / 30 phút`), mở **Circuit Breaker** để tạm dừng thực thi và chỉ duy trì quan sát nền.
2. **Zero-Secret Config & XDG Compliance:** Cấu hình lưu tại `$XDG_CONFIG_HOME/michael-auto/config.json` (quyền `0600`) không chứa mật khẩu, cookie hay OAuth token; khóa phiên hệ thống (`restore_token`) lưu trong `libsecret`; nhật ký kiểm toán JSONL tại `$XDG_STATE_HOME/michael-auto/audit.jsonl` tự động che thông tin định danh nhạy cảm và xoay vòng dung lượng.
3. **Kiểm thử Tự động Không Phụ thuộc Màn hình Thật:** Đạt tối thiểu `80%` độ phủ Unit Test cho `FSM`, `HumanYieldController (30s)` và `CircuitBreaker` thông qua Mock Ports và bộ dữ liệu `testdata/snapshots/` để chạy kiểm thử hồi quy trên CI không cần giao diện đồ họa.

## Hệ quả

- Hệ thống tự bảo vệ an toàn trước các kịch bản cạn sạch hạn mức hoặc thay đổi phiên bản giao diện, đồng thời kiểm thử được hoàn toàn tự động trên CI/CD.

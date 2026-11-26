# ADR-001: Chiến lược Cảm biến Hạn mức Phân tầng qua `trycua/cua` & Chống Dương tính Giả

- **Trạng thái:** Đã cập nhật (Updated — Loại bỏ hoàn toàn Screenpipe, chuyển sang `trycua/cua`)
- **Ngày cập nhật:** 2026-11-25

## Bối cảnh

Hệ thống chạy ở chế độ thực thi tự động không cần bấm nút xác nhận (`Zero-Prompt Unattended Execution`). Việc sử dụng công cụ ghi hình/OCR toàn thời gian như `Screenpipe` gây tốn tài nguyên nền, phức tạp hóa quản lý dữ liệu ghi hình và không hỗ trợ điều khiển tương tác đa cửa sổ chính xác giữa Antigravity và trình duyệt Google Chrome.

## Quyết định

Loại bỏ hoàn toàn `Screenpipe` và chuẩn hóa kiến trúc cảm biến thành 2 tầng gọn nhẹ phía sau giao diện `ports.StateSensor`:

1. **Tier 1 — Cấu hình / Tín hiệu Chính thức (`cli_config`):** Đọc và đồng bộ nguyên tử tệp cấu hình chính thức (`~/.gemini/antigravity-cli/settings.json`).
2. **Tier 2 — Động cơ Chính `trycua/cua` (`cua_driver` qua `michael_cua_sidecar`):** Sử dụng SDK gốc `cua-driver` ở chế độ định danh ngữ nghĩa (`tree_format="elements"`, `element_token`) để tự động Focus cửa sổ Antigravity (khi người dùng tĩnh `>= 5s`), bấm mở nút xem Quota theo giãn cách mặc định **1 phút**, đọc chỉ số hạn mức rồi đóng bảng Quota gọn gàng, đồng thời loại trừ hoàn toàn văn bản nằm trong khung soạn thảo mã nguồn/tài liệu (`EDITOR_ROLES`).
3. **Dự phòng Nội bộ (`atspi2`):** Giữ bộ đọc `AT-SPI2` thuần Go làm kênh quan sát chỉ-đọc dự phòng trong trường hợp máy người dùng chưa cài gói Python `cua-driver`.

## Hệ quả

- Loại bỏ 100% phụ thuộc và tiến trình nền `Screenpipe`.
- Độ trễ kiểm tra Quota mỗi phút `< 120ms`, chi phí token LLM bằng `0`, dữ liệu xử lý hoàn toàn cục bộ trên máy.

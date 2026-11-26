# ADR-006: Quản lý Vòng đời Screenpipe Đi kèm (Đã Bãi bỏ / Thay thế bởi ADR-007)

- **Trạng thái:** Đã thay thế bởi [ADR-007](ADR-007-cua-driver-sidecar-integration.md) (*Superseded — Không còn sử dụng Screenpipe*)
- **Ngày cập nhật:** 2026-11-25

## Lý do Bãi bỏ

Theo định hướng kiến trúc v2.0, `Michael Auto` **loại bỏ hoàn toàn việc sử dụng Screenpipe** (cả ở tầng cảm biến HTTP OCR, cấu hình `AppConfig`, giao diện người dùng lẫn sidecar nền) để chuyển hoàn toàn sang **`trycua/cua` (`cua-driver`)** thông qua gói sidecar nội bộ `sidecar/michael_cua_sidecar/`.

Các nguyên tắc kỹ thuật tốt về an toàn vòng đời tiến trình con trên Linux (`Pdeathsig: syscall.SIGTERM`, `Exponential Backoff` và `Sidecar Circuit Breaker`) đã được kế thừa và nâng cấp trực tiếp bên trong `CuaDriverActuator` ([ADR-007](ADR-007-cua-driver-sidecar-integration.md)).

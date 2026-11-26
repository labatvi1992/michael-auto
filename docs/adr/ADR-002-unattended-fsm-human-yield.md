# ADR-002: Vận hành Tự động Không Xác nhận, Ngắt Đa Tín hiệu & Bộ đếm Tĩnh lặng Trượt 30 Giây

- **Trạng thái:** Đã chấp thuận (Accepted)
- **Ngày:** 2026-11-24

## Bối cảnh

Người dùng yêu cầu hệ thống tự động thực hiện luồng chuyển đổi mà không cần bấm xác nhận thủ công, nhưng tuyệt đối không được tranh chấp quyền điều khiển khi con người đang sử dụng máy tính. Nếu chỉ dùng hàm ngủ (`sleep`) tuần tự hoặc chỉ theo dõi mỗi di chuyển chuột mà bỏ qua bàn phím/cuộn chuột, hệ thống sẽ cướp tiêu điểm khi người dùng đang gõ phím, hoặc click nhầm vào giao diện đã thay đổi sau khi hết thời gian tạm dừng.

## Quyết định

1. **Zero-Prompt Trigger:** Thực thi ngay khi vượt qua `PreFlightCheck` (`Confidence >= 0.95` và máy đang ở trạng thái tĩnh).
2. **Ngắt Đa Tín hiệu (`< 20ms`):** Mọi thao tác vật lý của người dùng — gồm **di chuyển chuột (`> 3px` deadzone), nhấp chuột, lăn con lăn chuột và gõ bàn phím** — đều lập tức hủy `context.Context` của vi bước đang thực thi và chuyển FSM sang trạng thái `PausedHumanInput`.
3. **Cửa sổ Tĩnh lặng Trượt 30 Giây (`30s Sliding Quiet Window`):** Bộ đếm 30 giây tự động đặt lại mỗi khi phát sinh thêm bất kỳ thao tác chuột, cuộn chuột hoặc bàn phím nào. Hệ thống chỉ rời trạng thái tạm dừng khi Wayland Compositor xác nhận thời gian nhàn rỗi liên tục đạt `>= 30 giây`.
4. **Đối soát Lại Từ đầu (`Idempotent State Reconciliation`):** Hết 30 giây tĩnh lặng, hệ thống luôn quay về trạng thái `Observing` (không chạy tiếp dòng lệnh dở dang trước đó), đọc lại trạng thái màn hình thực tế và đối chiếu với `PendingIntent` (có TTL) để quyết định bước hợp lệ tiếp theo.

## Hệ quả

- Trải nghiệm tự động liền mạch khi người dùng rời tay, đồng thời nhường quyền tức thì và không bao giờ gây gián đoạn khi người dùng đang gõ code hoặc điều khiển chuột.
- Loại bỏ hoàn toàn lỗi click sai vị trí sau khi người dùng đã thay đổi cửa sổ trong thời gian tạm dừng 30 giây.

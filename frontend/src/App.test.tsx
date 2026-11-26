import "@testing-library/jest-dom";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { backendBridge } from "./bridge";

describe("Michael Auto Desktop Dashboard v2.5 Production", () => {
  beforeEach(async () => {
    const cfg = await backendBridge.getConfig();
    cfg.automationEnabled = true;
    cfg.dryRunActuation = true;
    cfg.cuaDriver.agentWorkspaceName = "Agent Workspace";
    cfg.profiles[0].enabled = true;
    cfg.profiles[1].enabled = true;
    await backendBridge.saveConfig(cfg);
    backendBridge.__setMockStatusPatch({
      state: "Observing",
      automationEnabled: true,
      dryRunActuation: true,
      circuitBreakerTripped: false,
      hardStopReason: undefined,
      screenLocked: false,
      configWarning: undefined
    });
  });

  it("renders Dry-Run Live Mode banner, 5-step pipeline, canonical Agent Workspace, and 30s yield banner on human input", async () => {
    render(<App />);

    await waitFor(() => {
      expect(screen.getByTestId("fsm-state-pill")).toHaveTextContent("Đang quan sát định kỳ");
    });

    expect(screen.getByTestId("dryrun-mode-banner")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Bật Chế độ Thực thi Thật/i }));
    await waitFor(() => {
      expect(screen.queryByTestId("dryrun-mode-banner")).not.toBeInTheDocument();
    });

    expect(screen.getByTestId("cua-pipeline-stepper")).toHaveTextContent("① Kiểm tra Quota (1m)");
    expect(screen.getByTestId("agent-workspace-item")).toHaveTextContent("Workspace #2: Agent Workspace");

    fireEvent.click(screen.getByRole("button", { name: /Thiết lập Agent Workspace/i }));
    expect(await screen.findByRole("status")).toHaveTextContent(/Agent Workspace/i);

    const simMouseBtn = screen.getByRole("button", { name: /Giả lập Di Chuột/i });
    fireEvent.click(simMouseBtn);

    await waitFor(() => {
      expect(screen.getByTestId("yield-countdown-banner")).toBeInTheDocument();
      expect(screen.getByTestId("fsm-state-pill")).toHaveTextContent("Tạm dừng 30s");
    });
  });

  it("displays Hard-Stop banner on MFA/CAPTCHA challenge and recovers on reset", async () => {
    render(<App />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /Giả lập Gặp MFA/i })).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole("button", { name: /Giả lập Gặp MFA/i }));

    await waitFor(() => {
      expect(screen.getByTestId("hardstop-banner")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole("button", { name: /Đặt lại & Tiếp tục Quan sát/i }));

    await waitFor(() => {
      expect(screen.queryByTestId("hardstop-banner")).not.toBeInTheDocument();
      expect(screen.getByTestId("fsm-state-pill")).toHaveTextContent("Đang quan sát định kỳ");
    });
  });

  it("supports Chrome profile scanner, live Antigravity model discovery, Autostart toggle, Diagnostics export, and Evidence preview", async () => {
    render(<App />);

    await waitFor(() => {
      expect(screen.getByText(/Workspace Chính/i)).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole("button", { name: /Hồ sơ Tài khoản & Model/i }));
    expect(await screen.findByText(/Quản lý Hồ sơ Tài khoản/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Quét tự động Profile Chrome/i }));
    expect(await screen.findByRole("status")).toHaveTextContent(/Đã quét 2 Profile trình duyệt/i);

    fireEvent.click(screen.getByRole("button", { name: /Quét Model từ Antigravity/i }));
    expect(await screen.findByTestId("discovered-models-bar")).toHaveTextContent(/Gemini 4 Neon Extra High/i);

    fireEvent.click(screen.getByRole("button", { name: /Cấu hình Điều phối & Wayland/i }));
    expect(await screen.findByText(/Tự động che mờ vùng khung soạn thảo Code\/Chat/i)).toBeInTheDocument();
    expect(screen.queryByText(/Screenpipe/i)).not.toBeInTheDocument();

    const autostartCheckbox = screen.getByTestId("autostart-toggle") as HTMLInputElement;
    expect(autostartCheckbox.checked).toBe(false);
    fireEvent.click(autostartCheckbox);
    await waitFor(() => {
      expect(autostartCheckbox.checked).toBe(true);
    });

    fireEvent.click(screen.getByRole("button", { name: /Thử Focus Antigravity/i }));
    expect(await screen.findByRole("status")).toHaveTextContent(/focus_antigravity/i);

    fireEvent.click(screen.getByRole("button", { name: /Quét Cây Phần Tử Thật/i }));
    expect(await screen.findByTestId("ui-inspect-report-box")).toHaveTextContent(/quota_button/i);

    fireEvent.click(screen.getByRole("button", { name: /Nhật ký & Evidence/i }));
    expect(await screen.findByTestId("evidence-vault-card")).toBeInTheDocument();

    fireEvent.click(screen.getByTestId("export-diagnostics-btn"));
    expect(await screen.findByRole("status")).toHaveTextContent(/michael-auto-diagnostics/i);

    fireEvent.click(screen.getByRole("button", { name: /Xem ảnh Evidence/i }));
    const modal = await screen.findByTestId("evidence-preview-modal");
    expect(modal).toHaveTextContent(/01_verify_quota_zero/i);

    fireEvent.click(screen.getByRole("button", { name: /Xóa kho Evidence/i }));
    await waitFor(() => {
      expect(screen.queryByTestId("evidence-preview-modal")).not.toBeInTheDocument();
      expect(screen.getByText(/Chưa có ảnh bằng chứng nào được ghi nhận/i)).toBeInTheDocument();
    });
  });

  it("protects unsaved form edits from background config:updated events (P2-1 Dirty-Form Guard)", async () => {
    render(<App />);

    await waitFor(() => {
      expect(screen.getByTestId("fsm-state-pill")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole("button", { name: /Hồ sơ Tài khoản & Model/i }));
    const nameInputs = await screen.findAllByDisplayValue("Workspace Chính");
    fireEvent.change(nameInputs[0], { target: { value: "My Unsaved Custom Name" } });

    expect(await screen.findByTestId("unsaved-changes-banner")).toBeInTheDocument();

    // Backend pushes a background config update (e.g. FSM updating ExhaustedUntil) while user is typing!
    const liveCfg = await backendBridge.getConfig();
    liveCfg.profiles[0].name = "Backend Pushed Name";
    act(() => {
      backendBridge.__emitMockConfigUpdate(liveCfg);
    });

    // User's unsaved edit MUST NOT be overwritten!
    expect(screen.getByDisplayValue("My Unsaved Custom Name")).toBeInTheDocument();

    // Clicking Discard restores serverConfig cleanly and clears the dirty banner.
    fireEvent.click(screen.getByRole("button", { name: /Hủy bỏ/i }));
    await waitFor(() => {
      expect(screen.queryByTestId("unsaved-changes-banner")).not.toBeInTheDocument();
      expect(screen.getByDisplayValue("Backend Pushed Name")).toBeInTheDocument();
    });
  });

  it("displays Onboarding 3-Step Checklist and Low-Profile warning when fewer than 2 profiles are enabled", async () => {
    const cfg = await backendBridge.getConfig();
    cfg.profiles[1].enabled = false;
    await backendBridge.saveConfig(cfg);

    render(<App />);

    expect(await screen.findByTestId("low-profile-warning")).toHaveTextContent(/Hiện chỉ bật 1 hồ sơ Chrome/i);
    expect(screen.getByTestId("onboarding-checklist")).toHaveTextContent(/Danh mục Khởi tạo Production/i);
  });

  it("displays Screen-Locked banner and Config-Warning recovery banner when reported by backend", async () => {
    backendBridge.__setMockStatusPatch({
      screenLocked: true,
      configWarning: "Cấu hình cũ hỏng đã được sao lưu tại ~/.config/michael-auto/config.json.corrupt.20261126"
    });

    render(<App />);

    expect(await screen.findByTestId("screen-locked-banner")).toHaveTextContent(/Màn hình GNOME đang khóa/i);
    expect(screen.getByTestId("config-warning-banner")).toHaveTextContent(/config.json.corrupt/i);
  });

  it("allows collapsing and expanding the Developer Sandbox panel safely", async () => {
    render(<App />);

    const toggleBtn = await screen.findByTestId("sandbox-panel-toggle");
    expect(screen.getByTestId("sandbox-panel-body")).toBeInTheDocument();

    fireEvent.click(toggleBtn);
    expect(screen.queryByTestId("sandbox-panel-body")).not.toBeInTheDocument();

    fireEvent.click(toggleBtn);
    expect(screen.getByTestId("sandbox-panel-body")).toBeInTheDocument();
  });
});

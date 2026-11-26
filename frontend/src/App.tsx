import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { backendBridge } from "./bridge";
import {
  AccountProfile,
  AppConfig,
  AuditEntry,
  BrowserType,
  EvidenceRecord,
  FSMState,
  RuntimeStatus
} from "./types";

type TabId = "overview" | "profiles" | "settings" | "logs";

const STATE_LABELS: Record<FSMState, string> = {
  Observing: "Đang quan sát định kỳ",
  PreFlightCheck: "Kiểm tra an toàn trước thực thi",
  AutoExecuting: "Đang tự động điều phối",
  PausedHumanInput: "Tạm dừng 30s (Người dùng thao tác)",
  VerifyingState: "Đang thực thi chu trình / Chờ phiên",
  Cooldown: "Nghỉ ổn định sau chuyển đổi",
  HardStopChallenge: "Dừng cứng bảo vệ (MFA / CAPTCHA / Hạn mức)",
  PausedByUser: "Tạm tắt bởi người dùng"
};

function computePipelineStage(status: RuntimeStatus): number {
  const sub = status.pendingIntent?.subStep;
  if (status.state === "Cooldown" || sub === "verify_and_select_model") return 5;
  if (sub === "click_signin" || sub === "select_chrome_account") return 4;
  if (sub === "wait_before_signin" || (status.intentWaitRemainingMs ?? 0) > 0) return 3;
  if (sub === "signout_settings" || status.state === "PreFlightCheck" || status.state === "AutoExecuting") return 2;
  if (status.state === "VerifyingState" && status.pendingIntent) return 4;
  return 1;
}

export function App(): React.ReactElement {
  const [activeTab, setActiveTab] = useState<TabId>("overview");
  const [status, setStatus] = useState<RuntimeStatus | null>(null);
  const [serverConfig, setServerConfig] = useState<AppConfig | null>(null);
  const [config, setConfig] = useState<AppConfig | null>(null);
  const [autostartEnabled, setAutostartEnabled] = useState<boolean>(false);
  const [discoveredModels, setDiscoveredModels] = useState<string[]>([]);
  const [showSandbox, setShowSandbox] = useState<boolean>(() => !backendBridge.isDesktopRuntime());
  const [logs, setLogs] = useState<AuditEntry[]>([]);
  const [evidence, setEvidence] = useState<EvidenceRecord[]>([]);
  const [previewEvidence, setPreviewEvidence] = useState<{
    record: EvidenceRecord;
    dataUrl: string;
  } | null>(null);
  const [logFilter, setLogFilter] = useState<"ALL" | "INFO" | "WARN" | "ERROR">("ALL");
  const [uiInspectReport, setUiInspectReport] = useState<string>("");
  const [notice, setNotice] = useState<string>("");

  const isDirty = useMemo(() => {
    if (!config || !serverConfig) return false;
    return JSON.stringify(config) !== JSON.stringify(serverConfig);
  }, [config, serverConfig]);

  const isDirtyRef = useRef<boolean>(false);
  useEffect(() => {
    isDirtyRef.current = isDirty;
  }, [isDirty]);

  const refreshAll = useCallback(async (forceSyncDraft = false) => {
    const [st, cfg, audit, ev, autoStart] = await Promise.all([
      backendBridge.getStatus(),
      backendBridge.getConfig(),
      backendBridge.getAuditLogs(120),
      backendBridge.listRecentEvidence(30),
      backendBridge.getAutostartEnabled()
    ]);
    setStatus(st);
    setServerConfig(cfg);
    if (forceSyncDraft || !isDirtyRef.current) {
      setConfig(cfg);
    }
    setLogs(audit);
    setEvidence(ev);
    setAutostartEnabled(autoStart);
  }, []);

  useEffect(() => {
    void refreshAll(true);
    const unsubStatus = backendBridge.subscribeStatus((liveStatus) => {
      setStatus(liveStatus);
    });
    const unsubConfig = backendBridge.subscribeConfig((liveCfg) => {
      setServerConfig(liveCfg);
      // P2-1 Dirty-Form Guard: never overwrite user's half-edited Profiles/Settings draft!
      if (!isDirtyRef.current) {
        setConfig(liveCfg);
      }
    });
    const pollMs = backendBridge.isDesktopRuntime() ? 5000 : 1000;
    const timer = window.setInterval(async () => {
      const st = await backendBridge.getStatus();
      setStatus(st);
      if (activeTab === "logs") {
        const [latest, latestEv] = await Promise.all([
          backendBridge.getAuditLogs(120),
          backendBridge.listRecentEvidence(30)
        ]);
        setLogs(latest);
        setEvidence(latestEv);
      }
    }, pollMs);
    return () => {
      unsubStatus();
      unsubConfig();
      window.clearInterval(timer);
    };
  }, [refreshAll, activeTab]);

  const showToast = (msg: string) => {
    setNotice(msg);
    window.setTimeout(() => setNotice(""), 4200);
  };

  const handleToggleAutomation = async () => {
    if (!status) return;
    const next = !status.automationEnabled;
    await backendBridge.setAutomationEnabled(next);
    await refreshAll(false);
    showToast(next ? "Đã bật chế độ tự động điều phối" : "Đã tạm tắt điều phối tự động");
  };

  const handleResetHardStop = async () => {
    await backendBridge.resetHardStop();
    await refreshAll(false);
    showToast("Đã đặt lại chốt an toàn & bộ ngắt sidecar — hệ thống quay về quan sát định kỳ");
  };

  const handleSaveConfig = async (e?: React.FormEvent) => {
    if (e) e.preventDefault();
    if (!config) return;
    try {
      await backendBridge.saveConfig(config);
      await refreshAll(true);
      showToast("Đã lưu cấu hình an toàn (XDG 0600)");
    } catch (err) {
      showToast(`Lỗi lưu cấu hình: ${String(err)}`);
    }
  };

  const handleDiscardDraft = () => {
    if (!serverConfig) return;
    setConfig(structuredClone(serverConfig));
    showToast("Đã hủy các thay đổi chưa lưu");
  };

  const handleAddProfile = () => {
    if (!config) return;
    const idx = config.profiles.length + 1;
    const newProf: AccountProfile = {
      id: `profile-${Date.now()}`,
      name: `Workspace ${idx}`,
      emailHint: `account${idx}@example.com`,
      browserType: "chrome",
      browserProfileDir: `Profile ${idx}`,
      primaryModel: "Gemini 4 Neon Extra High",
      fallbackModels: ["Claude Sonnet 4.6 (Thinking)"],
      enabled: true,
      priority: idx
    };
    setConfig({ ...config, profiles: [...config.profiles, newProf] });
  };

  const handleDiscoverProfiles = async () => {
    if (!config) return;
    try {
      const discovered = await backendBridge.discoverBrowserProfiles();
      if (discovered.length === 0) {
        showToast("Không tìm thấy hồ sơ Chrome/Chromium/Brave/Edge nào trong Local State");
        return;
      }
      const existingKeys = new Set(
        config.profiles.map((p) => `${p.browserType}::${p.browserProfileDir.toLowerCase()}`)
      );
      const nextProfiles = [...config.profiles];
      let added = 0;
      let updated = 0;
      for (const item of discovered) {
        const key = `${item.browserType}::${item.profileDir.toLowerCase()}`;
        const existingIdx = nextProfiles.findIndex(
          (p) => `${p.browserType}::${p.browserProfileDir.toLowerCase()}` === key
        );
        if (existingIdx >= 0) {
          if (item.emailHint && (!nextProfiles[existingIdx].emailHint || nextProfiles[existingIdx].emailHint.includes("*"))) {
            nextProfiles[existingIdx] = {
              ...nextProfiles[existingIdx],
              emailHint: item.emailHint,
              name: item.displayName || nextProfiles[existingIdx].name
            };
            updated++;
          }
          continue;
        }
        if (!existingKeys.has(key)) {
          existingKeys.add(key);
          const priority = nextProfiles.length + 1;
          nextProfiles.push({
            id: `profile-${item.browserType}-${item.profileDir.toLowerCase().replace(/\s+/g, "-")}-${Date.now() + added}`,
            name: item.displayName || `${item.browserType.toUpperCase()} (${item.profileDir})`,
            emailHint: item.emailHint || "",
            browserType: item.browserType,
            browserProfileDir: item.profileDir,
            primaryModel: "Gemini 4 Neon Extra High",
            fallbackModels: ["Claude Sonnet 4.6 (Thinking)"],
            enabled: true,
            priority
          });
          added++;
        }
      }
      setConfig({ ...config, profiles: nextProfiles });
      showToast(
        `Đã quét ${discovered.length} Profile trình duyệt (thêm mới ${added}, cập nhật ${updated}). Bấm 'Lưu Thay đổi' để áp dụng.`
      );
    } catch (err) {
      showToast(`Lỗi quét Profile Chrome: ${String(err)}`);
    }
  };

  const updateProfile = (index: number, patch: Partial<AccountProfile>) => {
    if (!config) return;
    const next = config.profiles.map((p, i) => (i === index ? { ...p, ...patch } : p));
    setConfig({ ...config, profiles: next });
  };

  const removeProfile = (index: number) => {
    if (!config || config.profiles.length <= 1) {
      showToast("Cần giữ lại ít nhất 1 hồ sơ tài khoản");
      return;
    }
    const next = config.profiles.filter((_, i) => i !== index);
    setConfig({ ...config, profiles: next });
  };

  if (!status || !config) {
    return (
      <div className="app-shell">
        <div className="card">Đang kết nối bộ điều phối Michael Auto (trycua/cua)...</div>
      </div>
    );
  }

  const yieldPct =
    status.yieldTotalMs > 0
      ? Math.min(100, Math.max(0, (status.yieldRemainingMs / status.yieldTotalMs) * 100))
      : 0;

  const dotColor =
    status.state === "Observing"
      ? "#22c55e"
      : status.state === "PausedHumanInput"
      ? "#f59e0b"
      : status.state === "HardStopChallenge"
      ? "#ef4444"
      : status.state === "PausedByUser"
      ? "#94a3b8"
      : "#3b82f6";

  const filteredLogs = logs.filter((entry) => logFilter === "ALL" || entry.level === logFilter);
  const pipelineStage = computePipelineStage(status);
  const agentWs = status.agentWorkspace;
  const enabledProfilesCount = (serverConfig ?? config).profiles.filter((p) => p.enabled).length;
  const needsOnboarding = !status.waylandPortalReady || !agentWs?.ready || enabledProfilesCount < 2;

  const pipelineSteps = [
    {
      num: 1,
      title: "① Kiểm tra Quota (1m)",
      desc: "Tự động kiểm tra trên Agent Workspace mỗi 60s"
    },
    {
      num: 2,
      title: "② Verify 0% & SignOut",
      desc: "Chụp ảnh Evidence khi về 0% / Quota exhausted rồi SignOut"
    },
    {
      num: 3,
      title: "③ Chờ 1 Phút",
      desc: "Đếm ngược 60s không chặn vòng lặp điều phối"
    },
    {
      num: 4,
      title: "④ Verify Chrome Profile & Account",
      desc: "Khớp --profile-directory, Omnibox accounts.google.com & chụp Evidence"
    },
    {
      num: 5,
      title: "⑤ Chọn Model Mặc định",
      desc: "Xác nhận Model trên Antigravity & xóa tệp trạng thái tạm"
    }
  ];

  return (
    <div className="app-shell">
      <header className="topbar">
        <div className="brand-group">
          <span className="brand-dot" style={{ color: dotColor, backgroundColor: dotColor }} />
          <div>
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <h1 className="brand-title">Michael Auto</h1>
              <span
                data-testid="app-version-badge"
                style={{
                  fontSize: "0.72rem",
                  fontWeight: 600,
                  padding: "2px 7px",
                  borderRadius: 999,
                  background: "rgba(59, 130, 246, 0.16)",
                  border: "1px solid rgba(59, 130, 246, 0.35)",
                  color: "#93c5fd"
                }}
              >
                v{status.appVersion || "2.5.0"}
              </span>
            </div>
            <p className="brand-subtitle">
              Điều phối Hạn mức Tự động qua <strong>trycua/cua</strong> • Nền tảng: <strong>{status.displayServer.toUpperCase()}</strong>
              {agentWs?.enabled ? ` • Agent Workspace #${(agentWs.workspaceIndex ?? 1) + 1}` : ""}
              {status.dryRunActuation ? " • [Chế độ Dry-Run An toàn]" : ""}
            </p>
          </div>
        </div>

        <div className="topbar-actions">
          <span className={`state-pill state-${status.state}`} data-testid="fsm-state-pill">
            {STATE_LABELS[status.state] ?? status.state}
          </span>
          <button type="button" className="btn btn-primary" onClick={handleToggleAutomation}>
            {status.automationEnabled ? "Tạm tắt Tự động" : "Bật Tự động"}
          </button>
        </div>
      </header>

      {notice && <div className="toast-banner" role="status">{notice}</div>}

      {isDirty && (
        <section className="yield-banner" data-testid="unsaved-changes-banner">
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
            <div>
              <strong>✏️ Bạn có thay đổi cấu hình chưa lưu</strong>
              <div style={{ fontSize: "0.84rem", color: "#fde68a", marginTop: 3 }}>
                Các cập nhật trạng thái nền sẽ không ghi đè biểu mẫu đang chỉnh sửa cho tới khi bạn bấm Lưu hoặc Hủy.
              </div>
            </div>
            <div style={{ display: "flex", gap: 8 }}>
              <button type="button" className="btn" onClick={handleDiscardDraft}>
                Hủy bỏ
              </button>
              <button type="button" className="btn btn-primary" onClick={() => void handleSaveConfig()}>
                Lưu Thay đổi Ngay
              </button>
            </div>
          </div>
        </section>
      )}

      {status.configWarning && (
        <section className="alert-hardstop" data-testid="config-warning-banner">
          <div>
            <strong>⚠️ Đã khôi phục cấu hình mặc định từ tệp config.json lỗi</strong>
            <div style={{ fontSize: "0.84rem", color: "#fecaca", marginTop: 3 }}>
              {status.configWarning}
            </div>
          </div>
        </section>
      )}

      {status.screenLocked && (
        <section className="yield-banner" data-testid="screen-locked-banner">
          <strong>🔒 Màn hình GNOME đang khóa — tạm hoãn thao tác UI an toàn cho tới khi mở khóa</strong>
        </section>
      )}

      {enabledProfilesCount < 2 && (
        <section className="yield-banner" data-testid="low-profile-warning">
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
            <div>
              <strong>⚠️ Hiện chỉ bật {enabledProfilesCount} hồ sơ Chrome</strong>
              <div style={{ fontSize: "0.84rem", color: "#fde68a", marginTop: 3 }}>
                Cần bật ít nhất 2 hồ sơ (mỗi tài khoản 1 <code>--profile-directory</code> riêng) để tự động chuyển tài khoản khi chạm 0% Quota.
              </div>
            </div>
            <button type="button" className="btn btn-primary" onClick={() => setActiveTab("profiles")}>
              Mở tab Hồ sơ Tài khoản
            </button>
          </div>
        </section>
      )}

      {status.dryRunActuation && (
        <section className="yield-banner" data-testid="dryrun-mode-banner">
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
            <div>
              <strong>🛡️ Chế độ Dry-Run An toàn đang bật (Chỉ mô phỏng quyết định, chưa click thật)</strong>
              <div style={{ fontSize: "0.84rem", color: "#fde68a", marginTop: 3 }}>
                Khi đã cài đặt <code>cua-driver</code> và thiết lập <code>Agent Workspace #2</code>, bấm nút bên phải để kích hoạt điều khiển thực tế trên Antigravity &amp; Google Chrome.
              </div>
            </div>
            <button
              type="button"
              className="btn btn-primary"
              onClick={async () => {
                try {
                  await backendBridge.setDryRunActuation(false);
                  await refreshAll();
                  showToast("Đã bật Chế độ Thực thi Thật (Live Mode) — hệ thống sẽ chụp Evidence & điều phối thật");
                } catch (err) {
                  showToast(`Không thể chuyển chế độ: ${String(err)}`);
                }
              }}
            >
              Bật Chế độ Thực thi Thật (Live Mode)
            </button>
          </div>
        </section>
      )}

      {status.state === "PausedHumanInput" && (
        <section className="yield-banner" data-testid="yield-countdown-banner">
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12 }}>
            <div>
              <strong>🟡 Đang nhường quyền thao tác cho con người (Cửa sổ tĩnh lặng trượt 30s)</strong>
              <div style={{ fontSize: "0.84rem", color: "#fde68a", marginTop: 3 }}>
                {status.stateReason} — Hệ thống sẽ quay lại <code>Observing</code> và đối soát lại từ đầu khi đủ 30s không thao tác.
              </div>
            </div>
            <div style={{ fontSize: "1.35rem", fontWeight: 700, color: "#fbbf24", whiteSpace: "nowrap" }}>
              {(status.yieldRemainingMs / 1000).toFixed(1)}s
            </div>
          </div>
          <div className="progress-track">
            <div className="progress-fill" style={{ width: `${yieldPct}%` }} />
          </div>
        </section>
      )}

      {(status.intentWaitRemainingMs ?? 0) > 0 && status.state !== "PausedHumanInput" && (
        <section className="yield-banner" data-testid="signout-wait-banner">
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12 }}>
            <div>
              <strong>⏳ Chu trình CUA 5 Bước: Đang chờ sau khi bấm SignOut trước khi bấm SignIn</strong>
              <div style={{ fontSize: "0.84rem", color: "#fde68a", marginTop: 3 }}>
                {status.stateReason}
              </div>
            </div>
            <div style={{ fontSize: "1.35rem", fontWeight: 700, color: "#60a5fa", whiteSpace: "nowrap" }}>
              {((status.intentWaitRemainingMs ?? 0) / 1000).toFixed(0)}s
            </div>
          </div>
        </section>
      )}

      {(status.state === "HardStopChallenge" || status.circuitBreakerTripped) && (
        <section className="alert-hardstop" data-testid="hardstop-banner">
          <div>
            <strong>🔴 Chốt chặn An toàn đang kích hoạt (Dừng tự động hóa)</strong>
            <div style={{ fontSize: "0.85rem", color: "#fecaca", marginTop: 4 }}>
              {status.hardStopReason || status.circuitBreakerReason || status.stateReason}
            </div>
          </div>
          <button type="button" className="btn btn-danger" onClick={handleResetHardStop}>
            Đặt lại &amp; Tiếp tục Quan sát
          </button>
        </section>
      )}

      <nav className="nav-tabs" aria-label="Điều hướng chính">
        <button
          type="button"
          className={`tab-btn ${activeTab === "overview" ? "active" : ""}`}
          onClick={() => setActiveTab("overview")}
        >
          Tổng quan &amp; Giám sát
        </button>
        <button
          type="button"
          className={`tab-btn ${activeTab === "profiles" ? "active" : ""}`}
          onClick={() => setActiveTab("profiles")}
        >
          Hồ sơ Tài khoản &amp; Model ({config.profiles.length})
        </button>
        <button
          type="button"
          className={`tab-btn ${activeTab === "settings" ? "active" : ""}`}
          onClick={() => setActiveTab("settings")}
        >
          Cấu hình Điều phối &amp; Wayland
        </button>
        <button
          type="button"
          className={`tab-btn ${activeTab === "logs" ? "active" : ""}`}
          onClick={() => setActiveTab("logs")}
        >
          Nhật ký &amp; Evidence ({evidence.length})
        </button>
      </nav>

      {activeTab === "overview" && (
        <>
          {needsOnboarding && (
            <div className="card" data-testid="onboarding-checklist" style={{ marginBottom: 14 }}>
              <div className="card-title">
                <span>🚀 Danh mục Khởi tạo Production (3 Bước Nhanh)</span>
              </div>
              <div className="sensor-list">
                <div className="sensor-item">
                  <span>1. Thiết lập <strong>Agent Workspace</strong> tách biệt &amp; cài động cơ <code>trycua/cua</code></span>
                  <span className={agentWs?.ready && status.waylandPortalReady ? "badge-ok" : "badge-err"}>
                    {agentWs?.ready && status.waylandPortalReady ? "Hoàn tất" : "Chưa xong"}
                  </span>
                </div>
                <div className="sensor-item">
                  <span>2. Quét và bật tối thiểu <strong>2 hồ sơ Chrome</strong> độc lập (<code>Default</code>, <code>Profile 1</code>, ...)</span>
                  <span className={enabledProfilesCount >= 2 ? "badge-ok" : "badge-err"}>
                    {enabledProfilesCount >= 2 ? `Đã bật ${enabledProfilesCount} hồ sơ` : `Mới bật ${enabledProfilesCount}/2`}
                  </span>
                </div>
                <div className="sensor-item">
                  <span>3. Xác minh cây UI thực địa (<code>Inspect UI</code>) &amp; bật Chế độ Thực thi Thật (Live Mode)</span>
                  <span className={!status.dryRunActuation ? "badge-ok" : "badge-err"}>
                    {!status.dryRunActuation ? "Live Mode" : "Đang Dry-Run"}
                  </span>
                </div>
              </div>
            </div>
          )}

          <div className="card" data-testid="cua-pipeline-stepper">
            <div className="card-title">
              <span>Tiến trình Tự động hóa 5 Bước (Verify → Screenshot Evidence → Execute)</span>
              <span className="metric-sub" style={{ margin: 0 }}>
                {status.pendingIntent?.runId ? `Phiên: ${status.pendingIntent.runId}` : "Giám sát định kỳ (ADR-008)"}
              </span>
            </div>
            <div className="pipeline-bar">
              {pipelineSteps.map((step) => {
                const cls =
                  step.num === pipelineStage
                    ? "pipeline-step active"
                    : step.num < pipelineStage
                    ? "pipeline-step done"
                    : "pipeline-step";
                return (
                  <div key={step.num} className={cls}>
                    <div style={{ fontWeight: 700, marginBottom: 4 }}>{step.title}</div>
                    <div style={{ fontSize: "0.75rem", opacity: 0.88 }}>{step.desc}</div>
                  </div>
                );
              })}
            </div>
          </div>

          <div className="grid-4">
            <div className="card">
              <div className="metric-label">Hồ sơ đang kích hoạt</div>
              <div className="metric-value">{status.activeProfileName || status.activeProfileId}</div>
              <div className="metric-sub">Khóa cứng --profile-directory trên Chrome</div>
            </div>
            <div className="card">
              <div className="metric-label">Model AI mặc định / hiện hành</div>
              <div className="metric-value">{status.activeModel || "Chưa xác định"}</div>
              <div className="metric-sub">
                {config.preferIntraProfileModelDrop ? "Ưu tiên hạ model nội bộ trước" : "Chuyển thẳng sang Account kế tiếp"}
              </div>
            </div>
            <div className="card">
              <div className="metric-label">Chu kỳ bấm nút xem Quota</div>
              <div className="metric-value">{Math.round(config.cuaDriver.quotaCheckIntervalMs / 1000)}s</div>
              <div className="metric-sub">
                {(status.nextQuotaCheckRemMs ?? 0) > 0
                  ? `Kiểm tra kế tiếp sau ${Math.ceil((status.nextQuotaCheckRemMs ?? 0) / 1000)}s`
                  : "Sẵn sàng kiểm tra ngay khi máy tĩnh"}
              </div>
            </div>
            <div className="card">
              <div className="metric-label">Ngân sách Circuit Breaker &amp; Thống kê 24/7</div>
              <div className="metric-value">
                {status.switchesUsedInWindow} / {status.maxSwitchesPerWindow} lần
              </div>
              <div className="metric-sub">
                Cửa sổ {config.switchWindowMinutes}p • Đã kiểm Quota: {status.quotaChecksTotal ?? 0} • Đã chuyển: {status.switchesCompletedTotal ?? 0}
              </div>
            </div>
          </div>

          <div className="grid-2">
            <div className="card">
              <h2 className="card-title">
                <span>Sức khỏe Động cơ trycua/cua, Agent Workspace &amp; Cảm biến Fedora 45</span>
              </h2>
              <div className="sensor-list">
                {agentWs && (
                  <div className="sensor-item" data-testid="agent-workspace-item">
                    <div>
                      <strong>
                        Không gian Dành Riêng Tự Động Hóa (Workspace #{(agentWs.workspaceIndex ?? 1) + 1}: {agentWs.workspaceName || "Agent Workspace"})
                      </strong>
                      <div className="metric-sub">{agentWs.detail}</div>
                    </div>
                    <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                      <span className={agentWs.ready ? "badge-ok" : "badge-err"}>
                        {agentWs.enabled ? (agentWs.ready ? "Đã cô lập" : "Cần thiết lập") : "Đang tắt"}
                      </span>
                      <button
                        type="button"
                        className="btn"
                        onClick={async () => {
                          try {
                            const ws = await backendBridge.setupAgentWorkspace();
                            await refreshAll();
                            showToast(`Agent Workspace: ${ws.detail}`);
                          } catch (err) {
                            showToast(`Agent Workspace: ${String(err)}`);
                          }
                        }}
                      >
                        Thiết lập Agent Workspace
                      </button>
                    </div>
                  </div>
                )}

                <div className="sensor-item">
                  <div>
                    <strong>Động cơ Tương tác Chính (trycua/cua Sidecar)</strong>
                    <div className="metric-sub">{status.waylandPortalDetail}</div>
                  </div>
                  <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                    <span className={status.waylandPortalReady ? "badge-ok" : "badge-err"}>
                      {status.waylandPortalReady ? "Sẵn sàng" : "Chưa khởi tạo"}
                    </span>
                    <button
                      type="button"
                      className="btn"
                      onClick={async () => {
                        try {
                          const msg = await backendBridge.authorizeWaylandPortal();
                          await refreshAll();
                          showToast(msg);
                        } catch (err) {
                          showToast(`CUA Engine: ${String(err)}`);
                        }
                      }}
                    >
                      Kiểm tra Kết nối
                    </button>
                  </div>
                </div>

                {status.sensors.map((s) => (
                  <div key={s.tierName} className="sensor-item">
                    <div>
                      <strong>Tier: {s.tierName}</strong>
                      <div className="metric-sub">{s.detail}</div>
                    </div>
                    <span className={s.healthy ? "badge-ok" : "badge-err"}>
                      {s.healthy ? "Hoạt động" : "Gián đoạn"}
                    </span>
                  </div>
                ))}
              </div>
            </div>

            <div className="card">
              <h2 className="card-title">
                <span>Trạng thái Lý giải &amp; Công cụ Chẩn đoán (Developer Sandbox)</span>
                <button
                  type="button"
                  className="btn"
                  data-testid="sandbox-panel-toggle"
                  onClick={() => setShowSandbox((v) => !v)}
                >
                  {showSandbox ? "Thu gọn Sandbox" : "Mở Công cụ Mô phỏng"}
                </button>
              </h2>

              <div style={{ marginBottom: 14 }}>
                <div className="metric-label">Trạng thái Lý giải Hiện tại</div>
                <div className="code-box" style={{ marginTop: 6 }}>
                  [{status.state}] {status.stateReason}
                </div>
              </div>

              {showSandbox && (
                <div data-testid="sandbox-panel-body">
                  <p className="metric-sub" style={{ marginBottom: 12 }}>
                    Kiểm chứng tức thì cơ chế ngắt 30 giây khi di chuột/gõ phím, chu trình hết hạn mức 0% và dừng cứng MFA/CAPTCHA:
                  </p>
                  <div style={{ display: "flex", flexWrap: "wrap", gap: 10 }}>
                    <button
                      type="button"
                      className="btn btn-amber"
                      onClick={async () => {
                        const st = await backendBridge.simulateHumanInput("pointer_move");
                        setStatus(st);
                      }}
                    >
                      Giả lập Di Chuột (Ngắt 30s)
                    </button>
                    <button
                      type="button"
                      className="btn btn-amber"
                      onClick={async () => {
                        const st = await backendBridge.simulateHumanInput("keyboard");
                        setStatus(st);
                      }}
                    >
                      Giả lập Gõ Phím (Ngắt 30s)
                    </button>
                    <button
                      type="button"
                      className="btn btn-primary"
                      onClick={async () => {
                        const st = await backendBridge.simulateQuotaExhausted();
                        setStatus(st);
                        await refreshAll(false);
                      }}
                    >
                      Giả lập Hết Hạn Mức
                    </button>
                    <button
                      type="button"
                      className="btn btn-danger"
                      onClick={async () => {
                        const st = await backendBridge.simulateAuthChallenge();
                        setStatus(st);
                      }}
                    >
                      Giả lập Gặp MFA / CAPTCHA
                    </button>
                  </div>
                </div>
              )}
            </div>
          </div>
        </>
      )}

      {activeTab === "profiles" && (
        <form onSubmit={handleSaveConfig} className="card">
          <div className="card-title" style={{ flexWrap: "wrap", gap: 8 }}>
            <span>Quản lý Hồ sơ Tài khoản Chrome đăng nhập sẵn &amp; Model Mặc định</span>
            <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
              <button type="button" className="btn" onClick={handleDiscoverProfiles}>
                Quét tự động Profile Chrome
              </button>
              <button
                type="button"
                className="btn"
                onClick={async () => {
                  try {
                    const models = await backendBridge.discoverAntigravityModels();
                    setDiscoveredModels(models);
                    showToast(
                      models.length > 0
                        ? `Đã phát hiện ${models.length} Model từ Antigravity: ${models.slice(0, 3).join(", ")}`
                        : "Không tìm thấy danh sách Model trên cửa sổ Antigravity"
                    );
                  } catch (err) {
                    showToast(`Quét Model thất bại: ${String(err)}`);
                  }
                }}
              >
                Quét Model từ Antigravity
              </button>
              <button type="button" className="btn" onClick={handleAddProfile}>
                + Thêm Hồ sơ
              </button>
              <button type="submit" className="btn btn-primary">
                Lưu Thay đổi
              </button>
            </div>
          </div>

          {discoveredModels.length > 0 && (
            <div style={{ marginBottom: 14, display: "flex", flexWrap: "wrap", gap: 6, alignItems: "center" }} data-testid="discovered-models-bar">
              <span className="metric-sub" style={{ margin: 0 }}>Model sống trên Antigravity:</span>
              {discoveredModels.map((m) => (
                <code key={m} style={{ padding: "2px 8px", borderRadius: 6, border: "1px solid var(--border-subtle)" }}>
                  {m}
                </code>
              ))}
            </div>
          )}

          <p className="metric-sub" style={{ marginBottom: 16 }}>
            Mỗi hồ sơ khóa chặt với một <code>--profile-directory</code> của Google Chrome (quét tự động từ tệp <code>Local State</code>) và xác minh chéo Omnibox <code>accounts.google.com</code> + chụp Evidence trước khi chọn Account. Không lưu mật khẩu hay OAuth token.
          </p>

          {config.profiles.map((prof, idx) => (
            <div key={prof.id} className="profile-card">
              <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", flexWrap: "wrap", gap: 8, marginBottom: 10 }}>
                <strong>
                  #{idx + 1} — {prof.name}{" "}
                  {config.activeProfileId === prof.id ? "(Đang chọn làm Hồ sơ hiện hành)" : ""}
                </strong>
                <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
                  <button
                    type="button"
                    className="btn btn-primary"
                    onClick={async () => {
                      try {
                        await backendBridge.triggerManualSwitch(prof.id);
                        await refreshAll();
                        showToast(`Đã kích hoạt chu trình CUA chuyển sang '${prof.name}'`);
                      } catch (err) {
                        showToast(`Không thể chuyển hồ sơ: ${String(err)}`);
                      }
                    }}
                  >
                    Chuyển ngay qua CUA
                  </button>
                  <button
                    type="button"
                    className="btn"
                    onClick={() => setConfig({ ...config, activeProfileId: prof.id })}
                  >
                    Đặt làm Hiện hành
                  </button>
                  <button
                    type="button"
                    className="btn"
                    onClick={async () => {
                      try {
                        await backendBridge.testOpenBrowserProfile(prof.id);
                        showToast(`Đã gửi lệnh mở trình duyệt (${prof.browserProfileDir})`);
                      } catch (err) {
                        showToast(`Không mở được trình duyệt: ${String(err)}`);
                      }
                    }}
                  >
                    Mở thử Profile
                  </button>
                  <button type="button" className="btn btn-danger" onClick={() => removeProfile(idx)}>
                    Xóa
                  </button>
                </div>
              </div>

              <div className="form-grid">
                <div className="field">
                  <label>Tên hiển thị</label>
                  <input
                    value={prof.name}
                    onChange={(e) => updateProfile(idx, { name: e.target.value })}
                    required
                  />
                </div>
                <div className="field">
                  <label>Email trên Chrome (để CUA nhận diện &amp; click chọn Account)</label>
                  <input
                    value={prof.emailHint}
                    onChange={(e) => updateProfile(idx, { emailHint: e.target.value })}
                    placeholder="work@company.com"
                  />
                </div>
                <div className="field">
                  <label>Trình duyệt</label>
                  <select
                    value={prof.browserType}
                    onChange={(e) => updateProfile(idx, { browserType: e.target.value as BrowserType })}
                  >
                    <option value="chrome">Google Chrome</option>
                    <option value="chromium">Chromium</option>
                    <option value="brave">Brave Browser</option>
                    <option value="edge">Microsoft Edge</option>
                    <option value="firefox">Mozilla Firefox (-P)</option>
                    <option value="custom">Tùy chỉnh nhị phân</option>
                  </select>
                </div>
                <div className="field">
                  <label>Thư mục Profile Chrome (ví dụ: Default, Profile 1)</label>
                  <input
                    value={prof.browserProfileDir}
                    onChange={(e) => updateProfile(idx, { browserProfileDir: e.target.value })}
                    required
                  />
                </div>
                <div className="field">
                  <label>Model Mặc định của Hồ sơ (Primary)</label>
                  <input
                    value={prof.primaryModel}
                    onChange={(e) => updateProfile(idx, { primaryModel: e.target.value })}
                    required
                  />
                </div>
                <div className="field">
                  <label>Chuỗi Model Dự phòng (phân cách dấu phẩy)</label>
                  <input
                    value={prof.fallbackModels.join(", ")}
                    onChange={(e) =>
                      updateProfile(idx, {
                        fallbackModels: e.target.value
                          .split(",")
                          .map((s) => s.trim())
                          .filter(Boolean)
                      })
                    }
                    placeholder="gemini-2.5-flash, claude-sonnet-4-6"
                  />
                </div>
                <div className="field">
                  <label>Thứ tự ưu tiên (nhỏ hơn = ưu tiên hơn)</label>
                  <input
                    type="number"
                    min={1}
                    max={99}
                    value={prof.priority}
                    onChange={(e) => updateProfile(idx, { priority: Number(e.target.value) || 1 })}
                  />
                </div>
              </div>

              <label className="checkbox-row">
                <input
                  type="checkbox"
                  checked={prof.enabled}
                  onChange={(e) => updateProfile(idx, { enabled: e.target.checked })}
                />
                Cho phép đưa hồ sơ này vào danh sách chọn Account tự động trên Chrome
              </label>
            </div>
          ))}
        </form>
      )}

      {activeTab === "settings" && (
        <form onSubmit={handleSaveConfig} className="card">
          <div className="card-title">
            <span>Cấu hình Bộ Điều phối, Tạm dừng 30s &amp; Động cơ trycua/cua</span>
            <button type="submit" className="btn btn-primary">
              Lưu Cấu hình
            </button>
          </div>

          <div className="form-grid" style={{ marginBottom: 16 }}>
            <div className="field">
              <label>Thời gian tạm dừng khi chuột/phím thao tác (giây)</label>
              <input
                type="number"
                min={5}
                max={3600}
                value={config.yieldPauseSeconds}
                onChange={(e) => setConfig({ ...config, yieldPauseSeconds: Number(e.target.value) })}
              />
            </div>
            <div className="field">
              <label>Thời gian Cooldown ổn định sau chuyển đổi (giây)</label>
              <input
                type="number"
                min={1}
                max={3600}
                value={config.cooldownSeconds}
                onChange={(e) => setConfig({ ...config, cooldownSeconds: Number(e.target.value) })}
              />
            </div>
            <div className="field">
              <label>Chu kỳ vòng lặp trạng thái nền (mili-giây)</label>
              <input
                type="number"
                min={250}
                max={60000}
                value={config.pollIntervalMs}
                onChange={(e) => setConfig({ ...config, pollIntervalMs: Number(e.target.value) })}
              />
            </div>
            <div className="field">
              <label>Số khung hình đồng thuận thụ động</label>
              <input
                type="number"
                min={1}
                max={20}
                value={config.consensusFrames}
                onChange={(e) => setConfig({ ...config, consensusFrames: Number(e.target.value) })}
              />
            </div>
            <div className="field">
              <label>Ngân sách chuyển tối đa / cửa sổ 30 phút</label>
              <input
                type="number"
                min={1}
                max={50}
                value={config.maxSwitchesPerWindow}
                onChange={(e) => setConfig({ ...config, maxSwitchesPerWindow: Number(e.target.value) })}
              />
            </div>
            <div className="field">
              <label>Đường dẫn CLI settings.json chính thức</label>
              <input
                value={config.cliSettingsPath}
                onChange={(e) => setConfig({ ...config, cliSettingsPath: e.target.value })}
              />
            </div>
          </div>

          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={config.preferIntraProfileModelDrop}
              onChange={(e) => setConfig({ ...config, preferIntraProfileModelDrop: e.target.checked })}
            />
            Ưu tiên hạ xuống Model dự phòng trên cùng hồ sơ trước khi SignOut chuyển Account khác
          </label>

          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={config.dryRunActuation}
              onChange={(e) => setConfig({ ...config, dryRunActuation: e.target.checked })}
            />
            Chế độ Dry-Run (Chỉ ghi nhận quyết định, không thực thi click thật lên Antigravity / Chrome)
          </label>

          <hr style={{ borderColor: "var(--border-subtle)", margin: "18px 0" }} />

          <h3 style={{ fontSize: "0.94rem", margin: "0 0 10px" }}>
            Động cơ Chính: trycua/cua (cua-driver) — Tự động hóa 5 Bước Antigravity &amp; Chrome (ADR-007 / ADR-008)
          </h3>
          <p className="metric-sub" style={{ marginBottom: 10 }}>
            Cô lập trên Agent Workspace Fedora 45 → Kiểm tra Quota mỗi 1 phút → Khi Quota về 0% (Quota exhausted), chụp Evidence &amp; SignOut →
            Chờ 1 phút → Bấm SignIn, xác minh Chrome Profile + Omnibox &amp; chọn Account → Chọn Model mặc định → Xóa tệp trạng thái tạm.
          </p>

          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={config.cuaDriver.enabled}
              onChange={(e) =>
                setConfig({
                  ...config,
                  cuaDriver: { ...config.cuaDriver, enabled: e.target.checked }
                })
              }
            />
            Bật CUA Driver Sidecar làm động cơ giám sát &amp; tương tác chính (Mặc định)
          </label>

          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={config.cuaDriver.agentWorkspaceEnabled}
              onChange={(e) =>
                setConfig({
                  ...config,
                  cuaDriver: { ...config.cuaDriver, agentWorkspaceEnabled: e.target.checked }
                })
              }
            />
            Cô lập thao tác tự động sang Agent Workspace riêng trên Fedora 45 Wayland (không giành Focus của User)
          </label>

          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={config.cuaDriver.requireEvidenceBeforeActuate}
              onChange={(e) =>
                setConfig({
                  ...config,
                  cuaDriver: { ...config.cuaDriver, requireEvidenceBeforeActuate: e.target.checked }
                })
              }
            />
            Bắt buộc Verify &amp; chụp Screenshot lưu Evidence (SHA-256) trước mọi thao tác nhạy cảm
          </label>

          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={config.cuaDriver.maskEditorInEvidence}
              onChange={(e) =>
                setConfig({
                  ...config,
                  cuaDriver: { ...config.cuaDriver, maskEditorInEvidence: e.target.checked }
                })
              }
            />
            Tự động che mờ vùng khung soạn thảo Code/Chat trong ảnh chụp Evidence trước khi lưu đĩa (Bảo vệ quyền riêng tư dự án)
          </label>

          <div className="form-grid" style={{ marginTop: 10 }}>
            <div className="field">
              <label>Giãn cách bấm nút xem Quota (ms — mặc định 60000 = 1 phút)</label>
              <input
                type="number"
                min={5000}
                max={600000}
                value={config.cuaDriver.quotaCheckIntervalMs}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    cuaDriver: { ...config.cuaDriver, quotaCheckIntervalMs: Number(e.target.value) }
                  })
                }
              />
            </div>
            <div className="field">
              <label>Thời gian chờ sau SignOut trước khi SignIn (ms — mặc định 60000 = 1 phút)</label>
              <input
                type="number"
                min={1000}
                max={300000}
                value={config.cuaDriver.signOutCooldownMs}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    cuaDriver: { ...config.cuaDriver, signOutCooldownMs: Number(e.target.value) }
                  })
                }
              />
            </div>
            <div className="field">
              <label>Chỉ số Agent Workspace (0-based — mặc định 1 = Workspace #2)</label>
              <input
                type="number"
                min={1}
                max={15}
                value={config.cuaDriver.agentWorkspaceIndex}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    cuaDriver: { ...config.cuaDriver, agentWorkspaceIndex: Number(e.target.value) }
                  })
                }
              />
            </div>
            <div className="field">
              <label>Tên định danh Agent Workspace (mặc định: Agent Workspace)</label>
              <input
                value={config.cuaDriver.agentWorkspaceName || ""}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    cuaDriver: { ...config.cuaDriver, agentWorkspaceName: e.target.value }
                  })
                }
                placeholder="Agent Workspace"
              />
            </div>
            <div className="field">
              <label>Số phiên chuyển đổi lưu trữ ảnh Evidence tối đa</label>
              <input
                type="number"
                min={1}
                max={200}
                value={config.cuaDriver.evidenceRetentionRuns}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    cuaDriver: { ...config.cuaDriver, evidenceRetentionRuns: Number(e.target.value) }
                  })
                }
              />
            </div>
            <div className="field">
              <label>Độ tĩnh tối thiểu trước khi tự Focus kiểm tra Quota (ms)</label>
              <input
                type="number"
                min={0}
                max={60000}
                value={config.cuaDriver.minIdleBeforeFocusMs}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    cuaDriver: { ...config.cuaDriver, minIdleBeforeFocusMs: Number(e.target.value) }
                  })
                }
              />
            </div>
            <div className="field">
              <label>Thời gian chờ tối đa cửa sổ Chrome mở trang Account (ms)</label>
              <input
                type="number"
                min={2000}
                max={60000}
                value={config.cuaDriver.browserSettleTimeoutMs}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    cuaDriver: { ...config.cuaDriver, browserSettleTimeoutMs: Number(e.target.value) }
                  })
                }
              />
            </div>
            <div className="field">
              <label>Model mặc định ghi đè chung (để trống = theo Hồ sơ; nhập <code>preserve_active</code> = giữ nguyên model đang chạy)</label>
              <input
                value={config.cuaDriver.defaultModel || ""}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    cuaDriver: { ...config.cuaDriver, defaultModel: e.target.value || undefined }
                  })
                }
                placeholder="Ví dụ: Gemini 4 Neon Extra High hoặc preserve_active"
              />
            </div>
            <div className="field">
              <label>Chế độ truyền tương tác (delivery_mode)</label>
              <select
                value={config.cuaDriver.deliveryMode || "auto"}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    cuaDriver: { ...config.cuaDriver, deliveryMode: e.target.value }
                  })
                }
              >
                <option value="auto">Auto (Ưu tiên Background, tự chuyển Foreground khi cần)</option>
                <option value="background">Background (Semantic element_token)</option>
                <option value="foreground">Foreground (Focus trực tiếp cửa sổ)</option>
              </select>
            </div>
            <div className="field">
              <label>Python binary (để trống = tự phát hiện python3)</label>
              <input
                value={config.cuaDriver.pythonBin || ""}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    cuaDriver: { ...config.cuaDriver, pythonBin: e.target.value || undefined }
                  })
                }
                placeholder="python3"
              />
            </div>
          </div>

          <label className="checkbox-row" style={{ marginTop: 8 }}>
            <input
              type="checkbox"
              checked={config.cuaDriver.allowSameAccountRelogin}
              onChange={(e) =>
                setConfig({
                  ...config,
                  cuaDriver: { ...config.cuaDriver, allowSameAccountRelogin: e.target.checked }
                })
              }
            />
            Cho phép thực hiện SignOut 1 phút → SignIn lại cùng tài khoản hiện hành khi chỉ bật 1 hồ sơ duy nhất
          </label>

          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={autostartEnabled}
              data-testid="autostart-toggle"
              onChange={async (e) => {
                const checked = e.target.checked;
                try {
                  await backendBridge.setAutostartEnabled(checked);
                  setAutostartEnabled(checked);
                  showToast(checked ? "Đã bật tự khởi động cùng Fedora (~/.config/autostart)" : "Đã tắt tự khởi động cùng Fedora");
                } catch (err) {
                  showToast(`Cấu hình Autostart lỗi: ${String(err)}`);
                }
              }}
            />
            Tự động khởi chạy ẩn vào khay hệ thống khi đăng nhập Fedora (XDG Autostart)
          </label>

          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={config.cuaDriver.enableWayland}
              onChange={(e) =>
                setConfig({
                  ...config,
                  cuaDriver: { ...config.cuaDriver, enableWayland: e.target.checked }
                })
              }
            />
            Kích hoạt backend Wayland cho cua-driver (CUA_DRIVER_RS_ENABLE_WAYLAND=1)
          </label>

          <div style={{ marginTop: 14, display: "flex", flexWrap: "wrap", gap: 8 }}>
            <button
              type="button"
              className="btn btn-primary"
              onClick={async () => {
                showToast("Đang khởi tạo môi trường ảo XDG và cài đặt cua-driver (--only-binary=:all:)...");
                try {
                  const msg = await backendBridge.provisionCuaSidecar();
                  await refreshAll();
                  showToast(msg);
                } catch (err) {
                  showToast(`Cài đặt sidecar lỗi: ${String(err)}`);
                }
              }}
            >
              Cài đặt tự động Python Venv + cua-driver
            </button>
            <button
              type="button"
              className="btn"
              onClick={async () => {
                try {
                  const msg = await backendBridge.enableWaylandAccessibility();
                  showToast(msg);
                } catch (err) {
                  showToast(`Accessibility: ${String(err)}`);
                }
              }}
            >
              Bật Accessibility cho Antigravity &amp; Chrome
            </button>
            <button
              type="button"
              className="btn"
              onClick={async () => {
                try {
                  const msg = await backendBridge.testCuaAction("focus_antigravity");
                  showToast(msg);
                } catch (err) {
                  showToast(`Test Focus: ${String(err)}`);
                }
              }}
            >
              Thử Focus Antigravity
            </button>
            <button
              type="button"
              className="btn"
              onClick={async () => {
                try {
                  const msg = await backendBridge.testCuaAction("inspect_quota");
                  showToast(msg);
                } catch (err) {
                  showToast(`Test Quota: ${String(err)}`);
                }
              }}
            >
              Thử Bấm Xem Quota
            </button>
            <button
              type="button"
              className="btn"
              onClick={async () => {
                try {
                  const msg = await backendBridge.testCuaAction("focus_chrome");
                  showToast(msg);
                } catch (err) {
                  showToast(`Test Chrome: ${String(err)}`);
                }
              }}
            >
              Thử Focus Chrome
            </button>
            <button
              type="button"
              className="btn"
              onClick={async () => {
                try {
                  const report = await backendBridge.inspectUISelectors();
                  setUiInspectReport(report);
                  showToast("Đã quét chỉ-đọc cây phần tử UI thành công");
                } catch (err) {
                  showToast(`Inspect UI: ${String(err)}`);
                }
              }}
            >
              Quét Cây Phần Tử Thật (Inspect UI)
            </button>
            <button
              type="button"
              className="btn"
              onClick={async () => {
                try {
                  const msg = await backendBridge.testCuaAction("select_model");
                  showToast(msg);
                } catch (err) {
                  showToast(`Test Model: ${String(err)}`);
                }
              }}
            >
              Thử Chọn Model Mặc định
            </button>
          </div>

          {uiInspectReport && (
            <div style={{ marginTop: 12 }} data-testid="ui-inspect-report-box">
              <div className="metric-label">Báo cáo Hiệu chuẩn Cây Phần Tử Thực Địa (Read-Only Inspect UI)</div>
              <pre className="code-box" style={{ marginTop: 6, maxHeight: 220 }}>
                {uiInspectReport}
              </pre>
            </div>
          )}
        </form>
      )}

      {activeTab === "logs" && (
        <>
          <div className="card" data-testid="evidence-vault-card">
            <div className="card-title" style={{ flexWrap: "wrap", gap: 8 }}>
              <span>Kho Bằng chứng Ảnh chụp Trước Thực thi (Evidence Vault — SHA-256)</span>
              <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                <span className="metric-sub" style={{ margin: 0 }}>
                  Quyền tệp 0600 • Giữ {config.cuaDriver.evidenceRetentionRuns} phiên gần nhất
                </span>
                <button
                  type="button"
                  className="btn btn-danger"
                  onClick={async () => {
                    try {
                      await backendBridge.clearEvidenceVault();
                      setPreviewEvidence(null);
                      await refreshAll();
                      showToast("Đã xóa sạch toàn bộ ảnh chụp trong Evidence Vault");
                    } catch (err) {
                      showToast(`Không xóa được kho Evidence: ${String(err)}`);
                    }
                  }}
                >
                  Xóa kho Evidence
                </button>
              </div>
            </div>

            {previewEvidence && (
              <div className="evidence-preview-box" data-testid="evidence-preview-modal">
                <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12 }}>
                  <div>
                    <strong>
                      Ảnh Bằng chứng: <code>{previewEvidence.record.step}</code> ({previewEvidence.record.runId})
                    </strong>
                    <div className="metric-sub">
                      Cửa sổ: {previewEvidence.record.windowTitle} (PID {previewEvidence.record.pid})
                      {previewEvidence.record.browserProfile ? ` • Chrome Profile: ${previewEvidence.record.browserProfile}` : ""}
                      {previewEvidence.record.omniboxOrigin ? ` • Origin: ${previewEvidence.record.omniboxOrigin}` : ""}
                    </div>
                    <div className="metric-sub">
                      SHA-256: <code>{previewEvidence.record.sha256}</code>
                    </div>
                  </div>
                  <button
                    type="button"
                    className="btn"
                    onClick={() => setPreviewEvidence(null)}
                  >
                    Đóng xem trước
                  </button>
                </div>
                <img
                  className="evidence-img"
                  src={previewEvidence.dataUrl}
                  alt={`Evidence ${previewEvidence.record.step}`}
                />
              </div>
            )}

            {evidence.length === 0 ? (
              <p className="metric-sub">Chưa có ảnh bằng chứng nào được ghi nhận.</p>
            ) : (
              <div style={{ overflowX: "auto" }}>
                <table className="log-table">
                  <thead>
                    <tr>
                      <th>Thời gian</th>
                      <th>Phiên (Run ID)</th>
                      <th>Cổng Kiểm chứng</th>
                      <th>Cửa sổ &amp; Xác minh</th>
                      <th>SHA-256</th>
                      <th>Hành động</th>
                    </tr>
                  </thead>
                  <tbody>
                    {evidence.map((rec, idx) => (
                      <tr key={`${rec.runId}-${rec.step}-${idx}`}>
                        <td style={{ whiteSpace: "nowrap", color: "var(--text-secondary)" }}>
                          {new Date(rec.createdAt).toLocaleTimeString("vi-VN")}
                        </td>
                        <td><code>{rec.runId}</code></td>
                        <td><code>{rec.step}</code></td>
                        <td>
                          <div>{rec.verifiedDetail || rec.windowTitle}</div>
                          {(rec.browserProfile || rec.omniboxOrigin) && (
                            <div className="metric-sub">
                              {rec.browserProfile ? `Profile: ${rec.browserProfile} ` : ""}
                              {rec.omniboxOrigin ? `• ${rec.omniboxOrigin}` : ""}
                            </div>
                          )}
                        </td>
                        <td><code>{rec.sha256.slice(0, 12)}…</code></td>
                        <td>
                          <button
                            type="button"
                            className="btn"
                            onClick={async () => {
                              try {
                                const dataUrl = await backendBridge.getEvidenceDataURL(rec.imagePath);
                                setPreviewEvidence({ record: rec, dataUrl });
                              } catch (err) {
                                showToast(`Không đọc được ảnh Evidence: ${String(err)}`);
                              }
                            }}
                          >
                            Xem ảnh Evidence
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>

          <div className="card">
            <div className="card-title">
              <span>Nhật ký Kiểm toán JSONL (Tail-Seek O(1) — Tự động che PII &amp; OAuth Query)</span>
              <div style={{ display: "flex", gap: 8 }}>
                <button
                  type="button"
                  className="btn"
                  data-testid="export-diagnostics-btn"
                  onClick={async () => {
                    try {
                      const outPath = await backendBridge.exportDiagnostics();
                      showToast(`Đã xuất gói chẩn đoán ẩn danh: ${outPath}`);
                    } catch (err) {
                      showToast(`Xuất gói chẩn đoán lỗi: ${String(err)}`);
                    }
                  }}
                >
                  Xuất Gói Chẩn đoán (.tar.gz)
                </button>
                <select
                  value={logFilter}
                  onChange={(e) => setLogFilter(e.target.value as "ALL" | "INFO" | "WARN" | "ERROR")}
                  style={{
                    background: "var(--bg-card)",
                    color: "var(--text-primary)",
                    border: "1px solid var(--border-subtle)",
                    borderRadius: 8,
                    padding: "6px 10px"
                  }}
                >
                  <option value="ALL">Tất cả mức độ</option>
                  <option value="INFO">INFO</option>
                  <option value="WARN">WARN</option>
                  <option value="ERROR">ERROR</option>
                </select>
                <button
                  type="button"
                  className="btn btn-danger"
                  onClick={async () => {
                    await backendBridge.clearAuditLogs();
                    await refreshAll();
                    showToast("Đã làm sạch nhật ký vận hành");
                  }}
                >
                  Xóa Nhật ký
                </button>
              </div>
            </div>

            {filteredLogs.length === 0 ? (
              <p className="metric-sub">Chưa có bản ghi nhật ký nào khớp bộ lọc.</p>
            ) : (
              <div style={{ overflowX: "auto" }}>
                <table className="log-table">
                  <thead>
                    <tr>
                      <th>Thời gian</th>
                      <th>Mức</th>
                      <th>Sự kiện</th>
                      <th>Chuyển trạng thái</th>
                      <th>Nội dung</th>
                    </tr>
                  </thead>
                  <tbody>
                    {filteredLogs.map((item, i) => (
                      <tr key={`${item.timestamp}-${i}`}>
                        <td style={{ whiteSpace: "nowrap", color: "var(--text-secondary)" }}>
                          {new Date(item.timestamp).toLocaleTimeString("vi-VN")}
                        </td>
                        <td className={`lvl-${item.level}`}>{item.level}</td>
                        <td><code>{item.event}</code></td>
                        <td>
                          {item.fromState || item.toState
                            ? `${item.fromState ?? "-"} → ${item.toState ?? "-"}`
                            : "-"}
                        </td>
                        <td>{item.message}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </>
      )}
    </div>
  );
}

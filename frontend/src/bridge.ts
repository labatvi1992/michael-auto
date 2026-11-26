import {
  AgentWorkspaceStatus,
  AppConfig,
  AuditEntry,
  DiscoveredBrowserProfile,
  EvidenceRecord,
  RuntimeStatus
} from "./types";

interface WailsAppBindings {
  GetStatus(): Promise<RuntimeStatus>;
  GetConfig(): Promise<AppConfig>;
  SaveConfig(cfg: AppConfig): Promise<void>;
  SetAutomationEnabled(enabled: boolean): Promise<void>;
  SetDryRunActuation(dryRun: boolean): Promise<void>;
  ResetHardStop(): Promise<void>;
  GetAuditLogs(limit: number): Promise<AuditEntry[]>;
  ClearAuditLogs(): Promise<void>;
  ListRecentEvidence(limit: number): Promise<EvidenceRecord[]>;
  ClearEvidenceVault(): Promise<void>;
  GetEvidenceDataURL(imagePath: string): Promise<string>;
  SetupAgentWorkspace(): Promise<AgentWorkspaceStatus>;
  DiscoverBrowserProfiles(): Promise<DiscoveredBrowserProfile[]>;
  DiscoverAntigravityModels(): Promise<string[]>;
  GetAutostartEnabled(): Promise<boolean>;
  SetAutostartEnabled(enabled: boolean): Promise<void>;
  ExportDiagnostics(): Promise<string>;
  TriggerManualSwitch(profileId: string): Promise<void>;
  AuthorizeWaylandPortal(): Promise<string>;
  ProvisionCuaSidecar(): Promise<string>;
  EnableWaylandAccessibility(): Promise<string>;
  InspectUISelectors(): Promise<string>;
  TestCuaAction(action: string): Promise<string>;
  TestOpenBrowserProfile(profileId: string): Promise<void>;
  SimulateHumanInput(kind: string): Promise<RuntimeStatus>;
  SimulateQuotaExhausted(): Promise<RuntimeStatus>;
  SimulateAuthChallenge(): Promise<RuntimeStatus>;
}

interface WailsRuntime {
  EventsOn(eventName: string, callback: (data: any) => void): () => void;
}

declare global {
  interface Window {
    go?: {
      main?: {
        App?: WailsAppBindings;
      };
    };
    runtime?: WailsRuntime;
  }
}

const MOCK_PNG_DATA_URL =
  "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=";

let mockConfig: AppConfig = {
  version: 3,
  automationEnabled: true,
  dryRunActuation: true,
  yieldPauseSeconds: 30,
  cooldownSeconds: 15,
  pollIntervalMs: 2000,
  minConfidence: 0.95,
  consensusFrames: 2,
  pointerDeadzonePx: 4,
  maxSwitchesPerWindow: 3,
  switchWindowMinutes: 30,
  exhaustedCooldownMinutes: 60,
  intentTtlMinutes: 10,
  preferIntraProfileModelDrop: false,
  cliSettingsPath: "~/.gemini/antigravity-cli/settings.json",
  targetAppKeywords: ["Antigravity"],
  activeProfileId: "profile-primary",
  cuaDriver: {
    enabled: true,
    sessionName: "michael_auto",
    quotaCheckIntervalMs: 60000,
    minIdleBeforeFocusMs: 5000,
    signOutCooldownMs: 60000,
    browserSettleTimeoutMs: 15000,
    defaultModel: "",
    allowSameAccountRelogin: false,
    enableWayland: true,
    deliveryMode: "auto",
    agentWorkspaceEnabled: true,
    agentWorkspaceIndex: 1,
    agentWorkspaceName: "Agent Workspace",
    requireEvidenceBeforeActuate: true,
    maskEditorInEvidence: true,
    evidenceRetentionRuns: 20
  },
  profiles: [
    {
      id: "profile-primary",
      name: "Workspace Chính",
      emailHint: "p***@example.com",
      browserType: "chrome",
      browserProfileDir: "Default",
      primaryModel: "Gemini 4 Neon Extra High",
      fallbackModels: ["Claude Sonnet 4.6 (Thinking)"],
      enabled: true,
      priority: 1
    },
    {
      id: "profile-backup",
      name: "Workspace Dự phòng",
      emailHint: "b***@example.com",
      browserType: "chrome",
      browserProfileDir: "Profile 1",
      primaryModel: "Gemini 4 Neon Extra High",
      fallbackModels: ["Claude Sonnet 4.6 (Thinking)"],
      enabled: true,
      priority: 2
    }
  ]
};

let mockAutostart = false;
const mockConfigListeners = new Set<(cfg: AppConfig) => void>();

let mockStatus: RuntimeStatus = {
  state: "Observing",
  stateReason: "Đang giám sát định kỳ — hạn mức ổn định",
  automationEnabled: true,
  dryRunActuation: true,
  yieldRemainingMs: 0,
  yieldTotalMs: 30000,
  cooldownRemainingMs: 0,
  intentWaitRemainingMs: 0,
  nextQuotaCheckRemMs: 60000,
  activeProfileId: "profile-primary",
  activeProfileName: "Workspace Chính",
  activeModel: "Gemini 4 Neon Extra High",
  consensusCount: 0,
  consensusRequired: 2,
  switchesUsedInWindow: 0,
  maxSwitchesPerWindow: 3,
  circuitBreakerTripped: false,
  agentWorkspace: {
    enabled: true,
    ready: true,
    workspaceIndex: 1,
    workspaceName: "Agent Workspace",
    detail: "Đã dành riêng \"Agent Workspace\" (Workspace #2) + Cầu nối Native Wayland D-Bus đang hoạt động"
  },
  waylandPortalReady: true,
  waylandPortalDetail: "Sẵn sàng (trycua/cua — Agent Workspace + Evidence Gate)",
  displayServer: "wayland",
  sensors: [
    { tierName: "cli_config", healthy: true, detail: "Sẵn sàng đồng bộ settings.json", lastChecked: new Date().toISOString() },
    { tierName: "cua_driver", healthy: true, detail: "CUA sidecar hoạt động ổn định (michael_cua_sidecar JSON-RPC)", lastChecked: new Date().toISOString() },
    { tierName: "atspi2", healthy: true, detail: "Dự phòng chỉ-đọc qua AT-SPI2 D-Bus", lastChecked: new Date().toISOString() }
  ],
  screenLocked: false,
  quotaChecksTotal: 12,
  switchesCompletedTotal: 1,
  updatedAt: new Date().toISOString()
};

let mockLogs: AuditEntry[] = [
  {
    timestamp: new Date().toISOString(),
    level: "INFO",
    event: "system_initialized",
    toState: "Observing",
    message: "Khởi động Michael Auto v2.1 trên Agent Workspace cô lập (Fedora 45 Wayland)"
  }
];

let mockEvidence: EvidenceRecord[] = [
  {
    runId: "run_demo_01",
    step: "01_verify_quota_zero",
    createdAt: new Date().toISOString(),
    windowTitle: "Antigravity — Workspace",
    pid: 1001,
    verifiedDetail: "Xác minh bảng Quota về 0% & báo lỗi Quota exhausted trước khi SignOut",
    sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    imagePath: "~/.local/state/michael-auto/evidence/run_demo_01/01_verify_quota_zero.png",
    metaPath: "~/.local/state/michael-auto/evidence/run_demo_01/01_verify_quota_zero.meta.json"
  }
];

function wailsApp(): WailsAppBindings | undefined {
  return window.go?.main?.App;
}

export const backendBridge = {
  isDesktopRuntime(): boolean {
    return Boolean(window.runtime?.EventsOn && window.go?.main?.App);
  },

  async getStatus(): Promise<RuntimeStatus> {
    const app = wailsApp();
    if (app) return app.GetStatus();
    return { ...mockStatus, updatedAt: new Date().toISOString() };
  },

  async getConfig(): Promise<AppConfig> {
    const app = wailsApp();
    if (app) return app.GetConfig();
    return structuredClone(mockConfig);
  },

  async saveConfig(cfg: AppConfig): Promise<void> {
    const app = wailsApp();
    if (app) return app.SaveConfig(cfg);
    mockConfig = structuredClone(cfg);
    mockStatus.automationEnabled = cfg.automationEnabled;
    mockStatus.dryRunActuation = cfg.dryRunActuation;
    mockStatus.yieldTotalMs = cfg.yieldPauseSeconds * 1000;
    mockStatus.state = cfg.automationEnabled ? "Observing" : "PausedByUser";
  },

  async setAutomationEnabled(enabled: boolean): Promise<void> {
    const app = wailsApp();
    if (app) return app.SetAutomationEnabled(enabled);
    mockConfig.automationEnabled = enabled;
    mockStatus.automationEnabled = enabled;
    mockStatus.state = enabled ? "Observing" : "PausedByUser";
  },

  async setDryRunActuation(dryRun: boolean): Promise<void> {
    const app = wailsApp();
    if (app) return app.SetDryRunActuation(dryRun);
    mockConfig.dryRunActuation = dryRun;
    mockStatus.dryRunActuation = dryRun;
  },

  async resetHardStop(): Promise<void> {
    const app = wailsApp();
    if (app) return app.ResetHardStop();
    mockStatus.state = "Observing";
    mockStatus.circuitBreakerTripped = false;
    mockStatus.hardStopReason = undefined;
    mockStatus.stateReason = "Đã đặt lại chốt an toàn — tiếp tục quan sát";
  },

  async getAuditLogs(limit = 100): Promise<AuditEntry[]> {
    const app = wailsApp();
    if (app) return app.GetAuditLogs(limit);
    return [...mockLogs].slice(0, limit);
  },

  async clearAuditLogs(): Promise<void> {
    const app = wailsApp();
    if (app) return app.ClearAuditLogs();
    mockLogs = [];
  },

  async listRecentEvidence(limit = 30): Promise<EvidenceRecord[]> {
    const app = wailsApp();
    if (app) return app.ListRecentEvidence(limit);
    return [...mockEvidence].slice(0, limit);
  },

  async clearEvidenceVault(): Promise<void> {
    const app = wailsApp();
    if (app) return app.ClearEvidenceVault();
    mockEvidence = [];
  },

  async getEvidenceDataURL(imagePath: string): Promise<string> {
    const app = wailsApp();
    if (app) return app.GetEvidenceDataURL(imagePath);
    return MOCK_PNG_DATA_URL;
  },

  async setupAgentWorkspace(): Promise<AgentWorkspaceStatus> {
    const app = wailsApp();
    if (app) return app.SetupAgentWorkspace();
    mockStatus.agentWorkspace.ready = true;
    return mockStatus.agentWorkspace;
  },

  async discoverBrowserProfiles(): Promise<DiscoveredBrowserProfile[]> {
    const app = wailsApp();
    if (app) return app.DiscoverBrowserProfiles();
    return [
      { browserType: "chrome", profileDir: "Default", displayName: "Workspace Chính", emailHint: "primary@example.com" },
      { browserType: "chrome", profileDir: "Profile 1", displayName: "Workspace Dự phòng", emailHint: "backup@example.com" }
    ];
  },

  async discoverAntigravityModels(): Promise<string[]> {
    const app = wailsApp();
    if (app) return app.DiscoverAntigravityModels();
    return [
      "Gemini 4 Neon Extra High",
      "Gemini 4 Carbon Medium",
      "Claude Sonnet 4.6 (Thinking)",
      "Claude Opus 4.6 (Thinking)",
      "GPT-OSS 120B (Medium)"
    ];
  },

  async getAutostartEnabled(): Promise<boolean> {
    const app = wailsApp();
    if (app) return app.GetAutostartEnabled();
    return mockAutostart;
  },

  async setAutostartEnabled(enabled: boolean): Promise<void> {
    const app = wailsApp();
    if (app) return app.SetAutostartEnabled(enabled);
    mockAutostart = enabled;
  },

  async exportDiagnostics(): Promise<string> {
    const app = wailsApp();
    if (app) return app.ExportDiagnostics();
    return "~/.local/state/michael-auto/diagnostics/michael-auto-diagnostics-demo.tar.gz";
  },

  async triggerManualSwitch(profileId: string): Promise<void> {
    const app = wailsApp();
    if (app) return app.TriggerManualSwitch(profileId);
    mockStatus = {
      ...mockStatus,
      state: "VerifyingState",
      activeProfileId: profileId,
      intentWaitRemainingMs: 60000,
      stateReason: `Đang thực thi chuyển chủ động sang ${profileId} (chờ 60s sau SignOut)`
    };
  },

  async authorizeWaylandPortal(): Promise<string> {
    const app = wailsApp();
    if (app) return app.AuthorizeWaylandPortal();
    mockStatus.waylandPortalReady = true;
    return "Đã kết nối phiên CUA Driver sidecar (chế độ mô phỏng)";
  },

  async provisionCuaSidecar(): Promise<string> {
    const app = wailsApp();
    if (app) return app.ProvisionCuaSidecar();
    mockStatus.waylandPortalReady = true;
    return "Đã khởi tạo XDG venv & cài đặt cua-driver (chế độ mô phỏng)";
  },

  async enableWaylandAccessibility(): Promise<string> {
    const app = wailsApp();
    if (app) return app.EnableWaylandAccessibility();
    return "Đã bật nóng GNOME toolkit-accessibility + ScreenReaderEnabled (chế độ mô phỏng)";
  },

  async inspectUISelectors(): Promise<string> {
    const app = wailsApp();
    if (app) return app.InspectUISelectors();
    return JSON.stringify(
      {
        antigravityWindows: [
          {
            pid: 1001,
            title: "Antigravity — Workspace",
            elementCount: 48,
            hollowWindow: false,
            matchedSelectors: ["model_selector", "quota_button", "settings_button"]
          }
        ],
        chromeWindows: []
      },
      null,
      2
    );
  },

  async testCuaAction(action: string): Promise<string> {
    const app = wailsApp();
    if (app) return app.TestCuaAction(action);
    return `Đã thực thi thử bước CUA '${action}' thành công (chế độ mô phỏng)`;
  },

  async testOpenBrowserProfile(profileId: string): Promise<void> {
    const app = wailsApp();
    if (app) return app.TestOpenBrowserProfile(profileId);
  },

  async simulateHumanInput(kind: string): Promise<RuntimeStatus> {
    const app = wailsApp();
    if (app) return app.SimulateHumanInput(kind);
    mockStatus = {
      ...mockStatus,
      state: "PausedHumanInput",
      stateReason: `Phát hiện thao tác người dùng (${kind}) — tạm dừng trượt ${mockConfig.yieldPauseSeconds}s`,
      yieldRemainingMs: mockConfig.yieldPauseSeconds * 1000,
      yieldTotalMs: mockConfig.yieldPauseSeconds * 1000,
      lastActivityKind: kind,
      updatedAt: new Date().toISOString()
    };
    mockLogs.unshift({
      timestamp: new Date().toISOString(),
      level: "INFO",
      event: "human_input_yield",
      fromState: "Observing",
      toState: "PausedHumanInput",
      message: `Ngắt tức thời do thao tác ${kind}; bắt đầu đếm tĩnh lặng ${mockConfig.yieldPauseSeconds} giây`
    });
    return mockStatus;
  },

  async simulateQuotaExhausted(): Promise<RuntimeStatus> {
    const app = wailsApp();
    if (app) return app.SimulateQuotaExhausted();
    mockStatus = {
      ...mockStatus,
      state: "Cooldown",
      activeModel: "gemini-2.5-flash",
      stateReason: "Đã chuyển sang model dự phòng gemini-2.5-flash",
      updatedAt: new Date().toISOString()
    };
    return mockStatus;
  },

  async simulateAuthChallenge(): Promise<RuntimeStatus> {
    const app = wailsApp();
    if (app) return app.SimulateAuthChallenge();
    mockStatus = {
      ...mockStatus,
      state: "HardStopChallenge",
      circuitBreakerTripped: true,
      hardStopReason: "Phát hiện cổng bảo mật / xác minh: MFA / CAPTCHA",
      stateReason: "Dừng cứng an toàn: Phát hiện cổng bảo mật / xác minh",
      updatedAt: new Date().toISOString()
    };
    return mockStatus;
  },

  subscribeStatus(cb: (status: RuntimeStatus) => void): () => void {
    if (window.runtime?.EventsOn) {
      return window.runtime.EventsOn("runtime:status", cb);
    }
    return () => {};
  },

  subscribeConfig(cb: (cfg: AppConfig) => void): () => void {
    if (window.runtime?.EventsOn) {
      return window.runtime.EventsOn("config:updated", cb);
    }
    mockConfigListeners.add(cb);
    return () => {
      mockConfigListeners.delete(cb);
    };
  },

  __emitMockConfigUpdate(cfg: AppConfig): void {
    mockConfig = structuredClone(cfg);
    for (const cb of mockConfigListeners) {
      cb(structuredClone(cfg));
    }
  },

  __setMockStatusPatch(patch: Partial<RuntimeStatus>): void {
    mockStatus = { ...mockStatus, ...patch };
  }
};

export type FSMState =
  | "Observing"
  | "PreFlightCheck"
  | "AutoExecuting"
  | "PausedHumanInput"
  | "VerifyingState"
  | "Cooldown"
  | "HardStopChallenge"
  | "PausedByUser";

export type BrowserType =
  | "chrome"
  | "chromium"
  | "brave"
  | "edge"
  | "firefox"
  | "custom";

export interface DiscoveredBrowserProfile {
  browserType: BrowserType;
  profileDir: string;
  displayName: string;
  emailHint: string;
}

export interface AccountProfile {
  id: string;
  name: string;
  emailHint: string;
  browserType: BrowserType;
  browserBinary?: string;
  browserProfileDir: string;
  authStartUrl?: string;
  primaryModel: string;
  fallbackModels: string[];
  enabled: boolean;
  priority: number;
  exhaustedUntil?: string;
  exhaustedModels?: string[];
}

export interface CuaDriverConfig {
  enabled: boolean;
  pythonBin?: string;
  sessionName?: string;
  quotaCheckIntervalMs: number;
  minIdleBeforeFocusMs: number;
  signOutCooldownMs: number;
  browserSettleTimeoutMs: number;
  defaultModel?: string;
  allowSameAccountRelogin: boolean;
  enableWayland: boolean;
  deliveryMode?: string;
  agentWorkspaceEnabled: boolean;
  agentWorkspaceIndex: number;
  agentWorkspaceName?: string;
  requireEvidenceBeforeActuate: boolean;
  maskEditorInEvidence: boolean;
  evidenceRetentionRuns: number;
}

export interface AppConfig {
  version: number;
  automationEnabled: boolean;
  dryRunActuation: boolean;
  yieldPauseSeconds: number;
  cooldownSeconds: number;
  pollIntervalMs: number;
  minConfidence: number;
  consensusFrames: number;
  pointerDeadzonePx: number;
  maxSwitchesPerWindow: number;
  switchWindowMinutes: number;
  exhaustedCooldownMinutes: number;
  intentTtlMinutes: number;
  preferIntraProfileModelDrop: boolean;
  cliSettingsPath: string;
  targetAppKeywords: string[];
  activeProfileId: string;
  cuaDriver: CuaDriverConfig;
  profiles: AccountProfile[];
}

export interface SensorHealth {
  tierName: string;
  healthy: boolean;
  detail: string;
  lastChecked: string;
}

export interface AgentWorkspaceStatus {
  enabled: boolean;
  ready: boolean;
  workspaceIndex: number;
  workspaceName: string;
  detail: string;
}

export interface PendingIntent {
  runId?: string;
  kind: "switch_model" | "switch_profile";
  fromProfileId: string;
  targetProfileId: string;
  targetModel: string;
  createdAt: string;
  expiresAt: string;
  attempts: number;
  lastStep?: string;
  subStep?: string;
  nextActionAt?: string;
}

export interface ScreenState {
  observedAt: string;
  windowDetected: boolean;
  windowTitle?: string;
  windowFocused: boolean;
  windowPid?: number;
  appAuthState: string;
  activeAccountHint?: string;
  activeModel?: string;
  remainingQuotaPct?: number;
  quotaExhausted: boolean;
  quotaMessage?: string;
  evidencePath?: string;
  evidenceSha256?: string;
  authGateDetected: boolean;
  authGateReason?: string;
  isEditorContent: boolean;
  confidence: number;
  sourceTier: string;
}

export interface RuntimeStatus {
  appVersion?: string;
  state: FSMState;
  stateReason: string;
  automationEnabled: boolean;
  dryRunActuation: boolean;
  yieldRemainingMs: number;
  yieldTotalMs: number;
  lastActivityKind?: string;
  lastActivityAt?: string;
  cooldownRemainingMs: number;
  intentWaitRemainingMs?: number;
  nextQuotaCheckRemMs?: number;
  activeProfileId: string;
  activeProfileName: string;
  activeModel: string;
  consensusCount: number;
  consensusRequired: number;
  switchesUsedInWindow: number;
  maxSwitchesPerWindow: number;
  circuitBreakerTripped: boolean;
  circuitBreakerReason?: string;
  hardStopReason?: string;
  pendingIntent?: PendingIntent;
  lastObservation?: ScreenState;
  agentWorkspace: AgentWorkspaceStatus;
  waylandPortalReady: boolean;
  waylandPortalDetail: string;
  displayServer: string;
  sensors: SensorHealth[];
  screenLocked?: boolean;
  configWarning?: string;
  quotaChecksTotal?: number;
  switchesCompletedTotal?: number;
  lastSwitchAt?: string;
  updatedAt: string;
}

export interface AuditEntry {
  timestamp: string;
  level: "INFO" | "WARN" | "ERROR";
  event: string;
  fromState?: FSMState;
  toState?: FSMState;
  profileId?: string;
  model?: string;
  sourceTier?: string;
  confidence?: number;
  message: string;
  metadata?: Record<string, string>;
}

export interface EvidenceRecord {
  runId: string;
  step: string;
  createdAt: string;
  windowTitle: string;
  pid: number;
  processExe?: string;
  browserProfile?: string;
  omniboxOrigin?: string;
  verifiedDetail: string;
  redactedRegions?: number;
  sha256: string;
  imagePath: string;
  metaPath: string;
}

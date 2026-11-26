package domain

import "time"

// FSMState enumerates the re-entrant automation lifecycle states (ADR-002).
type FSMState string

const (
	StateObserving         FSMState = "Observing"
	StatePreFlightCheck    FSMState = "PreFlightCheck"
	StateAutoExecuting     FSMState = "AutoExecuting"
	StatePausedHumanInput  FSMState = "PausedHumanInput"
	StateVerifyingState    FSMState = "VerifyingState"
	StateCooldown          FSMState = "Cooldown"
	StateHardStopChallenge FSMState = "HardStopChallenge"
	StatePausedByUser      FSMState = "PausedByUser"
)

// ActivityKind classifies physical human input signals (mouse, wheel, click, keyboard).
type ActivityKind string

const (
	ActivityPointerMove   ActivityKind = "pointer_move"
	ActivityPointerButton ActivityKind = "pointer_button"
	ActivityPointerScroll ActivityKind = "pointer_scroll"
	ActivityKeyboard      ActivityKind = "keyboard"
	ActivityCompositorAny ActivityKind = "compositor_any"
)

// AuthState describes observed application authentication status.
type AuthState string

const (
	AuthUnknown       AuthState = "unknown"
	AuthAuthenticated AuthState = "authenticated"
	AuthLoggedOut     AuthState = "logged_out"
	AuthChallenge     AuthState = "auth_challenge"
)

// UIElementTarget identifies a discovered semantic UI element for actuation.
type UIElementTarget struct {
	Role         string `json:"role"`
	Name         string `json:"name"`
	ElementToken string `json:"elementToken,omitempty"`
	BusName      string `json:"busName,omitempty"`
	ObjectPath   string `json:"objectPath,omitempty"`
	ActionIndex  int    `json:"actionIndex"`
	CenterX      int    `json:"centerX"`
	CenterY      int    `json:"centerY"`
	HasBounds    bool   `json:"hasBounds"`
	HasAction    bool   `json:"hasAction"`
}

// ScreenState represents a normalized observation of the target application at a point in time.
type ScreenState struct {
	ObservedAt        time.Time                  `json:"observedAt"`
	WindowDetected    bool                       `json:"windowDetected"`
	WindowTitle       string                     `json:"windowTitle,omitempty"`
	WindowFocused     bool                       `json:"windowFocused"`
	WindowPID         int                        `json:"windowPid,omitempty"`
	ElementCount      int                        `json:"elementCount,omitempty"`
	HollowWindow      bool                       `json:"hollowWindow,omitempty"`
	AppAuthState      AuthState                  `json:"appAuthState"`
	ActiveAccountHint string                     `json:"activeAccountHint,omitempty"`
	ActiveModel       string                     `json:"activeModel,omitempty"`
	RemainingQuotaPct *int                       `json:"remainingQuotaPct,omitempty"`
	QuotaExhausted    bool                       `json:"quotaExhausted"`
	QuotaMessage      string                     `json:"quotaMessage,omitempty"`
	EvidencePath      string                     `json:"evidencePath,omitempty"`
	EvidenceSHA256    string                     `json:"evidenceSha256,omitempty"`
	AuthGateDetected  bool                       `json:"authGateDetected"`
	AuthGateReason    string                     `json:"authGateReason,omitempty"`
	IsEditorContent   bool                       `json:"isEditorContent"`
	Confidence        float64                    `json:"confidence"`
	SourceTier        string                     `json:"sourceTier"`
	Targets           map[string]UIElementTarget `json:"targets,omitempty"`
}

// IntentKind specifies what pending transition is being reconciled across interrupts.
type IntentKind string

const (
	IntentSwitchModel   IntentKind = "switch_model"
	IntentSwitchProfile IntentKind = "switch_profile"
)

// Non-blocking sub-steps for CUA 5-step account rotation & recovery workflow (ADR-007).
const (
	SubStepSignOutSettings      = "signout_settings"
	SubStepWaitBeforeSignIn     = "wait_before_signin"
	SubStepClickSignIn          = "click_signin"
	SubStepSelectChromeAccount  = "select_chrome_account"
	SubStepVerifyAndSelectModel = "verify_and_select_model"
)

// PendingIntent stores idempotent transition intent across 30s human-yield pauses
// and non-blocking timed waits. Persisted ephemerally and deleted immediately on completion.
type PendingIntent struct {
	RunID           string     `json:"runId,omitempty"`
	Kind            IntentKind `json:"kind"`
	FromProfileID   string     `json:"fromProfileId"`
	TargetProfileID string     `json:"targetProfileId"`
	TargetModel     string     `json:"targetModel"`
	CreatedAt       time.Time  `json:"createdAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	Attempts        int        `json:"attempts"`
	LastStep        string     `json:"lastStep,omitempty"`
	SubStep         string     `json:"subStep,omitempty"`
	NextActionAt    *time.Time `json:"nextActionAt,omitempty"`
}

// IsExpired reports whether the pending intent exceeded its TTL.
func (pi *PendingIntent) IsExpired(now time.Time) bool {
	if pi == nil {
		return true
	}
	return !now.Before(pi.ExpiresAt)
}

// SensorHealth tracks availability of each observation tier and OS portal.
type SensorHealth struct {
	TierName    string    `json:"tierName"`
	Healthy     bool      `json:"healthy"`
	Detail      string    `json:"detail"`
	LastChecked time.Time `json:"lastChecked"`
}

// AgentWorkspaceStatus describes the isolated Fedora 45 Wayland workspace dedicated to Agent automation.
type AgentWorkspaceStatus struct {
	Enabled        bool   `json:"enabled"`
	Ready          bool   `json:"ready"`
	WorkspaceIndex int    `json:"workspaceIndex"`
	WorkspaceName  string `json:"workspaceName"`
	Detail         string `json:"detail"`
}

// RuntimeStatus provides real-time telemetry to the Wails React dashboard and System Tray.
type RuntimeStatus struct {
	State                 FSMState             `json:"state"`
	StateReason           string               `json:"stateReason"`
	AutomationEnabled     bool                 `json:"automationEnabled"`
	DryRunActuation       bool                 `json:"dryRunActuation"`
	YieldRemainingMs      int64                `json:"yieldRemainingMs"`
	YieldTotalMs          int64                `json:"yieldTotalMs"`
	LastActivityKind      ActivityKind         `json:"lastActivityKind,omitempty"`
	LastActivityAt        *time.Time           `json:"lastActivityAt,omitempty"`
	CooldownRemainingMs   int64                `json:"cooldownRemainingMs"`
	IntentWaitRemainingMs int64                `json:"intentWaitRemainingMs,omitempty"`
	NextQuotaCheckRemMs   int64                `json:"nextQuotaCheckRemMs,omitempty"`
	ActiveProfileID       string               `json:"activeProfileId"`
	ActiveProfileName     string               `json:"activeProfileName"`
	ActiveModel           string               `json:"activeModel"`
	ConsensusCount        int                  `json:"consensusCount"`
	ConsensusRequired     int                  `json:"consensusRequired"`
	SwitchesUsedInWindow  int                  `json:"switchesUsedInWindow"`
	MaxSwitchesPerWindow  int                  `json:"maxSwitchesPerWindow"`
	CircuitBreakerTripped bool                 `json:"circuitBreakerTripped"`
	CircuitBreakerReason  string               `json:"circuitBreakerReason,omitempty"`
	HardStopReason        string               `json:"hardStopReason,omitempty"`
	PendingIntent         *PendingIntent       `json:"pendingIntent,omitempty"`
	LastObservation       *ScreenState         `json:"lastObservation,omitempty"`
	AgentWorkspace        AgentWorkspaceStatus `json:"agentWorkspace"`
	WaylandPortalReady    bool                 `json:"waylandPortalReady"`
	WaylandPortalDetail   string               `json:"waylandPortalDetail"`
	DisplayServer          string               `json:"displayServer"`
	Sensors                []SensorHealth       `json:"sensors"`
	ScreenLocked           bool                 `json:"screenLocked"`
	ConfigWarning          string               `json:"configWarning,omitempty"`
	QuotaChecksTotal       int                  `json:"quotaChecksTotal"`
	SwitchesCompletedTotal int                  `json:"switchesCompletedTotal"`
	LastSwitchAt           *time.Time           `json:"lastSwitchAt,omitempty"`
	UpdatedAt              time.Time            `json:"updatedAt"`
}

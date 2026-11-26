package domain

import "time"

// AuditLevel represents severity of an operational audit record.
type AuditLevel string

const (
	AuditInfo  AuditLevel = "INFO"
	AuditWarn  AuditLevel = "WARN"
	AuditError AuditLevel = "ERROR"
)

// AuditEntry is a single structured, PII-redacted JSONL operational event.
type AuditEntry struct {
	Timestamp  time.Time         `json:"timestamp"`
	Level      AuditLevel        `json:"level"`
	Event      string            `json:"event"`
	FromState  FSMState          `json:"fromState,omitempty"`
	ToState    FSMState          `json:"toState,omitempty"`
	ProfileID  string            `json:"profileId,omitempty"`
	Model      string            `json:"model,omitempty"`
	SourceTier string            `json:"sourceTier,omitempty"`
	Confidence float64           `json:"confidence,omitempty"`
	Message    string            `json:"message"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// RedactRect defines a window-relative pixel bounding box to mask before saving an Evidence screenshot.
type RedactRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// EvidenceRecord captures pre-actuation verification metadata and screenshot proof
// saved in the XDG Evidence Vault before executing sensitive operations.
type EvidenceRecord struct {
	RunID           string    `json:"runId"`
	Step            string    `json:"step"`
	CreatedAt       time.Time `json:"createdAt"`
	WindowTitle     string    `json:"windowTitle"`
	PID             int       `json:"pid"`
	ProcessExe      string    `json:"processExe,omitempty"`
	BrowserProfile  string    `json:"browserProfile,omitempty"`
	OmniboxOrigin   string    `json:"omniboxOrigin,omitempty"`
	VerifiedDetail  string    `json:"verifiedDetail"`
	RedactedRegions int       `json:"redactedRegions,omitempty"`
	SHA256          string    `json:"sha256"`
	ImagePath       string    `json:"imagePath"`
	MetaPath        string    `json:"metaPath"`
}

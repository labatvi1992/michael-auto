package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

// EphemeralIntentStore persists PendingIntent temporarily during an active account switch
// (preferring tmpfs $XDG_RUNTIME_DIR on Fedora 45) and strictly removes the file once completed.
type EphemeralIntentStore struct {
	mu   sync.Mutex
	path string
}

// DefaultEphemeralIntentPath returns $XDG_RUNTIME_DIR/michael-auto/pending_intent.json
// (falling back to $XDG_STATE_HOME/michael-auto/pending_intent.json).
func DefaultEphemeralIntentPath() string {
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		return filepath.Join(runtimeDir, "michael-auto", "pending_intent.json")
	}
	base := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, ".local", "state")
		} else {
			base = os.TempDir()
		}
	}
	return filepath.Join(base, "michael-auto", "pending_intent.json")
}

// NewEphemeralIntentStore initializes an EphemeralIntentStore.
func NewEphemeralIntentStore(customPath string) *EphemeralIntentStore {
	if strings.TrimSpace(customPath) == "" {
		customPath = DefaultEphemeralIntentPath()
	}
	return &EphemeralIntentStore{path: filepath.Clean(customPath)}
}

// Path returns the ephemeral intent file path.
func (s *EphemeralIntentStore) Path() string {
	return s.path
}

// Save atomically persists the active PendingIntent with 0600 permissions.
func (s *EphemeralIntentStore) Save(intent domain.PendingIntent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic0600(s.path, append(raw, '\n'))
}

// Load reads any surviving PendingIntent on startup; returns (nil, nil) if none exists.
func (s *EphemeralIntentStore) Load() (*domain.PendingIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var intent domain.PendingIntent
	if err := json.Unmarshal(raw, &intent); err != nil {
		_ = os.Remove(s.path)
		return nil, fmt.Errorf("tệp pending_intent lỗi định dạng (đã tự giải phóng): %w", err)
	}
	return &intent, nil
}

// Clear strictly removes the temporary PendingIntent file from disk/tmpfs once the workflow finishes or aborts.
func (s *EphemeralIntentStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

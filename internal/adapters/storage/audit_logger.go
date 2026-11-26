package storage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

const (
	defaultMaxLogBytes = 10 * 1024 * 1024 // 10 MB per log file
	defaultMaxBackups  = 3
	tailReadWindowSize = 128 * 1024 // O(1) 128 KB tail window for Recent() queries
)

var (
	emailRegex      = regexp.MustCompile(`([a-zA-Z0-9._%+\-])[a-zA-Z0-9._%+\-]*@([a-zA-Z0-9.\-]+\.[a-zA-Z]{2,})`)
	tokenRegex      = regexp.MustCompile(`(?i)(bearer\s+|ya29\.|aiZa|ghp_|token[=:\s]+)[a-zA-Z0-9._\-]{8,}`)
	oauthQueryRegex = regexp.MustCompile(`(?i)([?&](?:code|state|access_token|id_token|refresh_token|login_hint|code_challenge|code_challenge_method|code_verifier|nonce|client_id|flowName)=)[^&\s"']+`)
)

// JSONLAuditLogger persists structured, PII-redacted operational events under $XDG_STATE_HOME.
type JSONLAuditLogger struct {
	mu         sync.Mutex
	path       string
	homeDir    string
	maxBytes   int64
	maxBackups int
}

// DefaultXDGAuditPath returns $XDG_STATE_HOME/michael-auto/audit.jsonl.
func DefaultXDGAuditPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, ".local", "state")
		} else {
			base = os.TempDir()
		}
	}
	return filepath.Join(base, "michael-auto", "audit.jsonl")
}

// NewJSONLAuditLogger creates a rotating PII-redacting JSONL audit logger.
func NewJSONLAuditLogger(customPath string) *JSONLAuditLogger {
	if customPath == "" {
		customPath = DefaultXDGAuditPath()
	}
	home, _ := os.UserHomeDir()
	return &JSONLAuditLogger{
		path:       customPath,
		homeDir:    home,
		maxBytes:   defaultMaxLogBytes,
		maxBackups: defaultMaxBackups,
	}
}

// SetRotationLimits configures rotation thresholds (used in tests).
func (l *JSONLAuditLogger) SetRotationLimits(maxBytes int64, maxBackups int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if maxBytes > 0 {
		l.maxBytes = maxBytes
	}
	if maxBackups > 0 {
		l.maxBackups = maxBackups
	}
}

// Path returns the active JSONL log file path.
func (l *JSONLAuditLogger) Path() string {
	return l.path
}

// RedactString masks emails, secret tokens, OAuth query codes, and local home directory prefixes.
func (l *JSONLAuditLogger) RedactString(input string) string {
	if input == "" {
		return ""
	}
	out := tokenRegex.ReplaceAllString(input, "[REDACTED_TOKEN]")
	out = oauthQueryRegex.ReplaceAllString(out, "${1}[REDACTED]")
	out = emailRegex.ReplaceAllString(out, "${1}***@${2}")
	if l.homeDir != "" && l.homeDir != "/" {
		out = strings.ReplaceAll(out, l.homeDir, "~")
	}
	return out
}

// NewRedactingWriter wraps dst (e.g. os.Stderr for sidecar logs) so sensitive tokens/emails are masked in transit.
func (l *JSONLAuditLogger) NewRedactingWriter(dst io.Writer) io.Writer {
	return &redactingWriter{logger: l, dst: dst}
}

type redactingWriter struct {
	logger *JSONLAuditLogger
	dst    io.Writer
}

func (rw *redactingWriter) Write(p []byte) (int, error) {
	if rw.dst == nil {
		return len(p), nil
	}
	sanitized := rw.logger.RedactString(string(p))
	_, err := rw.dst.Write([]byte(sanitized))
	return len(p), err
}

// Log redacts PII and appends an entry to the JSONL log, rotating if necessary.
func (l *JSONLAuditLogger) Log(entry domain.AuditEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry.Message = l.RedactString(entry.Message)
	entry.ProfileID = l.RedactString(entry.ProfileID)
	if len(entry.Metadata) > 0 {
		cleanMeta := make(map[string]string, len(entry.Metadata))
		for k, v := range entry.Metadata {
			cleanMeta[k] = l.RedactString(v)
		}
		entry.Metadata = cleanMeta
	}

	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}

	l.rotateIfNeededLocked()

	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
}

func (l *JSONLAuditLogger) rotateIfNeededLocked() {
	info, err := os.Stat(l.path)
	if err != nil || info.Size() < l.maxBytes {
		return
	}

	for i := l.maxBackups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.path, i)
		dst := fmt.Sprintf("%s.%d", l.path, i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}
	_ = os.Rename(l.path, l.path+".1")
}

// Recent returns up to limit most recent entries in newest-first order using O(1) Tail-Seek.
func (l *JSONLAuditLogger) Recent(limit int) ([]domain.AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []domain.AuditEntry{}, nil
		}
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	var startOffset int64
	if info.Size() > tailReadWindowSize {
		startOffset = info.Size() - tailReadWindowSize
		if _, seekErr := f.Seek(startOffset, io.SeekStart); seekErr != nil {
			startOffset = 0
			_, _ = f.Seek(0, io.SeekStart)
		}
	}

	rawChunk, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	// If we sought into the middle of a line, discard the partial first line cleanly.
	if startOffset > 0 {
		if nl := bytes.IndexByte(rawChunk, '\n'); nl >= 0 {
			rawChunk = rawChunk[nl+1:]
		}
	}

	var all []domain.AuditEntry
	scanner := bufio.NewScanner(bytes.NewReader(rawChunk))
	scanner.Buffer(make([]byte, 64*1024), 512*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e domain.AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err == nil {
			all = append(all, e)
		}
	}

	n := len(all)
	if n > limit {
		all = all[n-limit:]
		n = limit
	}
	out := make([]domain.AuditEntry, n)
	for i := 0; i < n; i++ {
		out[i] = all[n-1-i]
	}
	return out, nil
}

// Clear truncates the active audit file when requested by the user.
func (l *JSONLAuditLogger) Clear() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Close satisfies ports.AuditLogger.
func (l *JSONLAuditLogger) Close() error {
	return nil
}

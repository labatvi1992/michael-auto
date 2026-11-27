//go:build linux || darwin

package storage

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// InstanceGuard enforces single-instance daemon execution via kernel flock() and exposes a 0600
// UNIX domain control socket so launching `michael-auto` a second time on GNOME Wayland cleanly
// wakes and raises the existing hidden window instead of spawning a split-brain second daemon.
type InstanceGuard struct {
	mu            sync.Mutex
	lockPath      string
	sockPath      string
	lockFile      *os.File
	listener      net.Listener
	onShowWindow  func()
	onControl     func(cmd string) string
	lastWakeReply string
	closed        bool
}

// DefaultInstanceRuntimePaths returns ($XDG_RUNTIME_DIR/michael-auto/daemon.lock, control.sock).
func DefaultInstanceRuntimePaths() (string, string) {
	base := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if base == "" {
		base = strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	}
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, ".local", "state")
		} else {
			base = os.TempDir()
		}
	}
	dir := filepath.Join(base, "michael-auto")
	sockPath := filepath.Join(dir, "control.sock")
	if len(sockPath) >= 100 {
		// Linux AF_UNIX sun_path hard limit is 108 bytes; fall back to short user-scoped tmp dir.
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("ma-%d", os.Getuid()))
		sockPath = filepath.Join(dir, "control.sock")
	}
	return filepath.Join(dir, "daemon.lock"), sockPath
}

// NewInstanceGuard creates a single-instance lock & wakeup socket guard.
func NewInstanceGuard(customDir string) *InstanceGuard {
	lockPath, sockPath := DefaultInstanceRuntimePaths()
	if strings.TrimSpace(customDir) != "" {
		lockPath = filepath.Join(customDir, "daemon.lock")
		sockPath = filepath.Join(customDir, "control.sock")
	}
	return &InstanceGuard{
		lockPath: lockPath,
		sockPath: sockPath,
	}
}

// SetShowWindowCallback updates the callback invoked when a secondary invocation sends "show-window".
func (g *InstanceGuard) SetShowWindowCallback(fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onShowWindow = fn
}

// SetControlCallback registers a handler for runtime control commands ("status", "pause", "resume", "quit").
func (g *InstanceGuard) SetControlCallback(fn func(cmd string) string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onControl = fn
}

// LastWakeReply returns the reply received from the running instance when lock acquisition yielded.
// For "show-window", a GUI instance replies "ok:gui" while a headless daemon replies "ok:headless".
func (g *InstanceGuard) LastWakeReply() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastWakeReply
}

// SendCommand sends a single-line control command to an already-running instance over control.sock.
func (g *InstanceGuard) SendCommand(command string) (string, error) {
	g.mu.Lock()
	sockPath := g.sockPath
	g.mu.Unlock()
	return sendControlCommand(sockPath, command)
}

// AcquireOrWakeExisting attempts to acquire the exclusive daemon lock.
// If another instance already holds the lock, it sends "show-window" to the running instance and returns acquired=false.
func (g *InstanceGuard) AcquireOrWakeExisting(onShowWindow func()) (bool, error) {
	return g.acquireWithFallbackCommand(onShowWindow, "show-window")
}

// AcquireWithoutWake attempts to acquire the exclusive daemon lock without popping up an existing GUI window.
// If another instance already holds the lock, it queries "status" over control.sock and returns acquired=false.
func (g *InstanceGuard) AcquireWithoutWake() (bool, error) {
	return g.acquireWithFallbackCommand(nil, "status")
}

func (g *InstanceGuard) acquireWithFallbackCommand(onShowWindow func(), fallbackCmd string) (bool, error) {
	g.mu.Lock()
	g.onShowWindow = onShowWindow
	g.lastWakeReply = ""
	lockPath := g.lockPath
	sockPath := g.sockPath
	g.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return false, fmt.Errorf("không tạo được thư mục runtime %s: %w", filepath.Dir(lockPath), err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("mở tệp khóa daemon %s lỗi: %w", lockPath, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			if fallbackCmd != "" {
				reply, _ := sendControlCommand(sockPath, fallbackCmd)
				g.mu.Lock()
				g.lastWakeReply = strings.TrimSpace(reply)
				g.mu.Unlock()
			}
			return false, nil
		}
		return false, fmt.Errorf("khóa flock %s lỗi: %w", lockPath, err)
	}

	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())

	_ = os.Remove(sockPath)
	ln, lnErr := net.Listen("unix", sockPath)
	if lnErr == nil {
		_ = os.Chmod(sockPath, 0o600)
		go g.acceptControlLoop(ln)
	}

	g.mu.Lock()
	g.lockFile = f
	g.listener = ln
	g.mu.Unlock()

	return true, nil
}

func (g *InstanceGuard) acceptControlLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			scanner := bufio.NewScanner(c)
			if !scanner.Scan() {
				return
			}
			cmd := strings.TrimSpace(scanner.Text())
			g.mu.Lock()
			cb := g.onShowWindow
			ctrl := g.onControl
			g.mu.Unlock()

			var reply string
			switch cmd {
			case "show-window":
				if cb != nil {
					cb()
					reply = "ok:gui"
				} else {
					reply = "ok:headless"
				}
			case "status", "pause", "resume", "quit":
				if ctrl != nil {
					reply = strings.TrimSpace(ctrl(cmd))
					if reply == "" {
						reply = "ok"
					}
				} else if cmd == "status" {
					if cb != nil {
						reply = "ok:gui"
					} else {
						reply = "ok:headless"
					}
				} else {
					reply = "unsupported"
				}
			default:
				reply = "err:unknown-command"
			}
			_, _ = c.Write([]byte(reply + "\n"))
		}(conn)
	}
}

func sendControlCommand(sockPath, command string) (string, error) {
	conn, err := net.DialTimeout("unix", sockPath, 1500*time.Millisecond)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(1500 * time.Millisecond))
	if _, err := conn.Write([]byte(strings.TrimSpace(command) + "\n")); err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text()), nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", nil
}

// Close releases the UNIX control socket and kernel flock cleanly.
func (g *InstanceGuard) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true

	if g.listener != nil {
		_ = g.listener.Close()
		g.listener = nil
	}
	if g.sockPath != "" {
		_ = os.Remove(g.sockPath)
	}
	if g.lockFile != nil {
		_ = syscall.Flock(int(g.lockFile.Fd()), syscall.LOCK_UN)
		_ = g.lockFile.Close()
		g.lockFile = nil
	}
	return nil
}

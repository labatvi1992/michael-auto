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
	mu           sync.Mutex
	lockPath     string
	sockPath     string
	lockFile     *os.File
	listener     net.Listener
	onShowWindow func()
	closed       bool
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
	return filepath.Join(dir, "daemon.lock"), filepath.Join(dir, "control.sock")
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

// AcquireOrWakeExisting attempts to acquire the exclusive daemon lock.
// If another instance already holds the lock, it sends "show-window" to the running instance and returns acquired=false.
func (g *InstanceGuard) AcquireOrWakeExisting(onShowWindow func()) (bool, error) {
	g.mu.Lock()
	g.onShowWindow = onShowWindow
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
			_ = sendWakeupCommand(sockPath, "show-window")
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
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			scanner := bufio.NewScanner(c)
			if scanner.Scan() {
				cmd := strings.TrimSpace(scanner.Text())
				if cmd == "show-window" {
					g.mu.Lock()
					cb := g.onShowWindow
					g.mu.Unlock()
					if cb != nil {
						cb()
					}
					_, _ = c.Write([]byte("ok\n"))
				}
			}
		}(conn)
	}
}

func sendWakeupCommand(sockPath, command string) error {
	conn, err := net.DialTimeout("unix", sockPath, 1500*time.Millisecond)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(1500 * time.Millisecond))
	_, err = conn.Write([]byte(command + "\n"))
	return err
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

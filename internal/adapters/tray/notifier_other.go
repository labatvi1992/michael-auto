//go:build !linux

package tray

type DesktopNotifier struct{}

func NewDesktopNotifier() *DesktopNotifier                       { return &DesktopNotifier{} }
func (n *DesktopNotifier) Notify(title, body string, crit bool)  {}
func IsStatusNotifierHostAvailable() bool                        { return true }

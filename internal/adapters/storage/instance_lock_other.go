//go:build !linux && !darwin

package storage

type InstanceGuard struct{}

func NewInstanceGuard(customDir string) *InstanceGuard                 { return &InstanceGuard{} }
func (g *InstanceGuard) SetShowWindowCallback(fn func())               {}
func (g *InstanceGuard) SetControlCallback(fn func(cmd string) string) {}
func (g *InstanceGuard) LastWakeReply() string                         { return "" }
func (g *InstanceGuard) SendCommand(command string) (string, error)    { return "", nil }
func (g *InstanceGuard) AcquireOrWakeExisting(fn func()) (bool, error) { return true, nil }
func (g *InstanceGuard) AcquireWithoutWake() (bool, error)             { return true, nil }
func (g *InstanceGuard) Close() error                                  { return nil }

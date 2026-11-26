//go:build !linux && !darwin

package storage

type InstanceGuard struct{}

func NewInstanceGuard(customDir string) *InstanceGuard            { return &InstanceGuard{} }
func (g *InstanceGuard) SetShowWindowCallback(fn func())          {}
func (g *InstanceGuard) AcquireOrWakeExisting(fn func()) (bool, error) { return true, nil }
func (g *InstanceGuard) Close() error                             { return nil }

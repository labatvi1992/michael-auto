//go:build !linux

package workspace

import (
	"context"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

type StubWorkspaceManager struct{}

func NewWorkspaceManager() *StubWorkspaceManager {
	return &StubWorkspaceManager{}
}

func (m *StubWorkspaceManager) Status(ctx context.Context, cfg domain.AppConfig) domain.AgentWorkspaceStatus {
	return domain.AgentWorkspaceStatus{
		Enabled:        cfg.CuaDriver.AgentWorkspaceEnabled,
		Ready:          true,
		WorkspaceIndex: cfg.CuaDriver.AgentWorkspaceIndex,
		WorkspaceName:  cfg.CuaDriver.AgentWorkspaceName,
		Detail:         "Stub workspace manager (non-Linux)",
	}
}

func (m *StubWorkspaceManager) EnsureAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (domain.AgentWorkspaceStatus, error) {
	return m.Status(ctx, cfg), nil
}

func (m *StubWorkspaceManager) RouteTargetWindowsToAgentWorkspace(ctx context.Context, cfg domain.AppConfig) error {
	return nil
}

func (m *StubWorkspaceManager) UserIsOnAgentWorkspace(ctx context.Context, cfg domain.AppConfig) (bool, bool) {
	return true, false
}

func (m *StubWorkspaceManager) UninstallGnomeBridgeExtension(ctx context.Context) error {
	return nil
}

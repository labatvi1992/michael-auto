package orchestrator

import (
	"sort"
	"strings"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

// PolicyActionType classifies the orchestrator decision when quota is exhausted.
type PolicyActionType string

const (
	PolicyNoOp          PolicyActionType = "noop"
	PolicySwitchModel   PolicyActionType = "switch_model"
	PolicySwitchProfile PolicyActionType = "switch_profile"
	PolicyAllExhausted  PolicyActionType = "all_exhausted"
)

// PolicyDecision encapsulates the target profile and model selected by PolicyEngine.
type PolicyDecision struct {
	Action          PolicyActionType
	CurrentProfile  string
	TargetProfileID string
	TargetModel     string
	Reason          string
}

// PolicyEngine evaluates intra-account model fallback chains and cross-profile failover pools (ADR-004, ADR-007).
type PolicyEngine struct{}

// NewPolicyEngine returns a new PolicyEngine.
func NewPolicyEngine() *PolicyEngine {
	return &PolicyEngine{}
}

// ResolveActiveProfile matches observed account hints against configured profiles,
// falling back to cfg.ActiveProfileID or the first enabled profile.
func (pe *PolicyEngine) ResolveActiveProfile(cfg *domain.AppConfig, obs domain.ScreenState) *domain.AccountProfile {
	hint := strings.TrimSpace(strings.ToLower(obs.ActiveAccountHint))
	if hint != "" {
		for i := range cfg.Profiles {
			p := &cfg.Profiles[i]
			if strings.EqualFold(p.ID, hint) ||
				(p.EmailHint != "" && strings.Contains(hint, strings.ToLower(p.EmailHint))) ||
				(p.Name != "" && strings.Contains(hint, strings.ToLower(p.Name))) {
				return p
			}
		}
	}
	if active := cfg.FindProfile(cfg.ActiveProfileID); active != nil {
		return active
	}
	for i := range cfg.Profiles {
		if cfg.Profiles[i].Enabled {
			return &cfg.Profiles[i]
		}
	}
	if len(cfg.Profiles) > 0 {
		return &cfg.Profiles[0]
	}
	return nil
}

// DecideQuotaRemediation updates exhaustion markers on cfg and determines whether to drop
// to a fallback model on the current profile, switch to the next pre-logged-in Chrome profile,
// or re-authenticate via trycua/cua when configured.
func (pe *PolicyEngine) DecideQuotaRemediation(
	cfg *domain.AppConfig,
	obs domain.ScreenState,
	now time.Time,
	cuaEngineActive bool,
) PolicyDecision {
	current := pe.ResolveActiveProfile(cfg, obs)
	if current == nil {
		return PolicyDecision{
			Action: PolicyAllExhausted,
			Reason: "Chưa có hồ sơ tài khoản nào được kích hoạt",
		}
	}

	activeModel := strings.TrimSpace(obs.ActiveModel)
	if activeModel == "" {
		activeModel = resolvePreferredModel(cfg, *current)
	}

	// If the screen shows an unconfigured account email while current configured profile has a different email,
	// switch directly into the configured profile without marking the configured profile exhausted!
	obsHint := strings.TrimSpace(strings.ToLower(obs.ActiveAccountHint))
	if obsHint != "" && current.EmailHint != "" &&
		!strings.Contains(obsHint, strings.ToLower(current.EmailHint)) &&
		current.Enabled && !current.IsExhausted(now) {
		return PolicyDecision{
			Action:          PolicySwitchProfile,
			CurrentProfile:  obsHint,
			TargetProfileID: current.ID,
			TargetModel:     resolvePreferredModel(cfg, *current, obs.ActiveModel),
			Reason:          "Chuyển từ phiên " + obs.ActiveAccountHint + " sang hồ sơ cấu hình " + current.Name,
		}
	}

	// Mark the currently active model exhausted on this profile.
	if activeModel != "" && !containsFold(current.ExhaustedModels, activeModel) {
		current.ExhaustedModels = append(current.ExhaustedModels, activeModel)
	}

	// Step 1: Intra-profile fallback model check (if enabled by user policy).
	if cfg.PreferIntraProfileModelDrop && current.Enabled && !current.IsExhausted(now) {
		if nextModel, ok := current.NextAvailableFallback(activeModel, now); ok {
			return PolicyDecision{
				Action:          PolicySwitchModel,
				CurrentProfile:  current.ID,
				TargetProfileID: current.ID,
				TargetModel:     nextModel,
				Reason:          "Chuyển sang model dự phòng trên cùng hồ sơ " + current.Name,
			}
		}
	}

	// Step 2: Current profile has no remaining usable models; mark profile exhausted for cooldown window.
	exhaustUntil := now.Add(time.Duration(cfg.ExhaustedCooldownMinutes) * time.Minute)
	current.ExhaustedUntil = &exhaustUntil

	// Step 3: Find the highest-priority enabled, non-exhausted alternative profile.
	candidates := make([]domain.AccountProfile, 0, len(cfg.Profiles))
	for _, p := range cfg.Profiles {
		if !p.Enabled || p.ID == current.ID || p.IsExhausted(now) {
			continue
		}
		candidates = append(candidates, p)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Priority < candidates[j].Priority
	})

	if len(candidates) > 0 {
		next := candidates[0]
		targetModel := resolvePreferredModel(cfg, next, activeModel)
		if next.IsModelExhausted(targetModel, now) {
			if fb, ok := next.NextAvailableFallback(targetModel, now); ok {
				targetModel = fb
			}
		}
		return PolicyDecision{
			Action:          PolicySwitchProfile,
			CurrentProfile:  current.ID,
			TargetProfileID: next.ID,
			TargetModel:     targetModel,
			Reason:          "Chuyển tự động từ hồ sơ " + current.Name + " sang " + next.Name,
		}
	}

	// Step 4: Single-account CUA relogin mode (SignOut -> Wait 1m -> SignIn Chrome -> Default Model).
	// Strictly disabled when operating a multi-profile pool (enabledCount >= 2, 1 Account = 1 --profile-directory)
	// so exhausting all accounts in the pool stops cleanly with PolicyAllExhausted instead of relogging an exhausted profile!
	enabledCount := 0
	for _, p := range cfg.Profiles {
		if p.Enabled {
			enabledCount++
		}
	}
	if enabledCount <= 1 && cuaEngineActive && cfg.CuaDriver.Enabled && cfg.CuaDriver.AllowSameAccountRelogin && current.Enabled {
		current.ExhaustedUntil = nil
		current.ExhaustedModels = nil
		targetModel := resolvePreferredModel(cfg, *current, activeModel)
		return PolicyDecision{
			Action:          PolicySwitchProfile,
			CurrentProfile:  current.ID,
			TargetProfileID: current.ID,
			TargetModel:     targetModel,
			Reason:          "Thực hiện chu trình SignOut 1 phút và đăng nhập lại hồ sơ " + current.Name,
		}
	}

	return PolicyDecision{
		Action:         PolicyAllExhausted,
		CurrentProfile: current.ID,
		Reason:         "Tất cả hồ sơ tài khoản được bật và model dự phòng đều đã chạm hạn mức",
	}
}

func resolvePreferredModel(cfg *domain.AppConfig, prof domain.AccountProfile, observedActiveModel ...string) string {
	obsModel := ""
	if len(observedActiveModel) > 0 {
		obsModel = strings.TrimSpace(observedActiveModel[0])
	}
	if cfg != nil && cfg.CuaDriver.Enabled {
		override := strings.TrimSpace(cfg.CuaDriver.DefaultModel)
		if strings.EqualFold(override, "preserve_active") {
			if obsModel != "" {
				return obsModel
			}
			return strings.TrimSpace(prof.PrimaryModel)
		}
		if override != "" {
			return override
		}
	}
	if strings.EqualFold(strings.TrimSpace(prof.PrimaryModel), "preserve_active") && obsModel != "" {
		return obsModel
	}
	return strings.TrimSpace(prof.PrimaryModel)
}

func containsFold(items []string, target string) bool {
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}

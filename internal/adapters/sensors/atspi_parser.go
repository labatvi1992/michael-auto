package sensors

import (
	"strings"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

// AccessibleNode represents a cross-platform snapshot of an AT-SPI2 accessible UI node.
// Separating tree analysis from D-Bus transport allows 100% deterministic Replay Snapshot Tests on CI.
type AccessibleNode struct {
	Role        string           `json:"role"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Text        string           `json:"text,omitempty"`
	BusName     string           `json:"busName,omitempty"`
	ObjectPath  string           `json:"objectPath,omitempty"`
	Focused     bool             `json:"focused,omitempty"`
	Editable    bool             `json:"editable,omitempty"`
	HasAction   bool             `json:"hasAction,omitempty"`
	ActionIndex int              `json:"actionIndex,omitempty"`
	X           int              `json:"x,omitempty"`
	Y           int              `json:"y,omitempty"`
	Width       int              `json:"width,omitempty"`
	Height      int              `json:"height,omitempty"`
	Children    []AccessibleNode `json:"children,omitempty"`
}

var (
	quotaPhrases = []string{
		"quota exceeded",
		"quota exhausted",
		"usage limit reached",
		"rate limit exceeded",
		"out of quota",
		"you've reached your usage limit",
		"model quota reached",
		"hết hạn mức",
		"đã chạm hạn mức",
		"vượt quá hạn mức",
	}

	authChallengePhrases = []string{
		"verify you are human",
		"recaptcha",
		"hcaptcha",
		"cloudflare turnstile",
		"captcha",
		"2-step verification",
		"two-factor authentication",
		"enter your password",
		"authenticator code",
		"xác minh 2 bước",
		"xác minh bạn là con người",
		"nhập mật khẩu",
	}

	// NOTE (v2.5.0 / P0-3): NEVER include "document web" here!
	// In Electron (Antigravity 2.34+), the root `document web` container wraps 100% of the app UI.
	editorContainerRoles = map[string]bool{
		"document text":  true,
		"document frame": true,
		"source code":    true,
		"terminal":       true,
		"text":           true,
		"entry":          true,
		"textbox":        true,
		"text box":       true,
		"paragraph":      true,
	}

	// Passive conversation/Markdown roles inside Antigravity IDE shielded from false HardStop/Quota triggers.
	conversationContentRoles = map[string]bool{
		"paragraph":     true,
		"block quote":   true,
		"table":         true,
		"table row":     true,
		"table cell":    true,
		"column header": true,
		"heading":       true,
		"code":          true,
	}

	// Explicit modal/alert/notification containers where Auth/MFA & Quota alerts legitimately appear in the IDE.
	systemSurfaceRoles = map[string]bool{
		"dialog":       true,
		"alert dialog": true,
		"alert":        true,
		"notification": true,
		"info bar":     true,
		"status bar":   true,
		"banner":       true,
		"menu":         true,
		"popup menu":   true,
		"list box":     true,
	}

	alertContainerRoles = map[string]bool{
		"alert":        true,
		"notification": true,
		"info bar":     true,
		"status bar":   true,
		"dialog":       true,
		"alert dialog": true,
		"banner":       true,
		"label":        true,
		"panel":        true,
	}
)

// AnalyzeAccessibleTree inspects top-level windows and extracts normalized ScreenState
// while strictly excluding code editor and Markdown chat subtrees from false-positive quota/MFA matching.
func AnalyzeAccessibleTree(roots []AccessibleNode, targetKeywords []string, profiles []domain.AccountProfile) domain.ScreenState {
	state := domain.ScreenState{
		ObservedAt:   time.Now(),
		AppAuthState: domain.AuthUnknown,
		SourceTier:   "atspi2",
		Targets:      make(map[string]domain.UIElementTarget),
	}

	if len(targetKeywords) == 0 {
		targetKeywords = []string{"antigravity"}
	}

	for _, win := range roots {
		// Check browser or dialog windows for CAPTCHA/MFA challenges if focused (100% sensitivity),
		// and check target application windows for quota & auth state with IDE chat shield.
		isTargetWin := matchesAnyKeyword(win.Name+" "+win.Description, targetKeywords)
		if !isTargetWin {
			if win.Focused {
				if gate, reason := detectAuthGateInSubtree(win); gate {
					state.AuthGateDetected = true
					state.AuthGateReason = reason
					state.AppAuthState = domain.AuthChallenge
					return state
				}
			}
			continue
		}

		state.WindowDetected = true
		state.WindowTitle = win.Name
		state.WindowFocused = state.WindowFocused || win.Focused
		state.Confidence = 0.90

		inspectTargetNode(win, false, false, false, profiles, &state)
	}

	return state
}

func isValidIDEControlNode(node AccessibleNode, cleaned string, insideSystem bool) bool {
	roleLower := strings.ToLower(strings.TrimSpace(node.Role))
	if node.Editable || editorContainerRoles[roleLower] || conversationContentRoles[roleLower] {
		return false
	}
	if roleLower == "list item" && !insideSystem {
		return false
	}
	if roleLower != "push button" && roleLower != "button" && roleLower != "menu item" && roleLower != "combo box" {
		return false
	}
	if cleaned == "" || len(cleaned) > 90 || strings.HasPrefix(cleaned, "•") {
		return false
	}
	if node.Height > 0 && (node.Y+node.Height) <= 0 {
		return false
	}
	return true
}

func inspectTargetNode(
	node AccessibleNode,
	insideEditor bool,
	insideChat bool,
	insideSystem bool,
	profiles []domain.AccountProfile,
	state *domain.ScreenState,
) {
	roleLower := strings.ToLower(strings.TrimSpace(node.Role))
	nowSystem := insideSystem || systemSurfaceRoles[roleLower] || (roleLower == "landmark" && strings.Contains(strings.ToLower(node.Name), "notification"))
	combined := strings.TrimSpace(strings.ReplaceAll(node.Name+" "+node.Description+" "+node.Text, "\ufffc", " "))
	combinedLower := strings.ToLower(combined)

	// 1. Non-bypassable Hard-Stop Check: Password inputs anywhere in IDE; MFA/CAPTCHA phrases inside system surfaces!
	if roleLower == "password text" {
		state.AuthGateDetected = true
		state.AuthGateReason = "Phát hiện ô nhập mật khẩu xác thực (" + node.Name + ")"
		state.AppAuthState = domain.AuthChallenge
		return
	}

	// 2. Determine if entering an editable code/document buffer or passive Markdown chat subtree (P0-3).
	isEditorNode := insideEditor || node.Editable || editorContainerRoles[roleLower]
	isChatNode := insideChat || (!nowSystem && (conversationContentRoles[roleLower] || roleLower == "list item"))

	if !isEditorNode && !isChatNode && combinedLower != "" {
		if nowSystem {
			for _, phrase := range authChallengePhrases {
				if strings.Contains(combinedLower, phrase) {
					state.AuthGateDetected = true
					state.AuthGateReason = "Phát hiện cổng bảo mật / xác minh: " + phrase
					state.AppAuthState = domain.AuthChallenge
					return
				}
			}
		}

		// Detect account identity hints from non-editor chrome labels/buttons.
		for _, p := range profiles {
			if p.EmailHint != "" && strings.Contains(combinedLower, strings.ToLower(p.EmailHint)) {
				state.ActiveAccountHint = p.EmailHint
				if state.AppAuthState == domain.AuthUnknown {
					state.AppAuthState = domain.AuthAuthenticated
				}
			}
		}

		// Detect semantic interactive buttons for zero-coordinate actuation!
		if isValidIDEControlNode(node, combined, nowSystem) {
			target := buildTarget(node)
			switch {
			case containsAny(combinedLower, []string{"log out", "logout", "sign out", "đăng xuất"}):
				state.Targets["logout_button"] = target
				if state.AppAuthState == domain.AuthUnknown {
					state.AppAuthState = domain.AuthAuthenticated
				}
			case containsAny(combinedLower, []string{"sign in", "log in", "login", "đăng nhập", "sign in with google"}):
				state.Targets["sign_in_button"] = target
				state.AppAuthState = domain.AuthLoggedOut
			case containsAny(combinedLower, []string{"settings", "preferences", "cài đặt"}):
				state.Targets["settings_button"] = target
				if state.AppAuthState == domain.AuthUnknown {
					state.AppAuthState = domain.AuthAuthenticated
				}
			case containsAny(combinedLower, []string{"select model", "model selection", "gemini", "claude", "orchestrator"}):
				state.Targets["model_selector"] = target
				if state.ActiveModel == "" {
					nameClean := strings.TrimSpace(node.Name)
					if idx := strings.Index(strings.ToLower(nameClean), "current:"); idx >= 0 {
						nameClean = strings.TrimSpace(nameClean[idx+len("current:"):])
					}
					state.ActiveModel = nameClean
				}
			}
		}

		// Detect quota exhaustion strictly inside UI chrome / alert / system roles!
		if alertContainerRoles[roleLower] || nowSystem || roleLower == "" {
			for _, qp := range quotaPhrases {
				if strings.Contains(combinedLower, qp) {
					state.QuotaExhausted = true
					state.QuotaMessage = strings.TrimSpace(combined)
					state.Confidence = 0.97
					break
				}
			}
		}
	} else if isEditorNode || isChatNode {
		for _, qp := range quotaPhrases {
			if strings.Contains(combinedLower, qp) && !state.QuotaExhausted {
				state.IsEditorContent = true
			}
		}
	}

	for _, child := range node.Children {
		inspectTargetNode(child, isEditorNode, isChatNode, nowSystem, profiles, state)
		if state.AuthGateDetected {
			return
		}
	}
}

func detectAuthGateInSubtree(node AccessibleNode) (bool, string) {
	roleLower := strings.ToLower(strings.TrimSpace(node.Role))
	if roleLower == "password text" {
		return true, "Phát hiện yêu cầu nhập mật khẩu trên cửa sổ xác thực"
	}
	if !node.Editable {
		combinedLower := strings.ToLower(node.Name + " " + node.Description + " " + node.Text)
		for _, phrase := range authChallengePhrases {
			if strings.Contains(combinedLower, phrase) {
				return true, "Phát hiện yêu cầu xác minh bảo mật (" + phrase + ")"
			}
		}
	}
	for _, c := range node.Children {
		if ok, r := detectAuthGateInSubtree(c); ok {
			return true, r
		}
	}
	return false, ""
}

func buildTarget(node AccessibleNode) domain.UIElementTarget {
	hasBounds := node.Width > 0 && node.Height > 0
	return domain.UIElementTarget{
		Role:        node.Role,
		Name:        node.Name,
		BusName:     node.BusName,
		ObjectPath:  node.ObjectPath,
		ActionIndex: node.ActionIndex,
		CenterX:     node.X + node.Width/2,
		CenterY:     node.Y + node.Height/2,
		HasBounds:   hasBounds,
		HasAction:   node.HasAction,
	}
}

func matchesAnyKeyword(text string, keywords []string) bool {
	lower := strings.ToLower(text)
	for _, kw := range keywords {
		k := strings.ToLower(strings.TrimSpace(kw))
		if k != "" && strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

func containsAny(lowerText string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(lowerText, n) {
			return true
		}
	}
	return false
}

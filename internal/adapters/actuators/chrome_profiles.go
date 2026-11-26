package actuators

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

type chromiumLocalState struct {
	Profile struct {
		InfoCache map[string]struct {
			Name           string `json:"name"`
			UserName       string `json:"user_name"`
			GaiaName       string `json:"gaia_name"`
			ShortcutName   string `json:"shortcut_name"`
			IsUsingDefault bool   `json:"is_using_default_name"`
		} `json:"info_cache"`
	} `json:"profile"`
}

type chromiumProfilePreferences struct {
	AccountInfo []struct {
		Email    string `json:"email"`
		FullName string `json:"full_name"`
	} `json:"account_info"`
}

// DiscoverBrowserProfiles scans read-only Chromium/Chrome/Brave/Edge "Local State" metadata
// (plus read-only `<ProfileDir>/Preferences` `account_info[0].email` fallback when Chrome Sync is off,
// NEVER touching SQLite Cookies or Login Data) to list exact `--profile-directory` names and email hints.
func DiscoverBrowserProfiles(customConfigBase string) []domain.DiscoveredBrowserProfile {
	base := strings.TrimSpace(customConfigBase)
	if base == "" {
		base = os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				base = filepath.Join(home, ".config")
			}
		}
	}
	if base == "" {
		return nil
	}

	targets := []struct {
		bType      domain.BrowserType
		browserDir string
	}{
		{domain.BrowserChrome, "google-chrome"},
		{domain.BrowserChromium, "chromium"},
		{domain.BrowserBrave, filepath.Join("BraveSoftware", "Brave-Browser")},
		{domain.BrowserEdge, "microsoft-edge"},
	}

	var out []domain.DiscoveredBrowserProfile
	for _, t := range targets {
		browserRoot := filepath.Join(base, t.browserDir)
		stateFile := filepath.Join(browserRoot, "Local State")
		raw, err := os.ReadFile(stateFile)
		if err != nil {
			continue
		}
		var parsed chromiumLocalState
		if json.Unmarshal(raw, &parsed) != nil || len(parsed.Profile.InfoCache) == 0 {
			continue
		}

		dirs := make([]string, 0, len(parsed.Profile.InfoCache))
		for dir := range parsed.Profile.InfoCache {
			dirs = append(dirs, dir)
		}
		sort.Slice(dirs, func(i, j int) bool {
			if dirs[i] == "Default" {
				return true
			}
			if dirs[j] == "Default" {
				return false
			}
			return dirs[i] < dirs[j]
		})

		for _, dir := range dirs {
			meta := parsed.Profile.InfoCache[dir]
			disp := strings.TrimSpace(meta.Name)
			if disp == "" {
				disp = strings.TrimSpace(meta.GaiaName)
			}
			email := strings.TrimSpace(meta.UserName)
			if email == "" || disp == "" {
				prefEmail, prefName := readPreferencesEmailFallback(filepath.Join(browserRoot, dir, "Preferences"))
				if email == "" {
					email = prefEmail
				}
				if disp == "" {
					disp = prefName
				}
			}
			if disp == "" {
				disp = dir
			}
			out = append(out, domain.DiscoveredBrowserProfile{
				BrowserType: t.bType,
				ProfileDir:  dir,
				DisplayName: disp,
				EmailHint:   email,
			})
		}
	}
	return out
}

func readPreferencesEmailFallback(prefsPath string) (email string, fullName string) {
	raw, err := os.ReadFile(prefsPath)
	if err != nil {
		return "", ""
	}
	var prefs chromiumProfilePreferences
	if json.Unmarshal(raw, &prefs) != nil || len(prefs.AccountInfo) == 0 {
		return "", ""
	}
	for _, acc := range prefs.AccountInfo {
		if em := strings.TrimSpace(acc.Email); em != "" {
			return em, strings.TrimSpace(acc.FullName)
		}
	}
	return "", ""
}

// LookupBrowserProfileMeta returns the exact DisplayName and signed-in EmailHint recorded
// in Local State / Preferences for a specific (BrowserType, BrowserProfileDir) pair.
func LookupBrowserProfileMeta(bType domain.BrowserType, profileDir string, customConfigBase string) (displayName string, emailHint string, found bool) {
	all := DiscoverBrowserProfiles(customConfigBase)
	for _, item := range all {
		if item.BrowserType == bType && item.ProfileDir == strings.TrimSpace(profileDir) {
			return item.DisplayName, item.EmailHint, true
		}
	}
	return "", "", false
}

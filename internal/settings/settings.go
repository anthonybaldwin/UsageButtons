// Package settings manages global and per-key plugin settings.
package settings

import (
	"os"
	"strings"
	"sync"
)

// RefreshPresets are the allowed refresh intervals in minutes.
var RefreshPresets = []int{5, 10, 15, 30, 60}

const DefaultRefreshMinutes = 15

type TextSize string

const (
	TextSmall  TextSize = "small"
	TextMedium TextSize = "medium"
	TextLarge  TextSize = "large"
)

// GlobalSettings are shared across every key and persisted by
// Stream Deck (survive plugin rebuilds, ride with user profiles).
type GlobalSettings struct {
	DefaultRefreshMinutes *int         `json:"defaultRefreshMinutes,omitempty"`
	DefaultValueSize      TextSize     `json:"defaultValueSize,omitempty"`
	DefaultSubvalueSize   TextSize     `json:"defaultSubvalueSize,omitempty"`
	InvertFill            bool         `json:"invertFill,omitempty"`
	ShowGlyphs            *bool        `json:"showGlyphs,omitempty"`
	SkipUpdateCheck       bool         `json:"skipUpdateCheck,omitempty"`
	CookieHostOptedOut    bool         `json:"cookieHostOptedOut,omitempty"`
	ProviderKeys          ProviderKeys `json:"providerKeys,omitempty"`
}

// ProviderKeys holds user-entered credentials and endpoint overrides
// from the Property Inspector. Fields are empty when the user hasn't
// provided one; resolvers fall back to environment variables in that
// case. Persisted by Stream Deck in the global settings blob, so
// survives plugin rebuilds.
type ProviderKeys struct {
	// API keys / tokens
	OpenRouterKey string `json:"openRouterKey,omitempty"`
	WarpKey       string `json:"warpKey,omitempty"`
	ZaiKey        string `json:"zaiKey,omitempty"`
	KimiK2Key     string `json:"kimiK2Key,omitempty"`
	CopilotToken  string `json:"copilotToken,omitempty"`

	// Endpoint overrides
	OpenRouterURL      string `json:"openRouterURL,omitempty"`
	ZaiHost            string `json:"zaiHost,omitempty"`
	ZaiQuotaURL        string `json:"zaiQuotaURL,omitempty"`
	ZaiRegion          string `json:"zaiRegion,omitempty"` // "global" | "bigmodel-cn"
	CodexChatGPTBaseURL string `json:"codexChatGPTBaseURL,omitempty"`
}

// KeySettings are per-button settings stored by Stream Deck.
type KeySettings struct {
	// Provider is now derived from action UUID; this field is
	// kept for backwards compat but ignored.
	ProviderID     string   `json:"providerId,omitempty"`
	MetricID       string   `json:"metricId,omitempty"`
	RefreshMinutes *int     `json:"refreshMinutes,omitempty"`
	WarnBelow      *float64 `json:"warnBelow,omitempty"`
	CriticalBelow  *float64 `json:"criticalBelow,omitempty"`
	WarnColor      string   `json:"warnColor,omitempty"`
	CriticalColor  string   `json:"criticalColor,omitempty"`
	LabelOverride  string   `json:"labelOverride,omitempty"`
	HideLabel      bool     `json:"hideLabel,omitempty"`
	CaptionOverride string  `json:"captionOverride,omitempty"`
	FillColor      string   `json:"fillColor,omitempty"`
	BgColor        string   `json:"bgColor,omitempty"`
	TextColor      string   `json:"textColor,omitempty"`
	FillDirection  string   `json:"fillDirection,omitempty"`
	ValueSize      TextSize `json:"valueSize,omitempty"`
	SubvalueSize   TextSize `json:"subvalueSize,omitempty"`
	ShowBorder     *bool    `json:"showBorder,omitempty"`
	ShowGlyph      *bool    `json:"showGlyph,omitempty"`
	ShowResetTimer *bool    `json:"showResetTimer,omitempty"`
	ShowRawCounts  *bool    `json:"showRawCounts,omitempty"`
	HideSubvalue   bool     `json:"hideSubvalue,omitempty"`
}

// --- Global settings singleton ---

var (
	mu      sync.RWMutex
	current = GlobalSettings{
		DefaultValueSize:    TextLarge,
		DefaultSubvalueSize: TextLarge,
	}
)

// Set replaces the global settings, normalising values.
func Set(gs GlobalSettings) {
	mu.Lock()
	defer mu.Unlock()

	// Normalise refresh
	if gs.DefaultRefreshMinutes != nil && !isValidRefresh(*gs.DefaultRefreshMinutes) {
		d := DefaultRefreshMinutes
		gs.DefaultRefreshMinutes = &d
	}

	// Normalise text sizes
	gs.DefaultValueSize = normaliseTextSize(gs.DefaultValueSize, TextLarge)
	gs.DefaultSubvalueSize = normaliseTextSize(gs.DefaultSubvalueSize, TextLarge)

	current = gs
}

// Get returns the current global settings.
func Get() GlobalSettings {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// DefaultValueSz returns the global default value text size.
func DefaultValueSz() TextSize {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultValueSize == "" {
		return TextLarge
	}
	return current.DefaultValueSize
}

// DefaultSubvalueSz returns the global default subvalue text size.
func DefaultSubvalueSz() TextSize {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultSubvalueSize == "" {
		return TextLarge
	}
	return current.DefaultSubvalueSize
}

// InvertFillEnabled returns the global invert-fill toggle.
func InvertFillEnabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return current.InvertFill
}

// ShowGlyphsEnabled returns the global show-glyphs toggle.
func ShowGlyphsEnabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	if current.ShowGlyphs == nil {
		return true
	}
	return *current.ShowGlyphs
}

// SkipUpdateCheckEnabled returns the skip-update-check toggle.
func SkipUpdateCheckEnabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return current.SkipUpdateCheck
}

// ResolveRefreshMs returns the effective refresh interval in ms for a key.
func ResolveRefreshMs(ks KeySettings) int64 {
	if ks.RefreshMinutes != nil && isValidRefresh(*ks.RefreshMinutes) {
		return int64(*ks.RefreshMinutes) * 60 * 1000
	}
	mu.RLock()
	defer mu.RUnlock()
	mins := DefaultRefreshMinutes
	if current.DefaultRefreshMinutes != nil {
		mins = *current.DefaultRefreshMinutes
	}
	return int64(mins) * 60 * 1000
}

func isValidRefresh(n int) bool {
	for _, p := range RefreshPresets {
		if n == p {
			return true
		}
	}
	return false
}

func normaliseTextSize(raw TextSize, fallback TextSize) TextSize {
	switch raw {
	case TextSmall, TextMedium, TextLarge:
		return raw
	default:
		return fallback
	}
}

// ProviderKeysGet returns a snapshot of the per-provider credential
// and endpoint overrides from global settings.
func ProviderKeysGet() ProviderKeys {
	mu.RLock()
	defer mu.RUnlock()
	return current.ProviderKeys
}

// ChangedProviderIDs returns the provider IDs whose credentials or
// endpoint overrides differ between prev and next. Callers use this
// to invalidate cached provider snapshots so the next poll picks up
// the new configuration instead of serving stale data.
func ChangedProviderIDs(prev, next ProviderKeys) []string {
	var out []string
	if prev.OpenRouterKey != next.OpenRouterKey ||
		prev.OpenRouterURL != next.OpenRouterURL {
		out = append(out, "openrouter")
	}
	if prev.WarpKey != next.WarpKey {
		out = append(out, "warp")
	}
	if prev.ZaiKey != next.ZaiKey ||
		prev.ZaiHost != next.ZaiHost ||
		prev.ZaiQuotaURL != next.ZaiQuotaURL ||
		prev.ZaiRegion != next.ZaiRegion {
		out = append(out, "zai")
	}
	if prev.KimiK2Key != next.KimiK2Key {
		out = append(out, "kimi-k2")
	}
	if prev.CopilotToken != next.CopilotToken {
		out = append(out, "copilot")
	}
	if prev.CodexChatGPTBaseURL != next.CodexChatGPTBaseURL {
		out = append(out, "codex")
	}
	return out
}

// ResolveAPIKey returns the first non-empty credential from: the
// user-supplied value (typically a PI settings field) or the named
// environment variables in order. Values are trimmed and stripped
// of surrounding quotes. Returns "" when nothing is set.
func ResolveAPIKey(fromUser string, envNames ...string) string {
	if v := cleanCredential(fromUser); v != "" {
		return v
	}
	for _, name := range envNames {
		if v := cleanCredential(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// ResolveEndpoint returns the first non-empty endpoint from: the
// user-supplied settings field, the named environment variables, or
// the provided default. Trims trailing slashes.
func ResolveEndpoint(fromUser string, defaultURL string, envNames ...string) string {
	pick := func(raw string) string {
		v := strings.TrimSpace(raw)
		if v == "" {
			return ""
		}
		return strings.TrimRight(v, "/")
	}
	if v := pick(fromUser); v != "" {
		return v
	}
	for _, name := range envNames {
		if v := pick(os.Getenv(name)); v != "" {
			return v
		}
	}
	return strings.TrimRight(defaultURL, "/")
}

// cleanCredential trims whitespace and strips a single set of
// matched surrounding quotes (single or double) — copy/paste from a
// shell .env file often includes them.
func cleanCredential(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			v = strings.TrimSpace(v[1 : len(v)-1])
		}
	}
	return v
}


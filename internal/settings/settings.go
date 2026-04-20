// Package settings manages global and per-key plugin settings.
package settings

import (
	"os"
	"strings"
	"sync"
)

// RefreshPresets are the allowed refresh intervals in minutes.
var RefreshPresets = []int{5, 10, 15, 30, 60}

// DefaultRefreshMinutes is the built-in fallback refresh interval in
// minutes when neither the user nor the provider overrides it.
const DefaultRefreshMinutes = 15

// TextSize is the discriminator for value / subvalue font-size buckets
// exposed in the Property Inspector.
type TextSize string

// TextSize presets. Values match the strings the PI persists.
const (
	// TextSmall is the small text-size preset persisted by the PI.
	TextSmall TextSize = "small"
	// TextMedium is the medium text-size preset persisted by the PI.
	TextMedium TextSize = "medium"
	// TextLarge is the large text-size preset persisted by the PI.
	TextLarge TextSize = "large"
)

// GlobalSettings are shared across every key and persisted by
// Stream Deck (survive plugin rebuilds, ride with user profiles).
type GlobalSettings struct {
	DefaultRefreshMinutes *int                        `json:"defaultRefreshMinutes,omitempty"`
	DefaultValueSize      TextSize                    `json:"defaultValueSize,omitempty"`
	DefaultSubvalueSize   TextSize                    `json:"defaultSubvalueSize,omitempty"`
	DefaultTextColor      string                      `json:"defaultTextColor,omitempty"`
	DefaultFillColor      string                      `json:"defaultFillColor,omitempty"`
	DefaultBgColor        string                      `json:"defaultBgColor,omitempty"`
	DefaultShowBorder     *bool                       `json:"defaultShowBorder,omitempty"`
	DefaultFillDirection  string                      `json:"defaultFillDirection,omitempty"`
	DefaultShowResetTimer *bool                       `json:"defaultShowResetTimer,omitempty"`
	DefaultShowRawCounts  *bool                       `json:"defaultShowRawCounts,omitempty"`
	DefaultHideSubvalue   *bool                       `json:"defaultHideSubvalue,omitempty"`
	DefaultWarnBelow      *float64                    `json:"defaultWarnBelow,omitempty"`
	DefaultWarnColor      string                      `json:"defaultWarnColor,omitempty"`
	DefaultCriticalBelow  *float64                    `json:"defaultCriticalBelow,omitempty"`
	DefaultCriticalColor  string                      `json:"defaultCriticalColor,omitempty"`
	InvertFill            bool                        `json:"invertFill,omitempty"`
	ShowGlyphs            *bool                       `json:"showGlyphs,omitempty"`
	SkipUpdateCheck       bool                        `json:"skipUpdateCheck,omitempty"`
	CookieHostOptedOut    bool                        `json:"cookieHostOptedOut,omitempty"`
	ProviderKeys          ProviderKeys                `json:"providerKeys,omitempty"`
	// ProviderSettings are per-provider overrides that sit between the
	// plugin-wide defaults above and the per-button KeySettings. Only
	// fields that make sense at the provider tier are overridable —
	// see ProviderSettings for the set. Keyed by provider ID ("claude",
	// "codex", "zai", ...).
	ProviderSettings map[string]ProviderSettings `json:"providerSettings,omitempty"`
}

// ProviderSettings carries per-provider overrides. Every field is
// optional; an unset field means "inherit from the plugin default".
// At render time these values are merged under the per-button
// KeySettings via EffectiveSettings, so a field set here applies to
// every button for that provider unless the button overrides it too.
type ProviderSettings struct {
	RefreshMinutes *int     `json:"refreshMinutes,omitempty"`
	WarnBelow      *float64 `json:"warnBelow,omitempty"`
	CriticalBelow  *float64 `json:"criticalBelow,omitempty"`
	WarnColor      string   `json:"warnColor,omitempty"`
	CriticalColor  string   `json:"criticalColor,omitempty"`
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
	HideSubvalue   *bool    `json:"hideSubvalue,omitempty"`
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
	HideSubvalue   *bool    `json:"hideSubvalue,omitempty"`
}

// EffectiveSettings merges provider-tier overrides under per-button
// settings so the caller sees a single resolved KeySettings. Per-button
// values win; provider overrides fill in any fields the button didn't
// set; plugin-wide defaults are applied at read time by the individual
// getters (DefaultValueSz, ResolveRefreshMs, ...). This keeps the
// precedence chain plugin -> provider -> button without every call
// site having to walk it.
func EffectiveSettings(ks KeySettings, providerID string) KeySettings {
	ps := providerSettingsFor(providerID)
	if ks.RefreshMinutes == nil && ps.RefreshMinutes != nil {
		v := *ps.RefreshMinutes
		ks.RefreshMinutes = &v
	}
	if ks.WarnBelow == nil && ps.WarnBelow != nil {
		v := *ps.WarnBelow
		ks.WarnBelow = &v
	}
	if ks.CriticalBelow == nil && ps.CriticalBelow != nil {
		v := *ps.CriticalBelow
		ks.CriticalBelow = &v
	}
	if ks.WarnColor == "" {
		ks.WarnColor = ps.WarnColor
	}
	if ks.CriticalColor == "" {
		ks.CriticalColor = ps.CriticalColor
	}
	if ks.FillColor == "" {
		ks.FillColor = ps.FillColor
	}
	if ks.BgColor == "" {
		ks.BgColor = ps.BgColor
	}
	if ks.TextColor == "" {
		ks.TextColor = ps.TextColor
	}
	if ks.FillDirection == "" {
		ks.FillDirection = ps.FillDirection
	}
	if ks.ValueSize == "" {
		ks.ValueSize = ps.ValueSize
	}
	if ks.SubvalueSize == "" {
		ks.SubvalueSize = ps.SubvalueSize
	}
	if ks.ShowBorder == nil && ps.ShowBorder != nil {
		v := *ps.ShowBorder
		ks.ShowBorder = &v
	}
	if ks.ShowGlyph == nil && ps.ShowGlyph != nil {
		v := *ps.ShowGlyph
		ks.ShowGlyph = &v
	}
	if ks.ShowResetTimer == nil && ps.ShowResetTimer != nil {
		v := *ps.ShowResetTimer
		ks.ShowResetTimer = &v
	}
	if ks.ShowRawCounts == nil && ps.ShowRawCounts != nil {
		v := *ps.ShowRawCounts
		ks.ShowRawCounts = &v
	}
	if ks.HideSubvalue == nil && ps.HideSubvalue != nil {
		v := *ps.HideSubvalue
		ks.HideSubvalue = &v
	}
	return ks
}

// providerSettingsFor returns the provider-tier override block for a
// given provider ID. Returns a zero value when the user hasn't set any
// override for that provider (every field then falls through to the
// plugin defaults).
func providerSettingsFor(providerID string) ProviderSettings {
	mu.RLock()
	defer mu.RUnlock()
	if current.ProviderSettings == nil {
		return ProviderSettings{}
	}
	return current.ProviderSettings[providerID]
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

// DefaultTextColorValue returns the plugin-wide text default, falling
// back to the historical hardcoded value when unset.
func DefaultTextColorValue() string {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultTextColor != "" {
		return current.DefaultTextColor
	}
	return "#f9fafb"
}

// DefaultShowBorderEnabled returns the plugin-wide border default,
// which is on when unset — matches the pre-setting behavior.
func DefaultShowBorderEnabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultShowBorder == nil {
		return true
	}
	return *current.DefaultShowBorder
}

// DefaultFillDirectionValue returns the plugin-wide fill direction
// default ("up" / "down" / "right" / "left"), falling back to "up"
// when unset.
func DefaultFillDirectionValue() string {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultFillDirection != "" {
		return current.DefaultFillDirection
	}
	return "up"
}

// DefaultShowResetTimerEnabled returns the plugin-wide reset-timer
// default, on when unset. Only has a visible effect on metrics whose
// type includes a timer (pct, pace).
func DefaultShowResetTimerEnabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultShowResetTimer == nil {
		return true
	}
	return *current.DefaultShowResetTimer
}

// DefaultShowRawCountsEnabled returns the plugin-wide raw-counts
// default. Off by default — the render path auto-enables for credit
// providers when the API returns counts, so a user typically only
// flips this on to force raw counts for a percent-only metric.
func DefaultShowRawCountsEnabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultShowRawCounts == nil {
		return false
	}
	return *current.DefaultShowRawCounts
}

// DefaultHideSubvalueEnabled returns the plugin-wide hide-subtext
// default. Off by default — users typically want to see the subtext.
func DefaultHideSubvalueEnabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultHideSubvalue == nil {
		return false
	}
	return *current.DefaultHideSubvalue
}

// DefaultFillColorValue returns the plugin-wide fill-color default,
// or empty string when unset. When set, it overrides the per-provider
// brand color for meter metrics. Reference cards still use their
// lightened-bg trick regardless — that's a visual differentiation
// users don't think of as "the fill color".
func DefaultFillColorValue() string {
	mu.RLock()
	defer mu.RUnlock()
	return current.DefaultFillColor
}

// DefaultBgColorValue returns the plugin-wide background-color
// default. When set, overrides the per-provider brand bg. Empty means
// fall through to brand bg.
func DefaultBgColorValue() string {
	mu.RLock()
	defer mu.RUnlock()
	return current.DefaultBgColor
}

// DefaultWarnBelowValue returns the plugin-wide warn-threshold default
// and true when the user has set one. Returns (0, false) when unset,
// in which case the per-metric-type smart default applies.
func DefaultWarnBelowValue() (float64, bool) {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultWarnBelow == nil {
		return 0, false
	}
	return *current.DefaultWarnBelow, true
}

// DefaultCriticalBelowValue — same pattern as DefaultWarnBelowValue.
func DefaultCriticalBelowValue() (float64, bool) {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultCriticalBelow == nil {
		return 0, false
	}
	return *current.DefaultCriticalBelow, true
}

// DefaultWarnColorValue returns the plugin-wide warn color, falling
// back to the historical #f59e0b when unset.
func DefaultWarnColorValue() string {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultWarnColor != "" {
		return current.DefaultWarnColor
	}
	return "#f59e0b"
}

// DefaultCriticalColorValue — same pattern as DefaultWarnColorValue.
func DefaultCriticalColorValue() string {
	mu.RLock()
	defer mu.RUnlock()
	if current.DefaultCriticalColor != "" {
		return current.DefaultCriticalColor
	}
	return "#ef4444"
}

// SkipUpdateCheckEnabled returns the skip-update-check toggle.
func SkipUpdateCheckEnabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return current.SkipUpdateCheck
}

// ResolveRefreshMs returns the effective refresh interval in ms for a
// key. Precedence: button RefreshMinutes -> provider RefreshMinutes
// -> plugin DefaultRefreshMinutes -> built-in DefaultRefreshMinutes.
func ResolveRefreshMs(ks KeySettings, providerID string) int64 {
	if ks.RefreshMinutes != nil && isValidRefresh(*ks.RefreshMinutes) {
		return int64(*ks.RefreshMinutes) * 60 * 1000
	}
	if ps := providerSettingsFor(providerID); ps.RefreshMinutes != nil && isValidRefresh(*ps.RefreshMinutes) {
		return int64(*ps.RefreshMinutes) * 60 * 1000
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


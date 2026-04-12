// Package settings manages global and per-key plugin settings.
package settings

import "sync"

// RefreshPresets are the allowed refresh intervals in minutes.
var RefreshPresets = []int{5, 10, 15, 30, 60}

const DefaultRefreshMinutes = 15

type TextSize string

const (
	TextSmall  TextSize = "small"
	TextMedium TextSize = "medium"
	TextLarge  TextSize = "large"
)

type ProviderSource string

const (
	SourceOAuth  ProviderSource = "oauth"
	SourceCookie ProviderSource = "cookie"
	SourceBoth   ProviderSource = "both"
)

// ClaudeProviderSettings holds Claude-specific auth config.
type ClaudeProviderSettings struct {
	Source       ProviderSource `json:"source,omitempty"`
	CookieHeader string        `json:"cookieHeader,omitempty"`
}

// CursorProviderSettings holds Cursor-specific auth config.
type CursorProviderSettings struct {
	CookieHeader string `json:"cookieHeader,omitempty"`
}

// ProviderSettingsMap holds per-provider config.
type ProviderSettingsMap struct {
	Claude *ClaudeProviderSettings `json:"claude,omitempty"`
	Cursor *CursorProviderSettings `json:"cursor,omitempty"`
}

// GlobalSettings are shared across every key and persisted by
// Stream Deck (survive plugin rebuilds, ride with user profiles).
type GlobalSettings struct {
	DefaultRefreshMinutes *int                 `json:"defaultRefreshMinutes,omitempty"`
	DefaultValueSize      TextSize             `json:"defaultValueSize,omitempty"`
	DefaultSubvalueSize   TextSize             `json:"defaultSubvalueSize,omitempty"`
	InvertFill            bool                 `json:"invertFill,omitempty"`
	ShowGlyphs            *bool                `json:"showGlyphs,omitempty"`
	SkipUpdateCheck       bool                 `json:"skipUpdateCheck,omitempty"`
	Providers             *ProviderSettingsMap `json:"providers,omitempty"`
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

	// Normalise claude source
	if gs.Providers != nil && gs.Providers.Claude != nil {
		gs.Providers.Claude.Source = normaliseSource(gs.Providers.Claude.Source)
	}

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

// ClaudeSettings returns Claude provider settings with defaults.
func ClaudeSettings() ClaudeProviderSettings {
	mu.RLock()
	defer mu.RUnlock()
	if current.Providers != nil && current.Providers.Claude != nil {
		return *current.Providers.Claude
	}
	return ClaudeProviderSettings{Source: SourceBoth}
}

// CursorSettings returns Cursor provider settings.
func CursorSettings() CursorProviderSettings {
	mu.RLock()
	defer mu.RUnlock()
	if current.Providers != nil && current.Providers.Cursor != nil {
		return *current.Providers.Cursor
	}
	return CursorProviderSettings{}
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

func normaliseSource(raw ProviderSource) ProviderSource {
	switch raw {
	case SourceOAuth, SourceCookie:
		return raw
	default:
		return SourceBoth
	}
}

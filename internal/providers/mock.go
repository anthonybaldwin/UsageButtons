package providers

import (
	"math"
	"time"
)

// MockProvider generates deterministic sine-wave data for development.
type MockProvider struct{}

// ID returns the provider identifier used by the registry.
func (MockProvider) ID() string { return "mock" }

// Name returns the human-readable provider name.
func (MockProvider) Name() string { return "Mock" }

// BrandColor returns the accent color used on button faces.
func (MockProvider) BrandColor() string { return "#3b82f6" }

// BrandBg returns the background color used on button faces.
func (MockProvider) BrandBg() string { return "#111827" }

// MetricIDs enumerates the metrics this provider emits.
func (MockProvider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent", "credits"}
}

// Fetch returns a synthetic Snapshot driven by time-based sine waves.
func (MockProvider) Fetch(_ FetchContext) (Snapshot, error) {
	t := float64(time.Now().UnixMilli()) / 1000.0

	sessionVal := 50 + 50*math.Sin(t/300)
	weeklyVal := 50 + 50*math.Sin(t/600)
	credits := 50 + 50*math.Cos(t/900)

	sessionRatio := sessionVal / 100
	weeklyRatio := weeklyVal / 100

	resetSec := 18000 - math.Mod(t, 18000) // 5h window

	sv := math.Round(sessionVal)
	wv := math.Round(weeklyVal)
	cv := math.Round(credits*100) / 100

	return Snapshot{
		ProviderID:   "mock",
		ProviderName: "Mock",
		Source:       "mock",
		Metrics: []MetricValue{
			{
				ID:            "session-percent",
				Label:         "SESSION",
				Name:          "Session % remaining",
				Value:         sv,
				NumericValue:  &sv,
				NumericUnit:   "percent",
				Unit:          "%",
				Ratio:         &sessionRatio,
				Direction:     "up",
				ResetInSeconds: &resetSec,
			},
			{
				ID:           "weekly-percent",
				Label:        "WEEKLY",
				Name:         "Weekly % remaining",
				Value:        wv,
				NumericValue: &wv,
				NumericUnit:  "percent",
				Unit:         "%",
				Ratio:        &weeklyRatio,
				Direction:    "up",
			},
			{
				ID:          "credits",
				Label:       "CREDITS",
				Name:        "Credits remaining",
				Value:       cv,
				NumericValue: &cv,
				NumericUnit: "count",
			},
		},
	}, nil
}

// init registers MockProvider with the package-wide registry on import.
func init() {
	Register(MockProvider{})
}

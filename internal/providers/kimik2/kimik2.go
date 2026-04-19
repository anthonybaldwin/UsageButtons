// Package kimik2 implements the Kimi K2 credits provider.
//
// Auth: Property Inspector settings field or KIMI_K2_API_KEY /
// KIMI_API_KEY / KIMI_KEY environment variable.
// Endpoint: GET https://kimi-k2.ai/api/user/credits
package kimik2

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const creditsURL = "https://kimi-k2.ai/api/user/credits"

func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().KimiK2Key,
		"KIMI_K2_API_KEY", "KIMI_API_KEY", "KIMI_KEY",
	)
}

// --- Flexible response parsing ---

// dig traverses a nested map[string]any along a path and returns a float if found.
func dig(obj any, path []string) (float64, bool) {
	current := obj
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return 0, false
		}
		current = m[key]
		if current == nil {
			return 0, false
		}
	}
	switch v := current.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

var consumedPaths = [][]string{
	{"total_credits_consumed"},
	{"totalCreditsConsumed"},
	{"credits_consumed"},
	{"creditsConsumed"},
	{"consumedCredits"},
	{"usedCredits"},
	{"total"},
	{"usage", "total"},
	{"usage", "consumed"},
	{"data", "usage", "total_credits_consumed"},
	{"data", "total_credits_consumed"},
}

var remainingPaths = [][]string{
	{"credits_remaining"},
	{"creditsRemaining"},
	{"remaining_credits"},
	{"remainingCredits"},
	{"available_credits"},
	{"availableCredits"},
	{"credits_left"},
	{"creditsLeft"},
	{"usage", "credits_remaining"},
	{"usage", "remaining"},
	{"data", "usage", "credits_remaining"},
	{"data", "credits_remaining"},
}

func extractCredits(body map[string]any) (consumed float64, consumedOk bool, remaining float64, remainingOk bool) {
	for _, path := range consumedPaths {
		if v, ok := dig(body, path); ok {
			consumed = v
			consumedOk = true
			break
		}
	}
	for _, path := range remainingPaths {
		if v, ok := dig(body, path); ok {
			remaining = v
			remainingOk = true
			break
		}
	}
	return
}

// Provider fetches Kimi K2 usage data.
type Provider struct{}

func (Provider) ID() string         { return "kimi-k2" }
func (Provider) Name() string       { return "Kimi K2" }
func (Provider) BrandColor() string { return "#0071e3" }
func (Provider) BrandBg() string    { return "#0a1225" }
func (Provider) MetricIDs() []string {
	return []string{"credits-balance"}
}

func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providers.Snapshot{
			ProviderID:   "kimi-k2",
			ProviderName: "Kimi K2",
			Source:       "none",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Enter a Kimi K2 API key in plugin settings, or set KIMI_K2_API_KEY.",
		}, nil
	}

	var body map[string]any
	err := httputil.GetJSON(creditsURL, map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Accept":        "application/json",
	}, 15*time.Second, &body)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return providers.Snapshot{
				ProviderID:   "kimi-k2",
				ProviderName: "Kimi K2",
				Source:       "api-key",
				Metrics:      []providers.MetricValue{},
				Status:       "unknown",
				Error:        "Kimi K2 API key unauthorized. Check KIMI_K2_API_KEY.",
			}, nil
		}
		return providers.Snapshot{}, err
	}

	consumed, consumedOk, remaining, remainingOk := extractCredits(body)
	var metrics []providers.MetricValue
	now := time.Now().UTC().Format(time.RFC3339)

	if consumedOk || remainingOk {
		c := consumed
		if !consumedOk {
			c = 0
		}
		r := remaining
		if !remainingOk {
			r = 0
		}
		total := c + r

		if total > 0 {
			remainPct := (r / total) * 100
			ratio := remainPct / 100
			rc := int(math.Round(r))
			rm := int(math.Round(total))

			metrics = append(metrics, providers.MetricValue{
				ID:           "credits-balance",
				Label:        "CREDITS",
				Name:         "Kimi K2 credits remaining",
				Value:        math.Round(remainPct),
				NumericValue: &remainPct,
				NumericUnit:  "percent",
				Unit:         "%",
				Ratio:        &ratio,
				Direction:    "up",
				RawCount:     &rc,
				RawMax:       &rm,
				UpdatedAt:    now,
			})
		} else if r > 0 {
			metrics = append(metrics, providers.MetricValue{
				ID:              "credits-balance",
				Label:           "CREDITS",
				Name:            "Kimi K2 credits",
				Value:           fmt.Sprintf("%d", int(math.Round(r))),
				NumericValue:    &r,
				NumericUnit:     "count",
				NumericGoodWhen: "high",
				Caption:         "Available",
				UpdatedAt:       now,
			})
		}
	}

	return providers.Snapshot{
		ProviderID:   "kimi-k2",
		ProviderName: "Kimi K2",
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

func init() {
	providers.Register(Provider{})
}

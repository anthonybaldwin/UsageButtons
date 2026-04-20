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

// creditsURL is the Kimi K2 credits lookup endpoint.
const creditsURL = "https://kimi-k2.ai/api/user/credits"

// getAPIKey resolves a Kimi K2 API key from user settings or env vars.
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

// Short key paths applied against each context map so nested APIs that
// return { data: { credits: { ... } } } or { result: { usage: ... } }
// resolve without exhaustive prefix enumeration.
// consumedPaths lists JSON key paths that may carry a consumed-credits value.
var consumedPaths = [][]string{
	{"total_credits_consumed"},
	{"totalCreditsConsumed"},
	{"credits_consumed"},
	{"creditsConsumed"},
	{"consumedCredits"},
	{"usedCredits"},
	{"total"},
	{"consumed"},
}

// remainingPaths lists JSON key paths that may carry a remaining-credits value.
var remainingPaths = [][]string{
	{"credits_remaining"},
	{"creditsRemaining"},
	{"remaining_credits"},
	{"remainingCredits"},
	{"available_credits"},
	{"availableCredits"},
	{"credits_left"},
	{"creditsLeft"},
	{"remaining"},
}

// contexts returns every nested map worth searching. Mirrors CodexBar's
// KimiK2UsageFetcher.contexts so either side accepts both rooted
// ({credits_remaining: N}) and wrapped ({data: {usage: {...}}}) shapes.
func contexts(body map[string]any) []map[string]any {
	out := []map[string]any{body}
	add := func(key string) {
		if sub, ok := body[key].(map[string]any); ok {
			out = append(out, sub)
			if u, ok := sub["usage"].(map[string]any); ok {
				out = append(out, u)
			}
			if c, ok := sub["credits"].(map[string]any); ok {
				out = append(out, c)
			}
		}
	}
	add("data")
	add("result")
	if u, ok := body["usage"].(map[string]any); ok {
		out = append(out, u)
	}
	if c, ok := body["credits"].(map[string]any); ok {
		out = append(out, c)
	}
	return out
}

// findFirst returns the first dig hit from any of paths under ctx.
func findFirst(ctx map[string]any, paths [][]string) (float64, bool) {
	for _, path := range paths {
		if v, ok := dig(ctx, path); ok {
			return v, true
		}
	}
	return 0, false
}

// extractCredits prefers consumed/remaining values that come from the same
// subtree, so generic keys like "consumed" / "remaining" can't be paired
// across unrelated parts of the response. Falls back to the first match
// for each, in any context, when no single subtree carries both.
func extractCredits(body map[string]any) (consumed float64, consumedOk bool, remaining float64, remainingOk bool) {
	ctxs := contexts(body)
	var cFallback, rFallback float64
	var cFallbackOK, rFallbackOK bool

	for _, ctx := range ctxs {
		c, cOK := findFirst(ctx, consumedPaths)
		r, rOK := findFirst(ctx, remainingPaths)

		if cOK && rOK {
			return c, true, r, true
		}
		if cOK && !cFallbackOK {
			cFallback, cFallbackOK = c, true
		}
		if rOK && !rFallbackOK {
			rFallback, rFallbackOK = r, true
		}
	}
	return cFallback, cFallbackOK, rFallback, rFallbackOK
}

// Provider fetches Kimi K2 usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "kimi-k2" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Kimi K2" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#0071e3" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#0a1225" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"credits-balance"}
}

// Fetch returns the latest Kimi K2 credits snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providers.Snapshot{
			ProviderID:   "kimi-k2",
			ProviderName: "Kimi K2",
			Source:       "none",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Enter a Kimi K2 API key in the Kimi K2 tab, or set KIMI_K2_API_KEY.",
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

// init registers the Kimi K2 provider with the package registry.
func init() {
	providers.Register(Provider{})
}

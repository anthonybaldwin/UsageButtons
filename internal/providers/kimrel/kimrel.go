// Package kimrel implements the Kimrel credits provider.
//
// Kimrel (kimrel.com, formerly kimi-k2.ai) is an INDEPENDENT THIRD-PARTY
// reseller of Kimi K2 model access. It is NOT affiliated with, endorsed
// by, or sponsored by Moonshot AI — Kimrel's own footer states this.
// Users only see data here if they hold a kimrel.com account; Moonshot
// API keys won't authenticate against this endpoint.
//
// For the official Moonshot dev platform (api.moonshot.ai/cn) use the
// `moonshot` provider instead. For the kimi.com membership (Moderato,
// Allegretto, etc.) use the `kimi` provider.
//
// Auth: Property Inspector settings field or KIMREL_API_KEY (preferred)
// / KIMI_K2_API_KEY / KIMI_API_KEY / KIMI_KEY environment variable.
// Endpoint: GET https://kimi-k2.ai/api/user/credits (308-redirects to
// the kimrel.com production host).
//
// Provider ID stays "kimi-k2" so existing user button settings keep
// working across the rename — only the user-visible label and package
// name moved to "Kimrel."
package kimrel

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

// creditsURL is the Kimrel credits lookup endpoint. The kimi-k2.ai
// host 308-redirects to kimrel.com today; we point at the original
// host so server-side renames remain transparent.
const creditsURL = "https://kimi-k2.ai/api/user/credits"

// getAPIKey resolves a Kimrel API key from user settings or env vars.
// KIMREL_API_KEY is the preferred name; the older KIMI_K2_API_KEY /
// KIMI_API_KEY / KIMI_KEY names still resolve so existing setups keep
// working after the rename.
func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().KimiK2Key,
		"KIMREL_API_KEY", "KIMI_K2_API_KEY", "KIMI_API_KEY", "KIMI_KEY",
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
	{"total_credits_used"},
	{"totalCreditsUsed"},
	{"credits_consumed"},
	{"creditsConsumed"},
	{"consumedCredits"},
	{"usedCredits"},
	{"total"},
	{"usage", "total"},
	{"usage", "consumed"},
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
	{"usage", "credits_remaining"},
	{"usage", "remaining"},
	{"remaining"},
}

// averageTokenPaths lists optional average-token-per-request fields.
var averageTokenPaths = [][]string{
	{"average_tokens_per_request"},
	{"averageTokensPerRequest"},
	{"average_tokens"},
	{"averageTokens"},
	{"avg_tokens"},
	{"avgTokens"},
}

// timestampPaths lists optional updated-at fields.
var timestampPaths = [][]string{
	{"updated_at"},
	{"updatedAt"},
	{"timestamp"},
	{"time"},
	{"last_update"},
	{"lastUpdated"},
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

// findDate returns the first timestamp value found under paths.
func findDate(body map[string]any, paths [][]string) *time.Time {
	for _, ctx := range contexts(body) {
		for _, path := range paths {
			if raw, ok := digAny(ctx, path); ok {
				if t, ok := parseDate(raw); ok {
					return &t
				}
			}
		}
	}
	return nil
}

// digAny traverses a nested object path and returns the raw value.
func digAny(obj any, path []string) (any, bool) {
	current := obj
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current = m[key]
		if current == nil {
			return nil, false
		}
	}
	return current, true
}

// parseDate parses ISO or epoch-second/millisecond timestamps.
func parseDate(raw any) (time.Time, bool) {
	switch v := raw.(type) {
	case float64:
		return dateFromNumeric(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return time.Time{}, false
		}
		return dateFromNumeric(f)
	case string:
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return dateFromNumeric(n)
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, v); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// dateFromNumeric parses epoch seconds or milliseconds.
func dateFromNumeric(v float64) (time.Time, bool) {
	if v <= 0 {
		return time.Time{}, false
	}
	if v > 1_000_000_000_000 {
		return time.UnixMilli(int64(v)), true
	}
	return time.Unix(int64(v), 0), true
}

// Provider fetches Kimrel usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry. Kept as
// "kimi-k2" so existing user button settings keep working after the
// rename to Kimrel.
func (Provider) ID() string { return "kimi-k2" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Kimrel" }

// BrandColor returns the accent color used on button faces. Slate gray
// is intentional — Kimrel is third-party and shouldn't borrow Kimi's
// orange or Moonshot's blue, which would imply official affiliation.
func (Provider) BrandColor() string { return "#64748b" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#1e293b" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"credits-balance"}
}

// Fetch returns the latest Kimrel credits snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providers.Snapshot{
			ProviderID:   "kimi-k2",
			ProviderName: "Kimrel",
			Source:       "none",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Enter a Kimrel API key in the Kimrel tab, or set KIMREL_API_KEY. Kimrel (kimrel.com) is an independent third-party reseller of Kimi K2 — not affiliated with Moonshot AI. For the official Moonshot platform, use the Moonshot provider instead.",
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
				ProviderName: "Kimrel",
				Source:       "api-key",
				Metrics:      []providers.MetricValue{},
				Status:       "unknown",
				Error:        "Kimrel API key unauthorized. Check KIMREL_API_KEY.",
			}, nil
		}
		return providers.Snapshot{}, err
	}

	consumed, consumedOk, remaining, remainingOk := extractCredits(body)
	var metrics []providers.MetricValue
	now := time.Now().UTC().Format(time.RFC3339)
	if t := findDate(body, timestampPaths); t != nil {
		now = t.UTC().Format(time.RFC3339)
	}
	averageTokens, _ := firstInContexts(body, averageTokenPaths)

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
				Name:         "Kimrel credits remaining",
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
			if averageTokens > 0 {
				metrics[len(metrics)-1].Caption = fmt.Sprintf("%.0f avg tokens", averageTokens)
			}
		} else if r > 0 {
			metrics = append(metrics, providers.MetricValue{
				ID:              "credits-balance",
				Label:           "CREDITS",
				Name:            "Kimrel credits",
				Value:           fmt.Sprintf("%d", int(math.Round(r))),
				NumericValue:    &r,
				NumericUnit:     "count",
				NumericGoodWhen: "high",
				Caption:         "Available",
				UpdatedAt:       now,
			})
			if averageTokens > 0 {
				metrics[len(metrics)-1].Caption = fmt.Sprintf("%.0f avg tokens", averageTokens)
			}
		}
	}

	return providers.Snapshot{
		ProviderID:   "kimi-k2",
		ProviderName: "Kimrel",
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// firstInContexts returns the first numeric value found under paths.
func firstInContexts(body map[string]any, paths [][]string) (float64, bool) {
	for _, ctx := range contexts(body) {
		if v, ok := findFirst(ctx, paths); ok {
			return v, true
		}
	}
	return 0, false
}

// init registers the Kimrel provider with the package registry.
func init() {
	providers.Register(Provider{})
}

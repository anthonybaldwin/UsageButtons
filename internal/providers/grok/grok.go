// Package grok implements the Grok (xAI) usage provider.
//
// Auth: Usage Buttons Helper extension with the user's grok.com browser
// session (cookies). Endpoint: POST /rest/rate-limits with a JSON body
// specifying {requestKind, modelName}. We hit it twice — once for grok-3
// and once for grok-4-heavy — mirroring how grok.com's own /usage page
// (and the JoshuaWang2211/grok-usage-watch extension) fetches limits.
//
// Unlike Perplexity's rate-limit endpoint, Grok's response includes
// total* values alongside remaining*, so we can render honest percent
// metrics with a reset countdown derived from windowSizeSeconds.
package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const (
	rateLimitsURL = "https://grok.com/rest/rate-limits"
	modelGrok3    = "grok-3"
	modelGrok4    = "grok-4-heavy"
)

// modelStats is one model's parsed rate-limit response.
type modelStats struct {
	RemainingQueries  *int
	TotalQueries      *int
	RemainingTokens   *int
	TotalTokens       *int
	WindowSizeSeconds *int
}

// usageSnapshot is the normalized Grok quota state.
type usageSnapshot struct {
	Grok3     modelStats
	Grok4     modelStats
	UpdatedAt time.Time
}

// Provider fetches Grok usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "grok" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Grok" }

// BrandColor returns the accent color used on button faces. Grok / xAI
// is monochrome; the inverse of Ollama's near-white-on-near-black —
// black mark on a white canvas. Smart contrast (enabled for grok in
// settings.providerDefaultSmartContrast) auto-flips text/watermark
// over the dark fill bar at high meter ratios so the icon stays
// visible regardless of the ratio.
func (Provider) BrandColor() string { return "#000000" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#ffffff" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{
		"grok3-queries-percent",
		"grok3-tokens-percent",
		"grok4-heavy-queries-percent",
	}
}

// Fetch returns the latest Grok quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("grok.com")), nil
	}
	g3, err := fetchModel(ctx, modelGrok3)
	if err != nil {
		return mapHTTPError(err), nil
	}
	// grok-4-heavy is best-effort: free-tier accounts don't have it.
	// Failure here suppresses just the heavy metric, not the whole snapshot.
	g4, _ := fetchModel(ctx, modelGrok4)
	usage := usageSnapshot{Grok3: g3, Grok4: g4, UpdatedAt: time.Now().UTC()}
	return snapshotFromUsage(usage), nil
}

// fetchModel POSTs the rate-limits endpoint for one modelName and
// parses the response into modelStats.
func fetchModel(ctx context.Context, modelName string) (modelStats, error) {
	body, err := json.Marshal(map[string]string{
		"requestKind": "DEFAULT",
		"modelName":   modelName,
	})
	if err != nil {
		return modelStats{}, err
	}
	resp, err := cookies.Fetch(ctx, cookies.Request{
		URL:    rateLimitsURL,
		Method: "POST",
		Headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json",
			"Origin":       "https://grok.com",
			"Referer":      "https://grok.com/",
		},
		Body: body,
	})
	if err != nil {
		return modelStats{}, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return modelStats{}, &httputil.Error{
			Status:     resp.Status,
			StatusText: resp.StatusText,
			Body:       string(resp.Body),
			URL:        rateLimitsURL,
		}
	}
	root := map[string]any{}
	if err := json.Unmarshal(resp.Body, &root); err != nil {
		return modelStats{}, fmt.Errorf("Grok: %s response not JSON", modelName)
	}
	return parseModelStats(root), nil
}

// parseModelStats extracts the four rate-limit numbers from a single
// grok.com /rest/rate-limits response. The endpoint returns flat keys
// at root for grok-3 and grok-4-heavy alike — no envelope walking
// needed for the published response shape.
func parseModelStats(root map[string]any) modelStats {
	return modelStats{
		RemainingQueries:  intFromMap(root, "remainingQueries", "remaining_queries"),
		TotalQueries:      intFromMap(root, "totalQueries", "total_queries"),
		RemainingTokens:   intFromMap(root, "remainingTokens", "remaining_tokens"),
		TotalTokens:       intFromMap(root, "totalTokens", "total_tokens"),
		WindowSizeSeconds: intFromMap(root, "windowSizeSeconds", "window_size_seconds"),
	}
}

// intFromMap returns the first key's value as a rounded int pointer.
// nil when none of the keys are present or none parse as a number.
func intFromMap(m map[string]any, keys ...string) *int {
	if v, ok := providerutil.FirstFloat(m, keys...); ok {
		n := int(math.Round(v))
		return &n
	}
	return nil
}

// snapshotFromUsage maps Grok usage into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	var metrics []providers.MetricValue
	if m, ok := percentMetric(
		"grok3-queries-percent", "GROK 3", "Grok 3 queries remaining (window)",
		usage.Grok3.RemainingQueries, usage.Grok3.TotalQueries,
		usage.Grok3.WindowSizeSeconds, now); ok {
		metrics = append(metrics, m)
	}
	if m, ok := percentMetric(
		"grok3-tokens-percent", "GROK 3 TOK", "Grok 3 tokens remaining (window)",
		usage.Grok3.RemainingTokens, usage.Grok3.TotalTokens,
		usage.Grok3.WindowSizeSeconds, now); ok {
		metrics = append(metrics, m)
	}
	if m, ok := percentMetric(
		"grok4-heavy-queries-percent", "GROK 4 H", "Grok 4 Heavy queries remaining (window)",
		usage.Grok4.RemainingQueries, usage.Grok4.TotalQueries,
		usage.Grok4.WindowSizeSeconds, now); ok {
		metrics = append(metrics, m)
	}
	return providers.Snapshot{
		ProviderID:   "grok",
		ProviderName: "Grok",
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// percentMetric builds a remaining-percent metric. Returns ok=false
// when the response didn't include enough data (e.g. grok-4-heavy on
// free tier).
func percentMetric(id, label, name string, remaining, total, windowSecs *int, now string) (providers.MetricValue, bool) {
	if remaining == nil || total == nil || *total <= 0 {
		return providers.MetricValue{}, false
	}
	used := math.Max(0, float64(*total-*remaining))
	usedPct := math.Max(0, math.Min(100, used/float64(*total)*100))
	caption := fmt.Sprintf("%d/%d left", *remaining, *total)
	var resetAt *time.Time
	if windowSecs != nil && *windowSecs > 0 {
		t := time.Now().Add(time.Duration(*windowSecs) * time.Second)
		resetAt = &t
	}
	m := providerutil.PercentRemainingMetric(id, label, name, usedPct, resetAt, caption, now)
	m = providerutil.RawCounts(m, *remaining, *total)
	return m, true
}

// mapHTTPError turns a Fetch error into the most useful provider snapshot.
func mapHTTPError(err error) providers.Snapshot {
	var httpErr *httputil.Error
	if !errors.As(err, &httpErr) {
		return errorSnapshot(err.Error())
	}
	if httpErr.Status == 401 || httpErr.Status == 403 {
		return errorSnapshot(cookieaux.StaleMessage("grok.com"))
	}
	return errorSnapshot(fmt.Sprintf("Grok HTTP %d", httpErr.Status))
}

// errorSnapshot returns a Grok setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "grok",
		ProviderName: "Grok",
		Source:       "cookie",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// init registers the Grok provider with the package registry.
func init() {
	providers.Register(Provider{})
}

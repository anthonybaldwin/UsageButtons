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
	"os"
	"path/filepath"
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
//
// remainingTokens / totalTokens are present on API-platform accounts but
// not on consumer-chat (cookie-auth) accounts — the parser returns nil
// for them on chat tiers and the corresponding metric is silently
// skipped, leaving query metrics intact. Both code paths are kept so
// the PI dropdown advertises the same metric set to every user;
// whichever ones the API actually returns become live.
//
// WaitTimeSeconds is populated only when the user has actually hit the
// rate limit — both effort-tier rate-limit objects are null on a
// healthy response. When non-nil at remaining=0, this gives a real
// "retry in N seconds" countdown the API anchors per poll.
type modelStats struct {
	RemainingQueries  *int
	TotalQueries      *int
	RemainingTokens   *int
	TotalTokens       *int
	WindowSizeSeconds *int
	WaitTimeSeconds   *int
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
		"grok3-queries-remaining",
		"grok3-tokens-remaining",
		"grok4-heavy-queries-remaining",
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
		dumpUnknownResponse(modelName, resp.Body)
		return modelStats{}, fmt.Errorf("Grok: %s response not JSON", modelName)
	}
	stats := parseModelStats(root)
	if stats.RemainingQueries == nil && stats.TotalQueries == nil &&
		stats.RemainingTokens == nil && stats.TotalTokens == nil {
		// Recognized as JSON but missing every field we know how to
		// extract — record so a future debug pass can inspect the
		// real shape (mirrors OpenCode's dumpUnknownResponse pattern).
		dumpUnknownResponse(modelName, resp.Body)
	}
	return stats, nil
}

// dumpUnknownResponse appends a snippet of an unrecognized rate-limits
// response to a temp file for offline inspection. Owner-only perms,
// append mode, and a total-size cap keep it from running away if the
// API permanently changes shape.
func dumpUnknownResponse(modelName string, body []byte) {
	const (
		maxSnippetBytes = 8 * 1024
		maxFileBytes    = 256 * 1024
	)
	path := filepath.Join(os.TempDir(), "usagebuttons-grok-debug.txt")
	if info, err := os.Stat(path); err == nil && info.Size() >= maxFileBytes {
		return
	}
	snippet := body
	truncated := false
	if len(snippet) > maxSnippetBytes {
		snippet = snippet[:maxSnippetBytes]
		truncated = true
	}
	header := fmt.Sprintf("[%s] modelName=%s length=%d truncated=%v\n",
		time.Now().UTC().Format(time.RFC3339), modelName, len(body), truncated)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(header)
	_, _ = f.Write(snippet)
	_, _ = f.WriteString("\n\n")
}

// parseModelStats extracts the rate-limit numbers from a single
// grok.com /rest/rate-limits response. Top-level keys carry the
// totals; the optional nested `lowEffortRateLimits` /
// `highEffortRateLimits` objects only populate when the account is
// actively rate-limited, and that's where we read waitTimeSeconds.
func parseModelStats(root map[string]any) modelStats {
	return modelStats{
		RemainingQueries:  intFromMap(root, "remainingQueries", "remaining_queries"),
		TotalQueries:      intFromMap(root, "totalQueries", "total_queries"),
		RemainingTokens:   intFromMap(root, "remainingTokens", "remaining_tokens"),
		TotalTokens:       intFromMap(root, "totalTokens", "total_tokens"),
		WindowSizeSeconds: intFromMap(root, "windowSizeSeconds", "window_size_seconds"),
		WaitTimeSeconds:   readWaitTimeSeconds(root),
	}
}

// readWaitTimeSeconds returns the API-supplied "retry in N seconds"
// hint when the account is currently throttled. Searches the two
// effort-tier rate-limit blocks (low and high) and returns the
// smallest non-zero value — that's the next moment a slot opens.
func readWaitTimeSeconds(root map[string]any) *int {
	var smallest *int
	for _, key := range []string{"lowEffortRateLimits", "highEffortRateLimits"} {
		nested, ok := providerutil.NestedMap(root, key)
		if !ok {
			continue
		}
		v := intFromMap(nested, "waitTimeSeconds", "wait_time_seconds")
		if v == nil || *v <= 0 {
			continue
		}
		if smallest == nil || *v < *smallest {
			smallest = v
		}
	}
	return smallest
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
	if m, ok := countMetric(
		"grok3-queries-remaining", "GROK 3", "Searches",
		"Grok 3 queries remaining (window)",
		usage.Grok3.RemainingQueries, usage.Grok3.TotalQueries,
		usage.Grok3.WaitTimeSeconds, now); ok {
		metrics = append(metrics, m)
	}
	if m, ok := countMetric(
		"grok3-tokens-remaining", "GROK 3", "Tokens",
		"Grok 3 tokens remaining (window) — API tier only",
		usage.Grok3.RemainingTokens, usage.Grok3.TotalTokens,
		usage.Grok3.WaitTimeSeconds, now); ok {
		metrics = append(metrics, m)
	}
	if m, ok := countMetric(
		"grok4-heavy-queries-remaining", "GROK 4", "Heavy",
		"Grok 4 Heavy queries remaining (window)",
		usage.Grok4.RemainingQueries, usage.Grok4.TotalQueries,
		usage.Grok4.WaitTimeSeconds, now); ok {
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

// countMetric renders one rate-limit category as a count tile:
// "139/140" is the prominent value, the meter fill scales to
// remaining/total, and the caption is a one-word category
// ("Searches" / "Heavy" / "Tokens").
//
// Why count, not percent: grok.com's totals are small (140-cap on
// grok-3, 20-cap on grok-4 Heavy). "10% remaining" of 20 = 2 — the
// user has to do that math in their head. Showing X/Y directly
// mirrors what grok.com's own /usage page shows.
//
// Reset countdown: fires only when the account has hit the limit
// (remaining = 0) and the API surfaces waitTimeSeconds in one of
// the effort-tier rate-limit blocks. That's the API's own
// "next slot opens in N seconds" and re-anchors per poll correctly.
// windowSizeSeconds on a healthy response is the rolling-window
// length, not a countdown, and is intentionally not surfaced.
//
// Returns ok=false when the response didn't include enough data
// (grok-4-heavy on free tier; tokens on consumer-chat tier).
func countMetric(id, label, category, name string, remaining, total, waitSecs *int, now string) (providers.MetricValue, bool) {
	if remaining == nil || total == nil || *total <= 0 {
		return providers.MetricValue{}, false
	}
	rem := *remaining
	tot := *total
	val := fmt.Sprintf("%d/%d", rem, tot)
	num := float64(rem)
	ratio := math.Max(0, math.Min(1, float64(rem)/float64(tot)))
	m := providers.MetricValue{
		ID:              id,
		Label:           label,
		Name:            name,
		Value:           val, // "139/140" — Stream Deck title (label) sits above
		NumericValue:    &num,
		NumericUnit:     "count",
		NumericGoodWhen: "high",
		Ratio:           &ratio,
		Caption:         category, // "Searches" / "Tokens" / "Heavy"
		UpdatedAt:       now,
		RawCount:        &rem,
		RawMax:          &tot,
	}
	if rem == 0 && waitSecs != nil && *waitSecs > 0 {
		secs := float64(*waitSecs)
		m.ResetInSeconds = &secs
	}
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

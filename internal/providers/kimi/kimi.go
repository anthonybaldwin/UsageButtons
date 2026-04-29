// Package kimi implements the Kimi usage provider.
//
// Auth: Usage Buttons Helper extension with the user's kimi.com browser
// session. There is no manual cookie/JWT paste — the extension is the
// only path so credentials never leave Chrome.
// Endpoint: POST https://www.kimi.com/apiv2/kimi.gateway.billing.v1.BillingService/GetUsages.
package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const usageURL = "https://www.kimi.com/apiv2/kimi.gateway.billing.v1.BillingService/GetUsages"

// usageResponse is Kimi's coding usage response.
type usageResponse struct {
	Usages []usageEntry `json:"usages"`
}

// usageEntry is one Kimi scoped usage entry.
type usageEntry struct {
	Scope  string           `json:"scope"`
	Detail usageDetail      `json:"detail"`
	Limits []rateLimitEntry `json:"limits"`
}

// usageDetail is one quota lane returned by Kimi.
type usageDetail struct {
	Limit     string `json:"limit"`
	Used      string `json:"used"`
	Remaining string `json:"remaining"`
	ResetTime string `json:"resetTime"`
}

// rateLimitEntry is a Kimi nested rate-limit window.
type rateLimitEntry struct {
	Window rateWindow  `json:"window"`
	Detail usageDetail `json:"detail"`
}

// rateWindow describes a rate-limit duration.
type rateWindow struct {
	Duration int    `json:"duration"`
	TimeUnit string `json:"timeUnit"`
}

// usageSnapshot is the parsed Kimi quota state.
type usageSnapshot struct {
	Weekly    usageDetail
	Rate      *usageDetail
	UpdatedAt time.Time
}

// Provider fetches Kimi usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "kimi" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Kimi" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#fe603c" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#111214" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent"}
}

// Fetch returns the latest Kimi usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("kimi.com")), nil
	}
	usage, err := fetchWithBrowser(ctx)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot(cookieaux.StaleMessage("kimi.com")), nil
		}
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage), nil
}

// fetchWithBrowser fetches Kimi usage through the Helper extension.
func fetchWithBrowser(ctx context.Context) (usageSnapshot, error) {
	body, err := json.Marshal(map[string]any{
		"scope": []string{"FEATURE_CODING"},
	})
	if err != nil {
		return usageSnapshot{}, err
	}
	resp, err := cookies.Fetch(ctx, cookies.Request{
		URL:    usageURL,
		Method: "POST",
		Headers: map[string]string{
			"Accept":                   "*/*",
			"Content-Type":             "application/json",
			"Origin":                   "https://www.kimi.com",
			"Referer":                  "https://www.kimi.com/code/console",
			"User-Agent":               httputil.DefaultUserAgent,
			"connect-protocol-version": "1",
			"x-language":               "en-US",
			"x-msh-platform":           "web",
			"r-timezone":               "UTC",
		},
		Body: body,
	})
	if err != nil {
		return usageSnapshot{}, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return usageSnapshot{}, &httputil.Error{
			Status:     resp.Status,
			StatusText: resp.StatusText,
			Body:       string(resp.Body),
			URL:        usageURL,
		}
	}
	var out usageResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return usageSnapshot{}, fmt.Errorf("invalid Kimi JSON: %w", err)
	}
	return parseUsage(out, time.Now().UTC())
}

// parseUsage selects FEATURE_CODING quota and rate-limit lanes.
func parseUsage(resp usageResponse, now time.Time) (usageSnapshot, error) {
	for _, usage := range resp.Usages {
		if usage.Scope != "FEATURE_CODING" {
			continue
		}
		var rate *usageDetail
		if len(usage.Limits) > 0 {
			detail := usage.Limits[0].Detail
			rate = &detail
		}
		return usageSnapshot{
			Weekly:    usage.Detail,
			Rate:      rate,
			UpdatedAt: now,
		}, nil
	}
	return usageSnapshot{}, fmt.Errorf("Kimi response missing FEATURE_CODING usage")
}

// snapshotFromUsage maps parsed Kimi usage into Stream Deck metrics.
//
// Note: the metric IDs are historically swapped — `session-percent`
// emits the weekly window and `weekly-percent` emits the 5-hour rate
// limit. Rebinding the IDs would break existing user button settings,
// so the IDs stay; only the user-visible labels reflect the actual
// data (WEEKLY for the weekly window, SESSION for the 5h rate).
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	metrics := []providers.MetricValue{
		quotaMetric("session-percent", "WEEKLY", "Kimi weekly requests remaining", usage.Weekly, now),
	}
	if usage.Rate != nil {
		metrics = append(metrics, quotaMetric("weekly-percent", "SESSION", "Kimi session window remaining (5h rate limit)", *usage.Rate, now))
	}
	return providers.Snapshot{
		ProviderID:   "kimi",
		ProviderName: "Kimi",
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// quotaMetric builds a remaining-percent metric from a Kimi quota detail.
// Caption is left empty so the SD subtext falls through to the
// reset-time countdown when usage has started, or "Remaining" when
// the window is still idle. Users who want the raw "X / Y" count
// can flip the per-button "Show raw counts" toggle in the property
// inspector — RawCount/RawMax are still wired.
func quotaMetric(id, label, name string, detail usageDetail, now string) providers.MetricValue {
	limit := numericString(detail.Limit)
	remaining := numericString(detail.Remaining)
	used := numericString(detail.Used)
	if remaining == nil && used != nil && limit != nil {
		v := math.Max(0, *limit-*used)
		remaining = &v
	}
	if used == nil && remaining != nil && limit != nil {
		v := math.Max(0, *limit-*remaining)
		used = &v
	}
	usedPct := 0.0
	if used != nil && limit != nil && *limit > 0 {
		usedPct = math.Max(0, math.Min(100, *used / *limit * 100))
	}
	var resetAt *time.Time
	if t, ok := providerutil.TimeValue(detail.ResetTime); ok {
		resetAt = &t
	}
	metric := providerutil.PercentRemainingMetric(id, label, name, usedPct, resetAt, "", now)
	if remaining != nil && limit != nil {
		rawCount := int(math.Round(*remaining))
		rawMax := int(math.Round(*limit))
		metric.RawCount = &rawCount
		metric.RawMax = &rawMax
	}
	return metric
}

// numericString parses a numeric Kimi string.
func numericString(raw string) *float64 {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return nil
	}
	return &n
}

// errorSnapshot returns a Kimi setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "kimi",
		ProviderName: "Kimi",
		Source:       "auth",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// init registers the Kimi provider with the package registry.
func init() {
	providers.Register(Provider{})
}

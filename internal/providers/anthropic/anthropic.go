// Package anthropic implements the Anthropic org-level cost view —
// today / MTD / 30-day spend across the whole organization. Distinct
// from the claude provider (which is one user's session/weekly window
// from local OAuth credentials), and from the personal Anthropic SDK
// flow (which has no usage endpoint at all per the Feb 2026 admin-
// API-only policy).
//
// Auth: an admin API key in the Property Inspector settings field, or
// the ANTHROPIC_ADMIN_API_KEY environment variable. The env var is
// deliberately namespaced with _ADMIN_ so it doesn't collide with the
// SDK-standard ANTHROPIC_API_KEY (which won't work here — the admin
// endpoints reject anything that isn't an sk-ant-admin-... key).
//
// Endpoint: GET https://api.anthropic.com/v1/organizations/cost_report
// with x-api-key + anthropic-version headers. One call with a 30-day
// window slices into today / month-to-date / trailing 30d.
package anthropic

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	// costReportURL is the Anthropic admin cost-report endpoint.
	costReportURL = "https://api.anthropic.com/v1/organizations/cost_report"
	// anthropicVersion is the required API version header.
	anthropicVersion = "2023-06-01"
	// fetchTimeout bounds the cost-report call. Pagination loops share
	// this timeout per request, not in aggregate, so the total wall
	// clock can exceed it on multi-page responses.
	fetchTimeout = 20 * time.Second
	// maxPages caps the pagination loop so a buggy `has_more=true`
	// response can't turn this into an infinite fetch. Daily buckets
	// over 30 days fit comfortably under any reasonable page limit;
	// 10 is more than enough headroom.
	maxPages = 10
)

// costReportResponse mirrors /v1/organizations/cost_report.
type costReportResponse struct {
	Data     []costBucket `json:"data"`
	HasMore  bool         `json:"has_more"`
	NextPage *string      `json:"next_page"`
}

// costBucket is one time slice of org spend.
type costBucket struct {
	StartingAt string       `json:"starting_at"`
	EndingAt   string       `json:"ending_at"`
	Results    []costResult `json:"results"`
}

// costResult is one cost line item inside a bucket. Amounts are
// returned as decimal strings in cents per the Anthropic API
// contract — `"123.45"` represents $1.23.
type costResult struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// getAPIKey resolves an Anthropic admin API key from user settings
// or the deliberately-namespaced env var fallback.
func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().AnthropicKey,
		"ANTHROPIC_ADMIN_API_KEY",
	)
}

// Provider fetches Anthropic org cost data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "anthropic" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Anthropic" }

// BrandColor returns the accent color used on button faces. Same
// terracotta the Anthropic mark uses — the icon (the eight-pointed
// star) is the brand differentiator vs the clawd provider's claw.
func (Provider) BrandColor() string { return "#cc7c5e" }

// BrandBg returns the background color used on button faces. Deeper
// black than clawd's #1c1210 so the org-level Anthropic tile reads
// as distinct from the per-user clawd tile in the same deck.
func (Provider) BrandBg() string { return "#0a0807" }

// MetricIDs enumerates the metrics this provider can emit. All seven
// derive from a single 30-day cost_report fetch — adding more windows
// is free once we have the daily buckets.
func (Provider) MetricIDs() []string {
	return []string{
		"cost-today",
		"cost-yesterday",
		"cost-7d",
		"cost-mtd",
		"cost-30d",
		"cost-burn-7d",
		"cost-projected-month",
	}
}

// Fetch returns the latest org cost snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providerutil.MissingAuthSnapshot(
			"anthropic",
			"Anthropic",
			"Enter an Anthropic admin API key (sk-ant-admin-…) in the Anthropic tab, or set ANTHROPIC_ADMIN_API_KEY.",
		), nil
	}

	now := time.Now().UTC()
	thirtyDaysAgo := now.Add(-30 * 24 * time.Hour)

	buckets, err := fetchAllBuckets(apiKey, thirtyDaysAgo)
	if err != nil {
		return providers.Snapshot{}, err
	}

	w := sumWindows(buckets, now)
	metrics := buildMetrics(w, providerutil.NowString())

	return providers.Snapshot{
		ProviderID:   "anthropic",
		ProviderName: "Anthropic",
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// fetchAllBuckets pages through cost_report until has_more is false
// (or maxPages is hit) and returns the accumulated bucket list.
func fetchAllBuckets(apiKey string, startingAt time.Time) ([]costBucket, error) {
	headers := map[string]string{
		"x-api-key":         apiKey,
		"anthropic-version": anthropicVersion,
		"Accept":            "application/json",
	}
	q := url.Values{}
	q.Set("starting_at", startingAt.Format(time.RFC3339))
	q.Set("bucket_width", "1d")

	var all []costBucket
	pageToken := ""
	for i := 0; i < maxPages; i++ {
		if pageToken != "" {
			q.Set("page", pageToken)
		}
		var resp costReportResponse
		if err := httputil.GetJSON(costReportURL+"?"+q.Encode(), headers, fetchTimeout, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Data...)
		if !resp.HasMore || resp.NextPage == nil || *resp.NextPage == "" {
			return all, nil
		}
		pageToken = *resp.NextPage
	}
	return all, errors.New("Anthropic admin cost_report exceeded pagination cap; check for an upstream loop")
}

// costWindows holds the per-window aggregates we care about plus the
// ratio inputs needed to derive burn-rate / month-projection in
// buildMetrics.
type costWindows struct {
	today        float64 // UTC today only
	yesterday    float64 // UTC yesterday only (24h slice ending at today's start)
	last7d       float64 // last 7 calendar days incl. today
	mtd          float64 // month-to-date (UTC)
	last30d      float64 // last 30 calendar days incl. today
	daysElapsed  int     // 1..N, where N = today's day-of-month, used for projection
	daysInMonth  int     // total days in the current UTC calendar month
}

// sumWindows walks the bucket list and accumulates spend across each
// window. Amounts are decimal strings in cents per the API contract.
// All time math is UTC-aligned to match the cost_report bucket boundaries.
func sumWindows(buckets []costBucket, now time.Time) costWindows {
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yesterdayStart := todayStart.Add(-24 * time.Hour)
	sevenDaysAgo := todayStart.Add(-6 * 24 * time.Hour) // last 7 days inclusive of today
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	nextMonth := monthStart.AddDate(0, 1, 0)

	w := costWindows{
		daysElapsed: now.Day(),
		daysInMonth: int(nextMonth.Sub(monthStart).Hours() / 24),
	}
	for _, b := range buckets {
		bucketStart, err := time.Parse(time.RFC3339, b.StartingAt)
		if err != nil {
			continue
		}
		usd := sumResultsUSD(b.Results)
		w.last30d += usd
		if !bucketStart.Before(monthStart) {
			w.mtd += usd
		}
		if !bucketStart.Before(sevenDaysAgo) {
			w.last7d += usd
		}
		if !bucketStart.Before(yesterdayStart) && bucketStart.Before(todayStart) {
			w.yesterday += usd
		}
		if !bucketStart.Before(todayStart) {
			w.today += usd
		}
	}
	return w
}

// sumResultsUSD totals the per-result amounts inside a bucket and
// converts cents to USD. Unparseable amounts are skipped (treated
// as zero) rather than failing the whole snapshot, since one bad
// row shouldn't blank the cost line.
func sumResultsUSD(results []costResult) float64 {
	cents := 0.0
	for _, r := range results {
		v, err := strconv.ParseFloat(strings.TrimSpace(r.Amount), 64)
		if err != nil {
			continue
		}
		cents += v
	}
	return cents / 100.0
}

// buildMetrics packages the cost windows as MetricValues. Burn rate
// is the trailing-7-day average ($/day); projected-month grosses the
// MTD up by (daysInMonth / daysElapsed) and is only meaningful after
// at least one full day has elapsed in the month.
func buildMetrics(w costWindows, now string) []providers.MetricValue {
	round := func(v float64) float64 { return math.Round(v*100) / 100 }
	t := round(w.today)
	y := round(w.yesterday)
	w7 := round(w.last7d)
	m := round(w.mtd)
	l30 := round(w.last30d)
	burn := round(w.last7d / 7.0)

	// Projection only makes sense once we have at least one full day's
	// data; on day 1 of the month it would just echo today's spend
	// scaled by the month length, which over-promises.
	projected := m
	if w.daysElapsed >= 1 && w.daysInMonth > 0 {
		projected = round(w.mtd * float64(w.daysInMonth) / float64(w.daysElapsed))
	}

	caption := "Cost (admin)"
	return []providers.MetricValue{
		{
			ID:              "cost-today",
			Label:           "TODAY",
			Name:            "Org spend today (UTC)",
			Value:           fmt.Sprintf("$%.2f", t),
			NumericValue:    &t,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         caption,
			UpdatedAt:       now,
		},
		{
			ID:              "cost-yesterday",
			Label:           "YESTERDAY",
			Name:            "Org spend yesterday (UTC)",
			Value:           fmt.Sprintf("$%.2f", y),
			NumericValue:    &y,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         caption,
			UpdatedAt:       now,
		},
		{
			ID:              "cost-7d",
			Label:           "7 DAYS",
			Name:            "Org spend last 7 days",
			Value:           fmt.Sprintf("$%.2f", w7),
			NumericValue:    &w7,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         caption,
			UpdatedAt:       now,
		},
		{
			ID:              "cost-mtd",
			Label:           "MTD",
			Name:            "Org spend month-to-date (UTC)",
			Value:           fmt.Sprintf("$%.2f", m),
			NumericValue:    &m,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         caption,
			UpdatedAt:       now,
		},
		{
			ID:              "cost-30d",
			Label:           "30 DAYS",
			Name:            "Org spend last 30 days",
			Value:           fmt.Sprintf("$%.2f", l30),
			NumericValue:    &l30,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         caption,
			UpdatedAt:       now,
		},
		{
			ID:              "cost-burn-7d",
			Label:           "BURN 7D",
			Name:            "Burn rate (7-day avg, $/day)",
			Value:           fmt.Sprintf("$%.2f", burn),
			NumericValue:    &burn,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         "$/day (7d avg)",
			UpdatedAt:       now,
		},
		{
			ID:              "cost-projected-month",
			Label:           "PROJECTED",
			Name:            "Projected month total (MTD × daysInMonth/daysElapsed)",
			Value:           fmt.Sprintf("$%.2f", projected),
			NumericValue:    &projected,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         "MTD pace",
			UpdatedAt:       now,
		},
	}
}

// init registers the Anthropic provider with the package registry.
func init() {
	providers.Register(Provider{})
}

// Package openai implements the OpenAI org-level cost view — today
// / MTD / 30-day spend across the whole organization. Distinct from
// the codex provider (which is one user's session/weekly window from
// ChatGPT OAuth credentials) and from the personal OpenAI SDK flow
// (which has no usage endpoint at all — `/dashboard/billing/*` was
// retired in 2024 and never came back for personal sk- keys).
//
// Auth: an admin API key in the Property Inspector settings field, or
// the OPENAI_ADMIN_API_KEY environment variable. The env var is
// deliberately namespaced with _ADMIN_ so it doesn't collide with the
// SDK-standard OPENAI_API_KEY (which won't work here — the admin
// endpoints reject anything that isn't an sk-admin-... key).
//
// Endpoint: GET https://api.openai.com/v1/organization/costs with a
// Bearer admin key. One call with a 30-day window slices into today /
// MTD / 30d locally. Default page size is only 7 buckets, so we set
// limit=180 (the API max) explicitly to cover the 30-day window in a
// single request.
package openai

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	// costsURL is the OpenAI admin cost-report endpoint.
	costsURL = "https://api.openai.com/v1/organization/costs"
	// fetchTimeout bounds each cost-report call. Pagination loops
	// share this timeout per request, not in aggregate.
	fetchTimeout = 20 * time.Second
	// pageLimit is the max bucket count per response (1..180 per the
	// API spec; 180 covers our 30-day window with headroom).
	pageLimit = 180
	// maxPages caps the pagination loop so a buggy upstream
	// has_more=true loop can't turn this into an infinite fetch.
	maxPages = 5
)

// costsResponse mirrors /v1/organization/costs.
type costsResponse struct {
	Object   string       `json:"object"`
	Data     []costBucket `json:"data"`
	HasMore  bool         `json:"has_more"`
	NextPage *string      `json:"next_page"`
}

// costBucket is one time slice of org spend. start_time / end_time
// are Unix seconds per the OpenAI admin API spec (different from
// Anthropic, which uses RFC 3339 strings).
type costBucket struct {
	Object    string       `json:"object"`
	StartTime int64        `json:"start_time"`
	EndTime   int64        `json:"end_time"`
	Results   []costResult `json:"results"`
}

// costResult is one cost line item inside a bucket. amount.value is
// already in dollars (e.g. 0.06 = $0.06) — no cents conversion
// needed, unlike Anthropic's cost endpoint.
type costResult struct {
	Object string      `json:"object"`
	Amount *costAmount `json:"amount"`
}

// costAmount carries the numeric value + currency code.
type costAmount struct {
	Value    float64 `json:"value"`
	Currency string  `json:"currency"`
}

// getAPIKey resolves an OpenAI admin API key from user settings or
// the deliberately-namespaced env var fallback.
func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().OpenAIKey,
		"OPENAI_ADMIN_API_KEY",
	)
}

// Provider fetches OpenAI org cost data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "openai" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "OpenAI" }

// BrandColor returns the accent color used on button faces. OpenAI's
// signature green — the rosette glyph plus this color signal "OpenAI"
// at a glance, distinct from the codex provider's blue cloud mark.
func (Provider) BrandColor() string { return "#10a37f" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#06180f" }

// MetricIDs enumerates the metrics this provider can emit. All seven
// derive from a single 30-day /organization/costs fetch — adding more
// windows is free once we have the daily buckets.
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
			"openai",
			"OpenAI",
			"Enter an OpenAI admin API key (sk-admin-…) in the OpenAI tab, or set OPENAI_ADMIN_API_KEY.",
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
		ProviderID:   "openai",
		ProviderName: "OpenAI",
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// fetchAllBuckets pages through /v1/organization/costs until
// has_more is false (or maxPages is hit) and returns the
// accumulated bucket list.
func fetchAllBuckets(apiKey string, startTime time.Time) ([]costBucket, error) {
	headers := map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Accept":        "application/json",
	}
	q := url.Values{}
	q.Set("start_time", strconv.FormatInt(startTime.Unix(), 10))
	q.Set("bucket_width", "1d")
	q.Set("limit", strconv.Itoa(pageLimit))

	var all []costBucket
	pageToken := ""
	for i := 0; i < maxPages; i++ {
		if pageToken != "" {
			q.Set("page", pageToken)
		}
		var resp costsResponse
		if err := httputil.GetJSON(costsURL+"?"+q.Encode(), headers, fetchTimeout, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Data...)
		if !resp.HasMore || resp.NextPage == nil || *resp.NextPage == "" {
			return all, nil
		}
		pageToken = *resp.NextPage
	}
	return all, errors.New("OpenAI admin /organization/costs exceeded pagination cap; check for an upstream loop")
}

// costWindows holds the per-window aggregates we care about plus the
// ratio inputs needed to derive burn-rate / month-projection in
// buildMetrics.
type costWindows struct {
	today        float64
	yesterday    float64
	last7d       float64
	mtd          float64
	last30d      float64
	daysElapsed  int
	daysInMonth  int
}

// sumWindows walks the bucket list and accumulates spend across each
// window. Amounts are already in dollars per the API contract — no
// cents conversion. All time math is UTC-aligned to match bucket
// boundaries.
func sumWindows(buckets []costBucket, now time.Time) costWindows {
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yesterdayStart := todayStart.Add(-24 * time.Hour)
	sevenDaysAgo := todayStart.Add(-6 * 24 * time.Hour)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	nextMonth := monthStart.AddDate(0, 1, 0)

	w := costWindows{
		daysElapsed: now.Day(),
		daysInMonth: int(nextMonth.Sub(monthStart).Hours() / 24),
	}
	for _, b := range buckets {
		bucketStart := time.Unix(b.StartTime, 0).UTC()
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

// sumResultsUSD totals the per-result amounts inside a bucket. Nil
// or non-USD entries are skipped — we don't know how to combine a
// "eur" row with a "usd" row, so dropping non-USD is more honest
// than fabricating an exchange rate.
func sumResultsUSD(results []costResult) float64 {
	total := 0.0
	for _, r := range results {
		if r.Amount == nil {
			continue
		}
		if r.Amount.Currency != "" && r.Amount.Currency != "usd" {
			continue
		}
		total += r.Amount.Value
	}
	return total
}

// buildMetrics packages the cost windows as MetricValues. Burn rate
// is the trailing-7-day average ($/day); projected-month grosses the
// MTD up by (daysInMonth / daysElapsed) so users see where the month
// is trending after only a few days of spend.
func buildMetrics(w costWindows, now string) []providers.MetricValue {
	round := func(v float64) float64 { return math.Round(v*100) / 100 }
	t := round(w.today)
	y := round(w.yesterday)
	w7 := round(w.last7d)
	m := round(w.mtd)
	l30 := round(w.last30d)
	burn := round(w.last7d / 7.0)

	projected := m
	if w.daysElapsed >= 1 && w.daysInMonth > 0 {
		projected = round(w.mtd * float64(w.daysInMonth) / float64(w.daysElapsed))
	}

	caption := "Org cost (admin API)"
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
			Caption:         "Avg daily org spend (last 7d)",
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
			Caption:         fmt.Sprintf("MTD pace through day %d/%d", w.daysElapsed, w.daysInMonth),
			UpdatedAt:       now,
		},
	}
}

// init registers the OpenAI provider with the package registry.
func init() {
	providers.Register(Provider{})
}

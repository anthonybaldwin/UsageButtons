// Package openaiadmin implements the OpenAI Admin API usage
// provider — the org-level cost view for organization
// administrators. Distinct from the existing codex provider, which
// surfaces a single user's session/weekly window using ChatGPT
// OAuth credentials. Admin shows aggregate org-wide spend and only
// works for users with an OpenAI organization and an admin key.
//
// Auth: Property Inspector settings field or
// OPENAI_ADMIN_API_KEY environment variable. Personal keys
// (sk-...) won't work — the admin endpoints reject anything that
// isn't an sk-admin-... key.
//
// Endpoint: GET https://api.openai.com/v1/organization/costs with
// a Bearer admin key. One call with a 30-day window slices into
// today / month-to-date / trailing 30d. Default page size is only
// 7 buckets, so we set limit=180 (the API max) explicitly to cover
// the 30-day window in a single request.
package openaiadmin

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
// needed, unlike Anthropic Admin.
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
// env vars.
func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().OpenAIAdminKey,
		"OPENAI_ADMIN_API_KEY",
	)
}

// Provider fetches OpenAI admin cost data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "openai-admin" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "OpenAI Admin" }

// BrandColor returns the accent color used on button faces.
// OpenAI brand green — visually distinct from the Codex blue used
// by the regular codex provider, so org-admin tiles stand out from
// per-user Codex tiles in the same deck.
func (Provider) BrandColor() string { return "#10a37f" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#06180f" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"cost-today", "cost-mtd", "cost-30d"}
}

// Fetch returns the latest org cost snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providerutil.MissingAuthSnapshot(
			"openai-admin",
			"OpenAI Admin",
			"Enter an OpenAI admin API key (sk-admin-…) in the OpenAI Admin tab, or set OPENAI_ADMIN_API_KEY.",
		), nil
	}

	now := time.Now().UTC()
	thirtyDaysAgo := now.Add(-30 * 24 * time.Hour)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	buckets, err := fetchAllBuckets(apiKey, thirtyDaysAgo)
	if err != nil {
		return providers.Snapshot{}, err
	}

	today, mtd, last30 := sumWindows(buckets, todayStart, monthStart)
	metrics := buildMetrics(today, mtd, last30, providerutil.NowString())

	return providers.Snapshot{
		ProviderID:   "openai-admin",
		ProviderName: "OpenAI Admin",
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

// sumWindows walks the bucket list and accumulates total spend in
// USD across the three windows we care about. Amounts are already
// in dollars per the API contract — no cents conversion.
func sumWindows(buckets []costBucket, todayStart, monthStart time.Time) (today, mtd, last30 float64) {
	for _, b := range buckets {
		bucketStart := time.Unix(b.StartTime, 0).UTC()
		bucketUSD := sumResultsUSD(b.Results)
		last30 += bucketUSD
		if !bucketStart.Before(monthStart) {
			mtd += bucketUSD
		}
		if !bucketStart.Before(todayStart) {
			today += bucketUSD
		}
	}
	return today, mtd, last30
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

// buildMetrics packages the three cost windows as MetricValues with
// dollar formatting, matching the shape Codex's local-log cost
// tiles use so admin and per-user tiles read consistently in the
// same deck.
func buildMetrics(today, mtd, last30 float64, now string) []providers.MetricValue {
	round := func(v float64) float64 { return math.Round(v*100) / 100 }
	t, m, l := round(today), round(mtd), round(last30)
	return []providers.MetricValue{
		{
			ID:              "cost-today",
			Label:           "TODAY",
			Name:            "Org spend today (UTC)",
			Value:           fmt.Sprintf("$%.2f", t),
			NumericValue:    &t,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         "Org cost (admin API)",
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
			Caption:         "Org cost (admin API)",
			UpdatedAt:       now,
		},
		{
			ID:              "cost-30d",
			Label:           "30 DAYS",
			Name:            "Org spend last 30 days",
			Value:           fmt.Sprintf("$%.2f", l),
			NumericValue:    &l,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         "Org cost (admin API)",
			UpdatedAt:       now,
		},
	}
}

// init registers the OpenAI Admin provider with the package registry.
func init() {
	providers.Register(Provider{})
}

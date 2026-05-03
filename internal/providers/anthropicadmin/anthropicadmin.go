// Package anthropicadmin implements the Anthropic Admin API usage
// provider — the org-level cost view exposed to administrators via
// admin API keys (sk-ant-admin-...). This is distinct from the
// existing claude provider, which surfaces a single user's session/
// weekly window using OAuth credentials. Admin shows aggregate
// org-wide spend and only works for users with an Anthropic
// organization and an admin key.
//
// Auth: Property Inspector settings field or
// ANTHROPIC_ADMIN_API_KEY environment variable. Personal keys
// (sk-ant-...) won't work — the admin endpoints reject anything
// that isn't a sk-ant-admin-... key.
//
// Endpoint: GET https://api.anthropic.com/v1/organizations/cost_report
// with x-api-key + anthropic-version headers. One call with a
// 30-day window slices into today / month-to-date / trailing 30d.
package anthropicadmin

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
// or env vars.
func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().AnthropicAdminKey,
		"ANTHROPIC_ADMIN_API_KEY",
	)
}

// Provider fetches Anthropic admin cost data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "anthropic-admin" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Anthropic Admin" }

// BrandColor returns the accent color used on button faces.
// Same terracotta as the Claude provider — they're the same brand
// family. The visual distinction comes from the shield glyph (vs
// Claude's star) plus the deeper black background.
func (Provider) BrandColor() string { return "#cc7c5e" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#0a0807" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"cost-today", "cost-mtd", "cost-30d"}
}

// Fetch returns the latest org cost snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providerutil.MissingAuthSnapshot(
			"anthropic-admin",
			"Anthropic Admin",
			"Enter an Anthropic admin API key (sk-ant-admin-…) in the Anthropic Admin tab, or set ANTHROPIC_ADMIN_API_KEY.",
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
		ProviderID:   "anthropic-admin",
		ProviderName: "Anthropic Admin",
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

// sumWindows walks the bucket list and accumulates total spend in
// USD across the three windows we care about. Amounts are decimal
// strings in cents per the API contract.
func sumWindows(buckets []costBucket, todayStart, monthStart time.Time) (today, mtd, last30 float64) {
	for _, b := range buckets {
		bucketStart, err := time.Parse(time.RFC3339, b.StartingAt)
		if err != nil {
			continue
		}
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

// buildMetrics packages the three cost windows as MetricValues with
// dollar formatting, matching the shape Claude's cost-today /
// cost-30d tiles use.
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

// init registers the Anthropic Admin provider with the package registry.
func init() {
	providers.Register(Provider{})
}

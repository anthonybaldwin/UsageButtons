// Package mistral implements the Mistral billing usage provider.
//
// Auth: Usage Buttons Helper extension with the user's admin.mistral.ai
// browser session. Endpoint:
// https://admin.mistral.ai/api/billing/v2/usage?month=...&year=....
package mistral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const adminBaseURL = "https://admin.mistral.ai"

// billingResponse mirrors Mistral's billing usage response.
type billingResponse struct {
	Completion     *modelCategory      `json:"completion"`
	OCR            *modelCategory      `json:"ocr"`
	Connectors     *modelCategory      `json:"connectors"`
	LibrariesAPI   *librariesCategory  `json:"libraries_api"`
	FineTuning     *fineTuningCategory `json:"fine_tuning"`
	Audio          *modelCategory      `json:"audio"`
	VibeUsage      *float64            `json:"vibe_usage"`
	Currency       string              `json:"currency"`
	CurrencySymbol string              `json:"currency_symbol"`
	Prices         []priceEntry        `json:"prices"`
	StartDate      string              `json:"start_date"`
	EndDate        string              `json:"end_date"`
}

// modelCategory groups usage data by model.
type modelCategory struct {
	Models map[string]modelUsage `json:"models"`
}

// librariesCategory groups document-library usage.
type librariesCategory struct {
	Pages  *modelCategory `json:"pages"`
	Tokens *modelCategory `json:"tokens"`
}

// fineTuningCategory groups fine-tuning training and storage usage.
type fineTuningCategory struct {
	Training map[string]modelUsage `json:"training"`
	Storage  map[string]modelUsage `json:"storage"`
}

// modelUsage contains billable usage entries for one model.
type modelUsage struct {
	Input  []usageEntry `json:"input"`
	Output []usageEntry `json:"output"`
	Cached []usageEntry `json:"cached"`
}

// usageEntry is one billable Mistral usage row.
type usageEntry struct {
	BillingMetric string   `json:"billing_metric"`
	BillingGroup  string   `json:"billing_group"`
	Value         *float64 `json:"value"`
	ValuePaid     *float64 `json:"value_paid"`
}

// priceEntry is one price row for a billable metric/group pair.
type priceEntry struct {
	BillingMetric string `json:"billing_metric"`
	BillingGroup  string `json:"billing_group"`
	Price         string `json:"price"`
}

// usageSnapshot is the parsed Mistral monthly billing state.
type usageSnapshot struct {
	TotalCost      float64
	Currency       string
	CurrencySymbol string
	InputTokens    int
	OutputTokens   int
	CachedTokens   int
	ModelCount     int
	VibeUsage      float64
	PeriodEnd      *time.Time
	UpdatedAt      time.Time
}

// Provider fetches Mistral usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "mistral" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Mistral" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#ff500f" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#111214" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent"}
}

// Fetch returns the latest Mistral usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("mistral.ai")), nil
	}
	usage, err := fetchUsage(ctx)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot(cookieaux.StaleMessage("mistral.ai")), nil
		}
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage), nil
}

// fetchUsage fetches current-month Mistral billing usage.
func fetchUsage(ctx context.Context) (usageSnapshot, error) {
	now := time.Now().UTC()
	resp, err := cookies.Fetch(ctx, cookies.Request{
		URL:    usageURL(now),
		Method: "GET",
		Headers: map[string]string{
			"Accept":     "*/*",
			"Origin":     adminBaseURL,
			"Referer":    adminBaseURL + "/organization/usage",
			"User-Agent": httputil.DefaultUserAgent,
		},
	})
	if err != nil {
		return usageSnapshot{}, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return usageSnapshot{}, &httputil.Error{
			Status:     resp.Status,
			StatusText: resp.StatusText,
			Body:       string(resp.Body),
			URL:        usageURL(now),
		}
	}
	var body billingResponse
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return usageSnapshot{}, fmt.Errorf("invalid Mistral JSON: %w", err)
	}
	return parseUsage(body, now), nil
}

// usageURL builds the current-month Mistral usage endpoint.
func usageURL(now time.Time) string {
	u, _ := url.Parse(adminBaseURL + "/api/billing/v2/usage")
	q := u.Query()
	q.Set("month", strconv.Itoa(int(now.Month())))
	q.Set("year", strconv.Itoa(now.Year()))
	u.RawQuery = q.Encode()
	return u.String()
}

// parseUsage aggregates Mistral usage categories into one monthly snapshot.
func parseUsage(body billingResponse, now time.Time) usageSnapshot {
	prices := priceIndex(body.Prices)
	var totalCost float64
	var inputTokens, outputTokens, cachedTokens, modelCount int
	if body.Completion != nil {
		input, output, cached, cost, models := aggregateModelMap(body.Completion.Models, prices)
		inputTokens += input
		outputTokens += output
		cachedTokens += cached
		totalCost += cost
		modelCount += models
	}
	for _, category := range []*modelCategory{body.OCR, body.Connectors, body.Audio} {
		if category == nil {
			continue
		}
		_, _, _, cost, _ := aggregateModelMap(category.Models, prices)
		totalCost += cost
	}
	if body.LibrariesAPI != nil {
		for _, category := range []*modelCategory{body.LibrariesAPI.Pages, body.LibrariesAPI.Tokens} {
			if category == nil {
				continue
			}
			_, _, _, cost, _ := aggregateModelMap(category.Models, prices)
			totalCost += cost
		}
	}
	if body.FineTuning != nil {
		for _, models := range []map[string]modelUsage{body.FineTuning.Training, body.FineTuning.Storage} {
			_, _, _, cost, _ := aggregateModelMap(models, prices)
			totalCost += cost
		}
	}
	if body.VibeUsage != nil {
		totalCost += *body.VibeUsage
	}
	currency := body.Currency
	if currency == "" {
		currency = "EUR"
	}
	symbol := body.CurrencySymbol
	if symbol == "" {
		symbol = "€"
	}
	var periodEnd *time.Time
	if t, ok := providerutil.TimeValue(body.EndDate); ok {
		t = t.Add(time.Second)
		periodEnd = &t
	}
	return usageSnapshot{
		TotalCost:      totalCost,
		Currency:       currency,
		CurrencySymbol: symbol,
		InputTokens:    inputTokens,
		OutputTokens:   outputTokens,
		CachedTokens:   cachedTokens,
		ModelCount:     modelCount,
		PeriodEnd:      periodEnd,
		UpdatedAt:      now,
	}
}

// priceIndex returns price-per-unit by billing metric/group.
func priceIndex(prices []priceEntry) map[string]float64 {
	out := map[string]float64{}
	for _, price := range prices {
		if price.BillingMetric == "" || price.BillingGroup == "" || price.Price == "" {
			continue
		}
		value, err := strconv.ParseFloat(price.Price, 64)
		if err != nil {
			continue
		}
		out[price.BillingMetric+"::"+price.BillingGroup] = value
	}
	return out
}

// aggregateModelMap sums tokens and cost for a model map.
func aggregateModelMap(models map[string]modelUsage, prices map[string]float64) (input, output, cached int, cost float64, count int) {
	for _, model := range models {
		count++
		modelInput, modelOutput, modelCached, modelCost := aggregateModel(model, prices)
		input += modelInput
		output += modelOutput
		cached += modelCached
		cost += modelCost
	}
	return input, output, cached, cost, count
}

// aggregateModel sums usage and cost for one model.
func aggregateModel(model modelUsage, prices map[string]float64) (input, output, cached int, cost float64) {
	input, inputCost := aggregateEntries(model.Input, prices)
	output, outputCost := aggregateEntries(model.Output, prices)
	cached, cachedCost := aggregateEntries(model.Cached, prices)
	return input, output, cached, inputCost + outputCost + cachedCost
}

// aggregateEntries sums usage entries and their computed cost.
func aggregateEntries(entries []usageEntry, prices map[string]float64) (tokens int, cost float64) {
	for _, entry := range entries {
		value := entryValue(entry)
		tokens += int(math.Round(value))
		if entry.BillingMetric != "" && entry.BillingGroup != "" {
			cost += value * prices[entry.BillingMetric+"::"+entry.BillingGroup]
		}
	}
	return tokens, cost
}

// entryValue returns paid value when present, falling back to raw value.
func entryValue(entry usageEntry) float64 {
	if entry.ValuePaid != nil {
		return *entry.ValuePaid
	}
	if entry.Value != nil {
		return *entry.Value
	}
	return 0
}

// snapshotFromUsage maps Mistral usage into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	value := fmt.Sprintf("%s%.4f", usage.CurrencySymbol, usage.TotalCost)
	caption := "No usage this month"
	if usage.TotalCost > 0 {
		caption = fmt.Sprintf("%d models", usage.ModelCount)
	}
	metric := providers.MetricValue{
		ID:              "session-percent",
		Label:           "MONTHLY",
		Name:            "Mistral monthly usage cost",
		Value:           value,
		NumericValue:    &usage.TotalCost,
		NumericUnit:     "dollars",
		NumericGoodWhen: "low",
		Caption:         caption,
		UpdatedAt:       now,
	}
	if usage.PeriodEnd != nil {
		metric.ResetInSeconds = providerutil.ResetSeconds(*usage.PeriodEnd)
	}
	return providers.Snapshot{
		ProviderID:   "mistral",
		ProviderName: "Mistral",
		Source:       "cookie",
		Metrics:      []providers.MetricValue{metric},
		Status:       "operational",
	}
}

// errorSnapshot returns a Mistral setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "mistral",
		ProviderName: "Mistral",
		Source:       "cookie",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// init registers the Mistral provider with the package registry.
func init() {
	providers.Register(Provider{})
}

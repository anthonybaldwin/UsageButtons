// Package mistral implements the Mistral billing usage provider.
//
// Auth: Usage Buttons Helper extension with the user's admin.mistral.ai
// browser session. Endpoint:
// https://admin.mistral.ai/api/billing/v2/usage?month=...&year=....
//
// One Stream Deck action emits a namespaced metric inventory derived
// from the single billing endpoint. The response already breaks spend
// down by category (completion / OCR / audio / connectors / libraries
// / fine-tuning / Vibe), so each category surfaces as its own metric
// alongside aggregate cost, token, and model-count metrics.
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

// Metric IDs surfaced by this provider. The set is namespaced under
// "monthly-" so a future "tier-progress" / "spend-limit-percent" can
// land alongside without colliding.
const (
	metricMonthlyCost            = "monthly-cost"
	metricMonthlyCostCompletion  = "monthly-cost-completion"
	metricMonthlyCostOCR         = "monthly-cost-ocr"
	metricMonthlyCostAudio       = "monthly-cost-audio"
	metricMonthlyCostConnectors  = "monthly-cost-connectors"
	metricMonthlyCostLibraries   = "monthly-cost-libraries"
	metricMonthlyCostFineTuning  = "monthly-cost-fine-tuning"
	metricMonthlyCostVibe        = "monthly-cost-vibe"
	metricMonthlyInputTokens     = "monthly-input-tokens"
	metricMonthlyOutputTokens    = "monthly-output-tokens"
	metricMonthlyCachedTokens    = "monthly-cached-tokens"
	metricModelCount             = "model-count"
	metricPeriodEnd              = "period-end"
)

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

// categorySpend captures per-category spend in the response currency.
// Each field maps 1:1 to a "monthly-cost-<category>" metric.
type categorySpend struct {
	Completion float64
	OCR        float64
	Audio      float64
	Connectors float64
	Libraries  float64
	FineTuning float64
	Vibe       float64
}

// Total returns the sum across all categories — the same number the
// previous single-metric implementation surfaced as "monthly-cost".
func (c categorySpend) Total() float64 {
	return c.Completion + c.OCR + c.Audio + c.Connectors + c.Libraries + c.FineTuning + c.Vibe
}

// usageSnapshot is the parsed Mistral monthly billing state.
type usageSnapshot struct {
	Spend          categorySpend
	Currency       string
	CurrencySymbol string
	InputTokens    int
	OutputTokens   int
	CachedTokens   int
	ModelCount     int
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

// MetricIDs enumerates the metrics this provider can emit. Order is
// the PI's preferred display order: aggregate spend first, then per-
// category, then token totals, then account-shape metrics. Used as the
// dropdown's default ordering and as the "first metric" fallback when
// a freshly-dropped key has no saved metric ID yet.
func (Provider) MetricIDs() []string {
	return []string{
		metricMonthlyCost,
		metricMonthlyCostCompletion,
		metricMonthlyCostOCR,
		metricMonthlyCostAudio,
		metricMonthlyCostConnectors,
		metricMonthlyCostLibraries,
		metricMonthlyCostFineTuning,
		metricMonthlyCostVibe,
		metricMonthlyInputTokens,
		metricMonthlyOutputTokens,
		metricMonthlyCachedTokens,
		metricModelCount,
		metricPeriodEnd,
	}
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

	var inputTokens, outputTokens, cachedTokens, modelCount int
	var spend categorySpend
	if body.Completion != nil {
		input, output, cached, cost, models := aggregateModelMap(body.Completion.Models, prices)
		inputTokens += input
		outputTokens += output
		cachedTokens += cached
		spend.Completion = cost
		modelCount += models
	}
	if body.OCR != nil {
		_, _, _, cost, _ := aggregateModelMap(body.OCR.Models, prices)
		spend.OCR = cost
	}
	if body.Audio != nil {
		_, _, _, cost, _ := aggregateModelMap(body.Audio.Models, prices)
		spend.Audio = cost
	}
	if body.Connectors != nil {
		_, _, _, cost, _ := aggregateModelMap(body.Connectors.Models, prices)
		spend.Connectors = cost
	}
	if body.LibrariesAPI != nil {
		var librariesCost float64
		for _, category := range []*modelCategory{body.LibrariesAPI.Pages, body.LibrariesAPI.Tokens} {
			if category == nil {
				continue
			}
			_, _, _, cost, _ := aggregateModelMap(category.Models, prices)
			librariesCost += cost
		}
		spend.Libraries = librariesCost
	}
	if body.FineTuning != nil {
		var ftCost float64
		for _, models := range []map[string]modelUsage{body.FineTuning.Training, body.FineTuning.Storage} {
			_, _, _, cost, _ := aggregateModelMap(models, prices)
			ftCost += cost
		}
		spend.FineTuning = ftCost
	}
	if body.VibeUsage != nil {
		spend.Vibe = *body.VibeUsage
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
		Spend:          spend,
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
//
// Mistral is pay-as-you-go with no quota limit, so cost metrics are
// raw amounts (NumericGoodWhen=low) — there's no remaining-percent
// concept. The total-cost metric carries the period reset countdown so
// pinning "MONTHLY" gives users both the current spend and how long
// until the cycle resets.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	resetSecs := periodResetSeconds(usage)

	var metrics []providers.MetricValue

	// Total monthly cost (was "session-percent" pre-rename).
	total := usage.Spend.Total()
	totalMetric := costMetric(metricMonthlyCost, "MONTHLY", "Mistral monthly usage cost", total, usage, now)
	if totalMetric.Caption == "" && total > 0 {
		totalMetric.Caption = fmt.Sprintf("%d models", usage.ModelCount)
	} else if total == 0 {
		totalMetric.Caption = "No usage this month"
	}
	if resetSecs != nil {
		totalMetric.ResetInSeconds = resetSecs
	}
	metrics = append(metrics, totalMetric)

	// Per-category cost. Emitted unconditionally so the PI dropdown
	// items always work — a zero spend renders as "€0.0000" with the
	// "No usage this month" caption when no categories saw activity.
	categories := []struct {
		id, label, name string
		amount          float64
	}{
		{metricMonthlyCostCompletion, "COMPLETION", "Completion model spend", usage.Spend.Completion},
		{metricMonthlyCostOCR, "OCR", "OCR pages spend", usage.Spend.OCR},
		{metricMonthlyCostAudio, "AUDIO", "Audio (Voxtral / TTS / STT) spend", usage.Spend.Audio},
		{metricMonthlyCostConnectors, "CONNECT", "Agent / connector spend", usage.Spend.Connectors},
		{metricMonthlyCostLibraries, "LIBS", "RAG library spend", usage.Spend.Libraries},
		{metricMonthlyCostFineTuning, "FT", "Fine-tuning training + storage spend", usage.Spend.FineTuning},
		{metricMonthlyCostVibe, "VIBE", "Mistral Vibe flat charge", usage.Spend.Vibe},
	}
	for _, c := range categories {
		m := costMetric(c.id, c.label, c.name, c.amount, usage, now)
		metrics = append(metrics, m)
	}

	// Token totals. Cached-on-input ratio surfaces in the input-tokens
	// caption so users can see cache-hit health without pinning a
	// separate metric — same data, friendlier presentation.
	inputCaption := ""
	if usage.InputTokens > 0 && usage.CachedTokens > 0 {
		ratio := float64(usage.CachedTokens) / float64(usage.InputTokens) * 100
		inputCaption = fmt.Sprintf("%.0f%% cached", ratio)
	}
	metrics = append(metrics, tokenMetric(metricMonthlyInputTokens, "INPUT", "Total input tokens MTD", usage.InputTokens, inputCaption, now))
	metrics = append(metrics, tokenMetric(metricMonthlyOutputTokens, "OUTPUT", "Total output tokens MTD", usage.OutputTokens, "", now))
	metrics = append(metrics, tokenMetric(metricMonthlyCachedTokens, "CACHED", "Cached input tokens MTD", usage.CachedTokens, "", now))

	// Distinct completion models used MTD.
	modelCountValue := float64(usage.ModelCount)
	modelMetric := providers.MetricValue{
		ID:           metricModelCount,
		Label:        "MODELS",
		Name:         "Distinct completion models used",
		Value:        usage.ModelCount,
		NumericValue: &modelCountValue,
		NumericUnit:  "count",
		Caption:      "this month",
		UpdatedAt:    now,
	}
	metrics = append(metrics, modelMetric)

	// Days until billing reset. Surfaces as a countdown on the button
	// face via ResetInSeconds; the value is the human-readable day
	// count for the rare provider that bypasses ResetInSeconds.
	periodMetric := providers.MetricValue{
		ID:        metricPeriodEnd,
		Label:     "RESET",
		Name:      "Days until billing reset",
		Value:     "—",
		Caption:   "monthly cycle",
		UpdatedAt: now,
	}
	if resetSecs != nil {
		days := math.Ceil(*resetSecs / 86400)
		daysVal := days
		periodMetric.Value = fmt.Sprintf("%.0fd", days)
		periodMetric.NumericValue = &daysVal
		periodMetric.NumericUnit = "count"
		periodMetric.ResetInSeconds = resetSecs
	}
	metrics = append(metrics, periodMetric)

	return providers.Snapshot{
		ProviderID:   "mistral",
		ProviderName: "Mistral",
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// costMetric builds a numeric-currency metric. Mistral's currency
// symbol is used verbatim — no synthetic conversion to USD even if
// the user expects dollars. Four decimals because per-token spend on
// the smaller models often rounds to <€0.01 over a month, and a "€0"
// face would be misleading after a real but tiny session.
func costMetric(id, label, name string, amount float64, usage usageSnapshot, now string) providers.MetricValue {
	value := fmt.Sprintf("%s%.4f", usage.CurrencySymbol, amount)
	amt := amount
	return providers.MetricValue{
		ID:              id,
		Label:           label,
		Name:            name,
		Value:           value,
		NumericValue:    &amt,
		NumericUnit:     "dollars",
		NumericGoodWhen: "low",
		UpdatedAt:       now,
	}
}

// tokenMetric builds an integer count metric.
func tokenMetric(id, label, name string, count int, caption, now string) providers.MetricValue {
	v := float64(count)
	return providers.MetricValue{
		ID:           id,
		Label:        label,
		Name:         name,
		Value:        count,
		NumericValue: &v,
		NumericUnit:  "count",
		Caption:      caption,
		UpdatedAt:    now,
	}
}

// periodResetSeconds returns seconds until the billing-period end as a
// pointer suitable for MetricValue.ResetInSeconds.
func periodResetSeconds(usage usageSnapshot) *float64 {
	if usage.PeriodEnd == nil {
		return nil
	}
	return providerutil.ResetSeconds(*usage.PeriodEnd)
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

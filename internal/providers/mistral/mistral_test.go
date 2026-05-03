package mistral

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

// loadFixture decodes the testdata billing response into a billingResponse.
func loadFixture(t *testing.T) billingResponse {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "billing-v2-usage.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var body billingResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return body
}

// findMetric looks up a metric by ID; fails the test if absent so the
// rest of the assertion reads naturally.
func findMetric(t *testing.T, snap providers.Snapshot, id string) providers.MetricValue {
	t.Helper()
	for _, m := range snap.Metrics {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("metric %q not present in snapshot", id)
	return providers.MetricValue{}
}

func TestParseUsage_TokenTotalsAndModelCount(t *testing.T) {
	body := loadFixture(t)
	got := parseUsage(body, time.Now().UTC())

	// mistral-large input: 11121, mistral-small input: 20+100=120.
	if want := 11121 + 120; got.InputTokens != want {
		t.Errorf("InputTokens = %d, want %d", got.InputTokens, want)
	}
	// mistral-large output: 1115, mistral-small output: 500+2482=2982.
	if want := 1115 + 2982; got.OutputTokens != want {
		t.Errorf("OutputTokens = %d, want %d", got.OutputTokens, want)
	}
	// mistral-small cached: 50.
	if want := 50; got.CachedTokens != want {
		t.Errorf("CachedTokens = %d, want %d", got.CachedTokens, want)
	}
	if got.ModelCount != 2 {
		t.Errorf("ModelCount = %d, want 2", got.ModelCount)
	}
	if got.Currency != "EUR" {
		t.Errorf("Currency = %q, want %q", got.Currency, "EUR")
	}
	if got.CurrencySymbol != "€" {
		t.Errorf("CurrencySymbol = %q, want €", got.CurrencySymbol)
	}
}

func TestParseUsage_CategorySpend(t *testing.T) {
	body := loadFixture(t)
	got := parseUsage(body, time.Now().UTC())

	// Completion cost — same math as CodexBar's existing test, plus a
	// small cached-token contribution from the fixture (50 * 4.25e-8).
	expectedCompletion := 11121*0.0000017 + 1115*0.0000051 + 120*8.5e-8 + 2982*2.55e-7 + 50*4.25e-8
	if !approxEqual(got.Spend.Completion, expectedCompletion, 1e-6) {
		t.Errorf("Spend.Completion = %v, want %v", got.Spend.Completion, expectedCompletion)
	}
	// OCR: 200 pages * 0.001.
	if !approxEqual(got.Spend.OCR, 0.2, 1e-9) {
		t.Errorf("Spend.OCR = %v, want 0.2", got.Spend.OCR)
	}
	// Audio: 4 minutes * 0.05.
	if !approxEqual(got.Spend.Audio, 0.2, 1e-9) {
		t.Errorf("Spend.Audio = %v, want 0.2", got.Spend.Audio)
	}
	// Connectors: 10 calls * 0.01.
	if !approxEqual(got.Spend.Connectors, 0.1, 1e-9) {
		t.Errorf("Spend.Connectors = %v, want 0.1", got.Spend.Connectors)
	}
	// Libraries: 5 pages * 0.02 + 1000 tokens * 0.0001.
	if !approxEqual(got.Spend.Libraries, 0.2, 1e-9) {
		t.Errorf("Spend.Libraries = %v, want 0.2", got.Spend.Libraries)
	}
	// Fine-tuning: 2 * 0.5 + 1 * 0.25.
	if !approxEqual(got.Spend.FineTuning, 1.25, 1e-9) {
		t.Errorf("Spend.FineTuning = %v, want 1.25", got.Spend.FineTuning)
	}
	// Vibe: 1.5 from response.
	if !approxEqual(got.Spend.Vibe, 1.5, 1e-9) {
		t.Errorf("Spend.Vibe = %v, want 1.5", got.Spend.Vibe)
	}

	// Sanity: total = sum of all categories.
	want := got.Spend.Completion + got.Spend.OCR + got.Spend.Audio +
		got.Spend.Connectors + got.Spend.Libraries + got.Spend.FineTuning + got.Spend.Vibe
	if !approxEqual(got.Spend.Total(), want, 1e-9) {
		t.Errorf("Spend.Total() = %v, want %v", got.Spend.Total(), want)
	}
}

func TestParseUsage_PeriodEnd(t *testing.T) {
	body := loadFixture(t)
	got := parseUsage(body, time.Now().UTC())

	if got.PeriodEnd == nil {
		t.Fatal("PeriodEnd is nil; expected parsed end_date")
	}
	// The +1 second nudge in parseUsage rolls "2025-11-30T23:59:59.999Z"
	// over the midnight boundary so countdown math hits the next cycle
	// cleanly. Either side of the boundary is acceptable here.
	if got.PeriodEnd.Year() != 2025 {
		t.Errorf("PeriodEnd year = %d, want 2025", got.PeriodEnd.Year())
	}
	if month := got.PeriodEnd.Month(); month != time.November && month != time.December {
		t.Errorf("PeriodEnd month = %v, want November or December", month)
	}
}

func TestParseUsage_EmptyResponse(t *testing.T) {
	body := billingResponse{
		Completion:     &modelCategory{Models: map[string]modelUsage{}},
		OCR:            &modelCategory{Models: map[string]modelUsage{}},
		Connectors:     &modelCategory{Models: map[string]modelUsage{}},
		LibrariesAPI:   &librariesCategory{},
		FineTuning:     &fineTuningCategory{},
		Audio:          &modelCategory{Models: map[string]modelUsage{}},
		Currency:       "EUR",
		CurrencySymbol: "€",
	}
	got := parseUsage(body, time.Now().UTC())
	if got.Spend.Total() != 0 {
		t.Errorf("Spend.Total() = %v, want 0 for empty response", got.Spend.Total())
	}
	if got.ModelCount != 0 {
		t.Errorf("ModelCount = %d, want 0", got.ModelCount)
	}
}

// TestSnapshotFromUsage_AllMetricsPresent asserts every namespaced
// metric ID surfaces in the snapshot — this is the v1 acceptance gate
// from plans/mistral-tier-coverage.md. Buttons bound to any of these
// IDs need a hit, otherwise the dropdown advertises a metric the
// provider can't actually emit.
func TestSnapshotFromUsage_AllMetricsPresent(t *testing.T) {
	body := loadFixture(t)
	usage := parseUsage(body, mustParse("2025-11-15T12:00:00Z"))
	snap := snapshotFromUsage(usage)

	wantIDs := []string{
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
	if len(wantIDs) != len(Provider{}.MetricIDs()) {
		t.Errorf("MetricIDs() length %d, test expects %d", len(Provider{}.MetricIDs()), len(wantIDs))
	}

	got := map[string]providers.MetricValue{}
	for _, m := range snap.Metrics {
		got[m.ID] = m
	}
	for _, id := range wantIDs {
		if _, ok := got[id]; !ok {
			t.Errorf("snapshot missing metric %q", id)
		}
	}
}

func TestSnapshotFromUsage_MonthlyCostFormatting(t *testing.T) {
	body := loadFixture(t)
	usage := parseUsage(body, mustParse("2025-11-15T12:00:00Z"))
	snap := snapshotFromUsage(usage)

	m := findMetric(t, snap, metricMonthlyCost)
	value, ok := m.Value.(string)
	if !ok {
		t.Fatalf("Value type %T, want string", m.Value)
	}
	// Currency symbol from response is preserved verbatim.
	if value[:len("€")] != "€" {
		t.Errorf("Value %q does not start with €", value)
	}
	if m.NumericValue == nil || *m.NumericValue != usage.Spend.Total() {
		t.Errorf("NumericValue = %v, want %v", m.NumericValue, usage.Spend.Total())
	}
	if m.NumericGoodWhen != "low" {
		t.Errorf("NumericGoodWhen = %q, want low", m.NumericGoodWhen)
	}
	if m.ResetInSeconds == nil {
		t.Error("ResetInSeconds nil; expected populated from end_date")
	}
}

func TestSnapshotFromUsage_TokensAndModelCount(t *testing.T) {
	body := loadFixture(t)
	usage := parseUsage(body, mustParse("2025-11-15T12:00:00Z"))
	snap := snapshotFromUsage(usage)

	in := findMetric(t, snap, metricMonthlyInputTokens)
	if got, _ := in.Value.(int); got != usage.InputTokens {
		t.Errorf("input tokens Value = %v, want %d", in.Value, usage.InputTokens)
	}
	// Cached ratio should appear in caption when both >0.
	if usage.CachedTokens > 0 && in.Caption == "" {
		t.Error("input tokens caption empty; expected '%% cached' suffix")
	}

	out := findMetric(t, snap, metricMonthlyOutputTokens)
	if got, _ := out.Value.(int); got != usage.OutputTokens {
		t.Errorf("output tokens Value = %v, want %d", out.Value, usage.OutputTokens)
	}

	cached := findMetric(t, snap, metricMonthlyCachedTokens)
	if got, _ := cached.Value.(int); got != usage.CachedTokens {
		t.Errorf("cached tokens Value = %v, want %d", cached.Value, usage.CachedTokens)
	}

	mc := findMetric(t, snap, metricModelCount)
	if got, _ := mc.Value.(int); got != usage.ModelCount {
		t.Errorf("model-count Value = %v, want %d", mc.Value, usage.ModelCount)
	}
}

func TestMetricIDsContainsMonthlyCost(t *testing.T) {
	// Lock in the rename: "session-percent" is gone; "monthly-cost" is
	// the canonical ID for the aggregate spend metric. The migration
	// helper in cmd/plugin/main.go rebinds existing buttons.
	ids := Provider{}.MetricIDs()
	for _, id := range ids {
		if id == "session-percent" {
			t.Error("MetricIDs() still advertises legacy 'session-percent'")
		}
	}
	hasMonthly := false
	for _, id := range ids {
		if id == metricMonthlyCost {
			hasMonthly = true
			break
		}
	}
	if !hasMonthly {
		t.Errorf("MetricIDs() missing %q", metricMonthlyCost)
	}
}

func mustParse(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func approxEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

package openai

import (
	"testing"
	"time"
)

func TestSumResultsUSD_DollarsNotCents(t *testing.T) {
	got := sumResultsUSD([]costResult{
		{Amount: &costAmount{Value: 0.06, Currency: "usd"}},
		{Amount: &costAmount{Value: 12.50, Currency: "usd"}},
		{Amount: nil},
		{Amount: &costAmount{Value: 99.99, Currency: "eur"}},
		{Amount: &costAmount{Value: 1.00, Currency: ""}},
	})
	want := 0.06 + 12.50 + 1.00
	if !floatNear(got, want, 1e-9) {
		t.Errorf("sumResultsUSD = %v, want %v (USD only, dollars not cents)", got, want)
	}
}

func TestSumWindows_SlicesByUnixTime(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	todayStart := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	buckets := []costBucket{
		{
			StartTime: todayStart.Unix(),
			EndTime:   now.Unix(),
			Results:   []costResult{{Amount: &costAmount{Value: 5.00, Currency: "usd"}}},
		},
		{
			StartTime: time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC).Unix(),
			EndTime:   time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC).Unix(),
			Results:   []costResult{{Amount: &costAmount{Value: 10.00, Currency: "usd"}}},
		},
		{
			StartTime: time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC).Unix(),
			EndTime:   time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC).Unix(),
			Results:   []costResult{{Amount: &costAmount{Value: 7.00, Currency: "usd"}}},
		},
	}

	today, mtd, last30 := sumWindows(buckets, todayStart, monthStart)
	if !floatNear(today, 5.0, 1e-9) {
		t.Errorf("today = %v, want $5.00", today)
	}
	if !floatNear(mtd, 15.0, 1e-9) {
		t.Errorf("mtd = %v, want $15.00", mtd)
	}
	if !floatNear(last30, 22.0, 1e-9) {
		t.Errorf("last30 = %v, want $22.00", last30)
	}
}

func TestBuildMetrics_AllThreeWindows(t *testing.T) {
	got := buildMetrics(5.0, 15.0, 22.0, "now")
	if len(got) != 3 {
		t.Fatalf("metric count = %d, want 3", len(got))
	}
	if got[0].ID != "cost-today" || got[0].Value != "$5.00" {
		t.Errorf("today metric = %+v", got[0])
	}
	for _, m := range got {
		if m.NumericGoodWhen != "low" {
			t.Errorf("metric %s should rank low-is-good, got %q", m.ID, m.NumericGoodWhen)
		}
	}
}

func floatNear(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

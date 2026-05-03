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

	mkBucket := func(d time.Time, dollars float64) costBucket {
		return costBucket{
			StartTime: d.Unix(),
			EndTime:   d.Add(24 * time.Hour).Unix(),
			Results:   []costResult{{Amount: &costAmount{Value: dollars, Currency: "usd"}}},
		}
	}
	day := func(month, dayOfMonth int) time.Time {
		return time.Date(2026, time.Month(month), dayOfMonth, 0, 0, 0, 0, time.UTC)
	}

	buckets := []costBucket{
		mkBucket(day(5, 15), 5.00),  // Today
		mkBucket(day(5, 14), 2.00),  // Yesterday
		mkBucket(day(5, 10), 3.00),  // 5 days ago (in 7d, mtd, 30d)
		mkBucket(day(5, 8), 10.00),  // Earlier this month, outside 7d
		mkBucket(day(4, 28), 7.00),  // Previous month, in 30d
	}

	w := sumWindows(buckets, now)
	if !floatNear(w.today, 5.0, 1e-9) {
		t.Errorf("today = %v, want $5.00", w.today)
	}
	if !floatNear(w.yesterday, 2.0, 1e-9) {
		t.Errorf("yesterday = %v, want $2.00", w.yesterday)
	}
	if !floatNear(w.last7d, 10.0, 1e-9) {
		t.Errorf("last7d = %v, want $10.00", w.last7d)
	}
	if !floatNear(w.mtd, 20.0, 1e-9) {
		t.Errorf("mtd = %v, want $20.00", w.mtd)
	}
	if !floatNear(w.last30d, 27.0, 1e-9) {
		t.Errorf("last30d = %v, want $27.00", w.last30d)
	}
	if w.daysElapsed != 15 {
		t.Errorf("daysElapsed = %d, want 15", w.daysElapsed)
	}
	if w.daysInMonth != 31 {
		t.Errorf("daysInMonth = %d, want 31", w.daysInMonth)
	}
}

func TestBuildMetrics_SevenWindows(t *testing.T) {
	w := costWindows{
		today: 5.0, yesterday: 2.0,
		last7d: 14.0, mtd: 30.0, last30d: 60.0,
		daysElapsed: 15, daysInMonth: 31,
	}
	got := buildMetrics(w, "now")
	if len(got) != 7 {
		t.Fatalf("metric count = %d, want 7", len(got))
	}
	wantValues := map[string]string{
		"cost-today":           "$5.00",
		"cost-yesterday":       "$2.00",
		"cost-7d":              "$14.00",
		"cost-mtd":             "$30.00",
		"cost-30d":             "$60.00",
		"cost-burn-7d":         "$2.00",
		"cost-projected-month": "$62.00",
	}
	for _, m := range got {
		want, ok := wantValues[m.ID]
		if !ok {
			t.Errorf("unexpected metric ID %q", m.ID)
			continue
		}
		if m.Value != want {
			t.Errorf("metric %s value = %q, want %q", m.ID, m.Value, want)
		}
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

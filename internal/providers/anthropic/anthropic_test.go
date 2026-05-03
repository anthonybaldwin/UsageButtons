package anthropic

import (
	"testing"
	"time"
)

func TestSumResultsUSD_DecimalCentsToUSD(t *testing.T) {
	got := sumResultsUSD([]costResult{
		{Amount: "12345", Currency: "USD"},   // $123.45
		{Amount: "100.5", Currency: "USD"},   // $1.005
		{Amount: "0", Currency: "USD"},       // $0
		{Amount: "not-num", Currency: "USD"}, // skipped
	})
	want := 123.45 + 1.005
	if !floatNear(got, want, 1e-9) {
		t.Errorf("sumResultsUSD = %v, want %v", got, want)
	}
}

func TestSumWindows_SlicesByDate(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

	mkBucket := func(d time.Time, cents string) costBucket {
		return costBucket{
			StartingAt: d.Format(time.RFC3339),
			EndingAt:   d.Add(24 * time.Hour).Format(time.RFC3339),
			Results:    []costResult{{Amount: cents, Currency: "USD"}},
		}
	}
	day := func(month, dayOfMonth int) time.Time {
		return time.Date(2026, time.Month(month), dayOfMonth, 0, 0, 0, 0, time.UTC)
	}

	buckets := []costBucket{
		mkBucket(day(5, 15), "500"),  // Today: $5.00
		mkBucket(day(5, 14), "200"),  // Yesterday: $2.00 (also in 7d, mtd, 30d)
		mkBucket(day(5, 10), "300"),  // 5 days ago: $3.00 (in 7d, mtd, 30d)
		mkBucket(day(5, 8), "1000"),  // Earlier this month, outside 7d: $10.00
		mkBucket(day(4, 28), "700"),  // Previous month, in 30d: $7.00
		// Garbage timestamp: skipped silently.
		{StartingAt: "not-a-time", Results: []costResult{{Amount: "999999", Currency: "USD"}}},
	}

	w := sumWindows(buckets, now)
	if !floatNear(w.today, 5.0, 1e-9) {
		t.Errorf("today = %v, want $5.00", w.today)
	}
	if !floatNear(w.yesterday, 2.0, 1e-9) {
		t.Errorf("yesterday = %v, want $2.00", w.yesterday)
	}
	if !floatNear(w.last7d, 10.0, 1e-9) {
		t.Errorf("last7d = %v, want $10.00 (today + yesterday + 5 days ago)", w.last7d)
	}
	if !floatNear(w.mtd, 20.0, 1e-9) {
		t.Errorf("mtd = %v, want $20.00", w.mtd)
	}
	if !floatNear(w.last30d, 27.0, 1e-9) {
		t.Errorf("last30d = %v, want $27.00 (everything except the garbage row)", w.last30d)
	}
	if w.daysElapsed != 15 {
		t.Errorf("daysElapsed = %d, want 15", w.daysElapsed)
	}
	if w.daysInMonth != 31 {
		t.Errorf("daysInMonth = %d, want 31 (May)", w.daysInMonth)
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
		"cost-burn-7d":         "$2.00", // 14 / 7
		"cost-projected-month": "$62.00", // 30 * 31 / 15
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
			t.Errorf("metric %s should rank low-is-good (it's a cost), got %q", m.ID, m.NumericGoodWhen)
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

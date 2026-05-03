package anthropicadmin

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
	todayStart := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	buckets := []costBucket{
		// Today: $5.00
		{
			StartingAt: todayStart.Format(time.RFC3339),
			EndingAt:   now.Format(time.RFC3339),
			Results:    []costResult{{Amount: "500", Currency: "USD"}},
		},
		// Earlier this month: $10.00
		{
			StartingAt: time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			EndingAt:   time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			Results:    []costResult{{Amount: "1000", Currency: "USD"}},
		},
		// Previous month, within 30d: $7.00
		{
			StartingAt: time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			EndingAt:   time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			Results:    []costResult{{Amount: "700", Currency: "USD"}},
		},
		// Garbage timestamp: skipped silently
		{
			StartingAt: "not-a-time",
			Results:    []costResult{{Amount: "999999", Currency: "USD"}},
		},
	}

	today, mtd, last30 := sumWindows(buckets, todayStart, monthStart)
	if !floatNear(today, 5.0, 1e-9) {
		t.Errorf("today = %v, want $5.00", today)
	}
	if !floatNear(mtd, 15.0, 1e-9) {
		t.Errorf("mtd = %v, want $15.00 (today + earlier-this-month)", mtd)
	}
	if !floatNear(last30, 22.0, 1e-9) {
		t.Errorf("last30 = %v, want $22.00 (everything except the garbage row)", last30)
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
	if got[1].ID != "cost-mtd" || got[1].Value != "$15.00" {
		t.Errorf("mtd metric = %+v", got[1])
	}
	if got[2].ID != "cost-30d" || got[2].Value != "$22.00" {
		t.Errorf("30d metric = %+v", got[2])
	}
	for _, m := range got {
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

package deepseek

import (
	"strings"
	"testing"
	"time"
)

func TestPickBalance_PrefersUSD(t *testing.T) {
	got, err := pickBalance([]balanceInfo{
		{Currency: "CNY", TotalBalance: "70.00", GrantedBalance: "0", ToppedUpBalance: "70.00"},
		{Currency: "USD", TotalBalance: "10.00", GrantedBalance: "5.00", ToppedUpBalance: "5.00"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.currency != "USD" {
		t.Errorf("currency = %q, want USD", got.currency)
	}
	if got.symbol != "$" {
		t.Errorf("symbol = %q, want $", got.symbol)
	}
	if got.total != 10.0 || got.granted != 5.0 || got.toppedUp != 5.0 {
		t.Errorf("amounts = (%v, %v, %v), want (10, 5, 5)", got.total, got.granted, got.toppedUp)
	}
}

func TestPickBalance_FallsBackWhenNoUSD(t *testing.T) {
	got, err := pickBalance([]balanceInfo{
		{Currency: "CNY", TotalBalance: "70.00", GrantedBalance: "10.00", ToppedUpBalance: "60.00"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.currency != "CNY" {
		t.Errorf("currency = %q, want CNY", got.currency)
	}
	if got.symbol != "¥" {
		t.Errorf("symbol = %q, want ¥", got.symbol)
	}
}

func TestPickBalance_EmptyEntriesFails(t *testing.T) {
	if _, err := pickBalance(nil); err == nil {
		t.Fatalf("expected error for empty balance_infos, got nil")
	}
}

func TestPickBalance_NonNumericFails(t *testing.T) {
	_, err := pickBalance([]balanceInfo{
		{Currency: "USD", TotalBalance: "not-a-number", GrantedBalance: "0", ToppedUpBalance: "0"},
	})
	if err == nil {
		t.Fatalf("expected parse error for non-numeric balance, got nil")
	}
}

func TestBuildAPIMetrics_PrimaryShowsBreakdown(t *testing.T) {
	p := parsedBalance{currency: "USD", symbol: "$", total: 10.0, granted: 3.0, toppedUp: 7.0}
	got := buildAPIMetrics(p, true, "now")
	if len(got) < 3 {
		t.Fatalf("metric count = %d, want at least 3 (balance + topped-up + granted)", len(got))
	}
	if got[0].ID != "balance" || got[0].Value != "$10.00" {
		t.Errorf("balance metric = %+v", got[0])
	}
	if !strings.Contains(got[0].Caption, "Paid $7.00") || !strings.Contains(got[0].Caption, "Granted $3.00") {
		t.Errorf("caption = %q, want to contain 'Paid $7.00' and 'Granted $3.00'", got[0].Caption)
	}
}

func TestBuildAPIMetrics_ZeroBalanceHasAddCreditsCaption(t *testing.T) {
	p := parsedBalance{currency: "USD", symbol: "$", total: 0, granted: 0, toppedUp: 0}
	got := buildAPIMetrics(p, true, "now")
	if !strings.Contains(got[0].Caption, "Add credits") {
		t.Errorf("caption = %q, want to mention adding credits", got[0].Caption)
	}
}

func TestBuildAPIMetrics_UnavailableSurfacesInCaption(t *testing.T) {
	p := parsedBalance{currency: "USD", symbol: "$", total: 5.0, granted: 0, toppedUp: 5.0}
	got := buildAPIMetrics(p, false, "now")
	if !strings.Contains(got[0].Caption, "unavailable") {
		t.Errorf("caption = %q, want to surface 'unavailable'", got[0].Caption)
	}
}

func TestBuildAPIMetrics_StubsCostWindowsWithHelperPrompt(t *testing.T) {
	p := parsedBalance{currency: "USD", symbol: "$", total: 5.0, granted: 0, toppedUp: 5.0}
	got := buildAPIMetrics(p, true, "now")
	// Expect all 11 metric IDs present so the PI picker stays stable
	// regardless of which fetch path is live.
	wantIDs := []string{"balance", "topped-up", "granted",
		"cost-today", "cost-yesterday", "cost-7d", "cost-mtd", "cost-30d",
		"cost-burn-7d", "cost-projected-month", "tokens-mtd"}
	if len(got) != len(wantIDs) {
		t.Fatalf("metric count = %d, want %d", len(got), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("metric[%d].ID = %q, want %q", i, got[i].ID, want)
		}
	}
	// Stubs should mention the Helper extension.
	stub := got[3] // cost-today
	if !strings.Contains(stub.Caption, "Helper") {
		t.Errorf("cost-today stub caption = %q, want to mention Helper extension", stub.Caption)
	}
}

func TestBucketDailyCosts_SlicesByDate(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	day := func(month, dayOfMonth int) string {
		return time.Date(2026, time.Month(month), dayOfMonth, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	}
	daily := map[string]float64{
		day(5, 15): 5.00,  // Today
		day(5, 14): 2.00,  // Yesterday
		day(5, 10): 3.00,  // 5 days ago (in 7d, mtd, 30d)
		day(5, 8):  10.00, // Earlier this month, outside 7d
		day(4, 28): 7.00,  // Previous month, in 30d
		day(4, 1):  100.0, // Way earlier, outside 30d
	}
	w := bucketDailyCosts(daily, now)
	if w.today != 5.0 {
		t.Errorf("today = %v, want $5.00", w.today)
	}
	if w.yesterday != 2.0 {
		t.Errorf("yesterday = %v, want $2.00", w.yesterday)
	}
	if w.last7d != 10.0 {
		t.Errorf("last7d = %v, want $10.00", w.last7d)
	}
	if w.mtd != 20.0 {
		t.Errorf("mtd = %v, want $20.00", w.mtd)
	}
	if w.last30d != 27.0 {
		t.Errorf("last30d = %v, want $27.00 (excludes 4/1 which is outside 30d)", w.last30d)
	}
	if w.daysElapsed != 15 || w.daysInMonth != 31 {
		t.Errorf("daysElapsed/inMonth = (%d, %d), want (15, 31)", w.daysElapsed, w.daysInMonth)
	}
}

func TestMergeDailyCosts_SumsAcrossModelsAndTypes(t *testing.T) {
	curr := platformCostBucket{
		Currency: "USD",
		Days: []platformDayCost{
			{
				Date: "2026-05-15",
				Data: []platformModelUse{
					{Model: "deepseek-chat", Usage: []platformUsageLineItem{
						{Type: "input", Amount: "1.50"},
						{Type: "output", Amount: "2.25"},
					}},
					{Model: "deepseek-reasoner", Usage: []platformUsageLineItem{
						{Type: "input", Amount: "0.75"},
					}},
				},
			},
			{
				Date: "2026-05-14",
				Data: []platformModelUse{
					{Model: "deepseek-chat", Usage: []platformUsageLineItem{
						{Type: "output", Amount: "1.00"},
					}},
				},
			},
		},
	}
	got, currency := mergeDailyCosts(curr, platformCostBucket{})
	if currency != "USD" {
		t.Errorf("currency = %q, want USD", currency)
	}
	wantToday := 1.50 + 2.25 + 0.75 // $4.50
	if abs(got["2026-05-15"]-wantToday) > 1e-9 {
		t.Errorf("2026-05-15 = %v, want %v", got["2026-05-15"], wantToday)
	}
	if abs(got["2026-05-14"]-1.00) > 1e-9 {
		t.Errorf("2026-05-14 = %v, want 1.00", got["2026-05-14"])
	}
}

func TestSummaryToBalance_PrefersUSDWallets(t *testing.T) {
	s := platformSummaryData{
		NormalWallets: []platformWallet{
			{Currency: "CNY", Balance: "70.00"},
			{Currency: "USD", Balance: "5.50"},
		},
		BonusWallets: []platformWallet{
			{Currency: "USD", Balance: "1.25"},
		},
		MonthlyTokenUsage: 12345,
	}
	b := summaryToBalance(s)
	if b.currency != "USD" || b.symbol != "$" {
		t.Errorf("currency/symbol = (%q, %q), want USD/$", b.currency, b.symbol)
	}
	if abs(b.toppedUp-5.50) > 1e-9 {
		t.Errorf("toppedUp = %v, want 5.50", b.toppedUp)
	}
	if abs(b.granted-1.25) > 1e-9 {
		t.Errorf("granted = %v, want 1.25", b.granted)
	}
	if abs(b.total-6.75) > 1e-9 {
		t.Errorf("total = %v, want 6.75", b.total)
	}
}

func TestFormatTokens_AddsCommas(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{42, "42"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
	}
	for _, c := range cases {
		if got := formatTokens(c.in); got != c.want {
			t.Errorf("formatTokens(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

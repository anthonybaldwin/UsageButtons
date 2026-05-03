package deepseek

import (
	"strings"
	"testing"
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

func TestBuildMetrics_PrimaryShowsBreakdown(t *testing.T) {
	p := parsed{currency: "USD", symbol: "$", total: 10.0, granted: 3.0, toppedUp: 7.0}
	got := buildMetrics(p, true, "now")
	if len(got) != 3 {
		t.Fatalf("metric count = %d, want 3 (balance + topped-up + granted)", len(got))
	}
	if got[0].ID != "balance" || got[0].Value != "$10.00" {
		t.Errorf("balance metric = %+v", got[0])
	}
	if !strings.Contains(got[0].Caption, "Paid $7.00") || !strings.Contains(got[0].Caption, "Granted $3.00") {
		t.Errorf("caption = %q, want to contain 'Paid $7.00' and 'Granted $3.00'", got[0].Caption)
	}
}

func TestBuildMetrics_ZeroBalanceHasAddCreditsCaption(t *testing.T) {
	p := parsed{currency: "USD", symbol: "$", total: 0, granted: 0, toppedUp: 0}
	got := buildMetrics(p, true, "now")
	if len(got) != 1 {
		t.Fatalf("metric count = %d, want 1 (only primary, no breakdown when zero)", len(got))
	}
	if !strings.Contains(got[0].Caption, "Add credits") {
		t.Errorf("caption = %q, want to mention adding credits", got[0].Caption)
	}
}

func TestBuildMetrics_UnavailableSurfacesInCaption(t *testing.T) {
	p := parsed{currency: "USD", symbol: "$", total: 5.0, granted: 0, toppedUp: 5.0}
	got := buildMetrics(p, false, "now")
	if !strings.Contains(got[0].Caption, "unavailable") {
		t.Errorf("caption = %q, want to surface 'unavailable'", got[0].Caption)
	}
}

func TestBuildMetrics_OnlyGrantedNoPaid(t *testing.T) {
	p := parsed{currency: "USD", symbol: "$", total: 3.0, granted: 3.0, toppedUp: 0}
	got := buildMetrics(p, true, "now")
	// Expect: balance + granted (no topped-up since toppedUp == 0).
	if len(got) != 2 {
		t.Fatalf("metric count = %d, want 2 (balance + granted)", len(got))
	}
	if got[1].ID != "granted" {
		t.Errorf("second metric ID = %q, want granted", got[1].ID)
	}
}

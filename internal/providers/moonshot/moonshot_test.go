package moonshot

import (
	"strings"
	"testing"
)

func TestBuildMetrics_HealthyAccountShowsBreakdown(t *testing.T) {
	got := buildMetrics(balanceData{
		AvailableBalance: 49.59,
		VoucherBalance:   46.59,
		CashBalance:      3.00,
	}, "now")
	if len(got) != 3 {
		t.Fatalf("metric count = %d, want 3 (balance + voucher + cash)", len(got))
	}
	if got[0].ID != "balance" || got[0].Value != "$49.59" {
		t.Errorf("balance metric = %+v", got[0])
	}
	if !strings.Contains(got[0].Caption, "Voucher $46.59") || !strings.Contains(got[0].Caption, "Cash $3.00") {
		t.Errorf("caption = %q, want voucher + cash breakdown", got[0].Caption)
	}
	if got[1].ID != "voucher" || got[2].ID != "cash" {
		t.Errorf("metric ordering wrong: %v", []string{got[1].ID, got[2].ID})
	}
}

func TestBuildMetrics_ZeroAvailableSurfacesInferenceDisabled(t *testing.T) {
	got := buildMetrics(balanceData{
		AvailableBalance: 0,
		VoucherBalance:   0,
		CashBalance:      0,
	}, "now")
	if len(got) != 1 {
		t.Fatalf("metric count = %d, want 1 (only primary, all buckets zero)", len(got))
	}
	if !strings.Contains(strings.ToLower(got[0].Caption), "inference disabled") {
		t.Errorf("caption = %q, want to mention inference disabled", got[0].Caption)
	}
}

func TestBuildMetrics_NegativeCashOverridesBreakdownCaption(t *testing.T) {
	got := buildMetrics(balanceData{
		AvailableBalance: 5.0,
		VoucherBalance:   5.0,
		CashBalance:      -2.5,
	}, "now")
	if !strings.Contains(got[0].Caption, "overdrawn") {
		t.Errorf("primary caption = %q, want to surface overdrawn cash", got[0].Caption)
	}
	if !strings.Contains(got[0].Caption, "$2.50") {
		t.Errorf("primary caption = %q, want absolute deficit value $2.50", got[0].Caption)
	}
	// Cash tile still shown when negative.
	hasCash := false
	for _, m := range got {
		if m.ID == "cash" {
			hasCash = true
			if !strings.Contains(strings.ToLower(m.Caption), "overdrawn") {
				t.Errorf("cash tile caption = %q, want overdrawn", m.Caption)
			}
		}
	}
	if !hasCash {
		t.Errorf("cash tile suppressed when negative; want it shown")
	}
}

func TestBuildMetrics_VoucherOnlySkipsCashTile(t *testing.T) {
	got := buildMetrics(balanceData{
		AvailableBalance: 4.0,
		VoucherBalance:   4.0,
		CashBalance:      0.0,
	}, "now")
	for _, m := range got {
		if m.ID == "cash" {
			t.Errorf("cash tile present when cash_balance == 0; got %+v", m)
		}
	}
}

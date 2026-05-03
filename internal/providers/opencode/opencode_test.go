package opencode

import (
	"testing"
	"time"
)

// TestParseBlackResponse_ActiveUsage covers the populated subscription
// shape the existing provider always handled — rolling + weekly
// usagePercent + resetInSec from the Black plan's subscription.get.
func TestParseBlackResponse_ActiveUsage(t *testing.T) {
	now := time.Now().UTC()
	text := `{"rollingUsage":{"usagePercent":42.5,"resetInSec":1800,"status":"ok"},"weeklyUsage":{"usagePercent":18,"resetInSec":345600,"status":"ok"},"subscriptionPlan":"100"}`
	got := parseBlackResponse(text, now)
	if !got.HasSubscription {
		t.Fatal("HasSubscription = false, want true")
	}
	if got.RollingUsagePercent != 42.5 {
		t.Errorf("RollingUsagePercent = %v, want 42.5", got.RollingUsagePercent)
	}
	if got.WeeklyUsagePercent != 18 {
		t.Errorf("WeeklyUsagePercent = %v, want 18", got.WeeklyUsagePercent)
	}
	if got.RollingResetInSec != 1800 {
		t.Errorf("RollingResetInSec = %v, want 1800", got.RollingResetInSec)
	}
	if got.WeeklyResetInSec != 345600 {
		t.Errorf("WeeklyResetInSec = %v, want 345600", got.WeeklyResetInSec)
	}
	if got.Plan != "100" {
		t.Errorf("Plan = %q, want %q", got.Plan, "100")
	}
}

// TestParseBlackResponse_EmptyWorkspace_KeysShape verifies the
// /usage:[] /keys:[] shape — a workspace that exists but has no Black
// subscription — degrades to HasSubscription=false rather than
// erroring.
func TestParseBlackResponse_EmptyWorkspace_KeysShape(t *testing.T) {
	now := time.Now().UTC()
	text := `;0x000000d7;
((self.$R = self.$R || {})["server-fn:0"] = [],
($R => $R[0] = {
    usage: $R[1] = [],
    keys: $R[2] = [$R[3] = {id: "key_X", displayName: "x@y.z", deleted: !1}]
})($R["server-fn:0"]))`
	got := parseBlackResponse(text, now)
	if got.HasSubscription {
		t.Fatal("HasSubscription = true, want false for empty-workspace shape")
	}
	if got.RollingUsagePercent != 0 || got.WeeklyUsagePercent != 0 {
		t.Errorf("expected zero percents, got rolling=%v weekly=%v",
			got.RollingUsagePercent, got.WeeklyUsagePercent)
	}
}

// TestParseBlackResponse_NullUsageFields covers the schema-keys-present
// but values-null shape we believe applies to unsubscribed accounts —
// no usagePercent anywhere.
func TestParseBlackResponse_NullUsageFields(t *testing.T) {
	now := time.Now().UTC()
	text := `;0x000000aa;
((self.$R = self.$R || {})["server-fn:3"] = [],
($R => $R[0] = {
    rollingUsage: null,
    weeklyUsage: null
})($R["server-fn:3"]))`
	got := parseBlackResponse(text, now)
	if got.HasSubscription {
		t.Fatal("HasSubscription = true, want false for null-usage shape")
	}
}

// TestParseBlackResponse_NoSubscription_Minified is the same null
// shape but minified the way OpenCode emits compressed Solid SSR.
func TestParseBlackResponse_NoSubscription_Minified(t *testing.T) {
	now := time.Now().UTC()
	text := `;0x000001f9;((self.$R=self.$R||{})["server-fn:6"]=[],($R=>$R[0]={customerID:null,balance:0,monthlyLimit:null,monthlyUsage:null,subscription:null,subscriptionID:null,subscriptionPlan:null})($R["server-fn:6"]))`
	got := parseBlackResponse(text, now)
	if got.HasSubscription {
		t.Fatal("HasSubscription = true, want false")
	}
}

// TestParseBlackResponse_NullPayload_SolidWrapped covers a totally
// null payload Solid wraps in its SSR scaffolding.
func TestParseBlackResponse_NullPayload_SolidWrapped(t *testing.T) {
	now := time.Now().UTC()
	text := `;0x00000051;((self.$R=self.$R||{})["server-fn:00000000-0000-4000-8000-000000000000"]=[],null)`
	got := parseBlackResponse(text, now)
	if got.HasSubscription {
		t.Fatal("HasSubscription = true, want false for Solid-wrapped null")
	}
}

// TestParseLiteResponse_ActiveUsage covers the lite.subscription.get
// payload's three windows.
func TestParseLiteResponse_ActiveUsage(t *testing.T) {
	now := time.Now().UTC()
	text := `{"rollingUsage":{"usagePercent":35,"resetInSec":3600,"status":"ok"},"weeklyUsage":{"usagePercent":12,"resetInSec":172800,"status":"ok"},"monthlyUsage":{"usagePercent":4,"resetInSec":2500000,"status":"ok"}}`
	got := parseLiteResponse(text, now)
	if !got.HasSubscription {
		t.Fatal("HasSubscription = false, want true")
	}
	if got.RollingUsagePercent != 35 {
		t.Errorf("RollingUsagePercent = %v, want 35", got.RollingUsagePercent)
	}
	if got.WeeklyUsagePercent != 12 {
		t.Errorf("WeeklyUsagePercent = %v, want 12", got.WeeklyUsagePercent)
	}
	if got.MonthlyUsagePercent != 4 {
		t.Errorf("MonthlyUsagePercent = %v, want 4", got.MonthlyUsagePercent)
	}
}

// TestParseLiteResponse_NoSubscription verifies a null lite payload
// degrades to HasSubscription=false (the Lite-specific empty state per
// the plan: workspace exists but has no liteSubscriptionID).
func TestParseLiteResponse_NoSubscription(t *testing.T) {
	now := time.Now().UTC()
	text := `;0x00000051;((self.$R=self.$R||{})["server-fn:0"]=[],null)`
	got := parseLiteResponse(text, now)
	if got.HasSubscription {
		t.Fatal("HasSubscription = true, want false")
	}
}

// TestParseBillingResponse_PopulatedJSON covers a typical billing.get
// response with balance, monthly limit/usage, auto-reload, and last4.
// Units per console/core/src/billing.ts: balance is micro-cents,
// monthlyLimit is cents, monthlyUsage is micro-cents.
func TestParseBillingResponse_PopulatedJSON(t *testing.T) {
	now := time.Now().UTC()
	// $42.50 balance = 4_250_000 micro-cents.
	// $100 monthly limit = 10_000 cents.
	// $7.25 month-to-date = 725_000 micro-cents.
	// reloadTrigger $10 = 1000 cents; reloadAmount $25 = 2500 cents.
	text := `{
		"balance": 4250000,
		"monthlyLimit": 10000,
		"monthlyUsage": 725000,
		"reload": true,
		"reloadTrigger": 1000,
		"reloadAmount": 2500,
		"reloadError": null,
		"paymentMethodLast4": "4242",
		"subscriptionPlan": "100"
	}`
	got := parseBillingResponse(text, now)
	if !got.HasBilling {
		t.Fatal("HasBilling = false, want true")
	}
	if got.BalanceUSD != 4.25 {
		t.Errorf("BalanceUSD = %v, want 4.25", got.BalanceUSD)
	}
	if got.MonthlyLimitUSD != 100 {
		t.Errorf("MonthlyLimitUSD = %v, want 100", got.MonthlyLimitUSD)
	}
	if got.MonthlyUsageUSD != 0.725 {
		t.Errorf("MonthlyUsageUSD = %v, want 0.725", got.MonthlyUsageUSD)
	}
	if !got.AutoReloadOn {
		t.Error("AutoReloadOn = false, want true")
	}
	if got.ReloadTriggerUSD != 10 {
		t.Errorf("ReloadTriggerUSD = %v, want 10", got.ReloadTriggerUSD)
	}
	if got.ReloadAmountUSD != 25 {
		t.Errorf("ReloadAmountUSD = %v, want 25", got.ReloadAmountUSD)
	}
	if got.PaymentLast4 != "4242" {
		t.Errorf("PaymentLast4 = %q, want %q", got.PaymentLast4, "4242")
	}
	if got.SubscriptionPlan != "100" {
		t.Errorf("SubscriptionPlan = %q, want %q", got.SubscriptionPlan, "100")
	}
}

// TestParseBillingResponse_NullsDegrade verifies a billing payload with
// every field null returns HasBilling=false rather than reading every
// metric as zero.
func TestParseBillingResponse_NullsDegrade(t *testing.T) {
	now := time.Now().UTC()
	text := `{
		"balance": null,
		"monthlyLimit": null,
		"monthlyUsage": null,
		"reload": false,
		"paymentMethodLast4": null,
		"subscriptionPlan": null
	}`
	got := parseBillingResponse(text, now)
	if got.HasBilling {
		t.Fatal("HasBilling = true, want false for all-null shape")
	}
}

// TestParseBillingResponse_MinifiedSolid covers the same balance fields
// embedded in a minified Solid SSR wrapper — the regex fallback path.
func TestParseBillingResponse_MinifiedSolid(t *testing.T) {
	now := time.Now().UTC()
	text := `;0x000001f9;((self.$R=self.$R||{})["server-fn:6"]=[],($R=>$R[0]={customerID:"cus_x",balance:1500000,monthlyLimit:5000,monthlyUsage:200000,reload:true,reloadTrigger:500,reloadAmount:1000,reloadError:null,paymentMethodLast4:"1234",subscriptionPlan:"20"})($R["server-fn:6"]))`
	got := parseBillingResponse(text, now)
	if !got.HasBilling {
		t.Fatal("HasBilling = false, want true for minified-Solid populated payload")
	}
	if got.BalanceUSD != 1.5 {
		t.Errorf("BalanceUSD = %v, want 1.5", got.BalanceUSD)
	}
	if got.MonthlyLimitUSD != 50 {
		t.Errorf("MonthlyLimitUSD = %v, want 50", got.MonthlyLimitUSD)
	}
	if got.PaymentLast4 != "1234" {
		t.Errorf("PaymentLast4 = %q, want %q", got.PaymentLast4, "1234")
	}
}

// TestAssembleSnapshot_BlackOnly covers the user-on-Black path: only
// black-* metrics emit, go-* and billing-* are absent.
func TestAssembleSnapshot_BlackOnly(t *testing.T) {
	now := time.Now().UTC()
	black := blackSnapshot{
		HasSubscription:     true,
		Plan:                "100",
		RollingUsagePercent: 30,
		WeeklyUsagePercent:  10,
		RollingResetInSec:   1800,
		WeeklyResetInSec:    300_000,
		UpdatedAt:           now,
	}
	snap := assembleSnapshot(black, liteSnapshot{UpdatedAt: now}, billingSnapshot{UpdatedAt: now}, now)
	if snap.ProviderID != "opencode" {
		t.Errorf("ProviderID = %q, want opencode", snap.ProviderID)
	}
	wantIDs := map[string]bool{
		"black-rolling-percent": true,
		"black-weekly-percent":  true,
		"black-rolling-status":  true,
		"black-weekly-status":   true,
		"black-plan":            true,
	}
	for _, m := range snap.Metrics {
		if _, ok := wantIDs[m.ID]; !ok {
			t.Errorf("unexpected metric in black-only snapshot: %s", m.ID)
		}
		delete(wantIDs, m.ID)
	}
	if len(wantIDs) != 0 {
		t.Errorf("missing black metrics: %v", wantIDs)
	}
}

// TestAssembleSnapshot_GoOnly covers the user-on-Lite path.
func TestAssembleSnapshot_GoOnly(t *testing.T) {
	now := time.Now().UTC()
	lite := liteSnapshot{
		HasSubscription:     true,
		RollingUsagePercent: 20,
		WeeklyUsagePercent:  8,
		MonthlyUsagePercent: 3,
		UpdatedAt:           now,
	}
	snap := assembleSnapshot(blackSnapshot{UpdatedAt: now}, lite, billingSnapshot{UpdatedAt: now}, now)
	wantIDs := map[string]bool{
		"go-rolling-percent": true,
		"go-weekly-percent":  true,
		"go-monthly-percent": true,
		"go-rolling-status":  true,
		"go-weekly-status":   true,
		"go-monthly-status":  true,
	}
	for _, m := range snap.Metrics {
		if _, ok := wantIDs[m.ID]; !ok {
			t.Errorf("unexpected metric in go-only snapshot: %s", m.ID)
		}
		delete(wantIDs, m.ID)
	}
	if len(wantIDs) != 0 {
		t.Errorf("missing go metrics: %v", wantIDs)
	}
}

// TestAssembleSnapshot_BillingDerivesMonthlyPercent verifies the
// billing-monthly-percent metric is computed from limit + usage when
// both are present.
func TestAssembleSnapshot_BillingDerivesMonthlyPercent(t *testing.T) {
	now := time.Now().UTC()
	billing := billingSnapshot{
		HasBilling:      true,
		BalanceUSD:      10,
		HasMonthlyLimit: true,
		HasMonthlyUsage: true,
		MonthlyLimitUSD: 100,
		MonthlyUsageUSD: 25,
		UpdatedAt:       now,
	}
	snap := assembleSnapshot(blackSnapshot{UpdatedAt: now}, liteSnapshot{UpdatedAt: now}, billing, now)
	var found bool
	for _, m := range snap.Metrics {
		if m.ID != "billing-monthly-percent" {
			continue
		}
		found = true
		// PercentRemainingMetric stores remaining=75 (100-25 used).
		if m.NumericValue == nil {
			t.Fatal("billing-monthly-percent NumericValue is nil")
		}
		if *m.NumericValue != 75 {
			t.Errorf("billing-monthly-percent remaining = %v, want 75", *m.NumericValue)
		}
	}
	if !found {
		t.Error("billing-monthly-percent metric not present despite limit + usage data")
	}
}

// TestAssembleSnapshot_NothingActive verifies the plain Free /
// no-data path: no lanes populated, the snapshot is empty (operational,
// no metrics) — the user sees "no data" caption per metric.
func TestAssembleSnapshot_NothingActive(t *testing.T) {
	now := time.Now().UTC()
	snap := assembleSnapshot(
		blackSnapshot{UpdatedAt: now},
		liteSnapshot{UpdatedAt: now},
		billingSnapshot{UpdatedAt: now},
		now,
	)
	if len(snap.Metrics) != 0 {
		t.Errorf("expected 0 metrics for empty snapshot, got %d", len(snap.Metrics))
	}
	if snap.Status != "operational" {
		t.Errorf("Status = %q, want operational", snap.Status)
	}
}

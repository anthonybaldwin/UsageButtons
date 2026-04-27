package perplexity

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
)

func mustParse(t *testing.T, body string) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("test fixture invalid: %v", err)
	}
	return out
}

func mustParseAny(t *testing.T, body string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("test fixture invalid: %v", err)
	}
	return out
}

func TestFirstGroupID_StandardEnvelope(t *testing.T) {
	root := mustParseAny(t, `{"groups":[{"id":"g_111","name":"x"},{"id":"g_222"}]}`)
	if got := firstGroupID(root); got != "g_111" {
		t.Errorf("expected g_111, got %q", got)
	}
}

func TestFirstGroupID_OrgsEnvelope(t *testing.T) {
	// openusage's primary envelope key.
	root := mustParseAny(t, `{"orgs":[{"api_org_id":"org_xyz","name":"Default"}]}`)
	if got := firstGroupID(root); got != "org_xyz" {
		t.Errorf("expected org_xyz, got %q", got)
	}
}

func TestFirstGroupID_DataWrapped(t *testing.T) {
	root := mustParseAny(t, `{"data":{"groups":[{"group_id":"g_abc"}]}}`)
	if got := firstGroupID(root); got != "g_abc" {
		t.Errorf("expected g_abc, got %q", got)
	}
}

func TestFirstGroupID_AltKey(t *testing.T) {
	root := mustParseAny(t, `{"items":[{"uuid":"u_1"}]}`)
	if got := firstGroupID(root); got != "u_1" {
		t.Errorf("expected u_1, got %q", got)
	}
}

func TestFirstGroupID_RootArray(t *testing.T) {
	// Some APIs return a plain top-level array.
	root := mustParseAny(t, `[{"orgId":"o_42"}]`)
	if got := firstGroupID(root); got != "o_42" {
		t.Errorf("expected o_42, got %q", got)
	}
}

func TestFirstGroupID_PrefersDefaultOrg(t *testing.T) {
	// Two orgs; the second is flagged is_default_org → must win even
	// though the first appears earlier in the array.
	root := mustParseAny(t, `{"orgs":[
		{"api_org_id":"org_first","is_default_org":false},
		{"api_org_id":"org_default","is_default_org":true},
		{"api_org_id":"org_third"}
	]}`)
	if got := firstGroupID(root); got != "org_default" {
		t.Errorf("expected org_default, got %q", got)
	}
}

func TestFirstGroupID_PrefersDefaultOrg_CamelCase(t *testing.T) {
	root := mustParseAny(t, `{"orgs":[
		{"apiOrgId":"o_a"},
		{"apiOrgId":"o_b","isDefaultOrg":true}
	]}`)
	if got := firstGroupID(root); got != "o_b" {
		t.Errorf("expected o_b, got %q", got)
	}
}

func TestFirstGroupID_SingleObjectResponse(t *testing.T) {
	// No array envelope — root object is the org itself.
	root := mustParseAny(t, `{"api_org_id":"org_solo","name":"Personal"}`)
	if got := firstGroupID(root); got != "org_solo" {
		t.Errorf("expected org_solo, got %q", got)
	}
}

func TestFirstGroupID_None(t *testing.T) {
	root := mustParseAny(t, `{"groups":[]}`)
	if got := firstGroupID(root); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestFirstGroupID_FieldPrecedence(t *testing.T) {
	// api_org_id wins over id when both present.
	root := mustParseAny(t, `{"orgs":[{"api_org_id":"primary","id":"secondary"}]}`)
	if got := firstGroupID(root); got != "primary" {
		t.Errorf("expected primary, got %q", got)
	}
}

func TestReadBalanceCents_FlatBalanceUsd(t *testing.T) {
	root := mustParse(t, `{"balance_usd":12.34}`)
	if got := readBalanceCents(root); got != 1234 {
		t.Errorf("expected 1234 cents, got %v", got)
	}
}

func TestReadBalanceCents_NestedApiOrganization(t *testing.T) {
	root := mustParse(t, `{"apiOrganization":{"balanceUsd":7.5}}`)
	if got := readBalanceCents(root); got != 750 {
		t.Errorf("expected 750 cents, got %v", got)
	}
}

func TestReadBalanceCents_CustomerInfo(t *testing.T) {
	root := mustParse(t, `{"customerInfo":{"balance":3.21}}`)
	if got := readBalanceCents(root); got != 321 {
		t.Errorf("expected 321 cents, got %v", got)
	}
}

func TestReadBalanceCents_NoMatch(t *testing.T) {
	root := mustParse(t, `{"unknown":{"foo":1}}`)
	if got := readBalanceCents(root); got != 0 {
		t.Errorf("expected 0 cents, got %v", got)
	}
}

func TestReadSubscriptionTier(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"subscriptionTier":"Pro"}`, "Pro"},
		{`{"apiOrganization":{"tier":"Max"}}`, "Max"},
		{`{"organization":{"plan":"Enterprise"}}`, "Enterprise"},
		{`{"unrelated":1}`, ""},
	}
	for _, tc := range cases {
		got := readSubscriptionTier(mustParse(t, tc.body))
		if got != tc.want {
			t.Errorf("body=%s: tier=%q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestReadRemainingCount_Root(t *testing.T) {
	root := mustParse(t, `{"remaining_pro":200}`)
	got := readRemainingCount(root, "remaining_pro")
	if got == nil || *got != 200 {
		t.Errorf("expected 200, got %v", got)
	}
}

func TestReadRemainingCount_NestedEnvelope(t *testing.T) {
	root := mustParse(t, `{"rateLimits":{"remaining_research":20}}`)
	got := readRemainingCount(root, "remaining_research")
	if got == nil || *got != 20 {
		t.Errorf("expected 20, got %v", got)
	}
}

func TestReadRemainingCount_AbsentField(t *testing.T) {
	root := mustParse(t, `{"remaining_pro":1}`)
	if got := readRemainingCount(root, "remaining_research"); got != nil {
		t.Errorf("expected nil, got %d", *got)
	}
}

func TestReadFreeQueriesAvailable(t *testing.T) {
	root := mustParse(t, `{"free_queries":{"available":true}}`)
	if !readFreeQueriesAvailable(root) {
		t.Error("expected true")
	}
	root2 := mustParse(t, `{"free_queries":{"available":false}}`)
	if readFreeQueriesAvailable(root2) {
		t.Error("expected false")
	}
	root3 := mustParse(t, `{}`)
	if readFreeQueriesAvailable(root3) {
		t.Error("expected false on missing")
	}
}

func TestReadSpendCents(t *testing.T) {
	cases := []struct {
		body string
		want float64
	}{
		{`{"customerInfo":{"spend":{"total_spend":1.23}}}`, 123},
		{`{"customer_info":{"spend":{"total_spend":4.56}}}`, 456},
		{`{"apiOrganization":{"customerInfo":{"spend":{"total_spend":7.89}}}}`, 789},
		{`{}`, 0},
	}
	for _, tc := range cases {
		got := readSpendCents(mustParse(t, tc.body))
		if got != tc.want {
			t.Errorf("body=%s: got %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestUsageFromResponses_RealUserShape(t *testing.T) {
	// Captured from a Pro plan account with zero usage. The flat
	// /rest/rate-limit/all shape (no `rateLimits` envelope) is what
	// the API actually returns.
	groupBody := `{
		"apiOrganization":{"api_org_id":"45882203","is_default_org":true},
		"customerInfo":{"is_pro":true,"balance":0.0,"pending_balance":0.0,"spend":{"total_spend":0.0}}
	}`
	rateBody := `{
		"free_queries":{"available":true,"remaining_detail":{"kind":"not_provided"}},
		"remaining_pro":200,
		"remaining_research":20,
		"remaining_labs":25,
		"remaining_agentic_research":2
	}`
	analytics := mustParseAny(t, `[{"meter_event_summaries":[]}]`)
	now := time.Date(2026, 4, 26, 17, 0, 0, 0, time.UTC)
	usage := usageFromResponses(mustParse(t, groupBody), mustParse(t, rateBody), analytics, now)
	if usage.BalanceCents != 0 {
		t.Errorf("balance: got %v", usage.BalanceCents)
	}
	if usage.SpendCents != 0 {
		t.Errorf("spend: got %v", usage.SpendCents)
	}
	if usage.SubscriptionTier != "Pro" {
		t.Errorf("tier: got %q, want Pro", usage.SubscriptionTier)
	}
	if !usage.FreeQueriesAvailable {
		t.Error("expected free_queries.available=true")
	}
	if usage.ProRemaining == nil || *usage.ProRemaining != 200 {
		t.Errorf("pro: got %v", usage.ProRemaining)
	}
	if usage.ResearchRemain == nil || *usage.ResearchRemain != 20 {
		t.Errorf("research: got %v", usage.ResearchRemain)
	}
	if usage.LabsRemain == nil || *usage.LabsRemain != 25 {
		t.Errorf("labs: got %v", usage.LabsRemain)
	}
	if usage.AgenticRemain == nil || *usage.AgenticRemain != 2 {
		t.Errorf("agentic: got %v", usage.AgenticRemain)
	}
}

func TestUsageFromResponses_WithSpend(t *testing.T) {
	groupBody := `{"apiOrganization":{"customerInfo":{"is_pro":true,"balance":2.25,"spend":{"total_spend":0.10}}}}`
	rateBody := `{"remaining_pro":300}`
	analytics := mustParseAny(t, `[
		{"meter_event_summaries":[{"cost":1.50},{"cost":0.25}]},
		{"meter_event_summaries":[{"cost":0.50}]}
	]`)
	now := time.Date(2026, 4, 26, 17, 0, 0, 0, time.UTC)
	usage := usageFromResponses(mustParse(t, groupBody), mustParse(t, rateBody), analytics, now)
	// usage-analytics overrides customerInfo.spend (more granular source).
	if usage.SpendCents != 225 {
		t.Errorf("spend: got %v, want 225 cents", usage.SpendCents)
	}
	if usage.BalanceCents != 225 {
		t.Errorf("balance: got %v, want 225 cents", usage.BalanceCents)
	}
}

func TestSumUsageCostCents_EmptyArrayCountsAsValid(t *testing.T) {
	root := mustParseAny(t, `[]`)
	c, ok := sumUsageCostCents(root)
	if !ok {
		t.Fatal("expected ok=true for empty meter array (zero usage so far)")
	}
	if c != 0 {
		t.Errorf("expected 0 cents, got %v", c)
	}
}

func TestSumUsageCostCents_UnrecognizedShape(t *testing.T) {
	root := mustParseAny(t, `{"unrelated":42}`)
	if _, ok := sumUsageCostCents(root); ok {
		t.Error("expected ok=false for unknown shape")
	}
}

func TestSumUsageCostCents_CamelCaseSummaries(t *testing.T) {
	root := mustParseAny(t, `[{"meterEventSummaries":[{"cost":3}]}]`)
	c, ok := sumUsageCostCents(root)
	if !ok || c != 300 {
		t.Errorf("expected 300 cents, got %v ok=%v", c, ok)
	}
}

func TestSumUsageCostCents_EnvelopeWrapped(t *testing.T) {
	root := mustParseAny(t, `{"data":[{"meter_event_summaries":[{"cost":1.10}]}]}`)
	c, ok := sumUsageCostCents(root)
	if !ok || c != 110 {
		t.Errorf("expected 110 cents, got %v ok=%v", c, ok)
	}
}

func TestReadSubscriptionTier_IsProBoolean(t *testing.T) {
	root := mustParse(t, `{"customerInfo":{"is_pro":true}}`)
	if got := readSubscriptionTier(root); got != "Pro" {
		t.Errorf("expected Pro, got %q", got)
	}
}

func TestReadSubscriptionTier_IsMaxBoolean(t *testing.T) {
	root := mustParse(t, `{"customerInfo":{"is_max":true,"is_pro":true}}`)
	if got := readSubscriptionTier(root); got != "Max" {
		t.Errorf("expected Max (is_max wins), got %q", got)
	}
}

func TestSumMeterCostCents_CometOnly(t *testing.T) {
	root := mustParseAny(t, `[
		{"name":"input_tokens","meter_event_summaries":[{"cost":1.0}]},
		{"name":"comet_cloud_duration_hours","meter_event_summaries":[{"cost":2.50},{"cost":0.75}]},
		{"name":"output_tokens","meter_event_summaries":[{"cost":3.0}]}
	]`)
	got := sumMeterCostCents(root, "comet_cloud_duration_hours")
	if got != 325 {
		t.Errorf("expected 325 cents (2.50 + 0.75), got %v", got)
	}
}

func TestSumMeterCostCents_NoMatch(t *testing.T) {
	root := mustParseAny(t, `[{"name":"input_tokens","meter_event_summaries":[{"cost":1.0}]}]`)
	got := sumMeterCostCents(root, "comet_cloud_duration_hours")
	if got != 0 {
		t.Errorf("expected 0, got %v", got)
	}
}

func TestSnapshotFromUsage_FreshProAccount(t *testing.T) {
	// All counts present, no spend/balance. Should produce 7 metrics:
	// 4 count + comet-spend + balance + spend (dollars always emitted).
	pro, research, labs, agentic := 200, 20, 25, 2
	usage := usageSnapshot{
		SubscriptionTier:     "Pro",
		FreeQueriesAvailable: true,
		ProRemaining:         &pro,
		ResearchRemain:       &research,
		LabsRemain:           &labs,
		AgenticRemain:        &agentic,
		UpdatedAt:            time.Now(),
	}
	snap := snapshotFromUsage(usage)
	if len(snap.Metrics) != 7 {
		t.Fatalf("expected 7 metrics, got %d: %+v", len(snap.Metrics), snap.Metrics)
	}
	want := []string{
		"pro-queries-remaining",
		"deep-research-remaining",
		"labs-remaining",
		"agentic-research-remaining",
		"comet-spend",
		"api-balance",
		"api-spend",
	}
	for i, w := range want {
		if snap.Metrics[i].ID != w {
			t.Errorf("metric[%d]: got %q, want %q", i, snap.Metrics[i].ID, w)
		}
	}
	// Pro count metric: Value should be the count (200), Ratio 1.0,
	// NumericUnit "count" — NOT a percent.
	pq := snap.Metrics[0]
	if pq.NumericUnit != "count" {
		t.Errorf("expected NumericUnit=count, got %q", pq.NumericUnit)
	}
	if v, ok := pq.Value.(float64); !ok || v != 200 {
		t.Errorf("expected Value=200 float64, got %v (%T)", pq.Value, pq.Value)
	}
	if pq.Ratio == nil || *pq.Ratio != 1.0 {
		t.Errorf("expected Ratio=1.0, got %v", pq.Ratio)
	}
}

func TestSnapshotFromUsage_OnlySomeRateLimits(t *testing.T) {
	// API returns only Pro; others nil. Should still emit comet/balance/spend.
	pro := 200
	usage := usageSnapshot{
		ProRemaining: &pro,
		UpdatedAt:    time.Now(),
	}
	snap := snapshotFromUsage(usage)
	// 1 pro count + comet-spend + balance + spend = 4
	if len(snap.Metrics) != 4 {
		t.Fatalf("expected 4 metrics, got %d", len(snap.Metrics))
	}
}

func TestSnapshotFromUsage_DollarMetricsAlwaysEmitted(t *testing.T) {
	// No rate limits, $0 across the board — comet/balance/spend still
	// render so the user can see they have no API platform activity.
	usage := usageSnapshot{UpdatedAt: time.Now()}
	snap := snapshotFromUsage(usage)
	if len(snap.Metrics) != 3 {
		t.Fatalf("expected 3 dollar metrics, got %d", len(snap.Metrics))
	}
	want := []string{"comet-spend", "api-balance", "api-spend"}
	for i, w := range want {
		if snap.Metrics[i].ID != w {
			t.Errorf("metric[%d]: got %s, want %s", i, snap.Metrics[i].ID, w)
		}
		if snap.Metrics[i].NumericUnit != "dollars" {
			t.Errorf("metric[%d] unit: got %s", i, snap.Metrics[i].NumericUnit)
		}
	}
}

func TestProviderName_TierMapping(t *testing.T) {
	for _, tc := range []struct {
		tier string
		want string
	}{
		{"Pro", "Perplexity Pro"},
		{"Max", "Perplexity Max"},
		{"enterprise pro", "Perplexity Enterprise"},
		{"", "Perplexity"},
		{"unknown", "Perplexity"},
	} {
		got := providerName(usageSnapshot{SubscriptionTier: tc.tier})
		if got != tc.want {
			t.Errorf("tier=%q: got %q, want %q", tc.tier, got, tc.want)
		}
	}
}

func TestMapHTTPError_Stale401(t *testing.T) {
	snap := mapHTTPError(&httputil.Error{Status: 401})
	if snap.Status != "unknown" {
		t.Errorf("expected unknown status, got %q", snap.Status)
	}
	if snap.Error == "" {
		t.Error("expected non-empty error message")
	}
}

func TestMapHTTPError_404FeatureNotAvailable(t *testing.T) {
	snap := mapHTTPError(&httputil.Error{
		Status: 404,
		Body:   `{"detail":{"error_code":"feature_not_available","message":"Feature not available"}}`,
	})
	if !strings.Contains(snap.Error, "Perplexity usage API not available") {
		t.Errorf("expected feature-not-available message, got %q", snap.Error)
	}
}

func TestMapHTTPError_GenericNonHTTPDoesNotLeakBody(t *testing.T) {
	snap := mapHTTPError(&httputil.Error{
		Status: 500,
		Body:   `<html><body>internal goo with secret=abc</body></html>`,
		URL:    "https://www.perplexity.ai/rest/...",
	})
	if strings.Contains(snap.Error, "secret=abc") {
		t.Errorf("body leaked into user-visible error: %q", snap.Error)
	}
	if !strings.Contains(snap.Error, "HTTP 500") {
		t.Errorf("expected short HTTP code message, got %q", snap.Error)
	}
}

func TestMapHTTPError_NonHTTPError(t *testing.T) {
	snap := mapHTTPError(errors.New("dial tcp: timeout"))
	if !strings.Contains(snap.Error, "dial tcp") {
		t.Errorf("expected raw network error preserved, got %q", snap.Error)
	}
}

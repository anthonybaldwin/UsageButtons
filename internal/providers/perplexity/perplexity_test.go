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

func TestFirstGroupID_StandardEnvelope(t *testing.T) {
	root := mustParse(t, `{"groups":[{"id":"g_111","name":"x"},{"id":"g_222"}]}`)
	if got := firstGroupID(root); got != "g_111" {
		t.Errorf("expected g_111, got %q", got)
	}
}

func TestFirstGroupID_DataWrapped(t *testing.T) {
	root := mustParse(t, `{"data":{"groups":[{"group_id":"g_abc"}]}}`)
	if got := firstGroupID(root); got != "g_abc" {
		t.Errorf("expected g_abc, got %q", got)
	}
}

func TestFirstGroupID_AltKey(t *testing.T) {
	root := mustParse(t, `{"items":[{"uuid":"u_1"}]}`)
	if got := firstGroupID(root); got != "u_1" {
		t.Errorf("expected u_1, got %q", got)
	}
}

func TestFirstGroupID_None(t *testing.T) {
	root := mustParse(t, `{"groups":[]}`)
	if got := firstGroupID(root); got != "" {
		t.Errorf("expected empty, got %q", got)
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

func TestReadRateLimit_Pro(t *testing.T) {
	root := mustParse(t, `{"rateLimits":{"remaining_pro":545}}`)
	rem, limit := readRateLimit(root, "remaining_pro")
	if rem == nil || *rem != 545 {
		t.Errorf("expected remaining=545, got %v", rem)
	}
	if limit != rateLimitDailyDefault {
		t.Errorf("expected default limit %d, got %d", rateLimitDailyDefault, limit)
	}
}

func TestReadRateLimit_ExplicitLimit(t *testing.T) {
	root := mustParse(t, `{"rateLimits":{"remaining_research":4,"limit_remaining_research":10}}`)
	rem, limit := readRateLimit(root, "remaining_research")
	if rem == nil || *rem != 4 {
		t.Errorf("expected remaining=4, got %v", rem)
	}
	if limit != 10 {
		t.Errorf("expected limit=10, got %d", limit)
	}
}

func TestReadRateLimit_AbsentField(t *testing.T) {
	root := mustParse(t, `{"rateLimits":{"remaining_pro":1}}`)
	rem, limit := readRateLimit(root, "remaining_research")
	if rem != nil {
		t.Errorf("expected nil remaining for absent field, got %v", *rem)
	}
	if limit != 0 {
		t.Errorf("expected 0 limit for absent field, got %d", limit)
	}
}

func TestUsageFromResponses_FullShape(t *testing.T) {
	groupBody := `{"apiOrganization":{"balance_usd":42.5,"tier":"pro"}}`
	rateBody := `{"rateLimits":{"remaining_pro":555,"remaining_research":3,"remaining_labs":12,"remaining_agentic_research":2}}`
	now := time.Date(2026, 4, 26, 17, 0, 0, 0, time.UTC)
	usage := usageFromResponses(mustParse(t, groupBody), mustParse(t, rateBody), now)
	if usage.BalanceCents != 4250 {
		t.Errorf("balance: got %v, want 4250", usage.BalanceCents)
	}
	if usage.SubscriptionTier != "pro" {
		t.Errorf("tier: got %q, want pro", usage.SubscriptionTier)
	}
	if usage.ProRemaining == nil || *usage.ProRemaining != 555 {
		t.Errorf("pro: got %v", usage.ProRemaining)
	}
	if usage.ResearchRemain == nil || *usage.ResearchRemain != 3 {
		t.Errorf("research: got %v", usage.ResearchRemain)
	}
	if usage.LabsRemain == nil || *usage.LabsRemain != 12 {
		t.Errorf("labs: got %v", usage.LabsRemain)
	}
	if usage.AgenticRemain == nil || *usage.AgenticRemain != 2 {
		t.Errorf("agentic: got %v", usage.AgenticRemain)
	}
}

func TestSnapshotFromUsage_BalanceFallbackWhenNoRateLimits(t *testing.T) {
	usage := usageSnapshot{BalanceCents: 1500, UpdatedAt: time.Now()}
	snap := snapshotFromUsage(usage)
	if len(snap.Metrics) != 1 {
		t.Fatalf("expected 1 metric (balance), got %d", len(snap.Metrics))
	}
	if snap.Metrics[0].ID != "balance" {
		t.Errorf("expected balance metric, got %s", snap.Metrics[0].ID)
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

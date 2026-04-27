package hermes

import (
	"strings"
	"testing"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
)

// productsFixture is a sanitized excerpt of the script-tag-embedded JSON
// the Nous portal renders into /products. Numeric values match the real
// portal shape (Plus plan: $22 monthly, $10 rollover cap), but identifiers,
// Stripe URLs, and per-user IDs are scrubbed to placeholders.
const productsFixture = `<html><body><script>
self.__next_f.push([1, "20:[\"$\",\"section\",null,{\"className\":\"x\",\"children\":[\"$\",\"$L22\",null,{\"availableTiers\":[{\"id\":\"tier_free\",\"name\":\"Free\",\"tier\":5,\"monthlyCredits\":0.1,\"maxRolloverCredits\":0,\"dollarsPerMonth\":0,\"enabled\":true},{\"id\":\"tier_plus\",\"name\":\"Plus\",\"tier\":2,\"monthlyCredits\":22,\"maxRolloverCredits\":10,\"dollarsPerMonth\":20,\"enabled\":true},{\"id\":\"tier_super\",\"name\":\"Super\",\"tier\":4,\"monthlyCredits\":110,\"maxRolloverCredits\":50,\"dollarsPerMonth\":100,\"enabled\":true}],\"activeSubscription\":{\"id\":\"sub_redacted\",\"subscriptionTypeId\":\"tier_plus\",\"tier\":\"Plus\",\"tierLevel\":2,\"dollarsPerMonth\":20,\"monthlyCredits\":22,\"maxRolloverCredits\":10,\"currentPeriodStart\":\"2026-04-26T22:22:18.060Z\",\"currentPeriodEnd\":\"2026-05-26T22:22:18.060Z\",\"expiresAt\":\"2026-05-26T22:22:18.060Z\",\"cancelAtPeriodEnd\":false,\"active\":true,\"pendingCancellation\":false},\"apiCreditsBalance\":0,\"subscriptionCredits\":{\"balance\":21.998392,\"rolloverCredits\":0},\"recentExpiredSubscription\":null,\"cryptoEnabled\":false}]}]"]);
</script></body></html>`

// apiKeysFixture mirrors the /api-keys page's embedded usageByKey block
// at the moment of zero-API activity (just one chat-bound usage row).
// All identifiers scrubbed.
const apiKeysFixture = `<html><body><script>
self.__next_f.push([1, "c:[\"$\",\"main\",null,{\"className\":\"x\",\"children\":[\"$\",\"$L25\",null,{\"keys\":[],\"usageByKey\":{\"timeframe\":{\"start\":\"2025-01-01T00:00:00.000Z\",\"end\":\"2026-04-27T02:10:31.465Z\",\"granularity\":\"total\"},\"series\":[{\"id\":\"key_redacted\",\"name\":\"Chat\",\"type\":\"keyId\",\"data\":{\"tokens\":[1072],\"spend\":[0.001608],\"requests\":[1]}}],\"totals\":{\"tokens\":1072,\"inputTokens\":965,\"outputTokens\":107,\"cacheReadTokens\":0,\"cacheWriteTokens\":0,\"spend\":0.001608,\"requests\":1},\"totalsByAllowanceId\":{}}}]}]"]);
</script></body></html>`

// apiKeysEmpty is the page render before any API activity: totals all zero.
const apiKeysEmpty = `<html><body><script>
self.__next_f.push([1, "c:[\"$\",\"main\",null,{\"children\":[\"$\",\"$L25\",null,{\"keys\":[],\"usageByKey\":{\"timeframe\":{\"start\":\"2025-01-01T00:00:00.000Z\",\"end\":\"2026-04-27T02:10:31.465Z\",\"granularity\":\"total\"},\"series\":[],\"totals\":{\"tokens\":0,\"inputTokens\":0,\"outputTokens\":0,\"cacheReadTokens\":0,\"cacheWriteTokens\":0,\"spend\":0,\"requests\":0},\"totalsByAllowanceId\":{}}}]}]"]);
</script></body></html>`

func TestSnapshotFromHTML_PlusTier(t *testing.T) {
	now := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	u := snapshotFromHTML([]byte(productsFixture), now)

	// Sub credits: 21.998392 → rounded to $21.998... cents (≈ 2199.84
	// cents). math.Round at the cents level is fine for small drift.
	if got := u.SubBalanceCents; got != 2200 {
		t.Errorf("SubBalanceCents: got %v, want 2200 (≈ $21.998 rounded)", got)
	}
	if u.SubRolloverCents != 0 {
		t.Errorf("SubRolloverCents: got %v, want 0", u.SubRolloverCents)
	}
	if u.SubMonthlyCents != 2200 {
		t.Errorf("SubMonthlyCents: got %v, want 2200", u.SubMonthlyCents)
	}
	if u.SubRolloverCapCents != 1000 {
		t.Errorf("SubRolloverCapCents: got %v, want 1000", u.SubRolloverCapCents)
	}
	if u.APIBalanceCents != 0 {
		t.Errorf("APIBalanceCents: got %v, want 0", u.APIBalanceCents)
	}
	if u.Tier != "Plus" {
		t.Errorf("Tier: got %q, want Plus", u.Tier)
	}
	if u.RenewsAt == nil || u.RenewsAt.Year() != 2026 || u.RenewsAt.Month() != 5 {
		t.Errorf("RenewsAt: got %v, want 2026-05-26", u.RenewsAt)
	}
}

func TestSnapshotFromHTML_DoesNotPickPerTierMonthlyCredits(t *testing.T) {
	// availableTiers contains a Super tier with monthlyCredits=110 and
	// a Free tier with 0.1. The active-subscription regex must skip
	// those and pick the Plus value (22).
	u := snapshotFromHTML([]byte(productsFixture), time.Now())
	if u.SubMonthlyCents != 2200 {
		t.Errorf("regex regression: should land on activeSubscription.monthlyCredits=22, got %v cents", u.SubMonthlyCents)
	}
}

func TestMergeAPIKeysHTML_TotalsExtracted(t *testing.T) {
	u := usageSnapshot{}
	mergeAPIKeysHTML([]byte(apiKeysFixture), &u)
	if !u.HasAPIData {
		t.Fatal("HasAPIData should be true after a successful parse")
	}
	if got := u.APISpendCents; got != 0 {
		// 0.001608 USD → 0.16 cents → rounds to 0. That's the API's
		// real granularity at sub-cent spend; we accept the loss.
		t.Errorf("APISpendCents: got %v, want 0 (rounded sub-cent)", got)
	}
	if u.APIRequests != 1 {
		t.Errorf("APIRequests: got %v, want 1", u.APIRequests)
	}
}

func TestMergeAPIKeysHTML_ZeroActivity(t *testing.T) {
	u := usageSnapshot{}
	mergeAPIKeysHTML([]byte(apiKeysEmpty), &u)
	if !u.HasAPIData {
		t.Fatal("zero-activity totals are still data; HasAPIData must be true")
	}
	if u.APISpendCents != 0 || u.APIRequests != 0 {
		t.Errorf("got non-zero from zero-activity fixture: %+v", u)
	}
}

func TestSnapshotToProvider_FullPlusAccount(t *testing.T) {
	now := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	u := snapshotFromHTML([]byte(productsFixture), now)
	mergeAPIKeysHTML([]byte(apiKeysFixture), &u)
	snap := snapshotToProvider(u)

	if snap.ProviderName != "Hermes Plus" {
		t.Errorf("ProviderName: got %q, want \"Hermes Plus\"", snap.ProviderName)
	}
	// Sub + api-balance + api-spend + api-requests = 4 metrics.
	if len(snap.Metrics) != 4 {
		t.Fatalf("expected 4 metrics, got %d: %+v", len(snap.Metrics), snap.Metrics)
	}
	want := map[string]bool{
		"hermes-sub-credits-remaining": true,
		"hermes-api-credits-remaining": true,
		"hermes-api-spend-total":       true,
		"hermes-api-requests-total":    true,
	}
	for _, m := range snap.Metrics {
		if !want[m.ID] {
			t.Errorf("unexpected metric ID %q", m.ID)
		}
	}
}

func TestSnapshotToProvider_NoAPIData_OmitsAPIMetrics(t *testing.T) {
	u := snapshotFromHTML([]byte(productsFixture), time.Now())
	// No mergeAPIKeysHTML call → HasAPIData stays false.
	// productsFixture has apiCreditsBalance=0 too, so api-balance is also
	// skipped — otherwise a $0 balance + default dollar threshold would
	// render a permanently-critical-red tile for users with no API account.
	snap := snapshotToProvider(u)
	for _, m := range snap.Metrics {
		if m.ID == "hermes-api-spend-total" || m.ID == "hermes-api-requests-total" || m.ID == "hermes-api-credits-remaining" {
			t.Errorf("api-* metric %q must not emit when /api-keys wasn't fetched and balance is $0", m.ID)
		}
	}
}

func TestSnapshotToProvider_APIBalanceEmittedWhenNonZero(t *testing.T) {
	// Pretend the user has $5 sitting in their API platform account but
	// hasn't actually used any of it (HasAPIData stays false because
	// /api-keys wasn't fetched). We still want to render the balance —
	// it tells them they have funds available even if no usage logged.
	u := snapshotFromHTML([]byte(productsFixture), time.Now())
	u.APIBalanceCents = 500
	snap := snapshotToProvider(u)
	found := false
	for _, m := range snap.Metrics {
		if m.ID == "hermes-api-credits-remaining" {
			found = true
		}
	}
	if !found {
		t.Error("api-balance must emit when balance > 0, even without /api-keys data")
	}
}

func TestSnapshotToProvider_FreeTierOmitsSubMetric(t *testing.T) {
	// SubMonthlyCents == 0 (no active paid subscription) → skip the
	// subscription tile rather than render a $0/$0 meter.
	u := usageSnapshot{UpdatedAt: time.Now()}
	snap := snapshotToProvider(u)
	for _, m := range snap.Metrics {
		if m.ID == "hermes-sub-credits-remaining" {
			t.Errorf("Free tier should not emit subscription tile, got %+v", m)
		}
	}
}

func TestProviderName_NoTier(t *testing.T) {
	if got := providerName(usageSnapshot{}); got != "Hermes" {
		t.Errorf("got %q, want plain Hermes when tier missing", got)
	}
}

func TestRenewSeconds_PastDateReturnsNil(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour)
	if renewSeconds(&past, time.Now()) != nil {
		t.Error("expired renewal should suppress the countdown")
	}
}

func TestMapHTTPError_StaleSession(t *testing.T) {
	for _, status := range []int{401, 403} {
		snap := mapHTTPError(&httputil.Error{Status: status})
		if !strings.Contains(snap.Error, "session") && !strings.Contains(snap.Error, "Sign") {
			t.Errorf("status %d: error should hint at re-auth, got %q", status, snap.Error)
		}
	}
}

func TestMapHTTPError_GenericNoBodyLeak(t *testing.T) {
	snap := mapHTTPError(&httputil.Error{Status: 502, Body: "<html>session: privy_token=secret_value</html>"})
	if strings.Contains(snap.Error, "secret_value") {
		t.Errorf("body leaked into user-visible error: %q", snap.Error)
	}
}

func TestProviderMetadata(t *testing.T) {
	p := Provider{}
	if p.ID() != "hermes" {
		t.Errorf("ID: got %q", p.ID())
	}
	if p.Name() != "Hermes" {
		t.Errorf("Name: got %q", p.Name())
	}
	want := map[string]bool{
		"hermes-sub-credits-remaining": true,
		"hermes-api-credits-remaining": true,
		"hermes-api-spend-total":       true,
		"hermes-api-requests-total":    true,
	}
	for _, id := range p.MetricIDs() {
		if !want[id] {
			t.Errorf("unexpected metric ID %q", id)
		}
	}
}

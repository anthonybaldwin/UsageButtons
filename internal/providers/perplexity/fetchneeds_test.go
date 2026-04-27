package perplexity

import "testing"

func TestFetchNeeds_NilActiveFetchesEverything(t *testing.T) {
	n := perplexityFetchNeedsFor(nil)
	if !n.group || !n.analytics || !n.rate {
		t.Errorf("nil active set must fetch everything, got %+v", n)
	}
}

func TestFetchNeeds_OnlyRateMetricsSkipGroupAndAnalytics(t *testing.T) {
	n := perplexityFetchNeedsFor([]string{"pro-queries-remaining", "labs-remaining"})
	if n.group || n.analytics {
		t.Errorf("rate-only active set should skip group + analytics, got %+v", n)
	}
	if !n.rate {
		t.Error("rate must be true when rate metrics are bound")
	}
}

func TestFetchNeeds_OnlyBalanceSkipsRateAndAnalytics(t *testing.T) {
	// api-balance lives on the per-group endpoint but doesn't need
	// usage-analytics — the balance comes straight from customerInfo.
	n := perplexityFetchNeedsFor([]string{"api-balance"})
	if !n.group {
		t.Error("api-balance requires the per-group fetch")
	}
	if n.analytics {
		t.Error("api-balance must NOT trigger usage-analytics")
	}
	if n.rate {
		t.Error("api-balance must NOT trigger /rate-limit/all")
	}
}

func TestFetchNeeds_SpendMetricsRequireAnalytics(t *testing.T) {
	for _, id := range []string{"comet-spend", "api-spend"} {
		n := perplexityFetchNeedsFor([]string{id})
		if !n.group {
			t.Errorf("%s: group should be true", id)
		}
		if !n.analytics {
			t.Errorf("%s: analytics should be true", id)
		}
		if n.rate {
			t.Errorf("%s: rate should be false (no rate metrics bound)", id)
		}
	}
}

func TestFetchNeeds_MixedSetUnionsRequirements(t *testing.T) {
	n := perplexityFetchNeedsFor([]string{"pro-queries-remaining", "comet-spend"})
	if !n.group || !n.analytics || !n.rate {
		t.Errorf("mixed set must request every endpoint reachable from any bound metric, got %+v", n)
	}
}

func TestFetchNeeds_EmptySetSkipsEverything(t *testing.T) {
	// Defensive: empty (non-nil) active set means "no buttons bound".
	// Cache layer normally won't call Fetch in that state, but if it
	// does we should skip every endpoint rather than wastefully fetch.
	n := perplexityFetchNeedsFor([]string{})
	if n.group || n.analytics || n.rate {
		t.Errorf("empty active set should skip every endpoint, got %+v", n)
	}
}

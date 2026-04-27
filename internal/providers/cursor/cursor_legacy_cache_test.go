package cursor

import "testing"

func TestLegacyCache_FreshStateProbes(t *testing.T) {
	resetLegacyPlanCache()
	if !shouldProbeLegacy() {
		t.Error("fresh cache must probe on first call")
	}
}

func TestLegacyCache_ModernAccountSkipsAfterFirstSuccess(t *testing.T) {
	resetLegacyPlanCache()
	// First poll: clean fetch returned no legacy data → account is modern.
	rememberLegacyPlan(nil, false)
	if shouldProbeLegacy() {
		t.Error("expected probe to be skipped once account is classified modern")
	}
}

func TestLegacyCache_LegacyAccountKeepsProbing(t *testing.T) {
	resetLegacyPlanCache()
	// Legacy users still get probed every poll — that endpoint is
	// what supplies their actual data.
	rememberLegacyPlan(&legacyModelUsage{}, false)
	if !shouldProbeLegacy() {
		t.Error("legacy accounts must continue probing — that's their data source")
	}
}

func TestLegacyCache_ErrorDoesNotCacheClassification(t *testing.T) {
	resetLegacyPlanCache()
	// A network error must NOT cache "modern" — we don't know yet.
	rememberLegacyPlan(nil, true)
	if !shouldProbeLegacy() {
		t.Error("transient fetch error must leave the cache unset so we re-probe")
	}
}

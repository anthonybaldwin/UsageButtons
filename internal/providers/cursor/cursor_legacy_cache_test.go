package cursor

import "testing"

func TestLegacyCache_UnknownSubFallsThrough(t *testing.T) {
	resetLegacyPlanCache()
	if isAccountKnownModern("never-seen") {
		t.Error("a sub we've never classified must not register as modern")
	}
}

func TestLegacyCache_RememberedSubSkipsUsageCall(t *testing.T) {
	resetLegacyPlanCache()
	rememberAccountModern("user-a")
	if !isAccountKnownModern("user-a") {
		t.Error("user-a was just classified modern; must read back as modern")
	}
}

func TestLegacyCache_KeyedBySub_NoCrossAccountBleed(t *testing.T) {
	// The bug Greptile flagged: process-globally caching "modern"
	// would silently drop the legacy metric when a legacy user logged
	// in on a machine that previously cached a modern user. Per-sub
	// keying makes account switches invalidate by landing on a fresh
	// cache slot.
	resetLegacyPlanCache()
	rememberAccountModern("user-modern")
	if isAccountKnownModern("user-legacy") {
		t.Error("legacy account must not inherit modern's cached classification")
	}
}

func TestLegacyCache_EmptySubIsNoOp(t *testing.T) {
	resetLegacyPlanCache()
	rememberAccountModern("")
	if isAccountKnownModern("") {
		t.Error("empty sub must not register — it would otherwise act as a wildcard")
	}
}

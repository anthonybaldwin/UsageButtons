// Active-metric registry: tracks which (provider, metric) pairs have
// at least one Stream Deck button currently bound to them, with a
// short grace window so a profile-switch (which fires
// willDisappear → willAppear in rapid succession) doesn't flap the
// fetch shape between two adjacent polls.
//
// The cache layer reads ActiveFor(providerID) and threads the result
// into FetchContext.ActiveMetricIDs so multi-endpoint providers can
// skip work whose results aren't displayed anywhere.
package providers

import (
	"sort"
	"sync"
	"time"
)

// ActiveGracePeriod is how long a (provider, metric) pair lingers in
// the active set after its MarkInactive call. Sized to absorb a
// Stream Deck profile switch (which fires willDisappear immediately
// followed by willAppear from the new profile) so the next poll's
// FetchContext.ActiveMetricIDs is identical across the switch.
//
// Independent of provider-tier MinTTL — the grace window only governs
// whether a metric stays in the active set, not when fetches happen.
var ActiveGracePeriod = 30 * time.Second

// activeRegistry is the process-wide active-metric tracker.
type activeRegistry struct {
	mu sync.Mutex
	// counts[providerID][metricID] = number of bound buttons.
	counts map[string]map[string]int
	// expiringAt[providerID][metricID] = time after which a count-zero
	// entry should be considered inactive. Stays in counts so we can
	// answer ActiveFor without an additional index walk.
	expiringAt map[string]map[string]time.Time
}

// activeReg is the process-wide singleton active-metric registry.
var activeReg = &activeRegistry{
	counts:     map[string]map[string]int{},
	expiringAt: map[string]map[string]time.Time{},
}

// MarkActive records that one bound button is now displaying
// (providerID, metricID). Safe to call repeatedly — duplicates count.
// No-op if either argument is empty.
func MarkActive(providerID, metricID string) {
	if providerID == "" || metricID == "" {
		return
	}
	activeReg.mu.Lock()
	defer activeReg.mu.Unlock()
	m, ok := activeReg.counts[providerID]
	if !ok {
		m = map[string]int{}
		activeReg.counts[providerID] = m
	}
	m[metricID]++
	// Clear any pending expiration — re-binding cancels the grace timer.
	if exp, ok := activeReg.expiringAt[providerID]; ok {
		delete(exp, metricID)
	}
}

// MarkInactive decrements the bound-button count for
// (providerID, metricID). When the count drops to zero, the entry
// enters a grace window and is removed from ActiveFor results
// ActiveGracePeriod later. No-op if either argument is empty or no
// matching MarkActive call was made.
func MarkInactive(providerID, metricID string) {
	if providerID == "" || metricID == "" {
		return
	}
	activeReg.mu.Lock()
	defer activeReg.mu.Unlock()
	m, ok := activeReg.counts[providerID]
	if !ok {
		return
	}
	if m[metricID] <= 0 {
		return
	}
	m[metricID]--
	if m[metricID] == 0 {
		exp, ok := activeReg.expiringAt[providerID]
		if !ok {
			exp = map[string]time.Time{}
			activeReg.expiringAt[providerID] = exp
		}
		exp[metricID] = time.Now().Add(ActiveGracePeriod)
	}
}

// ActiveFor returns the sorted, deduped set of metric IDs currently
// bound for the given provider — including count-zero entries still
// in their grace window. Returns nil when the provider has never had
// any bound metrics (not an empty slice — `nil` carries the explicit
// "fetch everything" semantics defined on FetchContext).
//
// Lazily evicts count-zero entries past their grace window so the
// inner maps don't accumulate stale records for long-running
// sessions where users repeatedly bind/unbind buttons.
func ActiveFor(providerID string) []string {
	if providerID == "" {
		return nil
	}
	activeReg.mu.Lock()
	defer activeReg.mu.Unlock()
	m, ok := activeReg.counts[providerID]
	if !ok || len(m) == 0 {
		return nil
	}
	now := time.Now()
	exp := activeReg.expiringAt[providerID]
	out := make([]string, 0, len(m))
	for metricID, count := range m {
		if count > 0 {
			out = append(out, metricID)
			continue
		}
		if exp != nil {
			if until, ok := exp[metricID]; ok && now.Before(until) {
				out = append(out, metricID)
				continue
			}
		}
		// Count zero AND past the grace window (or never had a
		// pending expiration) — drop both index entries so the maps
		// don't grow without bound across long sessions.
		delete(m, metricID)
		if exp != nil {
			delete(exp, metricID)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// ResetActiveRegistry clears the registry. Test-only — production
// code should never call this; the registry is process-lifetime.
func ResetActiveRegistry() {
	activeReg.mu.Lock()
	defer activeReg.mu.Unlock()
	activeReg.counts = map[string]map[string]int{}
	activeReg.expiringAt = map[string]map[string]time.Time{}
}

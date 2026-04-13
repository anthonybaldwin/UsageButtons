package providers

import (
	"fmt"
	"sync"
	"time"
)

const (
	// MinTTL matches the shortest user-selectable poll interval.
	// Any poll within this window reuses the snapshot.
	MinTTL = 5 * time.Minute

	// CooldownDuration: after an upstream error, stop hitting the API.
	CooldownDuration = 10 * time.Minute

	// ManualCooldown: minimum gap between user-initiated (force=true)
	// refreshes per provider. Prevents button-mashing from hammering
	// upstream APIs. 30s is responsive enough for deliberate retries
	// but limits a frustrated user to ~2 req/min.
	ManualCooldown = 30 * time.Second

	// StaleTTL: how long missing metrics are preserved from a previous
	// snapshot. After this window a permanently failed sub-fetch (e.g.
	// expired cookie) stops carrying forward stale data so the button
	// can show a setup/error state instead.
	StaleTTL = 30 * time.Minute
)

// LogSink is called for cache observability. Set by the plugin at init.
var LogSink func(msg string)

func cacheLog(format string, args ...any) {
	if LogSink != nil {
		LogSink(fmt.Sprintf(format, args...))
	}
}

type cacheError struct {
	message string
	at      time.Time
}

type cacheEntry struct {
	snapshot  *Snapshot
	fetchedAt time.Time
	lastError *cacheError
	lastForceAt time.Time

	// mu protects the inflight promise pattern.
	mu       sync.Mutex
	inflight chan struct{} // non-nil when a fetch is in progress
	result   *Snapshot     // set when inflight completes
	resultErr error
}

var (
	cacheMu sync.Mutex
	entries = map[string]*cacheEntry{}
)

func getEntry(providerID string) *cacheEntry {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	e, ok := entries[providerID]
	if !ok {
		e = &cacheEntry{}
		entries[providerID] = e
	}
	return e
}

// GetSnapshotOptions configures a cache lookup.
type GetSnapshotOptions struct {
	Force bool
}

// GetSnapshot returns a provider snapshot, using the cache when
// possible. Guarantees at most one in-flight fetch per provider.
func GetSnapshot(p Provider, opts GetSnapshotOptions) Snapshot {
	e := getEntry(p.ID())
	now := time.Now()

	e.mu.Lock()

	// 1. Error cooldown: serve stale or error snapshot.
	if e.lastError != nil && now.Sub(e.lastError.at) < CooldownDuration {
		left := CooldownDuration - now.Sub(e.lastError.at)
		cacheLog("cache[%s] cool-down: %ds left, serving %s (last error: %s)",
			p.ID(), int(left.Seconds()),
			boolStr(e.snapshot != nil, "stale snapshot", "error face"),
			e.lastError.message)
		if e.snapshot != nil {
			s := markStale(*e.snapshot, e.lastError.message)
			e.mu.Unlock()
			return s
		}
		e.mu.Unlock()
		return errorSnapshot(p, e.lastError.message)
	}

	// 2. Manual refresh throttle.
	if opts.Force && !e.lastForceAt.IsZero() &&
		now.Sub(e.lastForceAt) < ManualCooldown && e.snapshot != nil {
		left := ManualCooldown - now.Sub(e.lastForceAt)
		cacheLog("cache[%s] manual refresh throttled: %ds until next allowed",
			p.ID(), int(left.Seconds()))
		s := *e.snapshot
		e.mu.Unlock()
		return s
	}

	// 3. Coalesce: if a fetch is already in-flight, wait for it.
	if e.inflight != nil {
		ch := e.inflight
		cacheLog("cache[%s] coalesced with in-flight fetch", p.ID())
		e.mu.Unlock()
		<-ch // wait for the in-flight fetch to complete
		e.mu.Lock()
		s := *e.result
		e.mu.Unlock()
		return s
	}

	// 4. Cache hit: fresh-enough snapshot and not forced.
	if !opts.Force && e.snapshot != nil && !e.fetchedAt.IsZero() &&
		now.Sub(e.fetchedAt) < MinTTL {
		age := int(now.Sub(e.fetchedAt).Seconds())
		cacheLog("cache[%s] hit (age=%ds)", p.ID(), age)
		s := *e.snapshot
		e.mu.Unlock()
		return s
	}

	// 5. Cache miss — fetch upstream.
	if opts.Force {
		e.lastForceAt = now
	}

	ch := make(chan struct{})
	e.inflight = ch

	forceStr := ""
	if opts.Force {
		forceStr = "forced "
	}
	cacheLog("cache[%s] miss — %sfetching upstream", p.ID(), forceStr)
	e.mu.Unlock()

	// Do the actual fetch outside the lock.
	snapshot, fetchErr := p.Fetch(FetchContext{
		PollIntervalMs: int64(MinTTL / time.Millisecond),
		Force:          opts.Force,
	})

	e.mu.Lock()
	if fetchErr != nil {
		msg := fetchErr.Error()
		e.lastError = &cacheError{message: msg, at: time.Now()}
		cacheLog("cache[%s] fetch FAILED: %s — cooling down for %ds",
			p.ID(), msg, int(CooldownDuration.Seconds()))
		if e.snapshot != nil {
			snapshot = markStale(*e.snapshot, msg)
		} else {
			snapshot = errorSnapshot(p, msg)
		}
	} else {
		// Carry forward metrics that were in the old snapshot but
		// missing from the new one (e.g. a cookie sub-fetch 403'd).
		// They're marked stale so the UI can dim them, but the user
		// still sees data instead of a blank button.
		// Only preserve within StaleTTL so a permanently expired
		// cookie doesn't keep showing stale data forever.
		if e.snapshot != nil && !e.fetchedAt.IsZero() &&
			time.Since(e.fetchedAt) < StaleTTL {
			snapshot = preserveMissing(*e.snapshot, snapshot)
		}
		e.snapshot = &snapshot
		e.fetchedAt = time.Now()
		hadError := e.lastError != nil
		e.lastError = nil
		ids := metricIDs(snapshot)
		recovered := ""
		if hadError {
			recovered = ", recovered from error"
		}
		cacheLog("cache[%s] fetched OK (source=%s, metrics=[%s]%s)",
			p.ID(), snapshot.Source, ids, recovered)
	}

	e.result = &snapshot
	e.inflight = nil
	close(ch) // wake all coalesced waiters
	e.mu.Unlock()

	return snapshot
}

// PeekSnapshot returns the cached snapshot without fetching.
func PeekSnapshot(providerID string) *Snapshot {
	cacheMu.Lock()
	e, ok := entries[providerID]
	cacheMu.Unlock()
	if !ok {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshot
}

// ClearCache removes cached data for a provider (or all if id is "").
func ClearCache(providerID string) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if providerID == "" {
		entries = map[string]*cacheEntry{}
	} else {
		delete(entries, providerID)
	}
}

func markStale(s Snapshot, errMsg string) Snapshot {
	out := s
	out.Error = errMsg
	out.Metrics = make([]MetricValue, len(s.Metrics))
	copy(out.Metrics, s.Metrics)
	for i := range out.Metrics {
		t := true
		out.Metrics[i].Stale = &t
	}
	return out
}

func errorSnapshot(p Provider, errMsg string) Snapshot {
	return Snapshot{
		ProviderID:   p.ID(),
		ProviderName: p.Name(),
		Source:       "cache",
		Metrics:      nil,
		Error:        errMsg,
		Status:       "unknown",
	}
}

// preserveMissing copies metrics from prev into next that are present
// in prev but absent from next, marking them stale. This keeps data
// visible when a sub-fetch (e.g. cookie path) fails transiently.
func preserveMissing(prev, next Snapshot) Snapshot {
	have := make(map[string]struct{}, len(next.Metrics))
	for _, m := range next.Metrics {
		have[m.ID] = struct{}{}
	}
	for _, m := range prev.Metrics {
		if _, ok := have[m.ID]; !ok {
			t := true
			m.Stale = &t
			next.Metrics = append(next.Metrics, m)
		}
	}
	return next
}

func metricIDs(s Snapshot) string {
	if len(s.Metrics) == 0 {
		return "(none)"
	}
	ids := make([]string, len(s.Metrics))
	for i, m := range s.Metrics {
		ids[i] = m.ID
	}
	result := ids[0]
	for _, id := range ids[1:] {
		result += "," + id
	}
	return result
}

func boolStr(cond bool, t, f string) string {
	if cond {
		return t
	}
	return f
}

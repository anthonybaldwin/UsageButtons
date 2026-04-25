package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	// MinTTL matches the shortest user-selectable poll interval.
	// Any poll within this window reuses the snapshot.
	MinTTL = 5 * time.Minute

	// CooldownDuration is how long to stop hitting an API after an
	// upstream error.
	CooldownDuration = 10 * time.Minute

	// ManualCooldown is the minimum gap between user-initiated
	// (force=true) refreshes per provider. Prevents button-mashing from
	// hammering upstream APIs. 30s is responsive enough for deliberate
	// retries but limits a frustrated user to ~2 req/min.
	ManualCooldown = 30 * time.Second

	// StaleTTL is how long missing metrics are preserved from a previous
	// snapshot. After this window a permanently failed sub-fetch (e.g.
	// expired cookie) stops carrying forward stale data so the button
	// can show a setup/error state instead.
	StaleTTL = 30 * time.Minute

	// PersistentTTL bounds how long a snapshot survives process restarts.
	// This smooths normal Stream Deck / computer restarts without showing
	// week-old usage after a long absence.
	PersistentTTL = 24 * time.Hour

	persistentCacheVersion = 1
)

// LogSink is called for cache observability. Set by the plugin at init.
// Dispatch is asynchronous (see cacheLog), so the sink does not need to
// be non-blocking — but callers should not assume log lines arrive
// strictly before subsequent code runs.
var LogSink func(msg string)

// logCh buffers log messages for the async worker. Sized generously so
// a burst of cache events under lock can't overflow in practice; if it
// ever does, cacheLog drops the line rather than blocking.
var logCh = make(chan string, 256)

// logWorkerOnce starts the consumer goroutine on first use.
var logWorkerOnce sync.Once

// startLogWorker drains logCh into LogSink on a dedicated goroutine,
// so cacheLog callers never block on (or re-enter via) the sink.
func startLogWorker() {
	go func() {
		for msg := range logCh {
			if sink := LogSink; sink != nil {
				sink(msg)
			}
		}
	}()
}

// cacheLog formats a log line and enqueues it for the async worker.
// Safe to call while holding cache locks: the sink never runs on the
// caller's goroutine, and a full buffer drops the line instead of
// blocking. Messages preserve FIFO order across callers.
func cacheLog(format string, args ...any) {
	if LogSink == nil {
		return
	}
	logWorkerOnce.Do(startLogWorker)
	msg := fmt.Sprintf(format, args...)
	select {
	case logCh <- msg:
	default:
		// Buffer full — drop rather than stall the cache.
	}
}

// cacheError records the last upstream failure for a provider and the
// earliest time at which fresh fetches should be attempted again.
type cacheError struct {
	message string
	at      time.Time
	// retryAt, when non-zero, overrides the flat CooldownDuration with
	// an absolute time supplied by the server (via Retry-After /
	// x-ratelimit-reset / anthropic-ratelimit-*-reset headers).
	retryAt time.Time
}

// retryAfterer is satisfied by error types that expose a server-hinted
// retry time. httputil.Error implements it; any future error type that
// wraps an upstream response can satisfy the same interface to share
// this cooldown logic without importing httputil from the cache layer.
type retryAfterer interface {
	RetryAfter() time.Time
}

// retryAfterFromError returns the upstream-supplied retry time if the
// error (or any error in its chain) provides one, else zero-value.
func retryAfterFromError(err error) time.Time {
	var ra retryAfterer
	if errors.As(err, &ra) {
		return ra.RetryAfter()
	}
	return time.Time{}
}

// cooldownUntil returns the absolute time the cache should stop
// serving fresh fetches after a given error. A server hint wins when
// present; otherwise we fall back to at+CooldownDuration.
func (e *cacheError) cooldownUntil() time.Time {
	if !e.retryAt.IsZero() {
		return e.retryAt
	}
	return e.at.Add(CooldownDuration)
}

// cacheEntry is the per-provider cache slot tracking the last snapshot,
// the last error, and the in-flight fetch promise used for coalescing.
type cacheEntry struct {
	snapshot    *Snapshot
	fetchedAt   time.Time
	lastError   *cacheError
	lastForceAt time.Time

	// mu protects the inflight promise pattern.
	mu        sync.Mutex
	inflight  chan struct{} // non-nil when a fetch is in progress
	result    *Snapshot     // set when inflight completes
	resultErr error
}

// persistentCacheDir returns the directory used for restart-surviving
// provider snapshots. Tests replace it with a temp directory.
var persistentCacheDir = defaultPersistentCacheDir

// persistentSnapshot is the on-disk representation of one cached provider
// snapshot.
type persistentSnapshot struct {
	Version           int       `json:"version"`
	ProviderID        string    `json:"providerId"`
	ConfigFingerprint string    `json:"configFingerprint"`
	FetchedAt         time.Time `json:"fetchedAt"`
	Snapshot          Snapshot  `json:"snapshot"`
}

var (
	// cacheMu guards entries against concurrent map access.
	cacheMu sync.Mutex
	// entries is the provider-ID-keyed cache map.
	entries = map[string]*cacheEntry{}
)

// getEntry returns (and lazily allocates) the cache entry for providerID.
func getEntry(providerID string) *cacheEntry {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	e, ok := entries[providerID]
	if !ok {
		e = &cacheEntry{}
		if s, fetchedAt, ok := loadPersistentSnapshot(providerID); ok {
			e.snapshot = &s
			e.result = &s
			e.fetchedAt = fetchedAt
			cacheLog("cache[%s] restored persisted snapshot (age=%ds)",
				providerID, int(time.Since(fetchedAt).Seconds()))
		}
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

	// 1. Error cooldown: serve stale or error snapshot. If the server
	//    sent a retry hint, honor it; otherwise fall back to the flat
	//    CooldownDuration.
	if e.lastError != nil {
		until := e.lastError.cooldownUntil()
		if now.Before(until) {
			left := until.Sub(now)
			source := "default"
			if !e.lastError.retryAt.IsZero() {
				source = "server-hinted"
			}
			cacheLog("cache[%s] cool-down (%s): %ds left, serving %s (last error: %s)",
				p.ID(), source, int(left.Seconds()),
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
	fetchConfigFingerprint := providerConfigFingerprint(p.ID())

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
	persist := false
	persistedAt := time.Time{}
	if fetchErr != nil {
		msg := fetchErr.Error()
		retryAt := retryAfterFromError(fetchErr)
		e.lastError = &cacheError{message: msg, at: time.Now(), retryAt: retryAt}
		if !retryAt.IsZero() {
			cacheLog("cache[%s] fetch FAILED: %s — server hint: retry after %ds",
				p.ID(), msg, int(time.Until(retryAt).Seconds()))
		} else {
			cacheLog("cache[%s] fetch FAILED: %s — cooling down for %ds",
				p.ID(), msg, int(CooldownDuration.Seconds()))
		}
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
		persist = shouldPersistProviderSnapshot(p.ID(), snapshot)
		persistedAt = e.fetchedAt
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

	if persist {
		persistSnapshot(p.ID(), fetchConfigFingerprint, snapshot, persistedAt)
	}

	return snapshot
}

// PeekSnapshotState returns the last rendered snapshot and its
// fetch time without triggering a new fetch. Prefers e.result so
// stale/error faces produced on the last fetch are preserved across
// minute redraws; falls back to e.snapshot when no fetch has run.
func PeekSnapshotState(providerID string) (*Snapshot, time.Time) {
	if providerID == "" {
		return nil, time.Time{}
	}
	e := getEntry(providerID)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.result != nil {
		return e.result, e.fetchedAt
	}
	return e.snapshot, e.fetchedAt
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
	clearPersistentCache(providerID)
}

// ClearRuntimeCache removes only in-memory cached data for a provider
// (or all providers if id is ""), preserving restart-surviving snapshots.
func ClearRuntimeCache(providerID string) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if providerID == "" {
		entries = map[string]*cacheEntry{}
	} else {
		delete(entries, providerID)
	}
}

// markStale returns a deep-enough copy of s with every metric marked
// stale and the given error message attached.
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

// errorSnapshot builds a placeholder Snapshot for a provider that has no
// prior successful fetch to fall back on.
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

// metricIDs returns a comma-joined list of the metric IDs in s for logging.
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

// boolStr picks t when cond is true and f otherwise; a tiny helper used
// by log formatting.
func boolStr(cond bool, t, f string) string {
	if cond {
		return t
	}
	return f
}

// defaultPersistentCacheDir resolves the cross-platform cache directory.
func defaultPersistentCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base, err = os.UserConfigDir()
		if err != nil || base == "" {
			return "", err
		}
	}
	return filepath.Join(base, "UsageButtons", "provider-cache"), nil
}

// persistentSnapshotPath returns the path for providerID's persisted
// snapshot file.
func persistentSnapshotPath(providerID string) (string, error) {
	dir, err := persistentCacheDir()
	if err != nil || dir == "" {
		return "", err
	}
	return filepath.Join(dir, safeCacheFilename(providerID)+".json"), nil
}

// safeCacheFilename maps provider IDs onto a conservative filename subset.
func safeCacheFilename(providerID string) string {
	if providerID == "" {
		return "unknown"
	}
	out := make([]rune, 0, len(providerID))
	for _, r := range providerID {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r)
		case r >= '0' && r <= '9':
			out = append(out, r)
		case r == '-' || r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// shouldPersistSnapshot reports whether a snapshot has useful data to restore
// after a restart.
func shouldPersistSnapshot(s Snapshot) bool {
	return s.Error == "" && len(s.Metrics) > 0
}

// shouldPersistProviderSnapshot reports whether a provider snapshot is safe to
// restore after a restart without revalidating upstream account identity.
func shouldPersistProviderSnapshot(providerID string, s Snapshot) bool {
	return shouldPersistSnapshot(s) && !usesUnfingerprintedBrowserSession(providerID, s)
}

// usesUnfingerprintedBrowserSession reports whether the snapshot depends on a
// browser session that providerConfigFingerprint cannot validate at startup.
func usesUnfingerprintedBrowserSession(providerID string, s Snapshot) bool {
	switch providerID {
	case "cursor", "ollama", "amp":
		return true
	case "claude", "codex", "augment":
		return s.Source == "cookie"
	default:
		return false
	}
}

// persistSnapshot writes a successful provider snapshot to disk with the
// config fingerprint captured at fetch start. Failures are logged but do not
// affect the live button update path.
func persistSnapshot(providerID, configFingerprint string, s Snapshot, fetchedAt time.Time) {
	path, err := persistentSnapshotPath(providerID)
	if err != nil || path == "" {
		cacheLog("cache[%s] persist skipped: %v", providerID, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		cacheLog("cache[%s] persist mkdir: %v", providerID, err)
		return
	}
	payload := persistentSnapshot{
		Version:           persistentCacheVersion,
		ProviderID:        providerID,
		ConfigFingerprint: configFingerprint,
		FetchedAt:         fetchedAt,
		Snapshot:          s,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		cacheLog("cache[%s] persist marshal: %v", providerID, err)
		return
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		cacheLog("cache[%s] persist write: %v", providerID, err)
	}
}

// loadPersistentSnapshot reads a restart-surviving snapshot, rejecting
// incompatible, mismatched, or overly old cache entries.
func loadPersistentSnapshot(providerID string) (Snapshot, time.Time, bool) {
	path, err := persistentSnapshotPath(providerID)
	if err != nil || path == "" {
		return Snapshot{}, time.Time{}, false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, time.Time{}, false
	}
	var payload persistentSnapshot
	if err := json.Unmarshal(body, &payload); err != nil {
		cacheLog("cache[%s] persisted snapshot invalid: %v", providerID, err)
		_ = os.Remove(path)
		return Snapshot{}, time.Time{}, false
	}
	if payload.Version != persistentCacheVersion ||
		payload.ProviderID != providerID ||
		payload.ConfigFingerprint != providerConfigFingerprint(providerID) ||
		payload.Snapshot.ProviderID != providerID ||
		payload.FetchedAt.IsZero() ||
		!shouldPersistProviderSnapshot(providerID, payload.Snapshot) {
		_ = os.Remove(path)
		return Snapshot{}, time.Time{}, false
	}
	age := time.Since(payload.FetchedAt)
	if age < 0 || age > PersistentTTL {
		_ = os.Remove(path)
		return Snapshot{}, time.Time{}, false
	}
	s := payload.Snapshot
	if age >= MinTTL {
		s = markMetricsStale(s)
	}
	return s, payload.FetchedAt, true
}

// providerConfigFingerprint returns a stable fingerprint of the credentials
// and endpoint inputs the provider will use for upstream requests.
func providerConfigFingerprint(providerID string) string {
	pk := settings.ProviderKeysGet()
	parts := []string{"provider", providerID}
	switch providerID {
	case "openrouter":
		parts = append(parts,
			"api-key", settings.ResolveAPIKey(pk.OpenRouterKey, "OPENROUTER_API_KEY"),
			"base-url", settings.ResolveEndpoint(pk.OpenRouterURL, "https://openrouter.ai/api/v1", "OPENROUTER_API_URL"),
		)
	case "warp":
		parts = append(parts,
			"api-key", settings.ResolveAPIKey(pk.WarpKey, "WARP_API_KEY", "WARP_TOKEN"),
		)
	case "zai":
		parts = append(parts,
			"api-key", settings.ResolveAPIKey(pk.ZaiKey, "Z_AI_API_KEY", "ZAI_API_TOKEN", "ZAI_API_KEY"),
			"quota-url", zaiQuotaURLFingerprint(pk),
		)
	case "kimi-k2":
		parts = append(parts,
			"api-key", settings.ResolveAPIKey(pk.KimiK2Key, "KIMI_K2_API_KEY", "KIMI_API_KEY", "KIMI_KEY"),
		)
	case "copilot":
		parts = append(parts,
			"token", settings.ResolveAPIKey(pk.CopilotToken, "GITHUB_TOKEN"),
			"hosts", fileContentFingerprint(copilotHostsPath()),
			"apps", fileContentFingerprint(copilotAppsPath()),
		)
	case "synthetic":
		parts = append(parts,
			"api-key", settings.ResolveAPIKey(pk.SyntheticKey, "SYNTHETIC_API_KEY", "SYNTHETIC_API_TOKEN"),
		)
	case "kilo":
		parts = append(parts,
			"api-key", settings.ResolveAPIKey(pk.KiloKey, "KILO_API_KEY"),
			"cli-auth", fileContentFingerprint(homePath(".local", "share", "kilo", "auth.json")),
		)
	case "codex":
		parts = append(parts,
			"base-url", pk.CodexChatGPTBaseURL,
			"codex-home", strings.TrimSpace(os.Getenv("CODEX_HOME")),
			"auth", fileContentFingerprint(codexCredPath()),
			"config", fileContentFingerprint(codexConfigPath()),
		)
	case "claude":
		parts = append(parts,
			"credentials", claudeCredentialsFingerprint(),
		)
	}
	return fingerprintParts(parts...)
}

// zaiQuotaURLFingerprint mirrors the z.ai provider endpoint selection.
func zaiQuotaURLFingerprint(pk settings.ProviderKeys) string {
	if full := settings.ResolveEndpoint(pk.ZaiQuotaURL, "", "Z_AI_QUOTA_URL"); full != "" {
		return full
	}
	host := settings.ResolveEndpoint(pk.ZaiHost, "", "Z_AI_API_HOST")
	if host == "" {
		host = zaiRegionHostFingerprint(pk.ZaiRegion)
	}
	return host + "/api/monitor/usage/quota/limit"
}

// zaiRegionHostFingerprint mirrors the z.ai region picker defaults.
func zaiRegionHostFingerprint(region string) string {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "bigmodel-cn", "bigmodel", "cn", "china":
		return "https://open.bigmodel.cn"
	default:
		return "https://api.z.ai"
	}
}

// copilotHostsPath returns the GitHub Copilot hosts.json path.
func copilotHostsPath() string {
	return homePath(".config", "github-copilot", "hosts.json")
}

// copilotAppsPath returns the GitHub Copilot apps.json path.
func copilotAppsPath() string {
	return homePath(".config", "github-copilot", "apps.json")
}

// codexConfigPath returns the Codex config.toml path.
func codexConfigPath() string {
	if ch := strings.TrimSpace(os.Getenv("CODEX_HOME")); ch != "" {
		return filepath.Join(ch, "config.toml")
	}
	return homePath(".codex", "config.toml")
}

// claudeCredentialsFingerprint mirrors Claude credential source precedence.
func claudeCredentialsFingerprint() string {
	path := claudeCredPath()
	body, err := os.ReadFile(path)
	if err == nil {
		return "file:" + contentFingerprint(body)
	}
	if !os.IsNotExist(err) {
		return "file-error"
	}
	return "keychain:" + claudeKeychainCredentialFingerprint()
}

// homePath joins path elements under the current user's home directory.
func homePath(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	all := append([]string{home}, parts...)
	return filepath.Join(all...)
}

// fileContentFingerprint hashes a known config or credential file.
func fileContentFingerprint(path string) string {
	if strings.TrimSpace(path) == "" {
		return "missing"
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "missing"
	}
	return contentFingerprint(body)
}

// contentFingerprint hashes a credential or config blob.
func contentFingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// fingerprintParts hashes a delimited list of provider config inputs.
func fingerprintParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// clearPersistentCache removes one provider snapshot, or the full persisted
// cache directory when providerID is empty.
func clearPersistentCache(providerID string) {
	if providerID == "" {
		dir, err := persistentCacheDir()
		if err == nil && dir != "" {
			_ = os.RemoveAll(dir)
		}
		return
	}
	path, err := persistentSnapshotPath(providerID)
	if err == nil && path != "" {
		_ = os.Remove(path)
	}
}

// markMetricsStale returns a copy of s with every metric dimmed, without
// setting Snapshot.Error. Used for restored disk snapshots so the tile can
// look like last-known data without triggering an error banner.
func markMetricsStale(s Snapshot) Snapshot {
	out := s
	out.Metrics = make([]MetricValue, len(s.Metrics))
	copy(out.Metrics, s.Metrics)
	for i := range out.Metrics {
		t := true
		out.Metrics[i].Stale = &t
	}
	return out
}

package providers

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// cacheTestProvider is a deterministic provider used by cache persistence tests.
type cacheTestProvider struct {
	id       string
	snapshot Snapshot
	fetches  int
}

// ID returns the configured test provider ID.
func (p *cacheTestProvider) ID() string { return p.id }

// Name returns the display name for cache test snapshots.
func (p *cacheTestProvider) Name() string { return "Test Provider" }

// BrandColor returns a stable foreground color for test rendering metadata.
func (p *cacheTestProvider) BrandColor() string { return "#ffffff" }

// BrandBg returns a stable background color for test rendering metadata.
func (p *cacheTestProvider) BrandBg() string { return "#000000" }

// MetricIDs returns the single metric emitted by the cache test provider.
func (p *cacheTestProvider) MetricIDs() []string { return []string{"session-percent"} }

// Fetch records a fetch and returns the configured snapshot.
func (p *cacheTestProvider) Fetch(_ FetchContext) (Snapshot, error) {
	p.fetches++
	return p.snapshot, nil
}

// TestPersistentCacheHydratesPeek verifies cache-only reads restore from disk.
func TestPersistentCacheHydratesPeek(t *testing.T) {
	withTempPersistentCache(t)
	p := newCacheTestProvider("test-provider")

	got := GetSnapshot(p, GetSnapshotOptions{})
	if got.ProviderID != p.id {
		t.Fatalf("GetSnapshot provider = %q, want %q", got.ProviderID, p.id)
	}
	if p.fetches != 1 {
		t.Fatalf("fetches after first GetSnapshot = %d, want 1", p.fetches)
	}

	path, err := persistentSnapshotPath(p.id)
	if err != nil {
		t.Fatalf("persistentSnapshotPath: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("persisted snapshot missing: %v", err)
	}

	clearMemoryCache()
	snapshot, fetchedAt := PeekSnapshotState(p.id)
	if snapshot == nil {
		t.Fatal("PeekSnapshotState returned nil after persisted restore")
	}
	if snapshot.ProviderID != p.id {
		t.Fatalf("restored provider = %q, want %q", snapshot.ProviderID, p.id)
	}
	if fetchedAt.IsZero() {
		t.Fatal("restored fetchedAt is zero")
	}
	if p.fetches != 1 {
		t.Fatalf("fetches after persisted restore = %d, want 1", p.fetches)
	}

	_ = GetSnapshot(p, GetSnapshotOptions{})
	if p.fetches != 1 {
		t.Fatalf("fresh persisted snapshot should hit cache; fetches = %d, want 1", p.fetches)
	}
}

// TestPersistentCacheStaleMarksRestoredMetrics verifies old disk snapshots dim metrics.
func TestPersistentCacheStaleMarksRestoredMetrics(t *testing.T) {
	withTempPersistentCache(t)
	p := newCacheTestProvider("test-stale-provider")
	_ = GetSnapshot(p, GetSnapshotOptions{})

	path, err := persistentSnapshotPath(p.id)
	if err != nil {
		t.Fatalf("persistentSnapshotPath: %v", err)
	}
	var payload persistentSnapshot
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal persisted snapshot: %v", err)
	}
	payload.FetchedAt = time.Now().Add(-(MinTTL + time.Second))
	body, err = json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal persisted snapshot: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("rewrite persisted snapshot: %v", err)
	}

	clearMemoryCache()
	snapshot, _ := PeekSnapshotState(p.id)
	if snapshot == nil || len(snapshot.Metrics) != 1 {
		t.Fatalf("restored snapshot metrics = %#v, want one metric", snapshot)
	}
	if snapshot.Metrics[0].Stale == nil || !*snapshot.Metrics[0].Stale {
		t.Fatal("restored old metric was not marked stale")
	}
	if snapshot.Error != "" {
		t.Fatalf("restored stale snapshot error = %q, want empty", snapshot.Error)
	}
}

// TestPersistentCacheRejectsFingerprintMismatch verifies config changes reject disk snapshots.
func TestPersistentCacheRejectsFingerprintMismatch(t *testing.T) {
	withTempPersistentCache(t)
	p := newCacheTestProvider("test-fingerprint-provider")
	_ = GetSnapshot(p, GetSnapshotOptions{})

	path, err := persistentSnapshotPath(p.id)
	if err != nil {
		t.Fatalf("persistentSnapshotPath: %v", err)
	}
	var payload persistentSnapshot
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal persisted snapshot: %v", err)
	}
	payload.ConfigFingerprint = "different-config"
	body, err = json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal persisted snapshot: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("rewrite persisted snapshot: %v", err)
	}

	clearMemoryCache()
	snapshot, _ := PeekSnapshotState(p.id)
	if snapshot != nil {
		t.Fatalf("PeekSnapshotState restored mismatched snapshot: %#v", snapshot)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched persisted snapshot still exists: %v", err)
	}
}

// TestClearRuntimeCachePreservesPersistentSnapshot verifies startup clears stay disk-backed.
func TestClearRuntimeCachePreservesPersistentSnapshot(t *testing.T) {
	withTempPersistentCache(t)
	p := newCacheTestProvider("test-runtime-clear-provider")
	_ = GetSnapshot(p, GetSnapshotOptions{})

	path, err := persistentSnapshotPath(p.id)
	if err != nil {
		t.Fatalf("persistentSnapshotPath: %v", err)
	}
	ClearRuntimeCache(p.id)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("persisted snapshot missing after ClearRuntimeCache: %v", err)
	}
	snapshot, _ := PeekSnapshotState(p.id)
	if snapshot == nil {
		t.Fatal("PeekSnapshotState returned nil after runtime-only clear")
	}
	if p.fetches != 1 {
		t.Fatalf("runtime-only restore fetched upstream; fetches = %d, want 1", p.fetches)
	}
}

// TestClearCacheRemovesPersistentSnapshot verifies full clears remove disk state.
func TestClearCacheRemovesPersistentSnapshot(t *testing.T) {
	withTempPersistentCache(t)
	p := newCacheTestProvider("test-clear-provider")
	_ = GetSnapshot(p, GetSnapshotOptions{})

	path, err := persistentSnapshotPath(p.id)
	if err != nil {
		t.Fatalf("persistentSnapshotPath: %v", err)
	}
	ClearCache(p.id)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("persisted snapshot still exists after ClearCache: %v", err)
	}
}

// newCacheTestProvider returns a provider with one successful metric.
func newCacheTestProvider(id string) *cacheTestProvider {
	return &cacheTestProvider{
		id: id,
		snapshot: Snapshot{
			ProviderID:   id,
			ProviderName: "Test Provider",
			Source:       "test",
			Status:       "operational",
			Metrics: []MetricValue{{
				ID:    "session-percent",
				Label: "SESSION",
				Value: "42%",
			}},
		},
	}
}

// withTempPersistentCache redirects persistent cache writes to a temp directory.
func withTempPersistentCache(t *testing.T) {
	t.Helper()
	oldDir := persistentCacheDir
	dir := t.TempDir()
	persistentCacheDir = func() (string, error) {
		return dir, nil
	}
	clearMemoryCache()
	t.Cleanup(func() {
		clearMemoryCache()
		persistentCacheDir = oldDir
	})
}

// clearMemoryCache resets the process-local provider cache map.
func clearMemoryCache() {
	cacheMu.Lock()
	entries = map[string]*cacheEntry{}
	cacheMu.Unlock()
}

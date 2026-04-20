package providers

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// credWatchInterval is how often we poll watched credential files for
// mtime changes. Kept short so a post-login tile update arrives within
// tens of seconds, but not so short that the loop is wasteful.
const credWatchInterval = 10 * time.Second

// credWatchTarget pairs a provider ID with a credential-file path producer
// and the last-observed stat data used to detect changes.
type credWatchTarget struct {
	providerID string
	pathFn     func() string
	lastMtime  time.Time
	lastSize   int64
}

var (
	// credWatchOnce guards one-shot initialization of the watcher.
	credWatchOnce sync.Once
	// credWatchTargets is the list of credential files being polled.
	credWatchTargets []*credWatchTarget
)

// StartCredentialWatcher starts a background goroutine that polls known
// provider credential files and clears the matching provider's cache
// entry when the file changes. The next scheduled poll then fetches
// fresh data instead of waiting up to MinTTL for the timer to advance.
//
// Safe to call more than once — the watcher is a singleton.
func StartCredentialWatcher() {
	credWatchOnce.Do(func() {
		credWatchTargets = []*credWatchTarget{
			{providerID: "claude", pathFn: claudeCredPath},
			{providerID: "codex", pathFn: codexCredPath},
		}
		// Seed last-known stats so the first change after startup —
		// not the presence of a pre-existing file — triggers the clear.
		for _, t := range credWatchTargets {
			if fi, err := os.Stat(t.pathFn()); err == nil {
				t.lastMtime = fi.ModTime()
				t.lastSize = fi.Size()
			}
		}
		go credWatchLoop()
	})
}

// credWatchLoop is the goroutine that polls every credWatchTarget on a
// fixed interval until the process exits.
func credWatchLoop() {
	t := time.NewTicker(credWatchInterval)
	defer t.Stop()
	for range t.C {
		for _, tgt := range credWatchTargets {
			checkCredTarget(tgt)
		}
	}
}

// checkCredTarget inspects a credential file and clears the matching
// provider's cache when mtime or size has changed since the last check.
func checkCredTarget(t *credWatchTarget) {
	fi, err := os.Stat(t.pathFn())
	if err != nil {
		// File went missing. Treat as a change only if we had seen it
		// before — otherwise we'd churn the cache on every tick when a
		// provider isn't configured.
		if !t.lastMtime.IsZero() {
			cacheLog("credwatch[%s] file removed — clearing cache", t.providerID)
			ClearCache(t.providerID)
			t.lastMtime = time.Time{}
			t.lastSize = 0
		}
		return
	}
	mt := fi.ModTime()
	sz := fi.Size()
	if mt.Equal(t.lastMtime) && sz == t.lastSize {
		return
	}
	// Suppress the very first observation: StartCredentialWatcher
	// already seeded initial stats, so a zero lastMtime here means the
	// file was created after the plugin started. That IS a change
	// worth clearing the cache for.
	cacheLog("credwatch[%s] credential file changed — clearing cache", t.providerID)
	ClearCache(t.providerID)
	t.lastMtime = mt
	t.lastSize = sz
}

// claudeCredPath returns the filesystem path to the Claude OAuth credentials.
func claudeCredPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", ".credentials.json")
}

// codexCredPath returns the filesystem path to the Codex OAuth credentials,
// honoring the CODEX_HOME override when set.
func codexCredPath() string {
	if ch := os.Getenv("CODEX_HOME"); ch != "" {
		return filepath.Join(ch, "auth.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "auth.json")
}

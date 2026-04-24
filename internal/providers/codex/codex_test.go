package codex

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUsagePathMatchesCodexBarEndpointSelection verifies usage-path selection
// for backend-api-style and Codex-API-style base URLs.
func TestUsagePathMatchesCodexBarEndpointSelection(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{name: "chatgpt backend", base: "https://chatgpt.com/backend-api", want: "/wham/usage"},
		{name: "custom codex api", base: "https://example.test", want: "/api/codex/usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usagePath(tt.base); got != tt.want {
				t.Fatalf("usagePath(%q) = %q, want %q", tt.base, got, tt.want)
			}
		})
	}
}

// TestNeedsRefreshBacksOffRecentFailedAttempt verifies older auth files without
// last_refresh do not retry a failed OAuth refresh on every polling cycle.
func TestNeedsRefreshBacksOffRecentFailedAttempt(t *testing.T) {
	recent := time.Now().UTC().Add(-refreshRetryAfter / 2)
	old := time.Now().UTC().Add(-refreshRetryAfter * 2)

	if (codexCreds{refreshToken: "refresh", lastAttempt: &recent}).needsRefresh() {
		t.Fatalf("needsRefresh() = true with a recent failed refresh attempt, want false")
	}
	if !(codexCreds{refreshToken: "refresh", lastAttempt: &old}).needsRefresh() {
		t.Fatalf("needsRefresh() = false with an old failed refresh attempt, want true")
	}
}

// TestSaveRefreshAttemptPersistsCooldown verifies failed refresh cooldowns are
// persisted so reloads from auth.json keep backing off refresh outages.
func TestSaveRefreshAttemptPersistsCooldown(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	auth := `{"tokens":{"access_token":"access","refresh_token":"refresh"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	attemptedAt := time.Now().UTC()
	if err := saveRefreshAttempt(attemptedAt); err != nil {
		t.Fatalf("saveRefreshAttempt() error = %v", err)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials() error = %v", err)
	}
	if creds.lastAttempt == nil {
		t.Fatalf("loadCredentials().lastAttempt = nil, want persisted attempt timestamp")
	}
	if creds.needsRefresh() {
		t.Fatalf("needsRefresh() = true after persisted failed refresh attempt, want false")
	}
}

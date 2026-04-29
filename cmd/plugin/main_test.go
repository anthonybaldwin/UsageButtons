package main

import "testing"

// TestMigrateMetricID locks in the rename-alias contract: pre-rename
// metric IDs persisted in saved buttons must keep resolving to the
// new provider-specific IDs, and unrelated providers / unaliased IDs
// must pass through unchanged.
func TestMigrateMetricID(t *testing.T) {
	tests := []struct {
		name           string
		provider, in   string
		want           string
	}{
		{"gemini opus → flash-lite", "gemini", "opus-percent", "flash-lite-percent"},
		{"gemini session → pro", "gemini", "session-percent", "pro-percent"},
		{"gemini weekly → flash", "gemini", "weekly-percent", "flash-percent"},
		{"antigravity opus → gemini-flash", "antigravity", "opus-percent", "gemini-flash-percent"},
		{"antigravity session → claude", "antigravity", "session-percent", "claude-percent"},
		{"antigravity weekly → gemini-pro", "antigravity", "weekly-percent", "gemini-pro-percent"},
		{"alibaba opus → monthly", "alibaba", "opus-percent", "monthly-percent"},
		{"alibaba session unchanged", "alibaba", "session-percent", "session-percent"},
		{"alibaba weekly unchanged", "alibaba", "weekly-percent", "weekly-percent"},
		{"claude passthrough", "claude", "session-percent", "session-percent"},
		{"claude opus passthrough", "claude", "weekly-opus-percent", "weekly-opus-percent"},
		{"unknown provider passthrough", "unknown", "opus-percent", "opus-percent"},
		{"empty provider passthrough", "", "opus-percent", "opus-percent"},
		{"empty metric passthrough", "gemini", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := migrateMetricID(tc.provider, tc.in)
			if got != tc.want {
				t.Errorf("migrateMetricID(%q, %q) = %q, want %q", tc.provider, tc.in, got, tc.want)
			}
		})
	}
}

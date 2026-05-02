package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/streamdeck"
)

// TestManifestActionUUIDsResolveToRegisteredProviders catches the
// silent breakage where a manifest action UUID lower-cases to a
// provider ID that nobody registered. The Hermes Agent button shipped
// busted in PR #70 with UUID `...hermesagent` while the provider
// registers as `hermes-agent` — ProviderIDFromAction returned
// `hermesagent` and the lookup fell through to nil, so the plugin
// never called Fetch(). This regression test would have caught it.
func TestManifestActionUUIDsResolveToRegisteredProviders(t *testing.T) {
	manifestPath := filepath.Join("..", "..", "io.github.anthonybaldwin.UsageButtons.sdPlugin", "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m struct {
		Actions []struct {
			UUID string `json:"UUID"`
			Name string `json:"Name"`
		} `json:"Actions"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(m.Actions) == 0 {
		t.Fatal("no actions in manifest; parse failed silently")
	}
	for _, a := range m.Actions {
		t.Run(a.Name, func(t *testing.T) {
			id := streamdeck.ProviderIDFromAction(a.UUID)
			if id == "" {
				t.Errorf("UUID %q does not parse to a provider ID (missing prefix?)", a.UUID)
				return
			}
			if p := providers.Get(id); p == nil {
				t.Errorf("UUID %q derives provider ID %q, but no provider with that ID is registered (check the action UUID in manifest.json against the providerID const in internal/providers/%s/)", a.UUID, id, strings.ReplaceAll(id, "-", ""))
			}
		})
	}
}

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

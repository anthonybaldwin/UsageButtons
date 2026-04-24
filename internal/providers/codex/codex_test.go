package codex

import "testing"

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

package openclaw

import (
	"errors"
	"testing"

	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

func TestProviderMetadata(t *testing.T) {
	p := Provider{}
	if p.ID() != "openclaw" {
		t.Errorf("ID: got %q, want openclaw", p.ID())
	}
	if p.Name() != "OpenClaw" {
		t.Errorf("Name: got %q, want OpenClaw", p.Name())
	}
	// 5 views × 3 windows = 15 metric IDs, no extras.
	const want = 15
	ids := p.MetricIDs()
	if len(ids) != want {
		t.Fatalf("expected %d metric IDs, got %d", want, len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate metric ID: %s", id)
		}
		seen[id] = true
	}
	for _, want := range []string{
		"openclaw-input-tokens-daily",
		"openclaw-cost-monthly",
		"openclaw-total-tokens-weekly",
	} {
		if !seen[want] {
			t.Errorf("expected metric %q in MetricIDs()", want)
		}
	}
}

// TestResolveBase_NormalizesScheme — users may paste an http(s):// URL
// from their browser; we transparently convert to ws(s)://. The
// trailing slash trimming is handled by settings.ResolveEndpoint.
func TestResolveBase_NormalizesScheme(t *testing.T) {
	oldSettings := settings.Get()
	t.Cleanup(func() { settings.Set(oldSettings) })
	t.Setenv("OPENCLAW_BASE_URL", "")

	cases := []struct {
		in   string
		want string
	}{
		{"http://192.168.1.1:18789", "ws://192.168.1.1:18789"},
		{"https://openclaw.tailnet.ts.net", "wss://openclaw.tailnet.ts.net"},
		{"ws://127.0.0.1:18789", "ws://127.0.0.1:18789"},
		{"wss://openclaw.tailnet.ts.net", "wss://openclaw.tailnet.ts.net"},
	}
	for _, c := range cases {
		settings.Set(settings.GlobalSettings{
			ProviderKeys: settings.ProviderKeys{OpenClawBaseURL: c.in},
		})
		got, err := resolveBase()
		if err != nil {
			t.Errorf("resolveBase(%q) err: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolveBase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveBase_RejectsBadScheme(t *testing.T) {
	oldSettings := settings.Get()
	t.Cleanup(func() { settings.Set(oldSettings) })
	t.Setenv("OPENCLAW_BASE_URL", "")
	settings.Set(settings.GlobalSettings{
		ProviderKeys: settings.ProviderKeys{OpenClawBaseURL: "ftp://oops"},
	})
	if _, err := resolveBase(); err == nil {
		t.Error("expected error for bad scheme, got nil")
	}
}

func TestWindowMetrics_Shape(t *testing.T) {
	now := "2026-05-01T00:00:00Z"
	totals := CostUsageTotals{
		Input:       1000,
		Output:      500,
		CacheRead:   200,
		TotalTokens: 1700,
		TotalCost:   3.456,
	}
	out := windowMetrics(window{Days: 7, Slug: "weekly", Label: "WEEK"}, totals, now)
	if len(out) != 5 {
		t.Fatalf("expected 5 metrics per window, got %d", len(out))
	}
	for _, m := range out {
		if m.ID == "openclaw-cost-weekly" {
			// Stored rounded to cents.
			if got := m.NumericVal(); got != 3.46 {
				t.Errorf("cost NumericVal = %v, want 3.46", got)
			}
			if v, ok := m.Value.(string); !ok || v != "$3.46" {
				t.Errorf("cost Value = %v, want \"$3.46\"", m.Value)
			}
		}
		if m.ID == "openclaw-total-tokens-weekly" {
			if got := m.NumericVal(); got != 1700 {
				t.Errorf("total tokens NumericVal = %v, want 1700", got)
			}
		}
	}
}

func TestFormatCount(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{42, "42"},
		{1500, "1.5k"},
		{2_500_000, "2.5M"},
		{4_200_000_000, "4.2B"},
	}
	for _, c := range cases {
		if got := formatCount(c.in); got != c.want {
			t.Errorf("formatCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsAuthErr(t *testing.T) {
	if !isAuthErr(&gatewayError{Code: "AUTH_TOKEN_MISMATCH", Message: ""}) {
		t.Error("AUTH_TOKEN_MISMATCH should be auth error")
	}
	if !isAuthErr(&gatewayError{Code: "UNAUTHORIZED"}) {
		t.Error("UNAUTHORIZED should be auth error")
	}
	if !isAuthErr(&gatewayError{Code: "OTHER", Message: "invalid token"}) {
		t.Error("substring 'invalid token' should be flagged as auth")
	}
	if isAuthErr(&gatewayError{Code: "INTERNAL", Message: "kaboom"}) {
		t.Error("non-auth error should not be flagged")
	}
	if isAuthErr(errors.New("network unreachable")) {
		t.Error("plain error without auth signal should not be flagged")
	}
}

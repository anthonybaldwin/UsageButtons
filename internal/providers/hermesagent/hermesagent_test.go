package hermesagent

import (
	"testing"
	"time"
)

// TestTokenInjectionRegex verifies the regex matches the literal HTML
// shape Hermes Agent injects into its index.html. Source:
// hermes_cli/web_server.py:3203 (NousResearch/hermes-agent).
func TestTokenInjectionRegex(t *testing.T) {
	html := `<html><body><div id="root"></div>` +
		`<script>window.__HERMES_SESSION_TOKEN__="abc123XYZ-_";` +
		`window.__HERMES_DASHBOARD_EMBEDDED_CHAT__=false;</script>` +
		`<script src="/assets/index-abc.js"></script></body></html>`
	m := tokenInjectionRe.FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("regex did not match expected injection format, got %v", m)
	}
	if m[1] != "abc123XYZ-_" {
		t.Errorf("captured token = %q, want abc123XYZ-_", m[1])
	}
}

func TestTokenInjectionRegex_MissingReturnsNil(t *testing.T) {
	html := `<html><body><div id="root"></div></body></html>`
	if m := tokenInjectionRe.FindStringSubmatch(html); m != nil {
		t.Errorf("regex should not match plain HTML, got %v", m)
	}
}

func TestProviderMetadata(t *testing.T) {
	p := Provider{}
	if p.ID() != "hermes-agent" {
		t.Errorf("ID: got %q, want hermes-agent", p.ID())
	}
	if p.Name() != "Hermes Agent" {
		t.Errorf("Name: got %q, want Hermes Agent", p.Name())
	}
	// 5 views × 3 windows + 1 active-sessions = 16 metrics.
	const want = 16
	ids := p.MetricIDs()
	if len(ids) != want {
		t.Fatalf("expected %d metric IDs, got %d: %v", want, len(ids), ids)
	}
	// Every emitted ID must be unique — duplicates would silently
	// conflict in the registry.
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate metric ID: %s", id)
		}
		seen[id] = true
	}
	// Sanity-check a few canonical IDs exist.
	for _, want := range []string{
		"hermes-agent-input-tokens-daily",
		"hermes-agent-cost-monthly",
		"hermes-agent-active-sessions",
	} {
		if !seen[want] {
			t.Errorf("expected metric %q in MetricIDs()", want)
		}
	}
}

func TestWindowMetrics_Shape(t *testing.T) {
	now := "2026-05-01T00:00:00Z"
	totals := usageTotals{
		TotalInput:         1000,
		TotalOutput:        500,
		TotalCacheRead:     200,
		TotalReasoning:     50,
		TotalEstimatedCost: 1.234,
	}
	out := windowMetrics(window{Days: 1, Slug: "daily", Label: "DAY"}, totals, now)
	if len(out) != 5 {
		t.Fatalf("expected 5 metrics per window, got %d", len(out))
	}
	// total_tokens emitted as input + output + cache_read + reasoning.
	const wantTotal = 1000 + 500 + 200 + 50
	for _, m := range out {
		if m.ID == "hermes-agent-total-tokens-daily" {
			if got := m.NumericVal(); got != wantTotal {
				t.Errorf("total tokens NumericVal = %v, want %d", got, wantTotal)
			}
		}
		if m.ID == "hermes-agent-cost-daily" {
			// Stored rounded to cents.
			if got := m.NumericVal(); got != 1.23 {
				t.Errorf("cost NumericVal = %v, want 1.23", got)
			}
			if v, ok := m.Value.(string); !ok || v != "$1.23" {
				t.Errorf("cost Value = %v, want \"$1.23\"", m.Value)
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
		{-1500, "-1.5k"},
	}
	for _, c := range cases {
		if got := formatCount(c.in); got != c.want {
			t.Errorf("formatCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTokenCacheKeyedByBase(t *testing.T) {
	// Reset state so the test is hermetic regardless of test ordering.
	clearCachedToken()

	setCachedToken("http://a", "tokenA")
	if got := getCachedToken("http://a"); got != "tokenA" {
		t.Errorf("getCachedToken(a) = %q, want tokenA", got)
	}
	// Different base → cache miss, even though we have a token cached
	// for some other base. Avoids the case where a user changes their
	// base URL and we keep sending the stale token.
	if got := getCachedToken("http://b"); got != "" {
		t.Errorf("getCachedToken(b) = %q, want empty", got)
	}
	clearCachedToken()
	if got := getCachedToken("http://a"); got != "" {
		t.Errorf("after clear, getCachedToken(a) = %q, want empty", got)
	}
}

func TestActiveSessionsMetric_Shape(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	m := activeSessionsMetric(3, now)
	if m.ID != "hermes-agent-active-sessions" {
		t.Errorf("ID = %q", m.ID)
	}
	if m.Label != "ACTIVE" {
		t.Errorf("Label = %q", m.Label)
	}
	if got := m.NumericVal(); got != 3 {
		t.Errorf("NumericVal = %v, want 3", got)
	}
}

package kimik2

import (
	"encoding/json"
	"testing"
	"time"
)

// TestExtractCreditsMatchesCodexBarAliases verifies CodexBar-compatible consumed/remaining aliases.
func TestExtractCreditsMatchesCodexBarAliases(t *testing.T) {
	body := map[string]any{
		"data": map[string]any{
			"usage": map[string]any{
				"total_credits_used": 12.0,
				"credits_remaining":  88.0,
			},
		},
	}

	consumed, consumedOK, remaining, remainingOK := extractCredits(body)
	if !consumedOK || !remainingOK {
		t.Fatalf("extractCredits ok = %v/%v, want true/true", consumedOK, remainingOK)
	}
	if consumed != 12 || remaining != 88 {
		t.Fatalf("extractCredits = %.0f/%.0f, want 12/88", consumed, remaining)
	}
}

// TestFindDateParsesSupportedTimestampShapes verifies flexible timestamp extraction.
func TestFindDateParsesSupportedTimestampShapes(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	tests := []struct {
		name string
		raw  any
		want time.Time
		ok   bool
	}{
		{name: "float epoch seconds", raw: float64(base.Unix()), want: base, ok: true},
		{name: "float epoch milliseconds", raw: float64(base.UnixMilli()), want: base, ok: true},
		{name: "json number seconds", raw: json.Number("1700000000"), want: base, ok: true},
		{name: "json number milliseconds", raw: json.Number("1700000000000"), want: base, ok: true},
		{name: "numeric string seconds", raw: "1700000000", want: base, ok: true},
		{name: "numeric string milliseconds", raw: "1700000000000", want: base, ok: true},
		{name: "rfc3339", raw: base.Format(time.RFC3339), want: base, ok: true},
		{name: "rfc3339 nano", raw: base.Format(time.RFC3339Nano), want: base, ok: true},
		{name: "zero", raw: float64(0), ok: false},
		{name: "negative", raw: float64(-1), ok: false},
		{name: "invalid string", raw: "nope", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findDate(map[string]any{"updatedAt": tt.raw}, timestampPaths)
			if !tt.ok {
				if got != nil {
					t.Fatalf("findDate returned %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("findDate returned nil")
			}
			if !got.Equal(tt.want) {
				t.Fatalf("findDate = %s, want %s", got.Format(time.RFC3339Nano), tt.want.Format(time.RFC3339Nano))
			}
		})
	}
}

// TestParseDateAndDateFromNumeric verify timestamp helpers directly.
func TestParseDateAndDateFromNumeric(t *testing.T) {
	want := time.Unix(1_700_000_000, 0).UTC()
	if got, ok := dateFromNumeric(float64(want.UnixMilli())); !ok || !got.Equal(want) {
		t.Fatalf("dateFromNumeric(ms) = %v/%v, want %v/true", got, ok, want)
	}
	if got, ok := parseDate(json.Number("1700000000")); !ok || !got.Equal(want) {
		t.Fatalf("parseDate(json.Number) = %v/%v, want %v/true", got, ok, want)
	}
}

// TestFindDateUsesFirstContextMatch verifies context traversal order.
func TestFindDateUsesFirstContextMatch(t *testing.T) {
	first := time.Unix(1_700_000_000, 0).UTC()
	second := time.Unix(1_800_000_000, 0).UTC()
	body := map[string]any{
		"data": map[string]any{
			"updatedAt": second.Format(time.RFC3339),
		},
		"updatedAt": first.Format(time.RFC3339),
	}
	got := findDate(body, timestampPaths)
	if got == nil {
		t.Fatal("findDate returned nil")
	}
	if !got.Equal(first) {
		t.Fatalf("findDate = %s, want first root match %s", got.Format(time.RFC3339), first.Format(time.RFC3339))
	}
}

// TestFirstInContextsUsesCodexBarAverageAliases verifies average-token alias lookup.
func TestFirstInContextsUsesCodexBarAverageAliases(t *testing.T) {
	body := map[string]any{
		"data": map[string]any{
			"usage": map[string]any{"avgTokens": 42.0},
		},
	}
	got, ok := firstInContexts(body, averageTokenPaths)
	if !ok || got != 42 {
		t.Fatalf("firstInContexts = %.0f/%v, want 42/true", got, ok)
	}
}

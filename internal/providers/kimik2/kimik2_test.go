package kimik2

import "testing"

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

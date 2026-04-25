package claude

import (
	"math"
	"testing"
)

// TestClaudeTokenCostNormalizesModelAndUsesPricing verifies normalized model IDs use exact pricing.
func TestClaudeTokenCostNormalizesModelAndUsesPricing(t *testing.T) {
	cheap := tokenCost("claude-haiku-4-5-20251001", 1_000_000, 1_000_000, 0, 0)
	expensive := tokenCost("anthropic.claude-opus-4-20250514-v1:0", 1_000_000, 1_000_000, 0, 0)
	vertex := tokenCost("claude-sonnet-4-5@20250929", 1_000_000, 1_000_000, 0, 0)

	if math.Abs(cheap-6) > 0.000001 {
		t.Fatalf("expected normalized haiku cost 6.00, got %.6f", cheap)
	}
	if math.Abs(expensive-90) > 0.000001 {
		t.Fatalf("expected normalized opus cost 90.00, got %.6f", expensive)
	}
	if math.Abs(vertex-28.5) > 0.000001 {
		t.Fatalf("expected normalized Vertex sonnet cost 28.50, got %.6f", vertex)
	}
}

// TestClaudeTokenCostAppliesSonnetLongContextTier verifies request-level long-context pricing.
func TestClaudeTokenCostAppliesSonnetLongContextTier(t *testing.T) {
	got := tokenCost("claude-sonnet-4-5", 250_000, 10_000, 0, 0)

	if want := 1.725; math.Abs(got-want) > 0.000001 {
		t.Fatalf("expected request-level long-context cost %.3f, got %.3f", want, got)
	}
}

// TestClaudeTokenCostDoesNotApplySonnet46LongContextTier verifies Sonnet 4.6 stays standard-rate.
func TestClaudeTokenCostDoesNotApplySonnet46LongContextTier(t *testing.T) {
	base := tokenCost("claude-sonnet-4-6", 200_000, 0, 0, 0)
	long := tokenCost("claude-sonnet-4-6", 201_000, 0, 0, 0)

	if got, want := long-base, 0.003; math.Abs(got-want) > 0.000001 {
		t.Fatalf("expected 1k over threshold to stay standard-rate %.3f, got %.3f", want, got)
	}
}

// TestClaudeTokenCostPricesLegacyModels verifies Claude 3.5 and Sonnet 4.0
// session entries bill at their published rates instead of silently returning
// $0 when the normalizer strips a dated suffix.
func TestClaudeTokenCostPricesLegacyModels(t *testing.T) {
	sonnet35 := tokenCost("claude-3-5-sonnet-20241022", 1_000_000, 1_000_000, 0, 0)
	haiku35 := tokenCost("claude-3-5-haiku-20241022", 1_000_000, 1_000_000, 0, 0)
	// Sonnet 4.0 shares Sonnet 4's long-context tier, so 1M input triggers
	// the above-threshold rates ($6 input, $22.5 output).
	sonnet40 := tokenCost("claude-sonnet-4-0-20250514", 1_000_000, 1_000_000, 0, 0)

	if math.Abs(sonnet35-18) > 0.000001 {
		t.Fatalf("expected claude-3-5-sonnet-20241022 cost 18.00, got %.6f", sonnet35)
	}
	if math.Abs(haiku35-4.8) > 0.000001 {
		t.Fatalf("expected claude-3-5-haiku-20241022 cost 4.80, got %.6f", haiku35)
	}
	if math.Abs(sonnet40-28.5) > 0.000001 {
		t.Fatalf("expected claude-sonnet-4-0-20250514 long-context cost 28.50, got %.6f", sonnet40)
	}
}

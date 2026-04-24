package cursor

import "testing"

// TestLegacyCursorUsagePrefersTotalRequests verifies legacy total request precedence.
func TestLegacyCursorUsagePrefersTotalRequests(t *testing.T) {
	current := 100
	total := 140
	if got := firstIntPtr(&total, &current); got != total {
		t.Fatalf("firstIntPtr returned %d, want %d", got, total)
	}
}

// TestRemainingPercentClampsInput verifies remainingPercent clamps used percentages.
func TestRemainingPercentClampsInput(t *testing.T) {
	if got := remainingPercent(135); got != 0 {
		t.Fatalf("remainingPercent(135) = %.0f, want 0", got)
	}
	if got := remainingPercent(-20); got != 100 {
		t.Fatalf("remainingPercent(-20) = %.0f, want 100", got)
	}
}

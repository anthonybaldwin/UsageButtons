package zai

import "testing"

// TestQuotaUsedAndCapMatchesCodexBarFields verifies CodexBar-compatible quota field mapping.
func TestQuotaUsedAndCapMatchesCodexBarFields(t *testing.T) {
	usage := 1000.0
	current := 250.0
	remaining := 700.0
	limit := quotaLimit{Usage: &usage, CurrentValue: &current, Remaining: &remaining}

	used, cap, rawCounts, ok := quotaUsedAndCap(limit)
	if !ok {
		t.Fatal("quotaUsedAndCap returned !ok")
	}
	if !rawCounts {
		t.Fatal("quotaUsedAndCap returned rawCounts=false, want true")
	}
	if used != 250 || cap != 1000 {
		t.Fatalf("quotaUsedAndCap = used %.0f cap %.0f, want 250/1000", used, cap)
	}
}

// TestQuotaUsedAndCapFallsBackToUsageConsumed verifies legacy limit/usage payloads keep usage as consumed.
func TestQuotaUsedAndCapFallsBackToUsageConsumed(t *testing.T) {
	limitValue := 1000.0
	usage := 250.0
	limit := quotaLimit{Limit: &limitValue, Usage: &usage}

	used, cap, rawCounts, ok := quotaUsedAndCap(limit)
	if !ok {
		t.Fatal("quotaUsedAndCap returned !ok")
	}
	if !rawCounts {
		t.Fatal("quotaUsedAndCap returned rawCounts=false, want true")
	}
	if used != 250 || cap != 1000 {
		t.Fatalf("quotaUsedAndCap = used %.0f cap %.0f, want 250/1000", used, cap)
	}
}

// TestQuotaUsedAndCapPercentageOnlySuppressesRawCounts verifies percent-only quotas do not fake counts.
func TestQuotaUsedAndCapPercentageOnlySuppressesRawCounts(t *testing.T) {
	pct := 63.0
	used, cap, rawCounts, ok := quotaUsedAndCap(quotaLimit{Percentage: &pct})
	if !ok {
		t.Fatal("quotaUsedAndCap returned !ok")
	}
	if rawCounts {
		t.Fatal("quotaUsedAndCap returned rawCounts=true, want false")
	}
	if used != 63 || cap != 100 {
		t.Fatalf("quotaUsedAndCap = used %.0f cap %.0f, want 63/100", used, cap)
	}
}

// TestTokenLimitsSortShortestWindowFirst verifies shorter quota windows sort first.
func TestTokenLimitsSortShortestWindowFirst(t *testing.T) {
	hours := 3
	five := 5
	days := 1
	seven := 7
	short := quotaLimit{Unit: &hours, Number: &five}
	long := quotaLimit{Unit: &days, Number: &seven}

	if windowMinutes(short) >= windowMinutes(long) {
		t.Fatalf("short window minutes %d should be less than long %d", windowMinutes(short), windowMinutes(long))
	}
}

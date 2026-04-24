package zai

import "testing"

func TestQuotaUsedAndCapMatchesCodexBarFields(t *testing.T) {
	usage := 1000.0
	current := 250.0
	remaining := 700.0
	limit := quotaLimit{Usage: &usage, CurrentValue: &current, Remaining: &remaining}

	used, cap, ok := quotaUsedAndCap(limit)
	if !ok {
		t.Fatal("quotaUsedAndCap returned !ok")
	}
	if used != 250 || cap != 1000 {
		t.Fatalf("quotaUsedAndCap = used %.0f cap %.0f, want 250/1000", used, cap)
	}
}

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

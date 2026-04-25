package factory

import (
	"testing"
	"time"
)

func TestBuildSnapshotMapsStandardAndPremium(t *testing.T) {
	end := int64(1893456000000)
	auth := authResponse{
		Organization: &organization{
			Name: "Example Org",
			Subscription: &subscription{
				FactoryTier: "team",
				OrbSubscription: &orbSubscription{
					Plan: &plan{Name: "Droid Pro"},
				},
			},
		},
	}
	usage := usageResponse{
		UserID: "user_123",
		Usage: &usageData{
			EndDate: &end,
			Standard: &tokenUsage{
				UserTokens:     25,
				TotalAllowance: 100,
				UsedRatio:      floatPtr(0.25),
			},
			Premium: &tokenUsage{
				UserTokens:     75,
				TotalAllowance: 100,
				UsedRatio:      floatPtr(75),
			},
		},
	}

	snap := buildSnapshot(auth, usage, "cookie")
	snap.UpdatedAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	out := snapshotFromUsage(snap)
	if out.ProviderName != "Droid Team Example Org" {
		t.Fatalf("ProviderName = %q", out.ProviderName)
	}
	if len(out.Metrics) != 2 {
		t.Fatalf("metric count = %d, want 2", len(out.Metrics))
	}
	if got := out.Metrics[0].NumericVal(); got != 75 {
		t.Fatalf("standard remaining = %v, want 75", got)
	}
	if got := out.Metrics[1].NumericVal(); got != 25 {
		t.Fatalf("premium remaining = %v, want 25", got)
	}
	if out.Metrics[0].ResetInSeconds == nil {
		t.Fatalf("standard reset timer missing")
	}
}

func TestUsagePercentHandlesUnlimitedAllowance(t *testing.T) {
	got := usagePercent(tokenUsage{
		UserTokens:     50_000_000,
		TotalAllowance: 2_000_000_000_000,
	})
	if got != 50 {
		t.Fatalf("usagePercent() = %v, want 50", got)
	}
}

func floatPtr(v float64) *float64 {
	return &v
}

package claude

import (
	"testing"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

func TestAnyStaleResetWindow(t *testing.T) {
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	rfc := func(at time.Time) *string {
		s := at.Format(time.RFC3339)
		return &s
	}
	util := 0.4
	win := func(resetAt time.Time) *usageWindow {
		return &usageWindow{Utilization: &util, ResetsAt: rfc(resetAt)}
	}

	tests := []struct {
		name string
		resp usageResponse
		want bool
	}{
		{
			name: "all windows in future",
			resp: usageResponse{
				FiveHour: win(now.Add(2 * time.Hour)),
				SevenDay: win(now.Add(3 * 24 * time.Hour)),
			},
			want: false,
		},
		{
			name: "slightly past within grace",
			resp: usageResponse{
				FiveHour: win(now.Add(-30 * time.Second)),
				SevenDay: win(now.Add(3 * 24 * time.Hour)),
			},
			want: false,
		},
		{
			name: "weekly in the past beyond grace",
			resp: usageResponse{
				FiveHour: win(now.Add(2 * time.Hour)),
				SevenDay: win(now.Add(-10 * time.Minute)),
			},
			want: true,
		},
		{
			name: "design in the past beyond grace",
			resp: usageResponse{
				FiveHour:       win(now.Add(2 * time.Hour)),
				SevenDayDesign: win(now.Add(-5 * time.Minute)),
			},
			want: true,
		},
		{
			name: "all nil windows",
			resp: usageResponse{},
			want: false,
		},
		{
			name: "unparseable resets_at ignored",
			resp: usageResponse{
				FiveHour: &usageWindow{Utilization: &util, ResetsAt: ptrString("not-a-date")},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := anyStaleResetWindow(tc.resp, now)
			if got != tc.want {
				t.Errorf("anyStaleResetWindow = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyStaleWindowMarker(t *testing.T) {
	reset := 1234.0
	makeMetrics := func() []providers.MetricValue {
		val := 60.0
		ratio := 0.6
		return []providers.MetricValue{
			{ID: "session-percent", Value: val, NumericValue: &val, Ratio: &ratio, ResetInSeconds: &reset},
			{ID: "weekly-percent", Value: val, NumericValue: &val, Ratio: &ratio, ResetInSeconds: &reset},
		}
	}

	t.Run("cookie source gets Reload Helper caption", func(t *testing.T) {
		m := makeMetrics()
		applyStaleWindowMarker(m, "cookie")
		for _, mv := range m {
			if mv.Caption != "Reload Helper" {
				t.Errorf("%s: caption = %q, want Reload Helper", mv.ID, mv.Caption)
			}
			if mv.Stale == nil || !*mv.Stale {
				t.Errorf("%s: Stale not set", mv.ID)
			}
			if mv.ResetInSeconds != nil {
				t.Errorf("%s: ResetInSeconds = %v, want nil", mv.ID, *mv.ResetInSeconds)
			}
		}
	})

	t.Run("oauth source gets generic caption", func(t *testing.T) {
		m := makeMetrics()
		applyStaleWindowMarker(m, "oauth")
		for _, mv := range m {
			if mv.Caption != "Stale data" {
				t.Errorf("%s: caption = %q, want Stale data", mv.ID, mv.Caption)
			}
		}
	})
}

func ptrString(s string) *string { return &s }

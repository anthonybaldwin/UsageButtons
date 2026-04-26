package render

import "testing"

func TestFormatCountdown(t *testing.T) {
	cases := []struct {
		name string
		secs float64
		want string
	}{
		{"sub-minute", 45, "45s"},
		{"exact-minute", 60, "1m"},
		{"sub-hour", 30 * 60, "30m"},
		{"hour-rounded-down", 60*60 - 1, "59m"},
		{"exact-hour", 60 * 60, "1h 0m"},
		{"hours-and-minutes", 2*60*60 + 5*60, "2h 5m"},
		{"just-under-day", 23*60*60 + 59*60, "23h 59m"},
		{"exact-day", 24 * 60 * 60, "1d 0h"},
		{"days-and-hours", 4*24*60*60 + 12*60*60, "4d 12h"},
		{"days-without-hour-portion", 7 * 24 * 60 * 60, "7d 0h"},
		{"just-under-week", 6*24*60*60 + 23*60*60, "6d 23h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatCountdown(tc.secs)
			if got != tc.want {
				t.Errorf("FormatCountdown(%v) = %q, want %q", tc.secs, got, tc.want)
			}
		})
	}
}

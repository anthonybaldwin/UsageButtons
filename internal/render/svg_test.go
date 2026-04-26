package render

import "testing"

func TestFormatCountdown(t *testing.T) {
	cases := []struct {
		name string
		secs float64
		want string
	}{
		// Sub-minute and minute-only cases are unchanged.
		{"sub-minute", 45, "45s"},
		{"exact-minute", 60, "1m"},
		{"sub-hour", 30 * 60, "30m"},
		{"hour-rounded-down", 60*60 - 1, "59m"},

		// Exact-hour: now drops the trailing 0m. Was "1h 0m".
		{"exact-hour", 60 * 60, "1h"},
		{"hours-and-minutes", 2*60*60 + 5*60, "2h 5m"},
		{"just-under-day", 23*60*60 + 59*60, "23h 59m"},

		// Exact-day: now drops trailing 0h. Was "1d 0h".
		{"exact-day", 24 * 60 * 60, "1d"},
		{"days-and-hours", 4*24*60*60 + 12*60*60, "4d 12h"},

		// Days with 0 hours BUT non-zero minutes — falls through to "Xd Ym"
		// rather than swallowing the minutes as "Xd 0h" / "Xd".
		{"day-plus-minutes", 24*60*60 + 30*60, "1d 30m"},
		{"two-days-plus-minutes", 2*24*60*60 + 5*60, "2d 5m"},

		// Whole-week boundary: now drops trailing 0h. Was "7d 0h".
		{"days-without-hour-portion", 7 * 24 * 60 * 60, "7d"},
		{"just-under-week", 6*24*60*60 + 23*60*60, "6d 23h"},

		// Days with non-zero hours dominates over minutes (existing behavior
		// preserved — we only carry the secondary, not a third unit).
		{"days-hours-dominate-minutes", 24*60*60 + 5*60*60 + 30*60, "1d 5h"},
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

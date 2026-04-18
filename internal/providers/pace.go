package providers

import (
	"fmt"
	"math"
	"time"
)

// PaceInput is the data a provider passes to compute a pace metric.
type PaceInput struct {
	MetricID       string        // e.g. "session-pace", "weekly-pace"
	Label          string        // e.g. "Session", "Weekly"
	Name           string        // e.g. "Session pace"
	UsedPercent    float64       // 0-100, how much has been consumed
	WindowDuration time.Duration // total window length
	ResetIn        time.Duration // time remaining until reset
}

// PaceMetric computes a linear-burn-rate pace metric.
//
// Returns nil if the input is degenerate (zero window, no elapsed time).
//
// The metric value shows the delta between actual and expected usage:
//   - Positive value = reserve (used less than expected — good)
//   - Negative value = deficit (used more than expected — bad)
//
// Pace metrics intentionally do NOT set a Ratio: mapping ±N% delta
// onto a 0..1 fill bar produces a "half-filled tile" at 0% that reads
// like a percent meter at 50% and confuses everyone. The signed value
// + caption ("On pace" / "Reserve" / "Deficit") carries the signal
// without a misleading fill.
func PaceMetric(in PaceInput) *MetricValue {
	windowSec := in.WindowDuration.Seconds()
	resetSec := in.ResetIn.Seconds()
	if windowSec <= 0 {
		return nil
	}
	if resetSec < 0 {
		resetSec = 0
	}

	elapsed := windowSec - resetSec
	if elapsed <= 0 {
		return nil // window just started, no data point
	}

	expectedUsed := (elapsed / windowSec) * 100
	actualUsed := math.Max(0, math.Min(100, in.UsedPercent))

	// delta > 0 means used MORE than expected (deficit)
	// delta < 0 means used LESS than expected (reserve)
	delta := actualUsed - expectedUsed

	// Flip sign so positive = reserve (good), negative = deficit (bad)
	reserve := -delta

	// Display value — always signed so it doesn't read as "percent
	// remaining." Near pace (< 1%) we drop to one-decimal precision
	// so e.g. "+0.3%" / "-0.7%" instead of a bare "0%".
	absReserve := math.Abs(reserve)
	var valueStr string
	switch {
	case absReserve < 0.05:
		valueStr = "±0.0%"
	case absReserve < 1:
		valueStr = fmt.Sprintf("%+.1f%%", reserve)
	default:
		valueStr = fmt.Sprintf("%+d%%", int(math.Round(reserve)))
	}

	// Caption disambiguates a pace tile from its percent sibling
	// (same top label, e.g. "SESSION"). The +/- on the value already
	// conveys direction, so a single constant word is enough.
	caption := "Pace"

	nv := reserve
	// Intentionally no ResetInSeconds: the renderer's subvalue priority
	// puts countdown above caption, which would hide the pace signal
	// ("On pace" / "Reserve" / "Deficit") behind a timer that already
	// shows on the sibling percent tile.
	return &MetricValue{
		ID:              in.MetricID,
		Label:           in.Label,
		Name:            in.Name,
		Value:           valueStr,
		NumericValue:    &nv,
		NumericUnit:     "percent",
		NumericGoodWhen: "high",
		Caption:         caption,
		UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
}

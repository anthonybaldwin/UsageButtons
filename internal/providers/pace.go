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
// Ratio is mapped 0..1 where 0.5 = on track, 1.0 = full reserve,
// 0.0 = full deficit. Clamped to [0, 1].
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

	// Ratio: 0.5 = on pace, 1.0 = 50%+ reserve, 0.0 = 50%+ deficit
	ratio := math.Max(0, math.Min(1, 0.5+(reserve/100)))

	// Display value
	roundedReserve := math.Round(reserve)
	var valueStr string
	if roundedReserve > 0 {
		valueStr = fmt.Sprintf("+%d%%", int(roundedReserve))
	} else {
		valueStr = fmt.Sprintf("%d%%", int(roundedReserve))
	}

	// Caption
	var caption string
	absReserve := math.Abs(reserve)
	switch {
	case absReserve < 2:
		caption = "On pace"
	case reserve > 0:
		caption = "Reserve"
	default:
		caption = "Deficit"
	}

	nv := reserve
	resetF := resetSec
	return &MetricValue{
		ID:              in.MetricID,
		Label:           in.Label,
		Name:            in.Name,
		Value:           valueStr,
		NumericValue:    &nv,
		NumericUnit:     "percent",
		NumericGoodWhen: "high",
		Ratio:           &ratio,
		Direction:       "up",
		Caption:         caption,
		ResetInSeconds:  &resetF,
		UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
}

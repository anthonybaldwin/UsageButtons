// Package providerutil contains small helpers shared by quota-style providers.
package providerutil

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

// NowString returns the timestamp format used by provider metrics.
func NowString() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// MissingAuthSnapshot returns a configured-but-missing-credentials snapshot.
func MissingAuthSnapshot(providerID, providerName, message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   providerID,
		ProviderName: providerName,
		Source:       "none",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// PercentRemainingMetric builds a standard remaining-percent metric from a
// used percentage.
func PercentRemainingMetric(id, label, name string, usedPct float64, resetAt *time.Time, caption string, now string) providers.MetricValue {
	remaining := 100 - math.Max(0, math.Min(100, usedPct))
	ratio := remaining / 100
	m := providers.MetricValue{
		ID:           id,
		Label:        label,
		Name:         name,
		Value:        math.Round(remaining),
		NumericValue: &remaining,
		NumericUnit:  "percent",
		Unit:         "%",
		Ratio:        &ratio,
		Direction:    "up",
		Caption:      caption,
		UpdatedAt:    now,
	}
	if resetAt != nil {
		if secs := ResetSeconds(*resetAt); secs != nil {
			m.ResetInSeconds = secs
		}
	}
	return m
}

// RawCounts attaches remaining and total counts to a metric.
func RawCounts(m providers.MetricValue, remaining, total int) providers.MetricValue {
	if total <= 0 {
		return m
	}
	if remaining < 0 {
		remaining = 0
	}
	m.RawCount = &remaining
	m.RawMax = &total
	return m
}

// ResetSeconds returns seconds from now until resetAt, clamped to zero.
func ResetSeconds(resetAt time.Time) *float64 {
	delta := resetAt.Sub(time.Now()).Seconds()
	if delta < 0 {
		delta = 0
	}
	return &delta
}

// RootMap decodes arbitrary JSON into a root object. Top-level arrays are
// wrapped as {"quotas": array} to keep provider parsers uniform.
func RootMap(body []byte) (map[string]any, error) {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	return RootMapFromAny(raw)
}

// RootMapFromAny normalizes a decoded JSON value into a root object.
func RootMapFromAny(raw any) (map[string]any, error) {
	switch v := raw.(type) {
	case map[string]any:
		return v, nil
	case []any:
		return map[string]any{"quotas": v}, nil
	default:
		return nil, fmt.Errorf("unexpected JSON root %T", raw)
	}
}

// MapValue returns v as a JSON object when possible.
func MapValue(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// NestedMap follows keys through nested JSON objects.
func NestedMap(root map[string]any, keys ...string) (map[string]any, bool) {
	cur := root
	for _, key := range keys {
		next, ok := MapValue(cur[key])
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// FirstString returns the first non-empty string-like value for keys.
func FirstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := StringValue(m[key]); s != "" {
			return s
		}
	}
	return ""
}

// StringValue converts a JSON scalar to a trimmed string.
func StringValue(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// FirstFloat returns the first numeric value for keys.
func FirstFloat(m map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if n, ok := FloatValue(m[key]); ok {
			return n, true
		}
	}
	return 0, false
}

// FloatValue converts a JSON scalar to float64.
func FloatValue(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case json.Number:
		n, err := x.Float64()
		return n, err == nil
	case string:
		clean := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(x, ",", ""), "$", ""))
		if clean == "" {
			return 0, false
		}
		n, err := strconv.ParseFloat(clean, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

// FirstTime returns the first date-like value for keys.
func FirstTime(m map[string]any, keys ...string) (*time.Time, bool) {
	for _, key := range keys {
		if t, ok := TimeValue(m[key]); ok {
			return &t, true
		}
	}
	return nil, false
}

// TimeValue converts RFC3339 strings or Unix timestamps into time.Time.
func TimeValue(v any) (time.Time, bool) {
	if n, ok := FloatValue(v); ok {
		switch {
		case n > 1_000_000_000_000:
			return time.UnixMilli(int64(n)), true
		case n > 1_000_000_000:
			return time.Unix(int64(n), 0), true
		}
	}
	s := StringValue(v)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ExtractObjects walks arbitrary JSON and returns objects accepted by match.
func ExtractObjects(v any, match func(map[string]any) bool) []map[string]any {
	switch x := v.(type) {
	case []any:
		var out []map[string]any
		for _, item := range x {
			out = append(out, ExtractObjects(item, match)...)
		}
		return out
	case map[string]any:
		if match(x) {
			return []map[string]any{x}
		}
		var out []map[string]any
		for _, item := range x {
			out = append(out, ExtractObjects(item, match)...)
		}
		return out
	default:
		return nil
	}
}

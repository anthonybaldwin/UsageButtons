// Package synthetic implements the Synthetic usage provider.
//
// Auth: Property Inspector settings field or SYNTHETIC_API_KEY /
// SYNTHETIC_API_TOKEN environment variable.
// Endpoint: GET https://api.synthetic.new/v2/quotas
package synthetic

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	quotaURL = "https://api.synthetic.new/v2/quotas"
)

// quotaEntry is one parsed Synthetic quota lane.
type quotaEntry struct {
	Label         string
	UsedPercent   float64
	WindowMinutes int
	ResetAt       *time.Time
	Remaining     *int
	Total         *int
}

// getAPIKey resolves a Synthetic API key from user settings or env vars.
func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().SyntheticKey,
		"SYNTHETIC_API_KEY", "SYNTHETIC_API_TOKEN",
	)
}

// Provider fetches Synthetic quota data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "synthetic" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Synthetic" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#141414" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#f4f4f5" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent", "search-percent"}
}

// Fetch returns the latest Synthetic quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providerutil.MissingAuthSnapshot(
			"synthetic",
			"Synthetic",
			"Enter a Synthetic API key in the Synthetic tab, or set SYNTHETIC_API_KEY.",
		), nil
	}

	var raw any
	err := httputil.GetJSON(quotaURL, map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Accept":        "application/json",
	}, 15*time.Second, &raw)
	if err != nil {
		return providers.Snapshot{}, err
	}

	root, err := providerutil.RootMapFromAny(raw)
	if err != nil {
		return providers.Snapshot{}, err
	}

	metrics, err := metricsFromRoot(root)
	if err != nil {
		return providers.Snapshot{
			ProviderID:   "synthetic",
			ProviderName: "Synthetic",
			Source:       "api-key",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        err.Error(),
		}, nil
	}

	providerName := "Synthetic"
	if plan := planName(root); plan != "" {
		providerName += " " + plan
	}
	return providers.Snapshot{
		ProviderID:   "synthetic",
		ProviderName: providerName,
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// metricsFromRoot maps Synthetic quota payloads to Stream Deck metrics.
func metricsFromRoot(root map[string]any) ([]providers.MetricValue, error) {
	now := providerutil.NowString()
	if slots, ok := prioritizedSlots(root); ok {
		ids := []struct {
			id    string
			label string
			name  string
		}{
			{"session-percent", "SESSION", "Five-hour quota remaining"},
			{"weekly-percent", "WEEKLY", "Weekly tokens remaining"},
			{"search-percent", "SEARCH", "Search hourly remaining"},
		}
		var metrics []providers.MetricValue
		for i, slot := range slots {
			if slot == nil {
				continue
			}
			q, ok := parseQuota(slot)
			if !ok {
				continue
			}
			m := quotaMetric(ids[i].id, ids[i].label, ids[i].name, q, now)
			metrics = append(metrics, m)
		}
		if len(metrics) > 0 {
			return metrics, nil
		}
	}

	objects := fallbackQuotaObjects(root)
	var metrics []providers.MetricValue
	for i, obj := range objects {
		q, ok := parseQuota(obj)
		if !ok {
			continue
		}
		id, label, name := fallbackIdentity(q, i)
		metrics = append(metrics, quotaMetric(id, label, name, q, now))
	}
	if len(metrics) == 0 {
		return nil, fmt.Errorf("Synthetic response missing quota data")
	}
	return metrics, nil
}

// prioritizedSlots returns Synthetic's known lanes as [5-hour, weekly, search].
func prioritizedSlots(root map[string]any) ([]map[string]any, bool) {
	data, _ := providerutil.MapValue(root["data"])
	rolling := namedQuota(root["rollingFiveHourLimit"], "Rolling five-hour limit")
	if rolling == nil {
		rolling = namedQuota(data["rollingFiveHourLimit"], "Rolling five-hour limit")
	}
	weekly := namedQuota(root["weeklyTokenLimit"], "Weekly token limit")
	if weekly == nil {
		weekly = namedQuota(data["weeklyTokenLimit"], "Weekly token limit")
	}
	search := namedQuota(nestedValue(root, "search", "hourly"), "Search hourly")
	if search == nil {
		search = namedQuota(nestedValue(data, "search", "hourly"), "Search hourly")
	}
	slots := []map[string]any{rolling, weekly, search}
	return slots, rolling != nil || weekly != nil || search != nil
}

// nestedValue follows keys through JSON objects and returns the final value.
func nestedValue(root map[string]any, keys ...string) any {
	if root == nil {
		return nil
	}
	var cur any = root
	for _, key := range keys {
		m, ok := providerutil.MapValue(cur)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

// namedQuota returns a quota payload with a fallback label.
func namedQuota(raw any, label string) map[string]any {
	m, ok := providerutil.MapValue(raw)
	if !ok || !isQuotaPayload(m) {
		return nil
	}
	if providerutil.FirstString(m, "label", "name") == "" {
		m["label"] = label
	}
	return m
}

// fallbackQuotaObjects extracts quota-shaped objects from less-known payloads.
func fallbackQuotaObjects(root map[string]any) []map[string]any {
	data, _ := providerutil.MapValue(root["data"])
	candidates := []any{
		root["quotas"], root["quota"], root["limits"], root["usage"],
		root["entries"], root["subscription"], root["data"],
		data["quotas"], data["quota"], data["limits"], data["usage"],
		data["entries"], data["subscription"],
	}
	for _, candidate := range candidates {
		objects := providerutil.ExtractObjects(candidate, isQuotaPayload)
		if len(objects) > 0 {
			return objects
		}
	}
	return nil
}

// isQuotaPayload reports whether m has enough quota fields to parse.
func isQuotaPayload(m map[string]any) bool {
	keySets := [][]string{percentUsedKeys, percentRemainingKeys, limitKeys, usedKeys, remainingKeys}
	for _, keys := range keySets {
		if _, ok := providerutil.FirstFloat(m, keys...); ok {
			return true
		}
	}
	return false
}

// parseQuota converts one JSON quota object to a quotaEntry.
func parseQuota(m map[string]any) (quotaEntry, bool) {
	q := quotaEntry{
		Label:         providerutil.FirstString(m, labelKeys...),
		WindowMinutes: windowMinutes(m),
	}
	if resetAt, ok := providerutil.FirstTime(m, resetKeys...); ok {
		q.ResetAt = resetAt
	}

	usedPct, ok := normalizedPercent(providerutil.FirstFloat(m, percentUsedKeys...))
	if !ok {
		if remainingPct, found := normalizedPercent(providerutil.FirstFloat(m, percentRemainingKeys...)); found {
			usedPct = 100 - remainingPct
			ok = true
		}
	}

	limit, hasLimit := providerutil.FirstFloat(m, limitKeys...)
	used, hasUsed := providerutil.FirstFloat(m, usedKeys...)
	remaining, hasRemaining := providerutil.FirstFloat(m, remainingKeys...)
	if !ok {
		if !hasLimit && hasUsed && hasRemaining {
			limit = used + remaining
			hasLimit = true
		}
		if !hasUsed && hasLimit && hasRemaining {
			used = limit - remaining
			hasUsed = true
		}
		if hasLimit && hasUsed && limit > 0 {
			usedPct = used / limit * 100
			ok = true
		}
	}
	if !ok {
		return quotaEntry{}, false
	}
	q.UsedPercent = math.Max(0, math.Min(100, usedPct))
	if hasLimit && limit > 0 {
		total := int(math.Round(limit))
		if !hasRemaining && hasUsed {
			remaining = math.Max(0, limit-used)
			hasRemaining = true
		}
		if hasRemaining {
			rem := int(math.Round(math.Max(0, remaining)))
			q.Remaining = &rem
		}
		q.Total = &total
	}
	return q, true
}

// quotaMetric turns a parsed Synthetic lane into a Stream Deck metric.
// Caption is left empty so the SD subtext falls through to the
// reset-time countdown when usage has started, or the "Remaining"
// fallback when the window is still idle — same convention as Claude
// and Codex. The window length is implicit from the SESSION / WEEKLY /
// SEARCH label.
func quotaMetric(id, label, name string, q quotaEntry, now string) providers.MetricValue {
	m := providerutil.PercentRemainingMetric(id, label, name, q.UsedPercent, q.ResetAt, "", now)
	if q.Remaining != nil && q.Total != nil {
		m = providerutil.RawCounts(m, *q.Remaining, *q.Total)
	}
	return m
}

// fallbackIdentity assigns stable IDs to unrecognized quota lanes.
func fallbackIdentity(q quotaEntry, index int) (id, label, name string) {
	base := strings.TrimSpace(strings.ToLower(q.Label))
	switch {
	case strings.Contains(base, "week"):
		return "weekly-percent", "WEEKLY", "Weekly quota remaining"
	case strings.Contains(base, "search"):
		return "search-percent", "SEARCH", "Search quota remaining"
	case strings.Contains(base, "five") || strings.Contains(base, "5"):
		return "session-percent", "SESSION", "Five-hour quota remaining"
	}
	if base == "" {
		base = fmt.Sprintf("quota-%d", index+1)
	}
	slug := strings.NewReplacer("_", "-", " ", "-").Replace(base)
	return slug + "-percent", strings.ToUpper(strings.ReplaceAll(base, "-", " ")), q.Label + " remaining"
}

// planName extracts a displayable Synthetic plan label.
func planName(root map[string]any) string {
	if v := providerutil.FirstString(root, planKeys...); v != "" {
		return v
	}
	if data, ok := providerutil.MapValue(root["data"]); ok {
		return providerutil.FirstString(data, planKeys...)
	}
	return ""
}

// normalizedPercent converts 0..1 or 0..100 percentages to 0..100.
func normalizedPercent(v float64, ok bool) (float64, bool) {
	if !ok {
		return 0, false
	}
	if v <= 1 {
		return v * 100, true
	}
	return v, true
}

// windowMinutes extracts a quota window in minutes.
func windowMinutes(m map[string]any) int {
	if n, ok := providerutil.FirstFloat(m, windowMinutesKeys...); ok {
		return int(math.Round(n))
	}
	if n, ok := providerutil.FirstFloat(m, windowHoursKeys...); ok {
		return int(math.Round(n * 60))
	}
	if n, ok := providerutil.FirstFloat(m, windowDaysKeys...); ok {
		return int(math.Round(n * 24 * 60))
	}
	if n, ok := providerutil.FirstFloat(m, windowSecondsKeys...); ok {
		return int(math.Round(n / 60))
	}
	if s := providerutil.FirstString(m, windowStringKeys...); s != "" {
		return windowMinutesText(s)
	}
	return 0
}

// windowMinutesText parses compact durations like "5hr" or "2 days".
func windowMinutesText(text string) int {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(text), " ", ""))
	for _, item := range []struct {
		suffix string
		mult   float64
	}{
		{"minutes", 1}, {"minute", 1}, {"mins", 1}, {"min", 1}, {"m", 1},
		{"hours", 60}, {"hour", 60}, {"hrs", 60}, {"hr", 60}, {"h", 60},
		{"days", 24 * 60}, {"day", 24 * 60}, {"d", 24 * 60},
	} {
		if strings.HasSuffix(normalized, item.suffix) {
			raw := strings.TrimSuffix(normalized, item.suffix)
			var value float64
			if _, err := fmt.Sscanf(raw, "%f", &value); err == nil && value > 0 {
				return int(math.Round(value * item.mult))
			}
		}
	}
	return 0
}

// windowDescription formats a known quota window.
func windowDescription(minutes int) string {
	if minutes <= 0 {
		return ""
	}
	if minutes%(24*60) == 0 {
		days := minutes / (24 * 60)
		if days == 1 {
			return "1 day window"
		}
		return fmt.Sprintf("%d days window", days)
	}
	if minutes%60 == 0 {
		hours := minutes / 60
		if hours == 1 {
			return "1 hour window"
		}
		return fmt.Sprintf("%d hours window", hours)
	}
	if minutes == 1 {
		return "1 minute window"
	}
	return fmt.Sprintf("%d minutes window", minutes)
}

var (
	planKeys = []string{
		"plan", "planName", "plan_name", "subscription",
		"subscriptionPlan", "tier", "package", "packageName",
	}
	labelKeys = []string{
		"name", "label", "type", "period", "scope", "title", "id",
	}
	percentUsedKeys = []string{
		"percentUsed", "usedPercent", "usagePercent", "usage_percent",
		"used_percent", "percent_used", "percent",
	}
	percentRemainingKeys = []string{
		"percentRemaining", "remainingPercent", "remaining_percent", "percent_remaining",
	}
	limitKeys = []string{
		"limit", "messageLimit", "message_limit", "messages", "maxRequests",
		"max_requests", "requestLimit", "request_limit", "quota", "max",
		"total", "capacity", "allowance",
	}
	usedKeys = []string{
		"used", "usage", "usedMessages", "used_messages", "messagesUsed",
		"messages_used", "requests", "requestCount", "request_count", "consumed", "spent",
	}
	remainingKeys = []string{"remaining", "left", "available", "balance"}
	resetKeys     = []string{
		"resetAt", "reset_at", "resetsAt", "resets_at", "renewAt", "renew_at",
		"renewsAt", "renews_at", "nextTickAt", "next_tick_at", "nextRegenAt",
		"next_regen_at", "periodEnd", "period_end", "expiresAt", "expires_at",
		"endAt", "end_at",
	}
	windowMinutesKeys = []string{"windowMinutes", "window_minutes", "periodMinutes", "period_minutes"}
	windowHoursKeys   = []string{"windowHours", "window_hours", "periodHours", "period_hours"}
	windowDaysKeys    = []string{"windowDays", "window_days", "periodDays", "period_days"}
	windowSecondsKeys = []string{"windowSeconds", "window_seconds", "periodSeconds", "period_seconds"}
	windowStringKeys  = []string{"window", "windowLabel", "window_label", "period", "periodLabel", "period_label"}
)

// init registers the Synthetic provider with the package registry.
func init() {
	providers.Register(Provider{})
}

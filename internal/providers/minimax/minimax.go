// Package minimax implements the MiniMax coding-plan usage provider.
//
// Auth: MiniMax API key from Property Inspector or MINIMAX_API_KEY, falling
// back to the Usage Buttons Helper extension with the user's MiniMax browser
// session. Endpoint: /v1/api/openplatform/coding_plan/remains.
package minimax

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const remainsPath = "/v1/api/openplatform/coding_plan/remains"

var (
	errUnauthorized = errors.New("minimax unauthorized")
	errMissingUsage = errors.New("MiniMax response missing coding plan data")
)

// endpointSet is the platform and API host pair for one MiniMax region.
type endpointSet struct {
	Name         string
	PlatformBase string
	APIBase      string
}

// usageSnapshot is the parsed MiniMax coding-plan state.
type usageSnapshot struct {
	PlanName         string
	TotalPrompts     *float64
	RemainingPrompts *float64
	UsedPrompts      *float64
	UsedPercent      *float64
	WindowMinutes    *int
	ResetsAt         *time.Time
	UpdatedAt        time.Time
}

// Provider fetches MiniMax usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "minimax" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "MiniMax" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#fe603c" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#111214" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent"}
}

// Fetch returns the latest MiniMax coding-plan usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	if token := configuredAPIKey(); token != "" {
		usage, err := fetchWithAPIKey(token)
		if err == nil {
			return snapshotFromUsage(usage, "api-key"), nil
		}
		if errors.Is(err, errUnauthorized) {
			return errorSnapshot("MiniMax API key is invalid or expired. Check MINIMAX_API_KEY."), nil
		}
		return providers.Snapshot{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("minimax.io")), nil
	}
	usage, err := fetchWithBrowser(ctx)
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			return errorSnapshot(cookieaux.StaleMessage("minimax.io")), nil
		}
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage, "cookie"), nil
}

// configuredAPIKey resolves a MiniMax API key from settings or env.
func configuredAPIKey() string {
	return settings.ResolveAPIKey(settings.ProviderKeysGet().MiniMaxKey, "MINIMAX_API_KEY")
}

// configuredRegion returns the requested MiniMax region.
func configuredRegion() string {
	raw := strings.TrimSpace(settings.ProviderKeysGet().MiniMaxRegion)
	if raw == "" {
		raw = strings.TrimSpace(firstEnv("MINIMAX_REGION", "MINIMAX_HOST"))
	}
	return strings.ToLower(raw)
}

// firstEnv returns the first non-empty environment variable.
func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// getenv is isolated for tests and keeps env lookup close to settings helpers.
var getenv = func(name string) string {
	return strings.TrimSpace(settings.ResolveAPIKey("", name))
}

// fetchWithAPIKey calls MiniMax's API-host remains endpoint.
func fetchWithAPIKey(token string) (usageSnapshot, error) {
	var lastErr error
	for _, endpoints := range endpointOrder() {
		var raw any
		err := httputil.GetJSON(apiRemainsURL(endpoints.APIBase), map[string]string{
			"Authorization": "Bearer " + token,
			"Accept":        "application/json",
			"Content-Type":  "application/json",
			"MM-API-Source": "UsageButtons",
		}, 20*time.Second, &raw)
		if err != nil {
			var httpErr *httputil.Error
			if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
				lastErr = errUnauthorized
				continue
			}
			return usageSnapshot{}, err
		}
		usage, err := parseUsage(raw, time.Now().UTC())
		if err == nil {
			return usage, nil
		}
		if errors.Is(err, errUnauthorized) {
			lastErr = errUnauthorized
			continue
		}
		return usageSnapshot{}, err
	}
	if lastErr != nil {
		return usageSnapshot{}, lastErr
	}
	return usageSnapshot{}, errMissingUsage
}

// fetchWithBrowser calls MiniMax through the Helper extension using browser cookies.
func fetchWithBrowser(ctx context.Context) (usageSnapshot, error) {
	var lastErr error
	for _, endpoints := range endpointOrder() {
		resp, err := cookies.Fetch(ctx, cookies.Request{
			URL:    apiRemainsURL(endpoints.PlatformBase),
			Method: "GET",
			Headers: map[string]string{
				"Accept":           "application/json, text/plain, */*",
				"Origin":           endpoints.PlatformBase,
				"Referer":          endpoints.PlatformBase + "/user-center/payment/coding-plan?cycle_type=3",
				"User-Agent":       httputil.DefaultUserAgent,
				"X-Requested-With": "XMLHttpRequest",
			},
		})
		if err != nil {
			return usageSnapshot{}, err
		}
		if resp.Status < 200 || resp.Status >= 300 {
			if resp.Status == 401 || resp.Status == 403 {
				lastErr = errUnauthorized
				continue
			}
			return usageSnapshot{}, &httputil.Error{
				Status:     resp.Status,
				StatusText: resp.StatusText,
				Body:       string(resp.Body),
				URL:        apiRemainsURL(endpoints.PlatformBase),
			}
		}
		root, err := providerutil.RootMap(resp.Body)
		if err != nil {
			return usageSnapshot{}, fmt.Errorf("invalid MiniMax JSON: %w", err)
		}
		usage, err := parseUsage(root, time.Now().UTC())
		if err == nil {
			return usage, nil
		}
		if errors.Is(err, errUnauthorized) {
			lastErr = errUnauthorized
			continue
		}
		return usageSnapshot{}, err
	}
	if lastErr != nil {
		return usageSnapshot{}, lastErr
	}
	return usageSnapshot{}, errMissingUsage
}

// endpointOrder returns region endpoints in retry order.
func endpointOrder() []endpointSet {
	global := endpointSet{
		Name:         "global",
		PlatformBase: "https://platform.minimax.io",
		APIBase:      "https://api.minimax.io",
	}
	china := endpointSet{
		Name:         "cn",
		PlatformBase: "https://platform.minimaxi.com",
		APIBase:      "https://api.minimaxi.com",
	}
	switch configuredRegion() {
	case "cn", "china", "china-mainland", "mainland", "platform.minimaxi.com", "api.minimaxi.com":
		return []endpointSet{china}
	case "global", "platform.minimax.io", "api.minimax.io":
		return []endpointSet{global, china}
	default:
		return []endpointSet{global, china}
	}
}

// apiRemainsURL builds a remains endpoint URL.
func apiRemainsURL(base string) string {
	u, err := url.Parse(strings.TrimRight(base, "/") + remainsPath)
	if err != nil {
		return strings.TrimRight(base, "/") + remainsPath
	}
	return u.String()
}

// parseUsage extracts coding-plan fields from MiniMax JSON.
func parseUsage(raw any, now time.Time) (usageSnapshot, error) {
	root, err := providerutil.RootMapFromAny(raw)
	if err != nil {
		return usageSnapshot{}, err
	}
	if err := statusError(root); err != nil {
		return usageSnapshot{}, err
	}
	data := root
	if nested, ok := providerutil.MapValue(root["data"]); ok {
		data = nested
		if err := statusError(data); err != nil {
			return usageSnapshot{}, err
		}
	}
	model := firstModelRemains(data)
	if model == nil {
		if found := findPayloadWithModelRemains(root); found != nil {
			data = found
			model = firstModelRemains(found)
		}
	}
	if model == nil {
		return usageSnapshot{}, errMissingUsage
	}
	total := firstFloatPtr(model, "current_interval_total_count", "currentIntervalTotalCount", "total", "limit")
	remaining := firstFloatPtr(model, "current_interval_usage_count", "currentIntervalUsageCount", "remaining", "remain")
	used := firstFloatPtr(model, "used", "current", "current_count", "currentCount")
	if used == nil && total != nil && remaining != nil {
		v := math.Max(0, *total-*remaining)
		used = &v
	}
	if remaining == nil && total != nil && used != nil {
		v := math.Max(0, *total-*used)
		remaining = &v
	}
	var usedPct *float64
	if total != nil && *total > 0 && used != nil {
		v := math.Max(0, math.Min(100, *used / *total * 100))
		usedPct = &v
	}
	if usedPct == nil {
		usedPct = firstFloatPtr(model, "used_percent", "usedPercent", "usage_percent", "usagePercent")
	}
	reset := resetTime(model, now)
	window := windowMinutes(model)
	return usageSnapshot{
		PlanName:         planName(data),
		TotalPrompts:     total,
		RemainingPrompts: remaining,
		UsedPrompts:      used,
		UsedPercent:      usedPct,
		WindowMinutes:    window,
		ResetsAt:         reset,
		UpdatedAt:        now,
	}, nil
}

// statusError maps MiniMax base response status to errors.
func statusError(m map[string]any) error {
	base := m
	if nested, ok := providerutil.MapValue(m["base_resp"]); ok {
		base = nested
	} else if nested, ok := providerutil.MapValue(m["baseResp"]); ok {
		base = nested
	}
	status, ok := providerutil.FirstFloat(base, "status_code", "statusCode", "code")
	if !ok || int(status) == 0 {
		return nil
	}
	msg := providerutil.FirstString(base, "status_msg", "statusMessage", "message", "msg")
	lower := strings.ToLower(msg)
	if int(status) == 1004 || strings.Contains(lower, "cookie") ||
		strings.Contains(lower, "login") || strings.Contains(lower, "log in") {
		return errUnauthorized
	}
	return fmt.Errorf("MiniMax API error: %s", defStr(msg, fmt.Sprintf("status_code %.0f", status)))
}

// firstModelRemains returns the first model_remains object.
func firstModelRemains(data map[string]any) map[string]any {
	for _, key := range []string{"model_remains", "modelRemains"} {
		values, ok := data[key].([]any)
		if !ok || len(values) == 0 {
			continue
		}
		if m, ok := providerutil.MapValue(values[0]); ok {
			return m
		}
	}
	return nil
}

// findPayloadWithModelRemains searches nested JSON for MiniMax usage payloads.
func findPayloadWithModelRemains(value any) map[string]any {
	switch v := value.(type) {
	case map[string]any:
		if firstModelRemains(v) != nil {
			return v
		}
		for _, item := range v {
			if found := findPayloadWithModelRemains(item); found != nil {
				return found
			}
		}
	case []any:
		for _, item := range v {
			if found := findPayloadWithModelRemains(item); found != nil {
				return found
			}
		}
	}
	return nil
}

// resetTime returns the reset time from end time or remains seconds.
func resetTime(model map[string]any, now time.Time) *time.Time {
	if end := epochTime(firstFloatPtr(model, "end_time", "endTime")); end != nil && end.After(now) {
		return end
	}
	remaining := firstFloatPtr(model, "remains_time", "remainsTime")
	if remaining == nil || *remaining <= 0 {
		return nil
	}
	seconds := *remaining
	if seconds > 1_000_000 {
		seconds /= 1000
	}
	t := now.Add(time.Duration(seconds) * time.Second)
	return &t
}

// windowMinutes returns the quota window length in minutes.
func windowMinutes(model map[string]any) *int {
	start := epochTime(firstFloatPtr(model, "start_time", "startTime"))
	end := epochTime(firstFloatPtr(model, "end_time", "endTime"))
	if start == nil || end == nil || !end.After(*start) {
		return nil
	}
	minutes := int(end.Sub(*start).Minutes())
	if minutes <= 0 {
		return nil
	}
	return &minutes
}

// epochTime converts seconds or milliseconds since epoch to time.
func epochTime(raw *float64) *time.Time {
	if raw == nil || *raw <= 0 {
		return nil
	}
	value := *raw
	var t time.Time
	if value > 1_000_000_000_000 {
		t = time.UnixMilli(int64(value))
	} else if value > 1_000_000_000 {
		t = time.Unix(int64(value), 0)
	} else {
		return nil
	}
	return &t
}

// planName returns a displayable MiniMax plan title.
func planName(data map[string]any) string {
	if currentCombo, ok := providerutil.MapValue(data["current_combo_card"]); ok {
		if title := providerutil.FirstString(currentCombo, "title"); title != "" {
			return title
		}
	}
	for _, candidate := range []string{
		providerutil.FirstString(data, "current_subscribe_title", "currentSubscribeTitle"),
		providerutil.FirstString(data, "plan_name", "planName"),
		providerutil.FirstString(data, "combo_title", "comboTitle"),
		providerutil.FirstString(data, "current_plan_title", "currentPlanTitle"),
	} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

// firstFloatPtr returns a non-negative pointer for the first numeric key.
func firstFloatPtr(m map[string]any, keys ...string) *float64 {
	if v, ok := providerutil.FirstFloat(m, keys...); ok {
		v = math.Max(0, v)
		return &v
	}
	return nil
}

// snapshotFromUsage maps MiniMax usage into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot, source string) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	usedPct := 0.0
	if usage.UsedPercent != nil {
		usedPct = *usage.UsedPercent
	}
	caption := usage.PlanName
	if caption == "" {
		caption = windowDescription(usage.WindowMinutes)
	}
	if caption == "" {
		caption = "Coding Plan"
	}
	metric := providerutil.PercentRemainingMetric(
		"session-percent",
		"PROMPTS",
		"MiniMax coding prompts remaining",
		usedPct,
		usage.ResetsAt,
		caption,
		now,
	)
	if usage.RemainingPrompts != nil && usage.TotalPrompts != nil {
		rawCount := int(math.Round(*usage.RemainingPrompts))
		rawMax := int(math.Round(*usage.TotalPrompts))
		metric.RawCount = &rawCount
		metric.RawMax = &rawMax
	}
	return providers.Snapshot{
		ProviderID:   "minimax",
		ProviderName: "MiniMax",
		Source:       source,
		Metrics:      []providers.MetricValue{metric},
		Status:       "operational",
	}
}

// windowDescription formats a MiniMax quota window.
func windowDescription(minutes *int) string {
	if minutes == nil || *minutes <= 0 {
		return ""
	}
	if *minutes%(24*60) == 0 {
		days := *minutes / (24 * 60)
		return fmt.Sprintf("%dd window", days)
	}
	if *minutes%60 == 0 {
		hours := *minutes / 60
		return fmt.Sprintf("%dh window", hours)
	}
	return fmt.Sprintf("%dm window", *minutes)
}

// errorSnapshot returns a MiniMax setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "minimax",
		ProviderName: "MiniMax",
		Source:       "auth",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// defStr returns fallback when value is empty.
func defStr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// init registers the MiniMax provider with the package registry.
func init() {
	providers.Register(Provider{})
}

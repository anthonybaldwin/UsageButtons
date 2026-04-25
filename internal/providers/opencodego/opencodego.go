// Package opencodego implements the OpenCode Go usage provider.
//
// Auth: Usage Buttons Helper extension with the user's opencode.ai browser
// session. Endpoint: https://opencode.ai/workspace/{workspace}/go.
package opencodego

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/opencode"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const baseURL = "https://opencode.ai"

// usageSnapshot is OpenCode Go rolling, weekly, and optional monthly usage.
type usageSnapshot struct {
	HasMonthlyUsage     bool
	RollingUsagePercent float64
	WeeklyUsagePercent  float64
	MonthlyUsagePercent float64
	RollingResetInSec   int
	WeeklyResetInSec    int
	MonthlyResetInSec   int
	UpdatedAt           time.Time
}

// windowCandidate is one parsed usage window from flexible JSON.
type windowCandidate struct {
	Percent    float64
	ResetInSec int
	PathLower  string
}

// parsedWindow is one quota window parsed from a JSON object.
type parsedWindow struct {
	Percent    float64
	ResetInSec int
}

// Provider fetches OpenCode Go usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "opencodego" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "OpenCode Go" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#3b82f6" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#081a33" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent", "monthly-percent"}
}

// Fetch returns the latest OpenCode Go usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("opencode.ai")), nil
	}
	workspaceID, err := opencode.WorkspaceID(ctx, "CODEXBAR_OPENCODEGO_WORKSPACE_ID")
	if err != nil {
		return errorSnapshot(err.Error()), nil
	}
	text, err := fetchUsagePage(ctx, workspaceID)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot(cookieaux.StaleMessage("opencode.ai")), nil
		}
		if looksSignedOut(err.Error()) {
			return errorSnapshot(cookieaux.StaleMessage("opencode.ai")), nil
		}
		return errorSnapshot(err.Error()), nil
	}
	if looksSignedOut(text) {
		return errorSnapshot(cookieaux.StaleMessage("opencode.ai")), nil
	}
	usage, err := parseSubscription(text, time.Now().UTC())
	if err != nil {
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage), nil
}

// fetchUsagePage fetches the workspace Go usage page.
func fetchUsagePage(ctx context.Context, workspaceID string) (string, error) {
	rawURL := fmt.Sprintf("%s/workspace/%s/go", baseURL, workspaceID)
	resp, err := cookies.Fetch(ctx, cookies.Request{
		URL:    rawURL,
		Method: "GET",
		Headers: map[string]string{
			"Accept":     "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			"User-Agent": httputil.DefaultUserAgent,
		},
	})
	if err != nil {
		return "", err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return "", &httputil.Error{
			Status:     resp.Status,
			StatusText: resp.StatusText,
			Body:       string(resp.Body),
			URL:        rawURL,
		}
	}
	return string(resp.Body), nil
}

// parseSubscription parses rolling, weekly, and optional monthly usage.
func parseSubscription(text string, now time.Time) (usageSnapshot, error) {
	if usage, ok := parseSubscriptionJSON(text, now); ok {
		return usage, nil
	}
	rollingPercent := extractFloat(`rollingUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`, text)
	rollingReset := extractInt(`rollingUsage[^}]*?resetInSec\s*:\s*([0-9]+)`, text)
	weeklyPercent := extractFloat(`weeklyUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`, text)
	weeklyReset := extractInt(`weeklyUsage[^}]*?resetInSec\s*:\s*([0-9]+)`, text)
	if rollingPercent == nil || rollingReset == nil || weeklyPercent == nil || weeklyReset == nil {
		return usageSnapshot{}, fmt.Errorf("OpenCode Go parse error: missing usage fields")
	}
	monthlyPercent := extractFloat(`monthlyUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`, text)
	monthlyReset := extractInt(`monthlyUsage[^}]*?resetInSec\s*:\s*([0-9]+)`, text)
	usage := usageSnapshot{
		HasMonthlyUsage:     monthlyPercent != nil || monthlyReset != nil,
		RollingUsagePercent: clampPercent(*rollingPercent),
		WeeklyUsagePercent:  clampPercent(*weeklyPercent),
		RollingResetInSec:   *rollingReset,
		WeeklyResetInSec:    *weeklyReset,
		UpdatedAt:           now,
	}
	if monthlyPercent != nil {
		usage.MonthlyUsagePercent = clampPercent(*monthlyPercent)
	}
	if monthlyReset != nil {
		usage.MonthlyResetInSec = *monthlyReset
	}
	return usage, nil
}

// parseSubscriptionJSON parses flexible JSON usage payloads.
func parseSubscriptionJSON(text string, now time.Time) (usageSnapshot, bool) {
	var raw any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &raw); err != nil {
		return usageSnapshot{}, false
	}
	var candidates []windowCandidate
	collectWindowCandidates(raw, now, nil, &candidates)
	if len(candidates) == 0 {
		return usageSnapshot{}, false
	}
	rolling := pickWindow(candidates, true, "rolling", "hour", "5h", "5-hour")
	weekly := pickWindow(candidates, false, "weekly", "week")
	monthly := pickWindow(candidates, false, "monthly", "month")
	if rolling == nil {
		rolling = pickAnyWindow(candidates, true, nil)
	}
	if weekly == nil {
		weekly = pickAnyWindow(candidates, false, rolling)
	}
	if rolling == nil || weekly == nil {
		return usageSnapshot{}, false
	}
	usage := usageSnapshot{
		HasMonthlyUsage:     monthly != nil,
		RollingUsagePercent: rolling.Percent,
		WeeklyUsagePercent:  weekly.Percent,
		RollingResetInSec:   rolling.ResetInSec,
		WeeklyResetInSec:    weekly.ResetInSec,
		UpdatedAt:           now,
	}
	if monthly != nil {
		usage.MonthlyUsagePercent = monthly.Percent
		usage.MonthlyResetInSec = monthly.ResetInSec
	}
	return usage, true
}

// collectWindowCandidates finds quota-like objects in arbitrary JSON.
func collectWindowCandidates(value any, now time.Time, path []string, out *[]windowCandidate) {
	switch v := value.(type) {
	case map[string]any:
		if window, ok := parseWindow(v, now); ok {
			*out = append(*out, windowCandidate{
				Percent:    window.Percent,
				ResetInSec: window.ResetInSec,
				PathLower:  strings.ToLower(strings.Join(path, ".")),
			})
		}
		for key, item := range v {
			collectWindowCandidates(item, now, append(path, key), out)
		}
	case []any:
		for i, item := range v {
			collectWindowCandidates(item, now, append(path, fmt.Sprintf("[%d]", i)), out)
		}
	}
}

// parseWindow extracts percent and reset data from a JSON object.
func parseWindow(m map[string]any, now time.Time) (parsedWindow, bool) {
	percent, ok := providerutil.FirstFloat(m,
		"usagePercent", "usedPercent", "percentUsed", "percent",
		"usage_percent", "used_percent", "utilization",
		"utilizationPercent", "utilization_percent", "usage")
	if !ok {
		used, usedOK := providerutil.FirstFloat(m, "used", "usage", "consumed", "count", "usedTokens")
		limit, limitOK := providerutil.FirstFloat(m, "limit", "total", "quota", "max", "cap", "tokenLimit")
		if usedOK && limitOK && limit > 0 {
			percent = used / limit * 100
			ok = true
		}
	}
	if !ok {
		return parsedWindow{}, false
	}
	reset, resetOK := providerutil.FirstFloat(m,
		"resetInSec", "resetInSeconds", "resetSeconds", "reset_sec",
		"reset_in_sec", "resetsInSec", "resetsInSeconds", "resetIn", "resetSec")
	if !resetOK {
		if resetAt, ok := providerutil.FirstTime(m,
			"resetAt", "resetsAt", "reset_at", "resets_at",
			"nextReset", "next_reset", "renewAt", "renew_at"); ok {
			reset = math.Max(0, resetAt.Sub(now).Seconds())
			resetOK = true
		}
	}
	if !resetOK {
		reset = 0
	}
	return parsedWindow{
		Percent:    clampPercent(percent),
		ResetInSec: int(math.Round(reset)),
	}, true
}

// pickWindow chooses a candidate matching one of the path hints.
func pickWindow(candidates []windowCandidate, pickShorter bool, hints ...string) *windowCandidate {
	var filtered []windowCandidate
	for _, candidate := range candidates {
		for _, hint := range hints {
			if strings.Contains(candidate.PathLower, hint) {
				filtered = append(filtered, candidate)
				break
			}
		}
	}
	return pickAnyWindow(filtered, pickShorter, nil)
}

// pickAnyWindow chooses by shortest or longest reset.
func pickAnyWindow(candidates []windowCandidate, pickShorter bool, excluding *windowCandidate) *windowCandidate {
	var picked *windowCandidate
	for _, candidate := range candidates {
		if excluding != nil && candidate.PathLower == excluding.PathLower && candidate.ResetInSec == excluding.ResetInSec {
			continue
		}
		c := candidate
		if picked == nil {
			picked = &c
			continue
		}
		if pickShorter {
			if candidate.ResetInSec < picked.ResetInSec {
				picked = &c
			}
		} else if candidate.ResetInSec > picked.ResetInSec {
			picked = &c
		}
	}
	return picked
}

// snapshotFromUsage maps parsed OpenCode Go usage into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	metrics := []providers.MetricValue{
		percentMetric("session-percent", "5-HOUR", "OpenCode Go five-hour usage remaining", usage.RollingUsagePercent, usage.RollingResetInSec, "5h window", now),
		percentMetric("weekly-percent", "WEEKLY", "OpenCode Go weekly usage remaining", usage.WeeklyUsagePercent, usage.WeeklyResetInSec, "7d window", now),
	}
	if usage.HasMonthlyUsage {
		metrics = append(metrics, percentMetric("monthly-percent", "MONTHLY", "OpenCode Go monthly usage remaining", usage.MonthlyUsagePercent, usage.MonthlyResetInSec, "30d window", now))
	}
	return providers.Snapshot{
		ProviderID:   "opencodego",
		ProviderName: "OpenCode Go",
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// percentMetric builds a remaining-percent OpenCode Go metric.
func percentMetric(id, label, name string, usedPct float64, resetSeconds int, caption string, now string) providers.MetricValue {
	var resetAt *time.Time
	if resetSeconds > 0 {
		t := time.Now().Add(time.Duration(resetSeconds) * time.Second)
		resetAt = &t
	}
	return providerutil.PercentRemainingMetric(id, label, name, usedPct, resetAt, caption, now)
}

// looksSignedOut reports whether text is an auth/login response.
func looksSignedOut(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "login") ||
		strings.Contains(lower, "sign in") ||
		strings.Contains(lower, "auth/authorize") ||
		strings.Contains(lower, "not associated with an account") ||
		strings.Contains(lower, `actor of type "public"`)
}

// extractFloat extracts a float from the first capture group.
func extractFloat(pattern string, text string) *float64 {
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return nil
	}
	v, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return nil
	}
	return &v
}

// extractInt extracts an int from the first capture group.
func extractInt(pattern string, text string) *int {
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return nil
	}
	v, err := strconv.Atoi(match[1])
	if err != nil {
		return nil
	}
	return &v
}

// clampPercent normalizes 0..1 or 0..100 values to 0..100.
func clampPercent(value float64) float64 {
	if value >= 0 && value <= 1 {
		value *= 100
	}
	return math.Max(0, math.Min(100, value))
}

// errorSnapshot returns an OpenCode Go setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "opencodego",
		ProviderName: "OpenCode Go",
		Source:       "cookie",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// init registers the OpenCode Go provider with the package registry.
func init() {
	providers.Register(Provider{})
}

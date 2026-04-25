// Package kimi implements the Kimi usage provider.
//
// Auth: Kimi auth token from Property Inspector or env, falling back to the
// Usage Buttons Helper extension with the user's kimi.com browser session.
// Endpoint: POST https://www.kimi.com/apiv2/kimi.gateway.billing.v1.BillingService/GetUsages.
package kimi

import (
	"context"
	"encoding/base64"
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
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const usageURL = "https://www.kimi.com/apiv2/kimi.gateway.billing.v1.BillingService/GetUsages"

var kimiAuthRE = regexp.MustCompile(`(?i)kimi-auth[:=]\s*([A-Za-z0-9._\-+=/]+)`)

// usageResponse is Kimi's coding usage response.
type usageResponse struct {
	Usages []usageEntry `json:"usages"`
}

// usageEntry is one Kimi scoped usage entry.
type usageEntry struct {
	Scope  string           `json:"scope"`
	Detail usageDetail      `json:"detail"`
	Limits []rateLimitEntry `json:"limits"`
}

// usageDetail is one quota lane returned by Kimi.
type usageDetail struct {
	Limit     string `json:"limit"`
	Used      string `json:"used"`
	Remaining string `json:"remaining"`
	ResetTime string `json:"resetTime"`
}

// rateLimitEntry is a Kimi nested rate-limit window.
type rateLimitEntry struct {
	Window rateWindow  `json:"window"`
	Detail usageDetail `json:"detail"`
}

// rateWindow describes a rate-limit duration.
type rateWindow struct {
	Duration int    `json:"duration"`
	TimeUnit string `json:"timeUnit"`
}

// sessionInfo is optional metadata decoded from the kimi-auth JWT.
type sessionInfo struct {
	DeviceID  string
	SessionID string
	TrafficID string
}

// usageSnapshot is the parsed Kimi quota state.
type usageSnapshot struct {
	Weekly    usageDetail
	Rate      *usageDetail
	UpdatedAt time.Time
}

// Provider fetches Kimi usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "kimi" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Kimi" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#fe603c" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#111214" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent"}
}

// Fetch returns the latest Kimi usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	if token := configuredToken(); token != "" {
		usage, err := fetchWithToken(token)
		if err == nil {
			return snapshotFromUsage(usage, "token"), nil
		}
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot("Kimi auth token is invalid or expired. Refresh the kimi-auth cookie/token."), nil
		}
		return providers.Snapshot{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("kimi.com")), nil
	}
	usage, err := fetchWithBrowser(ctx)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot(cookieaux.StaleMessage("kimi.com")), nil
		}
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage, "cookie"), nil
}

// configuredToken resolves a Kimi auth token from settings or env.
func configuredToken() string {
	pk := settings.ProviderKeysGet()
	for _, raw := range []string{
		pk.KimiAuthToken,
		settings.ResolveAPIKey("", "KIMI_AUTH_TOKEN", "kimi_auth_token", "KIMI_MANUAL_COOKIE"),
	} {
		if token := cleanToken(raw); token != "" {
			return token
		}
	}
	return ""
}

// cleanToken extracts a kimi-auth JWT from a token, cookie, or curl header.
func cleanToken(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
		v = strings.TrimSpace(v[1 : len(v)-1])
	}
	if match := kimiAuthRE.FindStringSubmatch(v); len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	if strings.HasPrefix(v, "eyJ") && strings.Count(v, ".") == 2 {
		return v
	}
	return ""
}

// fetchWithToken fetches Kimi usage with a known kimi-auth token.
func fetchWithToken(token string) (usageSnapshot, error) {
	var out usageResponse
	err := httputil.PostJSON(usageURL, usageHeaders(token), map[string]any{
		"scope": []string{"FEATURE_CODING"},
	}, 20*time.Second, &out)
	if err != nil {
		return usageSnapshot{}, err
	}
	return parseUsage(out, time.Now().UTC())
}

// fetchWithBrowser fetches Kimi usage through the Helper extension.
func fetchWithBrowser(ctx context.Context) (usageSnapshot, error) {
	body, err := json.Marshal(map[string]any{
		"scope": []string{"FEATURE_CODING"},
	})
	if err != nil {
		return usageSnapshot{}, err
	}
	resp, err := cookies.Fetch(ctx, cookies.Request{
		URL:    usageURL,
		Method: "POST",
		Headers: map[string]string{
			"Accept":                   "*/*",
			"Content-Type":             "application/json",
			"Origin":                   "https://www.kimi.com",
			"Referer":                  "https://www.kimi.com/code/console",
			"User-Agent":               httputil.DefaultUserAgent,
			"connect-protocol-version": "1",
			"x-language":               "en-US",
			"x-msh-platform":           "web",
			"r-timezone":               "UTC",
		},
		Body: body,
	})
	if err != nil {
		return usageSnapshot{}, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return usageSnapshot{}, &httputil.Error{
			Status:     resp.Status,
			StatusText: resp.StatusText,
			Body:       string(resp.Body),
			URL:        usageURL,
		}
	}
	var out usageResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return usageSnapshot{}, fmt.Errorf("invalid Kimi JSON: %w", err)
	}
	return parseUsage(out, time.Now().UTC())
}

// usageHeaders builds direct API headers for a known kimi-auth token.
func usageHeaders(token string) map[string]string {
	headers := map[string]string{
		"Authorization":            "Bearer " + token,
		"Cookie":                   "kimi-auth=" + token,
		"Accept":                   "*/*",
		"Origin":                   "https://www.kimi.com",
		"Referer":                  "https://www.kimi.com/code/console",
		"connect-protocol-version": "1",
		"x-language":               "en-US",
		"x-msh-platform":           "web",
		"r-timezone":               "UTC",
	}
	if info := decodeSessionInfo(token); info != nil {
		if info.DeviceID != "" {
			headers["x-msh-device-id"] = info.DeviceID
		}
		if info.SessionID != "" {
			headers["x-msh-session-id"] = info.SessionID
		}
		if info.TrafficID != "" {
			headers["x-traffic-id"] = info.TrafficID
		}
	}
	return headers
}

// decodeSessionInfo extracts optional Kimi request headers from the JWT payload.
func decodeSessionInfo(jwt string) *sessionInfo {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil
		}
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil
	}
	return &sessionInfo{
		DeviceID:  providerutil.StringValue(root["device_id"]),
		SessionID: providerutil.StringValue(root["ssid"]),
		TrafficID: providerutil.StringValue(root["sub"]),
	}
}

// parseUsage selects FEATURE_CODING quota and rate-limit lanes.
func parseUsage(resp usageResponse, now time.Time) (usageSnapshot, error) {
	for _, usage := range resp.Usages {
		if usage.Scope != "FEATURE_CODING" {
			continue
		}
		var rate *usageDetail
		if len(usage.Limits) > 0 {
			detail := usage.Limits[0].Detail
			rate = &detail
		}
		return usageSnapshot{
			Weekly:    usage.Detail,
			Rate:      rate,
			UpdatedAt: now,
		}, nil
	}
	return usageSnapshot{}, fmt.Errorf("Kimi response missing FEATURE_CODING usage")
}

// snapshotFromUsage maps parsed Kimi usage into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot, source string) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	metrics := []providers.MetricValue{
		quotaMetric("session-percent", "WEEKLY", "Kimi weekly requests remaining", usage.Weekly, "requests", now),
	}
	if usage.Rate != nil {
		metrics = append(metrics, quotaMetric("weekly-percent", "5-HOUR", "Kimi five-hour rate limit remaining", *usage.Rate, "per 5 hours", now))
	}
	return providers.Snapshot{
		ProviderID:   "kimi",
		ProviderName: "Kimi",
		Source:       source,
		Metrics:      metrics,
		Status:       "operational",
	}
}

// quotaMetric builds a remaining-percent metric from a Kimi quota detail.
func quotaMetric(id, label, name string, detail usageDetail, unitLabel string, now string) providers.MetricValue {
	limit := numericString(detail.Limit)
	remaining := numericString(detail.Remaining)
	used := numericString(detail.Used)
	if remaining == nil && used != nil && limit != nil {
		v := math.Max(0, *limit-*used)
		remaining = &v
	}
	if used == nil && remaining != nil && limit != nil {
		v := math.Max(0, *limit-*remaining)
		used = &v
	}
	usedPct := 0.0
	if used != nil && limit != nil && *limit > 0 {
		usedPct = math.Max(0, math.Min(100, *used / *limit * 100))
	}
	var resetAt *time.Time
	if t, ok := providerutil.TimeValue(detail.ResetTime); ok {
		resetAt = &t
	}
	caption := unitLabel
	if used != nil && limit != nil {
		caption = fmt.Sprintf("%s/%s %s", compactNumber(*used), compactNumber(*limit), unitLabel)
	}
	metric := providerutil.PercentRemainingMetric(id, label, name, usedPct, resetAt, caption, now)
	if remaining != nil && limit != nil {
		rawCount := int(math.Round(*remaining))
		rawMax := int(math.Round(*limit))
		metric.RawCount = &rawCount
		metric.RawMax = &rawMax
	}
	return metric
}

// numericString parses a numeric Kimi string.
func numericString(raw string) *float64 {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return nil
	}
	return &n
}

// compactNumber renders request counts without noisy decimals.
func compactNumber(value float64) string {
	if value == math.Trunc(value) {
		return fmt.Sprintf("%.0f", value)
	}
	return fmt.Sprintf("%.1f", value)
}

// errorSnapshot returns a Kimi setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "kimi",
		ProviderName: "Kimi",
		Source:       "auth",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// init registers the Kimi provider with the package registry.
func init() {
	providers.Register(Provider{})
}

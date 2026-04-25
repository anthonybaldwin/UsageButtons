// Package factory implements the Factory Droid usage provider.
//
// Auth: Usage Buttons Helper extension with the user's app.factory.ai
// browser session, or an optional bearer token from the Property Inspector /
// FACTORY_TOKEN.
package factory

import (
	"context"
	"encoding/json"
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

const (
	providerID   = "factory"
	providerName = "Droid"
	defaultBase  = "https://app.factory.ai"
	authBase     = "https://auth.factory.ai"
	apiBase      = "https://api.factory.ai"
)

// tokenUsage is one Factory token bucket.
type tokenUsage struct {
	UserTokens         int64    `json:"userTokens"`
	OrgTotalTokensUsed int64    `json:"orgTotalTokensUsed"`
	TotalAllowance     int64    `json:"totalAllowance"`
	UsedRatio          *float64 `json:"usedRatio"`
	OrgOverageUsed     int64    `json:"orgOverageUsed"`
	BasicAllowance     int64    `json:"basicAllowance"`
	OrgOverageLimit    int64    `json:"orgOverageLimit"`
}

// usageData is Factory's subscription usage payload.
type usageData struct {
	StartDate *int64      `json:"startDate"`
	EndDate   *int64      `json:"endDate"`
	Standard  *tokenUsage `json:"standard"`
	Premium   *tokenUsage `json:"premium"`
}

// usageResponse is the Factory usage endpoint response.
type usageResponse struct {
	Usage  *usageData `json:"usage"`
	Source string     `json:"source"`
	UserID string     `json:"userId"`
}

// authResponse is the Factory auth/me endpoint response.
type authResponse struct {
	Organization *organization `json:"organization"`
}

// organization is the account organization returned by Factory.
type organization struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Subscription *subscription `json:"subscription"`
}

// subscription is Factory subscription metadata.
type subscription struct {
	FactoryTier     string           `json:"factoryTier"`
	OrbSubscription *orbSubscription `json:"orbSubscription"`
}

// orbSubscription is Factory's Orb billing subscription wrapper.
type orbSubscription struct {
	Plan   *plan  `json:"plan"`
	Status string `json:"status"`
}

// plan is the billing plan metadata returned by Factory.
type plan struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// usageSnapshot is the normalized Droid usage state.
type usageSnapshot struct {
	Standard         tokenUsage
	Premium          tokenUsage
	PeriodEnd        *time.Time
	PlanName         string
	Tier             string
	OrganizationName string
	UserID           string
	UpdatedAt        time.Time
	Source           string
}

// Provider fetches Droid usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return providerID }

// Name returns the human-readable provider name.
func (Provider) Name() string { return providerName }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#ff6b35" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#111214" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent"}
}

// Fetch returns the latest Droid usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	if cookies.HostAvailable(context.Background()) {
		usage, err := fetchBrowserUsage()
		if err == nil {
			return snapshotFromUsage(usage), nil
		}
		if isAuthFailure(err) {
			return errorSnapshot(cookieaux.StaleMessage("app.factory.ai")), nil
		}
	}

	token := bearerToken()
	if token == "" {
		return errorSnapshot(cookieaux.MissingMessage("app.factory.ai") + " Or enter a Factory/Droid bearer token."), nil
	}
	usage, err := fetchTokenUsage(token)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot("Factory/Droid bearer token was rejected."), nil
		}
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage), nil
}

// fetchBrowserUsage fetches Droid usage through the Helper extension.
func fetchBrowserUsage() (usageSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var lastErr error
	for _, base := range baseCandidates() {
		usage, err := fetchUsageViaBrowser(ctx, base)
		if err == nil {
			return usage, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("Factory/Droid browser fetch failed")
	}
	return usageSnapshot{}, lastErr
}

// fetchTokenUsage fetches Droid usage with a bearer token.
func fetchTokenUsage(token string) (usageSnapshot, error) {
	var lastErr error
	for _, base := range baseCandidates() {
		authInfo, err := fetchAuthInfoDirect(base, token)
		if err != nil {
			lastErr = err
			continue
		}
		usageInfo, err := fetchUsageDirect(base, token, "")
		if err != nil {
			lastErr = err
			continue
		}
		return buildSnapshot(authInfo, usageInfo, "token"), nil
	}
	if lastErr == nil {
		lastErr = errors.New("Factory/Droid token fetch failed")
	}
	return usageSnapshot{}, lastErr
}

// fetchUsageViaBrowser fetches auth and usage JSON for one base URL.
func fetchUsageViaBrowser(ctx context.Context, base string) (usageSnapshot, error) {
	authInfo, err := fetchAuthInfoBrowser(ctx, base)
	if err != nil {
		return usageSnapshot{}, err
	}
	usageInfo, err := fetchUsageBrowser(ctx, base, "")
	if err != nil {
		return usageSnapshot{}, err
	}
	return buildSnapshot(authInfo, usageInfo, "cookie"), nil
}

// fetchAuthInfoBrowser fetches the Factory auth/me payload through the browser.
func fetchAuthInfoBrowser(ctx context.Context, base string) (authResponse, error) {
	var out authResponse
	err := fetchBrowserJSON(ctx, joinURL(base, "/api/app/auth/me"), "GET", nil, &out)
	return out, err
}

// fetchUsageBrowser fetches the Factory usage payload through the browser.
func fetchUsageBrowser(ctx context.Context, base string, userID string) (usageResponse, error) {
	var out usageResponse
	body := map[string]any{"useCache": true}
	if userID != "" {
		body["userId"] = userID
	}
	err := fetchBrowserJSON(ctx, joinURL(base, "/api/organization/subscription/usage"), "POST", body, &out)
	return out, err
}

// fetchBrowserJSON performs one JSON fetch through the Helper extension.
func fetchBrowserJSON(ctx context.Context, url string, method string, body any, dst any) error {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	resp, err := cookies.Fetch(ctx, cookies.Request{
		URL:    url,
		Method: method,
		Headers: map[string]string{
			"Accept":           "application/json",
			"Content-Type":     "application/json",
			"x-factory-client": "web-app",
		},
		Body: raw,
	})
	if err != nil {
		return err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return &httputil.Error{
			Status:     resp.Status,
			StatusText: resp.StatusText,
			Body:       string(resp.Body),
			URL:        url,
		}
	}
	if len(resp.Body) == 0 {
		return fmt.Errorf("empty Factory/Droid response from %s", url)
	}
	if err := json.Unmarshal(resp.Body, dst); err != nil {
		return fmt.Errorf("invalid Factory/Droid JSON from %s: %w", url, err)
	}
	return nil
}

// fetchAuthInfoDirect fetches Factory auth/me using a bearer token.
func fetchAuthInfoDirect(base string, token string) (authResponse, error) {
	var out authResponse
	err := httputil.GetJSON(joinURL(base, "/api/app/auth/me"), tokenHeaders(token), 20*time.Second, &out)
	return out, err
}

// fetchUsageDirect fetches Factory usage using a bearer token.
func fetchUsageDirect(base string, token string, userID string) (usageResponse, error) {
	body := map[string]any{"useCache": true}
	if userID != "" {
		body["userId"] = userID
	}
	var out usageResponse
	err := httputil.PostJSON(joinURL(base, "/api/organization/subscription/usage"), tokenHeaders(token), body, 20*time.Second, &out)
	return out, err
}

// buildSnapshot normalizes Factory auth and usage responses.
func buildSnapshot(authInfo authResponse, usageInfo usageResponse, source string) usageSnapshot {
	usage := usageInfo.Usage
	snap := usageSnapshot{
		UpdatedAt: time.Now().UTC(),
		Source:    source,
		UserID:    usageInfo.UserID,
	}
	if usage != nil {
		if usage.Standard != nil {
			snap.Standard = *usage.Standard
		}
		if usage.Premium != nil {
			snap.Premium = *usage.Premium
		}
		if usage.EndDate != nil {
			end := time.UnixMilli(*usage.EndDate).UTC()
			snap.PeriodEnd = &end
		}
	}
	if org := authInfo.Organization; org != nil {
		snap.OrganizationName = org.Name
		if sub := org.Subscription; sub != nil {
			snap.Tier = sub.FactoryTier
			if sub.OrbSubscription != nil && sub.OrbSubscription.Plan != nil {
				snap.PlanName = sub.OrbSubscription.Plan.Name
			}
		}
	}
	return snap
}

// snapshotFromUsage maps Droid token buckets into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.Format(time.RFC3339)
	metrics := []providers.MetricValue{
		tokenMetric("session-percent", "STANDARD", "Droid Standard tokens remaining", usage.Standard, usage.PeriodEnd, now),
		tokenMetric("weekly-percent", "PREMIUM", "Droid Premium tokens remaining", usage.Premium, usage.PeriodEnd, now),
	}
	return providers.Snapshot{
		ProviderID:   providerID,
		ProviderName: displayName(usage),
		Source:       usage.Source,
		Metrics:      metrics,
		Status:       "operational",
	}
}

// tokenMetric builds one remaining-percent token metric.
func tokenMetric(id, label, name string, usage tokenUsage, resetAt *time.Time, now string) providers.MetricValue {
	usedPct := usagePercent(usage)
	caption := tokenCaption(usage)
	metric := providerutil.PercentRemainingMetric(id, label, name, usedPct, resetAt, caption, now)
	if usage.TotalAllowance > 0 && usage.TotalAllowance <= unlimitedAllowance {
		remaining := int(math.Round(math.Max(0, float64(usage.TotalAllowance-usage.UserTokens))))
		total := int(usage.TotalAllowance)
		metric.RawCount = &remaining
		metric.RawMax = &total
	}
	return metric
}

const unlimitedAllowance int64 = 1_000_000_000_000

// usagePercent returns used percent from Factory's ratio or token counts.
func usagePercent(usage tokenUsage) float64 {
	if usage.UsedRatio != nil {
		ratio := *usage.UsedRatio
		if !math.IsNaN(ratio) && !math.IsInf(ratio, 0) {
			if ratio >= -0.001 && ratio <= 1.001 {
				return math.Max(0, math.Min(100, ratio*100))
			}
			allowanceReliable := usage.TotalAllowance > 0 && usage.TotalAllowance <= unlimitedAllowance
			if !allowanceReliable && ratio >= -0.1 && ratio <= 100.1 {
				return math.Max(0, math.Min(100, ratio))
			}
		}
	}
	if usage.TotalAllowance > unlimitedAllowance {
		return math.Min(100, float64(usage.UserTokens)/100_000_000*100)
	}
	if usage.TotalAllowance <= 0 {
		return 0
	}
	return math.Max(0, math.Min(100, float64(usage.UserTokens)/float64(usage.TotalAllowance)*100))
}

// tokenCaption formats Factory token counts compactly.
func tokenCaption(usage tokenUsage) string {
	used := formatTokens(usage.UserTokens)
	if usage.TotalAllowance > 0 && usage.TotalAllowance <= unlimitedAllowance {
		return fmt.Sprintf("%s/%s tokens", used, formatTokens(usage.TotalAllowance))
	}
	if usage.OrgTotalTokensUsed > 0 {
		return fmt.Sprintf("%s used, org %s", used, formatTokens(usage.OrgTotalTokensUsed))
	}
	return used + " used"
}

// displayName returns Droid with org/tier context when available.
func displayName(usage usageSnapshot) string {
	parts := []string{providerName}
	if usage.Tier != "" {
		parts = append(parts, strings.Title(usage.Tier))
	} else if usage.PlanName != "" && !strings.Contains(strings.ToLower(usage.PlanName), "factory") {
		parts = append(parts, usage.PlanName)
	}
	if usage.OrganizationName != "" {
		parts = append(parts, usage.OrganizationName)
	}
	return strings.Join(parts, " ")
}

// baseCandidates returns Factory base URL candidates in CodexBar order.
func baseCandidates() []string {
	pk := settings.ProviderKeysGet()
	override := settings.ResolveEndpoint(pk.FactoryBaseURL, "", "FACTORY_BASE_URL", "FACTORY_DROID_BASE_URL")
	return uniqueStrings(nonEmptyStrings(authBase, apiBase, defaultBase, override))
}

// bearerToken resolves a Factory bearer token from settings or env vars.
func bearerToken() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().FactoryToken,
		"FACTORY_TOKEN",
		"FACTORY_BEARER_TOKEN",
		"DROID_TOKEN",
	)
}

// tokenHeaders returns Factory direct-request headers.
func tokenHeaders(token string) map[string]string {
	return map[string]string{
		"Authorization":    "Bearer " + token,
		"Accept":           "application/json",
		"Content-Type":     "application/json",
		"Origin":           defaultBase,
		"Referer":          defaultBase + "/",
		"x-factory-client": "web-app",
	}
}

// joinURL joins a base URL and absolute path.
func joinURL(base string, path string) string {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Scheme == "" {
		u, _ = url.Parse(defaultBase)
	}
	u.Path = path
	u.RawQuery = ""
	return u.String()
}

// isAuthFailure reports whether err means the Factory browser session is stale.
func isAuthFailure(err error) bool {
	var httpErr *httputil.Error
	if errors.As(err, &httpErr) {
		return httpErr.Status == 401 || httpErr.Status == 403
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "not logged in") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "forbidden")
}

// errorSnapshot returns a Droid setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   providerID,
		ProviderName: providerName,
		Source:       "cookie",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// formatTokens formats token counts using compact suffixes.
func formatTokens(n int64) string {
	sign := ""
	value := float64(n)
	if n < 0 {
		sign = "-"
		value = -value
	}
	switch {
	case value >= 1_000_000_000:
		return fmt.Sprintf("%s%.1fB", sign, value/1_000_000_000)
	case value >= 1_000_000:
		return fmt.Sprintf("%s%.1fM", sign, value/1_000_000)
	case value >= 1_000:
		return fmt.Sprintf("%s%.1fK", sign, value/1_000)
	default:
		return fmt.Sprintf("%s%.0f", sign, value)
	}
}

// nonEmptyStrings removes empty strings.
func nonEmptyStrings(values ...string) []string {
	var out []string
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimRight(strings.TrimSpace(value), "/"))
		}
	}
	return out
}

// uniqueStrings removes duplicate strings while preserving order.
func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// init registers the Factory provider with the package registry.
func init() {
	providers.Register(Provider{})
}

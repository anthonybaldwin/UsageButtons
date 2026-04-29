// Package alibaba implements the Alibaba Coding Plan usage provider.
//
// Auth: Usage Buttons Helper extension with the user's Alibaba Cloud
// console session, or an optional Alibaba Coding Plan API key from the
// Property Inspector / ALIBABA_CODING_PLAN_API_KEY.
package alibaba

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

// apiRegion describes one Alibaba Coding Plan console/API gateway.
type apiRegion struct {
	ID              string
	BaseURL         string
	DashboardURL    string
	CommodityCode   string
	CurrentRegionID string
}

var (
	internationalRegion = apiRegion{
		ID:              "intl",
		BaseURL:         "https://modelstudio.console.alibabacloud.com",
		DashboardURL:    "https://modelstudio.console.alibabacloud.com/ap-southeast-1/?tab=coding-plan#/efm/detail",
		CommodityCode:   "sfm_codingplan_public_intl",
		CurrentRegionID: "ap-southeast-1",
	}
	chinaRegion = apiRegion{
		ID:              "cn",
		BaseURL:         "https://bailian.console.aliyun.com",
		DashboardURL:    "https://bailian.console.aliyun.com/cn-beijing/?tab=model#/efm/coding_plan",
		CommodityCode:   "sfm_codingplan_public_cn",
		CurrentRegionID: "cn-beijing",
	}
)

// quotaWindow is one Alibaba quota period.
type quotaWindow struct {
	Used    int
	Total   int
	ResetAt *time.Time
}

// usageSnapshot is the normalized Alibaba Coding Plan quota state.
type usageSnapshot struct {
	PlanName  string
	FiveHour  *quotaWindow
	Weekly    *quotaWindow
	Monthly   *quotaWindow
	UpdatedAt time.Time
	Source    string
}

// Provider fetches Alibaba Coding Plan usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "alibaba" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Alibaba" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#ff6a00" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#111214" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent", "monthly-percent"}
}

// Fetch returns the latest Alibaba Coding Plan quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	if cookies.HostAvailable(context.Background()) {
		usage, err := fetchBrowserUsage()
		if err == nil {
			return snapshotFromUsage(usage), nil
		}
		if isAuthFailure(err) {
			return errorSnapshot(cookieaux.StaleMessage("Alibaba Cloud console")), nil
		}
	}

	apiKey := apiToken()
	if apiKey == "" {
		return errorSnapshot(cookieaux.MissingMessage("Alibaba Cloud console") + " Or enter an Alibaba Coding Plan API key."), nil
	}
	usage, err := fetchAPIUsage(apiKey)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot("Alibaba Coding Plan API key was rejected."), nil
		}
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage), nil
}

// fetchBrowserUsage fetches quota data through the Helper extension.
func fetchBrowserUsage() (usageSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var lastErr error
	for _, region := range candidateRegions() {
		raw, err := fetchRegionViaBrowser(ctx, region)
		if err != nil {
			lastErr = err
			continue
		}
		usage, err := parseUsage(raw, "cookie")
		if err == nil {
			return usage, nil
		}
		lastErr = err
		if !shouldTryAlternateRegion(err) {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("Alibaba Coding Plan browser fetch failed")
	}
	return usageSnapshot{}, lastErr
}

// fetchAPIUsage fetches quota data with an API key.
func fetchAPIUsage(apiKey string) (usageSnapshot, error) {
	var lastErr error
	for _, region := range candidateRegions() {
		var raw any
		err := httputil.PostJSON(quotaURL(region), map[string]string{
			"Authorization":        "Bearer " + apiKey,
			"x-api-key":            apiKey,
			"X-DashScope-API-Key":  apiKey,
			"Accept":               "application/json",
			"Origin":               region.BaseURL,
			"Referer":              region.DashboardURL,
			"User-Agent":           httputil.DefaultUserAgent,
			"X-Requested-With":     "XMLHttpRequest",
			"X-Acs-Console-Mode":   "api",
			"X-Acs-Console-Region": region.CurrentRegionID,
		}, quotaPayload(region), 25*time.Second, &raw)
		if err != nil {
			lastErr = err
			continue
		}
		usage, err := parseUsage(raw, "api-key")
		if err == nil {
			return usage, nil
		}
		lastErr = err
		if !shouldTryAlternateRegion(err) {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("Alibaba Coding Plan API fetch failed")
	}
	return usageSnapshot{}, lastErr
}

// fetchRegionViaBrowser dispatches one region request through the Helper.
func fetchRegionViaBrowser(ctx context.Context, region apiRegion) (any, error) {
	body, _ := json.Marshal(quotaPayload(region))
	resp, err := cookies.Fetch(ctx, cookies.Request{
		URL:    quotaURL(region),
		Method: "POST",
		Headers: map[string]string{
			"Accept":           "application/json",
			"Content-Type":     "application/json",
			"Origin":           region.BaseURL,
			"Referer":          region.DashboardURL,
			"User-Agent":       httputil.DefaultUserAgent,
			"X-Requested-With": "XMLHttpRequest",
		},
		Body: body,
	})
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, &httputil.Error{
			Status:     resp.Status,
			StatusText: resp.StatusText,
			Body:       string(resp.Body),
			URL:        quotaURL(region),
		}
	}
	var raw any
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return nil, fmt.Errorf("invalid Alibaba Coding Plan JSON: %w", err)
	}
	return raw, nil
}

// quotaPayload builds Alibaba's Coding Plan quota request body.
func quotaPayload(region apiRegion) map[string]any {
	return map[string]any{
		"queryCodingPlanInstanceInfoRequest": map[string]any{
			"commodityCode": region.CommodityCode,
		},
	}
}

// quotaURL resolves the region quota endpoint, honoring env/settings overrides.
func quotaURL(region apiRegion) string {
	pk := settings.ProviderKeysGet()
	if full := settings.ResolveEndpoint(pk.AlibabaQuotaURL, "", "ALIBABA_CODING_PLAN_QUOTA_URL"); full != "" {
		return full
	}
	base := settings.ResolveEndpoint(pk.AlibabaHost, region.BaseURL, "ALIBABA_CODING_PLAN_HOST")
	u, _ := url.Parse(base)
	if u.Scheme == "" {
		u, _ = url.Parse("https://" + base)
	}
	u.Path = "/data/api.json"
	q := u.Query()
	q.Set("action", "zeldaEasy.broadscope-bailian.codingPlan.queryCodingPlanInstanceInfoV2")
	q.Set("product", "broadscope-bailian")
	q.Set("api", "queryCodingPlanInstanceInfoV2")
	q.Set("currentRegionId", region.CurrentRegionID)
	u.RawQuery = q.Encode()
	return u.String()
}

// apiToken resolves an Alibaba Coding Plan token from settings or env vars.
func apiToken() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().AlibabaKey,
		"ALIBABA_CODING_PLAN_API_KEY",
		"ALIBABA_API_KEY",
		"DASHSCOPE_API_KEY",
	)
}

// candidateRegions returns the configured region followed by fallback.
func candidateRegions() []apiRegion {
	raw := strings.ToLower(strings.TrimSpace(settings.ResolveAPIKey(
		settings.ProviderKeysGet().AlibabaRegion,
		"ALIBABA_CODING_PLAN_REGION",
	)))
	switch raw {
	case "cn", "china", "china-mainland", "mainland":
		return []apiRegion{chinaRegion, internationalRegion}
	default:
		return []apiRegion{internationalRegion, chinaRegion}
	}
}

// parseUsage maps Alibaba's nested payload into a usage snapshot.
func parseUsage(raw any, source string) (usageSnapshot, error) {
	raw = expandJSON(raw)
	root, ok := raw.(map[string]any)
	if !ok {
		return usageSnapshot{}, fmt.Errorf("Alibaba Coding Plan response was not an object")
	}
	if err := responseError(root, source); err != nil {
		return usageSnapshot{}, err
	}
	instance := activeInstance(root)
	quota := quotaInfo(instance)
	if quota == nil {
		quota = quotaInfo(root)
	}
	if quota == nil {
		return usageSnapshot{}, fmt.Errorf("Alibaba Coding Plan response missing quota data")
	}
	usage := usageSnapshot{
		PlanName:  firstPlanName(instance, root),
		FiveHour:  quotaWindowFromKeys(quota, []string{"per5HourUsedQuota", "perFiveHourUsedQuota"}, []string{"per5HourTotalQuota", "perFiveHourTotalQuota"}, []string{"per5HourQuotaNextRefreshTime", "perFiveHourQuotaNextRefreshTime"}),
		Weekly:    quotaWindowFromKeys(quota, []string{"perWeekUsedQuota"}, []string{"perWeekTotalQuota"}, []string{"perWeekQuotaNextRefreshTime"}),
		Monthly:   quotaWindowFromKeys(quota, []string{"perBillMonthUsedQuota", "perMonthUsedQuota"}, []string{"perBillMonthTotalQuota", "perMonthTotalQuota"}, []string{"perBillMonthQuotaNextRefreshTime", "perMonthQuotaNextRefreshTime"}),
		UpdatedAt: time.Now().UTC(),
		Source:    source,
	}
	if usage.FiveHour == nil && usage.Weekly == nil && usage.Monthly == nil {
		return usageSnapshot{}, fmt.Errorf("Alibaba Coding Plan response had no usable quota windows")
	}
	usage.FiveHour = normalizeFiveHourReset(usage.FiveHour, usage.UpdatedAt)
	return usage, nil
}

// responseError detects transport-success API errors inside the JSON payload.
func responseError(root map[string]any, source string) error {
	if code, ok := findFirstFloat(root, []string{"statusCode", "status_code", "code"}); ok {
		if code != 0 && code != 200 {
			message := findFirstString(root, []string{"statusMessage", "status_msg", "message", "msg"})
			if message == "" {
				message = fmt.Sprintf("status code %.0f", code)
			}
			if code == 401 || code == 403 || isAuthText(message) {
				return fmt.Errorf("Alibaba Coding Plan login required: %s", message)
			}
			return fmt.Errorf("Alibaba Coding Plan API error: %s", message)
		}
	}
	text := strings.ToLower(findFirstString(root, []string{"code", "status", "statusCode", "message", "msg", "statusMessage"}))
	if strings.Contains(text, "needlogin") || strings.Contains(text, "login") {
		if source == "api-key" {
			return fmt.Errorf("Alibaba Coding Plan endpoint requires a console session for this account/region")
		}
		return fmt.Errorf("Alibaba Coding Plan login required")
	}
	return nil
}

// activeInstance finds the best plan instance in a nested response.
func activeInstance(root map[string]any) map[string]any {
	items := findFirstArray(root, []string{"codingPlanInstanceInfos", "coding_plan_instance_infos"})
	var first map[string]any
	var best map[string]any
	bestScore := math.MinInt
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if first == nil {
			first = m
		}
		score := activeScore(m)
		if score > bestScore {
			best = m
			bestScore = score
		}
	}
	if bestScore > 0 {
		return best
	}
	if best != nil {
		return best
	}
	return first
}

// activeScore ranks active Alibaba Coding Plan instances.
func activeScore(m map[string]any) int {
	status := strings.ToUpper(firstString(m, []string{"status", "instanceStatus"}))
	switch status {
	case "VALID", "ACTIVE":
		return 3
	case "EXPIRED", "INVALID", "INACTIVE", "DISABLED", "TERMINATED", "STOPPED":
		return -1
	}
	if active, ok := firstBool(m, []string{"isActive", "active"}); ok {
		if active {
			return 3
		}
		return -1
	}
	if expiry, ok := firstTime(m, []string{"endTime", "periodEndTime", "expireTime", "expirationTime"}); ok && time.Until(expiry) > 0 {
		return 1
	}
	return 0
}

// quotaInfo returns the first object that contains Alibaba quota fields.
func quotaInfo(root map[string]any) map[string]any {
	if root == nil {
		return nil
	}
	if direct := findFirstMap(root, []string{"codingPlanQuotaInfo", "coding_plan_quota_info"}); direct != nil {
		return direct
	}
	return findMapWithAnyKey(root, []string{
		"per5HourUsedQuota",
		"per5HourTotalQuota",
		"perFiveHourUsedQuota",
		"perFiveHourTotalQuota",
		"perWeekUsedQuota",
		"perWeekTotalQuota",
		"perBillMonthUsedQuota",
		"perBillMonthTotalQuota",
		"perMonthUsedQuota",
		"perMonthTotalQuota",
	})
}

// firstPlanName returns the best displayable plan name.
func firstPlanName(instance map[string]any, root map[string]any) string {
	for _, m := range []map[string]any{instance, root} {
		if m == nil {
			continue
		}
		if name := firstString(m, []string{"planName", "plan_name", "instanceName", "instance_name", "packageName", "package_name"}); name != "" {
			return name
		}
	}
	return ""
}

// quotaWindowFromKeys extracts one Alibaba quota window.
func quotaWindowFromKeys(root map[string]any, usedKeys []string, totalKeys []string, resetKeys []string) *quotaWindow {
	used, okUsed := firstFloat(root, usedKeys)
	total, okTotal := firstFloat(root, totalKeys)
	if !okUsed || !okTotal || total <= 0 {
		return nil
	}
	w := &quotaWindow{
		Used:  int(math.Round(math.Max(0, math.Min(used, total)))),
		Total: int(math.Round(total)),
	}
	if reset, ok := firstTime(root, resetKeys); ok {
		w.ResetAt = &reset
	}
	return w
}

// normalizeFiveHourReset shifts stale 5-hour timestamps into the next window.
func normalizeFiveHourReset(w *quotaWindow, updatedAt time.Time) *quotaWindow {
	if w == nil || w.ResetAt == nil {
		return w
	}
	if w.ResetAt.Sub(updatedAt) >= time.Minute {
		return w
	}
	shifted := w.ResetAt.Add(5 * time.Hour)
	if shifted.Sub(updatedAt) < time.Minute {
		shifted = updatedAt.Add(5 * time.Hour)
	}
	w.ResetAt = &shifted
	return w
}

// snapshotFromUsage maps Alibaba quota windows into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.Format(time.RFC3339)
	metrics := []providers.MetricValue{}
	if usage.FiveHour != nil {
		metrics = append(metrics, quotaMetric("session-percent", "5-HOUR", "Alibaba 5-hour quota remaining", usage.FiveHour, now))
	}
	if usage.Weekly != nil {
		metrics = append(metrics, quotaMetric("weekly-percent", "WEEKLY", "Alibaba weekly quota remaining", usage.Weekly, now))
	}
	if usage.Monthly != nil {
		metrics = append(metrics, quotaMetric("monthly-percent", "MONTHLY", "Alibaba monthly quota remaining", usage.Monthly, now))
	}
	return providers.Snapshot{
		ProviderID:   "alibaba",
		ProviderName: providerName(usage.PlanName),
		Source:       usage.Source,
		Metrics:      metrics,
		Status:       "operational",
	}
}

// quotaMetric converts one quota window to a remaining-percent metric.
func quotaMetric(id, label, name string, w *quotaWindow, now string) providers.MetricValue {
	usedPct := float64(w.Used) / float64(w.Total) * 100
	caption := fmt.Sprintf("%d / %d used", w.Used, w.Total)
	m := providerutil.PercentRemainingMetric(id, label, name, usedPct, w.ResetAt, caption, now)
	remaining := w.Total - w.Used
	m.RawCount = &remaining
	m.RawMax = &w.Total
	return m
}

// providerName returns Alibaba with a plan suffix when available.
func providerName(planName string) string {
	if strings.TrimSpace(planName) == "" {
		return "Alibaba"
	}
	return "Alibaba " + strings.TrimSpace(planName)
}

// errorSnapshot returns an Alibaba setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "alibaba",
		ProviderName: "Alibaba",
		Source:       "none",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// shouldTryAlternateRegion reports whether another Alibaba region may work.
func shouldTryAlternateRegion(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *httputil.Error
	if errors.As(err, &httpErr) {
		return httpErr.Status == 403 || httpErr.Status == 404
	}
	msg := err.Error()
	return strings.Contains(msg, "missing quota") ||
		strings.Contains(msg, "no usable quota") ||
		strings.Contains(msg, "login required")
}

// isAuthFailure reports whether err means the browser/API session is stale.
func isAuthFailure(err error) bool {
	var httpErr *httputil.Error
	if errors.As(err, &httpErr) {
		return httpErr.Status == 401 || httpErr.Status == 403
	}
	return isAuthText(err.Error())
}

// isAuthText matches Alibaba auth/login failures.
func isAuthText(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "needlogin") ||
		strings.Contains(lower, "login") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "unauthenticated") ||
		strings.Contains(lower, "forbidden") ||
		strings.Contains(lower, "api key")
}

// expandJSON recursively decodes JSON strings embedded in Alibaba payloads.
func expandJSON(v any) any {
	switch x := v.(type) {
	case string:
		s := strings.TrimSpace(x)
		if !(strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) {
			return x
		}
		var decoded any
		if err := json.Unmarshal([]byte(s), &decoded); err != nil {
			return x
		}
		return expandJSON(decoded)
	case []any:
		for i, item := range x {
			x[i] = expandJSON(item)
		}
		return x
	case map[string]any:
		for key, item := range x {
			x[key] = expandJSON(item)
		}
		return x
	default:
		return v
	}
}

// findFirstMap recursively finds the first object under any named key.
func findFirstMap(v any, keys []string) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range keys {
			if m, ok := x[key].(map[string]any); ok {
				return m
			}
		}
		for _, item := range x {
			if found := findFirstMap(item, keys); found != nil {
				return found
			}
		}
	case []any:
		for _, item := range x {
			if found := findFirstMap(item, keys); found != nil {
				return found
			}
		}
	}
	return nil
}

// findMapWithAnyKey recursively finds an object containing any key.
func findMapWithAnyKey(v any, keys []string) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range keys {
			if _, ok := x[key]; ok {
				return x
			}
		}
		for _, item := range x {
			if found := findMapWithAnyKey(item, keys); found != nil {
				return found
			}
		}
	case []any:
		for _, item := range x {
			if found := findMapWithAnyKey(item, keys); found != nil {
				return found
			}
		}
	}
	return nil
}

// findFirstArray recursively finds the first array under any named key.
func findFirstArray(v any, keys []string) []any {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range keys {
			if a, ok := x[key].([]any); ok {
				return a
			}
		}
		for _, item := range x {
			if found := findFirstArray(item, keys); len(found) > 0 {
				return found
			}
		}
	case []any:
		for _, item := range x {
			if found := findFirstArray(item, keys); len(found) > 0 {
				return found
			}
		}
	}
	return nil
}

// findFirstString recursively returns the first string-like value for keys.
func findFirstString(v any, keys []string) string {
	switch x := v.(type) {
	case map[string]any:
		if s := firstString(x, keys); s != "" {
			return s
		}
		for _, item := range x {
			if s := findFirstString(item, keys); s != "" {
				return s
			}
		}
	case []any:
		for _, item := range x {
			if s := findFirstString(item, keys); s != "" {
				return s
			}
		}
	}
	return ""
}

// findFirstFloat recursively returns the first numeric value for keys.
func findFirstFloat(v any, keys []string) (float64, bool) {
	switch x := v.(type) {
	case map[string]any:
		if n, ok := firstFloat(x, keys); ok {
			return n, true
		}
		for _, item := range x {
			if n, ok := findFirstFloat(item, keys); ok {
				return n, true
			}
		}
	case []any:
		for _, item := range x {
			if n, ok := findFirstFloat(item, keys); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// firstString returns the first string-like value for keys in one object.
func firstString(m map[string]any, keys []string) string {
	for _, key := range keys {
		if s := providerutil.StringValue(m[key]); s != "" {
			return s
		}
	}
	return ""
}

// firstFloat returns the first numeric value for keys in one object.
func firstFloat(m map[string]any, keys []string) (float64, bool) {
	for _, key := range keys {
		if n, ok := providerutil.FloatValue(m[key]); ok {
			return n, true
		}
	}
	return 0, false
}

// firstBool returns the first boolean-like value for keys in one object.
func firstBool(m map[string]any, keys []string) (bool, bool) {
	for _, key := range keys {
		switch v := m[key].(type) {
		case bool:
			return v, true
		case string:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "true", "1", "yes", "active", "valid":
				return true, true
			case "false", "0", "no", "inactive", "invalid", "expired":
				return false, true
			}
		case float64:
			return v != 0, true
		}
	}
	return false, false
}

// firstTime returns the first date-like value for keys in one object.
func firstTime(m map[string]any, keys []string) (time.Time, bool) {
	for _, key := range keys {
		if t, ok := providerutil.TimeValue(m[key]); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// init registers the Alibaba provider with the package registry.
func init() {
	providers.Register(Provider{})
}

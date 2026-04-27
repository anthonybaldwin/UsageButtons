// Package perplexity implements the Perplexity usage provider.
//
// Auth: Usage Buttons Helper extension with the user's perplexity.ai browser
// session. Endpoints (Perplexity removed /rest/billing/credits in 2026 —
// the new flow mirrors what perplexity.ai/account/usage itself fetches):
//
//	GET /rest/pplx-api/v2/groups                                — discover org/group ID
//	GET /rest/pplx-api/v2/groups/{groupId}                      — balance + plan tier
//	GET /rest/pplx-api/v2/groups/{groupId}/usage-analytics      — meter cost summaries
//	GET /rest/rate-limit/all                                    — per-tier query quotas
package perplexity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const (
	groupsURL    = "https://www.perplexity.ai/rest/pplx-api/v2/groups"
	groupURLBase = "https://www.perplexity.ai/rest/pplx-api/v2/groups/"
	rateLimitURL = "https://www.perplexity.ai/rest/rate-limit/all"
	// usageAnalyticsSuffix appends to the per-group URL to fetch the
	// meter event summaries used for the API-spend calculation.
	usageAnalyticsSuffix = "/usage-analytics"

	groupIDOverrideEnv = "USAGEBUTTONS_PERPLEXITY_GROUP_ID"
)

// usageSnapshot is the normalized Perplexity quota state.
//
// /rest/rate-limit/all only returns the *remaining* counts per tier —
// not a daily limit — so each rate-limit field is rendered as a count
// metric (the number itself is the value, no false percentage). Tier
// limits would have to be guessed per plan, and that's actively
// misleading when wrong (e.g. a fresh-day "200 remaining" rendering as
// "33% remaining" because we assumed a 600/day cap).
type usageSnapshot struct {
	// BalanceCents is customerInfo.balance × 100. $0 for Pro plan users
	// who haven't used the API platform.
	BalanceCents float64
	// SpendCents is customerInfo.spend.total_spend × 100 — all-time API
	// platform spend.
	SpendCents float64
	// CometSpendCents is cost from the `comet_cloud_duration_hours` meter
	// (Perplexity Comet — computer-use / AI-browser usage), USD * 100.
	// Tracked separately so users can see Comet activity at a glance
	// rather than buried in the aggregate api-spend.
	CometSpendCents float64
	// SubscriptionTier is "Pro" / "Max" / "Enterprise" / "" derived from
	// customerInfo.is_pro / is_max booleans.
	SubscriptionTier string
	// FreeQueriesAvailable mirrors free_queries.available — Pro plan's
	// "unlimited basic searches" flag.
	FreeQueriesAvailable bool
	// Per-tier remaining counts. nil when the API didn't return that key.
	ProRemaining   *int
	ResearchRemain *int
	LabsRemain     *int
	AgenticRemain  *int
	UpdatedAt      time.Time
}

// Provider fetches Perplexity usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "perplexity" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Perplexity" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#20b2aa" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#082423" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{
		"pro-queries-remaining",
		"deep-research-remaining",
		"labs-remaining",
		"agentic-research-remaining",
		"comet-spend",
		"api-balance",
		"api-spend",
	}
}

// Fetch returns the latest Perplexity quota snapshot.
func (Provider) Fetch(fctx providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("perplexity.ai")), nil
	}
	headers := map[string]string{
		"Accept":  "application/json",
		"Origin":  "https://www.perplexity.ai",
		"Referer": "https://www.perplexity.ai/account/usage",
	}

	// Demand-fetching: skip endpoints whose data doesn't contribute to
	// any bound metric. nil ActiveMetricIDs preserves the legacy
	// "fetch everything" behavior used during cold start / force.
	needs := perplexityFetchNeedsFor(fctx.ActiveMetricIDs)

	var groupMap, rateMap map[string]any
	var analyticsAny any

	if needs.group {
		groupID, err := resolveGroupID(ctx, headers)
		if err != nil {
			return mapHTTPError(err), nil
		}
		groupURL := groupURLBase + url.PathEscape(groupID)
		groupMap = map[string]any{}
		if err := cookies.FetchJSON(ctx, groupURL, headers, &groupMap); err != nil {
			return mapHTTPError(err), nil
		}
		// usage-analytics is best-effort — Pro accounts often have no
		// recorded meters yet (empty meter_event_summaries) and we still
		// want to render the rest of the dashboard. Errors only suppress
		// the api-spend metric.
		if needs.analytics {
			_, _ = fetchAny(ctx, groupURL+usageAnalyticsSuffix, headers, &analyticsAny)
		}
	}
	if needs.rate {
		rateMap = map[string]any{}
		if err := cookies.FetchJSON(ctx, rateLimitURL, headers, &rateMap); err != nil {
			return mapHTTPError(err), nil
		}
	}
	return snapshotFromUsage(usageFromResponses(groupMap, rateMap, analyticsAny, time.Now().UTC())), nil
}

// perplexityFetchNeeds maps the active-metric set to which of the four
// Perplexity endpoints actually need to be hit this poll. nil active
// set = "fetch everything" — every flag returns true.
type perplexityFetchNeeds struct {
	group     bool // /groups + /groups/{id} — needed by balance / spend tiles
	analytics bool // /groups/{id}/usage-analytics — needed by spend tiles
	rate      bool // /rate-limit/all — needed by per-feature query counts
}

// metricsNeedingGroup is the set of metric IDs whose values come from
// the per-group endpoint payload (balance, subscription tier, spend).
var (
	metricsNeedingGroup = map[string]bool{
		"comet-spend": true,
		"api-balance": true,
		"api-spend":   true,
	}
	metricsNeedingAnalytics = map[string]bool{
		"comet-spend": true,
		"api-spend":   true,
	}
	metricsNeedingRate = map[string]bool{
		"pro-queries-remaining":      true,
		"deep-research-remaining":    true,
		"labs-remaining":             true,
		"agentic-research-remaining": true,
	}
)

// perplexityFetchNeeds resolves which endpoints are required.
func perplexityFetchNeedsFor(active []string) perplexityFetchNeeds {
	if active == nil {
		return perplexityFetchNeeds{group: true, analytics: true, rate: true}
	}
	var n perplexityFetchNeeds
	for _, id := range active {
		if metricsNeedingGroup[id] {
			n.group = true
		}
		if metricsNeedingAnalytics[id] {
			n.analytics = true
		}
		if metricsNeedingRate[id] {
			n.rate = true
		}
	}
	return n
}

// fetchAny performs a best-effort GET and unmarshals into dst (any-typed
// so it accepts arrays or objects). Returns the raw bytes for diagnostic
// dumping plus any error so callers may distinguish "no data" from a
// real network failure.
func fetchAny(ctx context.Context, target string, headers map[string]string, dst *any) ([]byte, error) {
	resp, err := cookies.Fetch(ctx, cookies.Request{URL: target, Method: "GET", Headers: headers})
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return resp.Body, &httputil.Error{Status: resp.Status, StatusText: resp.StatusText, Body: string(resp.Body), URL: target}
	}
	if len(resp.Body) == 0 {
		return resp.Body, nil
	}
	return resp.Body, json.Unmarshal(resp.Body, dst)
}

// resolveGroupID returns the user-overridden group ID or discovers the
// first group from the groups list endpoint.
func resolveGroupID(ctx context.Context, headers map[string]string) (string, error) {
	if override := strings.TrimSpace(os.Getenv(groupIDOverrideEnv)); override != "" {
		return override, nil
	}
	// Use raw fetch + Unmarshal-into-any so we accept either an envelope
	// object ({"orgs":[…]}, {"groups":[…]}, …) or a plain root array.
	resp, err := cookies.Fetch(ctx, cookies.Request{URL: groupsURL, Method: "GET", Headers: headers})
	if err != nil {
		return "", err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return "", &httputil.Error{Status: resp.Status, StatusText: resp.StatusText, Body: string(resp.Body), URL: groupsURL}
	}
	var raw any
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		dumpUnknownGroups(resp.Body)
		return "", fmt.Errorf("Perplexity: groups response not JSON")
	}
	if id := firstGroupID(raw); id != "" {
		return id, nil
	}
	dumpUnknownGroups(resp.Body)
	return "", fmt.Errorf("Perplexity: no groups returned")
}

// firstGroupID walks the groups response looking for the active
// (default) group ID, falling back to the first viable ID. The response
// shape varies — it may be a raw array, or an object envelope under
// `orgs`, `groups`, `results`, `items`, `data`, `result`, etc. Each
// entry may identify itself with `api_org_id` / `apiOrgId` / `org_id` /
// `orgId` / `id` / `group_id` / `groupId` / `uuid` / `_id`. When any
// entry has `is_default_org: true` (or camelCase), prefer it over the
// first-seen id.
func firstGroupID(root any) string {
	if arr, ok := root.([]any); ok {
		return idFromArray(arr)
	}
	m, ok := root.(map[string]any)
	if !ok {
		return ""
	}
	envelopes := [][]string{
		{"orgs"}, {"groups"}, {"results"}, {"items"},
		{"data", "orgs"}, {"data", "groups"}, {"data", "results"}, {"data", "items"},
		{"result", "orgs"}, {"result", "groups"}, {"result", "results"}, {"result", "items"},
		{"data"}, {"result"},
	}
	for _, path := range envelopes {
		if arr, ok := nestedArray(m, path...); ok {
			if id := idFromArray(arr); id != "" {
				return id
			}
		}
	}
	// Single-object response (no envelope, no array).
	if id := idFromObj(m); id != "" {
		return id
	}
	return ""
}

// idFromArray returns the default org's ID when one is flagged, else the
// first object that yields any id-shaped string.
func idFromArray(arr []any) string {
	var first string
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := idFromObj(obj)
		if id == "" {
			continue
		}
		if first == "" {
			first = id
		}
		if isDefaultOrg(obj) {
			return id
		}
	}
	return first
}

// idFromObj reads an id-shaped string from a single group/org object.
func idFromObj(obj map[string]any) string {
	return providerutil.FirstString(obj,
		"api_org_id", "apiOrgId",
		"org_id", "orgId",
		"id", "group_id", "groupId",
		"uuid", "_id",
	)
}

// isDefaultOrg reports whether obj is flagged as the user's default org.
func isDefaultOrg(obj map[string]any) bool {
	for _, k := range []string{"is_default_org", "isDefaultOrg"} {
		if v, ok := obj[k].(bool); ok && v {
			return true
		}
	}
	return false
}

// nestedArray walks keys to return an array value when present.
func nestedArray(root map[string]any, keys ...string) ([]any, bool) {
	current := any(root)
	for _, k := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current = m[k]
	}
	arr, ok := current.([]any)
	return arr, ok
}

// dumpUnknownGroups appends a snippet of an unrecognized groups response
// to a temp file so a future debug pass can be precise. Owner-only perms,
// append mode with size caps; only fires when the groups call returns a
// shape we can't extract a group ID from.
func dumpUnknownGroups(body []byte) {
	const (
		maxSnippetBytes = 16 * 1024
		maxFileBytes    = 256 * 1024
	)
	path := filepath.Join(os.TempDir(), "usagebuttons-perplexity-debug.txt")
	if info, err := os.Stat(path); err == nil && info.Size() >= maxFileBytes {
		return
	}
	snippet := body
	truncated := false
	if len(snippet) > maxSnippetBytes {
		snippet = snippet[:maxSnippetBytes]
		truncated = true
	}
	header := fmt.Sprintf("[%s] groups endpoint: length=%d truncated=%v\n",
		time.Now().UTC().Format(time.RFC3339), len(body), truncated)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(header)
	_, _ = f.Write(snippet)
	_, _ = f.WriteString("\n\n")
}

// usageFromResponses normalizes group + rate-limit + usage-analytics JSON
// into usageSnapshot.
func usageFromResponses(groupMap, rateMap map[string]any, analytics any, now time.Time) usageSnapshot {
	usage := usageSnapshot{UpdatedAt: now}
	usage.BalanceCents = readBalanceCents(groupMap)
	usage.SpendCents = readSpendCents(groupMap)
	usage.SubscriptionTier = readSubscriptionTier(groupMap)
	usage.FreeQueriesAvailable = readFreeQueriesAvailable(rateMap)
	usage.ProRemaining = readRemainingCount(rateMap, "remaining_pro", "pro")
	usage.ResearchRemain = readRemainingCount(rateMap, "remaining_research", "research", "deep_research")
	usage.LabsRemain = readRemainingCount(rateMap, "remaining_labs", "labs")
	usage.AgenticRemain = readRemainingCount(rateMap, "remaining_agentic_research", "agentic_research", "agentic")
	if cost, ok := sumUsageCostCents(analytics); ok {
		// usage-analytics cost is paid API spend — overrides
		// customerInfo.spend.total_spend when present, since the
		// analytics endpoint is the more granular source.
		usage.SpendCents = cost
	}
	usage.CometSpendCents = sumMeterCostCents(analytics, "comet_cloud_duration_hours", "cometCloudDurationHours")
	return usage
}

// sumMeterCostCents totals `cost` (USD) across one specific meter's
// meter_event_summaries entries — used to break out Comet (computer-use)
// spend from the aggregate API spend. Returns 0 when the named meter
// is absent or has no summaries.
func sumMeterCostCents(root any, names ...string) float64 {
	arr, ok := analyticsArray(root)
	if !ok {
		return 0
	}
	wantName := func(n string) bool {
		for _, w := range names {
			if n == w {
				return true
			}
		}
		return false
	}
	var total float64
	for _, item := range arr {
		meter, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := meter["name"].(string)
		if !wantName(name) {
			continue
		}
		summaries, ok := firstAny(meter, "meter_event_summaries", "meterEventSummaries").([]any)
		if !ok {
			continue
		}
		for _, s := range summaries {
			obj, ok := s.(map[string]any)
			if !ok {
				continue
			}
			if v, ok := providerutil.FirstFloat(obj, "cost", "amount", "amount_usd", "amountUsd"); ok {
				total += v
			}
		}
	}
	return math.Round(total * 100)
}

// readSpendCents reads customerInfo.spend.total_spend × 100. Falls back
// across nested wrappers in case the response is restructured.
func readSpendCents(root map[string]any) float64 {
	wrappers := [][]string{
		{"customerInfo", "spend"}, {"customer_info", "spend"},
		{"apiOrganization", "customerInfo", "spend"},
		{"apiOrganization", "customer_info", "spend"},
		{"spend"},
	}
	for _, path := range wrappers {
		m, ok := providerutil.NestedMap(root, path...)
		if !ok {
			continue
		}
		if v, ok := providerutil.FirstFloat(m, "total_spend", "totalSpend", "amount", "amount_usd"); ok {
			return math.Round(v * 100)
		}
	}
	return 0
}

// readFreeQueriesAvailable returns the rate-limit response's
// `free_queries.available` flag — Pro plan's "unlimited basic queries"
// indicator. Currently informational; not surfaced as a metric.
func readFreeQueriesAvailable(rate map[string]any) bool {
	fq, ok := providerutil.NestedMap(rate, "free_queries")
	if !ok {
		return false
	}
	v, _ := fq["available"].(bool)
	return v
}

// readRemainingCount returns the first matching `remaining_*` key from
// the rate-limit response. Searches the root and any common envelope.
func readRemainingCount(rate map[string]any, keys ...string) *int {
	pools := []map[string]any{rate}
	for _, p := range []string{"rateLimits", "rate_limits", "data", "result"} {
		if nested, ok := providerutil.NestedMap(rate, p); ok {
			pools = append(pools, nested)
		}
	}
	for _, m := range pools {
		if v, ok := providerutil.FirstFloat(m, keys...); ok {
			n := int(math.Round(math.Max(0, v)))
			return &n
		}
	}
	return nil
}

// sumUsageCostCents walks the usage-analytics response and totals
// `cost` (USD) across all `meter_event_summaries[]` arrays. Returns
// (cents, true) on success — including the legitimate "no usage yet"
// zero. Returns (_, false) when the response shape is unrecognized
// so the provider can omit the derived metric.
func sumUsageCostCents(root any) (float64, bool) {
	arr, ok := analyticsArray(root)
	if !ok {
		return 0, false
	}
	var total float64
	hasMeter := len(arr) == 0
	hasCost := false
	for _, item := range arr {
		meter, ok := item.(map[string]any)
		if !ok {
			continue
		}
		summaries, ok := firstAny(meter, "meter_event_summaries", "meterEventSummaries").([]any)
		if !ok {
			continue
		}
		hasMeter = true
		for _, s := range summaries {
			obj, ok := s.(map[string]any)
			if !ok {
				continue
			}
			if v, ok := providerutil.FirstFloat(obj, "cost", "amount", "amount_usd", "amountUsd"); ok {
				total += v
				hasCost = true
			}
		}
	}
	if !hasMeter && !hasCost {
		return 0, false
	}
	return math.Round(total * 100), true
}

// analyticsArray returns the meter list from a usage-analytics response.
// Accepts a top-level array or an envelope object under common keys.
func analyticsArray(root any) ([]any, bool) {
	if arr, ok := root.([]any); ok {
		return arr, true
	}
	m, ok := root.(map[string]any)
	if !ok {
		return nil, false
	}
	for _, key := range []string{"usage_analytics", "usageAnalytics", "data", "result", "items"} {
		if v, ok := m[key]; ok {
			if arr, ok := v.([]any); ok {
				return arr, true
			}
		}
	}
	return nil, false
}

// firstAny returns the first present value among keys.
func firstAny(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

// readBalanceCents extracts a USD balance from the group payload's nested
// shape and converts to cents. Mirrors openusage's flexible lookup so we
// keep working when Perplexity tweaks wrapper key names.
func readBalanceCents(root map[string]any) float64 {
	wrappers := [][]string{
		nil,
		{"apiOrganization"}, {"api_organization"},
		{"group"}, {"org"}, {"organization"},
		{"data"}, {"result"}, {"item"},
		{"customerInfo"}, {"customer_info"},
		{"wallet"}, {"billing"}, {"usage"},
		// Nested: customerInfo lives under apiOrganization in the
		// real per-group response shape captured 2026-04.
		{"apiOrganization", "customerInfo"}, {"apiOrganization", "customer_info"},
		{"api_organization", "customerInfo"}, {"api_organization", "customer_info"},
		{"organization", "customerInfo"}, {"organization", "customer_info"},
		{"data", "customerInfo"}, {"data", "customer_info"},
	}
	keys := []string{"balance_usd", "balanceUsd", "balance", "pending_balance", "pendingBalance"}
	for _, path := range wrappers {
		m := root
		if len(path) > 0 {
			n, ok := providerutil.NestedMap(root, path...)
			if !ok {
				continue
			}
			m = n
		}
		if v, ok := providerutil.FirstFloat(m, keys...); ok {
			return math.Round(v * 100)
		}
	}
	return 0
}

// readSubscriptionTier returns "Pro" / "Max" / "Enterprise" / "" from the
// group payload. customerInfo.is_max / is_pro booleans take precedence
// over string fields (matches openusage's detectPlanLabel). Walks both
// top-level wrappers AND nested customerInfo combinations because the
// per-group response often nests the customerInfo under apiOrganization.
func readSubscriptionTier(root map[string]any) string {
	wrappers := [][]string{
		nil,
		{"customerInfo"}, {"customer_info"},
		{"apiOrganization"}, {"api_organization"},
		{"organization"}, {"org"},
		{"data"}, {"result"},
		{"apiOrganization", "customerInfo"}, {"apiOrganization", "customer_info"},
		{"api_organization", "customerInfo"}, {"api_organization", "customer_info"},
		{"organization", "customerInfo"}, {"organization", "customer_info"},
		{"data", "customerInfo"}, {"data", "customer_info"},
	}
	for _, path := range wrappers {
		m := root
		if len(path) > 0 {
			n, ok := providerutil.NestedMap(root, path...)
			if !ok {
				continue
			}
			m = n
		}
		if v, ok := m["is_max"].(bool); ok && v {
			return "Max"
		}
		if v, ok := m["isMax"].(bool); ok && v {
			return "Max"
		}
		if v, ok := m["is_pro"].(bool); ok && v {
			return "Pro"
		}
		if v, ok := m["isPro"].(bool); ok && v {
			return "Pro"
		}
		if tier := providerutil.FirstString(m, "subscriptionTier", "subscription_tier", "tier", "plan", "subscription"); tier != "" {
			return strings.TrimSpace(tier)
		}
	}
	return ""
}

// snapshotFromUsage maps Perplexity quotas into Stream Deck metrics.
//
// Rate-limit categories render as count metrics (full bar, count as
// the value) since /rest/rate-limit/all returns no daily caps to
// percent-against. Balance + spend render as dollar metrics, always
// emitted (even at $0) so users can see they have no API platform
// activity.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	var metrics []providers.MetricValue
	addCount := func(id, label, name string, remaining *int) {
		if remaining == nil {
			return
		}
		metrics = append(metrics, countMetric(id, label, name, *remaining, now))
	}
	addCount("pro-queries-remaining", "PRO", "Perplexity Pro queries remaining today", usage.ProRemaining)
	addCount("deep-research-remaining", "DEEP RSRCH.", "Perplexity Deep Research queries remaining today", usage.ResearchRemain)
	addCount("labs-remaining", "LABS", "Perplexity Labs queries remaining today", usage.LabsRemain)
	addCount("agentic-research-remaining", "AGENTIC", "Perplexity Agentic Research queries remaining today", usage.AgenticRemain)
	metrics = append(metrics,
		dollarMetric("comet-spend", "COMET", "Spend", "Perplexity Comet (computer-use) spend (all-time)", usage.CometSpendCents, "low", now),
		dollarMetric("api-balance", "API", "Balance", "Perplexity API balance", usage.BalanceCents, "high", now),
		dollarMetric("api-spend", "API", "Spend", "Perplexity API spend (all-time)", usage.SpendCents, "low", now),
	)
	return providers.Snapshot{
		ProviderID:   "perplexity",
		ProviderName: providerName(usage),
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// countMetric builds a "remaining queries" count tile. The count itself
// is the prominent value; the bar renders full because /rest/rate-limit
// doesn't expose the daily cap (a guessed cap would mis-fill the bar
// when wrong, which is worse than a static full bar). Caption mirrors
// the Grok pattern — title carries the feature name (PRO / DEEP RSRCH. /
// LABS / AGENTIC), subtitle is the constant "Queries" so a row of
// Perplexity tiles reads as parallel.
func countMetric(id, label, name string, remaining int, now string) providers.MetricValue {
	val := float64(remaining)
	ratio := 1.0
	return providers.MetricValue{
		ID:              id,
		Label:           label,
		Name:            name,
		Value:           val,
		NumericValue:    &val,
		NumericUnit:     "count",
		NumericGoodWhen: "high",
		Ratio:           &ratio,
		Caption:         "Queries",
		UpdatedAt:       now,
	}
}

// dollarMetric renders a USD-valued tile (balance, spend). Always emitted
// — a $0.00 value is itself useful information for accounts with no API
// platform activity. Caption is a short word ("Balance" / "Spend") so
// api-balance and api-spend can share the "API" title and still
// disambiguate via the subtitle (same trick Grok uses for GROK 3
// queries vs GROK 3 tokens).
//
// goodWhen flips threshold semantics: "high" for balance (lower is
// worse — you're running out), "low" for spend (lower is better —
// you've spent less). With "low" + no NumericMax, the threshold
// defaults don't fire, so a $0 spend stays neutral rather than
// painting the tile critical-red on accounts with no API activity.
func dollarMetric(id, label, caption, name string, cents float64, goodWhen, now string) providers.MetricValue {
	dollars := cents / 100
	ratio := 1.0
	return providers.MetricValue{
		ID:              id,
		Label:           label,
		Name:            name,
		Value:           fmt.Sprintf("$%.2f", dollars),
		NumericValue:    &dollars,
		NumericUnit:     "dollars",
		NumericGoodWhen: goodWhen,
		Ratio:           &ratio,
		Caption:         caption,
		UpdatedAt:       now,
	}
}

// providerName decorates the display name with the inferred plan tier.
func providerName(usage usageSnapshot) string {
	tier := strings.ToLower(usage.SubscriptionTier)
	switch {
	case strings.Contains(tier, "max"):
		return "Perplexity Max"
	case strings.Contains(tier, "enterprise"):
		return "Perplexity Enterprise"
	case strings.Contains(tier, "pro"):
		return "Perplexity Pro"
	default:
		return "Perplexity"
	}
}

// mapHTTPError turns a Fetch error into the most useful provider snapshot.
// 401/403 → stale cookie; 404 with feature_not_available → "API removed";
// anything else → "Perplexity HTTP <code>" without dumping the body.
func mapHTTPError(err error) providers.Snapshot {
	var httpErr *httputil.Error
	if !errors.As(err, &httpErr) {
		return errorSnapshot(err.Error())
	}
	if httpErr.Status == 401 || httpErr.Status == 403 {
		return errorSnapshot(cookieaux.StaleMessage("perplexity.ai"))
	}
	if httpErr.Status == 404 && strings.Contains(httpErr.Body, "feature_not_available") {
		return errorSnapshot("Perplexity usage API not available on this account")
	}
	return errorSnapshot(fmt.Sprintf("Perplexity HTTP %d", httpErr.Status))
}

// errorSnapshot returns a Perplexity setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "perplexity",
		ProviderName: "Perplexity",
		Source:       "cookie",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// init registers the Perplexity provider with the package registry.
func init() {
	providers.Register(Provider{})
}

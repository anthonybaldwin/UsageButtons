// Package perplexity implements the Perplexity usage provider.
//
// Auth: Usage Buttons Helper extension with the user's perplexity.ai browser
// session. Endpoints (Perplexity removed /rest/billing/credits in 2026 —
// the new flow mirrors what perplexity.ai/account/usage itself fetches):
//
//	GET /rest/pplx-api/v2/groups            — pick first group ID
//	GET /rest/pplx-api/v2/groups/{groupId}  — balance + subscription tier
//	GET /rest/rate-limit/all                — per-tier query quotas
package perplexity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
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

	groupIDOverrideEnv = "USAGEBUTTONS_PERPLEXITY_GROUP_ID"

	// rateLimitDailyDefault is a conservative per-tier daily allotment used
	// to compute a percent-remaining value when the API only returns the
	// remaining count (no explicit limit). Pro plans currently grant a
	// shared 600/day pool — this is the visible-on-the-page default.
	rateLimitDailyDefault = 600
)

// usageSnapshot is the normalized Perplexity quota state.
type usageSnapshot struct {
	BalanceCents     float64
	SubscriptionTier string
	ProRemaining     *int
	ProLimit         int
	ResearchRemain   *int
	ResearchLimit    int
	LabsRemain       *int
	LabsLimit        int
	AgenticRemain    *int
	AgenticLimit     int
	UpdatedAt        time.Time
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
		"balance",
	}
}

// Fetch returns the latest Perplexity quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
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
	groupID, err := resolveGroupID(ctx, headers)
	if err != nil {
		return mapHTTPError(err), nil
	}
	groupMap := map[string]any{}
	if err := cookies.FetchJSON(ctx, groupURLBase+url.PathEscape(groupID), headers, &groupMap); err != nil {
		return mapHTTPError(err), nil
	}
	rateMap := map[string]any{}
	if err := cookies.FetchJSON(ctx, rateLimitURL, headers, &rateMap); err != nil {
		return mapHTTPError(err), nil
	}
	return snapshotFromUsage(usageFromResponses(groupMap, rateMap, time.Now().UTC())), nil
}

// resolveGroupID returns the user-overridden group ID or discovers the
// first group from the groups list endpoint.
func resolveGroupID(ctx context.Context, headers map[string]string) (string, error) {
	if override := strings.TrimSpace(os.Getenv(groupIDOverrideEnv)); override != "" {
		return override, nil
	}
	root := map[string]any{}
	if err := cookies.FetchJSON(ctx, groupsURL, headers, &root); err != nil {
		return "", err
	}
	if id := firstGroupID(root); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("Perplexity: no groups returned")
}

// firstGroupID walks the groups response looking for the first group's ID.
// Perplexity's payload nests the array under common envelope keys, and
// each entry may use id / group_id / groupId / uuid for the identifier.
func firstGroupID(root map[string]any) string {
	envelopes := [][]string{
		{"groups"}, {"data", "groups"}, {"result", "groups"},
		{"data"}, {"result"}, {"items"},
	}
	for _, path := range envelopes {
		if arr, ok := nestedArray(root, path...); ok {
			if id := firstIDFromArray(arr); id != "" {
				return id
			}
		}
	}
	return ""
}

// firstIDFromArray pulls the first identifier-shaped string out of an
// array of group-like objects.
func firstIDFromArray(arr []any) string {
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id := providerutil.FirstString(obj, "id", "group_id", "groupId", "uuid", "_id"); id != "" {
			return id
		}
	}
	return ""
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

// usageFromResponses normalizes group + rate-limit JSON into usageSnapshot.
func usageFromResponses(groupMap, rateMap map[string]any, now time.Time) usageSnapshot {
	usage := usageSnapshot{UpdatedAt: now}
	usage.BalanceCents = readBalanceCents(groupMap)
	usage.SubscriptionTier = readSubscriptionTier(groupMap)
	usage.ProRemaining, usage.ProLimit = readRateLimit(rateMap, "remaining_pro", "pro")
	usage.ResearchRemain, usage.ResearchLimit = readRateLimit(rateMap, "remaining_research", "research", "deep_research")
	usage.LabsRemain, usage.LabsLimit = readRateLimit(rateMap, "remaining_labs", "labs")
	usage.AgenticRemain, usage.AgenticLimit = readRateLimit(rateMap, "remaining_agentic_research", "agentic_research", "agentic")
	return usage
}

// readBalanceCents extracts a USD balance from the group payload's nested
// shape and converts to cents. Mirrors openusage's flexible lookup so we
// keep working when Perplexity tweaks wrapper key names.
func readBalanceCents(root map[string]any) float64 {
	wrappers := [][]string{
		nil,
		{"apiOrganization"},
		{"api_organization"},
		{"group"},
		{"org"},
		{"organization"},
		{"data"},
		{"result"},
		{"item"},
		{"customerInfo"},
		{"customer_info"},
		{"wallet"},
		{"billing"},
		{"usage"},
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
// group payload, matching the strings Perplexity uses on its own UI.
func readSubscriptionTier(root map[string]any) string {
	for _, path := range [][]string{
		nil, {"apiOrganization"}, {"organization"}, {"customerInfo"}, {"customer_info"}, {"data"}, {"result"},
	} {
		m := root
		if len(path) > 0 {
			n, ok := providerutil.NestedMap(root, path...)
			if !ok {
				continue
			}
			m = n
		}
		if tier := providerutil.FirstString(m, "subscriptionTier", "subscription_tier", "tier", "plan", "subscription"); tier != "" {
			return strings.TrimSpace(tier)
		}
	}
	return ""
}

// readRateLimit returns (remaining, limit) for one tier from the
// /rest/rate-limit/all payload. The endpoint may nest values under a
// `rateLimits` key or place them at the root, and the explicit limit
// may live in a different pool than the remaining count — so we
// search all pools for both.
func readRateLimit(root map[string]any, keys ...string) (*int, int) {
	pools := []map[string]any{root}
	for _, p := range []string{"rateLimits", "rate_limits", "data", "result"} {
		if nested, ok := providerutil.NestedMap(root, p); ok {
			pools = append(pools, nested)
		}
	}
	limitKeys := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		limitKeys = append(limitKeys, "limit_"+k, k+"_limit")
	}
	var remaining *int
	for _, m := range pools {
		if v, ok := providerutil.FirstFloat(m, keys...); ok {
			r := int(math.Round(math.Max(0, v)))
			remaining = &r
			break
		}
	}
	if remaining == nil {
		return nil, 0
	}
	limit := rateLimitDailyDefault
	for _, m := range pools {
		if lim, ok := providerutil.FirstFloat(m, limitKeys...); ok && lim > 0 {
			limit = int(math.Round(lim))
			break
		}
	}
	return remaining, limit
}

// snapshotFromUsage maps Perplexity quotas into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	var metrics []providers.MetricValue
	addRate := func(id, label, name string, remaining *int, limit int) {
		if remaining == nil {
			return
		}
		used := math.Max(0, float64(limit-*remaining))
		usedPct := 100.0
		if limit > 0 {
			usedPct = math.Max(0, math.Min(100, used/float64(limit)*100))
		}
		caption := fmt.Sprintf("%d/%d left", *remaining, limit)
		m := providerutil.PercentRemainingMetric(id, label, name, usedPct, nil, caption, now)
		m = providerutil.RawCounts(m, *remaining, limit)
		metrics = append(metrics, m)
	}
	addRate("pro-queries-remaining", "QUERIES", "Perplexity Pro queries remaining", usage.ProRemaining, usage.ProLimit)
	addRate("deep-research-remaining", "DEEP", "Perplexity Deep Research queries remaining", usage.ResearchRemain, usage.ResearchLimit)
	addRate("labs-remaining", "LABS", "Perplexity Labs queries remaining", usage.LabsRemain, usage.LabsLimit)
	addRate("agentic-research-remaining", "AGENTIC", "Perplexity Agentic Research queries remaining", usage.AgenticRemain, usage.AgenticLimit)
	if usage.BalanceCents > 0 || len(metrics) == 0 {
		metrics = append(metrics, balanceMetric(usage, now))
	}
	return providers.Snapshot{
		ProviderID:   "perplexity",
		ProviderName: providerName(usage),
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// balanceMetric renders the API balance as a USD-formatted card.
func balanceMetric(usage usageSnapshot, now string) providers.MetricValue {
	dollars := usage.BalanceCents / 100
	caption := fmt.Sprintf("$%.2f", dollars)
	m := providerutil.PercentRemainingMetric("balance", "BALANCE", "Perplexity API balance",
		0, nil, caption, now)
	cents := int(math.Round(usage.BalanceCents))
	m.RawCount = &cents
	return m
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

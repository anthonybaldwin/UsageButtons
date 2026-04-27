// Package cursor implements the Cursor usage provider.
//
// Auth: Browser cookie pasted from cursor.com DevTools.
// Endpoint: GET https://cursor.com/api/usage-summary
package cursor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
)

// legacyPlanState caches the result of the one-time legacy-plan probe
// so modern Cursor accounts (the vast majority post-2024) don't pay
// the 2-call /api/auth/me + /api/usage tax on every poll. State is
// process-lifetime — a Stream Deck restart re-probes, which is enough
// to catch the rare case where a user's plan type genuinely changes.
var (
	legacyMu       sync.Mutex
	legacyKnown    bool // true once we've successfully classified the account
	accountIsModern bool // true when /api/usage definitively returned no legacy data
)

// rememberLegacyPlan classifies the account: legacy=nil after a clean
// fetch means modern; legacy non-nil means grandfathered. Errors and
// timeouts leave the state "unknown" so we re-probe next tick.
func rememberLegacyPlan(legacy *legacyModelUsage, fetchHadError bool) {
	if fetchHadError {
		return
	}
	legacyMu.Lock()
	defer legacyMu.Unlock()
	legacyKnown = true
	accountIsModern = legacy == nil
}

// shouldProbeLegacy returns whether to call /api/auth/me + /api/usage
// on this tick. Returns false once we've definitively classified the
// account as modern.
func shouldProbeLegacy() bool {
	legacyMu.Lock()
	defer legacyMu.Unlock()
	if legacyKnown && accountIsModern {
		return false
	}
	return true
}

// resetLegacyPlanCache clears the classification — test-only.
func resetLegacyPlanCache() {
	legacyMu.Lock()
	defer legacyMu.Unlock()
	legacyKnown = false
	accountIsModern = false
}

const (
	// usageSummaryURL is the primary Cursor usage-summary endpoint.
	usageSummaryURL = "https://cursor.com/api/usage-summary"
	// authMeURL identifies the signed-in user for legacy-plan lookups.
	authMeURL = "https://cursor.com/api/auth/me"
	// legacyUsageURL returns request-based usage for grandfathered plans.
	legacyUsageURL = "https://cursor.com/api/usage"
)

// --- API response types ---

// planUsage captures the plan-level usage block of a usage-summary response.
type planUsage struct {
	Enabled          *bool    `json:"enabled"`
	Used             *float64 `json:"used"`  // cents
	Limit            *float64 `json:"limit"` // cents
	Remaining        *float64 `json:"remaining"`
	TotalPercentUsed *float64 `json:"totalPercentUsed"`
	AutoPercentUsed  *float64 `json:"autoPercentUsed"`
	APIPercentUsed   *float64 `json:"apiPercentUsed"`
}

// authMeResponse carries the subscriber ID required for the legacy endpoint.
type authMeResponse struct {
	Sub *string `json:"sub"`
}

// legacyModelUsage is one model's quota in the legacy usage response.
type legacyModelUsage struct {
	NumRequests      *int `json:"numRequests"`
	NumRequestsTotal *int `json:"numRequestsTotal"`
	MaxRequestUsage  *int `json:"maxRequestUsage"`
}

// legacyUsageResponse is the wrapper returned by /api/usage.
type legacyUsageResponse struct {
	GPT4 *legacyModelUsage `json:"gpt-4"`
}

// onDemandUsage tracks overage spend for on-demand models.
type onDemandUsage struct {
	Enabled   *bool    `json:"enabled"`
	Used      *float64 `json:"used"`      // cents
	Limit     *float64 `json:"limit"`     // cents
	Remaining *float64 `json:"remaining"` // cents
}

// usageSummaryResponse is the top-level shape of /api/usage-summary.
type usageSummaryResponse struct {
	BillingCycleStart *string `json:"billingCycleStart"`
	BillingCycleEnd   *string `json:"billingCycleEnd"`
	MembershipType    *string `json:"membershipType"`
	IndividualUsage   *struct {
		Plan     *planUsage     `json:"plan"`
		OnDemand *onDemandUsage `json:"onDemand"`
	} `json:"individualUsage"`
	TeamUsage *struct {
		OnDemand *onDemandUsage `json:"onDemand"`
	} `json:"teamUsage"`
}

// resetFromCycleEnd parses a billing-cycle-end string into a delta in
// seconds from now, or returns nil when the input can't be parsed.
func resetFromCycleEnd(cycleEnd *string) *float64 {
	if cycleEnd == nil || *cycleEnd == "" {
		return nil
	}
	d, err := time.Parse(time.RFC3339, *cycleEnd)
	if err != nil {
		// Try other common formats
		d, err = time.Parse("2006-01-02T15:04:05Z", *cycleEnd)
		if err != nil {
			d, err = time.Parse("2006-01-02", *cycleEnd)
			if err != nil {
				return nil
			}
		}
	}
	delta := d.Sub(time.Now()).Seconds()
	if delta < 0 {
		delta = 0
	}
	return &delta
}

// --- Provider implementation ---

// Provider fetches Cursor usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "cursor" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Cursor" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#00bfa5" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#06221f" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"total-percent", "auto-percent", "api-percent", "ondemand-spent", "team-ondemand-spent"}
}

// Fetch returns the latest Cursor usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if !cookies.HostAvailable(ctx) {
		return providers.Snapshot{
			ProviderID:   "cursor",
			ProviderName: "Cursor",
			Source:       "none",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        cookieaux.MissingMessage("cursor.com"),
		}, nil
	}

	var resp usageSummaryResponse
	err := cookies.FetchJSON(ctx, usageSummaryURL, nil, &resp)

	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return providers.Snapshot{
				ProviderID:   "cursor",
				ProviderName: "Cursor",
				Source:       "cookie",
				Metrics:      []providers.MetricValue{},
				Status:       "unknown",
				Error:        cookieaux.StaleMessage("cursor.com"),
			}, nil
		}
		return providers.Snapshot{}, err
	}

	// Legacy request-based plan detection: only probe when the account
	// hasn't been classified yet. Once we've seen a clean modern-user
	// answer (404 from /api/usage or no Sub from /api/auth/me) we skip
	// these two calls entirely — the typical post-2024 Cursor user pays
	// zero ongoing cost for a code path that doesn't apply to them.
	// Process-lifetime cache: a Stream Deck restart re-probes, which is
	// enough to catch the rare plan-type change.
	var legacy *legacyModelUsage
	if shouldProbeLegacy() {
		legacyCtx, legacyCancel := context.WithTimeout(ctx, 3*time.Second)
		var legacyErr error
		legacy, legacyErr = fetchLegacyUsage(legacyCtx)
		legacyCancel()
		rememberLegacyPlan(legacy, legacyErr != nil)
	}

	var metrics []providers.MetricValue
	now := time.Now().UTC().Format(time.RFC3339)
	resetSecs := resetFromCycleEnd(resp.BillingCycleEnd)

	if legacy != nil {
		// Legacy plan owns the TOTAL lane — plan.totalPercentUsed from
		// usage-summary is unreliable for these accounts.
		used := firstIntPtr(legacy.NumRequestsTotal, legacy.NumRequests)
		limit := *legacy.MaxRequestUsage
		usedPct := 0.0
		if limit > 0 {
			usedPct = float64(used) / float64(limit) * 100
			if usedPct > 100 {
				usedPct = 100
			}
		}
		remaining := 100 - usedPct
		ratio := remaining / 100
		rc := limit - used
		if rc < 0 {
			rc = 0
		}
		m := providers.MetricValue{
			ID:           "total-percent",
			Label:        "TOTAL",
			Name:         "Requests remaining",
			Value:        math.Round(remaining),
			NumericValue: &remaining,
			NumericUnit:  "percent",
			Unit:         "%",
			Ratio:        &ratio,
			Direction:    "up",
			RawCount:     &rc,
			RawMax:       &limit,
			Caption:      fmt.Sprintf("%d/%d requests", used, limit),
			UpdatedAt:    now,
		}
		if resetSecs != nil {
			m.ResetInSeconds = resetSecs
		}
		metrics = append(metrics, m)
	}

	if resp.IndividualUsage != nil && resp.IndividualUsage.Plan != nil {
		plan := resp.IndividualUsage.Plan

		// Total plan usage — skipped on legacy plans where the legacy path
		// already emitted total-percent above.
		if plan.TotalPercentUsed != nil && legacy == nil {
			remaining := remainingPercent(*plan.TotalPercentUsed)
			ratio := remaining / 100
			m := providers.MetricValue{
				ID:           "total-percent",
				Label:        "TOTAL",
				Name:         "Total plan usage remaining",
				Value:        math.Round(remaining),
				NumericValue: &remaining,
				NumericUnit:  "percent",
				Unit:         "%",
				Ratio:        &ratio,
				Direction:    "up",
				UpdatedAt:    now,
			}
			if resetSecs != nil {
				m.ResetInSeconds = resetSecs
			}
			metrics = append(metrics, m)
		}

		// Auto / Composer usage
		if plan.AutoPercentUsed != nil {
			remaining := remainingPercent(*plan.AutoPercentUsed)
			ratio := remaining / 100
			m := providers.MetricValue{
				ID:           "auto-percent",
				Label:        "AUTO",
				Name:         "Auto usage remaining",
				Value:        math.Round(remaining),
				NumericValue: &remaining,
				NumericUnit:  "percent",
				Unit:         "%",
				Ratio:        &ratio,
				Direction:    "up",
				UpdatedAt:    now,
			}
			if resetSecs != nil {
				m.ResetInSeconds = resetSecs
			}
			metrics = append(metrics, m)
		}

		// API / Named model usage
		if plan.APIPercentUsed != nil {
			remaining := remainingPercent(*plan.APIPercentUsed)
			ratio := remaining / 100
			m := providers.MetricValue{
				ID:           "api-percent",
				Label:        "API",
				Name:         "API usage remaining",
				Value:        math.Round(remaining),
				NumericValue: &remaining,
				NumericUnit:  "percent",
				Unit:         "%",
				Ratio:        &ratio,
				Direction:    "up",
				UpdatedAt:    now,
			}
			if resetSecs != nil {
				m.ResetInSeconds = resetSecs
			}
			metrics = append(metrics, m)
		}
	}

	// On-demand spend
	if resp.IndividualUsage != nil && resp.IndividualUsage.OnDemand != nil {
		od := resp.IndividualUsage.OnDemand
		if m := onDemandMetric("ondemand-spent", "ON-DEMAND", "On-demand spend", od, now); m != nil {
			metrics = append(metrics, *m)
		}
	}
	if resp.TeamUsage != nil && resp.TeamUsage.OnDemand != nil {
		if m := onDemandMetric("team-ondemand-spent", "TEAM", "Team on-demand spend", resp.TeamUsage.OnDemand, now); m != nil {
			metrics = append(metrics, *m)
		}
	}

	planLabel := "Cursor"
	if resp.MembershipType != nil && *resp.MembershipType != "" {
		planLabel = formatMembershipType(*resp.MembershipType)
	}

	return providers.Snapshot{
		ProviderID:   "cursor",
		ProviderName: planLabel,
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// remainingPercent clamps an upstream used percentage and returns its remaining percentage.
func remainingPercent(used float64) float64 {
	return 100 - math.Max(0, math.Min(100, used))
}

// firstIntPtr returns the first non-nil int pointer value from vals.
func firstIntPtr(vals ...*int) int {
	for _, v := range vals {
		if v != nil {
			return *v
		}
	}
	return 0
}

// onDemandMetric converts a Cursor on-demand usage block to a dollar metric.
func onDemandMetric(id, label, name string, od *onDemandUsage, now string) *providers.MetricValue {
	if od == nil || od.Enabled == nil || !*od.Enabled || od.Used == nil {
		return nil
	}
	spentDollars := *od.Used / 100
	m := providers.MetricValue{
		ID:              id,
		Label:           label,
		Name:            name,
		Value:           fmt.Sprintf("$%.2f", spentDollars),
		NumericValue:    &spentDollars,
		NumericUnit:     "dollars",
		NumericGoodWhen: "low",
		UpdatedAt:       now,
	}
	if od.Limit != nil {
		limitDollars := *od.Limit / 100
		if limitDollars > 0 {
			ratio := math.Min(1, spentDollars/limitDollars)
			m.NumericMax = &limitDollars
			m.Ratio = &ratio
			m.Direction = "up"
		}
		m.Caption = fmt.Sprintf("of $%.0f", limitDollars)
	} else {
		m.Caption = "Unlimited"
	}
	return &m
}

// formatMembershipType returns Cursor's display label for a membership type.
func formatMembershipType(mt string) string {
	switch strings.ToLower(mt) {
	case "enterprise":
		return "Cursor Enterprise"
	case "pro":
		return "Cursor Pro"
	case "hobby":
		return "Cursor Hobby"
	case "team":
		return "Cursor Team"
	default:
		return "Cursor " + upperFirst(mt)
	}
}

// fetchLegacyUsage returns the legacy gpt-4 request counts when the
// account is on a grandfathered request-based plan, or (nil, nil) when
// the account is modern (probe completed cleanly with no legacy data).
// Returns (nil, error) only on transient/network failure, so callers
// can distinguish "this is a modern user, cache the result" from "the
// fetch failed, try again next tick" — the legacy-plan cache only
// updates on the former.
func fetchLegacyUsage(ctx context.Context) (*legacyModelUsage, error) {
	var me authMeResponse
	if err := cookies.FetchJSON(ctx, authMeURL, nil, &me); err != nil {
		// 401/403 = stale cookie, 404 = endpoint gone for this account
		// — both are "definitive no" answers safe to cache. Other
		// errors (network, timeout) leave the cache untouched.
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403 || httpErr.Status == 404) {
			return nil, nil
		}
		return nil, err
	}
	if me.Sub == nil || *me.Sub == "" {
		return nil, nil
	}
	var usage legacyUsageResponse
	qs := url.Values{"user": []string{*me.Sub}}
	endpoint := legacyUsageURL + "?" + qs.Encode()
	if err := cookies.FetchJSON(ctx, endpoint, nil, &usage); err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && httpErr.Status == 404 {
			return nil, nil // modern account — endpoint doesn't exist for them
		}
		return nil, err
	}
	if usage.GPT4 == nil || usage.GPT4.MaxRequestUsage == nil ||
		(usage.GPT4.NumRequests == nil && usage.GPT4.NumRequestsTotal == nil) {
		return nil, nil
	}
	return usage.GPT4, nil
}

// upperFirst returns s with the first ASCII letter upper-cased.
func upperFirst(s string) string {
	if len(s) == 0 {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] = r[0] - 'a' + 'A'
	}
	return string(r)
}

// init registers the Cursor provider with the package registry.
func init() {
	providers.Register(Provider{})
}

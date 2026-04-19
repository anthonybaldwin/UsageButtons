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
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
)

const (
	usageSummaryURL = "https://cursor.com/api/usage-summary"
	authMeURL       = "https://cursor.com/api/auth/me"
	legacyUsageURL  = "https://cursor.com/api/usage"
)

// --- API response types ---

type planUsage struct {
	Enabled          *bool    `json:"enabled"`
	Used             *float64 `json:"used"`     // cents
	Limit            *float64 `json:"limit"`    // cents
	Remaining        *float64 `json:"remaining"`
	TotalPercentUsed *float64 `json:"totalPercentUsed"`
	AutoPercentUsed  *float64 `json:"autoPercentUsed"`
	APIPercentUsed   *float64 `json:"apiPercentUsed"`
}

type authMeResponse struct {
	Sub *string `json:"sub"`
}

type legacyModelUsage struct {
	NumRequests     *int `json:"numRequests"`
	MaxRequestUsage *int `json:"maxRequestUsage"`
}

type legacyUsageResponse struct {
	GPT4 *legacyModelUsage `json:"gpt-4"`
}

type onDemandUsage struct {
	Enabled   *bool    `json:"enabled"`
	Used      *float64 `json:"used"`      // cents
	Limit     *float64 `json:"limit"`     // cents
	Remaining *float64 `json:"remaining"` // cents
}

type usageSummaryResponse struct {
	BillingCycleStart *string `json:"billingCycleStart"`
	BillingCycleEnd   *string `json:"billingCycleEnd"`
	MembershipType    *string `json:"membershipType"`
	IndividualUsage   *struct {
		Plan     *planUsage     `json:"plan"`
		OnDemand *onDemandUsage `json:"onDemand"`
	} `json:"individualUsage"`
}

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

func (Provider) ID() string         { return "cursor" }
func (Provider) Name() string       { return "Cursor" }
func (Provider) BrandColor() string { return "#F54E00" }
func (Provider) BrandBg() string    { return "#1a0e06" }
func (Provider) MetricIDs() []string {
	return []string{"total-percent", "auto-percent", "api-percent", "ondemand-spent"}
}

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

	// Legacy request-based plan detection: fetch /api/auth/me for the user
	// sub, then /api/usage?user=SUB. Both calls tolerate failure — legacy
	// plans are grandfathered and the endpoint 404s for current users.
	// Run on a short child context so a slow auth/me or usage call can't
	// stall every Cursor refresh for modern accounts.
	legacyCtx, legacyCancel := context.WithTimeout(ctx, 3*time.Second)
	legacy := fetchLegacyUsage(legacyCtx)
	legacyCancel()

	var metrics []providers.MetricValue
	now := time.Now().UTC().Format(time.RFC3339)
	resetSecs := resetFromCycleEnd(resp.BillingCycleEnd)

	if legacy != nil {
		// Legacy plan owns the TOTAL lane — plan.totalPercentUsed from
		// usage-summary is unreliable for these accounts.
		used := *legacy.NumRequests
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
			remaining := 100 - *plan.TotalPercentUsed
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
			remaining := 100 - *plan.AutoPercentUsed
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
			remaining := 100 - *plan.APIPercentUsed
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
		if od.Enabled != nil && *od.Enabled && od.Used != nil {
			spentDollars := *od.Used / 100
			m := providers.MetricValue{
				ID:              "ondemand-spent",
				Label:           "ON-DEMAND",
				Name:            "On-demand spend",
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
			metrics = append(metrics, m)
		}
	}

	planLabel := "Cursor"
	if resp.MembershipType != nil && *resp.MembershipType != "" {
		mt := *resp.MembershipType
		planLabel = "Cursor " + upperFirst(mt)
	}

	return providers.Snapshot{
		ProviderID:   "cursor",
		ProviderName: planLabel,
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// fetchLegacyUsage returns the legacy gpt-4 request counts when the account
// is on a grandfathered request-based plan, or nil otherwise. Any failure
// (no sub, 404, decode error) returns nil so normal parsing proceeds.
func fetchLegacyUsage(ctx context.Context) *legacyModelUsage {
	var me authMeResponse
	if err := cookies.FetchJSON(ctx, authMeURL, nil, &me); err != nil {
		return nil
	}
	if me.Sub == nil || *me.Sub == "" {
		return nil
	}
	var usage legacyUsageResponse
	qs := url.Values{"user": []string{*me.Sub}}
	endpoint := legacyUsageURL + "?" + qs.Encode()
	if err := cookies.FetchJSON(ctx, endpoint, nil, &usage); err != nil {
		return nil
	}
	if usage.GPT4 == nil || usage.GPT4.MaxRequestUsage == nil || usage.GPT4.NumRequests == nil {
		return nil
	}
	return usage.GPT4
}

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

func init() {
	providers.Register(Provider{})
}

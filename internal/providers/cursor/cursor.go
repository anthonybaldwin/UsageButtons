// Package cursor implements the Cursor usage provider.
//
// Auth: Browser cookie pasted from cursor.com DevTools.
// Endpoint: GET https://cursor.com/api/usage-summary
package cursor

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const usageSummaryURL = "https://cursor.com/api/usage-summary"

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
func (Provider) BrandColor() string { return "#00bfa5" }
func (Provider) MetricIDs() []string {
	return []string{"total-percent", "auto-percent", "api-percent", "ondemand-spent"}
}

func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	cs := settings.CursorSettings()
	if cs.CookieHeader == "" {
		return providers.Snapshot{
			ProviderID:   "cursor",
			ProviderName: "Cursor",
			Source:       "none",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Paste a Cookie header from cursor.com in Plugin Settings.",
		}, nil
	}

	var resp usageSummaryResponse
	err := httputil.GetJSON(usageSummaryURL, map[string]string{
		"Cookie": cs.CookieHeader,
		"Accept": "application/json",
	}, 15*time.Second, &resp)

	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return providers.Snapshot{
				ProviderID:   "cursor",
				ProviderName: "Cursor",
				Source:       "cookie",
				Metrics:      []providers.MetricValue{},
				Status:       "unknown",
				Error:        "Cursor cookie expired. Paste a fresh one from cursor.com.",
			}, nil
		}
		return providers.Snapshot{}, err
	}

	var metrics []providers.MetricValue
	now := time.Now().UTC().Format(time.RFC3339)
	resetSecs := resetFromCycleEnd(resp.BillingCycleEnd)

	if resp.IndividualUsage != nil && resp.IndividualUsage.Plan != nil {
		plan := resp.IndividualUsage.Plan

		// Total plan usage
		if plan.TotalPercentUsed != nil {
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
				ratio := math.Min(1, spentDollars/limitDollars)
				m.NumericMax = &limitDollars
				m.Ratio = &ratio
				m.Direction = "up"
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

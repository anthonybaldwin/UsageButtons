// Package perplexity implements the Perplexity usage provider.
//
// Auth: Usage Buttons Helper extension with the user's perplexity.ai browser
// session. Endpoint: GET /rest/billing/credits.
package perplexity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const creditsURL = "https://www.perplexity.ai/rest/billing/credits?version=2.18&source=default"

// creditsResponse is Perplexity's credits endpoint payload.
type creditsResponse struct {
	BalanceCents                float64       `json:"balance_cents"`
	RenewalDateTS               float64       `json:"renewal_date_ts"`
	CurrentPeriodPurchasedCents float64       `json:"current_period_purchased_cents"`
	CreditGrants                []creditGrant `json:"credit_grants"`
	TotalUsageCents             float64       `json:"total_usage_cents"`
}

// creditGrant is one Perplexity grant bucket.
type creditGrant struct {
	Type        string   `json:"type"`
	AmountCents float64  `json:"amount_cents"`
	ExpiresAtTS *float64 `json:"expires_at_ts"`
}

// usageSnapshot is the normalized Perplexity credit state.
type usageSnapshot struct {
	RecurringTotal float64
	RecurringUsed  float64
	PromoTotal     float64
	PromoUsed      float64
	PurchasedTotal float64
	PurchasedUsed  float64
	RenewalDate    time.Time
	PromoExpiresAt *time.Time
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
	return []string{"session-percent", "weekly-percent", "opus-percent"}
}

// Fetch returns the latest Perplexity quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("perplexity.ai")), nil
	}
	var resp creditsResponse
	err := cookies.FetchJSON(ctx, creditsURL, map[string]string{
		"Accept":  "application/json",
		"Origin":  "https://www.perplexity.ai",
		"Referer": "https://www.perplexity.ai/account/usage",
	}, &resp)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot(cookieaux.StaleMessage("perplexity.ai")), nil
		}
		return providers.Snapshot{}, err
	}
	return snapshotFromUsage(usageFromResponse(resp)), nil
}

// usageFromResponse attributes Perplexity usage across credit grant pools.
func usageFromResponse(resp creditsResponse) usageSnapshot {
	now := time.Now().UTC()
	var recurringSum, promoSum, purchasedFromGrants float64
	var promoExpiry *time.Time
	for _, grant := range resp.CreditGrants {
		amount := math.Max(0, grant.AmountCents)
		switch strings.ToLower(strings.TrimSpace(grant.Type)) {
		case "recurring":
			recurringSum += amount
		case "promotional":
			if grant.ExpiresAtTS == nil || *grant.ExpiresAtTS > float64(now.Unix()) {
				promoSum += amount
				if grant.ExpiresAtTS != nil {
					expires := unixSeconds(*grant.ExpiresAtTS)
					if promoExpiry == nil || expires.Before(*promoExpiry) {
						promoExpiry = &expires
					}
				}
			}
		case "purchased":
			purchasedFromGrants += amount
		}
	}
	purchasedSum := math.Max(purchasedFromGrants, math.Max(0, resp.CurrentPeriodPurchasedCents))

	remainingUsage := math.Max(0, resp.TotalUsageCents)
	recurringUsed := math.Min(remainingUsage, recurringSum)
	remainingUsage -= recurringUsed
	purchasedUsed := math.Min(remainingUsage, purchasedSum)
	remainingUsage -= purchasedUsed
	promoUsed := math.Min(remainingUsage, promoSum)

	return usageSnapshot{
		RecurringTotal: recurringSum,
		RecurringUsed:  recurringUsed,
		PromoTotal:     promoSum,
		PromoUsed:      promoUsed,
		PurchasedTotal: purchasedSum,
		PurchasedUsed:  purchasedUsed,
		RenewalDate:    unixSeconds(resp.RenewalDateTS),
		PromoExpiresAt: promoExpiry,
		UpdatedAt:      now,
	}
}

// snapshotFromUsage maps Perplexity credit pools into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	var metrics []providers.MetricValue
	if usage.RecurringTotal > 0 {
		metrics = append(metrics, creditMetric(
			"session-percent",
			"CREDITS",
			"Perplexity recurring credits remaining",
			usage.RecurringUsed,
			usage.RecurringTotal,
			&usage.RenewalDate,
			"",
			now,
		))
	}
	metrics = append(metrics, creditMetric(
		"weekly-percent",
		"BONUS",
		"Perplexity bonus credits remaining",
		usage.PromoUsed,
		usage.PromoTotal,
		nil,
		promoCaption(usage),
		now,
	))
	metrics = append(metrics, creditMetric(
		"opus-percent",
		"PURCHASED",
		"Perplexity purchased credits remaining",
		usage.PurchasedUsed,
		usage.PurchasedTotal,
		nil,
		"",
		now,
	))
	return providers.Snapshot{
		ProviderID:   "perplexity",
		ProviderName: providerName(usage),
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// creditMetric builds one Perplexity remaining-credit metric.
func creditMetric(id, label, name string, used, total float64, resetAt *time.Time, extraCaption string, now string) providers.MetricValue {
	usedPct := 100.0
	if total > 0 {
		usedPct = math.Max(0, math.Min(100, used/total*100))
	}
	caption := fmt.Sprintf("%s/%s credits", wholeNumber(used), wholeNumber(total))
	if extraCaption != "" {
		caption += " " + extraCaption
	}
	m := providerutil.PercentRemainingMetric(id, label, name, usedPct, resetAt, caption, now)
	if total > 0 {
		remaining := int(math.Round(math.Max(0, total-used)))
		maximum := int(math.Round(total))
		m.RawCount = &remaining
		m.RawMax = &maximum
	}
	return m
}

// promoCaption returns a compact bonus expiry note.
func promoCaption(usage usageSnapshot) string {
	if usage.PromoExpiresAt == nil {
		return ""
	}
	return "exp. " + usage.PromoExpiresAt.Format("Jan 2")
}

// providerName returns Perplexity with an inferred plan when available.
func providerName(usage usageSnapshot) string {
	switch {
	case usage.RecurringTotal <= 0:
		return "Perplexity"
	case usage.RecurringTotal < 5000:
		return "Perplexity Pro"
	default:
		return "Perplexity Max"
	}
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

// unixSeconds converts Unix seconds into time.Time.
func unixSeconds(seconds float64) time.Time {
	return time.Unix(0, int64(seconds*1_000_000_000)).UTC()
}

// wholeNumber formats credit counts without decimals.
func wholeNumber(value float64) string {
	return fmt.Sprintf("%.0f", math.Round(value))
}

// init registers the Perplexity provider with the package registry.
func init() {
	providers.Register(Provider{})
}

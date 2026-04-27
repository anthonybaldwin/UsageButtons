// Package hermes implements the Nous Research portal usage provider.
//
// Auth: Usage Buttons Helper extension with the user's
// portal.nousresearch.com browser session (cookies). Branded "Hermes"
// after the Hermes Agent product, but the same subscription pool funds
// both Hermes Agent and Nous Chat — see the IMPORTANT note in the
// portal's API-keys page: "Subscription credits are for Nous Chat and
// Hermes Agent and do not count towards direct API access."
//
// Endpoints (all GET, both server-render the relevant JSON inline):
//
//	GET /products  — subscription tier, monthly credits, balance,
//	                  rollover cap, renewal date, API credits balance
//	GET /api-keys  — totals.{spend,requests,tokens} for the API tier
//
// We deliberately avoid the Server Action POST /usage for v1: its
// `Next-Action` header value is a content hash that rotates on every
// Nous deploy, which would mean shipping a plugin update every time
// the portal redeploys. The server-rendered HTML is stable, public,
// and exposes the figures we need — so we read them directly.
package hermes

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const (
	productsURL = "https://portal.nousresearch.com/products"
	apiKeysURL  = "https://portal.nousresearch.com/api-keys"
	provID      = "hermes"
	provName    = "Hermes"
)

// Provider fetches Nous Research portal usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return provID }

// Name returns the human-readable provider name.
func (Provider) Name() string { return provName }

// BrandColor returns the accent color (Nous portal teal-700).
func (Provider) BrandColor() string { return "#0f766e" }

// BrandBg returns the background color (deep complement of the teal).
func (Provider) BrandBg() string { return "#042f2e" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{
		"hermes-sub-credits-remaining",
		"hermes-api-credits-remaining",
		"hermes-api-spend-total",
		"hermes-api-requests-total",
	}
}

// Fetch returns the latest Nous Research usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("nousresearch.com")), nil
	}
	headers := map[string]string{
		"Accept":  "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Origin":  "https://portal.nousresearch.com",
		"Referer": "https://portal.nousresearch.com/",
	}

	products, err := fetchHTML(ctx, productsURL, headers)
	if err != nil {
		return mapHTTPError(err), nil
	}
	usage := snapshotFromHTML(products, time.Now().UTC())

	// /api-keys is best-effort: an account with zero API activity still
	// renders the page, but if the request fails we still emit the
	// subscription tile from /products. The api-* metrics simply omit.
	if api, err := fetchHTML(ctx, apiKeysURL, headers); err == nil {
		mergeAPIKeysHTML(api, &usage)
	}
	return snapshotToProvider(usage), nil
}

// usageSnapshot is the parsed Nous portal state.
type usageSnapshot struct {
	// SubBalanceCents is the current subscription credits remaining.
	SubBalanceCents float64
	// SubRolloverCents is the rolled-over subscription credits balance,
	// already included in SubBalanceCents but surfaced separately so
	// the caption can call it out.
	SubRolloverCents float64
	// SubMonthlyCents is the active tier's monthly credit grant
	// (e.g. $22.00 on Plus). Used as the meter denominator.
	SubMonthlyCents float64
	// SubRolloverCapCents is the maximum rollover the tier permits
	// (e.g. $10.00 on Plus). Surfaced in the caption when non-zero.
	SubRolloverCapCents float64
	// APIBalanceCents is the standalone API-credits balance, separate
	// from the subscription pool.
	APIBalanceCents float64
	// APISpendCents is total API platform spend (all-time, all keys).
	APISpendCents float64
	// APIRequests is total API platform request count (all-time).
	APIRequests int
	// Tier is the active subscription tier name ("Plus", "Free", ...).
	Tier string
	// RenewsAt is the subscription renewal timestamp (currentPeriodEnd).
	RenewsAt *time.Time
	// HasAPIData is set when /api-keys was successfully parsed; used to
	// gate emission of the api-* metrics (don't render $0 when we
	// don't actually know the value).
	HasAPIData bool
	UpdatedAt  time.Time
}

// snapshotFromHTML parses /products into the subscription portion of
// the snapshot. Quiet on missing fields so a Free-tier account (no
// active subscription) still produces a valid empty snapshot.
func snapshotFromHTML(html []byte, now time.Time) usageSnapshot {
	u := usageSnapshot{UpdatedAt: now}
	src := string(html)
	if m := reSubCreditsBalance.FindStringSubmatch(src); len(m) == 3 {
		u.SubBalanceCents = math.Round(parseFloat(m[1]) * 100)
		u.SubRolloverCents = math.Round(parseFloat(m[2]) * 100)
	}
	if m := reActiveSubMonthly.FindStringSubmatch(src); len(m) == 3 {
		u.SubMonthlyCents = math.Round(parseFloat(m[1]) * 100)
		u.SubRolloverCapCents = math.Round(parseFloat(m[2]) * 100)
	}
	if m := reActiveSubTier.FindStringSubmatch(src); len(m) == 2 {
		u.Tier = m[1]
	}
	if m := reActiveSubRenewsAt.FindStringSubmatch(src); len(m) == 2 {
		if t, err := time.Parse(time.RFC3339, m[1]); err == nil {
			u.RenewsAt = &t
		}
	}
	if m := reAPICreditsBalance.FindStringSubmatch(src); len(m) == 2 {
		u.APIBalanceCents = math.Round(parseFloat(m[1]) * 100)
	}
	return u
}

// mergeAPIKeysHTML pulls totals.{spend,requests} from the /api-keys
// page's embedded usageByKey block. Mutates u in place.
func mergeAPIKeysHTML(html []byte, u *usageSnapshot) {
	src := string(html)
	if m := reAPITotals.FindStringSubmatch(src); len(m) == 8 {
		u.APIRequests, _ = strconv.Atoi(m[7])
		u.APISpendCents = math.Round(parseFloat(m[6]) * 100)
		u.HasAPIData = true
	}
}

// Pre-compiled regexes for the Nous portal's inline-JSON shape. The
// data is rendered as a JS-stringified payload inside `self.__next_f.
// push([1, "..."])` so all double quotes appear escaped (`\"`) and the
// regexes match the escaped form. Numeric capture groups are intentionally
// permissive (`[\d.]+`) because the API uses bare floats AND ints
// interchangeably (e.g. monthlyCredits=22 vs balance=21.998392).
//
// activeSubscription anchors prevent collisions with the per-tier
// monthlyCredits in the availableTiers array: the activeSubscription
// object is flat (no nested braces) so `[^{}]*` reliably keeps the
// match inside that one object.
var (
	reSubCreditsBalance = regexp.MustCompile(`\\"subscriptionCredits\\":\{\\"balance\\":([\d.]+),\\"rolloverCredits\\":([\d.]+)\}`)
	reActiveSubMonthly  = regexp.MustCompile(`\\"activeSubscription\\":\{[^{}]*\\"monthlyCredits\\":([\d.]+),\\"maxRolloverCredits\\":([\d.]+)`)
	reActiveSubTier     = regexp.MustCompile(`\\"activeSubscription\\":\{[^{}]*\\"tier\\":\\"([^"\\]+)\\"`)
	reActiveSubRenewsAt = regexp.MustCompile(`\\"activeSubscription\\":\{[^{}]*\\"currentPeriodEnd\\":\\"([^"\\]+)\\"`)
	reAPICreditsBalance = regexp.MustCompile(`\\"apiCreditsBalance\\":([\d.]+)`)
	reAPITotals         = regexp.MustCompile(`\\"totals\\":\{\\"tokens\\":(\d+),\\"inputTokens\\":(\d+),\\"outputTokens\\":(\d+),\\"cacheReadTokens\\":(\d+),\\"cacheWriteTokens\\":(\d+),\\"spend\\":([\d.]+),\\"requests\\":(\d+)\}`)
)

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// snapshotToProvider maps the parsed state into the registry-shaped
// snapshot. Subscription metric is always emitted when we recognised
// any subscription state at all; API metrics only emit when /api-keys
// returned data so a Free-tier account with no API activity doesn't
// show three permanent $0/0 tiles.
func snapshotToProvider(u usageSnapshot) providers.Snapshot {
	now := u.UpdatedAt.Format(time.RFC3339)
	var metrics []providers.MetricValue

	if u.SubMonthlyCents > 0 {
		metrics = append(metrics, subCreditsMetric(u, now))
	}
	metrics = append(metrics, apiBalanceMetric(u, now))
	if u.HasAPIData {
		metrics = append(metrics, apiSpendMetric(u, now))
		metrics = append(metrics, apiRequestsMetric(u, now))
	}

	return providers.Snapshot{
		ProviderID:   provID,
		ProviderName: providerName(u),
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// subCreditsMetric renders the subscription-credits tile. Denominator
// is monthlyCredits + maxRolloverCredits so a fully-rolled-over balance
// can sit at 100% without overshooting. The renewal countdown is gated
// by ResetTimeWhenUsed so an idle account doesn't render a perpetual
// "27d to renewal" timer next to a full balance — once you've spent
// half your credits the countdown reappears as a "you'll roll over in
// X days" hint.
func subCreditsMetric(u usageSnapshot, now string) providers.MetricValue {
	balance := u.SubBalanceCents / 100
	cap := (u.SubMonthlyCents + u.SubRolloverCapCents) / 100
	ratio := math.Max(0, math.Min(1, balance/math.Max(cap, 0.01)))
	usedPct := math.Max(0, math.Min(100, (1-ratio)*100))
	resetSecs := renewSeconds(providerutil.ResetTimeWhenUsed(usedPct, u.RenewsAt), u.UpdatedAt)
	m := providers.MetricValue{
		ID:              "hermes-sub-credits-remaining",
		Label:           "SUB",
		Name:            "Nous subscription credits remaining",
		Value:           fmt.Sprintf("$%.2f", balance),
		NumericValue:    &balance,
		NumericUnit:     "dollars",
		NumericGoodWhen: "high",
		Ratio:           &ratio,
		Direction:       "up",
		Caption:         "Credits",
		UpdatedAt:       now,
	}
	if resetSecs != nil {
		m.ResetInSeconds = resetSecs
	}
	return m
}

// apiBalanceMetric renders the standalone API credits tile. Always
// emitted — a $0 balance is itself useful info for users who haven't
// topped up the API platform.
func apiBalanceMetric(u usageSnapshot, now string) providers.MetricValue {
	balance := u.APIBalanceCents / 100
	return providers.MetricValue{
		ID:              "hermes-api-credits-remaining",
		Label:           "API",
		Name:            "Nous API credits balance",
		Value:           fmt.Sprintf("$%.2f", balance),
		NumericValue:    &balance,
		NumericUnit:     "dollars",
		NumericGoodWhen: "high",
		Caption:         "Balance",
		UpdatedAt:       now,
	}
}

// apiSpendMetric renders the all-time API platform spend.
func apiSpendMetric(u usageSnapshot, now string) providers.MetricValue {
	spend := u.APISpendCents / 100
	return providers.MetricValue{
		ID:              "hermes-api-spend-total",
		Label:           "API",
		Name:            "Nous API spend (all-time)",
		Value:           fmt.Sprintf("$%.2f", spend),
		NumericValue:    &spend,
		NumericUnit:     "dollars",
		NumericGoodWhen: "low",
		Caption:         "Spend",
		UpdatedAt:       now,
	}
}

// apiRequestsMetric renders the all-time API request count.
func apiRequestsMetric(u usageSnapshot, now string) providers.MetricValue {
	v := float64(u.APIRequests)
	return providers.MetricValue{
		ID:              "hermes-api-requests-total",
		Label:           "API",
		Name:            "Nous API requests (all-time)",
		Value:           fmt.Sprintf("%d", u.APIRequests),
		NumericValue:    &v,
		NumericUnit:     "count",
		NumericGoodWhen: "high",
		Caption:         "Requests",
		UpdatedAt:       now,
	}
}

// renewSeconds returns seconds until renewAt, or nil when the date is
// missing or already past (so the renderer doesn't show a "reset in 0s"
// flash on an expired snapshot).
func renewSeconds(renewAt *time.Time, now time.Time) *float64 {
	if renewAt == nil {
		return nil
	}
	d := renewAt.Sub(now).Seconds()
	if d <= 0 {
		return nil
	}
	return &d
}

// providerName decorates the display name with the active tier when we
// know it. Free-tier or unknown accounts read as plain "Hermes".
func providerName(u usageSnapshot) string {
	if u.Tier == "" {
		return provName
	}
	return provName + " " + u.Tier
}

// fetchHTML wraps cookies.Fetch with HTTP-status checking so callers
// see a typed httputil.Error on non-2xx (mapHTTPError understands it).
func fetchHTML(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	resp, err := cookies.Fetch(ctx, cookies.Request{URL: url, Method: "GET", Headers: headers})
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, &httputil.Error{Status: resp.Status, StatusText: resp.StatusText, Body: string(resp.Body), URL: url}
	}
	return resp.Body, nil
}

// mapHTTPError converts a Fetch error into the most useful provider
// snapshot. 401/403 → stale cookie message; anything else → short
// "HTTP <code>" without leaking the response body.
func mapHTTPError(err error) providers.Snapshot {
	var httpErr *httputil.Error
	if !errors.As(err, &httpErr) {
		return errorSnapshot(err.Error())
	}
	if httpErr.Status == 401 || httpErr.Status == 403 {
		return errorSnapshot(cookieaux.StaleMessage("nousresearch.com"))
	}
	return errorSnapshot(fmt.Sprintf("Nous portal HTTP %d", httpErr.Status))
}

// errorSnapshot returns a setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   provID,
		ProviderName: provName,
		Source:       "cookie",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// init registers the Hermes provider with the package registry.
func init() {
	providers.Register(Provider{})
}

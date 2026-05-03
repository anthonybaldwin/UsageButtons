// Package deepseek implements the DeepSeek API usage provider.
//
// Auth: Property Inspector settings field or DEEPSEEK_API_KEY /
// DEEPSEEK_KEY environment variable.
// Endpoint: GET https://api.deepseek.com/user/balance
//
// Returns USD-preferred balance. Response carries balance per currency
// as decimal strings (DeepSeek serializes amounts as strings, not
// numbers); we parse them once and surface the spendable balance plus
// a paid/granted breakdown caption.
//
// FUTURE WORK — richer metrics via platform.deepseek.com (separate
// effort, requires extension allowlist + browser-bridge):
//
//   - GET /api/v0/users/get_user_summary       (account state)
//   - GET /api/v0/usage/cost?month=M&year=Y    (per-day cost, current/prior month)
//   - GET /api/v0/usage/amount?month=M&year=Y  (per-day token + request volume)
//   - GET /api/v0/users/get_api_keys           (api-key staleness from last_used)
//
// These would unlock today/yesterday/7d/MTD/burn/projected windows and
// monthly token volume. Auth model is a user web-token (not the
// `sk-…` API key — confirmed via console probe: API key returns 40002
// "Missing Token" against /api/v0/*). Plumbing requires:
//
//  1. chrome-extension/ allowlist entry for platform.deepseek.com
//  2. Token extraction from page localStorage on the extension side
//  3. A new fetch path in this package that prefers the bridge when
//     connected, falls back to the existing /user/balance API when not.
package deepseek

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	// balanceURL is the DeepSeek user-balance endpoint.
	balanceURL = "https://api.deepseek.com/user/balance"
	// fetchTimeout bounds the balance call.
	fetchTimeout = 15 * time.Second
)

// balanceResponse mirrors the /user/balance JSON envelope.
type balanceResponse struct {
	IsAvailable  bool          `json:"is_available"`
	BalanceInfos []balanceInfo `json:"balance_infos"`
}

// balanceInfo is one currency's balance entry. DeepSeek serializes
// amounts as decimal strings, not numbers, so the fields stay typed
// as strings until parseBalance converts them.
type balanceInfo struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

// parsed holds the numeric form of one balanceInfo plus its currency
// symbol for display.
type parsed struct {
	currency string
	symbol   string
	total    float64
	granted  float64
	toppedUp float64
}

// getAPIKey resolves a DeepSeek API key from user settings or env vars.
func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().DeepSeekKey,
		"DEEPSEEK_API_KEY", "DEEPSEEK_KEY",
	)
}

// Provider fetches DeepSeek balance data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "deepseek" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "DeepSeek" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#4d6bfe" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#0a1330" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"balance", "topped-up", "granted"}
}

// Fetch returns the latest DeepSeek balance snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providerutil.MissingAuthSnapshot(
			"deepseek",
			"DeepSeek",
			"Enter a DeepSeek API key in the DeepSeek tab, or set DEEPSEEK_API_KEY.",
		), nil
	}

	var resp balanceResponse
	err := httputil.GetJSON(balanceURL, map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Accept":        "application/json",
	}, fetchTimeout, &resp)
	if err != nil {
		return providers.Snapshot{}, err
	}

	info, err := pickBalance(resp.BalanceInfos)
	if err != nil {
		return providers.Snapshot{
			ProviderID:   "deepseek",
			ProviderName: "DeepSeek",
			Source:       "api-key",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        err.Error(),
		}, nil
	}

	now := providerutil.NowString()
	metrics := buildMetrics(info, resp.IsAvailable, now)

	return providers.Snapshot{
		ProviderID:   "deepseek",
		ProviderName: "DeepSeek",
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// pickBalance prefers the USD entry, falling back to the first
// available currency. Returns an error when the response carries no
// balance entries or the numeric strings can't be parsed.
func pickBalance(entries []balanceInfo) (parsed, error) {
	if len(entries) == 0 {
		return parsed{}, errors.New("DeepSeek balance response carried no balance_infos entries")
	}
	pick := entries[0]
	for _, e := range entries {
		if strings.EqualFold(e.Currency, "USD") {
			pick = e
			break
		}
	}

	total, err1 := strconv.ParseFloat(strings.TrimSpace(pick.TotalBalance), 64)
	granted, err2 := strconv.ParseFloat(strings.TrimSpace(pick.GrantedBalance), 64)
	toppedUp, err3 := strconv.ParseFloat(strings.TrimSpace(pick.ToppedUpBalance), 64)
	if err := errors.Join(err1, err2, err3); err != nil {
		return parsed{}, fmt.Errorf("DeepSeek balance has non-numeric value: %w", err)
	}

	symbol := "$"
	if strings.EqualFold(pick.Currency, "CNY") {
		symbol = "¥"
	}
	return parsed{
		currency: strings.ToUpper(strings.TrimSpace(pick.Currency)),
		symbol:   symbol,
		total:    total,
		granted:  granted,
		toppedUp: toppedUp,
	}, nil
}

// buildMetrics converts a parsed balance into renderable metrics.
//
// Primary "balance" tile shows the spendable total with a paid/granted
// breakdown caption. The two breakdown tiles (paid + granted) are
// optional secondary buttons users can pin if they care about which
// portion of the balance came from a top-up vs. a free grant.
//
// Account state shapes the primary tile: when the balance is zero we
// surface a "Add credits" caption; when the API marks the account as
// not-available-for-API-calls (suspended/quota-exceeded), we say so
// and skip the paid/granted breakdown so it doesn't read as fine.
func buildMetrics(p parsed, isAvailable bool, now string) []providers.MetricValue {
	balance := math.Round(p.total*100) / 100
	caption := fmt.Sprintf("Paid %s%.2f / Granted %s%.2f", p.symbol, p.toppedUp, p.symbol, p.granted)
	switch {
	case p.total <= 0:
		caption = "Add credits at platform.deepseek.com"
	case !isAvailable:
		caption = "Balance unavailable for API calls"
	}

	out := []providers.MetricValue{
		{
			ID:              "balance",
			Label:           "BALANCE",
			Name:            "DeepSeek balance",
			Value:           fmt.Sprintf("%s%.2f", p.symbol, balance),
			NumericValue:    &balance,
			NumericUnit:     "dollars",
			NumericGoodWhen: "high",
			Caption:         caption,
			UpdatedAt:       now,
		},
	}

	if p.toppedUp > 0 {
		paid := math.Round(p.toppedUp*100) / 100
		out = append(out, providers.MetricValue{
			ID:              "topped-up",
			Label:           "PAID",
			Name:            "DeepSeek paid balance",
			Value:           fmt.Sprintf("%s%.2f", p.symbol, paid),
			NumericValue:    &paid,
			NumericUnit:     "dollars",
			NumericGoodWhen: "high",
			Caption:         "Top-up balance",
			UpdatedAt:       now,
		})
	}
	if p.granted > 0 {
		gr := math.Round(p.granted*100) / 100
		out = append(out, providers.MetricValue{
			ID:              "granted",
			Label:           "GRANTED",
			Name:            "DeepSeek granted balance",
			Value:           fmt.Sprintf("%s%.2f", p.symbol, gr),
			NumericValue:    &gr,
			NumericUnit:     "dollars",
			NumericGoodWhen: "high",
			Caption:         "Free grant",
			UpdatedAt:       now,
		})
	}
	return out
}

// init registers the DeepSeek provider with the package registry.
func init() {
	providers.Register(Provider{})
}

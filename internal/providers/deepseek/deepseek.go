// Package deepseek implements the DeepSeek usage provider.
//
// Two fetch paths share one provider:
//
//  1. Platform-bridge path — when the Usage Buttons Helper is connected,
//     we fetch platform.deepseek.com's internal /api/v0 endpoints.
//     The extension auto-attaches the Authorization Bearer header
//     (read from localStorage["userToken"] in any open platform tab)
//     and the static x-app-version header. Surfaces the rich window-
//     based metrics (today/yesterday/7d/mtd/30d/burn/projected) plus
//     monthly token volume.
//
//  2. API-key fallback — when the bridge is unavailable or no platform
//     tab is open, we fall back to api.deepseek.com/user/balance with
//     a Bearer DEEPSEEK_API_KEY. The fallback only carries balance
//     and granted/paid breakdown; the cost-window tiles surface as
//     zero with a "needs Helper extension" caption.
//
// Both paths emit the same MetricIDs slice — keys that have no source
// in the active path are returned with zero values + an explanatory
// caption rather than dropped, so PI metric pickers stay stable
// regardless of which path is live.
package deepseek

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	// balanceURL is the public DeepSeek user-balance endpoint (sk-... auth).
	balanceURL = "https://api.deepseek.com/user/balance"

	// platformBase is the platform.deepseek.com web API base. The bridge
	// auto-attaches Authorization (read from the platform tab's
	// localStorage["userToken"]) and x-app-version per request.
	platformBase = "https://platform.deepseek.com"

	// fetchTimeout bounds each upstream call.
	fetchTimeout = 15 * time.Second
)

// balanceResponse mirrors api.deepseek.com/user/balance.
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

// parsedBalance holds the numeric form of one balanceInfo plus its
// currency symbol for display.
type parsedBalance struct {
	currency string
	symbol   string
	total    float64
	granted  float64
	toppedUp float64
}

// platformCostResponse is the envelope of /api/v0/usage/cost.
type platformCostResponse struct {
	Code int                 `json:"code"`
	Msg  string              `json:"msg"`
	Data platformCostDataEnv `json:"data"`
}

type platformCostDataEnv struct {
	BizCode int                  `json:"biz_code"`
	BizMsg  string               `json:"biz_msg"`
	BizData []platformCostBucket `json:"biz_data"`
}

// platformCostBucket is one currency's monthly cost rollup — total
// per model for the month plus a per-day breakdown.
type platformCostBucket struct {
	Currency string             `json:"currency"`
	Total    []platformModelUse `json:"total"`
	Days     []platformDayCost  `json:"days"`
}

type platformDayCost struct {
	Date string             `json:"date"`
	Data []platformModelUse `json:"data"`
}

type platformModelUse struct {
	Model string                  `json:"model"`
	Usage []platformUsageLineItem `json:"usage"`
}

type platformUsageLineItem struct {
	Type   string `json:"type"`
	Amount string `json:"amount"`
}

// platformSummaryResponse is the envelope of
// /api/v0/users/get_user_summary.
type platformSummaryResponse struct {
	Code int                    `json:"code"`
	Msg  string                 `json:"msg"`
	Data platformSummaryDataEnv `json:"data"`
}

type platformSummaryDataEnv struct {
	BizCode int                 `json:"biz_code"`
	BizMsg  string              `json:"biz_msg"`
	BizData platformSummaryData `json:"biz_data"`
}

type platformSummaryData struct {
	CurrentToken      int                   `json:"current_token"`
	MonthlyUsage      int                   `json:"monthly_usage"`
	TotalUsage        int                   `json:"total_usage"`
	MonthlyTokenUsage int                   `json:"monthly_token_usage"`
	NormalWallets     []platformWallet      `json:"normal_wallets"`
	BonusWallets      []platformWallet      `json:"bonus_wallets"`
	MonthlyCosts      []platformCostSummary `json:"monthly_costs"`
}

type platformWallet struct {
	Currency        string `json:"currency"`
	Balance         string `json:"balance"`
	TokenEstimation string `json:"token_estimation"`
}

type platformCostSummary struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
}

// getAPIKey resolves a DeepSeek API key from user settings or env vars.
func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().DeepSeekKey,
		"DEEPSEEK_API_KEY", "DEEPSEEK_KEY",
	)
}

// Provider fetches DeepSeek balance and (when the Helper extension is
// connected) richer per-day cost + token data from platform.deepseek.com.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "deepseek" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "DeepSeek" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#4d6bfe" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#0a1330" }

// MetricIDs enumerates the metrics this provider can emit. Both fetch
// paths emit the same set; metrics that have no source on the active
// path render as $0.00 with a "needs Helper extension" caption rather
// than disappearing, so PI metric pickers stay stable.
func (Provider) MetricIDs() []string {
	return []string{
		"balance",
		"topped-up",
		"granted",
		"cost-today",
		"cost-yesterday",
		"cost-7d",
		"cost-mtd",
		"cost-30d",
		"cost-burn-7d",
		"cost-projected-month",
		"tokens-mtd",
	}
}

// Fetch returns the latest DeepSeek snapshot. Prefers the platform
// bridge when the Helper extension is connected; falls back to the
// API-key /user/balance endpoint when not.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout*4)
	defer cancel()

	if cookies.HostAvailable(ctx) {
		snap, err := fetchPlatform(ctx)
		if err == nil {
			return snap, nil
		}
		// On platform error, transparently fall back to the API path
		// rather than failing the whole tile — but only if we have an
		// API key. Without a key, surface the platform error so the user
		// sees what happened.
		if getAPIKey() == "" {
			return platformErrorSnapshot(err), nil
		}
	}

	return fetchAPI(ctx)
}

// fetchAPI is the original /user/balance path — retained as the
// fallback when the Helper extension isn't connected.
func fetchAPI(_ context.Context) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providerutil.MissingAuthSnapshot(
			"deepseek",
			"DeepSeek",
			"Connect the Usage Buttons Helper extension (signed in to platform.deepseek.com) for full metrics, or enter a DeepSeek API key in the DeepSeek tab for balance only.",
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
	metrics := buildAPIMetrics(info, resp.IsAvailable, now)

	return providers.Snapshot{
		ProviderID:   "deepseek",
		ProviderName: "DeepSeek",
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// fetchPlatform runs the bridge path: pulls the current and prior
// month's daily cost arrays and the user-summary blob, then derives
// the seven cost windows + tokens-MTD + the three balance metrics.
func fetchPlatform(ctx context.Context) (providers.Snapshot, error) {
	now := time.Now().UTC()

	curr, err := fetchPlatformCost(ctx, now.Year(), int(now.Month()))
	if err != nil {
		return providers.Snapshot{}, err
	}
	prevMonthRef := now.AddDate(0, -1, 0)
	prev, err := fetchPlatformCost(ctx, prevMonthRef.Year(), int(prevMonthRef.Month()))
	if err != nil {
		// Prior-month fetch failure is not fatal — we just lose the
		// month-spanning portion of cost-7d and cost-30d. Keep going.
		prev = platformCostBucket{}
	}

	summary, err := fetchPlatformSummary(ctx)
	if err != nil {
		return providers.Snapshot{}, err
	}

	dailyCost, currency := mergeDailyCosts(curr, prev)
	w := bucketDailyCosts(dailyCost, now)
	bal := summaryToBalance(summary)
	tokens := float64(summary.MonthlyTokenUsage)

	return providers.Snapshot{
		ProviderID:   "deepseek",
		ProviderName: "DeepSeek",
		Source:       "browser",
		Metrics:      buildPlatformMetrics(bal, w, tokens, currency, providerutil.NowString()),
		Status:       "operational",
	}, nil
}

// fetchPlatformCost calls /api/v0/usage/cost?month=M&year=Y. Returns
// the first (typically only) currency bucket from biz_data, or an
// empty bucket if the response is empty/malformed.
func fetchPlatformCost(ctx context.Context, year, month int) (platformCostBucket, error) {
	url := fmt.Sprintf("%s/api/v0/usage/cost?month=%d&year=%d", platformBase, month, year)
	var resp platformCostResponse
	if err := cookies.FetchJSON(ctx, url, nil, &resp); err != nil {
		return platformCostBucket{}, err
	}
	if resp.Code != 0 {
		return platformCostBucket{}, fmt.Errorf("DeepSeek platform cost endpoint returned code=%d msg=%q",
			resp.Code, resp.Msg)
	}
	if len(resp.Data.BizData) == 0 {
		return platformCostBucket{}, nil
	}
	return resp.Data.BizData[0], nil
}

// fetchPlatformSummary calls /api/v0/users/get_user_summary.
func fetchPlatformSummary(ctx context.Context) (platformSummaryData, error) {
	url := platformBase + "/api/v0/users/get_user_summary"
	var resp platformSummaryResponse
	if err := cookies.FetchJSON(ctx, url, nil, &resp); err != nil {
		return platformSummaryData{}, err
	}
	if resp.Code != 0 {
		return platformSummaryData{}, fmt.Errorf("DeepSeek platform summary endpoint returned code=%d msg=%q",
			resp.Code, resp.Msg)
	}
	return resp.Data.BizData, nil
}

// mergeDailyCosts flattens current + prior month buckets into a
// date→USD map and returns the currency string from whichever bucket
// was populated. Inner per-type/per-model amounts are summed; we treat
// every line item as a cost regardless of type because the page itself
// totals them indiscriminately on the Expenses chart.
func mergeDailyCosts(curr, prev platformCostBucket) (map[string]float64, string) {
	out := map[string]float64{}
	currency := strings.ToUpper(strings.TrimSpace(curr.Currency))
	if currency == "" {
		currency = strings.ToUpper(strings.TrimSpace(prev.Currency))
	}
	if currency == "" {
		currency = "USD"
	}
	addAll := func(b platformCostBucket) {
		for _, d := range b.Days {
			date := strings.TrimSpace(d.Date)
			if date == "" {
				continue
			}
			for _, m := range d.Data {
				for _, u := range m.Usage {
					v, err := strconv.ParseFloat(strings.TrimSpace(u.Amount), 64)
					if err != nil {
						continue
					}
					out[date] += v
				}
			}
		}
	}
	addAll(prev)
	addAll(curr)
	return out, currency
}

// costWindows holds the per-window aggregates we care about plus the
// inputs needed to derive burn-rate and month-projection.
type costWindows struct {
	today       float64
	yesterday   float64
	last7d      float64
	mtd         float64
	last30d     float64
	daysElapsed int
	daysInMonth int
}

// bucketDailyCosts walks the date→USD map and accumulates spend across
// each window we expose. Date keys are interpreted as UTC YYYY-MM-DD
// (the platform reports days in UTC per the page's "All dates and
// times are UTC-based" disclaimer).
func bucketDailyCosts(daily map[string]float64, now time.Time) costWindows {
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yesterdayStart := todayStart.Add(-24 * time.Hour)
	sevenDaysAgo := todayStart.Add(-6 * 24 * time.Hour)
	thirtyDaysAgo := todayStart.Add(-29 * 24 * time.Hour)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	nextMonth := monthStart.AddDate(0, 1, 0)

	w := costWindows{
		daysElapsed: now.Day(),
		daysInMonth: int(nextMonth.Sub(monthStart).Hours() / 24),
	}
	for dateStr, v := range daily {
		d, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		if !d.Before(thirtyDaysAgo) {
			w.last30d += v
		}
		if !d.Before(monthStart) {
			w.mtd += v
		}
		if !d.Before(sevenDaysAgo) {
			w.last7d += v
		}
		if !d.Before(yesterdayStart) && d.Before(todayStart) {
			w.yesterday += v
		}
		if !d.Before(todayStart) {
			w.today += v
		}
	}
	return w
}

// summaryToBalance converts a /users/get_user_summary blob into the
// total/topped-up/granted USD triple. Wallet balances are decimal
// strings; we prefer the USD wallet, falling back to the first.
func summaryToBalance(s platformSummaryData) parsedBalance {
	pickWallet := func(ws []platformWallet) platformWallet {
		for _, w := range ws {
			if strings.EqualFold(w.Currency, "USD") {
				return w
			}
		}
		if len(ws) > 0 {
			return ws[0]
		}
		return platformWallet{}
	}
	normal := pickWallet(s.NormalWallets)
	bonus := pickWallet(s.BonusWallets)

	parse := func(v string) float64 {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0
		}
		return f
	}
	toppedUp := parse(normal.Balance)
	granted := parse(bonus.Balance)
	currency := strings.ToUpper(strings.TrimSpace(normal.Currency))
	if currency == "" {
		currency = strings.ToUpper(strings.TrimSpace(bonus.Currency))
	}
	if currency == "" {
		currency = "USD"
	}
	symbol := "$"
	if currency == "CNY" {
		symbol = "¥"
	}
	return parsedBalance{
		currency: currency,
		symbol:   symbol,
		total:    toppedUp + granted,
		granted:  granted,
		toppedUp: toppedUp,
	}
}

// pickBalance prefers the USD entry from the API-balance endpoint,
// falling back to the first available currency.
func pickBalance(entries []balanceInfo) (parsedBalance, error) {
	if len(entries) == 0 {
		return parsedBalance{}, errors.New("DeepSeek balance response carried no balance_infos entries")
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
		return parsedBalance{}, fmt.Errorf("DeepSeek balance has non-numeric value: %w", err)
	}

	symbol := "$"
	if strings.EqualFold(pick.Currency, "CNY") {
		symbol = "¥"
	}
	return parsedBalance{
		currency: strings.ToUpper(strings.TrimSpace(pick.Currency)),
		symbol:   symbol,
		total:    total,
		granted:  granted,
		toppedUp: toppedUp,
	}, nil
}

// buildPlatformMetrics packages the full set of 11 metrics emitted on
// the bridge path: balance/topped-up/granted plus the seven cost
// windows plus tokens-mtd.
func buildPlatformMetrics(b parsedBalance, w costWindows, tokensMTD float64, currency, now string) []providers.MetricValue {
	round := func(v float64) float64 { return math.Round(v*100) / 100 }
	t := round(w.today)
	y := round(w.yesterday)
	w7 := round(w.last7d)
	m := round(w.mtd)
	l30 := round(w.last30d)
	burn := round(w.last7d / 7.0)
	projected := m
	if w.daysElapsed >= 1 && w.daysInMonth > 0 {
		projected = round(w.mtd * float64(w.daysInMonth) / float64(w.daysElapsed))
	}
	bal := round(b.total)
	paid := round(b.toppedUp)
	gr := round(b.granted)
	tokens := round(tokensMTD)

	// Captions follow the Button Spec §7 convention: short,
	// source-tagged ("Cost (API)" mirrors Claude/Codex's "Cost
	// (local)"). The "API" tag also disambiguates this provider — the
	// data describes platform.deepseek.com / api.deepseek.com spend, NOT
	// chat.deepseek.com (the consumer chat product, which has no
	// public usage signal).
	const costCaption = "Cost (API)"
	_ = currency // currency is implicit in the symbol; reserved for future CNY/USD subtitle when symbol alone is insufficient

	return []providers.MetricValue{
		{
			ID:              "balance",
			Label:           "BALANCE",
			Name:            "DeepSeek API balance",
			Value:           fmt.Sprintf("%s%.2f", b.symbol, bal),
			NumericValue:    &bal,
			NumericUnit:     "dollars",
			NumericGoodWhen: "high",
			Caption:         fmt.Sprintf("Paid %s%.2f / Granted %s%.2f", b.symbol, paid, b.symbol, gr),
			UpdatedAt:       now,
		},
		{
			ID:              "topped-up",
			Label:           "PAID",
			Name:            "DeepSeek API paid balance",
			Value:           fmt.Sprintf("%s%.2f", b.symbol, paid),
			NumericValue:    &paid,
			NumericUnit:     "dollars",
			NumericGoodWhen: "high",
			Caption:         "Paid (API)",
			UpdatedAt:       now,
		},
		{
			ID:              "granted",
			Label:           "GRANTED",
			Name:            "DeepSeek API granted balance",
			Value:           fmt.Sprintf("%s%.2f", b.symbol, gr),
			NumericValue:    &gr,
			NumericUnit:     "dollars",
			NumericGoodWhen: "high",
			Caption:         "Granted (API)",
			UpdatedAt:       now,
		},
		{
			ID:              "cost-today",
			Label:           "TODAY",
			Name:            "DeepSeek API spend today (UTC)",
			Value:           fmt.Sprintf("%s%.2f", b.symbol, t),
			NumericValue:    &t,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         costCaption,
			UpdatedAt:       now,
		},
		{
			ID:              "cost-yesterday",
			Label:           "YESTERDAY",
			Name:            "DeepSeek API spend yesterday (UTC)",
			Value:           fmt.Sprintf("%s%.2f", b.symbol, y),
			NumericValue:    &y,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         costCaption,
			UpdatedAt:       now,
		},
		{
			ID:              "cost-7d",
			Label:           "7 DAYS",
			Name:            "DeepSeek API spend last 7 days",
			Value:           fmt.Sprintf("%s%.2f", b.symbol, w7),
			NumericValue:    &w7,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         costCaption,
			UpdatedAt:       now,
		},
		{
			ID:              "cost-mtd",
			Label:           "MTD",
			Name:            "DeepSeek API spend month-to-date (UTC)",
			Value:           fmt.Sprintf("%s%.2f", b.symbol, m),
			NumericValue:    &m,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         costCaption,
			UpdatedAt:       now,
		},
		{
			ID:              "cost-30d",
			Label:           "30 DAYS",
			Name:            "DeepSeek API spend last 30 days",
			Value:           fmt.Sprintf("%s%.2f", b.symbol, l30),
			NumericValue:    &l30,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         costCaption,
			UpdatedAt:       now,
		},
		{
			ID:              "cost-burn-7d",
			Label:           "BURN 7D",
			Name:            "Burn rate (7-day avg, $/day)",
			Value:           fmt.Sprintf("%s%.2f", b.symbol, burn),
			NumericValue:    &burn,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         "$/day (7d avg)",
			UpdatedAt:       now,
		},
		{
			ID:              "cost-projected-month",
			Label:           "PROJECTED",
			Name:            "Projected month total (MTD × daysInMonth/daysElapsed)",
			Value:           fmt.Sprintf("%s%.2f", b.symbol, projected),
			NumericValue:    &projected,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         "MTD pace",
			UpdatedAt:       now,
		},
		{
			ID:              "tokens-mtd",
			Label:           "TOKENS MTD",
			Name:            "DeepSeek API tokens used month-to-date",
			Value:           formatTokens(tokens),
			NumericValue:    &tokens,
			NumericUnit:     "tokens",
			NumericGoodWhen: "low",
			Caption:         "Tokens (API)",
			UpdatedAt:       now,
		},
	}
}

// buildAPIMetrics is the metric set surfaced when only the public
// /user/balance endpoint is available (no Helper extension). The
// cost-window metric IDs are also emitted (so the picker stays
// stable) but with a "Needs Helper" caption and zero values.
func buildAPIMetrics(p parsedBalance, isAvailable bool, now string) []providers.MetricValue {
	balance := math.Round(p.total*100) / 100
	caption := fmt.Sprintf("Paid %s%.2f / Granted %s%.2f", p.symbol, p.toppedUp, p.symbol, p.granted)
	switch {
	case p.total <= 0:
		caption = "Add credits at platform.deepseek.com"
	case !isAvailable:
		caption = "Balance unavailable"
	}

	out := []providers.MetricValue{
		{
			ID:              "balance",
			Label:           "BALANCE",
			Name:            "DeepSeek API balance",
			Value:           fmt.Sprintf("%s%.2f", p.symbol, balance),
			NumericValue:    &balance,
			NumericUnit:     "dollars",
			NumericGoodWhen: "high",
			Caption:         caption,
			UpdatedAt:       now,
		},
	}

	paid := math.Round(p.toppedUp*100) / 100
	out = append(out, providers.MetricValue{
		ID:              "topped-up",
		Label:           "PAID",
		Name:            "DeepSeek API paid balance",
		Value:           fmt.Sprintf("%s%.2f", p.symbol, paid),
		NumericValue:    &paid,
		NumericUnit:     "dollars",
		NumericGoodWhen: "high",
		Caption:         "Paid (API)",
		UpdatedAt:       now,
	})

	gr := math.Round(p.granted*100) / 100
	out = append(out, providers.MetricValue{
		ID:              "granted",
		Label:           "GRANTED",
		Name:            "DeepSeek API granted balance",
		Value:           fmt.Sprintf("%s%.2f", p.symbol, gr),
		NumericValue:    &gr,
		NumericUnit:     "dollars",
		NumericGoodWhen: "low",
		Caption:         "Granted (API)",
		UpdatedAt:       now,
	})

	// Stub the cost-window + token metrics with a short "Needs Helper"
	// caption — long-form explanation belongs in the tooltip and docs.
	const stubCaption = "Needs Helper"
	zero := 0.0
	for _, id := range []string{"cost-today", "cost-yesterday", "cost-7d", "cost-mtd", "cost-30d", "cost-burn-7d", "cost-projected-month"} {
		v := zero
		out = append(out, providers.MetricValue{
			ID:              id,
			Label:           costStubLabel(id),
			Name:            "DeepSeek API " + costStubName(id) + " (Helper extension required)",
			Value:           fmt.Sprintf("%s%.2f", p.symbol, v),
			NumericValue:    &v,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         stubCaption,
			UpdatedAt:       now,
		})
	}
	tokens := zero
	out = append(out, providers.MetricValue{
		ID:              "tokens-mtd",
		Label:           "TOKENS MTD",
		Name:            "DeepSeek API tokens used month-to-date (Helper extension required)",
		Value:           formatTokens(tokens),
		NumericValue:    &tokens,
		NumericUnit:     "tokens",
		NumericGoodWhen: "low",
		Caption:         stubCaption,
		UpdatedAt:       now,
	})

	return out
}

// platformErrorSnapshot wraps a platform-bridge error into a snapshot
// with a short caption per Button Spec §7. The full error string lives
// in Snapshot.Error for the Property Inspector and logs; on the tile
// itself we just say "Bridge error" so the subvalue stays one line.
func platformErrorSnapshot(err error) providers.Snapshot {
	now := providerutil.NowString()
	zero := 0.0
	caption := "Sign in to platform.deepseek.com"
	if err != nil {
		caption = "Bridge error"
	}
	out := []providers.MetricValue{
		{
			ID:              "balance",
			Label:           "BALANCE",
			Name:            "DeepSeek balance",
			Value:           "$0.00",
			NumericValue:    &zero,
			NumericUnit:     "dollars",
			NumericGoodWhen: "high",
			Caption:         caption,
			UpdatedAt:       now,
		},
	}
	for _, id := range []string{"topped-up", "granted", "cost-today", "cost-yesterday", "cost-7d", "cost-mtd", "cost-30d", "cost-burn-7d", "cost-projected-month"} {
		v := zero
		out = append(out, providers.MetricValue{
			ID:              id,
			Label:           costStubLabel(id),
			Name:            "DeepSeek " + costStubName(id),
			Value:           "$0.00",
			NumericValue:    &v,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         caption,
			UpdatedAt:       now,
		})
	}
	out = append(out, providers.MetricValue{
		ID:              "tokens-mtd",
		Label:           "TOKENS MTD",
		Name:            "DeepSeek tokens MTD",
		Value:           "0",
		NumericValue:    &zero,
		NumericUnit:     "tokens",
		NumericGoodWhen: "low",
		Caption:         caption,
		UpdatedAt:       now,
	})
	return providers.Snapshot{
		ProviderID:   "deepseek",
		ProviderName: "DeepSeek",
		Source:       "browser",
		Metrics:      out,
		Status:       "unknown",
		Error:        err.Error(),
	}
}

// costStubLabel returns the short tile label for a cost metric ID.
func costStubLabel(id string) string {
	switch id {
	case "topped-up":
		return "PAID"
	case "granted":
		return "GRANTED"
	case "cost-today":
		return "TODAY"
	case "cost-yesterday":
		return "YESTERDAY"
	case "cost-7d":
		return "7 DAYS"
	case "cost-mtd":
		return "MTD"
	case "cost-30d":
		return "30 DAYS"
	case "cost-burn-7d":
		return "BURN 7D"
	case "cost-projected-month":
		return "PROJECTED"
	}
	return strings.ToUpper(id)
}

// costStubName returns the long human-readable name for a cost metric ID.
func costStubName(id string) string {
	switch id {
	case "cost-today":
		return "spend today"
	case "cost-yesterday":
		return "spend yesterday"
	case "cost-7d":
		return "spend last 7 days"
	case "cost-mtd":
		return "spend month-to-date"
	case "cost-30d":
		return "spend last 30 days"
	case "cost-burn-7d":
		return "burn rate (7-day avg, $/day)"
	case "cost-projected-month":
		return "projected month total"
	}
	return id
}

// formatTokens renders a raw token count with a thousands separator
// (e.g. "12,345"). Whole-number formatting suits token counts better
// than the dollar formatting used elsewhere.
func formatTokens(v float64) string {
	n := int64(math.Round(v))
	s := strconv.FormatInt(n, 10)
	// Insert commas every three digits from the right.
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	prefixLen := len(s) % 3
	if prefixLen > 0 {
		b.WriteString(s[:prefixLen])
		if len(s) > prefixLen {
			b.WriteByte(',')
		}
	}
	for i := prefixLen; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

// init registers the DeepSeek provider with the package registry.
func init() {
	providers.Register(Provider{})
}

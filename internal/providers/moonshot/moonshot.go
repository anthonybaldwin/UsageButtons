// Package moonshot implements the Moonshot (api.moonshot.ai) usage
// provider. This is the Kimi developer platform — the paid API
// surface where Moonshot/Kimi sells direct-API inference for sk-...
// keys. It is not the same product as our existing `kimi` provider
// (which scrapes the kimi.com chat web product through the Helper
// extension): different account, different billing, different key.
//
// Auth: Property Inspector settings field or MOONSHOT_API_KEY /
// KIMI_PLATFORM_API_KEY environment variable.
//
// Endpoint: GET https://api.moonshot.ai/v1/users/me/balance
// (override host with MOONSHOT_API_HOST for the China region:
// api.moonshot.cn). Response carries available_balance,
// voucher_balance, and cash_balance as numeric floats.
//
// available_balance is the spendable total (cash + voucher); when it
// is <= 0, Moonshot disables inference for the account. cash_balance
// can go negative (the account "owes" Moonshot); when that happens,
// available_balance collapses to voucher_balance only.
package moonshot

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	// defaultBaseURL is the global Moonshot API host.
	defaultBaseURL = "https://api.moonshot.ai"
	// chinaBaseURL is the China-region host alternative. Selected via
	// MOONSHOT_API_HOST or settings.MoonshotAPIHost.
	chinaBaseURL = "https://api.moonshot.cn"
	// balancePath is the user balance endpoint relative to the base URL.
	balancePath = "/v1/users/me/balance"
	// fetchTimeout bounds the balance call.
	fetchTimeout = 15 * time.Second
)

// balanceResponse mirrors /v1/users/me/balance.
type balanceResponse struct {
	Code   int          `json:"code"`
	Data   *balanceData `json:"data"`
	Status bool         `json:"status"`
	Error  *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// balanceData carries the three balance buckets. Amounts are floats
// in USD per Moonshot's docs.
type balanceData struct {
	AvailableBalance float64 `json:"available_balance"`
	VoucherBalance   float64 `json:"voucher_balance"`
	CashBalance      float64 `json:"cash_balance"`
}

// getAPIKey resolves a Moonshot API key from user settings or env vars.
func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().MoonshotKey,
		"MOONSHOT_API_KEY", "KIMI_PLATFORM_API_KEY",
	)
}

// baseURL resolves the API base URL from user settings, env vars, or
// the default global host. Empty user override falls back to env then
// the global default.
func baseURL() string {
	return settings.ResolveEndpoint(
		settings.ProviderKeysGet().MoonshotAPIHost,
		defaultBaseURL,
		"MOONSHOT_API_HOST",
	)
}

// balanceURL returns the full URL of the balance endpoint, using the
// resolved base host.
func balanceURL() string { return baseURL() + balancePath }

// Provider fetches Moonshot balance data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "moonshot" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Moonshot" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#0a84ff" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#0c0d20" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"balance", "voucher", "cash"}
}

// Fetch returns the latest Moonshot balance snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providerutil.MissingAuthSnapshot(
			"moonshot",
			"Moonshot",
			"Enter a Moonshot API key in the Moonshot tab, or set MOONSHOT_API_KEY.",
		), nil
	}

	var resp balanceResponse
	err := httputil.GetJSON(balanceURL(), map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Accept":        "application/json",
	}, fetchTimeout, &resp)
	if err != nil {
		return providers.Snapshot{}, err
	}

	if resp.Data == nil {
		msg := "Moonshot balance response missing data"
		if resp.Error != nil && resp.Error.Message != "" {
			msg = "Moonshot API error: " + resp.Error.Message
		}
		return providers.Snapshot{}, errors.New(msg)
	}

	now := providerutil.NowString()
	metrics := buildMetrics(*resp.Data, now)

	return providers.Snapshot{
		ProviderID:   "moonshot",
		ProviderName: "Moonshot",
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// buildMetrics turns the parsed balance data into renderable metrics.
//
// The primary "balance" tile reports available_balance (Moonshot's
// own spendable total) with a caption that surfaces the voucher/cash
// split — or, when relevant, the account state (inference disabled
// at zero/negative; cash overdrawn when cash_balance went under).
// Voucher and cash tiles are optional secondaries the user can pin
// when they care which bucket is funding their usage.
func buildMetrics(d balanceData, now string) []providers.MetricValue {
	available := math.Round(d.AvailableBalance*100) / 100
	caption := fmt.Sprintf("Voucher $%.2f / Cash $%.2f", d.VoucherBalance, d.CashBalance)
	switch {
	case d.AvailableBalance <= 0:
		caption = "Inference disabled — top up at platform.kimi.ai"
	case d.CashBalance < 0:
		caption = fmt.Sprintf("Voucher $%.2f (cash overdrawn $%.2f)", d.VoucherBalance, math.Abs(d.CashBalance))
	}

	out := []providers.MetricValue{
		{
			ID:              "balance",
			Label:           "BALANCE",
			Name:            "Moonshot available balance",
			Value:           fmt.Sprintf("$%.2f", available),
			NumericValue:    &available,
			NumericUnit:     "dollars",
			NumericGoodWhen: "high",
			Caption:         caption,
			UpdatedAt:       now,
		},
	}

	if d.VoucherBalance > 0 {
		v := math.Round(d.VoucherBalance*100) / 100
		out = append(out, providers.MetricValue{
			ID:              "voucher",
			Label:           "VOUCHER",
			Name:            "Moonshot voucher balance",
			Value:           fmt.Sprintf("$%.2f", v),
			NumericValue:    &v,
			NumericUnit:     "dollars",
			NumericGoodWhen: "high",
			Caption:         "Promo / gift credit",
			UpdatedAt:       now,
		})
	}
	// Cash tile shows even when negative — that's the whole point of
	// surfacing it as its own metric (the user wants to see the deficit).
	if d.CashBalance != 0 {
		c := math.Round(d.CashBalance*100) / 100
		caption := "Prepaid balance"
		if d.CashBalance < 0 {
			caption = "Cash overdrawn"
		}
		out = append(out, providers.MetricValue{
			ID:              "cash",
			Label:           "CASH",
			Name:            "Moonshot cash balance",
			Value:           fmt.Sprintf("$%.2f", c),
			NumericValue:    &c,
			NumericUnit:     "dollars",
			NumericGoodWhen: "high",
			Caption:         caption,
			UpdatedAt:       now,
		})
	}
	return out
}

// init registers the Moonshot provider with the package registry.
func init() {
	providers.Register(Provider{})
}

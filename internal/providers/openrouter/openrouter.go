// Package openrouter implements the OpenRouter API usage provider.
//
// Auth: Property Inspector settings field or OPENROUTER_API_KEY env var.
// Endpoint: {base}/auth/credits where base comes from the PI settings
// override, the OPENROUTER_API_URL env var, or the default public URL.
package openrouter

import (
	"fmt"
	"math"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const defaultBaseURL = "https://openrouter.ai/api/v1"

type creditsResponse struct {
	Data *struct {
		TotalCredits *float64 `json:"total_credits"`
		TotalUsage   *float64 `json:"total_usage"`
	} `json:"data"`
}

func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().OpenRouterKey,
		"OPENROUTER_API_KEY",
	)
}

func creditsURL() string {
	base := settings.ResolveEndpoint(
		settings.ProviderKeysGet().OpenRouterURL,
		defaultBaseURL,
		"OPENROUTER_API_URL",
	)
	return base + "/auth/credits"
}

// Provider fetches OpenRouter usage data.
type Provider struct{}

func (Provider) ID() string         { return "openrouter" }
func (Provider) Name() string       { return "OpenRouter" }
func (Provider) BrandColor() string { return "#6467f2" }
func (Provider) BrandBg() string    { return "#101028" }
func (Provider) MetricIDs() []string {
	return []string{"credits-balance", "credits-used"}
}

func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providers.Snapshot{
			ProviderID:   "openrouter",
			ProviderName: "OpenRouter",
			Source:       "none",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Set OPENROUTER_API_KEY environment variable.",
		}, nil
	}

	var resp creditsResponse
	err := httputil.GetJSON(creditsURL(), map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Accept":        "application/json",
	}, 15*time.Second, &resp)
	if err != nil {
		return providers.Snapshot{}, err
	}

	totalCredits := 0.0
	totalUsage := 0.0
	if resp.Data != nil {
		if resp.Data.TotalCredits != nil {
			totalCredits = *resp.Data.TotalCredits
		}
		if resp.Data.TotalUsage != nil {
			totalUsage = *resp.Data.TotalUsage
		}
	}
	balance := totalCredits - totalUsage
	now := time.Now().UTC().Format(time.RFC3339)
	var metrics []providers.MetricValue

	metrics = append(metrics, providers.MetricValue{
		ID:              "credits-balance",
		Label:           "CREDITS",
		Name:            "OpenRouter credit balance",
		Value:           fmt.Sprintf("$%.2f", balance),
		NumericValue:    &balance,
		NumericUnit:     "dollars",
		NumericGoodWhen: "high",
		Caption:         "Balance",
		UpdatedAt:       now,
	})

	if totalUsage > 0 {
		m := providers.MetricValue{
			ID:              "credits-used",
			Label:           "USED",
			Name:            "OpenRouter total usage",
			Value:           fmt.Sprintf("$%.2f", totalUsage),
			NumericValue:    &totalUsage,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         "Lifetime",
			UpdatedAt:       now,
		}
		if totalCredits > 0 {
			ratio := math.Min(1, totalUsage/totalCredits)
			m.NumericMax = &totalCredits
			m.Ratio = &ratio
			m.Direction = "up"
		}
		metrics = append(metrics, m)
	}

	return providers.Snapshot{
		ProviderID:   "openrouter",
		ProviderName: "OpenRouter",
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

func init() {
	providers.Register(Provider{})
}

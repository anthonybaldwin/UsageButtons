// Package openrouter implements the OpenRouter API usage provider.
//
// Auth: OPENROUTER_API_KEY environment variable.
// Endpoint: GET https://openrouter.ai/api/v1/auth/credits
package openrouter

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

const creditsURL = "https://openrouter.ai/api/v1/auth/credits"

type creditsResponse struct {
	Data *struct {
		TotalCredits *float64 `json:"total_credits"`
		TotalUsage   *float64 `json:"total_usage"`
	} `json:"data"`
}

func getAPIKey() string {
	return strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
}

// Provider fetches OpenRouter usage data.
type Provider struct{}

func (Provider) ID() string         { return "openrouter" }
func (Provider) Name() string       { return "OpenRouter" }
func (Provider) BrandColor() string { return "#6467f2" }
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
	err := httputil.GetJSON(creditsURL, map[string]string{
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

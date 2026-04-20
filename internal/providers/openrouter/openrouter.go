// Package openrouter implements the OpenRouter API usage provider.
//
// Auth: Property Inspector settings field or OPENROUTER_API_KEY env var.
// Endpoint: {base}/auth/credits where base comes from the PI settings
// override, the OPENROUTER_API_URL env var, or the default public URL.
package openrouter

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	// defaultBaseURL is the public OpenRouter API base when no override is set.
	defaultBaseURL = "https://openrouter.ai/api/v1"
	// keyFetchTimeout bounds the optional /auth/key enrichment call.
	keyFetchTimeout = 1 * time.Second
)

// creditsResponse mirrors /auth/credits.
type creditsResponse struct {
	Data *struct {
		TotalCredits *float64 `json:"total_credits"`
		TotalUsage   *float64 `json:"total_usage"`
	} `json:"data"`
}

// keyResponse mirrors /auth/key; carries the key-specific quota and rate limit.
type keyResponse struct {
	Data *struct {
		Limit     *float64 `json:"limit"`
		Usage     *float64 `json:"usage"`
		RateLimit *struct {
			Requests *int    `json:"requests"`
			Interval *string `json:"interval"`
		} `json:"rate_limit"`
	} `json:"data"`
}

// getAPIKey resolves an OpenRouter API key from user settings or env vars.
func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().OpenRouterKey,
		"OPENROUTER_API_KEY",
	)
}

// baseURL resolves the API base URL from user settings, env vars, or the default.
func baseURL() string {
	return settings.ResolveEndpoint(
		settings.ProviderKeysGet().OpenRouterURL,
		defaultBaseURL,
		"OPENROUTER_API_URL",
	)
}

// creditsURL returns the full URL of the /auth/credits endpoint.
func creditsURL() string { return baseURL() + "/auth/credits" }

// keyURL returns the full URL of the /auth/key endpoint.
func keyURL() string { return baseURL() + "/auth/key" }

// fetchKeyInfo calls /auth/key with a tight timeout so a slow or absent
// endpoint can't delay the credits update. Any failure returns nil.
func fetchKeyInfo(apiKey string) *keyResponse {
	var resp keyResponse
	err := httputil.GetJSON(keyURL(), map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Accept":        "application/json",
	}, keyFetchTimeout, &resp)
	if err != nil || resp.Data == nil {
		return nil
	}
	return &resp
}

// Provider fetches OpenRouter usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "openrouter" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "OpenRouter" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#6467f2" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#101028" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"credits-balance", "credits-used", "key-percent", "rate-limit"}
}

// Fetch returns the latest OpenRouter credits + key snapshot.
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

	// Fire /credits (primary) and /key (enrichment) in parallel. /key has
	// a 1s timeout so a slow secondary endpoint can't delay the update.
	var (
		resp    creditsResponse
		keyInfo *keyResponse
		credErr error
		wg      sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		credErr = httputil.GetJSON(creditsURL(), map[string]string{
			"Authorization": "Bearer " + apiKey,
			"Accept":        "application/json",
		}, 15*time.Second, &resp)
	}()
	go func() {
		defer wg.Done()
		keyInfo = fetchKeyInfo(apiKey)
	}()
	wg.Wait()
	if credErr != nil {
		return providers.Snapshot{}, credErr
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

	// Key-specific quota (per-API-key cap, distinct from account credits).
	if keyInfo != nil && keyInfo.Data != nil &&
		keyInfo.Data.Limit != nil && *keyInfo.Data.Limit > 0 {
		used := 0.0
		if keyInfo.Data.Usage != nil {
			used = *keyInfo.Data.Usage
		}
		limit := *keyInfo.Data.Limit
		usedPct := math.Min(100, used/limit*100)
		remaining := 100 - usedPct
		ratio := remaining / 100
		metrics = append(metrics, providers.MetricValue{
			ID:           "key-percent",
			Label:        "KEY",
			Name:         "Key quota remaining",
			Value:        math.Round(remaining),
			NumericValue: &remaining,
			NumericUnit:  "percent",
			Unit:         "%",
			Ratio:        &ratio,
			Direction:    "up",
			Caption:      fmt.Sprintf("$%.2f of $%.2f", used, limit),
			UpdatedAt:    now,
		})
	}

	// Rate limit (informational — N requests per interval).
	if keyInfo != nil && keyInfo.Data != nil && keyInfo.Data.RateLimit != nil &&
		keyInfo.Data.RateLimit.Requests != nil && keyInfo.Data.RateLimit.Interval != nil {
		reqs := *keyInfo.Data.RateLimit.Requests
		interval := *keyInfo.Data.RateLimit.Interval
		metrics = append(metrics, providers.MetricValue{
			ID:        "rate-limit",
			Label:     "RATE",
			Name:      "OpenRouter rate limit",
			Value:     fmt.Sprintf("%d/%s", reqs, interval),
			Caption:   "requests",
			UpdatedAt: now,
		})
	}

	return providers.Snapshot{
		ProviderID:   "openrouter",
		ProviderName: "OpenRouter",
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// init registers the OpenRouter provider with the package registry.
func init() {
	providers.Register(Provider{})
}

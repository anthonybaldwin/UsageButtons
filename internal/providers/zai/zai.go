// Package zai implements the z.ai usage provider.
//
// Auth: Property Inspector settings field or Z_AI_API_KEY / ZAI_API_TOKEN
// / ZAI_API_KEY environment variable.
// Endpoint: {host}/api/monitor/usage/quota/limit where host is selected
// by the region picker (Global / BigModel CN) unless overridden via
// settings or Z_AI_API_HOST / Z_AI_QUOTA_URL.
package zai

import (
	"math"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	// defaultGlobalHost is the z.ai host used for the Global region.
	defaultGlobalHost = "https://api.z.ai"
	// defaultBigModelCNHost is the z.ai host used for the China region.
	defaultBigModelCNHost = "https://open.bigmodel.cn"
	// quotaPath is the usage quota endpoint path appended to the host.
	quotaPath = "/api/monitor/usage/quota/limit"
)

// --- API response types ---

// quotaLimit describes one lane (tokens, MCP minutes, etc.) in the
// z.ai quota response. Fields use pointers because the server sends
// different combinations depending on the lane type.
type quotaLimit struct {
	Type          *string  `json:"type"`
	Used          *float64 `json:"used"`
	Limit         *float64 `json:"limit"`
	ResetAt       *string  `json:"resetAt"`
	Unit          *int     `json:"unit"`          // 1=Days, 3=Hours, 5=Minutes
	Number        *int     `json:"number"`        // multiplier for unit
	Usage         *float64 `json:"usage"`
	CurrentValue  *float64 `json:"currentValue"`
	Remaining     *float64 `json:"remaining"`
	Percentage    *float64 `json:"percentage"`
	NextResetTime *int64   `json:"nextResetTime"` // epoch ms
}

// quotaResponse is the envelope returned by the z.ai quota endpoint; the
// limits array may be at the root or nested under data.
type quotaResponse struct {
	Limits *[]quotaLimit `json:"limits"`
	Data   *struct {
		Limits   *[]quotaLimit `json:"limits"`
		PlanName *string       `json:"plan_name"`
		Plan     *string       `json:"plan"`
		PlanType *string       `json:"plan_type"`
	} `json:"data"`
}

// getAPIToken resolves a z.ai API token from user settings or env vars.
func getAPIToken() string {
	// CodexBar uses Z_AI_API_KEY (with underscores); accept it and our
	// legacy ZAI_* names too.
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().ZaiKey,
		"Z_AI_API_KEY", "ZAI_API_TOKEN", "ZAI_API_KEY",
	)
}

// quotaURL resolves the endpoint to call. Settings-supplied Z_AI_QUOTA_URL
// (full URL) wins outright. Otherwise build {host}{quotaPath} where host
// comes from settings > Z_AI_API_HOST env > region picker > default.
func quotaURL() string {
	pk := settings.ProviderKeysGet()
	// Full URL override: settings > env > none
	if full := settings.ResolveEndpoint(pk.ZaiQuotaURL, "", "Z_AI_QUOTA_URL"); full != "" {
		return full
	}
	// Host override: settings > env > region > default
	host := settings.ResolveEndpoint(pk.ZaiHost, "", "Z_AI_API_HOST")
	if host == "" {
		host = regionHost(pk.ZaiRegion)
	}
	return host + quotaPath
}

// regionHost maps a user-facing region name to the matching host.
func regionHost(region string) string {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "bigmodel-cn", "bigmodel", "cn", "china":
		return defaultBigModelCNHost
	default:
		return defaultGlobalHost
	}
}

// resetSecondsFromLimit computes a reset delta (in seconds from now) from
// either nextResetTime or resetAt, returning nil when neither is present.
func resetSecondsFromLimit(limit quotaLimit) *float64 {
	// Try nextResetTime (epoch ms) first
	if limit.NextResetTime != nil {
		delta := float64(*limit.NextResetTime)/1000 - float64(time.Now().Unix())
		if delta < 0 {
			delta = 0
		}
		return &delta
	}
	// Fall back to resetAt (ISO string)
	if limit.ResetAt != nil && *limit.ResetAt != "" {
		if d, err := time.Parse(time.RFC3339, *limit.ResetAt); err == nil {
			delta := d.Sub(time.Now()).Seconds()
			if delta < 0 {
				delta = 0
			}
			return &delta
		}
	}
	return nil
}

// Provider fetches z.ai usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "zai" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "z.ai" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#ffffff" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#0c0c0c" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"tokens-percent", "mcp-percent"}
}

// Fetch returns the latest z.ai quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiToken := getAPIToken()
	if apiToken == "" {
		return providers.Snapshot{
			ProviderID:   "zai",
			ProviderName: "z.ai",
			Source:       "none",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Enter a z.ai API key in the z.ai tab, or set Z_AI_API_KEY.",
		}, nil
	}

	var resp quotaResponse
	err := httputil.GetJSON(quotaURL(), map[string]string{
		"Authorization": "Bearer " + apiToken,
		"Accept":        "application/json",
	}, 15*time.Second, &resp)
	if err != nil {
		return providers.Snapshot{}, err
	}

	// Limits can be at root or nested under data
	var limits []quotaLimit
	if resp.Limits != nil {
		limits = *resp.Limits
	} else if resp.Data != nil && resp.Data.Limits != nil {
		limits = *resp.Data.Limits
	}

	var planName string
	if resp.Data != nil {
		if resp.Data.PlanName != nil {
			planName = *resp.Data.PlanName
		} else if resp.Data.Plan != nil {
			planName = *resp.Data.Plan
		} else if resp.Data.PlanType != nil {
			planName = *resp.Data.PlanType
		}
	}

	// We'll collect token metrics first, then others
	var tokenMetrics []providers.MetricValue
	var otherMetrics []providers.MetricValue
	now := time.Now().UTC().Format(time.RFC3339)

	for _, limit := range limits {
		typeName := ""
		if limit.Type != nil {
			typeName = strings.ToLower(*limit.Type)
		}
		isTokens := strings.Contains(typeName, "token")
		isMcp := strings.Contains(typeName, "mcp") || strings.Contains(typeName, "time")

		// Resolve used value from multiple possible fields
		used := 0.0
		if limit.Used != nil {
			used = *limit.Used
		} else if limit.Usage != nil {
			used = *limit.Usage
		} else if limit.CurrentValue != nil {
			used = *limit.CurrentValue
		}

		cap := 0.0
		if limit.Limit != nil {
			cap = *limit.Limit
		}
		if cap <= 0 {
			continue
		}

		usedPct := math.Min(100, (used/cap)*100)
		remainPct := 100 - usedPct
		ratio := remainPct / 100
		resetSecs := resetSecondsFromLimit(limit)
		remaining := int(cap - used)
		if remaining < 0 {
			remaining = 0
		}
		capInt := int(cap)

		id := typeName + "-percent"
		label := strings.ToUpper(typeName)
		mName := typeName + " usage remaining"
		if isTokens {
			id = "tokens-percent"
			label = "TOKENS"
			mName = "Token usage remaining"
		} else if isMcp {
			id = "mcp-percent"
			label = "MCP"
			mName = "MCP usage remaining"
		}

		m := providers.MetricValue{
			ID:           id,
			Label:        label,
			Name:         mName,
			Value:        math.Round(remainPct),
			NumericValue: &remainPct,
			NumericUnit:  "percent",
			Unit:         "%",
			Ratio:        &ratio,
			Direction:    "up",
			RawCount:     &remaining,
			RawMax:       &capInt,
			UpdatedAt:    now,
		}
		if resetSecs != nil {
			m.ResetInSeconds = resetSecs
		}

		if isTokens {
			tokenMetrics = append(tokenMetrics, m)
		} else {
			otherMetrics = append(otherMetrics, m)
		}
	}

	// Tokens first, then others
	metrics := append(tokenMetrics, otherMetrics...)

	provName := "z.ai"
	if planName != "" {
		provName = "z.ai " + planName
	}

	return providers.Snapshot{
		ProviderID:   "zai",
		ProviderName: provName,
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// init registers the z.ai provider with the package registry.
func init() {
	providers.Register(Provider{})
}

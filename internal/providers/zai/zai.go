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
	"sort"
	"strconv"
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
	Unit          *int     `json:"unit"`   // 1=Days, 3=Hours, 5=Minutes
	Number        *int     `json:"number"` // multiplier for unit
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
		Limits           *[]quotaLimit `json:"limits"`
		PlanName         *string       `json:"plan_name"`
		Plan             *string       `json:"plan"`
		PlanType         *string       `json:"plan_type"`
		PackageName      *string       `json:"packageName"`
		PackageNameSnake *string       `json:"package_name"`
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
	return []string{"tokens-percent", "mcp-percent", "tokens-session-percent"}
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
		} else if resp.Data.PackageName != nil {
			planName = *resp.Data.PackageName
		} else if resp.Data.PackageNameSnake != nil {
			planName = *resp.Data.PackageNameSnake
		}
	}

	var tokenLimits []quotaLimit
	var otherLimits []quotaLimit
	for _, limit := range limits {
		if limitType(limit) == "tokens" {
			tokenLimits = append(tokenLimits, limit)
		} else {
			otherLimits = append(otherLimits, limit)
		}
	}
	sort.SliceStable(tokenLimits, func(i, j int) bool {
		return windowMinutes(tokenLimits[i]) < windowMinutes(tokenLimits[j])
	})

	var metrics []providers.MetricValue
	now := time.Now().UTC().Format(time.RFC3339)

	if len(tokenLimits) > 0 {
		primary := tokenLimits[len(tokenLimits)-1]
		if m := quotaMetric("tokens-percent", "TOKENS", "Token usage remaining", primary, now); m != nil {
			metrics = append(metrics, *m)
		}
		for _, limit := range tokenLimits {
			if windowMinutes(limit) != 5*60 {
				continue
			}
			if m := quotaMetric("tokens-session-percent", "5-HOUR", "5-hour token usage remaining", limit, now); m != nil {
				metrics = append(metrics, *m)
			}
			break
		}
	}
	for _, limit := range otherLimits {
		id, label, name := dynamicQuotaIdentity(limit)
		if m := quotaMetric(id, label, name, limit, now); m != nil {
			metrics = append(metrics, *m)
		}
	}

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

// limitType classifies a z.ai quota lane into provider-facing buckets.
func limitType(limit quotaLimit) string {
	typeName := ""
	if limit.Type != nil {
		typeName = strings.ToLower(*limit.Type)
	}
	switch {
	case strings.Contains(typeName, "token"):
		return "tokens"
	case strings.Contains(typeName, "mcp"), strings.Contains(typeName, "time"):
		return "mcp"
	default:
		return typeName
	}
}

// windowMinutes converts a z.ai unit/number pair to minutes for sorting.
func windowMinutes(limit quotaLimit) int {
	if limit.Number == nil || *limit.Number <= 0 {
		return math.MaxInt
	}
	switch {
	case limit.Unit == nil:
		return math.MaxInt
	case *limit.Unit == 5:
		return *limit.Number
	case *limit.Unit == 3:
		return *limit.Number * 60
	case *limit.Unit == 1:
		return *limit.Number * 24 * 60
	case *limit.Unit == 6:
		return *limit.Number * 7 * 24 * 60
	default:
		return math.MaxInt
	}
}

// quotaUsedAndCap resolves used and cap from the API's several quota field shapes.
func quotaUsedAndCap(limit quotaLimit) (used float64, cap float64, rawCounts bool, ok bool) {
	if limit.Limit != nil && *limit.Limit > 0 {
		cap = *limit.Limit
	} else if limit.Usage != nil && *limit.Usage > 0 {
		cap = *limit.Usage
	}
	if limit.Used != nil {
		used = *limit.Used
	} else if limit.CurrentValue != nil {
		used = *limit.CurrentValue
	} else if limit.Remaining != nil && cap > 0 {
		used = cap - *limit.Remaining
	}
	if cap <= 0 && limit.Percentage != nil {
		usedPct := math.Max(0, math.Min(100, *limit.Percentage))
		return usedPct, 100, false, true
	}
	if cap <= 0 {
		return 0, 0, false, false
	}
	return math.Max(0, math.Min(cap, used)), cap, true, true
}

// quotaMetric converts one z.ai quota lane to a remaining-percent metric.
func quotaMetric(id, label, name string, limit quotaLimit, now string) *providers.MetricValue {
	used, cap, rawCounts, ok := quotaUsedAndCap(limit)
	if !ok {
		return nil
	}
	usedPct := math.Min(100, (used/cap)*100)
	remainPct := 100 - usedPct
	ratio := remainPct / 100
	resetSecs := resetSecondsFromLimit(limit)
	remaining := int(math.Round(cap - used))
	if remaining < 0 {
		remaining = 0
	}
	capInt := int(math.Round(cap))
	m := providers.MetricValue{
		ID:           id,
		Label:        label,
		Name:         name,
		Value:        math.Round(remainPct),
		NumericValue: &remainPct,
		NumericUnit:  "percent",
		Unit:         "%",
		Ratio:        &ratio,
		Direction:    "up",
		UpdatedAt:    now,
	}
	if rawCounts {
		m.RawCount = &remaining
		m.RawMax = &capInt
	}
	if caption := windowCaption(limit); caption != "" {
		m.Caption = caption
	}
	if resetSecs != nil {
		m.ResetInSeconds = resetSecs
	}
	return &m
}

// dynamicQuotaIdentity returns a stable metric identity for non-token lanes.
func dynamicQuotaIdentity(limit quotaLimit) (id, label, name string) {
	switch limitType(limit) {
	case "mcp":
		return "mcp-percent", "MCP", "MCP usage remaining"
	default:
		typeName := "quota"
		if limit.Type != nil && strings.TrimSpace(*limit.Type) != "" {
			typeName = strings.ToLower(strings.TrimSpace(*limit.Type))
		}
		slug := strings.NewReplacer("_", "-", " ", "-").Replace(typeName)
		return slug + "-percent", strings.ToUpper(typeName), typeName + " usage remaining"
	}
}

// windowCaption returns a compact human label for a quota window length.
func windowCaption(limit quotaLimit) string {
	if limit.Number == nil || *limit.Number <= 0 || limit.Unit == nil {
		return ""
	}
	unit := ""
	switch *limit.Unit {
	case 5:
		unit = "minute"
	case 3:
		unit = "hour"
	case 1:
		unit = "day"
	case 6:
		unit = "week"
	}
	if unit == "" {
		return ""
	}
	if *limit.Number != 1 {
		unit += "s"
	}
	return strings.Join([]string{strconv.Itoa(*limit.Number), unit, "window"}, " ")
}

// init registers the z.ai provider with the package registry.
func init() {
	providers.Register(Provider{})
}

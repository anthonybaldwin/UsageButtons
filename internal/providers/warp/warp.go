// Package warp implements the Warp AI usage provider.
//
// Auth: WARP_API_KEY environment variable.
// Endpoint: POST https://app.warp.dev/graphql/v2?op=GetRequestLimitInfo
package warp

import (
	"math"
	"os"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

const graphqlURL = "https://app.warp.dev/graphql/v2?op=GetRequestLimitInfo"

const graphqlQuery = `
query GetRequestLimitInfo($requestContext: RequestContext!) {
  user(requestContext: $requestContext) {
    ... on AuthenticatedUser {
      user {
        requestLimitInfo {
          isUnlimited
          nextRefreshTime
          requestLimit
          requestsUsedSinceLastRefresh
        }
        bonusGrants {
          requestCreditsGranted
          requestCreditsRemaining
        }
      }
    }
  }
}`

// --- API response types ---

type requestLimitInfo struct {
	IsUnlimited                  *bool   `json:"isUnlimited"`
	NextRefreshTime              *string `json:"nextRefreshTime"`
	RequestLimit                 *int    `json:"requestLimit"`
	RequestsUsedSinceLastRefresh *int    `json:"requestsUsedSinceLastRefresh"`
}

type bonusGrant struct {
	RequestCreditsGranted   *int `json:"requestCreditsGranted"`
	RequestCreditsRemaining *int `json:"requestCreditsRemaining"`
}

type graphqlResponse struct {
	Data *struct {
		User *struct {
			User *struct {
				RequestLimitInfo *requestLimitInfo `json:"requestLimitInfo"`
				BonusGrants      []bonusGrant      `json:"bonusGrants"`
			} `json:"user"`
		} `json:"user"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type graphqlRequest struct {
	Query     string `json:"query"`
	Variables struct {
		RequestContext struct {
			ClientContext struct {
				ClientType string `json:"clientType"`
			} `json:"clientContext"`
			OsContext struct {
				OsName     string `json:"osName"`
				OsCategory string `json:"osCategory"`
			} `json:"osContext"`
		} `json:"requestContext"`
	} `json:"variables"`
}

func getAPIKey() string {
	return strings.TrimSpace(os.Getenv("WARP_API_KEY"))
}

// Provider fetches Warp usage data.
type Provider struct{}

func (Provider) ID() string         { return "warp" }
func (Provider) Name() string       { return "Warp" }
func (Provider) BrandColor() string { return "#01A4FF" }
func (Provider) BrandBg() string    { return "#081520" }
func (Provider) MetricIDs() []string {
	return []string{"credits-percent", "bonus-credits"}
}

func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providers.Snapshot{
			ProviderID:   "warp",
			ProviderName: "Warp",
			Source:       "none",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Set WARP_API_KEY environment variable.",
		}, nil
	}

	payload := graphqlRequest{}
	payload.Query = graphqlQuery
	payload.Variables.RequestContext.ClientContext.ClientType = "DESKTOP"
	payload.Variables.RequestContext.OsContext.OsName = "Windows"
	payload.Variables.RequestContext.OsContext.OsCategory = "DESKTOP"

	var resp graphqlResponse
	err := httputil.PostJSON(graphqlURL, map[string]string{
		"Authorization":  "Bearer " + apiKey,
		"Content-Type":   "application/json",
		"X-Warp-Client-Id": "warp-app",
		"User-Agent":     "Warp/1.0",
	}, payload, 15*time.Second, &resp)
	if err != nil {
		return providers.Snapshot{}, err
	}

	if len(resp.Errors) > 0 {
		return providers.Snapshot{
			ProviderID:   "warp",
			ProviderName: "Warp",
			Source:       "api-key",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Warp GraphQL error: " + resp.Errors[0].Message,
		}, nil
	}

	var info *requestLimitInfo
	var grants []bonusGrant
	if resp.Data != nil && resp.Data.User != nil && resp.Data.User.User != nil {
		info = resp.Data.User.User.RequestLimitInfo
		grants = resp.Data.User.User.BonusGrants
	}

	var metrics []providers.MetricValue
	now := time.Now().UTC().Format(time.RFC3339)

	if info != nil {
		if info.IsUnlimited != nil && *info.IsUnlimited {
			metrics = append(metrics, providers.MetricValue{
				ID:        "credits-percent",
				Label:     "CREDITS",
				Name:      "Warp credits (unlimited)",
				Value:     "\u221e", // ∞
				Caption:   "Unlimited",
				UpdatedAt: now,
			})
		} else if info.RequestLimit != nil && *info.RequestLimit > 0 {
			used := 0
			if info.RequestsUsedSinceLastRefresh != nil {
				used = *info.RequestsUsedSinceLastRefresh
			}
			limit := *info.RequestLimit
			remaining := limit - used
			if remaining < 0 {
				remaining = 0
			}
			remainingPct := float64(remaining) / float64(limit) * 100
			ratio := remainingPct / 100
			rc := remaining
			rm := limit

			m := providers.MetricValue{
				ID:           "credits-percent",
				Label:        "CREDITS",
				Name:         "Warp credits remaining",
				Value:        math.Round(remainingPct),
				NumericValue: &remainingPct,
				NumericUnit:  "percent",
				Unit:         "%",
				Ratio:        &ratio,
				Direction:    "up",
				RawCount:     &rc,
				RawMax:       &rm,
				UpdatedAt:    now,
			}

			if info.NextRefreshTime != nil && *info.NextRefreshTime != "" {
				if d, err := time.Parse(time.RFC3339, *info.NextRefreshTime); err == nil {
					delta := d.Sub(time.Now()).Seconds()
					if delta > 0 {
						m.ResetInSeconds = &delta
					}
				}
			}

			metrics = append(metrics, m)
		}
	}

	// Bonus credits - aggregate all grants
	if len(grants) > 0 {
		totalGranted := 0
		totalRemaining := 0
		for _, g := range grants {
			if g.RequestCreditsGranted != nil {
				totalGranted += *g.RequestCreditsGranted
			}
			if g.RequestCreditsRemaining != nil {
				totalRemaining += *g.RequestCreditsRemaining
			}
		}
		if totalGranted > 0 {
			usedPct := float64(totalGranted-totalRemaining) / float64(totalGranted) * 100
			remainPct := 100 - usedPct
			ratio := remainPct / 100

			metrics = append(metrics, providers.MetricValue{
				ID:           "bonus-credits",
				Label:        "BONUS",
				Name:         "Warp bonus credits remaining",
				Value:        math.Round(remainPct),
				NumericValue: &remainPct,
				NumericUnit:  "percent",
				Unit:         "%",
				Ratio:        &ratio,
				Direction:    "up",
				RawCount:     &totalRemaining,
				RawMax:       &totalGranted,
				UpdatedAt:    now,
			})
		}
	}

	return providers.Snapshot{
		ProviderID:   "warp",
		ProviderName: "Warp",
		Source:       "api-key",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

func init() {
	providers.Register(Provider{})
}

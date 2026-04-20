// Package warp implements the Warp AI usage provider.
//
// Auth: Property Inspector settings field or WARP_API_KEY / WARP_TOKEN
// environment variable.
// Endpoint: POST https://app.warp.dev/graphql/v2?op=GetRequestLimitInfo
package warp

import (
	"math"
	"strconv"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

// graphqlURL is the Warp GraphQL endpoint for the GetRequestLimitInfo op.
const graphqlURL = "https://app.warp.dev/graphql/v2?op=GetRequestLimitInfo"

// graphqlQuery is the GraphQL document sent by Fetch.
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
          expiration
        }
        workspaces {
          bonusGrantsInfo {
            grants {
              requestCreditsGranted
              requestCreditsRemaining
              expiration
            }
          }
        }
      }
    }
  }
}`

// --- API response types ---

// requestLimitInfo captures the per-user request limit and next refresh.
type requestLimitInfo struct {
	IsUnlimited                  *bool   `json:"isUnlimited"`
	NextRefreshTime              *string `json:"nextRefreshTime"`
	RequestLimit                 *int    `json:"requestLimit"`
	RequestsUsedSinceLastRefresh *int    `json:"requestsUsedSinceLastRefresh"`
}

// bonusGrant represents a one-off pool of bonus credits and its expiry.
type bonusGrant struct {
	RequestCreditsGranted   *int    `json:"requestCreditsGranted"`
	RequestCreditsRemaining *int    `json:"requestCreditsRemaining"`
	Expiration              *string `json:"expiration"`
}

// workspaceBonusInfo carries the per-workspace bonus grant list.
type workspaceBonusInfo struct {
	BonusGrantsInfo *struct {
		Grants []bonusGrant `json:"grants"`
	} `json:"bonusGrantsInfo"`
}

// graphqlResponse is the shape returned by the Warp GraphQL endpoint.
type graphqlResponse struct {
	Data *struct {
		User *struct {
			User *struct {
				RequestLimitInfo *requestLimitInfo    `json:"requestLimitInfo"`
				BonusGrants      []bonusGrant         `json:"bonusGrants"`
				Workspaces       []workspaceBonusInfo `json:"workspaces"`
			} `json:"user"`
		} `json:"user"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// graphqlRequest is the JSON body posted to the Warp GraphQL endpoint.
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

// getAPIKey resolves a Warp API key from user settings or env vars.
func getAPIKey() string {
	return settings.ResolveAPIKey(
		settings.ProviderKeysGet().WarpKey,
		"WARP_API_KEY", "WARP_TOKEN",
	)
}

// Provider fetches Warp usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "warp" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Warp" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#01A4FF" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#081520" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"credits-percent", "bonus-credits"}
}

// Fetch returns the latest Warp request-limit snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return providers.Snapshot{
			ProviderID:   "warp",
			ProviderName: "Warp",
			Source:       "none",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Enter a Warp API key in the Warp tab, or set WARP_API_KEY / WARP_TOKEN.",
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
		grants = append(grants, resp.Data.User.User.BonusGrants...)
		for _, ws := range resp.Data.User.User.Workspaces {
			if ws.BonusGrantsInfo != nil {
				grants = append(grants, ws.BonusGrantsInfo.Grants...)
			}
		}
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

	// Bonus credits - aggregate user + workspace grants, track earliest expiration.
	if len(grants) > 0 {
		totalGranted := 0
		totalRemaining := 0
		var earliestExpiry time.Time
		earliestRemaining := 0
		for _, g := range grants {
			granted := 0
			remaining := 0
			if g.RequestCreditsGranted != nil {
				granted = *g.RequestCreditsGranted
			}
			if g.RequestCreditsRemaining != nil {
				remaining = *g.RequestCreditsRemaining
			}
			totalGranted += granted
			totalRemaining += remaining
			if remaining > 0 && g.Expiration != nil && *g.Expiration != "" {
				if t, err := time.Parse(time.RFC3339, *g.Expiration); err == nil {
					if earliestExpiry.IsZero() || t.Before(earliestExpiry) {
						earliestExpiry = t
						earliestRemaining = remaining
					} else if t.Equal(earliestExpiry) {
						earliestRemaining += remaining
					}
				}
			}
		}
		if totalGranted > 0 {
			usedPct := float64(totalGranted-totalRemaining) / float64(totalGranted) * 100
			remainPct := 100 - usedPct
			ratio := remainPct / 100

			m := providers.MetricValue{
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
			}
			if !earliestExpiry.IsZero() && earliestRemaining > 0 {
				m.Caption = formatExpiryCaption(earliestRemaining, earliestExpiry)
			}
			metrics = append(metrics, m)
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

// formatExpiryCaption returns "N credits expire MMM D" for the earliest-expiring batch.
func formatExpiryCaption(remaining int, expiry time.Time) string {
	noun := "credits expire"
	if remaining == 1 {
		noun = "credit expires"
	}
	return strconv.Itoa(remaining) + " " + noun + " " + expiry.Local().Format("Jan 2")
}

// init registers the Warp provider with the package registry.
func init() {
	providers.Register(Provider{})
}

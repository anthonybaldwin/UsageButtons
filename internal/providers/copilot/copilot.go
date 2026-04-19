// Package copilot implements the GitHub Copilot usage provider.
//
// Auth, in priority order:
//   1. Property Inspector settings field
//   2. $GITHUB_TOKEN env var
//   3. ~/.config/github-copilot/hosts.json
//   4. ~/.config/github-copilot/apps.json
// Endpoint: GET https://api.github.com/copilot_internal/user
package copilot

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const usageURL = "https://api.github.com/copilot_internal/user"

// --- API response types ---

type quotaSnapshot struct {
	Entitlement      *int     `json:"entitlement"`
	Remaining        *int     `json:"remaining"`
	PercentRemaining *float64 `json:"percent_remaining"`
	QuotaID          string   `json:"quota_id"`
}

type copilotUsageResponse struct {
	CopilotPlan    *string `json:"copilot_plan"`
	QuotaResetDate *string `json:"quota_reset_date"`
	QuotaSnapshots *struct {
		PremiumInteractions *quotaSnapshot `json:"premium_interactions"`
		Chat                *quotaSnapshot `json:"chat"`
	} `json:"quota_snapshots"`
}

// --- Credential loading ---

func copilotHostsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "github-copilot", "hosts.json")
}

func copilotAppsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "github-copilot", "apps.json")
}

func loadGitHubToken() string {
	// Settings first, then env var
	if t := settings.ResolveAPIKey(
		settings.ProviderKeysGet().CopilotToken,
		"GITHUB_TOKEN",
	); t != "" {
		return t
	}

	// Try hosts.json then apps.json
	for _, path := range []string{copilotHostsPath(), copilotAppsPath()} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var parsed map[string]json.RawMessage
		if json.Unmarshal(data, &parsed) != nil {
			continue
		}
		for key, val := range parsed {
			if !strings.Contains(key, "github.com") {
				continue
			}
			// Try as plain string
			var strVal string
			if json.Unmarshal(val, &strVal) == nil && strings.TrimSpace(strVal) != "" {
				return strings.TrimSpace(strVal)
			}
			// Try as object with oauth_token or token
			var obj map[string]any
			if json.Unmarshal(val, &obj) == nil {
				for _, field := range []string{"oauth_token", "token"} {
					if tok, ok := obj[field].(string); ok && strings.TrimSpace(tok) != "" {
						return strings.TrimSpace(tok)
					}
				}
			}
		}
	}

	return ""
}

// --- Provider implementation ---

// Provider fetches Copilot usage data.
type Provider struct{}

func (Provider) ID() string         { return "copilot" }
func (Provider) Name() string       { return "Copilot" }
func (Provider) BrandColor() string { return "#8534F3" }
func (Provider) BrandBg() string    { return "#150d2e" }
func (Provider) MetricIDs() []string {
	return []string{"premium-percent", "chat-percent"}
}

func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	token := loadGitHubToken()
	if token == "" {
		return providers.Snapshot{
			ProviderID:   "copilot",
			ProviderName: "Copilot",
			Source:       "none",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Enter a GitHub token in plugin settings, set GITHUB_TOKEN, or provide ~/.config/github-copilot/hosts.json or apps.json.",
		}, nil
	}

	var resp copilotUsageResponse
	err := httputil.GetJSON(usageURL, map[string]string{
		"Authorization":          "token " + token,
		"Accept":                 "application/json",
		"editor-version":         "vscode/1.96.2",
		"editor-plugin-version":  "copilot-chat/0.26.7",
		"User-Agent":             "GitHubCopilotChat/0.26.7",
		"x-github-api-version":   "2025-04-01",
	}, 15*time.Second, &resp)

	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return providers.Snapshot{
				ProviderID:   "copilot",
				ProviderName: "Copilot",
				Source:       "token",
				Metrics:      []providers.MetricValue{},
				Status:       "unknown",
				Error:        "GitHub token unauthorized for Copilot. Update the token in plugin settings, GITHUB_TOKEN, or the github-copilot config files.",
			}, nil
		}
		return providers.Snapshot{}, err
	}

	var metrics []providers.MetricValue
	now := time.Now().UTC().Format(time.RFC3339)

	if resp.QuotaSnapshots != nil {
		if q := resp.QuotaSnapshots.PremiumInteractions; q != nil {
			if m := quotaMetric("premium-percent", "PREMIUM", "Premium interactions remaining", q, now); m != nil {
				metrics = append(metrics, *m)
			}
		}
		if q := resp.QuotaSnapshots.Chat; q != nil {
			if m := quotaMetric("chat-percent", "CHAT", "Chat interactions remaining", q, now); m != nil {
				metrics = append(metrics, *m)
			}
		}
	}

	planName := "Copilot"
	if resp.CopilotPlan != nil && *resp.CopilotPlan != "" {
		p := *resp.CopilotPlan
		planName = "Copilot " + strings.ToUpper(p[:1]) + p[1:]
	}

	return providers.Snapshot{
		ProviderID:   "copilot",
		ProviderName: planName,
		Source:       "token",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

func quotaMetric(id, label, name string, q *quotaSnapshot, now string) *providers.MetricValue {
	if q == nil || q.PercentRemaining == nil {
		return nil
	}
	used := 100 - *q.PercentRemaining
	remaining := 100 - math.Max(0, math.Min(100, used))
	ratio := remaining / 100

	m := providers.MetricValue{
		ID:           id,
		Label:        label,
		Name:         name,
		Value:        math.Round(remaining),
		NumericValue: &remaining,
		NumericUnit:  "percent",
		Unit:         "%",
		Ratio:        &ratio,
		Direction:    "up",
		UpdatedAt:    now,
	}
	if q.Entitlement != nil && q.Remaining != nil {
		rc := *q.Remaining
		rm := *q.Entitlement
		m.RawCount = &rc
		m.RawMax = &rm
	}
	return &m
}

func init() {
	providers.Register(Provider{})
}

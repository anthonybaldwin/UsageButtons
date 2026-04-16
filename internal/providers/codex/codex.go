// Package codex implements the Codex OAuth API usage provider.
//
// Reads credentials from ~/.codex/auth.json (or $CODEX_HOME/auth.json),
// hits chatgpt.com/backend-api/wham/usage for session/weekly metrics
// and credits balance.
package codex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

const (
	usageURL  = "https://chatgpt.com/backend-api/wham/usage"
	userAgent = "UsageButtons/0.0.1"

	sessionWindowSeconds = 5 * 60 * 60  // 18000
	weeklyWindowSeconds  = 7 * 24 * 60 * 60 // 604800
)

// --- Credential loading ---

type authTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	// camelCase variants
	AccessTokenC  string `json:"accessToken"`
	RefreshTokenC string `json:"refreshToken"`
	IDTokenC      string `json:"idToken"`
	AccountIDC    string `json:"accountId"`
}

type authFile struct {
	OpenAIAPIKey  *string     `json:"OPENAI_API_KEY"`
	OpenAIAPIKeyL *string     `json:"openai_api_key"`
	Tokens        *authTokens `json:"tokens"`
	LastRefresh   string      `json:"last_refresh"`
	LastRefreshC  string      `json:"lastRefresh"`
}

type codexCreds struct {
	accessToken string
	accountID   string
	idToken     string
	isAPIKey    bool
}

func authPath() string {
	if ch := os.Getenv("CODEX_HOME"); ch != "" {
		return filepath.Join(ch, "auth.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "auth.json")
}

func loadCredentials() (codexCreds, error) {
	path := authPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return codexCreds{}, fmt.Errorf("Codex credentials not found at %s. Run `codex` in a terminal to sign in.", path)
		}
		return codexCreds{}, err
	}

	var f authFile
	if err := json.Unmarshal(data, &f); err != nil {
		return codexCreds{}, fmt.Errorf("invalid JSON in %s: %w", path, err)
	}

	// Legacy API key auth
	apiKey := ptrStr(f.OpenAIAPIKey)
	if apiKey == "" {
		apiKey = ptrStr(f.OpenAIAPIKeyL)
	}
	if strings.TrimSpace(apiKey) != "" {
		return codexCreds{accessToken: strings.TrimSpace(apiKey), isAPIKey: true}, nil
	}

	if f.Tokens == nil {
		return codexCreds{}, fmt.Errorf("Codex credentials at %s missing tokens.access_token", path)
	}
	t := f.Tokens
	accessToken := firstNonEmpty(t.AccessToken, t.AccessTokenC)
	if strings.TrimSpace(accessToken) == "" {
		return codexCreds{}, fmt.Errorf("Codex credentials at %s missing tokens.access_token", path)
	}

	return codexCreds{
		accessToken: strings.TrimSpace(accessToken),
		accountID:   firstNonEmpty(t.AccountID, t.AccountIDC),
		idToken:     firstNonEmpty(t.IDToken, t.IDTokenC),
		isAPIKey:    false,
	}, nil
}

// --- API response types ---

type usageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	ResetAt            *float64 `json:"reset_at"` // epoch seconds
	LimitWindowSeconds *float64 `json:"limit_window_seconds"`
}

type usageResponse struct {
	PlanType  *string `json:"plan_type"`
	RateLimit *struct {
		PrimaryWindow   *usageWindow `json:"primary_window"`
		SecondaryWindow *usageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	Credits *struct {
		HasCredits *bool   `json:"has_credits"`
		Unlimited  *bool   `json:"unlimited"`
		Balance    any     `json:"balance"` // number or string
	} `json:"credits"`
}

// --- Plan name mapping ---

var planMap = map[string]string{
	"guest":          "ChatGPT Guest",
	"free":           "ChatGPT Free",
	"go":             "ChatGPT Go",
	"plus":           "ChatGPT Plus",
	"pro":            "ChatGPT Pro",
	"free_workspace": "ChatGPT Free (Workspace)",
	"team":           "ChatGPT Team",
	"business":       "ChatGPT Business",
	"education":      "ChatGPT Edu",
	"edu":            "ChatGPT Edu",
	"enterprise":     "ChatGPT Enterprise",
	"quorum":         "ChatGPT (Quorum)",
	"k12":            "ChatGPT (K12)",
}

func humanPlan(planType *string) string {
	if planType == nil || *planType == "" {
		return ""
	}
	key := strings.ToLower(*planType)
	if name, ok := planMap[key]; ok {
		return name
	}
	return *planType
}

// --- JWT helpers ---

func decodeJWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[1] == "" {
		return nil
	}
	// base64url → base64
	b64 := strings.ReplaceAll(strings.ReplaceAll(parts[1], "-", "+"), "_", "/")
	// pad
	if mod := len(b64) % 4; mod != 0 {
		b64 += strings.Repeat("=", 4-mod)
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal(decoded, &payload) != nil {
		return nil
	}
	return payload
}

func emailFromIDToken(idToken string) string {
	if idToken == "" {
		return ""
	}
	payload := decodeJWTPayload(idToken)
	if payload == nil {
		return ""
	}
	if email, ok := payload["email"].(string); ok {
		return email
	}
	if ns, ok := payload["https://api.openai.com/profile"].(map[string]any); ok {
		if email, ok := ns["email"].(string); ok {
			return email
		}
	}
	return ""
}

// --- Window normalization ---

type windowRole int

const (
	roleUnknown windowRole = iota
	roleSession
	roleWeekly
)

func classifyWindow(seconds *float64) windowRole {
	if seconds == nil {
		return roleUnknown
	}
	s := int(*seconds)
	if s == sessionWindowSeconds {
		return roleSession
	}
	if s == weeklyWindowSeconds {
		return roleWeekly
	}
	return roleUnknown
}

type normalizedWindows struct {
	session *usageWindow
	weekly  *usageWindow
}

func normalizeWindows(primary, secondary *usageWindow) normalizedWindows {
	result := normalizedWindows{}
	assign := func(w *usageWindow) {
		if w == nil {
			return
		}
		role := classifyWindow(w.LimitWindowSeconds)
		switch role {
		case roleSession:
			if result.session == nil {
				result.session = w
			}
		case roleWeekly:
			if result.weekly == nil {
				result.weekly = w
			}
		default:
			if result.session == nil {
				result.session = w
			} else if result.weekly == nil {
				result.weekly = w
			}
		}
	}
	assign(primary)
	assign(secondary)
	return result
}

// --- Metric helpers ---

func remainingMetric(id, label, name string, w *usageWindow) *providers.MetricValue {
	if w == nil || w.UsedPercent == nil {
		return nil
	}
	used := math.Max(0, math.Min(100, *w.UsedPercent))
	remaining := 100 - used
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
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if w.ResetAt != nil {
		delta := *w.ResetAt - float64(time.Now().Unix())
		if delta < 0 {
			delta = 0
		}
		m.ResetInSeconds = &delta
	}
	return &m
}

func paceFromWindow(id, label, name string, w *usageWindow) *providers.MetricValue {
	if w == nil || w.UsedPercent == nil || w.ResetAt == nil || w.LimitWindowSeconds == nil {
		return nil
	}
	resetIn := time.Until(time.Unix(int64(*w.ResetAt), 0))
	return providers.PaceMetric(providers.PaceInput{
		MetricID: id, Label: label, Name: name,
		UsedPercent:    *w.UsedPercent,
		WindowDuration: time.Duration(*w.LimitWindowSeconds) * time.Second,
		ResetIn:        resetIn,
	})
}

func parseBalance(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return 0, false
		}
		return v, true
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return f, true
		}
	}
	return 0, false
}

// --- Provider implementation ---

// Provider fetches Codex usage data.
type Provider struct{}

func (Provider) ID() string         { return "codex" }
func (Provider) Name() string       { return "Codex" }
func (Provider) BrandColor() string { return "#10A37F" }
func (Provider) BrandBg() string    { return "#000000" }
func (Provider) MetricIDs() []string {
	return []string{
		"session-percent", "session-pace",
		"weekly-percent", "weekly-pace",
		"credits-balance",
		"cost-today", "cost-30d",
	}
}

func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	creds, err := loadCredentials()
	if err != nil {
		return providers.Snapshot{}, err
	}

	headers := map[string]string{
		"Authorization": "Bearer " + creds.accessToken,
		"User-Agent":    userAgent,
		"Accept":        "application/json",
	}
	if creds.accountID != "" {
		headers["ChatGPT-Account-Id"] = creds.accountID
	}

	var resp usageResponse
	err = httputil.GetJSON(usageURL, headers, 30*time.Second, &resp)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) {
			if httpErr.Status == 401 || httpErr.Status == 403 {
				return providers.Snapshot{}, fmt.Errorf(
					"Codex OAuth request unauthorized. Run `codex` to re-authenticate. (Token refresh is not yet implemented in this plugin.)")
			}
			return providers.Snapshot{}, fmt.Errorf("Codex OAuth server error: HTTP %d", httpErr.Status)
		}
		return providers.Snapshot{}, fmt.Errorf("Codex OAuth network error: %v", err)
	}

	var primary, secondary *usageWindow
	if resp.RateLimit != nil {
		primary = resp.RateLimit.PrimaryWindow
		secondary = resp.RateLimit.SecondaryWindow
	}
	windows := normalizeWindows(primary, secondary)

	var metrics []providers.MetricValue

	if session := remainingMetric("session-percent", "SESSION", "Session window remaining (5h)", windows.session); session != nil {
		metrics = append(metrics, *session)
	}
	if p := paceFromWindow("session-pace", "Session", "Session pace", windows.session); p != nil {
		metrics = append(metrics, *p)
	}

	if weekly := remainingMetric("weekly-percent", "WEEKLY", "Weekly window remaining", windows.weekly); weekly != nil {
		metrics = append(metrics, *weekly)
	}
	if p := paceFromWindow("weekly-pace", "Weekly", "Weekly pace", windows.weekly); p != nil {
		metrics = append(metrics, *p)
	}

	// Credits metric. Emit whenever the plan actually has a credits
	// concept — including $0.00 (user ran out of prepaid credits).
	// Only skip emission when the plan has no credits at all
	// (HasCredits=false) or is unlimited — in those cases the metric
	// is a category error, so the button stays on a dash rather than
	// lie with $0.
	if resp.Credits != nil {
		balance, ok := parseBalance(resp.Credits.Balance)
		hasCredits := resp.Credits.HasCredits != nil && *resp.Credits.HasCredits
		unlimited := resp.Credits.Unlimited != nil && *resp.Credits.Unlimited
		if ok && hasCredits && !unlimited {
			if balance < 0 {
				balance = 0
			}
			now := time.Now().UTC().Format(time.RFC3339)
			metrics = append(metrics, providers.MetricValue{
				ID:              "credits-balance",
				Label:           "CREDITS",
				Name:            "Credits remaining",
				Value:           fmt.Sprintf("$%.2f", balance),
				NumericValue:    &balance,
				NumericUnit:     "dollars",
				NumericGoodWhen: "high",
				Caption:         "Prepaid",
				UpdatedAt:       now,
			})
		}
	}

	// Local cost tracking from ~/.codex/sessions/**/*.jsonl. Adds
	// cost-today + cost-30d if any session data is within the 30-day
	// window; no-op otherwise so buttons render as dash instead of
	// faking $0.00.
	metrics = append(metrics, codexCostMetrics()...)

	provName := humanPlan(resp.PlanType)
	if provName == "" {
		provName = "Codex"
	}
	email := emailFromIDToken(creds.idToken)
	if email != "" {
		provName = provName + " \u2014 " + email
	}

	source := "oauth"
	if creds.isAPIKey {
		source = "api-key"
	}

	return providers.Snapshot{
		ProviderID:   "codex",
		ProviderName: provName,
		Source:       source,
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// --- Utility ---

func ptrStr(p *string) string {
	if p != nil {
		return *p
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func init() {
	providers.Register(Provider{})
}

// Package codex implements the Codex OAuth API usage provider.
//
// Reads credentials from ~/.codex/auth.json (or $CODEX_HOME/auth.json),
// hits {base}/wham/usage for session/weekly metrics and credits balance,
// where {base} honors a chatgpt_base_url override in the Codex config
// (~/.codex/config.toml or $CODEX_HOME/config.toml) — matching the
// Codex CLI's own override plumbing — and defaults to chatgpt.com.
package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	defaultChatGPTBaseURL = "https://chatgpt.com/backend-api"
	userAgent             = "UsageButtons/0.0.1"

	sessionWindowSeconds = 5 * 60 * 60      // 18000
	weeklyWindowSeconds  = 7 * 24 * 60 * 60 // 604800
)

// chatGPTBaseURL resolves the ChatGPT/OpenAI backend base in this
// priority order:
//  1. Property Inspector override (settings.ProviderKeys.CodexChatGPTBaseURL)
//  2. chatgpt_base_url = "..." in ~/.codex/config.toml (or $CODEX_HOME)
//  3. defaultChatGPTBaseURL
// Normalizes trailing slashes and appends /backend-api when a bare
// chatgpt.com / chat.openai.com host is supplied.
func chatGPTBaseURL() string {
	if v := normalizeChatGPTBase(settings.ProviderKeysGet().CodexChatGPTBaseURL); v != "" {
		return v
	}
	if v := normalizeChatGPTBase(readConfigBaseURL()); v != "" {
		return v
	}
	return defaultChatGPTBaseURL
}

func usageURL() string {
	return chatGPTBaseURL() + "/wham/usage"
}

func codexConfigPath() string {
	if ch := strings.TrimSpace(os.Getenv("CODEX_HOME")); ch != "" {
		return filepath.Join(ch, "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

func readConfigBaseURL() string {
	data, err := os.ReadFile(codexConfigPath())
	if err != nil {
		return ""
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := raw
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key != "chatgpt_base_url" {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			first, last := val[0], val[len(val)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		return strings.TrimSpace(val)
	}
	return ""
}

func normalizeChatGPTBase(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	v = strings.TrimRight(v, "/")
	if (strings.HasPrefix(v, "https://chatgpt.com") ||
		strings.HasPrefix(v, "https://chat.openai.com")) &&
		!strings.Contains(v, "/backend-api") {
		v += "/backend-api"
	}
	return v
}

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

type rateLimitBlock struct {
	PrimaryWindow   *usageWindow `json:"primary_window"`
	SecondaryWindow *usageWindow `json:"secondary_window"`
}

type additionalRateLimit struct {
	LimitName      string          `json:"limit_name"`
	MeteredFeature string          `json:"metered_feature"`
	RateLimit      *rateLimitBlock `json:"rate_limit"`
}

type usageResponse struct {
	PlanType  *string         `json:"plan_type"`
	Email     *string         `json:"email"`
	AccountID *string         `json:"account_id"`
	RateLimit *rateLimitBlock `json:"rate_limit"`
	// Code Review (codex /review) quota — same primary/secondary shape
	// as rate_limit. Null when the user hasn't used Code Review yet.
	CodeReviewRateLimit *rateLimitBlock       `json:"code_review_rate_limit"`
	AdditionalRateLimits []additionalRateLimit `json:"additional_rate_limits"`
	Credits              *struct {
		HasCredits *bool `json:"has_credits"`
		Unlimited  *bool `json:"unlimited"`
		Balance    any   `json:"balance"` // number or string
	} `json:"credits"`
}

// --- Plan name mapping ---

var planMap = map[string]string{
	"guest":          "ChatGPT Guest",
	"free":           "ChatGPT Free",
	"go":             "ChatGPT Go",
	"plus":           "ChatGPT Plus",
	"pro":            "ChatGPT Pro",
	"prolite":        "ChatGPT Pro Lite",
	"pro_lite":       "ChatGPT Pro Lite",
	"pro-lite":       "ChatGPT Pro Lite",
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
		"review-percent", "review-pace",
		"weekly-review-percent", "weekly-review-pace",
		"credits-balance",
		"cost-today", "cost-30d",
	}
}

// extraLaneSlug turns an additional_rate_limits entry into a stable
// metric-ID slug. Prefers metered_feature (stable across API model
// renames) over limit_name (human-readable).
func extraLaneSlug(x additionalRateLimit) string {
	for _, candidate := range []string{x.MeteredFeature, x.LimitName} {
		s := slugify(candidate)
		if s != "" {
			return s
		}
	}
	return ""
}

// displayLaneLabel returns a human-readable label for an extra lane.
// Prefers limit_name; falls back to a title-cased metered_feature.
func displayLaneLabel(x additionalRateLimit) string {
	if n := strings.TrimSpace(x.LimitName); n != "" {
		return n
	}
	if n := strings.TrimSpace(x.MeteredFeature); n != "" {
		return n
	}
	return "Extra"
}

// slugify lower-cases and strips characters that would be unfriendly
// in a metric ID. Collapses runs of hyphens.
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_' || r == ' ' || r == '.' || r == '/':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// fetchUsage tries the browser-proxied path first (when the extension is
// connected and the configured base URL points at chatgpt.com), then falls
// back to the OAuth / API-key direct path. Returns the parsed response plus
// a source tag ("cookie" / "oauth" / "api-key") for Snapshot.Source and an
// email hint (empty when the browser payload didn't carry one — caller
// decodes from the OAuth id_token in that case).
func fetchUsage() (usageResponse, string, string, error) {
	base := chatGPTBaseURL()
	if browserPathApplicable(base) {
		if resp, ok := tryFetchViaExtension(base); ok {
			email := ""
			if resp.Email != nil {
				email = strings.TrimSpace(*resp.Email)
			}
			return resp, "cookie", email, nil
		}
	}
	return fetchUsageOAuth()
}

// browserPathApplicable returns true when the configured base URL is one
// of the public ChatGPT hosts the Chrome extension is allowlisted for.
// Self-hosted / proxied Codex setups (custom chatgpt_base_url) skip the
// browser path because the extension can't authenticate those hosts.
func browserPathApplicable(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")
}

// tryFetchViaExtension attempts the browser-proxied fetch. Any failure
// (extension missing, network error, non-2xx) returns ok=false so the
// caller falls through to OAuth. We deliberately swallow errors here —
// the direct path is the canonical fallback and will surface the real
// error if it fails too.
func tryFetchViaExtension(base string) (usageResponse, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return usageResponse{}, false
	}
	var resp usageResponse
	if err := cookies.FetchJSON(ctx, base+"/wham/usage", nil, &resp); err != nil {
		return usageResponse{}, false
	}
	return resp, true
}

// fetchUsageOAuth is the historical direct-HTTP path: read the Bearer
// token from ~/.codex/auth.json and hit chatgpt.com ourselves. Kept as
// the fallback so users without the extension still work, and so custom
// chatgpt_base_url setups continue to function.
func fetchUsageOAuth() (usageResponse, string, string, error) {
	creds, err := loadCredentials()
	if err != nil {
		return usageResponse{}, "", "", err
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
	err = httputil.GetJSON(usageURL(), headers, 30*time.Second, &resp)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) {
			if httpErr.Status == 401 || httpErr.Status == 403 {
				return usageResponse{}, "", "", fmt.Errorf(
					"Codex OAuth request unauthorized. Run `codex` to re-authenticate. (Token refresh is not yet implemented in this plugin.)")
			}
			return usageResponse{}, "", "", fmt.Errorf("Codex OAuth server error: HTTP %d", httpErr.Status)
		}
		return usageResponse{}, "", "", fmt.Errorf("Codex OAuth network error: %v", err)
	}

	source := "oauth"
	if creds.isAPIKey {
		source = "api-key"
	}
	return resp, source, emailFromIDToken(creds.idToken), nil
}

func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	resp, source, email, err := fetchUsage()
	if err != nil {
		return providers.Snapshot{}, err
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
	if p := paceFromWindow("session-pace", "SESSION", "Session pace (5h)", windows.session); p != nil {
		metrics = append(metrics, *p)
	}

	if weekly := remainingMetric("weekly-percent", "WEEKLY", "Weekly window remaining", windows.weekly); weekly != nil {
		metrics = append(metrics, *weekly)
	}
	if p := paceFromWindow("weekly-pace", "WEEKLY", "Weekly pace (7d)", windows.weekly); p != nil {
		metrics = append(metrics, *p)
	}

	// Code Review (codex /review) quota. The wham/usage response
	// carries a code_review_rate_limit block with the same
	// primary/secondary shape as the main rate_limit. It's null until
	// the user first uses Code Review, so we skip metric emission
	// when it's absent to avoid dead tiles on fresh accounts.
	if resp.CodeReviewRateLimit != nil {
		crWindows := normalizeWindows(
			resp.CodeReviewRateLimit.PrimaryWindow,
			resp.CodeReviewRateLimit.SecondaryWindow,
		)
		if m := remainingMetric("review-percent", "REVIEW", "Code Review session remaining (5h)", crWindows.session); m != nil {
			metrics = append(metrics, *m)
		}
		if m := paceFromWindow("review-pace", "REVIEW", "Code Review pace (5h)", crWindows.session); m != nil {
			metrics = append(metrics, *m)
		}
		if m := remainingMetric("weekly-review-percent", "REVIEW", "Code Review weekly remaining", crWindows.weekly); m != nil {
			metrics = append(metrics, *m)
		}
		if m := paceFromWindow("weekly-review-pace", "REVIEW", "Code Review weekly pace (7d)", crWindows.weekly); m != nil {
			metrics = append(metrics, *m)
		}
	}

	// Additional rate limits — per-model extras like GPT-5.3-Codex-Spark.
	// Each entry gets its own set of metrics, slugged by metered_feature
	// so the IDs stay stable across model renames. Labels use limit_name
	// (human-readable) and fall back to the slug when absent.
	for _, extra := range resp.AdditionalRateLimits {
		if extra.RateLimit == nil {
			continue
		}
		slug := extraLaneSlug(extra)
		if slug == "" {
			continue
		}
		label := strings.ToUpper(displayLaneLabel(extra))
		extraName := displayLaneLabel(extra)
		ew := normalizeWindows(extra.RateLimit.PrimaryWindow, extra.RateLimit.SecondaryWindow)
		if m := remainingMetric(slug+"-percent", label, extraName+" session remaining (5h)", ew.session); m != nil {
			metrics = append(metrics, *m)
		}
		if m := paceFromWindow(slug+"-pace", label, extraName+" pace (5h)", ew.session); m != nil {
			metrics = append(metrics, *m)
		}
		if m := remainingMetric("weekly-"+slug+"-percent", label, extraName+" weekly remaining", ew.weekly); m != nil {
			metrics = append(metrics, *m)
		}
		if m := paceFromWindow("weekly-"+slug+"-pace", label, extraName+" weekly pace (7d)", ew.weekly); m != nil {
			metrics = append(metrics, *m)
		}
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
	if email == "" && resp.Email != nil {
		email = strings.TrimSpace(*resp.Email)
	}
	if email != "" {
		provName = provName + " \u2014 " + email
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

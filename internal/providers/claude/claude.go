// Package claude implements the Claude OAuth API usage provider.
//
// Reads credentials from ~/.claude/.credentials.json, hits
// api.anthropic.com/api/oauth/usage for session/weekly/sonnet metrics,
// and optionally supplements with claude.ai cookie-based web API for
// the extra-usage block (spend limits, prepaid balance).
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

const (
	usageURL   = "https://api.anthropic.com/api/oauth/usage"
	betaHdr    = "oauth-2025-04-20"
	userAgent  = "claude-code/2.1.70"
	baseWebURL = "https://claude.ai/api"

	extrasDefaultTTL = 60 * time.Minute
	extrasActiveTTL  = 15 * time.Minute
	nearLimitPct     = 80.0
)

// LogSink is wired by the plugin for debug logging.
var LogSink func(string)

func logf(format string, args ...any) {
	if LogSink != nil {
		LogSink(fmt.Sprintf("[claude] "+format, args...))
	}
}

// --- Credential loading ---

type credFile struct {
	ClaudeAiOauth *struct {
		AccessToken   string   `json:"accessToken"`
		RefreshToken  string   `json:"refreshToken"`
		ExpiresAt     *int64   `json:"expiresAt"` // ms since epoch
		Scopes        []string `json:"scopes"`
		RateLimitTier string   `json:"rateLimitTier"`
	} `json:"claudeAiOauth"`
}

type creds struct {
	accessToken   string
	scopes        []string
	expiresAt     *time.Time
	rateLimitTier string
}

func credPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", ".credentials.json")
}

// readCredBlob returns the raw Claude credentials JSON and a
// human-readable label for where it came from. File first (primary on
// Linux/Windows and older macOS installs), then macOS keychain on
// darwin. A second-argument label is returned so downstream error
// messages can say "invalid JSON in macOS keychain" vs. a path.
func readCredBlob() ([]byte, string, error) {
	path := credPath()
	data, err := os.ReadFile(path)
	if err == nil {
		return data, path, nil
	}
	if !os.IsNotExist(err) {
		return nil, "", err
	}

	if blob, kcErr := readKeychainBlob(); kcErr == nil {
		return blob, "macOS keychain", nil
	}

	return nil, "", fmt.Errorf(
		"Claude credentials not found in %s. Run `claude` in a terminal to sign in.",
		credsSourceHint())
}

func loadCreds() (creds, error) {
	data, source, err := readCredBlob()
	if err != nil {
		return creds{}, err
	}

	var f credFile
	if err := json.Unmarshal(data, &f); err != nil {
		return creds{}, fmt.Errorf("invalid JSON in %s: %w", source, err)
	}

	if f.ClaudeAiOauth == nil || strings.TrimSpace(f.ClaudeAiOauth.AccessToken) == "" {
		return creds{}, fmt.Errorf("Claude credentials in %s missing claudeAiOauth.accessToken", source)
	}

	c := creds{
		accessToken:   strings.TrimSpace(f.ClaudeAiOauth.AccessToken),
		rateLimitTier: f.ClaudeAiOauth.RateLimitTier,
	}
	if len(f.ClaudeAiOauth.Scopes) > 0 {
		c.scopes = f.ClaudeAiOauth.Scopes
	} else {
		c.scopes = []string{"user:profile"}
	}
	if f.ClaudeAiOauth.ExpiresAt != nil {
		t := time.UnixMilli(*f.ClaudeAiOauth.ExpiresAt)
		c.expiresAt = &t
	}
	return c, nil
}

func planFromTier(tier string) string {
	t := strings.ToLower(strings.TrimSpace(tier))
	switch {
	case strings.Contains(t, "max"):
		return "Claude Max"
	case strings.Contains(t, "pro"):
		return "Claude Pro"
	case strings.Contains(t, "team"):
		return "Claude Team"
	case strings.Contains(t, "enterprise"):
		return "Claude Enterprise"
	case strings.Contains(t, "ultra"):
		return "Claude Ultra"
	default:
		return ""
	}
}

func hasScope(scopes []string, s string) bool {
	for _, sc := range scopes {
		if sc == s {
			return true
		}
	}
	return false
}

// --- API response types ---

type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type extraUsageRaw struct {
	IsEnabled    *bool    `json:"is_enabled"`
	MonthlyLimit *int64   `json:"monthly_limit"` // cents
	UsedCredits  *int64   `json:"used_credits"`  // cents
	Utilization  *float64 `json:"utilization"`
	Currency     *string  `json:"currency"`
}

type usageResponse struct {
	FiveHour       *usageWindow   `json:"five_hour"`
	SevenDay       *usageWindow   `json:"seven_day"`
	SevenDaySonnet *usageWindow   `json:"seven_day_sonnet"`
	SevenDayOpus   *usageWindow   `json:"seven_day_opus"`
	ExtraUsage     *extraUsageRaw `json:"extra_usage"`
}

// --- Extra usage source (unified shape from OAuth or Web) ---

type extraUsageSource struct {
	isEnabled         bool
	monthlyLimitCents int64
	usedCreditsCents  int64
	currency          string
	balanceCents      *int64
	autoReloadEnabled *bool
}

// --- Extras caching ---

type cachedExtrasEntry struct {
	source     extraUsageSource
	capturedAt time.Time
}

var (
	extrasMu     sync.Mutex
	cachedExtras *cachedExtrasEntry
)

func shouldRefreshExtras(resp usageResponse, force bool) bool {
	if force {
		return true
	}
	extrasMu.Lock()
	cached := cachedExtras
	extrasMu.Unlock()

	if cached == nil {
		return true
	}
	age := time.Since(cached.capturedAt)

	sessionPct := 0.0
	if resp.FiveHour != nil && resp.FiveHour.Utilization != nil {
		sessionPct = *resp.FiveHour.Utilization
	}
	weeklyPct := 0.0
	if resp.SevenDay != nil && resp.SevenDay.Utilization != nil {
		weeklyPct = *resp.SevenDay.Utilization
	}
	nearLimit := sessionPct >= nearLimitPct || weeklyPct >= nearLimitPct
	extrasOn := cached.source.isEnabled

	if extrasOn {
		return age >= extrasActiveTTL
	}
	if nearLimit {
		return age >= extrasActiveTTL
	}
	return age >= extrasDefaultTTL
}

// --- Web API (cookie path) ---

type orgRow struct {
	UUID         string   `json:"uuid"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

type overageResponse struct {
	IsEnabled          *bool  `json:"is_enabled"`
	MonthlyCreditLimit *int64 `json:"monthly_credit_limit"`
	UsedCredits        *int64 `json:"used_credits"`
	Currency           string `json:"currency"`
	AccountEmail       string `json:"account_email"`
	OutOfCredits       *bool  `json:"out_of_credits"`
	SeatTier           string `json:"seat_tier"`
}

type prepaidCreditsResponse struct {
	Amount             *int64 `json:"amount"`
	Currency           string `json:"currency"`
	AutoReloadSettings any    `json:"auto_reload_settings"`
}

var (
	orgMu    sync.Mutex
	orgCache = map[string]string{}
)

func fetchOrgID(ctx context.Context, cacheKey string) (string, error) {
	orgMu.Lock()
	cached, ok := orgCache[cacheKey]
	orgMu.Unlock()
	if ok {
		return cached, nil
	}

	var orgs []orgRow
	err := cookies.FetchJSON(ctx, baseWebURL+"/organizations", nil, &orgs)
	if err != nil {
		return "", err
	}
	if len(orgs) == 0 {
		return "", errors.New("claude.ai returned no organizations")
	}

	var selected string
	for _, o := range orgs {
		if o.UUID == "" {
			continue
		}
		for _, cap := range o.Capabilities {
			if strings.EqualFold(cap, "chat") {
				selected = o.UUID
				break
			}
		}
		if selected != "" {
			break
		}
	}
	if selected == "" {
		for _, o := range orgs {
			if o.UUID != "" {
				selected = o.UUID
				break
			}
		}
	}
	if selected == "" {
		return "", errors.New("no usable org UUID")
	}

	orgMu.Lock()
	orgCache[cacheKey] = selected
	orgMu.Unlock()
	return selected, nil
}

func fetchWebExtras() *extraUsageSource {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if !cookies.HostAvailable(ctx) {
		logf("extras: extension not connected; skipping web path")
		return nil
	}
	const cacheKey = "extension"
	logf("extras: fetching org via extension...")

	orgID, err := fetchOrgID(ctx, cacheKey)
	if err != nil {
		logf("extras: org fetch failed: %v", err)
		return nil
	}
	logf("extras: org=%s, fetching overage...", orgID)

	var overage overageResponse
	err = cookies.FetchJSON(ctx,
		fmt.Sprintf("%s/organizations/%s/overage_spend_limit", baseWebURL, orgID),
		nil, &overage,
	)
	if err != nil {
		logf("extras: overage fetch failed: %v", err)
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && httpErr.Status == 401 {
			orgMu.Lock()
			delete(orgCache, cacheKey)
			orgMu.Unlock()
		}
		return nil
	}

	var prepaid prepaidCreditsResponse
	prepaidErr := cookies.FetchJSON(ctx,
		fmt.Sprintf("%s/organizations/%s/prepaid/credits", baseWebURL, orgID),
		nil, &prepaid,
	)

	result := &extraUsageSource{
		isEnabled:         overage.IsEnabled != nil && *overage.IsEnabled,
		monthlyLimitCents: valOr(overage.MonthlyCreditLimit, 0),
		usedCreditsCents:  valOr(overage.UsedCredits, 0),
		currency:          defStr(overage.Currency, "USD"),
	}

	if prepaidErr == nil {
		if prepaid.Amount != nil {
			result.balanceCents = prepaid.Amount
		}
		autoReload := prepaid.AutoReloadSettings != nil
		result.autoReloadEnabled = &autoReload
	}

	return result
}

// --- Metric construction ---

func remainingMetric(id, label, name string, w *usageWindow) *providers.MetricValue {
	if w == nil || w.Utilization == nil {
		return nil
	}
	used := math.Max(0, math.Min(100, *w.Utilization))
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
	}

	if w.ResetsAt != nil {
		t, err := time.Parse(time.RFC3339, *w.ResetsAt)
		if err == nil {
			delta := time.Until(t).Seconds()
			if delta < 0 {
				delta = 0
			}
			m.ResetInSeconds = &delta
		}
	}
	return &m
}

func extraUsageMetrics(extra *extraUsageSource) []providers.MetricValue {
	if extra == nil {
		return nil
	}
	var out []providers.MetricValue

	val := "OFF"
	onRatio := 0.0
	if extra.isEnabled {
		val = "ON"
		onRatio = 1.0
	}
	out = append(out, providers.MetricValue{
		ID: "extra-usage-enabled", Label: "EXTRA USAGE", Name: "Extra usage enabled",
		Value: val, Ratio: &onRatio, Direction: "up",
	})

	if extra.balanceCents != nil {
		bal := float64(*extra.balanceCents) / 100
		out = append(out, providers.MetricValue{
			ID: "extra-usage-balance", Label: "BALANCE", Name: "Extra usage prepaid balance",
			Value: fmt.Sprintf("$%.2f", bal), NumericValue: &bal,
			NumericUnit: "dollars", NumericGoodWhen: "high", Caption: "Prepaid",
		})
	}

	if extra.autoReloadEnabled != nil {
		rv, rr := "OFF", 0.0
		if *extra.autoReloadEnabled {
			rv, rr = "ON", 1.0
		}
		out = append(out, providers.MetricValue{
			ID: "extra-usage-auto-reload", Label: "RELOAD", Name: "Extras auto-reload",
			Value: rv, Ratio: &rr, Direction: "up",
		})
	}

	if extra.monthlyLimitCents <= 0 {
		return out
	}

	limit := float64(extra.monthlyLimitCents) / 100
	spent := float64(extra.usedCreditsCents) / 100
	usedPct := math.Min(100, (spent/limit)*100)
	remPct := 100 - usedPct
	remRatio := remPct / 100
	spentRatio := usedPct / 100

	out = append(out,
		providers.MetricValue{
			ID: "extra-usage-percent", Label: "HEADROOM", Name: "Extra usage headroom",
			Value: math.Round(remPct), NumericValue: &remPct, NumericUnit: "percent",
			NumericGoodWhen: "high", Unit: "%", Ratio: &remRatio, Direction: "up",
		},
		providers.MetricValue{
			ID: "extra-usage-limit", Label: "LIMIT",
			Name: fmt.Sprintf("Extra usage monthly limit (%s)", extra.currency),
			Value: fmt.Sprintf("$%.2f", limit), NumericValue: &spent,
			NumericUnit: "dollars", NumericGoodWhen: "low", NumericMax: &limit,
			Caption: "Monthly",
		},
		providers.MetricValue{
			ID: "extra-usage-spent", Label: "SPENT",
			Name: fmt.Sprintf("Extra usage spent (%s)", extra.currency),
			Value: fmt.Sprintf("$%.2f", spent), NumericValue: &spent,
			NumericUnit: "dollars", NumericGoodWhen: "low", NumericMax: &limit,
			Ratio: &spentRatio, Direction: "up",
		},
	)
	return out
}

// --- Provider implementation ---

// Provider fetches Claude usage data via OAuth + optional cookie web API.
type Provider struct{}

func (Provider) ID() string         { return "claude" }
func (Provider) Name() string       { return "Claude" }
func (Provider) BrandColor() string { return "#cc7c5e" }
func (Provider) BrandBg() string    { return "#1c1210" }
func (Provider) MetricIDs() []string {
	return []string{
		"session-percent", "session-pace", "weekly-percent", "weekly-pace",
		"weekly-sonnet-percent", "sonnet-pace", "weekly-opus-percent", "opus-pace",
		"extra-usage-percent", "extra-usage-limit", "extra-usage-spent",
		"extra-usage-enabled", "extra-usage-balance", "extra-usage-auto-reload",
		"cost-today", "cost-30d",
	}
}

func (Provider) Fetch(ctx providers.FetchContext) (providers.Snapshot, error) {
	c, err := loadCreds()
	if err != nil {
		return providers.Snapshot{}, err
	}

	if !hasScope(c.scopes, "user:profile") {
		return providers.Snapshot{}, fmt.Errorf(
			"Claude OAuth token missing `user:profile` scope (has: %s). Run `claude setup-token` to regenerate.",
			strings.Join(c.scopes, ", "))
	}
	if c.expiresAt != nil && c.expiresAt.Before(time.Now()) {
		return providers.Snapshot{}, fmt.Errorf(
			"Claude OAuth access token expired at %s. Run `claude` to re-authenticate.",
			c.expiresAt.Format(time.RFC3339))
	}

	var resp usageResponse
	err = httputil.GetJSON(usageURL, map[string]string{
		"Authorization":  "Bearer " + c.accessToken,
		"anthropic-beta": betaHdr,
		"User-Agent":     userAgent,
		"Content-Type":   "application/json",
	}, 30*time.Second, &resp)

	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) {
			if httpErr.Status == 401 {
				return providers.Snapshot{}, fmt.Errorf(
					"Claude OAuth request unauthorized. Run `claude` to re-authenticate. body=%s",
					httputil.Truncate(httpErr.Body, 200))
			}
			if httpErr.Status == 403 && strings.Contains(httpErr.Body, "user:profile") {
				return providers.Snapshot{}, fmt.Errorf(
					"Claude OAuth token missing `user:profile` scope. Run `claude setup-token` to regenerate.")
			}
			parts := []string{fmt.Sprintf("HTTP %d", httpErr.Status)}
			if ra := httpErr.Header("Retry-After"); ra != "" {
				parts = append(parts, "retry-after="+ra)
			}
			if rid := httpErr.Header("Request-Id"); rid != "" {
				parts = append(parts, "req="+rid)
			} else if rid := httpErr.Header("X-Request-Id"); rid != "" {
				parts = append(parts, "req="+rid)
			}
			return providers.Snapshot{}, fmt.Errorf(
				"Claude OAuth server error: %s body=%s",
				strings.Join(parts, " "), httputil.Truncate(httpErr.Body, 200))
		}
		return providers.Snapshot{}, fmt.Errorf("Claude OAuth network error: %v", err)
	}

	var metrics []providers.MetricValue

	if session := remainingMetric("session-percent", "SESSION", "Session window remaining (5h)", resp.FiveHour); session != nil {
		metrics = append(metrics, *session)
	}
	if resp.FiveHour != nil && resp.FiveHour.Utilization != nil && resp.FiveHour.ResetsAt != nil {
		if t, err := time.Parse(time.RFC3339, *resp.FiveHour.ResetsAt); err == nil {
			if p := providers.PaceMetric(providers.PaceInput{
				MetricID: "session-pace", Label: "Session", Name: "Session pace (5h)",
				UsedPercent: *resp.FiveHour.Utilization, WindowDuration: 5 * time.Hour, ResetIn: time.Until(t),
			}); p != nil {
				metrics = append(metrics, *p)
			}
		}
	}

	weekly := remainingMetric("weekly-percent", "WEEKLY", "Weekly window remaining", resp.SevenDay)
	if weekly != nil {
		metrics = append(metrics, *weekly)
	}
	if resp.SevenDay != nil && resp.SevenDay.Utilization != nil && resp.SevenDay.ResetsAt != nil {
		if t, err := time.Parse(time.RFC3339, *resp.SevenDay.ResetsAt); err == nil {
			if p := providers.PaceMetric(providers.PaceInput{
				MetricID: "weekly-pace", Label: "Weekly", Name: "Weekly pace (7d)",
				UsedPercent: *resp.SevenDay.Utilization, WindowDuration: 7 * 24 * time.Hour, ResetIn: time.Until(t),
			}); p != nil {
				metrics = append(metrics, *p)
			}
		}
	}

	if ms := remainingMetric("weekly-sonnet-percent", "SONNET", "Weekly Sonnet remaining", resp.SevenDaySonnet); ms != nil {
		if ms.ResetInSeconds == nil && weekly != nil && weekly.ResetInSeconds != nil {
			ms.ResetInSeconds = weekly.ResetInSeconds
		}
		metrics = append(metrics, *ms)
	}
	if resp.SevenDaySonnet != nil && resp.SevenDaySonnet.Utilization != nil && resp.SevenDaySonnet.ResetsAt != nil {
		if t, err := time.Parse(time.RFC3339, *resp.SevenDaySonnet.ResetsAt); err == nil {
			if p := providers.PaceMetric(providers.PaceInput{
				MetricID: "sonnet-pace", Label: "Sonnet", Name: "Sonnet pace (7d)",
				UsedPercent: *resp.SevenDaySonnet.Utilization, WindowDuration: 7 * 24 * time.Hour, ResetIn: time.Until(t),
			}); p != nil {
				metrics = append(metrics, *p)
			}
		}
	}

	if resp.SevenDayOpus != nil {
		if mo := remainingMetric("weekly-opus-percent", "OPUS", "Weekly Opus remaining", resp.SevenDayOpus); mo != nil {
			if mo.ResetInSeconds == nil && weekly != nil && weekly.ResetInSeconds != nil {
				mo.ResetInSeconds = weekly.ResetInSeconds
			}
			metrics = append(metrics, *mo)
		}
		if resp.SevenDayOpus.Utilization != nil && resp.SevenDayOpus.ResetsAt != nil {
			if t, err := time.Parse(time.RFC3339, *resp.SevenDayOpus.ResetsAt); err == nil {
				if p := providers.PaceMetric(providers.PaceInput{
					MetricID: "opus-pace", Label: "Opus", Name: "Opus pace (7d)",
					UsedPercent: *resp.SevenDayOpus.Utilization, WindowDuration: 7 * 24 * time.Hour, ResetIn: time.Until(t),
				}); p != nil {
					metrics = append(metrics, *p)
				}
			}
		}
	}

	// Extra usage resolution: OAuth first (when the plan's extras are
	// enabled) and fall through to the browser path (extension) if
	// not. No user toggle — the metric itself determines whether
	// extras are even requested; the fetch is a no-op when the
	// extension isn't connected.
	var extraSrc *extraUsageSource

	oauthUsable := resp.ExtraUsage != nil &&
		resp.ExtraUsage.IsEnabled != nil && *resp.ExtraUsage.IsEnabled &&
		resp.ExtraUsage.MonthlyLimit != nil && *resp.ExtraUsage.MonthlyLimit > 0

	doRefresh := shouldRefreshExtras(resp, ctx.Force)

	if !doRefresh {
		extrasMu.Lock()
		if cachedExtras != nil {
			s := cachedExtras.source
			extraSrc = &s
		}
		extrasMu.Unlock()
	} else if oauthUsable {
		extraSrc = oauthToSource(resp.ExtraUsage)
		cacheExtras(extraSrc)
	} else if web := fetchWebExtras(); web != nil {
		extraSrc = web
		cacheExtras(web)
	}

	metrics = append(metrics, extraUsageMetrics(extraSrc)...)
	metrics = append(metrics, costMetrics()...)

	provName := "Claude"
	if p := planFromTier(c.rateLimitTier); p != "" {
		provName = p
	}

	return providers.Snapshot{
		ProviderID:   "claude",
		ProviderName: provName,
		Source:       "oauth",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

func oauthToSource(raw *extraUsageRaw) *extraUsageSource {
	if raw == nil {
		return nil
	}
	return &extraUsageSource{
		isEnabled:         raw.IsEnabled != nil && *raw.IsEnabled,
		monthlyLimitCents: valOr(raw.MonthlyLimit, 0),
		usedCreditsCents:  valOr(raw.UsedCredits, 0),
		currency:          defStr(ptrStr(raw.Currency), "USD"),
	}
}

func cacheExtras(src *extraUsageSource) {
	if src == nil {
		return
	}
	extrasMu.Lock()
	cachedExtras = &cachedExtrasEntry{source: *src, capturedAt: time.Now()}
	extrasMu.Unlock()
}

func valOr(p *int64, def int64) int64 {
	if p != nil {
		return *p
	}
	return def
}

func defStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func ptrStr(p *string) string {
	if p != nil {
		return *p
	}
	return ""
}

func init() {
	providers.Register(Provider{})
}

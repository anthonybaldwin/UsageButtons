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
	// usageURL is the OAuth-authenticated usage endpoint.
	usageURL = "https://api.anthropic.com/api/oauth/usage"
	// betaHdr identifies the OAuth beta profile required by the usage API.
	betaHdr = "oauth-2025-04-20"
	// userAgent mimics the Claude CLI so the endpoint accepts the request.
	userAgent = "claude-code/2.1.70"
	// baseWebURL is the root of the cookie-authenticated claude.ai web API.
	baseWebURL = "https://claude.ai/api"

	// extrasDefaultTTL is how long cached extras data is reused when the
	// plan is nowhere near its limits.
	extrasDefaultTTL = 60 * time.Minute
	// extrasActiveTTL is the shorter refresh interval used when extras are
	// enabled or session/weekly utilization is near the limit.
	extrasActiveTTL = 15 * time.Minute
	// nearLimitPct is the utilization threshold that switches extras to
	// the active refresh cadence.
	nearLimitPct = 80.0
	// staleResetGrace is how far a window's resets_at may lag "now"
	// before the snapshot is treated as stale data. Covers clock skew
	// and request latency without flagging a freshly-reset window.
	staleResetGrace = 90 * time.Second
)

// LogSink is wired by the plugin for debug logging.
var LogSink func(string)

// logf emits a tagged log line via LogSink when one is configured.
func logf(format string, args ...any) {
	if LogSink != nil {
		LogSink(fmt.Sprintf("[claude] "+format, args...))
	}
}

// --- Credential loading ---

// credFile is the on-disk shape of ~/.claude/.credentials.json.
type credFile struct {
	ClaudeAiOauth *struct {
		AccessToken   string   `json:"accessToken"`
		RefreshToken  string   `json:"refreshToken"`
		ExpiresAt     *int64   `json:"expiresAt"` // ms since epoch
		Scopes        []string `json:"scopes"`
		RateLimitTier string   `json:"rateLimitTier"`
	} `json:"claudeAiOauth"`
}

// creds is the parsed view of a Claude OAuth credential used by Fetch.
type creds struct {
	accessToken   string
	scopes        []string
	expiresAt     *time.Time
	rateLimitTier string
}

// credPath returns the canonical location of the Claude credential file.
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

// loadCreds loads, parses, and validates the Claude OAuth credential blob.
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

// planFromTier maps an Anthropic rate-limit tier string to a user-visible
// plan name, returning "" when the tier is unknown.
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

// hasScope reports whether s is present in the OAuth scopes list.
func hasScope(scopes []string, s string) bool {
	for _, sc := range scopes {
		if sc == s {
			return true
		}
	}
	return false
}

// --- API response types ---

// usageWindow is a single utilization bucket (session or weekly).
type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// extraUsageRaw is the extras block as returned by the OAuth endpoint.
type extraUsageRaw struct {
	IsEnabled    *bool    `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"` // cents (API may return int or float)
	UsedCredits  *float64 `json:"used_credits"`  // cents (API may return int or float)
	Utilization  *float64 `json:"utilization"`
	Currency     *string  `json:"currency"`
}

// usageResponse is the top-level shape returned by both the OAuth
// usage endpoint and the cookie-authenticated organization usage endpoint.
type usageResponse struct {
	FiveHour       *usageWindow `json:"five_hour"`
	SevenDay       *usageWindow `json:"seven_day"`
	SevenDaySonnet *usageWindow `json:"seven_day_sonnet"`
	SevenDayOpus   *usageWindow `json:"seven_day_opus"`
	// seven_day_omelette is Anthropic's internal codename for the
	// Claude Design weekly bucket (surfaced in the claude.ai UI as
	// "Claude Design" alongside "Sonnet only" and "All models").
	SevenDayDesign         *usageWindow   `json:"seven_day_omelette"`
	SevenDayDesignNamed    *usageWindow   `json:"seven_day_design"`
	SevenDayClaudeDesign   *usageWindow   `json:"seven_day_claude_design"`
	SevenDayRoutines       *usageWindow   `json:"seven_day_cowork"`
	SevenDayRoutinesNamed  *usageWindow   `json:"seven_day_routines"`
	SevenDayClaudeRoutines *usageWindow   `json:"seven_day_claude_routines"`
	ExtraUsage             *extraUsageRaw `json:"extra_usage"`
}

// --- Extra usage source (unified shape from OAuth or Web) ---

// extraUsageSource is the provider-internal view of extras spend and
// balance state, regardless of whether it came from OAuth or the web API.
type extraUsageSource struct {
	isEnabled         bool
	monthlyLimitCents int64
	usedCreditsCents  int64
	currency          string
	balanceCents      *int64
	autoReloadEnabled *bool
	// disabledReason / disabledUntil come from the web overage
	// endpoint and describe why account-level overage is paused
	// (e.g. "self_selected_spend_limit_reached" until the next
	// billing cycle). OAuth doesn't expose them.
	disabledReason string
	disabledUntil  *time.Time
}

// --- Extras caching ---

// cachedExtrasEntry is the most recent extraUsageSource retained between
// fetches so the web API isn't hit every poll.
type cachedExtrasEntry struct {
	source     extraUsageSource
	capturedAt time.Time
}

var (
	// extrasMu guards cachedExtras.
	extrasMu sync.Mutex
	// cachedExtras holds the last captured extras snapshot.
	cachedExtras *cachedExtrasEntry
)

// shouldRefreshExtras decides whether to hit the extras endpoints this
// fetch, using the cache TTL and current utilization as inputs.
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

// orgRow is one entry in the claude.ai organizations list response.
type orgRow struct {
	UUID         string   `json:"uuid"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

// overageResponse mirrors the claude.ai overage_spend_limit endpoint.
type overageResponse struct {
	IsEnabled          *bool    `json:"is_enabled"`
	MonthlyCreditLimit *float64 `json:"monthly_credit_limit"`
	UsedCredits        *float64 `json:"used_credits"`
	Currency           string   `json:"currency"`
	AccountEmail       string   `json:"account_email"`
	OutOfCredits       *bool    `json:"out_of_credits"`
	SeatTier           string   `json:"seat_tier"`
	DisabledReason     string   `json:"disabled_reason"`
	DisabledUntil      string   `json:"disabled_until"`
}

// prepaidCreditsResponse mirrors the claude.ai prepaid/credits endpoint.
type prepaidCreditsResponse struct {
	Amount             *float64 `json:"amount"`
	Currency           string   `json:"currency"`
	AutoReloadSettings any      `json:"auto_reload_settings"`
}

var (
	// orgMu guards orgCache.
	orgMu sync.Mutex
	// orgCache memoizes the resolved organization UUID per cacheKey.
	orgCache = map[string]string{}
)

// fetchOrgID resolves the user's chat-capable organization UUID, caching
// the result under cacheKey for the life of the process.
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

// fetchWebExtras calls the claude.ai overage + prepaid endpoints via the
// browser extension and returns an extraUsageSource or nil on failure.
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
		disabledReason:    overage.DisabledReason,
	}
	if overage.DisabledUntil != "" {
		if t, err := time.Parse(time.RFC3339, overage.DisabledUntil); err == nil && t.After(time.Now()) {
			result.disabledUntil = &t
		}
	}

	if prepaidErr == nil {
		if prepaid.Amount != nil {
			rounded := int64(math.Round(*prepaid.Amount))
			result.balanceCents = &rounded
		}
		autoReload := prepaid.AutoReloadSettings != nil
		result.autoReloadEnabled = &autoReload
	}

	return result
}

// fetchWebPrepaid fetches only the prepaid balance + auto-reload state
// via the browser extension. The OAuth extras block doesn't expose
// these fields, so they're layered onto an OAuth-sourced
// extraUsageSource when the extension is available.
func fetchWebPrepaid() (*int64, *bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if !cookies.HostAvailable(ctx) {
		logf("prepaid: extension not connected; skipping web path")
		return nil, nil
	}
	const cacheKey = "extension"

	orgID, err := fetchOrgID(ctx, cacheKey)
	if err != nil {
		logf("prepaid: org fetch failed: %v", err)
		return nil, nil
	}

	var prepaid prepaidCreditsResponse
	if err := cookies.FetchJSON(ctx,
		fmt.Sprintf("%s/organizations/%s/prepaid/credits", baseWebURL, orgID),
		nil, &prepaid,
	); err != nil {
		logf("prepaid: fetch failed: %v", err)
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && httpErr.Status == 401 {
			orgMu.Lock()
			delete(orgCache, cacheKey)
			orgMu.Unlock()
		}
		return nil, nil
	}

	var balanceCents *int64
	if prepaid.Amount != nil {
		rounded := int64(math.Round(*prepaid.Amount))
		balanceCents = &rounded
	}
	autoReload := prepaid.AutoReloadSettings != nil
	return balanceCents, &autoReload
}

// --- Metric construction ---

// remainingMetric builds a "remaining %" MetricValue from a usageWindow.
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

// extraUsageMetrics expands an extraUsageSource into the set of metric
// tiles shown in the UI (spent, limit, headroom, balance, toggle…).
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
		Value: val, Ratio: &onRatio, Direction: "up", Caption: "Toggle",
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
			Value: rv, Ratio: &rr, Direction: "up", Caption: "Auto-reload",
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

	spentCaption := "Account total"
	if reason := humanizeDisabledReason(extra.disabledReason); reason != "" {
		spentCaption = reason
	}

	spentMetric := providers.MetricValue{
		ID: "extra-usage-spent", Label: "SPENT",
		Name:  fmt.Sprintf("Extra usage spent (%s)", extra.currency),
		Value: fmt.Sprintf("$%.2f", spent), NumericValue: &spent,
		NumericUnit: "dollars", NumericGoodWhen: "low", NumericMax: &limit,
		Ratio: &spentRatio, Direction: "up", Caption: spentCaption,
	}
	if extra.disabledUntil != nil {
		delta := time.Until(*extra.disabledUntil).Seconds()
		if delta > 0 {
			spentMetric.ResetInSeconds = &delta
		}
	}

	out = append(out,
		providers.MetricValue{
			ID: "extra-usage-percent", Label: "HEADROOM", Name: "Extra usage headroom",
			Value: math.Round(remPct), NumericValue: &remPct, NumericUnit: "percent",
			NumericGoodWhen: "high", Unit: "%", Ratio: &remRatio, Direction: "up",
		},
		providers.MetricValue{
			ID: "extra-usage-limit", Label: "LIMIT",
			Name:  fmt.Sprintf("Extra usage monthly limit (%s)", extra.currency),
			Value: fmt.Sprintf("$%.2f", limit), NumericValue: &limit,
			NumericUnit: "dollars", NumericGoodWhen: "high",
			Caption: "Monthly",
		},
		spentMetric,
	)
	return out
}

// --- Provider implementation ---

// Provider fetches Claude usage data via OAuth + optional cookie web API.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "claude" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Claude" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#cc7c5e" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#1c1210" }

// MetricIDs enumerates every metric this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{
		"session-percent", "session-pace", "weekly-percent", "weekly-pace",
		"weekly-sonnet-percent", "sonnet-pace", "weekly-opus-percent", "opus-pace",
		"weekly-design-percent", "design-pace", "weekly-routines-percent", "routines-pace",
		"extra-usage-percent", "extra-usage-limit", "extra-usage-spent",
		"extra-usage-enabled", "extra-usage-balance", "extra-usage-auto-reload",
		"cost-today", "cost-30d",
	}
}

// claudeLocalOnlyMetrics is the set of metric IDs that are derived
// from local Claude CLI logs and don't require any claude.ai network
// call. Used by Fetch to short-circuit when no live-quota metric is
// bound — see plans/fetchcontext-active-metrics.md.
var claudeLocalOnlyMetrics = map[string]bool{
	"cost-today": true,
	"cost-30d":   true,
}

// claudeActiveIsLocalOnly returns true when the active set is non-nil,
// non-empty, AND every entry is a local-only metric. nil/empty fall
// through to the full fetch path.
func claudeActiveIsLocalOnly(active []string) bool {
	if len(active) == 0 {
		return false
	}
	for _, id := range active {
		if !claudeLocalOnlyMetrics[id] {
			return false
		}
	}
	return true
}

// Fetch returns the latest Claude usage snapshot, preferring the
// extension-proxied web API and falling back to OAuth.
func (Provider) Fetch(ctx providers.FetchContext) (providers.Snapshot, error) {
	// Demand-fetching: if the only bound metrics are local cost tiles
	// (cost-today / cost-30d), skip every Claude.ai network call and
	// return just the local-log scan. Saves the extension-proxy /usage
	// hit and the org-discovery probe for users who track Claude spend
	// without using the live quota tiles. Empty + nil active set fall
	// through to the full path (cold start, force-refresh, no signal).
	if claudeActiveIsLocalOnly(ctx.ActiveMetricIDs) {
		return providers.Snapshot{
			ProviderID:   "claude",
			ProviderName: "Claude",
			Source:       "local",
			Metrics:      costMetrics(),
			Status:       "operational",
		}, nil
	}

	// Browser-first: hit claude.ai/api/organizations/{id}/usage via the
	// extension when connected. Same JSON shape as the OAuth endpoint,
	// so downstream metric emission is identical. Falls back to OAuth
	// only when the extension isn't reachable or the request fails.
	//
	// Net win when the extension is present: no OAuth token leaves the
	// plugin, real browser TLS fingerprint + cookies + cf_clearance,
	// no expired-token errors for users who've never refreshed their
	// Claude CLI session.
	var (
		resp          usageResponse
		source        string
		rateLimitTier string
	)
	if webResp, ok := tryFetchUsageViaExtension(); ok {
		resp = webResp
		source = "cookie"
		// Optional enrichment for the plan-name display. Missing creds
		// is fine on the browser path — we just show "Claude" generic.
		if c, err := loadCreds(); err == nil {
			rateLimitTier = c.rateLimitTier
		}
	} else {
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
			// Distinguish JSON parse errors (API returned unexpected schema)
			// from actual network failures.
			if strings.Contains(err.Error(), "invalid JSON") || strings.Contains(err.Error(), "cannot unmarshal") {
				return providers.Snapshot{}, fmt.Errorf("Claude API response parse error: %v", err)
			}
			return providers.Snapshot{}, fmt.Errorf("Claude OAuth network error: %v", err)
		}
		source = "oauth"
		rateLimitTier = c.rateLimitTier
	}

	var metrics []providers.MetricValue

	if session := remainingMetric("session-percent", "SESSION", "Session window remaining (5h)", resp.FiveHour); session != nil {
		metrics = append(metrics, *session)
	}
	if resp.FiveHour != nil && resp.FiveHour.Utilization != nil && resp.FiveHour.ResetsAt != nil {
		if t, err := time.Parse(time.RFC3339, *resp.FiveHour.ResetsAt); err == nil {
			if p := providers.PaceMetric(providers.PaceInput{
				MetricID: "session-pace", Label: "SESSION", Name: "Session pace (5h)",
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
				MetricID: "weekly-pace", Label: "WEEKLY", Name: "Weekly pace (7d)",
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
				MetricID: "sonnet-pace", Label: "SONNET", Name: "Sonnet pace (7d)",
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
					MetricID: "opus-pace", Label: "OPUS", Name: "Opus pace (7d)",
					UsedPercent: *resp.SevenDayOpus.Utilization, WindowDuration: 7 * 24 * time.Hour, ResetIn: time.Until(t),
				}); p != nil {
					metrics = append(metrics, *p)
				}
			}
		}
	}

	designWindow := firstWindow(resp.SevenDayDesign, resp.SevenDayDesignNamed, resp.SevenDayClaudeDesign)
	if designWindow != nil {
		if md := remainingMetric("weekly-design-percent", "DESIGN", "Weekly Claude Design remaining", designWindow); md != nil {
			if md.ResetInSeconds == nil && weekly != nil && weekly.ResetInSeconds != nil {
				md.ResetInSeconds = weekly.ResetInSeconds
			}
			metrics = append(metrics, *md)
		}
		if designWindow.Utilization != nil && designWindow.ResetsAt != nil {
			if t, err := time.Parse(time.RFC3339, *designWindow.ResetsAt); err == nil {
				if p := providers.PaceMetric(providers.PaceInput{
					MetricID: "design-pace", Label: "DESIGN", Name: "Claude Design pace (7d)",
					UsedPercent: *designWindow.Utilization, WindowDuration: 7 * 24 * time.Hour, ResetIn: time.Until(t),
				}); p != nil {
					metrics = append(metrics, *p)
				}
			}
		}
	}

	routinesWindow := firstWindow(resp.SevenDayRoutines, resp.SevenDayRoutinesNamed, resp.SevenDayClaudeRoutines)
	if routinesWindow != nil {
		if mr := remainingMetric("weekly-routines-percent", "ROUTINES", "Weekly Daily Routines remaining", routinesWindow); mr != nil {
			if mr.ResetInSeconds == nil && weekly != nil && weekly.ResetInSeconds != nil {
				mr.ResetInSeconds = weekly.ResetInSeconds
			}
			metrics = append(metrics, *mr)
		}
		if routinesWindow.Utilization != nil && routinesWindow.ResetsAt != nil {
			if t, err := time.Parse(time.RFC3339, *routinesWindow.ResetsAt); err == nil {
				if p := providers.PaceMetric(providers.PaceInput{
					MetricID: "routines-pace", Label: "ROUTINES", Name: "Daily Routines pace (7d)",
					UsedPercent: *routinesWindow.Utilization, WindowDuration: 7 * 24 * time.Hour, ResetIn: time.Until(t),
				}); p != nil {
					metrics = append(metrics, *p)
				}
			}
		}
	}

	// Stale-snapshot detection: if any window's resets_at is in the
	// past beyond staleResetGrace, the upstream path served cached
	// data (typically Chrome's HTTP cache via the Helper extension's
	// service worker — or a flaky OAuth response). Mark only the
	// window/pace metrics built above; extras and cost metrics come
	// from separate sources and stay untouched.
	if anyStaleResetWindow(resp, time.Now()) {
		applyStaleWindowMarker(metrics, source)
		logf("stale snapshot: a window's resets_at is in the past (source=%s)", source)
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
		if bal, ar := fetchWebPrepaid(); bal != nil || ar != nil {
			extraSrc.balanceCents = bal
			extraSrc.autoReloadEnabled = ar
		}
		cacheExtras(extraSrc)
	} else if web := fetchWebExtras(); web != nil {
		extraSrc = web
		cacheExtras(web)
	}

	metrics = append(metrics, extraUsageMetrics(extraSrc)...)
	metrics = append(metrics, costMetrics()...)

	provName := "Claude"
	if p := planFromTier(rateLimitTier); p != "" {
		provName = p
	}

	return providers.Snapshot{
		ProviderID:   "claude",
		ProviderName: provName,
		Source:       source,
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// tryFetchUsageViaExtension attempts the browser-proxied /usage fetch.
// Returns ok=false on any failure (extension missing, org lookup
// failed, non-2xx response, decode error) so the caller falls through
// to the OAuth path. Silent by design — the direct path is the
// canonical fallback and surfaces real errors if it also fails.
func tryFetchUsageViaExtension() (usageResponse, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return usageResponse{}, false
	}
	orgID, err := fetchOrgID(ctx, "extension")
	if err != nil {
		logf("extension /usage skipped: org lookup failed: %v", err)
		return usageResponse{}, false
	}
	var resp usageResponse
	url := fmt.Sprintf("%s/organizations/%s/usage", baseWebURL, orgID)
	if err := cookies.FetchJSON(ctx, url, nil, &resp); err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && httpErr.Status == 401 {
			// Cached orgID may still be valid but the session cookie
			// expired. Drop the cache so the next attempt retries from
			// scratch (in case the user logs back into claude.ai).
			orgMu.Lock()
			delete(orgCache, "extension")
			orgMu.Unlock()
		}
		logf("extension /usage failed: %v — falling through to OAuth", err)
		return usageResponse{}, false
	}
	return resp, true
}

// oauthToSource converts the OAuth extras payload into the common
// extraUsageSource shape, returning nil if raw itself is nil.
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

// cacheExtras stores src as the most recent extraUsageSource snapshot
// used by shouldRefreshExtras on subsequent polls.
func cacheExtras(src *extraUsageSource) {
	if src == nil {
		return
	}
	extrasMu.Lock()
	cachedExtras = &cachedExtrasEntry{source: *src, capturedAt: time.Now()}
	extrasMu.Unlock()
}

// valOr rounds *p to an int64 or returns def when p is nil.
func valOr(p *float64, def int64) int64 {
	if p != nil {
		return int64(math.Round(*p))
	}
	return def
}

// defStr returns v when non-empty and fallback otherwise.
func defStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// humanizeDisabledReason maps Anthropic's machine-readable
// disabled_reason values to a short caption that fits a Stream Deck
// button. Returns "" for unknown or empty reasons.
func humanizeDisabledReason(r string) string {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "":
		return ""
	case "self_selected_spend_limit_reached":
		return "Limit hit"
	case "out_of_credits":
		return "No credits"
	case "payment_failed":
		return "Payment failed"
	default:
		return ""
	}
}

// ptrStr dereferences p or returns "" when p is nil.
func ptrStr(p *string) string {
	if p != nil {
		return *p
	}
	return ""
}

// anyStaleResetWindow reports whether any non-nil window in resp has a
// resets_at more than staleResetGrace in the past, which signals the
// upstream path is serving pre-reset data.
func anyStaleResetWindow(resp usageResponse, now time.Time) bool {
	for _, w := range []*usageWindow{
		resp.FiveHour, resp.SevenDay,
		resp.SevenDaySonnet, resp.SevenDayOpus,
		firstWindow(resp.SevenDayDesign, resp.SevenDayDesignNamed, resp.SevenDayClaudeDesign),
		firstWindow(resp.SevenDayRoutines, resp.SevenDayRoutinesNamed, resp.SevenDayClaudeRoutines),
	} {
		if w == nil || w.ResetsAt == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, *w.ResetsAt)
		if err != nil {
			continue
		}
		if now.Sub(t) > staleResetGrace {
			return true
		}
	}
	return false
}

// firstWindow returns the first populated usage window from windows.
func firstWindow(windows ...*usageWindow) *usageWindow {
	for _, w := range windows {
		if w != nil && (w.Utilization != nil || (w.ResetsAt != nil && *w.ResetsAt != "")) {
			return w
		}
	}
	return nil
}

// applyStaleWindowMarker dims every window/pace metric and replaces
// its countdown with an actionable caption so the user can tell the
// displayed numbers are from before the upstream window reset.
func applyStaleWindowMarker(metrics []providers.MetricValue, source string) {
	caption := "Stale data"
	if source == "cookie" {
		caption = "Reload Helper"
	}
	t := true
	for i := range metrics {
		metrics[i].Stale = &t
		metrics[i].Caption = caption
		// Suppress "0s" countdown so the caption wins the subvalue slot
		// (see cmd/plugin/main.go subvalue priority).
		metrics[i].ResetInSeconds = nil
	}
}

// init registers the Claude provider with the package registry.
func init() {
	providers.Register(Provider{})
}

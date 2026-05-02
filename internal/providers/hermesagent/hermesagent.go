// Package hermesagent implements the Hermes Agent self-hosted
// dashboard provider (NousResearch/hermes-agent on GitHub).
//
// Hermes Agent is Nous Research's self-hostable coding-agent product.
// It ships a FastAPI dashboard backed by SQLite, listening on
// 127.0.0.1:9119 by default. Users typically run it on their dev box
// or a Tailscale node, so the base URL is user-configurable in the PI
// (per provider, with per-button override). Distinct from the public
// portal.nousresearch.com provider in internal/providers/nousresearch/.
//
// Auth: the dashboard generates a per-process ephemeral session token
// (secrets.token_urlsafe(32)) at server start and injects it into the
// SPA's index.html as window.__HERMES_SESSION_TOKEN__. The provider
// scrapes that string on first use and sends it as the
// X-Hermes-Session-Token header on subsequent /api/analytics/* calls.
// If the scrape fails (server changed the injection format, paranoid
// proxy strips it, …) the user can paste a session token in the PI as
// a fallback. Token cache is invalidated on 401 so a server restart
// auto-recovers.
//
// Endpoints used:
//
//	GET  /                          — index.html, scraped for token
//	GET  /api/analytics/usage?days=N — gated; returns daily[], by_model[],
//	                                   totals (sum across N days) for the
//	                                   {1, 7, 30}-day windows we expose
//	GET  /api/status                — public; active_sessions count
//
// Source pointers (commit-pinned, NousResearch/hermes-agent):
//
//	hermes_cli/web_server.py:74      — _SESSION_TOKEN
//	hermes_cli/web_server.py:3196-3204 — token injection format
//	hermes_cli/web_server.py:2697-2762 — /api/analytics/usage handler
//	hermes_cli/web_server.py:511-616 — /api/status (active_sessions)
package hermesagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	providerID    = "hermes-agent"
	providerName  = "Hermes Agent"
	defaultBase   = "http://127.0.0.1:9119"
	sessionHeader = "X-Hermes-Session-Token"
	fetchTimeout  = 20 * time.Second
)

// tokenInjectionRe matches the line the dashboard injects into its
// index.html on every render:
//
//	<script>window.__HERMES_SESSION_TOKEN__="...";...</script>
//
// Source: hermes_cli/web_server.py:3203 (NousResearch/hermes-agent).
var tokenInjectionRe = regexp.MustCompile(`window\.__HERMES_SESSION_TOKEN__="([^"]+)"`)

// usageResponse is the relevant slice of GET /api/analytics/usage.
// We only read totals — the daily[] / by_model[] / skills bits are
// rendered by the dashboard itself, not by Stream Deck buttons.
type usageResponse struct {
	Totals     usageTotals `json:"totals"`
	PeriodDays int         `json:"period_days"`
}

// usageTotals mirrors the SUM(...) AS total_* row the analytics
// handler builds — see hermes_cli/web_server.py:2735-2745. Field names
// are the column aliases the SQL query uses.
type usageTotals struct {
	TotalInput         int64   `json:"total_input"`
	TotalOutput        int64   `json:"total_output"`
	TotalCacheRead     int64   `json:"total_cache_read"`
	TotalReasoning     int64   `json:"total_reasoning"`
	TotalEstimatedCost float64 `json:"total_estimated_cost"`
	TotalActualCost    float64 `json:"total_actual_cost"`
	TotalSessions      int64   `json:"total_sessions"`
	TotalAPICalls      int64   `json:"total_api_calls"`
}

// statusResponse is the relevant slice of GET /api/status. Public
// endpoint, no auth required.
type statusResponse struct {
	ActiveSessions int    `json:"active_sessions"`
	Version        string `json:"version"`
}

// window is one of the three time slices we emit metrics for.
type window struct {
	// Days is the value passed as ?days=N to /api/analytics/usage.
	Days int
	// Slug is the metric-ID suffix and PI dropdown row key.
	Slug string
	// Label is the short Stream Deck tile label (uppercase, <=5 chars).
	Label string
}

// windows are the three time slices we expose. Order matters — the PI
// dropdown shows them in this order, and Stream Deck preserves it.
var windows = []window{
	{Days: 1, Slug: "daily", Label: "DAY"},
	{Days: 7, Slug: "weekly", Label: "WEEK"},
	{Days: 30, Slug: "monthly", Label: "MONTH"},
}

// Provider fetches Hermes Agent dashboard usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return providerID }

// Name returns the human-readable provider name.
func (Provider) Name() string { return providerName }

// BrandColor returns the meter-fill accent — emerald-500. Distinct
// from the Nous Research portal's teal-500 so a deck running both
// providers reads them as separate at-a-glance.
func (Provider) BrandColor() string { return "#10b981" }

// BrandBg returns the deep complement of the emerald accent.
func (Provider) BrandBg() string { return "#022c22" }

// MetricIDs enumerates every metric this provider can emit.
//
// Naming convention:
//
//	hermes-agent-<view>-<window>
//
// where <view> ∈ {input-tokens, output-tokens, cache-tokens,
// total-tokens, cost} and <window> ∈ {daily, weekly, monthly}. Plus
// one extra: hermes-agent-active-sessions (no window — current
// sessions active in the last 5 min from /api/status).
func (Provider) MetricIDs() []string {
	views := []string{"input-tokens", "output-tokens", "cache-tokens", "total-tokens", "cost"}
	ids := make([]string, 0, len(views)*len(windows)+1)
	for _, v := range views {
		for _, w := range windows {
			ids = append(ids, "hermes-agent-"+v+"-"+w.Slug)
		}
	}
	ids = append(ids, "hermes-agent-active-sessions")
	return ids
}

// Fetch returns the latest Hermes Agent usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	base := resolveBase()
	if base == "" {
		return errorSnapshot("Hermes Agent base URL is not set."), nil
	}

	token, err := resolveToken(ctx, base)
	if err != nil {
		return errorSnapshot(err.Error()), nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var metrics []providers.MetricValue

	for _, w := range windows {
		usage, err := fetchUsage(ctx, base, token, w.Days)
		if err != nil {
			if isAuthFailure(err) {
				clearCachedToken()
				return errorSnapshot("Hermes Agent rejected the session token. Restart the dashboard or paste a fresh token in the PI."), nil
			}
			return errorSnapshot(fmt.Sprintf("Hermes Agent /api/analytics/usage failed: %s", short(err))), nil
		}
		metrics = append(metrics, windowMetrics(w, usage.Totals, now)...)
	}

	if active, err := fetchStatus(ctx, base); err == nil {
		metrics = append(metrics, activeSessionsMetric(active.ActiveSessions, now))
	}

	return providers.Snapshot{
		ProviderID:   providerID,
		ProviderName: providerName,
		Source:       "self-hosted",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// windowMetrics builds the five metrics emitted for one time-window
// off the totals row from /api/analytics/usage.
func windowMetrics(w window, t usageTotals, now string) []providers.MetricValue {
	totalTokens := t.TotalInput + t.TotalOutput + t.TotalCacheRead + t.TotalReasoning
	return []providers.MetricValue{
		tokenMetric("hermes-agent-input-tokens-"+w.Slug, w.Label, "Input", "Hermes Agent input tokens ("+w.Slug+")", t.TotalInput, now),
		tokenMetric("hermes-agent-output-tokens-"+w.Slug, w.Label, "Output", "Hermes Agent output tokens ("+w.Slug+")", t.TotalOutput, now),
		tokenMetric("hermes-agent-cache-tokens-"+w.Slug, w.Label, "Cache", "Hermes Agent cache-read tokens ("+w.Slug+")", t.TotalCacheRead, now),
		tokenMetric("hermes-agent-total-tokens-"+w.Slug, w.Label, "Total", "Hermes Agent total tokens, input+output+cache+reasoning ("+w.Slug+")", totalTokens, now),
		costMetric("hermes-agent-cost-"+w.Slug, w.Label, "Cost", "Hermes Agent estimated cost ("+w.Slug+")", t.TotalEstimatedCost, now),
	}
}

// tokenMetric builds one count-style token tile.
func tokenMetric(id, label, caption, name string, count int64, now string) providers.MetricValue {
	v := float64(count)
	return providers.MetricValue{
		ID:              id,
		Label:           label,
		Name:            name,
		Value:           formatCount(count),
		NumericValue:    &v,
		NumericUnit:     "count",
		NumericGoodWhen: "low",
		Caption:         caption,
		UpdatedAt:       now,
	}
}

// costMetric builds one dollar-style cost tile.
func costMetric(id, label, caption, name string, dollars float64, now string) providers.MetricValue {
	rounded := math.Round(dollars*100) / 100
	return providers.MetricValue{
		ID:              id,
		Label:           label,
		Name:            name,
		Value:           fmt.Sprintf("$%.2f", rounded),
		NumericValue:    &rounded,
		NumericUnit:     "dollars",
		NumericGoodWhen: "low",
		Caption:         caption,
		UpdatedAt:       now,
	}
}

// activeSessionsMetric builds the standalone active-sessions tile.
func activeSessionsMetric(count int, now string) providers.MetricValue {
	v := float64(count)
	return providers.MetricValue{
		ID:              "hermes-agent-active-sessions",
		Label:           "ACTIVE",
		Name:            "Hermes Agent sessions active in the last 5 minutes",
		Value:           fmt.Sprintf("%d", count),
		NumericValue:    &v,
		NumericUnit:     "count",
		NumericGoodWhen: "high",
		Caption:         "Sessions",
		UpdatedAt:       now,
	}
}

// resolveBase returns the Hermes Agent base URL: user setting first,
// then HERMES_AGENT_BASE_URL env, then the loopback default.
func resolveBase() string {
	pk := settings.ProviderKeysGet()
	return settings.ResolveEndpoint(pk.HermesAgentBaseURL, defaultBase, "HERMES_AGENT_BASE_URL")
}

// resolveToken returns a valid session token, preferring a user-pasted
// value, then a cached scrape, then a fresh scrape of <base>/.
func resolveToken(ctx context.Context, base string) (string, error) {
	pk := settings.ProviderKeysGet()
	if user := settings.ResolveAPIKey(pk.HermesAgentToken, "HERMES_AGENT_TOKEN"); user != "" {
		return user, nil
	}
	if t := getCachedToken(base); t != "" {
		return t, nil
	}
	t, err := scrapeToken(ctx, base)
	if err != nil {
		return "", fmt.Errorf("Hermes Agent: cannot reach dashboard at %s — %s", base, short(err))
	}
	if t == "" {
		return "", fmt.Errorf("Hermes Agent: no session token in dashboard HTML at %s — paste a token in the PI as fallback", base)
	}
	setCachedToken(base, t)
	return t, nil
}

// scrapeToken fetches the dashboard's index.html and extracts the
// injected session token.
func scrapeToken(ctx context.Context, base string) (string, error) {
	body, err := getRaw(ctx, base+"/")
	if err != nil {
		return "", err
	}
	if m := tokenInjectionRe.FindStringSubmatch(string(body)); len(m) == 2 {
		return m[1], nil
	}
	return "", nil
}

// fetchUsage GETs /api/analytics/usage?days=N with the session token.
func fetchUsage(ctx context.Context, base, token string, days int) (usageResponse, error) {
	u := fmt.Sprintf("%s/api/analytics/usage?days=%d", base, days)
	var out usageResponse
	headers := map[string]string{
		sessionHeader: token,
		"Accept":      "application/json",
	}
	err := getJSONWithCtx(ctx, u, headers, &out)
	return out, err
}

// fetchStatus GETs /api/status (public endpoint, no auth).
func fetchStatus(ctx context.Context, base string) (statusResponse, error) {
	var out statusResponse
	err := getJSONWithCtx(ctx, base+"/api/status", map[string]string{"Accept": "application/json"}, &out)
	return out, err
}

// --- token cache ---

var (
	tokenMu       sync.RWMutex
	cachedTok     string
	cachedTokBase string
)

// getCachedToken returns the cached token if its base URL still
// matches the active configuration. A user changing the base URL
// invalidates the cache implicitly.
func getCachedToken(base string) string {
	tokenMu.RLock()
	defer tokenMu.RUnlock()
	if cachedTokBase != base {
		return ""
	}
	return cachedTok
}

// setCachedToken stores the freshly scraped token for the given base.
func setCachedToken(base, token string) {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	cachedTok = token
	cachedTokBase = base
}

// clearCachedToken drops the cached token. Called on 401 so a dashboard
// restart triggers a fresh scrape on the next poll.
func clearCachedToken() {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	cachedTok = ""
	cachedTokBase = ""
}

// --- HTTP helpers ---

// getRaw performs a GET and returns the raw response body. Used for
// the index.html scrape; the rest of the provider speaks JSON.
func getRaw(ctx context.Context, rawURL string) ([]byte, error) {
	if _, err := url.Parse(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", httputil.DefaultUserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httputil.Error{Status: resp.StatusCode, StatusText: resp.Status, Body: string(body), URL: rawURL, Headers: resp.Header}
	}
	return body, nil
}

// getJSONWithCtx is GetJSON with our caller's context — saves the
// per-call timeout-context dance every fetch site would otherwise need.
func getJSONWithCtx(ctx context.Context, rawURL string, headers map[string]string, dst any) error {
	deadline, ok := ctx.Deadline()
	timeout := fetchTimeout
	if ok {
		if d := time.Until(deadline); d > 0 && d < timeout {
			timeout = d
		}
	}
	return httputil.GetJSON(rawURL, headers, timeout, dst)
}

// isAuthFailure reports whether err means the dashboard rejected our
// token (401 / 403 from the auth middleware).
func isAuthFailure(err error) bool {
	var httpErr *httputil.Error
	if errors.As(err, &httpErr) {
		return httpErr.Status == 401 || httpErr.Status == 403
	}
	return false
}

// short returns a compact, log-safe error string.
func short(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// formatCount formats integer token counts with k/M suffixes.
func formatCount(n int64) string {
	v := float64(n)
	sign := ""
	if n < 0 {
		sign = "-"
		v = -v
	}
	switch {
	case v >= 1_000_000_000:
		return fmt.Sprintf("%s%.1fB", sign, v/1_000_000_000)
	case v >= 1_000_000:
		return fmt.Sprintf("%s%.1fM", sign, v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%s%.1fk", sign, v/1_000)
	default:
		return fmt.Sprintf("%s%.0f", sign, v)
	}
}

// errorSnapshot returns a setup or auth failure snapshot with no
// metrics — same pattern as every other provider.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   providerID,
		ProviderName: providerName,
		Source:       "self-hosted",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// init registers the Hermes Agent provider with the package registry.
func init() {
	providers.Register(Provider{})
}

// Package opencode implements the unified OpenCode usage provider.
//
// OpenCode has four product tiers:
//
//   - Free        — IP-based rate limit inside Zen proxy. No console
//     data accessible. No metrics exposed by this
//     provider; users on Free see "no data" once their
//     button can't bind to a Black, Go, or Billing
//     metric.
//   - Lite (Go)   — paid prepaid plan ($12 / $30 / $60). Exposes
//     rolling, weekly, and monthly usage windows via
//     the `lite.subscription.get` server function.
//   - Black       — paid Zen subscription ($20 / $100 / $200).
//     Exposes rolling and weekly usage windows via the
//     `subscription.get` server function. No monthly
//     window for this tier.
//   - Enterprise  — sales-only, no console route, not surfaced.
//
// Cross-cutting credit / balance / auto-reload state lives on the
// shared `billing.get` server function, used by both Lite and Black
// users.
//
// This provider folds the previous `opencodego` provider back in:
// rather than two action UUIDs and two registry entries that share
// auth, host, and request envelope, one provider exposes namespaced
// `black-*` / `go-*` / `billing-*` metrics and lets the user pick
// whichever lane matches their plan via the property inspector.
//
// Auth: Usage Buttons Helper extension with the user's opencode.ai
// browser session. Endpoint: POST/GET https://opencode.ai/_server.
package opencode

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const (
	baseURL            = "https://opencode.ai"
	serverURL          = "https://opencode.ai/_server"
	workspacesServerID = "def39973159c7f0483d8793a822b8dbb10d067e12c65455fcb4608459ba0234f"
	// blackServerID is the SolidStart content-addressed hash for the
	// `subscription.get` server function — the Black plan's rolling +
	// weekly usage windows. Captured from a live opencode.ai session.
	blackServerID = "7abeebee372f304e050aaaf92be863f4a86490e382f8c79db68fd94040d691b4"
	// liteServerID is the SolidStart content-addressed hash for the
	// `lite.subscription.get` server function — the Lite (Go) plan's
	// rolling + weekly + monthly usage windows. SolidStart hashes the
	// function key + file location; the value here is a placeholder
	// until captured from a live session's DevTools network panel.
	// When empty, Lite metrics fall through to "no data" without
	// erroring the whole snapshot.
	liteServerID = ""
	// billingServerID is the SolidStart content-addressed hash for the
	// `billing.get` server function — the cross-cutting balance /
	// auto-reload / monthly cap state shared by Lite and Black. Same
	// "captured from DevTools" gap as liteServerID; an empty value
	// disables billing metrics until the hash lands.
	billingServerID = ""
)

var (
	workspaceIDRE = regexp.MustCompile(`id\s*:\s*\\?"(wrk_[^\\"]+)`)
	// missingHashLogOnce keeps Fetch() from spamming the log with the
	// same "lite/billing hash not captured" warning each minute. The
	// gap is documented; one warning per plugin launch is enough.
	missingHashLogOnce sync.Once
)

// blackSnapshot captures the Black-plan fields parsed from
// subscription.get. Pointer fields stay nil when absent so callers can
// distinguish "user has no Black plan" from "Black plan is at 0%
// usage". UpdatedAt is populated unconditionally — it's the parse
// timestamp, not a payload field.
type blackSnapshot struct {
	HasSubscription     bool
	Plan                string // "20" | "100" | "200" — best-effort
	RollingUsagePercent float64
	WeeklyUsagePercent  float64
	RollingResetInSec   int
	WeeklyResetInSec    int
	RollingStatus       string // "ok" | "rate-limited" — best-effort
	WeeklyStatus        string
	UpdatedAt           time.Time
}

// liteSnapshot captures the Lite (Go) plan fields parsed from
// lite.subscription.get.
type liteSnapshot struct {
	HasSubscription     bool
	RollingUsagePercent float64
	WeeklyUsagePercent  float64
	MonthlyUsagePercent float64
	RollingResetInSec   int
	WeeklyResetInSec    int
	MonthlyResetInSec   int
	RollingStatus       string
	WeeklyStatus        string
	MonthlyStatus       string
	UpdatedAt           time.Time
}

// billingSnapshot captures the shared billing.get fields.
type billingSnapshot struct {
	HasBilling       bool
	BalanceUSD       float64 // micro-cents → USD
	MonthlyLimitUSD  float64 // cents → USD
	MonthlyUsageUSD  float64 // micro-cents → USD
	HasMonthlyLimit  bool
	HasMonthlyUsage  bool
	AutoReloadOn     bool
	ReloadTriggerUSD float64
	ReloadAmountUSD  float64
	HasReloadAmounts bool
	ReloadError      string
	PaymentLast4     string
	SubscriptionPlan string
	UpdatedAt        time.Time
}

// usageWindow is one rolling/weekly/monthly window parsed from a
// flexible JSON payload.
type usageWindow struct {
	Percent    float64
	ResetInSec int
	Status     string
}

// windowCandidate is one parsed usage window with its JSON path so the
// black/go disambiguation logic can pick the right one.
type windowCandidate struct {
	Percent    float64
	ResetInSec int
	Status     string
	PathLower  string
}

// Provider fetches OpenCode usage data for all three lanes.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "opencode" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "OpenCode" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#3b82f6" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#081a33" }

// MetricIDs enumerates every metric this provider can emit, namespaced
// by lane (`black-*` / `go-*` / `billing-*`). The PI surfaces them in
// optgroups; the user picks one per button.
func (Provider) MetricIDs() []string {
	return []string{
		// Black (paid Zen subscription).
		"black-rolling-percent",
		"black-weekly-percent",
		"black-rolling-status",
		"black-weekly-status",
		"black-plan",
		// Go (Lite).
		"go-rolling-percent",
		"go-weekly-percent",
		"go-monthly-percent",
		"go-rolling-status",
		"go-weekly-status",
		"go-monthly-status",
		// Billing (shared).
		"billing-balance-usd",
		"billing-monthly-limit-usd",
		"billing-monthly-usage-usd",
		"billing-monthly-percent",
		"billing-auto-reload-on",
		"billing-reload-trigger-usd",
		"billing-reload-amount-usd",
		"billing-reload-error",
		"billing-payment-last4",
		"billing-subscription-plan",
	}
}

// Fetch returns the latest OpenCode usage snapshot. Black, Lite, and
// Billing lanes fetch in parallel; missing or null lanes degrade
// gracefully to "no data" rather than erroring the whole snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("opencode.ai")), nil
	}
	wsID, err := workspaceID(ctx)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot(cookieaux.StaleMessage("opencode.ai")), nil
		}
		if looksSignedOut(err.Error()) {
			return errorSnapshot(cookieaux.StaleMessage("opencode.ai")), nil
		}
		return errorSnapshot(err.Error()), nil
	}

	now := time.Now().UTC()
	var (
		wg                            sync.WaitGroup
		black                         blackSnapshot
		lite                          liteSnapshot
		billing                       billingSnapshot
		blackErr, liteErr, billingErr error
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		black, blackErr = fetchBlack(ctx, wsID, now)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if liteServerID == "" {
			// Hash not captured yet; surface no-data rather than HTTP
			// 400 / null. logMissingHashes() handles the user-visible
			// note via plugin log.
			lite = liteSnapshot{UpdatedAt: now}
			return
		}
		lite, liteErr = fetchLite(ctx, wsID, now)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if billingServerID == "" {
			billing = billingSnapshot{UpdatedAt: now}
			return
		}
		billing, billingErr = fetchBilling(ctx, wsID, now)
	}()

	wg.Wait()
	logMissingHashes()

	// If every lane errored AND we got a clear signed-out signal, surface
	// stale-cookie. Otherwise per-lane errors degrade to no-data for that
	// lane only.
	if blackErr != nil && liteErr != nil && billingErr != nil {
		if errIsSignedOut(blackErr) || errIsSignedOut(liteErr) || errIsSignedOut(billingErr) {
			return errorSnapshot(cookieaux.StaleMessage("opencode.ai")), nil
		}
	}

	return assembleSnapshot(black, lite, billing, now), nil
}

// errIsSignedOut reports whether err looks like an auth failure.
func errIsSignedOut(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *httputil.Error
	if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
		return true
	}
	return looksSignedOut(err.Error())
}

// logMissingHashes emits a one-time launch-time warning when the lite
// or billing server-fn hashes haven't been captured yet. Lets users
// know why those metrics return "no data" without a recurring nag.
func logMissingHashes() {
	if liteServerID != "" && billingServerID != "" {
		return
	}
	missingHashLogOnce.Do(func() {
		fmt.Fprintln(os.Stderr,
			"opencode: lite.subscription.get / billing.get hashes not captured; "+
				"go-* and billing-* metrics will return no data until populated. "+
				"See plans/opencode-tier-coverage.md.")
	})
}

// fetchBlack fetches and parses the Black plan's subscription.get.
func fetchBlack(ctx context.Context, wsID string, now time.Time) (blackSnapshot, error) {
	text, err := callServerFn(ctx, blackServerID, []any{wsID},
		fmt.Sprintf("%s/workspace/%s/billing", baseURL, wsID))
	if err != nil {
		return blackSnapshot{UpdatedAt: now}, err
	}
	if looksSignedOut(text) {
		return blackSnapshot{UpdatedAt: now}, fmt.Errorf("OpenCode session is signed out")
	}
	return parseBlackResponse(text, now), nil
}

// fetchLite fetches and parses the Lite plan's lite.subscription.get.
func fetchLite(ctx context.Context, wsID string, now time.Time) (liteSnapshot, error) {
	text, err := callServerFn(ctx, liteServerID, []any{wsID},
		fmt.Sprintf("%s/workspace/%s/go", baseURL, wsID))
	if err != nil {
		return liteSnapshot{UpdatedAt: now}, err
	}
	if looksSignedOut(text) {
		return liteSnapshot{UpdatedAt: now}, fmt.Errorf("OpenCode session is signed out")
	}
	return parseLiteResponse(text, now), nil
}

// fetchBilling fetches and parses the shared billing.get.
func fetchBilling(ctx context.Context, wsID string, now time.Time) (billingSnapshot, error) {
	text, err := callServerFn(ctx, billingServerID, []any{wsID},
		fmt.Sprintf("%s/workspace/%s/billing", baseURL, wsID))
	if err != nil {
		return billingSnapshot{UpdatedAt: now}, err
	}
	if looksSignedOut(text) {
		return billingSnapshot{UpdatedAt: now}, fmt.Errorf("OpenCode session is signed out")
	}
	return parseBillingResponse(text, now), nil
}

// callServerFn performs a GET _server call and falls back to POST when
// the GET response is null or unrecognized. Mirrors the existing
// fetchSubscriptionInfo behavior so all three lanes share retry logic.
func callServerFn(ctx context.Context, serverID string, args []any, referer string) (string, error) {
	text, err := serverText(ctx, serverRequest{
		ServerID: serverID,
		Args:     args,
		Method:   "GET",
		Referer:  referer,
	})
	if err != nil {
		return "", err
	}
	if (isNullPayload(text) || !payloadLooksUsable(text)) && !looksLikeEmptyUsage(text) {
		fallback, fbErr := serverText(ctx, serverRequest{
			ServerID: serverID,
			Args:     args,
			Method:   "POST",
			Referer:  referer,
		})
		if fbErr == nil && !isNullPayload(fallback) {
			text = fallback
		}
	}
	return text, nil
}

// payloadLooksUsable reports whether text contains any plausible usage
// markers — covers all three lanes' field names so the GET→POST retry
// triggers across the same "looks empty but isn't a real empty-state"
// shapes the original opencode provider already handled.
func payloadLooksUsable(text string) bool {
	for _, marker := range []string{
		"rollingUsage", "weeklyUsage", "monthlyUsage", "usagePercent",
		"balance", "monthlyLimit", "subscriptionPlan", "reloadAmount",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// workspaceID returns an override or discovers the first OpenCode workspace.
func workspaceID(ctx context.Context) (string, error) {
	return WorkspaceID(ctx, "OPENCODE_WORKSPACE_ID")
}

// WorkspaceID returns an override or discovers the first OpenCode
// workspace. Exported so the legacy migration path can resolve the
// workspace via the same logic without round-tripping through the
// provider registry.
func WorkspaceID(ctx context.Context, envName string) (string, error) {
	if override := normalizeWorkspaceID(os.Getenv(envName)); override != "" {
		return override, nil
	}
	text, err := serverText(ctx, serverRequest{
		ServerID: workspacesServerID,
		Method:   "GET",
		Referer:  baseURL,
	})
	if err != nil {
		return "", err
	}
	if looksSignedOut(text) {
		return "", fmt.Errorf("OpenCode session is signed out")
	}
	ids := parseWorkspaceIDs(text)
	if len(ids) == 0 {
		fallback, fallbackErr := serverText(ctx, serverRequest{
			ServerID: workspacesServerID,
			Args:     []any{},
			Method:   "POST",
			Referer:  baseURL,
		})
		if fallbackErr == nil {
			ids = parseWorkspaceIDs(fallback)
		}
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("OpenCode response missing workspace id")
	}
	return ids[0], nil
}

// serverRequest describes one OpenCode _server call.
type serverRequest struct {
	ServerID string
	Args     []any
	Method   string
	Referer  string
}

// serverText calls an OpenCode _server endpoint through the Helper.
func serverText(ctx context.Context, req serverRequest) (string, error) {
	if req.ServerID == "" {
		return "", fmt.Errorf("OpenCode: empty server-fn hash")
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = "GET"
	}
	rawURL, body, err := serverRequestURL(req.ServerID, req.Args, method)
	if err != nil {
		return "", err
	}
	headers := map[string]string{
		"Accept":            "text/javascript, application/json;q=0.9, */*;q=0.8",
		"Origin":            baseURL,
		"Referer":           req.Referer,
		"X-Server-Id":       req.ServerID,
		"X-Server-Instance": "server-fn:" + newRequestID(),
	}
	if method != "GET" {
		headers["Content-Type"] = "application/json"
	}
	resp, err := cookies.Fetch(ctx, cookies.Request{
		URL:     rawURL,
		Method:  method,
		Headers: headers,
		Body:    body,
	})
	if err != nil {
		return "", err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return "", &httputil.Error{
			Status:     resp.Status,
			StatusText: resp.StatusText,
			Body:       string(resp.Body),
			URL:        rawURL,
		}
	}
	return string(resp.Body), nil
}

// serverRequestURL builds the _server URL and optional JSON body.
func serverRequestURL(serverID string, args []any, method string) (string, []byte, error) {
	if method != "GET" {
		body, err := json.Marshal(args)
		return serverURL, body, err
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", nil, err
	}
	q := u.Query()
	q.Set("id", serverID)
	if len(args) > 0 {
		body, err := json.Marshal(args)
		if err != nil {
			return "", nil, err
		}
		q.Set("args", string(body))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil, nil
}

// parseBlackResponse parses the Black plan's subscription.get payload.
// Empty / null / no-subscription shapes return a snapshot with
// HasSubscription=false rather than erroring, so the unified Fetch
// can degrade those metrics to no-data while keeping other lanes.
func parseBlackResponse(text string, now time.Time) blackSnapshot {
	out := blackSnapshot{UpdatedAt: now}
	rolling, weekly, ok := extractBlackWindows(text, now)
	if ok {
		out.HasSubscription = true
		out.RollingUsagePercent = rolling.Percent
		out.WeeklyUsagePercent = weekly.Percent
		out.RollingResetInSec = rolling.ResetInSec
		out.WeeklyResetInSec = weekly.ResetInSec
		out.RollingStatus = rolling.Status
		out.WeeklyStatus = weekly.Status
	}
	if plan := extractFirstString(text, []string{"subscriptionPlan", "plan"}); plan != "" {
		// Plan strings come through as "20" / "100" / "200" — surface
		// verbatim so the badge metric stays consistent with the
		// console UI.
		if plan != "null" {
			out.Plan = plan
			out.HasSubscription = true
		}
	}
	return out
}

// parseLiteResponse parses the Lite (Go) plan's lite.subscription.get
// payload. Same degrade-on-empty contract as parseBlackResponse.
func parseLiteResponse(text string, now time.Time) liteSnapshot {
	out := liteSnapshot{UpdatedAt: now}
	rolling, weekly, monthly, ok := extractLiteWindows(text, now)
	if ok {
		out.HasSubscription = true
		out.RollingUsagePercent = rolling.Percent
		out.WeeklyUsagePercent = weekly.Percent
		out.RollingResetInSec = rolling.ResetInSec
		out.WeeklyResetInSec = weekly.ResetInSec
		out.RollingStatus = rolling.Status
		out.WeeklyStatus = weekly.Status
		if monthly != nil {
			out.MonthlyUsagePercent = monthly.Percent
			out.MonthlyResetInSec = monthly.ResetInSec
			out.MonthlyStatus = monthly.Status
		}
	}
	return out
}

// parseBillingResponse parses the shared billing.get payload. Returns
// HasBilling=false on null/empty shapes — billing metrics gracefully
// degrade to no-data, just like the per-plan lanes.
func parseBillingResponse(text string, now time.Time) billingSnapshot {
	out := billingSnapshot{UpdatedAt: now}

	// Try strict JSON first.
	var raw any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &raw); err == nil {
		fillBillingFromJSON(raw, &out)
		if out.HasBilling {
			return out
		}
	}

	// Fall through to regex extraction over Solid SSR text — the same
	// approach the Black/Lite parsers use, since the SSR output isn't
	// valid JSON and a regex over balanced-curly limited windows is
	// more reliable than a custom parser for the full Solid format.
	if balance, ok := extractFloatField(text, "balance"); ok {
		// balance is in micro-cents (1e-6 USD per unit per the schema).
		out.BalanceUSD = balance / 1_000_000.0
		out.HasBilling = true
	}
	if limit, ok := extractFloatField(text, "monthlyLimit"); ok {
		// monthlyLimit is in cents → USD.
		out.MonthlyLimitUSD = limit / 100.0
		out.HasMonthlyLimit = true
		out.HasBilling = true
	}
	if usage, ok := extractFloatField(text, "monthlyUsage"); ok {
		// monthlyUsage is in micro-cents → USD.
		out.MonthlyUsageUSD = usage / 1_000_000.0
		out.HasMonthlyUsage = true
		out.HasBilling = true
	}
	if reload := extractFirstString(text, []string{"reload"}); reload == "true" {
		out.AutoReloadOn = true
		out.HasBilling = true
	}
	if trig, ok := extractFloatField(text, "reloadTrigger"); ok {
		out.ReloadTriggerUSD = trig / 100.0
		out.HasReloadAmounts = true
		out.HasBilling = true
	}
	if amt, ok := extractFloatField(text, "reloadAmount"); ok {
		out.ReloadAmountUSD = amt / 100.0
		out.HasReloadAmounts = true
		out.HasBilling = true
	}
	if errStr := extractFirstString(text, []string{"reloadError"}); errStr != "" && errStr != "null" {
		out.ReloadError = errStr
		out.HasBilling = true
	}
	if last4 := extractFirstString(text, []string{"paymentMethodLast4"}); last4 != "" && last4 != "null" {
		out.PaymentLast4 = last4
		out.HasBilling = true
	}
	if plan := extractFirstString(text, []string{"subscriptionPlan"}); plan != "" && plan != "null" {
		out.SubscriptionPlan = plan
		out.HasBilling = true
	}
	return out
}

// fillBillingFromJSON walks a decoded JSON value and copies any
// recognized billing fields into out. Quietly tolerates null fields
// and missing keys.
func fillBillingFromJSON(raw any, out *billingSnapshot) {
	switch v := raw.(type) {
	case map[string]any:
		if balance, ok := providerutil.FirstFloat(v, "balance"); ok {
			out.BalanceUSD = balance / 1_000_000.0
			out.HasBilling = true
		}
		if limit, ok := providerutil.FirstFloat(v, "monthlyLimit"); ok {
			out.MonthlyLimitUSD = limit / 100.0
			out.HasMonthlyLimit = true
			out.HasBilling = true
		}
		if usage, ok := providerutil.FirstFloat(v, "monthlyUsage"); ok {
			out.MonthlyUsageUSD = usage / 1_000_000.0
			out.HasMonthlyUsage = true
			out.HasBilling = true
		}
		if reloadVal, exists := v["reload"]; exists {
			if b, ok := reloadVal.(bool); ok && b {
				out.AutoReloadOn = true
				out.HasBilling = true
			}
		}
		if trig, ok := providerutil.FirstFloat(v, "reloadTrigger"); ok {
			out.ReloadTriggerUSD = trig / 100.0
			out.HasReloadAmounts = true
			out.HasBilling = true
		}
		if amt, ok := providerutil.FirstFloat(v, "reloadAmount"); ok {
			out.ReloadAmountUSD = amt / 100.0
			out.HasReloadAmounts = true
			out.HasBilling = true
		}
		if s := providerutil.FirstString(v, "reloadError"); s != "" {
			out.ReloadError = s
			out.HasBilling = true
		}
		if s := providerutil.FirstString(v, "paymentMethodLast4"); s != "" {
			out.PaymentLast4 = s
			out.HasBilling = true
		}
		if s := providerutil.FirstString(v, "subscriptionPlan"); s != "" {
			out.SubscriptionPlan = s
			out.HasBilling = true
		}
		// Recurse into nested objects so wrapped envelopes
		// ({data: {...}}, etc.) still fill out.
		for _, item := range v {
			fillBillingFromJSON(item, out)
		}
	case []any:
		for _, item := range v {
			fillBillingFromJSON(item, out)
		}
	}
}

// extractBlackWindows returns the rolling/weekly windows from a Black
// subscription payload. Mirrors the previous parseSubscription path but
// keeps the windowCandidate type local to the new parser shape.
func extractBlackWindows(text string, now time.Time) (rolling, weekly usageWindow, ok bool) {
	if r, w, found := windowsFromJSON(text, now, false); found {
		return r, w, true
	}
	rPercent := extractFloat(`rollingUsage[^}]*?usagePercent"?\s*:\s*([0-9]+(?:\.[0-9]+)?)`, text)
	rReset := extractInt(`rollingUsage[^}]*?resetInSec"?\s*:\s*([0-9]+)`, text)
	wPercent := extractFloat(`weeklyUsage[^}]*?usagePercent"?\s*:\s*([0-9]+(?:\.[0-9]+)?)`, text)
	wReset := extractInt(`weeklyUsage[^}]*?resetInSec"?\s*:\s*([0-9]+)`, text)
	if rPercent != nil && rReset != nil && wPercent != nil && wReset != nil {
		return usageWindow{
				Percent:    clampPercent(*rPercent),
				ResetInSec: *rReset,
				Status:     extractWindowStatus(text, "rollingUsage"),
			},
			usageWindow{
				Percent:    clampPercent(*wPercent),
				ResetInSec: *wReset,
				Status:     extractWindowStatus(text, "weeklyUsage"),
			},
			true
	}
	return usageWindow{}, usageWindow{}, false
}

// extractLiteWindows returns the rolling/weekly/monthly windows from a
// Lite subscription payload.
func extractLiteWindows(text string, now time.Time) (rolling, weekly usageWindow, monthly *usageWindow, ok bool) {
	if r, w, found := windowsFromJSON(text, now, true); found {
		// JSON path may surface a monthly window too — extract via
		// regex from the same text since the JSON walker doesn't keep
		// path metadata in the typed return.
		mPercent := extractFloat(`monthlyUsage[^}]*?usagePercent"?\s*:\s*([0-9]+(?:\.[0-9]+)?)`, text)
		mReset := extractInt(`monthlyUsage[^}]*?resetInSec"?\s*:\s*([0-9]+)`, text)
		var m *usageWindow
		if mPercent != nil {
			reset := 0
			if mReset != nil {
				reset = *mReset
			}
			m = &usageWindow{
				Percent:    clampPercent(*mPercent),
				ResetInSec: reset,
				Status:     extractWindowStatus(text, "monthlyUsage"),
			}
		}
		return r, w, m, true
	}
	rPercent := extractFloat(`rollingUsage[^}]*?usagePercent"?\s*:\s*([0-9]+(?:\.[0-9]+)?)`, text)
	rReset := extractInt(`rollingUsage[^}]*?resetInSec"?\s*:\s*([0-9]+)`, text)
	wPercent := extractFloat(`weeklyUsage[^}]*?usagePercent"?\s*:\s*([0-9]+(?:\.[0-9]+)?)`, text)
	wReset := extractInt(`weeklyUsage[^}]*?resetInSec"?\s*:\s*([0-9]+)`, text)
	if rPercent != nil && rReset != nil && wPercent != nil && wReset != nil {
		rolling = usageWindow{
			Percent:    clampPercent(*rPercent),
			ResetInSec: *rReset,
			Status:     extractWindowStatus(text, "rollingUsage"),
		}
		weekly = usageWindow{
			Percent:    clampPercent(*wPercent),
			ResetInSec: *wReset,
			Status:     extractWindowStatus(text, "weeklyUsage"),
		}
		mPercent := extractFloat(`monthlyUsage[^}]*?usagePercent"?\s*:\s*([0-9]+(?:\.[0-9]+)?)`, text)
		mReset := extractInt(`monthlyUsage[^}]*?resetInSec"?\s*:\s*([0-9]+)`, text)
		if mPercent != nil {
			reset := 0
			if mReset != nil {
				reset = *mReset
			}
			monthly = &usageWindow{
				Percent:    clampPercent(*mPercent),
				ResetInSec: reset,
				Status:     extractWindowStatus(text, "monthlyUsage"),
			}
		}
		return rolling, weekly, monthly, true
	}
	return usageWindow{}, usageWindow{}, nil, false
}

// extractWindowStatus pulls a "status: \"...\"" value scoped to a
// named window block. Returns "" when no status field is present —
// callers map an empty string to "ok" for the metric value.
func extractWindowStatus(text, windowName string) string {
	// Allow letters and dashes in the status value (e.g. "rate-limited").
	// `"?` absorbs JSON's closing field-name quote on both sides.
	pat := fmt.Sprintf(`%s"?[^}]*?status"?\s*:\s*\\?"([a-zA-Z-]+)"`, regexp.QuoteMeta(windowName))
	re, err := regexp.Compile(pat)
	if err != nil {
		return ""
	}
	m := re.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// windowsFromJSON tries strict JSON parsing first. Returns rolling and
// weekly when both are present. wantMonthly only affects which
// candidate ranks are picked — the typed return doesn't include
// monthly because the typed path is rare; callers fall back to regex
// extraction for monthly.
func windowsFromJSON(text string, now time.Time, _ bool) (rolling, weekly usageWindow, ok bool) {
	var raw any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &raw); err != nil {
		return usageWindow{}, usageWindow{}, false
	}
	var candidates []windowCandidate
	collectWindowCandidates(raw, now, nil, &candidates)
	if len(candidates) == 0 {
		return usageWindow{}, usageWindow{}, false
	}
	r := pickWindow(candidates, true, "rolling", "hour", "5h", "5-hour")
	w := pickWindow(candidates, false, "weekly", "week")
	if r == nil {
		r = pickAnyWindow(candidates, true, nil)
	}
	if w == nil {
		w = pickAnyWindow(candidates, false, r)
	}
	if r == nil || w == nil {
		return usageWindow{}, usageWindow{}, false
	}
	return usageWindow{Percent: r.Percent, ResetInSec: r.ResetInSec, Status: r.Status},
		usageWindow{Percent: w.Percent, ResetInSec: w.ResetInSec, Status: w.Status},
		true
}

// looksLikeEmptyUsage reports whether text is an OpenCode _server
// response that conveys "no rolling/weekly usage" rather than a
// schema break. Identical contract to the previous package.
func looksLikeEmptyUsage(text string) bool {
	if strings.Contains(text, "usagePercent") {
		return false
	}
	if !strings.Contains(text, "server-fn:") {
		return false
	}
	compact := strings.Join(strings.Fields(text), "")
	if strings.HasSuffix(compact, ",null)") {
		return true
	}
	for _, marker := range []string{
		"rollingUsage:null",
		"weeklyUsage:null",
		"monthlyUsage:null",
		"subscription:null",
		"subscriptionPlan:null",
		"monthlyUsage:null",
		"monthlyLimit:null",
		"usage:$R",
		"usage:[]",
		"keys:$R",
		"keys:[]",
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}

// collectWindowCandidates finds quota-like objects in arbitrary JSON.
func collectWindowCandidates(value any, now time.Time, path []string, out *[]windowCandidate) {
	switch v := value.(type) {
	case map[string]any:
		if window, ok := parseWindow(v, now); ok {
			*out = append(*out, windowCandidate{
				Percent:    window.Percent,
				ResetInSec: window.ResetInSec,
				Status:     window.Status,
				PathLower:  strings.ToLower(strings.Join(path, ".")),
			})
		}
		for key, item := range v {
			collectWindowCandidates(item, now, append(path, key), out)
		}
	case []any:
		for i, item := range v {
			collectWindowCandidates(item, now, append(path, fmt.Sprintf("[%d]", i)), out)
		}
	}
}

// parseWindow extracts percent, reset, and status from a JSON object.
func parseWindow(m map[string]any, now time.Time) (usageWindow, bool) {
	percentKeys := []string{
		"usagePercent", "usedPercent", "percentUsed", "percent",
		"usage_percent", "used_percent", "utilization",
		"utilizationPercent", "utilization_percent", "usage",
	}
	resetInKeys := []string{
		"resetInSec", "resetInSeconds", "resetSeconds", "reset_sec",
		"reset_in_sec", "resetsInSec", "resetsInSeconds", "resetIn", "resetSec",
	}
	resetAtKeys := []string{
		"resetAt", "resetsAt", "reset_at", "resets_at",
		"nextReset", "next_reset", "renewAt", "renew_at",
	}
	percent, ok := providerutil.FirstFloat(m, percentKeys...)
	if !ok {
		used, usedOK := providerutil.FirstFloat(m, "used", "usage", "consumed", "count", "usedTokens")
		limit, limitOK := providerutil.FirstFloat(m, "limit", "total", "quota", "max", "cap", "tokenLimit")
		if usedOK && limitOK && limit > 0 {
			percent = used / limit * 100
			ok = true
		}
	}
	if !ok {
		return usageWindow{}, false
	}
	reset, resetOK := providerutil.FirstFloat(m, resetInKeys...)
	if !resetOK {
		if resetAt, ok := providerutil.FirstTime(m, resetAtKeys...); ok {
			reset = math.Max(0, resetAt.Sub(now).Seconds())
			resetOK = true
		}
	}
	if !resetOK {
		reset = 0
	}
	status := providerutil.FirstString(m, "status")
	return usageWindow{
		Percent:    clampPercent(percent),
		ResetInSec: int(math.Round(reset)),
		Status:     status,
	}, true
}

// pickWindow chooses a candidate matching one of the path hints.
func pickWindow(candidates []windowCandidate, pickShorter bool, hints ...string) *windowCandidate {
	var filtered []windowCandidate
	for _, candidate := range candidates {
		for _, hint := range hints {
			if strings.Contains(candidate.PathLower, hint) {
				filtered = append(filtered, candidate)
				break
			}
		}
	}
	return pickAnyWindow(filtered, pickShorter, nil)
}

// pickAnyWindow chooses by shortest or longest reset.
func pickAnyWindow(candidates []windowCandidate, pickShorter bool, excluding *windowCandidate) *windowCandidate {
	var picked *windowCandidate
	for _, candidate := range candidates {
		if excluding != nil && candidate.PathLower == excluding.PathLower && candidate.ResetInSec == excluding.ResetInSec {
			continue
		}
		c := candidate
		if picked == nil {
			picked = &c
			continue
		}
		if pickShorter {
			if candidate.ResetInSec < picked.ResetInSec {
				picked = &c
			}
		} else if candidate.ResetInSec > picked.ResetInSec {
			picked = &c
		}
	}
	return picked
}

// assembleSnapshot translates the three lane snapshots into the final
// Stream Deck metric set. Lane fields that aren't populated produce a
// "no data" caption so the button stays clean rather than reading 0.
func assembleSnapshot(black blackSnapshot, lite liteSnapshot, billing billingSnapshot, now time.Time) providers.Snapshot {
	nowStr := now.Format(time.RFC3339)
	metrics := []providers.MetricValue{}

	// --- Black lane ---
	if black.HasSubscription {
		metrics = append(metrics,
			percentMetric("black-rolling-percent", "ROLLING",
				"OpenCode Black rolling window remaining (5h)",
				black.RollingUsagePercent, black.RollingResetInSec, nowStr),
			percentMetric("black-weekly-percent", "WEEKLY",
				"OpenCode Black weekly window remaining",
				black.WeeklyUsagePercent, black.WeeklyResetInSec, nowStr),
			statusMetric("black-rolling-status", "STATUS",
				"OpenCode Black rolling window status",
				statusOrOK(black.RollingStatus), nowStr),
			statusMetric("black-weekly-status", "STATUS",
				"OpenCode Black weekly window status",
				statusOrOK(black.WeeklyStatus), nowStr),
		)
		if black.Plan != "" {
			metrics = append(metrics, planMetric("black-plan", "PLAN",
				"OpenCode Black plan tier",
				black.Plan, nowStr))
		}
	}

	// --- Go (Lite) lane ---
	if lite.HasSubscription {
		metrics = append(metrics,
			percentMetric("go-rolling-percent", "ROLLING",
				"OpenCode Go rolling window remaining (5h)",
				lite.RollingUsagePercent, lite.RollingResetInSec, nowStr),
			percentMetric("go-weekly-percent", "WEEKLY",
				"OpenCode Go weekly window remaining",
				lite.WeeklyUsagePercent, lite.WeeklyResetInSec, nowStr),
			percentMetric("go-monthly-percent", "MONTHLY",
				"OpenCode Go monthly window remaining",
				lite.MonthlyUsagePercent, lite.MonthlyResetInSec, nowStr),
			statusMetric("go-rolling-status", "STATUS",
				"OpenCode Go rolling window status",
				statusOrOK(lite.RollingStatus), nowStr),
			statusMetric("go-weekly-status", "STATUS",
				"OpenCode Go weekly window status",
				statusOrOK(lite.WeeklyStatus), nowStr),
			statusMetric("go-monthly-status", "STATUS",
				"OpenCode Go monthly window status",
				statusOrOK(lite.MonthlyStatus), nowStr),
		)
	}

	// --- Billing lane ---
	if billing.HasBilling {
		metrics = append(metrics,
			dollarMetric("billing-balance-usd", "BALANCE",
				"OpenCode credit balance",
				billing.BalanceUSD, nowStr),
		)
		if billing.HasMonthlyLimit {
			metrics = append(metrics,
				dollarMetric("billing-monthly-limit-usd", "LIMIT",
					"OpenCode monthly spending limit",
					billing.MonthlyLimitUSD, nowStr),
			)
		}
		if billing.HasMonthlyUsage {
			metrics = append(metrics,
				dollarMetric("billing-monthly-usage-usd", "MONTH",
					"OpenCode month-to-date spend",
					billing.MonthlyUsageUSD, nowStr),
			)
		}
		if billing.HasMonthlyLimit && billing.HasMonthlyUsage && billing.MonthlyLimitUSD > 0 {
			pct := math.Min(100, math.Max(0, billing.MonthlyUsageUSD/billing.MonthlyLimitUSD*100))
			metrics = append(metrics,
				percentMetric("billing-monthly-percent", "MONTH",
					"OpenCode monthly spend % of limit",
					pct, 0, nowStr),
			)
		}
		metrics = append(metrics,
			toggleMetric("billing-auto-reload-on", "RELOAD",
				"OpenCode auto-reload enabled",
				billing.AutoReloadOn, nowStr),
		)
		if billing.HasReloadAmounts {
			metrics = append(metrics,
				dollarMetric("billing-reload-trigger-usd", "TRIGGER",
					"OpenCode auto-reload trigger threshold",
					billing.ReloadTriggerUSD, nowStr),
				dollarMetric("billing-reload-amount-usd", "TOPUP",
					"OpenCode auto-reload top-up amount",
					billing.ReloadAmountUSD, nowStr),
			)
		}
		if billing.ReloadError != "" {
			metrics = append(metrics,
				stringMetric("billing-reload-error", "ERROR",
					"OpenCode auto-reload error",
					billing.ReloadError, nowStr),
			)
		}
		if billing.PaymentLast4 != "" {
			metrics = append(metrics,
				stringMetric("billing-payment-last4", "CARD",
					"OpenCode payment method last4",
					"…"+billing.PaymentLast4, nowStr),
			)
		}
		if billing.SubscriptionPlan != "" {
			metrics = append(metrics,
				planMetric("billing-subscription-plan", "PLAN",
					"OpenCode subscription plan",
					billing.SubscriptionPlan, nowStr),
			)
		}
	}

	return providers.Snapshot{
		ProviderID:   "opencode",
		ProviderName: "OpenCode",
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// statusOrOK normalizes an empty status to "ok" so the metric value
// reads the same whether the API omitted the field or returned the
// happy-path string.
func statusOrOK(status string) string {
	s := strings.TrimSpace(status)
	if s == "" {
		return "ok"
	}
	return s
}

// percentMetric builds a remaining-percent OpenCode metric.
func percentMetric(id, label, name string, usedPct float64, resetSeconds int, now string) providers.MetricValue {
	var resetAt *time.Time
	if resetSeconds > 0 {
		t := time.Now().Add(time.Duration(resetSeconds) * time.Second)
		resetAt = &t
	}
	return providerutil.PercentRemainingMetric(id, label, name, usedPct, resetAt, "", now)
}

// statusMetric builds a string-valued status metric (rate-limited /
// ok). Reference card style — no fill bar.
func statusMetric(id, label, name, value, now string) providers.MetricValue {
	return providers.MetricValue{
		ID:        id,
		Label:     label,
		Name:      name,
		Value:     value,
		UpdatedAt: now,
	}
}

// dollarMetric builds a dollar-valued reference metric.
func dollarMetric(id, label, name string, valueUSD float64, now string) providers.MetricValue {
	v := valueUSD
	return providers.MetricValue{
		ID:           id,
		Label:        label,
		Name:         name,
		Value:        fmt.Sprintf("$%.2f", valueUSD),
		NumericValue: &v,
		NumericUnit:  "dollars",
		UpdatedAt:    now,
	}
}

// toggleMetric builds an on/off boolean metric.
func toggleMetric(id, label, name string, on bool, now string) providers.MetricValue {
	value := "OFF"
	if on {
		value = "ON"
	}
	return providers.MetricValue{
		ID:        id,
		Label:     label,
		Name:      name,
		Value:     value,
		UpdatedAt: now,
	}
}

// planMetric builds a plan-tier label metric.
func planMetric(id, label, name, plan, now string) providers.MetricValue {
	display := plan
	// Bare "20" / "100" / "200" are friendlier as "$20".
	if _, err := strconv.Atoi(plan); err == nil {
		display = "$" + plan
	}
	return providers.MetricValue{
		ID:        id,
		Label:     label,
		Name:      name,
		Value:     display,
		UpdatedAt: now,
	}
}

// stringMetric builds a generic string-valued reference metric.
func stringMetric(id, label, name, value, now string) providers.MetricValue {
	return providers.MetricValue{
		ID:        id,
		Label:     label,
		Name:      name,
		Value:     value,
		UpdatedAt: now,
	}
}

// parseWorkspaceIDs finds workspace IDs in serialized text or JSON.
func parseWorkspaceIDs(text string) []string {
	found := uniqueStrings(workspaceIDRE.FindAllStringSubmatch(text, -1))
	if len(found) > 0 {
		return found
	}
	var raw any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &raw); err != nil {
		return nil
	}
	var out []string
	collectWorkspaceIDs(raw, &out)
	return out
}

// collectWorkspaceIDs walks arbitrary JSON looking for wrk_ strings.
func collectWorkspaceIDs(value any, out *[]string) {
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(v, "wrk_") && !containsString(*out, v) {
			*out = append(*out, v)
		}
	case []any:
		for _, item := range v {
			collectWorkspaceIDs(item, out)
		}
	case map[string]any:
		for _, item := range v {
			collectWorkspaceIDs(item, out)
		}
	}
}

// uniqueStrings returns regex capture group 1 values without duplicates.
func uniqueStrings(matches [][]string) []string {
	var out []string
	for _, match := range matches {
		if len(match) > 1 && !containsString(out, match[1]) {
			out = append(out, match[1])
		}
	}
	return out
}

// containsString reports whether values contains needle.
func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// normalizeWorkspaceID extracts a wrk_ identifier from text or URL.
func normalizeWorkspaceID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "wrk_") {
		return trimmed
	}
	if u, err := url.Parse(trimmed); err == nil {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i, part := range parts {
			if part == "workspace" && i+1 < len(parts) && strings.HasPrefix(parts[i+1], "wrk_") {
				return parts[i+1]
			}
		}
	}
	re := regexp.MustCompile(`wrk_[A-Za-z0-9]+`)
	return re.FindString(trimmed)
}

// isNullPayload reports explicit null responses.
func isNullPayload(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.EqualFold(trimmed, "null")
}

// looksSignedOut reports whether text is an auth/login response.
func looksSignedOut(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "login") ||
		strings.Contains(lower, "sign in") ||
		strings.Contains(lower, "auth/authorize") ||
		strings.Contains(lower, "not associated with an account") ||
		strings.Contains(lower, `actor of type "public"`)
}

// extractFloat extracts a float from the first capture group.
func extractFloat(pattern string, text string) *float64 {
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return nil
	}
	v, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return nil
	}
	return &v
}

// extractInt extracts an int from the first capture group.
func extractInt(pattern string, text string) *int {
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return nil
	}
	v, err := strconv.Atoi(match[1])
	if err != nil {
		return nil
	}
	return &v
}

// extractFloatField pulls a numeric value for a named field from
// minified Solid SSR or JSON text. Tolerates `"field": 123`,
// `field:123`, and scientific notation.
func extractFloatField(text, field string) (float64, bool) {
	pat := fmt.Sprintf(`%s"?\s*:\s*(-?[0-9]+(?:\.[0-9]+)?(?:[eE][+\-]?[0-9]+)?)`,
		regexp.QuoteMeta(field))
	v := extractFloat(pat, text)
	if v == nil {
		return 0, false
	}
	return *v, true
}

// extractFirstString returns the first quoted or bare value matching
// any of fields. Quoted strings come back without the surrounding
// quotes. `null` is returned verbatim so callers can distinguish "no
// match" (empty) from "explicit null".
//
// Both JSON-quoted (`"field":"value"`) and Solid-SSR-bare
// (`field:"value"` or `field:value`) shapes are accepted — the
// optional `"?` after the field name absorbs JSON's closing field-name
// quote.
func extractFirstString(text string, fields []string) string {
	for _, f := range fields {
		// Quoted variant first.
		quoted := fmt.Sprintf(`%s"?\s*:\s*\\?"([^"]*)"`, regexp.QuoteMeta(f))
		re := regexp.MustCompile(quoted)
		if m := re.FindStringSubmatch(text); len(m) >= 2 {
			return m[1]
		}
		// Bare variant for booleans / null / numbers used as labels.
		bare := fmt.Sprintf(`%s"?\s*:\s*([a-zA-Z0-9_]+)`, regexp.QuoteMeta(f))
		re = regexp.MustCompile(bare)
		if m := re.FindStringSubmatch(text); len(m) >= 2 {
			return m[1]
		}
	}
	return ""
}

// clampPercent normalizes 0..1 or 0..100 values to 0..100.
func clampPercent(value float64) float64 {
	if value >= 0 && value <= 1 {
		value *= 100
	}
	return math.Max(0, math.Min(100, value))
}

// errorSnapshot returns an OpenCode setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "opencode",
		ProviderName: "OpenCode",
		Source:       "cookie",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// newRequestID returns a v4-style UUID string used in X-Server-Instance.
// OpenCode's server appears to expect a unique ID per call.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// dumpUnknownResponse appends a truncated OpenCode response to a temp
// file when no parser can classify it. Owner-only perms (0o600); a
// per-call snippet cap and total-file cap keep growth bounded.
//
//nolint:unused // retained for future parser regressions.
func dumpUnknownResponse(text string) {
	const (
		maxSnippetBytes = 16 * 1024
		maxFileBytes    = 256 * 1024
	)
	path := filepath.Join(os.TempDir(), "usagebuttons-opencode-debug.txt")
	if info, err := os.Stat(path); err == nil && info.Size() >= maxFileBytes {
		return
	}
	snippet := text
	truncated := false
	if len(snippet) > maxSnippetBytes {
		snippet = snippet[:maxSnippetBytes]
		truncated = true
	}
	body := fmt.Sprintf("[%s] length=%d truncated=%v\n%s\n\n",
		time.Now().UTC().Format(time.RFC3339), len(text), truncated, snippet)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(body)
}

// init registers the OpenCode provider with the package registry.
func init() {
	providers.Register(Provider{})
}

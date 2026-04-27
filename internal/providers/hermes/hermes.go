// Package hermes implements the Nous Research portal usage provider.
//
// Auth: Usage Buttons Helper extension with the user's
// portal.nousresearch.com browser session (cookies). Branded "Hermes"
// after the Hermes Agent product, but the same subscription pool funds
// both Hermes Agent and Nous Chat — see the IMPORTANT note in the
// portal's API-keys page: "Subscription credits are for Nous Chat and
// Hermes Agent and do not count towards direct API access."
//
// Endpoints (all GET, both server-render the relevant JSON inline):
//
//	GET /products  — subscription tier, monthly credits, balance,
//	                  rollover cap, renewal date, API credits balance
//	GET /api-keys  — totals.{spend,requests,tokens} for the API tier
//
// We deliberately avoid the Server Action POST /usage for v1: its
// `Next-Action` header value is a content hash that rotates on every
// Nous deploy, which would mean shipping a plugin update every time
// the portal redeploys. The server-rendered HTML is stable, public,
// and exposes the figures we need — so we read them directly.
package hermes

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const (
	productsURL = "https://portal.nousresearch.com/products"
	apiKeysURL  = "https://portal.nousresearch.com/api-keys"
	provID      = "hermes"
	provName    = "Hermes"
)

// Provider fetches Nous Research portal usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return provID }

// Name returns the human-readable provider name.
func (Provider) Name() string { return provName }

// BrandColor returns the meter-fill accent (teal-500). Nous's own
// portal accents on teal-700, but at Stream Deck button size that
// darker shade smudges into the teal-950 bg — bumping to the brighter
// teal-500 keeps fill and bg visually distinct so the meter reads
// crisply at every fill level.
func (Provider) BrandColor() string { return "#14b8a6" }

// BrandBg returns the background color (deep complement of the teal).
func (Provider) BrandBg() string { return "#042f2e" }

// MetricIDs enumerates the metrics this provider can emit.
//
// Naming convention for all-time totals (from /api-keys page):
//
//	hermes-{view}-total       — combined across every allowance bucket
//	hermes-{view}-{source}    — restricted to one allowance (api / sub)
//
// View ∈ {spend, requests, tokens, input-tokens, output-tokens,
// cache-read-tokens, cache-write-tokens}. Source ∈ {api, sub}. The
// "total" suffix is reserved for the combined view to keep the
// per-source IDs readable on small Stream Deck tiles.
//
// hermes-api-spend-total and hermes-api-requests-total predate the
// total/per-source split — kept as aliases for hermes-spend-total /
// hermes-requests-total so v1 button bindings don't break.
func (Provider) MetricIDs() []string {
	ids := []string{
		"hermes-sub-credits-remaining",
		"hermes-api-credits-remaining",
		// All-source totals (combined across api + sub allowances).
		"hermes-spend-total",
		"hermes-requests-total",
		"hermes-tokens-total",
		"hermes-input-tokens-total",
		"hermes-output-tokens-total",
		"hermes-cache-read-tokens-total",
		"hermes-cache-write-tokens-total",
		// Aliases preserving v1 IDs.
		"hermes-api-spend-total",
		"hermes-api-requests-total",
	}
	for _, src := range []string{"api", "sub"} {
		for _, view := range []string{"spend", "requests", "tokens", "input-tokens", "output-tokens", "cache-read-tokens", "cache-write-tokens"} {
			ids = append(ids, "hermes-"+view+"-"+src)
		}
	}
	return ids
}

// Fetch returns the latest Nous Research usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("nousresearch.com")), nil
	}
	headers := map[string]string{
		"Accept":  "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Origin":  "https://portal.nousresearch.com",
		"Referer": "https://portal.nousresearch.com/",
	}

	products, err := fetchHTML(ctx, productsURL, headers)
	if err != nil {
		return mapHTTPError(err), nil
	}
	usage := snapshotFromHTML(products, time.Now().UTC())

	// /api-keys is best-effort: an account with zero API activity still
	// renders the page, but if the request fails we still emit the
	// subscription tile from /products. The api-* metrics simply omit.
	if api, err := fetchHTML(ctx, apiKeysURL, headers); err == nil {
		mergeAPIKeysHTML(api, &usage)
	}
	return snapshotToProvider(usage), nil
}

// allowanceTotals captures the seven count fields the Nous portal
// surfaces under /api-keys's `usageByKey.totals` and (per-source)
// under `usageByKey.totalsByAllowanceId[<id>]`. The unit semantics
// match the live page exactly: SpendCents stays in cents (rounded
// from the API's float dollars) while the rest are integer counts.
type allowanceTotals struct {
	SpendCents       float64
	Requests         int
	Tokens           int
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	// Found is true once we successfully parsed at least one field of
	// the totals object. Lets snapshotToProvider distinguish "no
	// activity yet" (Found=true, all zeros — legitimate metric) from
	// "couldn't parse" (Found=false — suppress the tile).
	Found bool
}

// usageSnapshot is the parsed Nous portal state.
type usageSnapshot struct {
	// SubBalanceCents is the current subscription credits remaining.
	SubBalanceCents float64
	// SubRolloverCents is the rolled-over subscription credits balance,
	// already included in SubBalanceCents but surfaced separately so
	// the caption can call it out.
	SubRolloverCents float64
	// SubMonthlyCents is the active tier's monthly credit grant
	// (e.g. $22.00 on Plus). Used as the meter denominator.
	SubMonthlyCents float64
	// SubRolloverCapCents is the maximum rollover the tier permits
	// (e.g. $10.00 on Plus). Surfaced in the caption when non-zero.
	SubRolloverCapCents float64
	// APIBalanceCents is the standalone API-credits balance, separate
	// from the subscription pool.
	APIBalanceCents float64

	// AllTotals is the page-level totals.* block from /api-keys —
	// summed across every allowance bucket including the unattributed
	// empty-string key. Use this for the "everything I've ever done"
	// metrics.
	AllTotals allowanceTotals
	// APITotals is the totalsByAllowanceId entry whose key matches the
	// API-credits allowance ID we discovered from the /api-keys page's
	// API Credits panel. Only populated when both the panel's
	// allowanceId AND its totals row were parseable.
	APITotals allowanceTotals
	// SubTotals is the remaining non-empty totalsByAllowanceId entry
	// (the subscription allowance) — discovered by elimination once
	// we know the API allowance ID. Reliable because Nous tracks at
	// most two non-empty allowances per account today.
	SubTotals allowanceTotals

	// Tier is the active subscription tier name ("Plus", "Free", ...).
	Tier string
	// RenewsAt is the subscription renewal timestamp (currentPeriodEnd).
	RenewsAt *time.Time
	// HasAPIData is set when /api-keys was successfully parsed; used to
	// gate emission of the api-* metrics (don't render $0 when we
	// don't actually know the value).
	HasAPIData bool
	UpdatedAt  time.Time
}

// snapshotFromHTML parses /products into the subscription portion of
// the snapshot. Quiet on missing fields so a Free-tier account (no
// active subscription) still produces a valid empty snapshot.
func snapshotFromHTML(html []byte, now time.Time) usageSnapshot {
	u := usageSnapshot{UpdatedAt: now}
	src := string(html)
	if m := reSubCreditsBalance.FindStringSubmatch(src); len(m) == 3 {
		u.SubBalanceCents = math.Round(parseFloat(m[1]) * 100)
		u.SubRolloverCents = math.Round(parseFloat(m[2]) * 100)
	}
	if m := reActiveSubMonthly.FindStringSubmatch(src); len(m) == 2 {
		u.SubMonthlyCents = math.Round(parseFloat(m[1]) * 100)
	}
	if m := reActiveSubRolloverCap.FindStringSubmatch(src); len(m) == 2 {
		u.SubRolloverCapCents = math.Round(parseFloat(m[1]) * 100)
	}
	if m := reActiveSubTier.FindStringSubmatch(src); len(m) == 2 {
		u.Tier = m[1]
	}
	if m := reActiveSubRenewsAt.FindStringSubmatch(src); len(m) == 2 {
		if t, err := time.Parse(time.RFC3339, m[1]); err == nil {
			u.RenewsAt = &t
		}
	}
	if m := reAPICreditsBalance.FindStringSubmatch(src); len(m) == 2 {
		u.APIBalanceCents = math.Round(parseFloat(m[1]) * 100)
	}
	return u
}

// mergeAPIKeysHTML extracts every field of the page-level totals plus
// the per-allowance breakdown from /api-keys's embedded usageByKey
// block. Mutates u in place. Per-field regexes anchored on
// \"totals\":\{ so a Nous schema change that reorders or inserts a
// new totals key only knocks out the affected metric, not every total
// at once.
//
// Allowance discovery: the API-Credits panel on the same page renders
// an `allowanceId:"<id>"` field — that's the API bucket. The other
// non-empty key in totalsByAllowanceId is the subscription bucket
// (Nous tracks at most two non-empty allowances per account today).
// Empty-string key is the unattributed bucket (panel-load activity);
// its totals are folded into AllTotals via the page-level totals.
func mergeAPIKeysHTML(html []byte, u *usageSnapshot) {
	src := string(html)
	parseInto(src, &u.AllTotals, allTotalsRegexes)
	if u.AllTotals.Found {
		u.HasAPIData = true
	}

	apiID := ""
	if m := reAPIPanelAllowanceID.FindStringSubmatch(src); len(m) == 2 {
		apiID = m[1]
	}
	subID := otherAllowanceID(src, apiID)

	if apiID != "" {
		parsePerAllowance(src, apiID, &u.APITotals)
	}
	if subID != "" {
		parsePerAllowance(src, subID, &u.SubTotals)
	}
	if u.APITotals.Found || u.SubTotals.Found {
		u.HasAPIData = true
	}
}

// parseInto runs each regex in the table over src and stores the
// captured value on totals. Each regex captures one numeric field;
// the field assignment is performed by the closure stored in the
// table value. Found is set true after any successful capture.
func parseInto(src string, t *allowanceTotals, table map[*regexp.Regexp]func(*allowanceTotals, string)) {
	for re, set := range table {
		if m := re.FindStringSubmatch(src); len(m) == 2 {
			set(t, m[1])
			t.Found = true
		}
	}
}

// parsePerAllowance digs out the seven totals for one specific
// allowance ID inside the totalsByAllowanceId block. Anchors the
// per-field match on \"<id>\":\{ so the inner [^{}]* keeps the
// capture inside that one allowance's flat object.
func parsePerAllowance(src, id string, t *allowanceTotals) {
	// Source JSON in the page is JS-string-escaped, so each `"` shows
	// as `\"`. Pattern needs `\\"` (regex-escaped backslash + literal
	// quote) to match — same convention used by every other regex in
	// this file.
	prefix := `\\"` + regexp.QuoteMeta(id) + `\\":\{`
	field := func(name string, intOnly bool) (string, bool) {
		re := regexp.MustCompile(prefix + `[^{}]*\\"` + name + `\\":(` +
			ifThen(intOnly, `\d+`, `[\d.]+`) + `)`)
		m := re.FindStringSubmatch(src)
		if len(m) != 2 {
			return "", false
		}
		return m[1], true
	}
	if v, ok := field("spend", false); ok {
		t.SpendCents = math.Round(parseFloat(v) * 100)
		t.Found = true
	}
	if v, ok := field("requests", true); ok {
		t.Requests, _ = strconv.Atoi(v)
		t.Found = true
	}
	if v, ok := field("tokens", true); ok {
		t.Tokens, _ = strconv.Atoi(v)
		t.Found = true
	}
	if v, ok := field("inputTokens", true); ok {
		t.InputTokens, _ = strconv.Atoi(v)
		t.Found = true
	}
	if v, ok := field("outputTokens", true); ok {
		t.OutputTokens, _ = strconv.Atoi(v)
		t.Found = true
	}
	if v, ok := field("cacheReadTokens", true); ok {
		t.CacheReadTokens, _ = strconv.Atoi(v)
		t.Found = true
	}
	if v, ok := field("cacheWriteTokens", true); ok {
		t.CacheWriteTokens, _ = strconv.Atoi(v)
		t.Found = true
	}
}

// otherAllowanceID scans totalsByAllowanceId for a non-empty key that
// isn't the API allowance — that's the subscription allowance ID. The
// regex captures every \"<id>\": that introduces an allowance entry
// inside totalsByAllowanceId; we then pick the first one that's
// non-empty and not equal to apiID. Empty-string key (unattributed
// bucket) is skipped.
func otherAllowanceID(src, apiID string) string {
	m := reTotalsByAllowanceBlock.FindStringSubmatch(src)
	if len(m) != 2 {
		return ""
	}
	for _, k := range reAllowanceKey.FindAllStringSubmatch(m[1], -1) {
		if len(k) != 2 {
			continue
		}
		id := k[1]
		if id == "" || id == apiID {
			continue
		}
		return id
	}
	return ""
}

// ifThen is a tiny ternary helper, used only for the int-vs-float
// digit class in parsePerAllowance.
func ifThen(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

// Pre-compiled regexes for the Nous portal's inline-JSON shape. The
// data is rendered as a JS-stringified payload inside `self.__next_f.
// push([1, "..."])` so all double quotes appear escaped (`\"`) and the
// regexes match the escaped form. Numeric capture groups are intentionally
// permissive (`[\d.]+`) because the API uses bare floats AND ints
// interchangeably (e.g. monthlyCredits=22 vs balance=21.998392).
//
// activeSubscription / totals anchors prevent collisions with the
// per-tier monthlyCredits in availableTiers and with per-key totals
// elsewhere on the page. Both objects are flat (no nested braces) so
// `[^{}]*` reliably keeps the match inside the right object. Each
// field gets its own regex so a future Nous schema change that
// reorders the keys or inserts a new one between two we read only
// knocks out the affected metric, not the whole tile or block.
var (
	reSubCreditsBalance    = regexp.MustCompile(`\\"subscriptionCredits\\":\{\\"balance\\":([\d.]+),\\"rolloverCredits\\":([\d.]+)\}`)
	reActiveSubMonthly     = regexp.MustCompile(`\\"activeSubscription\\":\{[^{}]*\\"monthlyCredits\\":([\d.]+)`)
	reActiveSubRolloverCap = regexp.MustCompile(`\\"activeSubscription\\":\{[^{}]*\\"maxRolloverCredits\\":([\d.]+)`)
	reActiveSubTier        = regexp.MustCompile(`\\"activeSubscription\\":\{[^{}]*\\"tier\\":\\"([^"\\]+)\\"`)
	reActiveSubRenewsAt    = regexp.MustCompile(`\\"activeSubscription\\":\{[^{}]*\\"currentPeriodEnd\\":\\"([^"\\]+)\\"`)
	reAPICreditsBalance    = regexp.MustCompile(`\\"apiCreditsBalance\\":([\d.]+)`)

	// Page-level totals (sum across every allowance bucket).
	reTotalsSpend            = regexp.MustCompile(`\\"totals\\":\{[^{}]*\\"spend\\":([\d.]+)`)
	reTotalsRequests         = regexp.MustCompile(`\\"totals\\":\{[^{}]*\\"requests\\":(\d+)`)
	reTotalsTokens           = regexp.MustCompile(`\\"totals\\":\{[^{}]*\\"tokens\\":(\d+)`)
	reTotalsInputTokens      = regexp.MustCompile(`\\"totals\\":\{[^{}]*\\"inputTokens\\":(\d+)`)
	reTotalsOutputTokens     = regexp.MustCompile(`\\"totals\\":\{[^{}]*\\"outputTokens\\":(\d+)`)
	reTotalsCacheReadTokens  = regexp.MustCompile(`\\"totals\\":\{[^{}]*\\"cacheReadTokens\\":(\d+)`)
	reTotalsCacheWriteTokens = regexp.MustCompile(`\\"totals\\":\{[^{}]*\\"cacheWriteTokens\\":(\d+)`)

	// allTotalsRegexes binds each page-level totals regex to a setter
	// closure on allowanceTotals — lets parseInto loop generically
	// over the seven fields without 7 hand-written branches.
	allTotalsRegexes = map[*regexp.Regexp]func(*allowanceTotals, string){
		reTotalsSpend:            func(t *allowanceTotals, s string) { t.SpendCents = math.Round(parseFloat(s) * 100) },
		reTotalsRequests:         func(t *allowanceTotals, s string) { t.Requests, _ = strconv.Atoi(s) },
		reTotalsTokens:           func(t *allowanceTotals, s string) { t.Tokens, _ = strconv.Atoi(s) },
		reTotalsInputTokens:      func(t *allowanceTotals, s string) { t.InputTokens, _ = strconv.Atoi(s) },
		reTotalsOutputTokens:     func(t *allowanceTotals, s string) { t.OutputTokens, _ = strconv.Atoi(s) },
		reTotalsCacheReadTokens:  func(t *allowanceTotals, s string) { t.CacheReadTokens, _ = strconv.Atoi(s) },
		reTotalsCacheWriteTokens: func(t *allowanceTotals, s string) { t.CacheWriteTokens, _ = strconv.Atoi(s) },
	}

	// reAPIPanelAllowanceID captures the API-Credits panel's
	// allowanceId — used to identify which totalsByAllowanceId bucket
	// belongs to the API allowance. Anchored after a typical RSC
	// neighbour (\"$L26\",null,{\"allowanceId\":\"...\") so we don't
	// accidentally pick up an allowanceId reference from elsewhere.
	reAPIPanelAllowanceID = regexp.MustCompile(`\\"allowanceId\\":\\"([^"\\]+)\\"`)

	// reTotalsByAllowanceBlock isolates the totalsByAllowanceId object
	// body so the per-key scan doesn't leak into surrounding RSC. The
	// inner [^{}]*(?:\{[^{}]*\}[^{}]*)* shape allows nested per-allowance
	// sub-objects without backtracking into a different block.
	reTotalsByAllowanceBlock = regexp.MustCompile(`\\"totalsByAllowanceId\\":\{((?:[^{}]|\{[^{}]*\})*)\}`)

	// reAllowanceKey matches each \"<id>\": label inside the
	// totalsByAllowanceId block. Captures the bare ID (unescaped
	// quotes already gone in the page payload).
	reAllowanceKey = regexp.MustCompile(`\\"([^"\\]*)\\":\{`)
)

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// snapshotToProvider maps the parsed state into the registry-shaped
// snapshot. Subscription metric is always emitted when we recognised
// any subscription state at all; API metrics only emit when /api-keys
// returned data so a Free-tier account with no API activity doesn't
// show three permanent $0/0 tiles.
func snapshotToProvider(u usageSnapshot) providers.Snapshot {
	now := u.UpdatedAt.Format(time.RFC3339)
	var metrics []providers.MetricValue

	if u.SubMonthlyCents > 0 {
		metrics = append(metrics, subCreditsMetric(u, now))
	}
	// API-balance only emits when there's actual API activity on the
	// account: a non-zero balance OR /api-keys returned recognisable
	// totals. Otherwise an account with no API platform usage would
	// render a permanently-critical-red "$0.00 Balance" tile (default
	// dollar threshold trips at <= 0 with NumericGoodWhen=high), which
	// is misleading — they don't have an API account, not an empty one.
	if u.APIBalanceCents > 0 || u.HasAPIData {
		metrics = append(metrics, apiBalanceMetric(u, now))
	}

	// All-time totals: 7 view × 3 source = 21 metrics, but only emit
	// the (source, view) pairs whose underlying allowanceTotals was
	// actually parsed (Found=true). That keeps a Sub-only account
	// from rendering a wall of $0/0 API tiles, and vice-versa.
	if u.HasAPIData {
		for _, src := range sourcesFor(&u) {
			t := src.Pick(&u)
			if !t.Found {
				continue
			}
			for _, v := range totalsViewSet {
				num, str, dollars := totalsValue(v.View, t)
				id := "hermes-" + v.View + "-" + src.Slug
				name := "Nous " + src.Name + " " + v.NameTag + " (all-time)"
				metrics = append(metrics, totalsMetric(id, src.Label, name, v.Caption, num, str, dollars, v.GoodHi, now))
			}
		}
		// v1 aliases: hermes-api-spend-total / hermes-api-requests-total
		// were originally the all-source spend/requests. Re-emit them
		// pointing at AllTotals so users with v1 bindings don't lose
		// their tiles when v2 ships.
		num, str, _ := totalsValue("spend", &u.AllTotals)
		metrics = append(metrics, totalsMetric(
			"hermes-api-spend-total", "ALL", "Nous all-source spend (all-time, v1 alias for hermes-spend-total)",
			"Spend", num, str, true, false, now))
		num, str, _ = totalsValue("requests", &u.AllTotals)
		metrics = append(metrics, totalsMetric(
			"hermes-api-requests-total", "ALL", "Nous all-source requests (all-time, v1 alias for hermes-requests-total)",
			"Requests", num, str, false, true, now))
	}

	return providers.Snapshot{
		ProviderID:   provID,
		ProviderName: providerName(u),
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// subCreditsMetric renders the subscription-credits tile. Denominator
// is monthlyCredits (the active tier's grant), NOT monthly+rolloverCap —
// when the user just renewed and hasn't spent, balance == monthly so
// ratio == 1.0 (full meter) and usedPct == 0 (no countdown). Including
// rolloverCap in the denominator made a fresh-renewal account look
// only ~69% full because the empty rollover bucket counted against it.
// Rollover credits beyond monthly overflow past 100% and clamp at the
// fill cap so the meter doesn't visually misrepresent the surplus.
//
// The renewal countdown is gated by ResetTimeWhenUsed so an idle
// account doesn't render a perpetual timer beside a full balance —
// once you've spent ~0.5% of monthly the countdown reappears as a
// "rolling over in X days" hint.
func subCreditsMetric(u usageSnapshot, now string) providers.MetricValue {
	balance := u.SubBalanceCents / 100
	monthly := u.SubMonthlyCents / 100
	ratio := math.Max(0, math.Min(1, balance/math.Max(monthly, 0.01)))
	usedPct := math.Max(0, math.Min(100, (1-ratio)*100))
	resetSecs := renewSeconds(providerutil.ResetTimeWhenUsed(usedPct, u.RenewsAt), u.UpdatedAt)
	m := providers.MetricValue{
		ID:              "hermes-sub-credits-remaining",
		Label:           "SUB",
		Name:            "Nous subscription credits remaining",
		Value:           fmt.Sprintf("$%.2f", balance),
		NumericValue:    &balance,
		NumericUnit:     "dollars",
		NumericGoodWhen: "high",
		Ratio:           &ratio,
		Direction:       "up",
		Caption:         "Credits",
		UpdatedAt:       now,
	}
	if resetSecs != nil {
		m.ResetInSeconds = resetSecs
	}
	return m
}

// apiBalanceMetric renders the standalone API credits tile. Always
// emitted — a $0 balance is itself useful info for users who haven't
// topped up the API platform.
func apiBalanceMetric(u usageSnapshot, now string) providers.MetricValue {
	balance := u.APIBalanceCents / 100
	return providers.MetricValue{
		ID:              "hermes-api-credits-remaining",
		Label:           "API",
		Name:            "Nous API credits balance",
		Value:           fmt.Sprintf("$%.2f", balance),
		NumericValue:    &balance,
		NumericUnit:     "dollars",
		NumericGoodWhen: "high",
		Caption:         "Balance",
		UpdatedAt:       now,
	}
}

// totalsViewSet enumerates the seven /api-keys totals fields plus the
// label / caption / unit / good-direction needed to emit each as a
// metric. The order is the order they appear in the dropdown.
var totalsViewSet = []struct {
	View    string // "spend" | "requests" | "tokens" | ...
	Caption string // tile subtitle
	NameTag string // human-readable phrase for the metric Name field
	Dollars bool   // dollars vs count rendering
	GoodHi  bool   // NumericGoodWhen = "high" vs "low"
}{
	{"spend", "Spend", "spend", true, false},
	{"requests", "Requests", "requests", false, true},
	{"tokens", "Tokens", "tokens", false, true},
	{"input-tokens", "In-tokens", "input tokens", false, true},
	{"output-tokens", "Out-tokens", "output tokens", false, true},
	{"cache-read-tokens", "Cache-R", "cache-read tokens", false, true},
	{"cache-write-tokens", "Cache-W", "cache-write tokens", false, true},
}

// totalsSourceSet maps the metric-ID source slug to the in-snapshot
// totals struct + button label. "total" intentionally renders as
// "ALL" so a row of Hermes tiles reads as labels at-a-glance.
type totalsSource struct {
	Slug   string // metric-ID suffix: "total" | "api" | "sub"
	Label  string // tile title: "ALL" | "API" | "SUB"
	Name   string // human prefix: "all-source" | "API allowance" | "Subscription allowance"
	Pick   func(*usageSnapshot) *allowanceTotals
}

func sourcesFor(u *usageSnapshot) []totalsSource {
	return []totalsSource{
		{"total", "ALL", "all-source", func(u *usageSnapshot) *allowanceTotals { return &u.AllTotals }},
		{"api", "API", "API allowance", func(u *usageSnapshot) *allowanceTotals { return &u.APITotals }},
		{"sub", "SUB", "Subscription allowance", func(u *usageSnapshot) *allowanceTotals { return &u.SubTotals }},
	}
}

// totalsValue extracts the (numeric, formatted-string) pair for one
// (view, totals) combination. Centralised so the emitter doesn't have
// to switch on view name 21 times.
func totalsValue(view string, t *allowanceTotals) (numeric float64, str string, dollars bool) {
	switch view {
	case "spend":
		v := t.SpendCents / 100
		return v, fmt.Sprintf("$%.2f", v), true
	case "requests":
		return float64(t.Requests), fmt.Sprintf("%d", t.Requests), false
	case "tokens":
		return float64(t.Tokens), fmt.Sprintf("%d", t.Tokens), false
	case "input-tokens":
		return float64(t.InputTokens), fmt.Sprintf("%d", t.InputTokens), false
	case "output-tokens":
		return float64(t.OutputTokens), fmt.Sprintf("%d", t.OutputTokens), false
	case "cache-read-tokens":
		return float64(t.CacheReadTokens), fmt.Sprintf("%d", t.CacheReadTokens), false
	case "cache-write-tokens":
		return float64(t.CacheWriteTokens), fmt.Sprintf("%d", t.CacheWriteTokens), false
	}
	return 0, "", false
}

// totalsMetric builds one all-time-totals tile. v1's two metric IDs
// (hermes-api-spend-total, hermes-api-requests-total) are now emitted
// as aliases pointing at AllTotals so existing button bindings keep
// rendering — see snapshotToProvider for where the aliases attach.
func totalsMetric(id, label, name, caption string, numeric float64, str string, dollars, goodHi bool, now string) providers.MetricValue {
	m := providers.MetricValue{
		ID:           id,
		Label:        label,
		Name:         name,
		Value:        str,
		NumericValue: &numeric,
		Caption:      caption,
		UpdatedAt:    now,
	}
	if dollars {
		m.NumericUnit = "dollars"
	} else {
		m.NumericUnit = "count"
	}
	if goodHi {
		m.NumericGoodWhen = "high"
	} else {
		m.NumericGoodWhen = "low"
	}
	return m
}

// renewSeconds returns seconds until renewAt, or nil when the date is
// missing or already past (so the renderer doesn't show a "reset in 0s"
// flash on an expired snapshot).
func renewSeconds(renewAt *time.Time, now time.Time) *float64 {
	if renewAt == nil {
		return nil
	}
	d := renewAt.Sub(now).Seconds()
	if d <= 0 {
		return nil
	}
	return &d
}

// providerName decorates the display name with the active tier when we
// know it. Free-tier or unknown accounts read as plain "Hermes".
func providerName(u usageSnapshot) string {
	if u.Tier == "" {
		return provName
	}
	return provName + " " + u.Tier
}

// fetchHTML wraps cookies.Fetch with HTTP-status checking so callers
// see a typed httputil.Error on non-2xx (mapHTTPError understands it).
func fetchHTML(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	resp, err := cookies.Fetch(ctx, cookies.Request{URL: url, Method: "GET", Headers: headers})
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, &httputil.Error{Status: resp.Status, StatusText: resp.StatusText, Body: string(resp.Body), URL: url}
	}
	return resp.Body, nil
}

// mapHTTPError converts a Fetch error into the most useful provider
// snapshot. 401/403 → stale cookie message; anything else → short
// "HTTP <code>" without leaking the response body.
func mapHTTPError(err error) providers.Snapshot {
	var httpErr *httputil.Error
	if !errors.As(err, &httpErr) {
		return errorSnapshot(err.Error())
	}
	if httpErr.Status == 401 || httpErr.Status == 403 {
		return errorSnapshot(cookieaux.StaleMessage("nousresearch.com"))
	}
	return errorSnapshot(fmt.Sprintf("Nous portal HTTP %d", httpErr.Status))
}

// errorSnapshot returns a setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   provID,
		ProviderName: provName,
		Source:       "cookie",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// init registers the Hermes provider with the package registry.
func init() {
	providers.Register(Provider{})
}

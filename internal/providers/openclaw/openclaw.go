// Package openclaw implements the OpenClaw self-hosted gateway
// dashboard provider (openclaw/openclaw on GitHub).
//
// OpenClaw is a self-hostable AI-gateway product that bridges chat
// channels (WhatsApp, Slack, Discord, …) to LLM-backed agents and
// tracks per-provider/per-model token + cost usage. The gateway
// listens on ws://127.0.0.1:18789 by default; users typically run
// it on their dev box or a Tailscale node, so the base URL is
// user-configurable in the PI per provider with per-button override.
//
// Auth: a shared "operator" gateway token, same scope the dashboard
// frontend uses. Pasted in PI / OPENCLAW_GATEWAY_TOKEN env. The
// gateway protocol is WebSocket JSON-RPC — no REST surface for these
// metrics. Fresh WS per Fetch() (typical poll interval is 15 min;
// connect overhead is negligible).
//
// Method called: usage.cost with { days: N } for the {1, 7, 30}-day
// windows we expose. Response shape is CostUsageSummary, with totals
// {input, output, cacheRead, cacheWrite, totalTokens, totalCost}.
//
// Source pointers (commit-pinned, openclaw/openclaw):
//
//	src/gateway/server-methods/usage.ts:391-433 — usage.cost handler
//	src/infra/session-cost-usage.types.ts:37-62 — CostUsageTotals
//	src/gateway/protocol/client-info.ts:3-19   — GATEWAY_CLIENT_IDS
//	ui/src/ui/gateway.ts:298-340               — WS protocol
//	ui/src/ui/gateway.ts:416-435               — connect params
//	docs/gateway/protocol.md                    — protocol overview
package openclaw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	providerID    = "openclaw"
	providerName  = "OpenClaw"
	defaultBase   = "ws://127.0.0.1:18789"
	connectMethod = "connect"
	usageMethod   = "usage.cost"
	dialTimeout   = 10 * time.Second
	requestTimeout = 15 * time.Second
)

// window is one of the time slices we emit metrics for.
type window struct {
	Days  int
	Slug  string
	Label string
}

// windows enumerated in dropdown order.
var windows = []window{
	{Days: 1, Slug: "daily", Label: "DAY"},
	{Days: 7, Slug: "weekly", Label: "WEEK"},
	{Days: 30, Slug: "monthly", Label: "MONTH"},
}

// Provider fetches OpenClaw gateway usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return providerID }

// Name returns the human-readable provider name.
func (Provider) Name() string { return providerName }

// BrandColor returns the meter-fill accent — red-500, matching the
// lobster mascot in OpenClaw's favicon (ui/public/favicon.svg).
func (Provider) BrandColor() string { return "#ef4444" }

// BrandBg returns a deep-red complement for the button background.
func (Provider) BrandBg() string { return "#1c0606" }

// MetricIDs enumerates every metric this provider can emit.
//
// Naming convention:
//
//	openclaw-<view>-<window>
//
// where <view> ∈ {input-tokens, output-tokens, cache-tokens,
// total-tokens, cost} and <window> ∈ {daily, weekly, monthly}.
func (Provider) MetricIDs() []string {
	views := []string{"input-tokens", "output-tokens", "cache-tokens", "total-tokens", "cost"}
	ids := make([]string, 0, len(views)*len(windows))
	for _, v := range views {
		for _, w := range windows {
			ids = append(ids, "openclaw-"+v+"-"+w.Slug)
		}
	}
	return ids
}

// Fetch returns the latest OpenClaw usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	base, err := resolveBase()
	if err != nil {
		return errorSnapshot(err.Error()), nil
	}
	token := resolveToken()
	if token == "" {
		return errorSnapshot("OpenClaw: paste the gateway operator token in the PI (or set OPENCLAW_GATEWAY_TOKEN)."), nil
	}

	conn, err := dialAndConnect(ctx, base, token)
	if err != nil {
		return errorSnapshot(short(err)), nil
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	now := time.Now().UTC().Format(time.RFC3339)
	var metrics []providers.MetricValue
	for _, w := range windows {
		summary, err := requestUsageCost(ctx, conn, w.Days)
		if err != nil {
			return errorSnapshot(fmt.Sprintf("OpenClaw usage.cost (days=%d) failed: %s", w.Days, short(err))), nil
		}
		metrics = append(metrics, windowMetrics(w, summary.Totals, now)...)
	}

	return providers.Snapshot{
		ProviderID:   providerID,
		ProviderName: providerName,
		Source:       "self-hosted",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// resolveBase returns the OpenClaw gateway WS URL: user setting first,
// then OPENCLAW_BASE_URL env, then the loopback default. Normalizes
// http(s) → ws(s) so users can paste the URL they see in their
// browser without thinking about scheme.
func resolveBase() (string, error) {
	pk := settings.ProviderKeysGet()
	raw := settings.ResolveEndpoint(pk.OpenClawBaseURL, defaultBase, "OPENCLAW_BASE_URL")
	if raw == "" {
		return "", errors.New("OpenClaw base URL is not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("OpenClaw base URL %q is not a valid URL: %w", raw, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// already correct
	default:
		return "", fmt.Errorf("OpenClaw base URL %q must use http(s) or ws(s) scheme", raw)
	}
	return u.String(), nil
}

// resolveToken returns the user-pasted operator gateway token from
// PI settings or the env fallback.
func resolveToken() string {
	pk := settings.ProviderKeysGet()
	return settings.ResolveAPIKey(pk.OpenClawToken, "OPENCLAW_GATEWAY_TOKEN", "OPENCLAW_TOKEN")
}

// --- WS protocol ---

// reqFrame is the outbound request envelope. Mirrors the frontend's
// frame: { type, id, method, params } — see ui/src/ui/gateway.ts:699.
type reqFrame struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// resFrame is the inbound response envelope.
type resFrame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload"`
	Error   *errorPayload   `json:"error,omitempty"`
}

// eventFrame is the inbound event envelope (we drop these on the
// floor — connect.challenge etc. — until/unless our connect frame
// fails for nonce-related reasons).
type eventFrame struct {
	Type    string          `json:"type"`
	Event   string          `json:"event,omitempty"`
	Seq     int             `json:"seq,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// errorPayload mirrors the server's error shape.
type errorPayload struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

// connectParams are the connect frame's params (token-only auth, no
// device identity — server skips nonce check when device is absent).
type connectParams struct {
	MinProtocol int                `json:"minProtocol"`
	MaxProtocol int                `json:"maxProtocol"`
	Client      connectClient      `json:"client"`
	Role        string             `json:"role"`
	Scopes      []string           `json:"scopes"`
	Caps        []string           `json:"caps"`
	Auth        connectAuthPayload `json:"auth"`
	UserAgent   string             `json:"userAgent"`
	Locale      string             `json:"locale"`
}

// connectClient identifies our process to the gateway. "gateway-client"
// is the closest fit in GATEWAY_CLIENT_IDS for an external programmatic
// consumer — we are not the dashboard SPA and not a CLI.
type connectClient struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Mode     string `json:"mode"`
}

// connectAuthPayload carries the operator token. deviceToken /
// password are unused.
type connectAuthPayload struct {
	Token string `json:"token"`
}

// CostUsageTotals mirrors the server's response shape — see
// src/infra/session-cost-usage.types.ts:37-50.
type CostUsageTotals struct {
	Input          float64 `json:"input"`
	Output         float64 `json:"output"`
	CacheRead      float64 `json:"cacheRead"`
	CacheWrite     float64 `json:"cacheWrite"`
	TotalTokens    float64 `json:"totalTokens"`
	TotalCost      float64 `json:"totalCost"`
	InputCost      float64 `json:"inputCost"`
	OutputCost     float64 `json:"outputCost"`
	CacheReadCost  float64 `json:"cacheReadCost"`
	CacheWriteCost float64 `json:"cacheWriteCost"`
}

// CostUsageSummary is the usage.cost response payload — see
// src/infra/session-cost-usage.types.ts:56-62.
type CostUsageSummary struct {
	UpdatedAt int64           `json:"updatedAt"`
	Days      int             `json:"days"`
	Totals    CostUsageTotals `json:"totals"`
}

// dialAndConnect opens the WS, sends the connect req, waits for the
// hello response. Returns a usable connection or an error describing
// the failure.
func dialAndConnect(ctx context.Context, base, token string) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	ws, _, err := websocket.Dial(dialCtx, base, nil)
	if err != nil {
		return nil, fmt.Errorf("OpenClaw: cannot dial gateway at %s — %w", base, err)
	}
	ws.SetReadLimit(8 * 1024 * 1024)

	connectID, err := newRequestID()
	if err != nil {
		ws.Close(websocket.StatusNormalClosure, "")
		return nil, err
	}
	frame := reqFrame{
		Type:   "req",
		ID:     connectID,
		Method: connectMethod,
		Params: connectParams{
			MinProtocol: 3,
			MaxProtocol: 3,
			Client: connectClient{
				ID:       "gateway-client",
				Version:  "usage-buttons",
				Platform: "stream-deck",
				Mode:     "probe",
			},
			Role:      "operator",
			Scopes:    []string{"operator.read"},
			Caps:      []string{},
			Auth:      connectAuthPayload{Token: token},
			UserAgent: "UsageButtons/Stream-Deck",
			Locale:    "en-US",
		},
	}
	if err := wsjson.Write(ctx, ws, frame); err != nil {
		ws.Close(websocket.StatusNormalClosure, "")
		return nil, fmt.Errorf("OpenClaw connect frame write failed: %w", err)
	}
	if err := awaitResponse(ctx, ws, connectID, nil); err != nil {
		ws.Close(websocket.StatusNormalClosure, "")
		if isAuthErr(err) {
			return nil, errors.New("OpenClaw rejected the gateway token. Check it in the PI.")
		}
		return nil, fmt.Errorf("OpenClaw connect failed: %w", err)
	}
	return ws, nil
}

// requestUsageCost sends one usage.cost request and waits for the
// matching response.
func requestUsageCost(ctx context.Context, ws *websocket.Conn, days int) (CostUsageSummary, error) {
	id, err := newRequestID()
	if err != nil {
		return CostUsageSummary{}, err
	}
	frame := reqFrame{
		Type:   "req",
		ID:     id,
		Method: usageMethod,
		Params: map[string]int{"days": days},
	}
	if err := wsjson.Write(ctx, ws, frame); err != nil {
		return CostUsageSummary{}, err
	}
	var summary CostUsageSummary
	if err := awaitResponse(ctx, ws, id, &summary); err != nil {
		return CostUsageSummary{}, err
	}
	return summary, nil
}

// awaitResponse blocks until a frame with the matching id arrives.
// Drops event frames and unmatched responses (defensive — we don't
// pipeline so the server only has our outstanding id, but the
// frontend pattern shows mid-stream events are normal). When dst is
// non-nil, the matching response payload is unmarshalled into it.
func awaitResponse(ctx context.Context, ws *websocket.Conn, id string, dst any) error {
	deadline := time.Now().Add(requestTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		readCtx, cancel := context.WithDeadline(ctx, deadline)
		_, raw, err := ws.Read(readCtx)
		cancel()
		if err != nil {
			return err
		}
		var typed struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &typed); err != nil {
			return fmt.Errorf("OpenClaw: malformed frame: %w", err)
		}
		switch typed.Type {
		case "event":
			// drop — events don't carry response data we want here
			continue
		case "res":
			var res resFrame
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("OpenClaw: malformed res frame: %w", err)
			}
			if res.ID != id {
				continue
			}
			if !res.OK {
				if res.Error != nil {
					return &gatewayError{Code: res.Error.Code, Message: res.Error.Message}
				}
				return errors.New("OpenClaw: request failed without error detail")
			}
			if dst != nil && len(res.Payload) > 0 {
				if err := json.Unmarshal(res.Payload, dst); err != nil {
					return fmt.Errorf("OpenClaw: malformed payload: %w", err)
				}
			}
			return nil
		default:
			// unknown frame type — skip
		}
	}
}

// gatewayError carries a server-reported error code + message so the
// caller can react (auth failures vs other errors).
type gatewayError struct {
	Code    string
	Message string
}

// Error returns the formatted gateway error string.
func (e *gatewayError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// isAuthErr reports whether err is an auth-related gateway error.
// Codes are documented loosely in the upstream code; we match on the
// common ones plus a fallback substring scan on the message.
func isAuthErr(err error) bool {
	var ge *gatewayError
	if errors.As(err, &ge) {
		switch ge.Code {
		case "AUTH_TOKEN_MISMATCH", "AUTH_REQUIRED", "FORBIDDEN", "UNAUTHORIZED":
			return true
		}
		lower := strings.ToLower(ge.Message)
		return strings.Contains(lower, "token") && (strings.Contains(lower, "invalid") || strings.Contains(lower, "rejected") || strings.Contains(lower, "missing"))
	}
	return false
}

// --- Metric construction ---

// windowMetrics builds the five metrics emitted for one time-window.
func windowMetrics(w window, t CostUsageTotals, now string) []providers.MetricValue {
	return []providers.MetricValue{
		tokenMetric("openclaw-input-tokens-"+w.Slug, w.Label, "Input", "OpenClaw input tokens ("+w.Slug+")", t.Input, now),
		tokenMetric("openclaw-output-tokens-"+w.Slug, w.Label, "Output", "OpenClaw output tokens ("+w.Slug+")", t.Output, now),
		tokenMetric("openclaw-cache-tokens-"+w.Slug, w.Label, "Cache", "OpenClaw cache-read tokens ("+w.Slug+")", t.CacheRead, now),
		tokenMetric("openclaw-total-tokens-"+w.Slug, w.Label, "Total", "OpenClaw total tokens ("+w.Slug+")", t.TotalTokens, now),
		costMetric("openclaw-cost-"+w.Slug, w.Label, "Cost", "OpenClaw total cost ("+w.Slug+")", t.TotalCost, now),
	}
}

// tokenMetric builds one count-style token tile.
func tokenMetric(id, label, caption, name string, count float64, now string) providers.MetricValue {
	rounded := math.Round(count)
	return providers.MetricValue{
		ID:              id,
		Label:           label,
		Name:            name,
		Value:           formatCount(int64(rounded)),
		NumericValue:    &rounded,
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

// --- Helpers ---

// newRequestID returns a hex-encoded random ID. JSON-RPC frames need
// only uniqueness within a connection — 16 random bytes is overkill
// but cheap.
func newRequestID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("OpenClaw: random ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// formatCount formats integer counts with k/M suffixes.
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

// short returns a compact log-safe error string for snapshots.
func short(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[:i]
	}
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// errorSnapshot returns a setup or auth failure snapshot with no
// metrics.
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

// init registers the OpenClaw provider with the package registry.
func init() {
	providers.Register(Provider{})
}

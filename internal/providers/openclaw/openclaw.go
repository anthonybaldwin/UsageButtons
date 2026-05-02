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

// BrandColor returns the meter-fill accent. Matches the lobster's
// brighter gradient stop in OpenClaw's favicon
// (ui/public/favicon.svg, #ff4d4d) — vibrant brand red.
func (Provider) BrandColor() string { return "#ff4d4d" }

// BrandBg returns a deep-red complement. Tuned warmer than the
// previous near-black so the empty-meter state reads as "OpenClaw
// red" instead of generic brown.
func (Provider) BrandBg() string { return "#330a0a" }

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

	identity, err := loadOrCreateIdentity()
	if err != nil {
		logf("identity load/create failed: %v", err)
		return errorSnapshot(fmt.Sprintf("OpenClaw: cannot read/write device keypair — %s", short(err))), nil
	}
	deviceToken, _, err := loadDeviceToken(base, identity.DeviceID)
	if err != nil {
		logf("device token load failed (continuing without): %v", err)
	}

	conn, err := dialAndConnect(ctx, base, token, identity, deviceToken)
	// dialAndConnect signals a stale cached deviceToken by returning
	// errStaleDeviceTokenCleared after wiping local state. Retry once
	// without the token so the bootstrap path runs this poll cycle and
	// the user sees the PAIR face immediately, instead of waiting up
	// to MinTTL for the next poll.
	if errors.Is(err, errStaleDeviceTokenCleared) {
		conn, err = dialAndConnect(ctx, base, token, identity, "")
	}
	if err != nil {
		var pp *pairingPendingError
		if errors.As(err, &pp) && pp.RequestID != "" {
			// Auto-approve our own pending pairing using the configured
			// shared operator token. The token already has admin/pairing
			// scope (the user pasted it in the PI), so we open a second
			// shared-token-only session — no device block, no signature
			// roundtrip — and call device.pair.approve. On success retry
			// the device-paired connect; the server now finds us in the
			// approved set and issues a deviceToken via hello-ok.
			//
			// Falls back to the PAIR face if the approve RPC errors —
			// that preserves the manual `openclaw devices approve …`
			// recovery path for setups where the PI token isn't
			// admin-scoped.
			logf("auto-approving self-pairing requestId=%s deviceId=%s", pp.RequestID, pp.DeviceID)
			if approveErr := autoApproveSelfPairing(ctx, base, token, pp.RequestID); approveErr != nil {
				logf("auto-approve failed (%v); surfacing PAIR face for manual approval", approveErr)
				return pairingPendingSnapshot(pp), nil
			}
			logf("auto-approve ok; retrying connect")
			conn, err = dialAndConnect(ctx, base, token, identity, "")
			if err != nil {
				if errors.As(err, &pp) {
					return pairingPendingSnapshot(pp), nil
				}
				return errorSnapshot(short(err)), nil
			}
		} else if errors.As(err, &pp) {
			return pairingPendingSnapshot(pp), nil
		} else {
			return errorSnapshot(short(err)), nil
		}
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

// connectParams are the connect frame's params. Mirrors the server's
// expected shape from connect handler at message-handler.ts. The
// device + auth.deviceToken pair is what makes remote (Tailscale)
// access work — without device pairing, the server strips scopes to
// the empty set on non-loopback connects.
type connectParams struct {
	MinProtocol int                `json:"minProtocol"`
	MaxProtocol int                `json:"maxProtocol"`
	Client      connectClient      `json:"client"`
	Role        string             `json:"role"`
	Scopes      []string           `json:"scopes"`
	Caps        []string           `json:"caps"`
	Auth        connectAuthPayload `json:"auth"`
	Device      *connectDevice     `json:"device,omitempty"`
	UserAgent   string             `json:"userAgent"`
	Locale      string             `json:"locale"`
}

// connectClient identifies our process to the gateway. We use
// "gateway-client" + "backend" so the same connect frame triggers
// the local-loopback bypass (shouldSkipLocalBackendSelfPairing in
// handshake-auth-helpers.ts:252-272) for users co-locating the
// plugin with their gateway, while remote users go through the
// device-pairing path.
type connectClient struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Mode     string `json:"mode"`
}

// connectAuthPayload carries the shared gateway token plus, after a
// successful pair, the per-device token returned by the server.
type connectAuthPayload struct {
	Token       string `json:"token,omitempty"`
	DeviceToken string `json:"deviceToken,omitempty"`
}

// connectDevice is the signed device block. Server validates id ==
// sha256(decode(publicKey)), nonce match, signature over the V3
// canonical payload, and signedAt within DEVICE_SIGNATURE_SKEW_MS of
// server clock. See message-handler.ts:744-789.
type connectDevice struct {
	ID        string `json:"id"`        // hex sha256(rawPublicKey)
	PublicKey string `json:"publicKey"` // base64url, raw 32-byte Ed25519
	Signature string `json:"signature"` // base64url Ed25519 sig of canonical payload
	SignedAt  int64  `json:"signedAt"`  // ms since unix epoch
	Nonce     string `json:"nonce"`     // from connect.challenge event
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

// helloResponse is the slice of hello-ok the connect call returns.
// The server's connect handler at src/gateway/server/ws-connection/
// message-handler.ts:1438-1467 sets payload.auth.scopes to the
// granted-scope list — which is empty when the default-deny strip
// path runs (no device identity, non-control-UI client). Logging
// this on every connect is how we confirm whether the user's setup
// hit the strip-on-no-device path.
type helloResponse struct {
	Auth struct {
		Role        string   `json:"role"`
		Scopes      []string `json:"scopes"`
		DeviceToken string   `json:"deviceToken,omitempty"`
	} `json:"auth"`
}

// requestedScopes is the operator scope set we ask the server for.
// Same set as the dashboard SPA's CONTROL_UI_OPERATOR_SCOPES at
// ui/src/ui/gateway.ts:152.
//
// Order is load-bearing: the gateway's signature verifier reconstructs
// the canonical V3 payload using `connectParams.scopes` AS-IS (no
// re-sort) — see resolveDeviceSignaturePayloadVersion at
// src/gateway/server/ws-connection/handshake-auth-helpers.ts:300-340.
// The literal slice we put on the wire must therefore equal the literal
// slice we hand to buildDeviceAuthPayloadV3. Keeping this constant
// alphabetically sorted is the simplest way to guarantee that
// invariant; TestRequestedScopesSorted enforces it.
var requestedScopes = []string{
	"operator.admin",
	"operator.approvals",
	"operator.pairing",
	"operator.read",
	"operator.write",
}

// errStaleDeviceTokenCleared is the sentinel dialAndConnect returns
// after wiping a locally cached deviceToken the gateway no longer
// honors. The caller (Fetch) retries the connect with an empty
// deviceToken on the same poll so the user sees the resulting state
// (PAIR face, success, or a real error) immediately instead of
// waiting for the next poll cycle.
var errStaleDeviceTokenCleared = errors.New("openclaw: cleared stale cached device token, retry without it")

// pairingPendingError signals the gateway returned NOT_PAIRED on
// connect because this device hasn't been approved yet. Carries the
// requestId the user passes to `openclaw devices approve`.
type pairingPendingError struct {
	RequestID string
	DeviceID  string
}

// Error returns the formatted pairing-pending error message.
func (e *pairingPendingError) Error() string {
	if e.RequestID == "" {
		return "OpenClaw: device pairing required — run `openclaw devices list` on the gateway host to find the requestId"
	}
	return fmt.Sprintf("OpenClaw: device pairing required — run `openclaw devices approve %s` on the gateway host", e.RequestID)
}

// pairingErrorDetails is the shape of error.details on a NOT_PAIRED
// response (see message-handler.ts:1083-1107 buildPairingConnectErrorDetails).
type pairingErrorDetails struct {
	RequestID string `json:"requestId"`
	DeviceID  string `json:"deviceId"`
	Reason    string `json:"reason"`
}

// challengePayload is the shape of the connect.challenge event body.
type challengePayload struct {
	Nonce string `json:"nonce"`
	TS    int64  `json:"ts"`
}

// awaitConnectChallenge reads frames until we see a connect.challenge
// event. The server sends this immediately on socket open; the client
// MUST wait for it before sending connect (the server's challenge-
// timeout watchdog rejects clients that fire connect prematurely).
// Returns the nonce we sign into the connect frame's device.signature.
func awaitConnectChallenge(ctx context.Context, ws *websocket.Conn) (string, error) {
	deadline := time.Now().Add(dialTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		readCtx, cancel := context.WithDeadline(ctx, deadline)
		_, raw, err := ws.Read(readCtx)
		cancel()
		if err != nil {
			return "", fmt.Errorf("waiting for connect.challenge: %w", err)
		}
		var ev eventFrame
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		if ev.Type != "event" || ev.Event != "connect.challenge" {
			continue
		}
		var payload challengePayload
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return "", fmt.Errorf("malformed connect.challenge payload: %w", err)
		}
		if payload.Nonce == "" {
			return "", errors.New("connect.challenge with empty nonce")
		}
		return payload.Nonce, nil
	}
}

// dialAndConnect opens the WS, completes the connect handshake
// (challenge → signed connect → hello-ok), and persists any deviceToken
// the server returns. Returns a usable connection on success.
//
// On NOT_PAIRED, returns a *pairingPendingError carrying the requestId
// so the caller can surface clear instructions to the user.
func dialAndConnect(ctx context.Context, base, sharedToken string, identity *deviceIdentity, deviceToken string) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	ws, _, err := websocket.Dial(dialCtx, base, nil)
	if err != nil {
		return nil, fmt.Errorf("OpenClaw: cannot dial gateway at %s — %w", base, err)
	}
	ws.SetReadLimit(8 * 1024 * 1024)

	nonce, err := awaitConnectChallenge(ctx, ws)
	if err != nil {
		ws.Close(websocket.StatusNormalClosure, "")
		return nil, fmt.Errorf("OpenClaw: %w", err)
	}
	logf("connect.challenge received: nonce-len=%d", len(nonce))

	const role = "operator"
	signedAt := time.Now().UnixMilli()
	// Mirror the server's resolveSignatureToken precedence
	// (handshake-auth-helpers.ts:274-281): canonical.token is
	// auth.token ?? auth.deviceToken ?? auth.bootstrapToken ?? "".
	// Using deviceToken alone here was the historical bug — the
	// server reconstructs canonical with the shared sharedToken when
	// auth.token is set, so signing over an empty deviceToken
	// produced "device signature invalid" on every connect that had
	// a configured operator token.
	signatureToken := sharedToken
	if signatureToken == "" {
		signatureToken = deviceToken
	}
	canonical := buildDeviceAuthPayloadV3(
		identity.DeviceID,
		"gateway-client",
		"backend",
		role,
		requestedScopes,
		signedAt,
		signatureToken,
		nonce,
		devicePlatform(),
		"", // deviceFamily — we have no equivalent; server treats empty as ok
	)
	signature := signCanonicalPayload(identity, canonical)

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
				Platform: devicePlatform(),
				Mode:     "backend",
			},
			Role:   role,
			Scopes: requestedScopes,
			Caps:   []string{},
			Auth: connectAuthPayload{
				Token:       sharedToken,
				DeviceToken: deviceToken,
			},
			Device: &connectDevice{
				ID:        identity.DeviceID,
				PublicKey: identity.PublicKey,
				Signature: signature,
				SignedAt:  signedAt,
				Nonce:     nonce,
			},
			UserAgent: "UsageButtons/Stream-Deck",
			Locale:    "en-US",
		},
	}
	if err := wsjson.Write(ctx, ws, frame); err != nil {
		ws.Close(websocket.StatusNormalClosure, "")
		return nil, fmt.Errorf("OpenClaw connect frame write failed: %w", err)
	}
	var hello helloResponse
	err = awaitResponse(ctx, ws, connectID, &hello)
	if err != nil {
		ws.Close(websocket.StatusNormalClosure, "")
		// NOT_PAIRED → pairing-pending: extract the requestId so we
		// can tell the user what to type at the gateway host.
		var ge *gatewayError
		if errors.As(err, &ge) && ge.Code == "NOT_PAIRED" {
			pp := &pairingPendingError{DeviceID: identity.DeviceID}
			if ge.Details != nil {
				var d pairingErrorDetails
				if jsonErr := json.Unmarshal(ge.Details, &d); jsonErr == nil {
					pp.RequestID = d.RequestID
					if d.DeviceID != "" {
						pp.DeviceID = d.DeviceID
					}
				}
			}
			logf("connect rejected NOT_PAIRED: requestId=%s deviceId=%s", pp.RequestID, pp.DeviceID)
			return nil, pp
		}
		// Stale-pairing recovery → wipe stored deviceToken and signal the
		// caller to retry the connect with no deviceToken so the bootstrap
		// path runs this poll cycle (NOT_PAIRED → PAIR face) instead of
		// the user waiting for the next poll. Two failure modes converge
		// here when the gateway has forgotten our pairing (e.g. admin ran
		// `openclaw devices clear`, gateway DB rebuilt):
		//   - Explicit DEVICE_TOKEN_MISMATCH / "device token" message
		//     when the server has the device but a different token.
		//   - INVALID_REQUEST "device signature invalid" when the server
		//     has no record of the device at all and reconstructs the
		//     canonical payload with an empty token, so the signature we
		//     made over the cached non-empty token can't verify.
		// Only act when we actually have a cached deviceToken to clear —
		// a fresh-bootstrap signature failure (no token sent) is a real
		// error worth surfacing, not stale state worth retrying past.
		if errors.As(err, &ge) && deviceToken != "" && isStaleCachedTokenRejection(ge) {
			_ = clearDeviceToken()
			logf("server rejected cached device token (code=%s msg=%q); cleared local store, retrying without it", ge.Code, ge.Message)
			return nil, errStaleDeviceTokenCleared
		}
		if isAuthErr(err) {
			return nil, errors.New("OpenClaw rejected the gateway token. Check it in the PI.")
		}
		return nil, fmt.Errorf("OpenClaw connect failed: %w", err)
	}
	logf("connect ok: role=%q granted-scopes=%v deviceToken-issued=%v deviceToken-cached=%v",
		hello.Auth.Role, hello.Auth.Scopes, hello.Auth.DeviceToken != "", deviceToken != "")
	if hello.Auth.DeviceToken != "" {
		if err := saveDeviceToken(base, identity.DeviceID, hello.Auth.DeviceToken, hello.Auth.Role, hello.Auth.Scopes); err != nil {
			logf("save device token: %v (continuing without persistence)", err)
		}
	}
	if !hasScope(hello.Auth.Scopes, "operator.read") {
		ws.Close(websocket.StatusNormalClosure, "")
		return nil, errors.New("OpenClaw connected but the granted session is missing scope operator.read. Approve the device pairing on the gateway host: `openclaw devices list` then `openclaw devices approve <requestId>`.")
	}
	return ws, nil
}

// hasScope reports whether scopes contains target (exact match).
func hasScope(scopes []string, target string) bool {
	for _, s := range scopes {
		if s == target {
			return true
		}
	}
	return false
}

// autoApproveSelfPairing approves a pending device-pairing requestId
// using the user's already-configured shared operator token, so the
// plugin can self-onboard without an SSH-and-CLI step. We open a
// second, transient WebSocket session that:
//
//   - presents only `auth.token` (no `device` block) — this is the
//     same posture the openclaw CLI uses; the server treats it as a
//     shared-token operator session (client.id = "cli") and grants
//     whatever scopes the token carries (admin → pairing included).
//
//   - calls the `device.pair.approve { requestId }` RPC documented at
//     docs/gateway/protocol.md and gated by operator.pairing in
//     src/gateway/method-scopes.ts.
//
//   - closes immediately after.
//
// Returns nil on a successful approve. Surfaces errors otherwise so
// the caller can fall back to the manual PAIR-face workflow when the
// configured token isn't pairing-capable (e.g. read-only operator
// token), instead of silently swallowing the rejection.
func autoApproveSelfPairing(ctx context.Context, base, sharedToken, requestID string) error {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	ws, _, err := websocket.Dial(dialCtx, base, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	ws.SetReadLimit(8 * 1024 * 1024)

	// Drain the connect.challenge — the server emits it eagerly even
	// when the eventual connect won't include a device block, and the
	// challenge-timeout watchdog fires if the client never reads it.
	if _, err := awaitConnectChallenge(ctx, ws); err != nil {
		return fmt.Errorf("challenge: %w", err)
	}

	connectID, err := newRequestID()
	if err != nil {
		return err
	}
	frame := reqFrame{
		Type:   "req",
		ID:     connectID,
		Method: connectMethod,
		Params: connectParams{
			MinProtocol: 3,
			MaxProtocol: 3,
			Client: connectClient{
				// "cli" / "cli" is the CLI tool's own posture — the
				// gateway recognizes it as a shared-token operator
				// session that doesn't require device pairing.
				ID:       "cli",
				Version:  "usage-buttons",
				Platform: devicePlatform(),
				Mode:     "cli",
			},
			Role:   "operator",
			Scopes: requestedScopes,
			Caps:   []string{},
			Auth: connectAuthPayload{
				Token: sharedToken,
			},
			// No Device block — that's what makes this a shared-token
			// session instead of a paired-device session.
			UserAgent: "UsageButtons/Stream-Deck-Auto-Approver",
			Locale:    "en-US",
		},
	}
	if err := wsjson.Write(ctx, ws, frame); err != nil {
		return fmt.Errorf("connect write: %w", err)
	}
	var hello helloResponse
	if err := awaitResponse(ctx, ws, connectID, &hello); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if !hasScope(hello.Auth.Scopes, "operator.pairing") && !hasScope(hello.Auth.Scopes, "operator.admin") {
		return fmt.Errorf("shared token lacks operator.pairing/admin scope; granted=%v", hello.Auth.Scopes)
	}

	approveID, err := newRequestID()
	if err != nil {
		return err
	}
	approveFrame := reqFrame{
		Type:   "req",
		ID:     approveID,
		Method: "device.pair.approve",
		Params: map[string]string{"requestId": requestID},
	}
	if err := wsjson.Write(ctx, ws, approveFrame); err != nil {
		return fmt.Errorf("approve write: %w", err)
	}
	var approveResp json.RawMessage
	if err := awaitResponse(ctx, ws, approveID, &approveResp); err != nil {
		return fmt.Errorf("approve rpc: %w", err)
	}
	return nil
}

// logf emits an [openclaw] log line via providers.LogSink when the
// plugin has wired one. No-op in tests.
func logf(format string, args ...any) {
	if providers.LogSink == nil {
		return
	}
	providers.LogSink(fmt.Sprintf("[openclaw] "+format, args...))
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
					return &gatewayError{Code: res.Error.Code, Message: res.Error.Message, Details: res.Error.Details}
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
// caller can react (auth failures vs other errors). Details captures
// the per-error structured payload — for NOT_PAIRED that contains the
// requestId the user passes to `openclaw devices approve`.
type gatewayError struct {
	Code    string
	Message string
	Details json.RawMessage
}

// Error returns the formatted gateway error string.
func (e *gatewayError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// isStaleCachedTokenRejection reports whether ge is the gateway's way
// of saying "your cached deviceToken doesn't match what I have on file
// for this device" — which the caller treats as a signal to wipe the
// local token and retry from the bootstrap path.
//
// Two server-side shapes converge here:
//   - DEVICE_TOKEN_MISMATCH (or any message mentioning "device token"):
//     the gateway has the device but a different token. Documented
//     rejection from message-handler.ts.
//   - INVALID_REQUEST + "signature": the gateway has no record of the
//     device at all (admin ran `openclaw devices clear`, gateway DB
//     rebuilt) and reconstructs the canonical payload with an empty
//     token, so our signature over the cached non-empty token can't
//     verify. Surfaces as the generic invalid-signature rejection.
func isStaleCachedTokenRejection(ge *gatewayError) bool {
	lower := strings.ToLower(ge.Message)
	if ge.Code == "DEVICE_TOKEN_MISMATCH" || strings.Contains(lower, "device token") {
		return true
	}
	return ge.Code == "INVALID_REQUEST" && strings.Contains(lower, "signature")
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

// pairingPendingSnapshot returns the snapshot we surface while the
// gateway is waiting for the user to approve our device pairing
// request. The Error message starts with "pairing required" so the
// plugin's renderer routes it to the PAIR face and so the PI's
// Copy-approve-command extractor (stat.html) can pull the requestId
// out via regex when it shows the prebuilt openclaw CLI command.
//
// Wording leads with the dashboard flow because that's the path that
// works without an SSH session for the common Tailscale-Serve setup
// (browser identity satisfies operator scope on the trusted-host
// path, no CLI invocation needed). The CLI fallback stays in the
// message for users running the gateway on a host they can shell
// into but no Tailscale identity grant — they'll see the same string
// in the PI plus the prebuilt copy-paste command from the Copy
// button.
func pairingPendingSnapshot(pp *pairingPendingError) providers.Snapshot {
	var msg string
	if pp.RequestID != "" {
		msg = fmt.Sprintf("pairing required — press the OpenClaw button to open the gateway dashboard and approve this request, or run on the gateway host: openclaw devices approve %s   (next plugin poll picks up metrics)", pp.RequestID)
	} else {
		msg = "pairing required — press the OpenClaw button to open the gateway dashboard and approve there, or run on the gateway host: openclaw devices list"
	}
	return providers.Snapshot{
		ProviderID:   providerID,
		ProviderName: providerName,
		Source:       "self-hosted",
		Metrics:      []providers.MetricValue{},
		Status:       "pairing-pending",
		Error:        msg,
	}
}

// init registers the OpenClaw provider with the package registry.
func init() {
	providers.Register(Provider{})
}

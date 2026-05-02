// Device pairing for the OpenClaw provider.
//
// The OpenClaw gateway's connect handler strips a session's scopes to
// the empty set whenever the client (a) isn't openclaw-control-ui, (b)
// isn't gateway-client+backend on a loopback connection, and (c)
// hasn't completed Ed25519 device pairing — see
// connect-policy.ts:88-106 in openclaw/openclaw. For Tailscale /
// remote-host gateway access, only path (c) works.
//
// This file implements the client side of pairing:
//
//  1. Generate an Ed25519 keypair on first connect, persist the raw
//     32-byte keys + derived deviceId to disk.
//  2. After the gateway emits its connect.challenge nonce, build the
//     V3 canonical signed payload (pipe-joined UTF-8 string of
//     [v3, deviceId, clientId, clientMode, role, scopes, signedAt,
//     token, nonce, platform, deviceFamily]; see
//     src/gateway/device-auth.ts:21-52) and sign it with the
//     private key. Send the {id, publicKey, signature, signedAt,
//     nonce} block on connect.params.device.
//  3. Server responds with NOT_PAIRED + details.requestId on the
//     first connect; the user runs `openclaw devices approve
//     <requestId>` on the gateway host to grant the requested scopes.
//  4. Next connect with the same signed payload returns hello-ok
//     containing auth.deviceToken (an opaque base64url 32-byte
//     string). We persist it and include it in the canonical signed
//     payload on subsequent connects so the signature binds to the
//     token.
package openclaw

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// deviceIdentity is the persisted Ed25519 keypair + derived ID. JSON
// shape mirrors the dashboard SPA's localStorage record (key
// "openclaw-device-identity-v1"), see ui/src/ui/device-identity.ts:18.
type deviceIdentity struct {
	Version    int    `json:"version"`
	DeviceID   string `json:"deviceId"`
	PublicKey  string `json:"publicKey"`  // base64url, raw 32 bytes
	PrivateKey string `json:"privateKey"` // base64url, ed25519 seed (32 bytes) — not the full 64-byte key
	CreatedAt  int64  `json:"createdAtMs"`

	priv ed25519.PrivateKey // populated on load
	pub  ed25519.PublicKey  // populated on load
}

// deviceTokenStore is the persisted deviceToken returned by the
// gateway after a successful pair. JSON shape mirrors the SPA's
// "openclaw.device.auth.v1" key — but flattened (we only ever pair
// one role per gateway URL).
type deviceTokenStore struct {
	Version     int    `json:"version"`
	DeviceID    string `json:"deviceId"`
	GatewayURL  string `json:"gatewayURL"` // store per-URL so URL changes invalidate the token
	Token       string `json:"token"`      // opaque base64url, 32 bytes
	Role        string `json:"role"`
	Scopes      []string `json:"scopes"`
	UpdatedAtMs int64  `json:"updatedAtMs"`
}

// stateMu guards on-disk identity + token reads / writes so a poll
// burst doesn't double-write the keypair file.
var stateMu sync.Mutex

// stateDir returns the per-user directory where the OpenClaw
// keypair and tokens live: $UserConfigDir/usage-buttons/openclaw/.
// Creates it on first call.
func stateDir() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	dir := filepath.Join(root, "usage-buttons", "openclaw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// identityPath / tokenPath return the per-user file locations.
func identityPath() (string, error) {
	d, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "device.json"), nil
}

func tokenPath() (string, error) {
	d, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "device-auth.json"), nil
}

// loadOrCreateIdentity returns the persisted Ed25519 keypair,
// generating + saving a fresh one on first call. The returned struct
// has priv/pub populated.
func loadOrCreateIdentity() (*deviceIdentity, error) {
	stateMu.Lock()
	defer stateMu.Unlock()

	path, err := identityPath()
	if err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(path); err == nil {
		var id deviceIdentity
		if err := json.Unmarshal(raw, &id); err != nil {
			return nil, fmt.Errorf("OpenClaw device identity at %s is corrupt — delete it and reconnect to repair: %w", path, err)
		}
		seed, err := base64.RawURLEncoding.DecodeString(id.PrivateKey)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("OpenClaw device identity at %s has an invalid private key — delete it and reconnect to repair", path)
		}
		id.priv = ed25519.NewKeyFromSeed(seed)
		id.pub = id.priv.Public().(ed25519.PublicKey)
		// Sanity: stored deviceId must still match sha256(pub).
		if want := deriveDeviceID(id.pub); want != id.DeviceID {
			return nil, fmt.Errorf("OpenClaw device identity at %s has mismatched deviceId — delete it and reconnect to repair", path)
		}
		return &id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// First run: generate fresh keypair.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate Ed25519 keypair: %w", err)
	}
	seed := priv.Seed()
	id := &deviceIdentity{
		Version:    1,
		DeviceID:   deriveDeviceID(pub),
		PublicKey:  base64.RawURLEncoding.EncodeToString(pub),
		PrivateKey: base64.RawURLEncoding.EncodeToString(seed),
		CreatedAt:  time.Now().UnixMilli(),
		priv:       priv,
		pub:        pub,
	}
	if err := writeFile0600(path, id); err != nil {
		return nil, err
	}
	return id, nil
}

// deriveDeviceID returns the sha256(rawPublicKey) hex digest, matching
// fingerprintPublicKey in src/infra/device-identity.ts:54-57.
func deriveDeviceID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:])
}

// loadDeviceToken reads the persisted gateway token for baseURL.
// Returns ("", nil) when no token has been minted yet (first pair
// hasn't happened). Returns the token shape only when it matches the
// passed deviceId — a stale token from a previous identity is
// treated as "no token".
func loadDeviceToken(baseURL, deviceID string) (string, []string, error) {
	stateMu.Lock()
	defer stateMu.Unlock()

	path, err := tokenPath()
	if err != nil {
		return "", nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", path, err)
	}
	var store deviceTokenStore
	if err := json.Unmarshal(raw, &store); err != nil {
		return "", nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if store.GatewayURL != baseURL || store.DeviceID != deviceID {
		// URL or identity changed — treat stored token as not ours.
		return "", nil, nil
	}
	return store.Token, store.Scopes, nil
}

// saveDeviceToken persists the deviceToken returned by hello-ok after
// a successful pair.
func saveDeviceToken(baseURL, deviceID, token, role string, scopes []string) error {
	stateMu.Lock()
	defer stateMu.Unlock()

	path, err := tokenPath()
	if err != nil {
		return err
	}
	store := deviceTokenStore{
		Version:     1,
		DeviceID:    deviceID,
		GatewayURL:  baseURL,
		Token:       token,
		Role:        role,
		Scopes:      append([]string(nil), scopes...),
		UpdatedAtMs: time.Now().UnixMilli(),
	}
	return writeFile0600(path, &store)
}

// clearDeviceToken removes the persisted token. Called when the
// gateway reports the token is bad (e.g. revoked) so the next connect
// re-pairs from scratch instead of looping with a dead token.
func clearDeviceToken() error {
	stateMu.Lock()
	defer stateMu.Unlock()

	path, err := tokenPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// writeFile0600 atomically writes value as JSON with mode 0600.
// Atomic via temp-file + rename so a crash mid-write doesn't leave a
// truncated keypair on disk.
func writeFile0600(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// buildDeviceAuthPayloadV3 returns the canonical V3 pipe-joined string
// signed in `device.signature`. Ports buildDeviceAuthPayloadV3 from
// src/gateway/device-auth.ts:36-52. Field order is fixed:
//
//	v3 | deviceId | clientId | clientMode | role | scopes | signedAt
//	   | token | nonce | platform | deviceFamily
//
// Empty fields render as the empty string. Scopes are comma-joined
// after sort to match normalizeDeviceAuthScopes' .toSorted() (see
// src/shared/device-auth.ts:20-37). signedAt is decimal ms epoch.
func buildDeviceAuthPayloadV3(deviceID, clientID, clientMode, role string, scopes []string, signedAtMs int64, token, nonce, platform, deviceFamily string) string {
	sortedScopes := append([]string(nil), scopes...)
	sort.Strings(sortedScopes)
	parts := []string{
		"v3",
		deviceID,
		clientID,
		clientMode,
		role,
		strings.Join(sortedScopes, ","),
		fmt.Sprintf("%d", signedAtMs),
		token,
		nonce,
		platform,
		deviceFamily,
	}
	return strings.Join(parts, "|")
}

// signCanonicalPayload returns the base64url Ed25519 signature over
// the UTF-8 bytes of the canonical payload string. Matches Node's
// crypto.sign(null, Buffer.from(payload, "utf8"), key) +
// base64url-encode flow at src/infra/device-identity.ts:150-154.
func signCanonicalPayload(id *deviceIdentity, payload string) string {
	sig := ed25519.Sign(id.priv, []byte(payload))
	return base64.RawURLEncoding.EncodeToString(sig)
}

// devicePlatform returns the platform string sent on the wire. Node's
// process.platform values: "win32" / "darwin" / "linux". Mirroring
// those so the gateway's normalization treats us consistently.
func devicePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	case "linux":
		return "linux"
	default:
		return runtime.GOOS
	}
}

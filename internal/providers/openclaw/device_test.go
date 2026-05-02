package openclaw

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// TestBuildDeviceAuthPayloadV3 locks in the canonical pipe-string
// format the server validates. Source-of-truth: src/gateway/device-auth.ts:36-52
// in openclaw/openclaw.
func TestBuildDeviceAuthPayloadV3(t *testing.T) {
	got := buildDeviceAuthPayloadV3(
		"ab12",          // deviceId
		"gateway-client", // clientId
		"backend",        // clientMode
		"operator",       // role
		[]string{"operator.read", "operator.admin"}, // scopes (intentionally unsorted)
		1714680000000,                              // signedAt
		"",                                         // token (empty for first connect)
		"5f3a",                                     // nonce
		"win32",                                    // platform
		"",                                         // deviceFamily
	)
	// Scopes must arrive sorted (server side does .toSorted() too —
	// see normalizeDeviceAuthScopes at src/shared/device-auth.ts:20-37).
	want := "v3|ab12|gateway-client|backend|operator|operator.admin,operator.read|1714680000000||5f3a|win32|"
	if got != want {
		t.Errorf("payload mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildDeviceAuthPayloadV3_WithToken(t *testing.T) {
	got := buildDeviceAuthPayloadV3(
		"ab12", "gateway-client", "backend", "operator",
		[]string{"operator.read"},
		1714680000000,
		"abc123", // token (post-pairing reconnect)
		"5f3a", "win32", "",
	)
	want := "v3|ab12|gateway-client|backend|operator|operator.read|1714680000000|abc123|5f3a|win32|"
	if got != want {
		t.Errorf("payload mismatch with token\n got: %q\nwant: %q", got, want)
	}
}

// TestDeriveDeviceID verifies sha256(rawPubKey).hex matches what
// fingerprintPublicKey produces server-side (src/infra/device-identity.ts:54-57).
func TestDeriveDeviceID(t *testing.T) {
	pub := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	for i := range pub {
		pub[i] = byte(i)
	}
	h := sha256.Sum256(pub)
	want := hex.EncodeToString(h[:])
	got := deriveDeviceID(pub)
	if got != want {
		t.Errorf("deriveDeviceID = %q, want %q", got, want)
	}
	if len(got) != 64 {
		t.Errorf("deriveDeviceID returned %d-char hex, want 64", len(got))
	}
}

// TestSignCanonicalPayload verifies the signature roundtrips through
// the public key — proves the Go ed25519.Sign output is the same bytes
// the server's verify() will accept (Node's crypto.verify(null, ...)).
func TestSignCanonicalPayload(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	id := &deviceIdentity{priv: priv, pub: pub}
	payload := "v3|ab12|gateway-client|backend|operator|operator.read|1714680000000||5f3a|win32|"

	sigB64 := signCanonicalPayload(id, payload)
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("base64url decode: %v", err)
	}
	if !ed25519.Verify(pub, []byte(payload), sig) {
		t.Errorf("signature did not verify against the public key — server would reject")
	}
	// Unpadded base64url: must not end with '='
	if strings.Contains(sigB64, "=") {
		t.Errorf("signature has padding %q; server expects RawURLEncoding (no padding)", sigB64)
	}
}

func TestDevicePlatform_KnownPlatforms(t *testing.T) {
	got := devicePlatform()
	allowed := map[string]bool{
		"win32": true, "darwin": true, "linux": true,
	}
	if !allowed[got] {
		// Other GOOS values pass through verbatim — server normalizes
		// any string, so this isn't a hard fail. Just informational.
		t.Logf("unusual platform string: %q (allowed-set: win32/darwin/linux)", got)
	}
}

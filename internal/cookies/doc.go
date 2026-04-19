// Package cookies gives cookie-gated providers (Claude web extras,
// Cursor, Ollama) a way to call their Cloudflare-protected APIs
// through Chrome's actual network stack — without ever handling the
// user's cookies in-process.
//
// A small MV3 companion extension exposes a single capability:
// "fetch(url) for a narrow allowlist of domains, with credentials".
// When the plugin wants, say, Claude's overage-spend-limit endpoint,
// it sends a fetch request to a tiny native host (cmd/native-host)
// via a local TCP loopback connection; the host relays the request to the
// extension over Chrome's native-messaging stdin/stdout protocol;
// the extension issues the fetch — with Chrome's real TLS
// fingerprint, real User-Agent, and the browser's own cookie jar —
// and ships the response body back.
//
// Why this shape (fetch-proxy) instead of exposing cookies directly:
//
//   - Cloudflare-proof: Chrome's TLS stack and UA. Go's net/http has
//     a distinct JA3 fingerprint; if CF ever starts fingerprinting
//     these endpoints, cookies-out would silently break.
//   - Smaller blast radius: plugin never sees cf_clearance /
//     sessionKey. Extension doesn't need the "cookies" permission at
//     all — just "nativeMessaging" plus narrow host_permissions.
//   - Web-Store-friendlier: the extension's purpose is "proxy for 3
//     specific APIs," not "exfiltrate cookies to a local binary."
//
// Three safety rails this package enforces:
//
//  1. Cookie-gated providers MUST check HostAvailable before firing
//     any request. When the extension isn't installed, Chrome isn't
//     running, or the extension hasn't handshaken this session, they
//     return a quiet "waiting on browser" snapshot instead of a
//     guaranteed-fail request.
//
//  2. HostAvailable means "the extension has said hello this
//     session," not merely "the IPC endpoint is listening."
//     Cold-start (Stream Deck launched before Chrome) stays in the
//     quiet state.
//
//  3. The allowlist of domains this package will fetch is hardcoded
//     in Go (see allowed.go) AND mirrored in the extension's service
//     worker. Adding a provider requires coordinated changes to both
//     plus a new extension release.
package cookies

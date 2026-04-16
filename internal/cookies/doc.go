// Package cookies gives cookie-gated providers (Claude web extras,
// Cursor, Ollama) access to cookies from a user's logged-in Chrome
// sessions, for endpoints that sit behind Cloudflare.
//
// The Go side never touches Chrome's on-disk cookie store. Modern
// Chrome (v127+) seals cookies with App-Bound Encryption, and the
// sealing COM service refuses any binary outside Chrome's install dir.
// Instead, a small MV3 companion extension calls chrome.cookies.getAll
// and ships results to a tiny native host (cmd/native-host) over
// Chrome's native-messaging stdin/stdout protocol. The plugin talks to
// that host over a local named pipe (Windows) or Unix socket (macOS).
//
// Three safety rails this package enforces:
//
//  1. Cookie-gated providers MUST check HostAvailable before firing
//     any request. When the extension isn't installed, Chrome isn't
//     running, or the extension hasn't handshaken this session,
//     providers return a "waiting on browser" snapshot instead of
//     producing guaranteed-fail requests that could anger Cloudflare.
//
//  2. HostAvailable means "the extension has said hello this session",
//     not merely "the IPC endpoint is listening." Cold-start (Stream
//     Deck launches before Chrome) therefore stays in the quiet
//     "waiting on browser" state.
//
//  3. The set of domains this package can query is hardcoded in the Go
//     allowlist (see allowed.go) AND mirrored in the extension's
//     service worker. Adding a provider requires coordinated changes
//     to both plus a new extension release.
package cookies

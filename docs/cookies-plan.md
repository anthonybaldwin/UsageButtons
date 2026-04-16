# Plan: Cookie access via Chrome extension + native messaging

## Context

usage-buttons needs browser cookies (`cf_clearance`, `sessionKey`, etc.)
from the user's logged-in browser sessions to fetch Cloudflare-protected
usage metrics (Claude web extras / Balance, Cursor, Ollama). Manual
paste from DevTools is fragile — Cloudflare cookies expire, users
forget to re-paste, and `cf_clearance` is HttpOnly so easy to miss.

**Direct DB decryption doesn't work on modern Chrome.** Chrome 127+
uses App-Bound Encryption (ABE): cookies with the `v20` prefix are
sealed by an `IElevator` COM service that verifies the caller's
executable path, refusing any binary outside Chrome's install dir.
SYSTEM-level DPAPI + path-gated COM = a user-mode third-party tool
cannot decrypt v20 cookies without reverse-engineering hacks (DLL
injection, handle stealing, binary-path spoofing) that are fragile,
AV-flagged, and morally dubious.

**Solution: native messaging + companion MV3 extension.** A small
Chrome extension uses `chrome.cookies.getAll()` (a legit, stable API)
and pipes the results to a native host binary via Chrome's native
messaging protocol. No decryption. No hacks. Works on every Chrome
version.

## Prior art (all incomplete / dead-ends)

| Library | License | v20 Chrome support | Notes |
|---------|---------|-------------------|-------|
| [SweetCookieKit](https://github.com/steipete/SweetCookieKit) | MIT | N/A (macOS only) | Swift. CodexBar uses this |
| [sweet-cookie](https://github.com/steipete/sweet-cookie) | **None** | Probably v10 only | TypeScript. Unlicensed — can't use |
| [chrome-cookie-extractor](https://github.com/pchuri/chrome-cookie-extractor) | MIT | No (Windows decrypt unimplemented) | TypeScript |

## Architecture

```
┌────────────────────┐          ┌─────────────────────────┐
│  Chrome Extension  │          │  Stream Deck Plugin     │
│  (MV3, SW stays    │          │  (usage-buttons.exe)    │
│   alive via port)  │          │                         │
│                    │          │  ┌───────────────────┐  │
│  chrome.cookies    │          │  │ internal/cookies  │  │
│  .getAll(domain)   │          │  │ client API:       │  │
│                    │          │  │ Header(Query)     │  │
└─────────┬──────────┘          │  └─────────┬─────────┘  │
          │ connectNative()                   │            │
          │ stdin/stdout                      │ named pipe │
          │ JSON framing                      │ (Win) /    │
          ▼                                   │ unix sock  │
┌────────────────────┐                        │ (mac)      │
│  Native Host       │◄───────────────────────┘            │
│  (cmd/native-host) │                                     │
│                    │          ┌──────────────────────────┘
│  Bridges extension │          │
│  ↔ plugin IPC      │          │
└────────────────────┘          │
```

**Data flow:**
1. Provider calls `cookies.Header(ctx, Query{Domain: "claude.ai"})`.
2. Client opens local IPC endpoint (named pipe / Unix socket) hosted
   by the native-host process.
3. Native host forwards query to the extension over stdin/stdout.
4. Extension calls `chrome.cookies.getAll({domain})`, returns list.
5. Native host forwards cookies back to the plugin via IPC.
6. Plugin builds `Cookie:` header and hits the target API.

The extension's service worker stays alive while its
`connectNative()` port is open, so the flow is always ready.

## Optional, not required

The extension is **optional**. The plugin works without it — users
just don't get cookie-gated metrics. Property inspector shows a prompt
to "Install the browser extension to enable Balance / web usage
metrics." Claude OAuth, Codex, Copilot, Warp, OpenRouter, z.ai,
Kimi K2 all keep working without cookies.

Cookie-gated providers (Claude web extras, Cursor, Ollama) call
`cookies.HostAvailable()` before issuing any request. If the host
isn't reachable, they return a snapshot with status "install the
extension" instead of firing requests that would fail (and possibly
anger providers).

## Components

### 1. Chrome extension (`chrome-extension/` in this repo)

- MV3 manifest
- Permissions: `cookies`, `nativeMessaging`
- Host permissions: `*://*.claude.ai/*`, `*://*.cursor.com/*`,
  `*://*.ollama.com/*` (minimum; expand as providers need)
- Service worker: maintains persistent `connectNative` port to the
  host, forwards cookie queries, subscribes to
  `chrome.cookies.onChanged` to push freshness signals

### 2. Native messaging host (`cmd/native-host/`)

- Tiny Go binary (`usagebuttons-native-host.exe` / `usagebuttons-native-host`)
- Ships alongside the main plugin binary
- Chrome spawns it on `connectNative`
- Reads/writes Chrome's native messaging frame format (4-byte LE
  length prefix + UTF-8 JSON) on stdin/stdout
- Hosts local IPC endpoint for the plugin to connect to

### 3. Native host manifest registration

Chrome needs a manifest JSON pointing at the host binary:
- **Windows:** registry key
  `HKCU\Software\Google\Chrome\NativeMessagingHosts\com.anthonybaldwin.usagebuttons`
  (default value = path to manifest JSON file)
- **macOS:** file at
  `~/Library/Application Support/Google/Chrome/NativeMessagingHosts/com.anthonybaldwin.usagebuttons.json`

Registration runs once per install; a property inspector button or
the plugin's first-run setup handles it.

### 4. `internal/cookies` Go package

Provides:
- **Host side:** message-loop helpers, frame codec, IPC server
  (`cookies.ServeNativeHost`)
- **Client side:** `cookies.Get(ctx, Query)`, `cookies.Header(ctx, Query)`
- **Installer helpers:** `cookies.RegisterHost(...)`,
  `cookies.UnregisterHost(...)` handle manifest + registry
- **Probes:** `cookies.HostAvailable(ctx)` for request-gating

### Package structure

```
usage-buttons/
  internal/cookies/
    cookies.go              — public API (Query, Cookie, Get, Header)
    host.go                 — native messaging host loop + frame codec
    host_windows.go         — named pipe IPC server
    host_darwin.go          — Unix socket IPC server
    client.go               — plugin-side client
    client_windows.go       — named pipe dial
    client_darwin.go        — Unix socket dial
    install.go              — manifest generation, RegisterHost/UnregisterHost
    install_windows.go      — registry + manifest file
    install_darwin.go       — manifest file
    probe.go                — HostAvailable()
  cmd/native-host/
    main.go                 — standalone host binary
  chrome-extension/
    manifest.json
    service-worker.js
    README.md
```

## Public API

```go
package cookies

type Query struct {
    Domain string   // required — e.g. "claude.ai"
    Names  []string // optional — filter by cookie name
}

type Cookie struct {
    Domain, Name, Value, Path string
    Expires                   time.Time
    Secure                    bool
}

// Get asks the extension (via native host) for cookies.
// Returns ErrHostUnavailable if the extension isn't installed /
// Chrome isn't running / the SW is asleep.
func Get(ctx context.Context, q Query) ([]Cookie, error)

// Header is a convenience returning "name1=v1; name2=v2".
func Header(ctx context.Context, q Query) (string, error)

// HostAvailable returns true if the native host IPC endpoint is
// reachable. Providers should gate requests on this.
func HostAvailable(ctx context.Context) bool

// ServeNativeHost runs the stdin/stdout message loop. Called by
// cmd/native-host/main.go only.
func ServeNativeHost(ctx context.Context) error

// RegisterHost writes the native messaging manifest (and on Windows,
// the registry key). allowedOrigins are the permitted extension IDs.
func RegisterHost(hostName, binaryPath string, allowedOrigins []string) error

func UnregisterHost(hostName string) error
```

## Install UX (property inspector)

1. Plugin ships with `usagebuttons-native-host{.exe}` alongside the
   main binary.
2. Property inspector "Enable browser cookies" section with:
   - Status: "extension not installed" / "connected" / "Chrome not running"
   - "Install extension" button → opens Chrome Web Store page (or
     sideload instructions during dev)
   - "Register native host" button → calls `RegisterHost`
3. On first extension connect, extension prompts user to reload Chrome
   if needed and verifies the connection.
4. Cookie-gated provider metrics display a small badge/tooltip saying
   "requires extension" when `HostAvailable` is false.

## Provider-side integration

Wire `cookies.HostAvailable` + `cookies.Header` into the three
cookie-dependent providers:

- `internal/providers/claude/claude.go` — `fetchWebExtras()` already
  guards on empty manual cookie; extend the guard to also try
  `cookies.Header(ctx, Query{Domain: "claude.ai"})` if manual isn't
  set. Full header (including `cf_clearance`) → Cloudflare happy.
- `internal/providers/cursor/cursor.go` — same pattern
- `internal/providers/ollama/ollama.go` — same pattern

Keep manual paste as a fallback for users who don't want the extension.

## Verification

1. `go build ./...` on Windows and macOS
2. `go test ./internal/cookies/...` — unit tests for frame codec,
   manifest generation, query marshaling
3. Manual E2E test:
   - Build the native host binary
   - Register the manifest
   - Load the extension unpacked in Chrome
   - Run the plugin
   - Verify Balance metric loads without 403

## Out of scope (for now)

- Firefox (different extension API + native messaging path — add
  later as a separate extension)
- Safari (extension model differs — later)
- Edge/Brave (Chromium extensions generally work in both; may need
  re-publishing to Edge Add-ons store)
- Direct DB decryption (dropped — ABE makes it a dead end on modern
  Chrome)

## Sequencing (rough order of implementation)

1. `internal/cookies` package skeleton — types + errors + frame codec
2. `cmd/native-host/main.go` stub that echoes messages (smoke test
   the stdin/stdout wiring without extension)
3. Manifest registration helpers + one-off CLI command to install
4. Extension MVP: MV3 manifest + SW that connects, forwards `cookies.getAll`
5. End-to-end smoke test: Go client → host → extension → Chrome → back
6. IPC server (named pipe / Unix socket) so plugin can talk to host
7. Wire `HostAvailable` + `Header` into Claude provider first
8. Property inspector UI for install/status
9. Cursor + Ollama providers
10. Publish extension to Chrome Web Store (unlisted)

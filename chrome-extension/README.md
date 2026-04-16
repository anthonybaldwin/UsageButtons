# Usage Buttons Helper (Chrome extension)

Companion Chrome extension for the
[Usage Buttons Stream Deck plugin](https://github.com/anthonybaldwin/usage-buttons).
Proxies a narrow allowlist of AI usage-monitoring APIs
(`claude.ai`, `cursor.com`, `ollama.com`) so the plugin can read your
usage stats using your real logged-in browser session.

**Cookies never leave the browser.** The extension issues
`fetch(url, { credentials: "include" })` — Chrome itself attaches the
user's cookies, the plugin only sees the API response bodies.

## Why it exists

Cookie-gated usage APIs (Claude's web extras, Cursor, Ollama) sit
behind Cloudflare. Routing through Chrome means:

- Real Chrome TLS fingerprint + User-Agent. No JA3 surprises.
- `cf_clearance` and session cookies stay in the browser's cookie jar
  — never serialized, never handed to a local binary.
- No `cookies` permission needed. The extension holds only
  `nativeMessaging` + narrow `host_permissions` for the three
  allowlisted domains.

## What it does and doesn't

| Does | Doesn't |
|---|---|
| Fetch `claude.ai`, `cursor.com`, `ollama.com` on behalf of the plugin | Touch any other site |
| Use your existing browser session (`credentials: "include"`) | Read cookies directly |
| Mirror the plugin's allowlist — refuse off-list URLs in the service worker | Let the plugin widen it at runtime |
| Hold a persistent native-messaging port so the plugin can probe liveness | Run unless Chrome is open |

## Install

### From a GitHub Release (simplest)

1. Grab `UsageButtons-Helper-unpacked.zip` from the
   [latest release](https://github.com/anthonybaldwin/usage-buttons/releases)
   and unzip it anywhere.
2. Open `chrome://extensions`, toggle **Developer mode** on
   (top-right).
3. Click **Load unpacked** and pick the unzipped folder.

That's it. The Stream Deck plugin auto-registers the native-messaging
bridge on launch — the extension ID is deterministic thanks to the
pinned public key in `manifest.json`, so no ID paste, no admin
prompt, no follow-up clicks.

### From source

`chrome://extensions` → **Load unpacked** → pick this
`chrome-extension/` directory.

### Chrome Web Store

Not yet published. Because the extension's ID is pinned by the `key`
field in `manifest.json`, publishing later won't break existing
installs — the ID stays the same.

## Why not a `.crx`?

Chrome blocks drag-and-drop `.crx` installs from anywhere except the
Chrome Web Store (since 2019). `.zip` + **Load unpacked** is the only
consumer path until this is published to the Web Store.

## Supported browsers

Any Chromium-based browser with standard native messaging and MV3
fetch: Chrome (stable/beta/Canary), Microsoft Edge, Brave, Chromium.
The plugin installs the native-messaging manifest for every browser
on the machine.

A Firefox port is on the roadmap (same JS, slightly different
manifest + `browser.*` shim). Safari is out of scope — its extension
model is Swift/Xcode-based with a distinct native-messaging story.

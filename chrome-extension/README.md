# Usage Buttons cookie bridge (Chrome extension)

Companion Chrome extension for the
[Usage Buttons Stream Deck plugin](https://github.com/anthonybaldwin/usage-buttons).
Proxies a narrow allowlist of AI usage-monitoring APIs
(`claude.ai`, `cursor.com`, `ollama.com`) to the plugin over Chrome's
native-messaging protocol.

**Cookies never leave the browser.** The extension uses
`fetch(url, { credentials: "include" })` — Chrome itself applies the
user's cookies, the plugin only sees the API response bodies.

## Why it exists

Cookie-gated usage APIs (Claude's web extras, Cursor, Ollama) sit
behind Cloudflare. Going through Chrome means:

- Real Chrome TLS fingerprint + User-Agent. Any future Cloudflare
  fingerprint tightening doesn't break the plugin.
- `cf_clearance` and `sessionKey` stay in the browser's cookie jar —
  never serialized, never handed to a local binary.
- No "cookies" permission needed. The extension only has
  `nativeMessaging` + narrow `host_permissions` for the three
  allowlisted domains.

## What it does and doesn't

| Does | Doesn't |
|---|---|
| Fetches `claude.ai`, `cursor.com`, `ollama.com` on behalf of the plugin | Touch any other site |
| Uses your existing browser session (credentials: "include") | Read cookies directly |
| Mirrors the plugin's allowlist — refuses off-list URLs in the service worker | Allow the plugin to widen it at runtime |
| Runs a persistent native-messaging port so the plugin can probe liveness | Run unless you opened Chrome |

## Install (developer sideload)

1. In the plugin's property inspector → **Plugin settings** tab →
   **Browser cookies extension** section, paste the extension ID (see
   step 3 below) and click **Register native host**. This writes the
   native-messaging manifest Chrome needs to launch the bridge.
2. Open `chrome://extensions` → toggle **Developer mode** (top-right).
3. Click **Load unpacked** → select this `chrome-extension/`
   directory. Copy the ID Chrome assigns — it's the 32-character
   string under the extension's card.
4. Paste that ID into the plugin's **Extension ID** field and click
   **Register** again if you hadn't yet.

The plugin's status indicator flips to "Extension connected" within a
second of Chrome launching. Cookie-gated metrics then go live.

## Install (Chrome Web Store)

TBD — a published version will have a fixed extension ID, so the
registration step becomes automatic on first install.

## Supported browsers

Any Chromium-based browser with standard native messaging and MV3
fetch: Chrome (stable/beta/Canary), Microsoft Edge, Brave, Chromium.
The plugin installs the native-messaging manifest for all of them.

A Firefox port is on the roadmap (same JS, slightly different manifest
+ `browser.*` API shim). Safari is out of scope — its extension model
is Swift/Xcode-based with a distinct native-messaging story.

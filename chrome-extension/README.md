# Usage Buttons cookie bridge (Chrome extension)

Companion Chrome extension for the
[Usage Buttons Stream Deck plugin](https://github.com/anthonybaldwin/usage-buttons).
Reads cookies for a **narrow allowlist of AI usage-monitoring sites**
(Claude, Cursor, Ollama) from your logged-in browser sessions and
forwards them to the plugin over Chrome's native-messaging protocol.

## What it does

- Watches only: `claude.ai`, `cursor.com`, `ollama.com`. No other
  sites are touched, and the allowlist is hardcoded in the service
  worker — not something the plugin can override at runtime.
- Replies to cookie queries from the plugin's native host with the
  current cookie values plus `navigator.userAgent`. Cloudflare's
  `cf_clearance` is UA-bound, so the plugin matches it when it sends
  requests.
- Never stores cookies anywhere. Every query re-reads Chrome's live
  cookie jar.
- Uses Chrome's official `chrome.cookies` API — no DPAPI, no COM
  hacks, no DB decryption. Works on every Chrome build.

## Why it exists

Modern Chrome (v127+) seals cookies with App-Bound Encryption; third-
party tools cannot legitimately decrypt them. A companion extension
using `chrome.cookies.getAll` is the only path that's stable, safe,
and Web-Store-friendly.

## Install (developer sideload)

1. Run the plugin's **Register native host** action from the property
   inspector — it installs the native-messaging manifest so Chrome
   knows how to spawn the bridge.
2. Open `chrome://extensions` and enable **Developer mode**
   (top-right).
3. Click **Load unpacked** and select this `chrome-extension/`
   directory.
4. Copy the extension ID Chrome assigns (shown on the extension's
   card) and paste it into the plugin's property inspector. That
   updates `allowed_origins` in the native-messaging manifest so
   Chrome will permit the connection.

Chrome auto-reconnects the native host on every browser launch. The
plugin detects the handshake and unlocks cookie-gated metrics; until
then, cookie-gated buttons stay in a quiet "waiting on browser" state
and never fire requests.

## Install (Chrome Web Store)

TBD — a published version will have a fixed extension ID, so the
`allowed_origins` step above becomes automatic on first install.

## Supported browsers

Any Chromium-based browser that honors standard native messaging and
ships the `chrome.cookies` API: Chrome (stable/beta/Canary),
Microsoft Edge, Brave, and Chromium. The plugin installs the
native-messaging manifest for all of them.

// Usage Buttons Helper — MV3 service worker.
//
// Talks to the Usage Buttons native-messaging host
// (io.github.anthonybaldwin.usagebuttons) over a persistent port. The
// native host bridges to the Stream Deck plugin via a local socket.
//
// Design: this extension does NOT expose cookies. It proxies fetch()
// for a hardcoded allowlist of origins, using credentials:"include"
// so Chrome itself applies the user's cookies. The plugin never sees
// cf_clearance, sessionKey, etc. Benefits:
//   - Chrome's real TLS fingerprint + User-Agent + cookie jar.
//   - Smaller permission surface (no "cookies" permission needed).
//   - Web-Store-friendlier: purpose is "proxy for allowlisted APIs" not
//     "exfiltrate cookies to a local binary."
//
// ALLOWED mirrors Go's cookies.Allowed. Adding a provider requires
// updating BOTH this list AND manifest.json host_permissions AND the
// Go allowlist, plus shipping a new extension release.

const HOST_NAME = "io.github.anthonybaldwin.usagebuttons";

const ALLOWED = [
  "abacus.ai",
  "alibabacloud.com",
  "aliyun.com",
  "claude.ai",
  "cursor.com",
  "factory.ai",
  "ollama.com",
  "chatgpt.com",
  "augmentcode.com",
  "ampcode.com",
  "perplexity.ai",
  "grok.com",
  "nousresearch.com",
  "opencode.ai",
  "kimi.com",
  "minimax.io",
  "minimaxi.com",
  "mistral.ai",
  "deepseek.com",
];

// Static x-app-version header expected by platform.deepseek.com's
// internal API. Captured from the live web client; if DeepSeek bumps
// this, the platform endpoints will start returning 4xxxx — update
// here and ship a new extension release.
const DEEPSEEK_PLATFORM_APP_VERSION = "20240425.0";

function originAllowed(rawURL) {
  let u;
  try {
    u = new URL(rawURL);
  } catch {
    return false;
  }
  if (u.protocol !== "https:") return false;
  const host = u.hostname.toLowerCase();
  return ALLOWED.some((a) => host === a || host.endsWith("." + a));
}

let port = null;
let isConnecting = false;
let lastConnectAt = 0;
let reconnectDelay = 1000;
const RECONNECT_MAX = 30000;
const MIN_CONNECT_INTERVAL_MS = 1000;
const HEARTBEAT_ALARM = "ub-heartbeat";

function connect() {
  // Single-flight: never have more than one connectNative pending.
  // Multiple events (alarm + setTimeout + onInstalled + module load) can
  // race; without this every race would spawn its own native-host process
  // before the first had a chance to settle.
  if (port || isConnecting) return;

  // Rate limit: cap respawns at one per second, even when the host keeps
  // exiting immediately. Stops the reconnect storm we saw when something
  // upstream was killing the port within 300ms of every spawn.
  const now = Date.now();
  const sinceLast = now - lastConnectAt;
  if (sinceLast < MIN_CONNECT_INTERVAL_MS) {
    setTimeout(connect, MIN_CONNECT_INTERVAL_MS - sinceLast);
    return;
  }
  lastConnectAt = now;
  isConnecting = true;

  let p;
  try {
    p = chrome.runtime.connectNative(HOST_NAME);
  } catch (e) {
    isConnecting = false;
    console.error("[UsageButtons] connectNative threw:", e);
    scheduleReconnect();
    return;
  }
  port = p;
  port.onMessage.addListener(handleMessage);
  port.onDisconnect.addListener(() => {
    const err = chrome.runtime.lastError;
    if (err) {
      console.warn("[UsageButtons] port disconnected:", err.message);
    }
    port = null;
    isConnecting = false;
    scheduleReconnect();
  });
  try {
    port.postMessage({
      kind: "ready",
      userAgent: navigator.userAgent,
      version: chrome.runtime.getManifest().version,
      allowedHosts: ALLOWED,
    });
    reconnectDelay = 1000;
  } catch (e) {
    console.warn("[UsageButtons] failed to send ready:", e);
  }
  isConnecting = false;
}

function scheduleReconnect() {
  // Fast path while the SW is still alive — quick recovery for transient drops.
  setTimeout(() => { if (!port) connect(); }, reconnectDelay);
  reconnectDelay = Math.min(reconnectDelay * 2, RECONNECT_MAX);
}

// Heartbeat alarm — survives SW suspension. After a system sleep or any other
// event that unloads the SW, pending setTimeouts are lost; this alarm wakes
// the SW periodically so connect() runs and the native host respawns.
// Production minimum is 30s, so 1 minute is the practical floor here.
chrome.alarms.create(HEARTBEAT_ALARM, { periodInMinutes: 1 });
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === HEARTBEAT_ALARM && !port) {
    connect();
  }
});

// Re-attempt on browser launch so the connection comes up before the user
// opens the inspector for the first time after Chrome cold-starts.
chrome.runtime.onStartup.addListener(() => { if (!port) connect(); });
chrome.runtime.onInstalled.addListener(() => { if (!port) connect(); });

function safeSend(obj) {
  try {
    port?.postMessage(obj);
  } catch (e) {
    console.warn("[UsageButtons] postMessage failed:", e);
  }
}

async function handleMessage(msg) {
  if (!msg || typeof msg !== "object" || !msg.kind) return;
  switch (msg.kind) {
    case "ping":
      safeSend({ id: msg.id, kind: "pong", userAgent: navigator.userAgent });
      return;
    case "fetch":
      await handleFetch(msg);
      return;
    case "reprime":
      await handleReprime(msg);
      return;
    default:
      console.warn("[UsageButtons] unknown message kind:", msg.kind);
  }
}

function toBase64(arrayBuffer) {
  const bytes = new Uint8Array(arrayBuffer);
  let bin = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    bin += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk));
  }
  return btoa(bin);
}

function fromBase64(b64) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

// readDeepSeekPlatformToken reads localStorage["userToken"] from any
// open platform.deepseek.com tab and returns the inner JWT-ish string,
// or null when no tab is open / no token is stored. The page persists
// the token via an appKit wrapper:
//
//   localStorage["userToken"] = '{"value":"<TOKEN>","__version":"0"}'
//
// If the user is signed out (or has no platform tab open), we can't
// read the token — the Go side then falls back to the public /user/balance
// API for the basic balance metric.
async function readDeepSeekPlatformToken() {
  let tabs;
  try {
    tabs = await chrome.tabs.query({ url: "*://platform.deepseek.com/*" });
  } catch (_e) {
    return null;
  }
  if (!tabs || tabs.length === 0) return null;
  for (const t of tabs) {
    try {
      const results = await chrome.scripting.executeScript({
        target: { tabId: t.id },
        // Runs in MAIN world to reach window.localStorage that the
        // platform's bundle has populated. Returns the raw JSON value
        // so we can parse it on this side.
        world: "MAIN",
        func: () => localStorage.getItem("userToken") || "",
      });
      const raw = results && results[0] && results[0].result;
      if (!raw) continue;
      try {
        const parsed = JSON.parse(raw);
        if (parsed && typeof parsed.value === "string" && parsed.value.length > 0) {
          return parsed.value;
        }
      } catch (_e) {
        // Older builds may store the token raw, not JSON-wrapped.
        if (typeof raw === "string" && raw.length > 0) return raw;
      }
    } catch (_e) {
      // executeScript can fail if the tab is loading or in a special state;
      // try the next one.
      continue;
    }
  }
  return null;
}

// readKimiAccessToken reads localStorage["access_token"] from any open
// kimi.com tab. Kimi migrated off cookie-based auth — the kimi-auth
// JWT cookie is no longer set, and apiv2 endpoints reject cookie-only
// requests with `REASON_INVALID_AUTH_TOKEN`. The session bearer token
// now lives in localStorage and the page's own client passes it via
// `Authorization: Bearer <tok>` on every API call.
//
// Returns null when no kimi.com tab is open or the user is signed out;
// the Go side then falls back to OAuth credentials placed by the
// `kimi login` CLI.
async function readKimiAccessToken() {
  let tabs;
  try {
    tabs = await chrome.tabs.query({ url: "*://*.kimi.com/*" });
  } catch (_e) {
    return null;
  }
  if (!tabs || tabs.length === 0) return null;
  for (const t of tabs) {
    try {
      const results = await chrome.scripting.executeScript({
        target: { tabId: t.id },
        // MAIN world reaches the page's own localStorage rather than
        // the isolated-world copy a content script would see.
        world: "MAIN",
        func: () => localStorage.getItem("access_token") || "",
      });
      const raw = results && results[0] && results[0].result;
      if (raw && typeof raw === "string" && raw.length > 0) {
        return raw;
      }
    } catch (_e) {
      // executeScript can fail if the tab is loading or in a special state;
      // try the next one.
      continue;
    }
  }
  return null;
}

// augmentHeadersForOrigin attaches site-specific auth/version headers
// for hosts whose internal APIs require explicit non-cookie auth.
// Returns the final headers object to pass to fetch(). For everything
// outside this branch we just relay the caller-supplied headers.
async function augmentHeadersForOrigin(url, callerHeaders) {
  const headers = { ...(callerHeaders || {}) };
  let host;
  try { host = new URL(url).hostname.toLowerCase(); } catch { return headers; }

  if (host === "platform.deepseek.com" || host.endsWith(".platform.deepseek.com")) {
    if (!hasHeader(headers, "authorization")) {
      const tok = await readDeepSeekPlatformToken();
      if (tok) {
        headers["Authorization"] = "Bearer " + tok;
      }
    }
    if (!hasHeader(headers, "x-app-version")) {
      headers["x-app-version"] = DEEPSEEK_PLATFORM_APP_VERSION;
    }
  }

  if (host === "kimi.com" || host === "www.kimi.com" || host.endsWith(".kimi.com")) {
    if (!hasHeader(headers, "authorization")) {
      const tok = await readKimiAccessToken();
      if (tok) {
        headers["Authorization"] = "Bearer " + tok;
      }
    }
  }

  return headers;
}

function hasHeader(h, name) {
  const target = name.toLowerCase();
  for (const k of Object.keys(h)) {
    if (k.toLowerCase() === target) return true;
  }
  return false;
}

async function handleFetch(msg) {
  const base = { id: msg.id, userAgent: navigator.userAgent };
  if (!originAllowed(msg.url)) {
    safeSend({ ...base, kind: "error", error: "origin not allowed: " + msg.url });
    return;
  }
  try {
    const headers = await augmentHeadersForOrigin(msg.url, msg.headers);
    const init = {
      method: msg.method || "GET",
      credentials: "include",
      // Bypass Chrome's HTTP cache. The plugin's own provider cache
      // (MinTTL floor, see internal/providers/cache.go) is the single
      // source of poll-rate control; letting Chrome cache on top of
      // that silently pins pre-reset values when claude.ai resets a
      // usage window earlier than the cached resets_at claimed.
      cache: "no-store",
      headers,
    };
    if (msg.body) {
      init.body = fromBase64(msg.body);
    }
    const resp = await fetch(msg.url, init);
    const ab = await resp.arrayBuffer();
    safeSend({
      ...base,
      kind: "fetchResult",
      status: resp.status,
      statusText: resp.statusText,
      contentType: resp.headers.get("content-type") || "",
      body: toBase64(ab),
    });
  } catch (e) {
    safeSend({ ...base, kind: "error", error: String(e && e.message ? e.message : e) });
  }
}

// Reprime: cookie-gated providers behind anti-bot (DataDome on
// portal.nousresearch.com) need their fingerprint cookie refreshed by
// a real page load. The plugin asks us to do that via this message.
// Behavior: if a tab on the target host already exists, reload it in
// place (cheapest, least disruptive); otherwise open a hidden
// background tab, give DataDome's JS time to run, then close the tab.
//
// Rate limit: per-URL in-memory map. The plugin also rate-limits, but
// SW suspension can wipe this map, so the plugin's limiter is the
// authoritative one. Keeping a local floor here is belt-and-suspenders
// for cases where the SW survives but the plugin restarts.
const lastReprimeAt = new Map();
const REPRIME_MIN_INTERVAL_MS = 60 * 60 * 1000; // 1 hour
const REPRIME_TAB_LINGER_MS = 8000; // give DataDome JS time to run + cookie set

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function handleReprime(msg) {
  const base = { id: msg.id, kind: "reprimeResult" };
  const url = msg.url;
  if (!url || !originAllowed(url)) {
    safeSend({ ...base, ok: false, error: "origin not allowed" });
    return;
  }
  const now = Date.now();
  const last = lastReprimeAt.get(url) || 0;
  if (now - last < REPRIME_MIN_INTERVAL_MS) {
    safeSend({ ...base, ok: true, skipped: true });
    return;
  }
  lastReprimeAt.set(url, now);

  try {
    const u = new URL(url);
    // Prefer reloading an existing tab on the same hostname — keeps the
    // user's tab strip stable and doesn't flash a new tab in/out.
    const matchPattern = `*://${u.hostname}/*`;
    const existing = await chrome.tabs.query({ url: matchPattern });
    if (existing.length > 0) {
      await chrome.tabs.reload(existing[0].id, { bypassCache: true });
      safeSend({ ...base, ok: true, mode: "reload" });
      return;
    }
    const tab = await chrome.tabs.create({ url, active: false });
    await sleep(REPRIME_TAB_LINGER_MS);
    try {
      await chrome.tabs.remove(tab.id);
    } catch (_e) {
      // User may have closed it already; ignore.
    }
    safeSend({ ...base, ok: true, mode: "create+close" });
  } catch (e) {
    safeSend({ ...base, ok: false, error: String(e && e.message ? e.message : e) });
  }
}

// Open the port on SW startup (install / update / browser launch).
// A live connectNative port keeps the SW alive.
connect();

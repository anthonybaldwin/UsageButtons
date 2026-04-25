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

const ALLOWED = ["claude.ai", "cursor.com", "ollama.com", "chatgpt.com", "augmentcode.com"];

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

async function handleFetch(msg) {
  const base = { id: msg.id, userAgent: navigator.userAgent };
  if (!originAllowed(msg.url)) {
    safeSend({ ...base, kind: "error", error: "origin not allowed: " + msg.url });
    return;
  }
  try {
    const init = {
      method: msg.method || "GET",
      credentials: "include",
      // Bypass Chrome's HTTP cache. The plugin's own provider cache
      // (MinTTL floor, see internal/providers/cache.go) is the single
      // source of poll-rate control; letting Chrome cache on top of
      // that silently pins pre-reset values when claude.ai resets a
      // usage window earlier than the cached resets_at claimed.
      cache: "no-store",
      // Only pass user-declared headers. The browser refuses to set
      // forbidden headers (Cookie, Host, Origin, Referer, ...); we
      // rely on credentials:"include" and the browser's own defaults
      // to get auth + a realistic UA.
      headers: msg.headers || {},
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

// Open the port on SW startup (install / update / browser launch).
// A live connectNative port keeps the SW alive.
connect();

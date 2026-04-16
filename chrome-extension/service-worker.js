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
//   - Web-Store-friendlier: purpose is "proxy for 3 APIs" not
//     "exfiltrate cookies to a local binary."
//
// ALLOWED mirrors Go's cookies.Allowed. Adding a provider requires
// updating BOTH this list AND manifest.json host_permissions AND the
// Go allowlist, plus shipping a new extension release.

const HOST_NAME = "io.github.anthonybaldwin.usagebuttons";

const ALLOWED = ["claude.ai", "cursor.com", "ollama.com"];

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
let reconnectDelay = 1000;
const RECONNECT_MAX = 30000;

function connect() {
  try {
    port = chrome.runtime.connectNative(HOST_NAME);
  } catch (e) {
    console.error("[UsageButtons] connectNative threw:", e);
    scheduleReconnect();
    return;
  }
  port.onMessage.addListener(handleMessage);
  port.onDisconnect.addListener(() => {
    const err = chrome.runtime.lastError;
    if (err) {
      console.warn("[UsageButtons] port disconnected:", err.message);
    }
    port = null;
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
}

function scheduleReconnect() {
  setTimeout(connect, reconnectDelay);
  reconnectDelay = Math.min(reconnectDelay * 2, RECONNECT_MAX);
}

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

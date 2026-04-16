// Usage Buttons cookie bridge — MV3 service worker.
//
// Talks to the Usage Buttons native-messaging host
// (io.github.anthonybaldwin.usagebuttons) over a persistent port. The
// native host bridges to the Stream Deck plugin via a local pipe.
//
// Two safety rails:
//   1. ALLOWED mirrors Go's cookies.Allowed list. The extension
//      refuses any query outside this set even if the host asks.
//      Adding a provider requires updating BOTH this list AND
//      manifest.json host_permissions AND Go's cookies.Allowed, plus
//      shipping a new extension release.
//   2. Every cookie reply carries navigator.userAgent so the plugin
//      can match the UA that obtained cf_clearance when hitting
//      Cloudflare-protected endpoints.

const HOST_NAME = "io.github.anthonybaldwin.usagebuttons";

const ALLOWED = ["claude.ai", "cursor.com", "ollama.com"];

function domainAllowed(domain) {
  const d = String(domain || "").trim().toLowerCase().replace(/^\./, "");
  if (!d) return false;
  return ALLOWED.some((a) => d === a || d.endsWith("." + a));
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

async function handleMessage(msg) {
  if (!msg || typeof msg !== "object" || !msg.kind) return;
  switch (msg.kind) {
    case "ping":
      safeSend({ id: msg.id, kind: "pong", userAgent: navigator.userAgent });
      return;
    case "getCookies":
      await handleGetCookies(msg);
      return;
    default:
      console.warn("[UsageButtons] unknown message kind:", msg.kind);
  }
}

function safeSend(obj) {
  try {
    port?.postMessage(obj);
  } catch (e) {
    console.warn("[UsageButtons] postMessage failed:", e);
  }
}

async function handleGetCookies(msg) {
  const baseReply = { id: msg.id, userAgent: navigator.userAgent };
  if (!domainAllowed(msg.domain)) {
    safeSend({ ...baseReply, kind: "error", error: "domain not allowed: " + msg.domain });
    return;
  }
  try {
    const all = await chrome.cookies.getAll({ domain: msg.domain });
    const filtered =
      Array.isArray(msg.names) && msg.names.length
        ? all.filter((c) => msg.names.includes(c.name))
        : all;
    const cookies = filtered.map((c) => ({
      name: c.name,
      value: c.value,
      domain: c.domain,
      path: c.path,
      secure: !!c.secure,
      expirationDate: c.expirationDate || 0,
      session: !!c.session,
    }));
    safeSend({ ...baseReply, kind: "cookies", cookies });
  } catch (e) {
    safeSend({ ...baseReply, kind: "error", error: String(e && e.message ? e.message : e) });
  }
}

// Open the port on SW startup (install / update / browser launch).
// A live connectNative port keeps the SW alive, so we never sleep
// while Chrome is running.
connect();

// Optional: push change notifications so the plugin can invalidate its
// cookie cache promptly. The host ignores unknown events today; this
// is forward-looking.
if (chrome.cookies?.onChanged) {
  chrome.cookies.onChanged.addListener((change) => {
    if (!change?.cookie || !domainAllowed(change.cookie.domain)) return;
    safeSend({ kind: "changed", domain: change.cookie.domain });
  });
}

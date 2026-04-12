/**
 * Per-provider snapshot cache.
 *
 * Many Stream Deck keys can be bound to the same provider — e.g., one
 * key for session %, one for weekly %, one for credits, all sourced
 * from Claude. Without coalescing, each button refresh triggers its
 * own HTTP call and we burn through rate limits in seconds (exactly
 * what happened in v0 — Anthropic 429'd us after ~6 calls).
 *
 * This cache guarantees:
 *
 *   1. At most one in-flight fetch per provider at a time. Concurrent
 *      callers await the same promise and fan out its result.
 *
 *   2. A successful snapshot is reused for `MIN_TTL_MS` regardless of
 *      how many keys poll in that window.
 *
 *   3. On HTTP 429 or other upstream error, we record a cool-down
 *      window (`COOLDOWN_MS`) during which callers get the cached
 *      snapshot marked `stale: true` with an error message — rather
 *      than retrying and making the rate limit worse.
 *
 *   4. `force` bypass is available for the keyDown "refresh now"
 *      action, but still respects cool-down (we won't re-hit a
 *      rate-limited endpoint just because the user mashed a button).
 */

import type { Provider, ProviderSnapshot } from "./types.ts";

/**
 * Observability hook. The plugin sets this to its own `log()` so cache
 * events show up in %APPDATA%/Elgato/StreamDeck/logs/io.github.anthonybaldwin.*.log.
 * Kept as an injected callback so this module stays dependency-free.
 */
let logSink: (message: string) => void = () => {};
export function setCacheLogSink(fn: (message: string) => void): void {
  logSink = fn;
}

/**
 * Cache TTL is set to match the plugin's shortest user-selectable
 * poll interval (5 minutes). That means any poll tick within 5m of
 * a prior successful fetch reuses the snapshot without touching the
 * upstream. Keys configured with longer intervals (10m / 15m / …)
 * automatically share the same snapshot as the fastest-polling key,
 * so 10 keys bound to Claude = at most one HTTP call per 5 minutes
 * regardless of how many keys exist or how often the plugin ticks.
 */
const MIN_TTL_MS = 5 * 60 * 1000;

/** After an upstream error, stop hitting the API for 10 minutes. */
const COOLDOWN_MS = 10 * 60 * 1000;

/**
 * Minimum gap between user-initiated (force=true) refreshes per
 * provider. Prevents button-mashing from hammering upstream APIs
 * even when the provider hasn't returned an error. The normal TTL
 * only gates scheduled polls — without this, every keyDown bypasses
 * the TTL and fires a real HTTP call.
 *
 * 30 seconds: responsive enough that "press to refresh" still feels
 * useful, but a frustrated user mashing won't send 60 requests/min.
 */
const MANUAL_COOLDOWN_MS = 30 * 1000;

interface CacheEntry {
  /** Last successful snapshot, if any. */
  snapshot?: ProviderSnapshot;
  /** When `snapshot` was produced. */
  fetchedAt?: number;
  /** In-flight fetch promise, so concurrent callers coalesce. */
  inflight?: Promise<ProviderSnapshot>;
  /** Error that triggered the current cool-down, if any. */
  lastError?: { message: string; at: number };
  /** Epoch ms of the last user-initiated (force=true) fetch. */
  lastForceAt?: number;
}

const entries = new Map<string, CacheEntry>();

function now(): number {
  return Date.now();
}

/** Mark a snapshot's metrics stale and replace their message. */
function markStale(
  snapshot: ProviderSnapshot,
  errorMessage: string,
): ProviderSnapshot {
  return {
    ...snapshot,
    metrics: snapshot.metrics.map((m) => ({ ...m, stale: true })),
    error: errorMessage,
  };
}

/**
 * Synthetic snapshot shown when we have nothing cached AND we're in
 * cool-down — so the button can render "RATE LIMIT" instead of
 * re-hitting the API.
 */
function errorSnapshot(
  provider: Provider,
  errorMessage: string,
): ProviderSnapshot {
  return {
    providerId: provider.id,
    providerName: provider.name,
    source: "cache",
    metrics: [],
    error: errorMessage,
    status: "unknown",
  };
}

export interface GetSnapshotOptions {
  /** Bypass the TTL (but not the cool-down). */
  force?: boolean;
}

export async function getSnapshot(
  provider: Provider,
  options: GetSnapshotOptions = {},
): Promise<ProviderSnapshot> {
  const t = now();
  const entry: CacheEntry = entries.get(provider.id) ?? {};
  entries.set(provider.id, entry);

  // Cool-down: an upstream error in the recent past. Serve cached
  // data (if any) marked stale, or a synthetic error snapshot.
  if (entry.lastError && t - entry.lastError.at < COOLDOWN_MS) {
    const secondsLeft = Math.ceil((COOLDOWN_MS - (t - entry.lastError.at)) / 1000);
    logSink(
      `cache[${provider.id}] cool-down: ${secondsLeft}s left, serving ${entry.snapshot ? "stale snapshot" : "error face"} (last error: ${entry.lastError.message})`,
    );
    if (entry.snapshot) return markStale(entry.snapshot, entry.lastError.message);
    return errorSnapshot(provider, entry.lastError.message);
  }

  // Manual refresh throttle: even when force=true, don't hit the
  // API more than once per MANUAL_COOLDOWN_MS. Serves the cached
  // snapshot instead — the user sees the same data they'd get from
  // a successful refresh anyway. This protects providers from
  // button-mashing without degrading the UX (30s is short enough
  // that a deliberate retry still works).
  if (
    options.force &&
    entry.lastForceAt &&
    t - entry.lastForceAt < MANUAL_COOLDOWN_MS &&
    entry.snapshot
  ) {
    const waitSec = Math.ceil((MANUAL_COOLDOWN_MS - (t - entry.lastForceAt)) / 1000);
    logSink(
      `cache[${provider.id}] manual refresh throttled: ${waitSec}s until next allowed`,
    );
    return entry.snapshot;
  }

  // Coalesce concurrent callers behind a single in-flight promise.
  if (entry.inflight) {
    logSink(`cache[${provider.id}] coalesced with in-flight fetch`);
    return entry.inflight;
  }

  // Fresh-enough cached snapshot wins unless the caller forces.
  if (
    !options.force &&
    entry.snapshot &&
    entry.fetchedAt &&
    t - entry.fetchedAt < MIN_TTL_MS
  ) {
    const ageSec = Math.round((t - entry.fetchedAt) / 1000);
    logSink(`cache[${provider.id}] hit (age=${ageSec}s)`);
    return entry.snapshot;
  }

  if (options.force) entry.lastForceAt = t;

  logSink(
    `cache[${provider.id}] miss — ${options.force ? "forced " : ""}fetching upstream`,
  );
  const fetchPromise = (async (): Promise<ProviderSnapshot> => {
    try {
      const snapshot = await provider.fetch({
        pollIntervalMs: MIN_TTL_MS,
        force: options.force === true,
      });
      entry.snapshot = snapshot;
      entry.fetchedAt = now();
      const hadError = !!entry.lastError;
      delete entry.lastError;
      const ids = snapshot.metrics.map((m) => m.id).join(",") || "(none)";
      logSink(
        `cache[${provider.id}] fetched OK (source=${snapshot.source}, metrics=[${ids}]${hadError ? ", recovered from error" : ""})`,
      );
      return snapshot;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      entry.lastError = { message, at: now() };
      logSink(
        `cache[${provider.id}] fetch FAILED: ${message} — cooling down for ${Math.round(COOLDOWN_MS / 1000)}s`,
      );
      if (entry.snapshot) return markStale(entry.snapshot, message);
      return errorSnapshot(provider, message);
    } finally {
      delete entry.inflight;
    }
  })();
  entry.inflight = fetchPromise;
  return fetchPromise;
}

/**
 * Synchronously peek at the cached snapshot for a provider WITHOUT
 * triggering a fetch. Returns the most recent snapshot if one exists
 * (regardless of age), or undefined if the provider was never fetched.
 *
 * Used by the willAppear handler to render directly from cache when
 * switching Stream Deck pages — avoids the loading-face flicker that
 * happens when every key fires willAppear → async fetch → render.
 */
export function peekSnapshot(providerId: string): ProviderSnapshot | undefined {
  return entries.get(providerId)?.snapshot;
}

/** Clear the cache — used by tests or by an explicit user action. */
export function clearCache(providerId?: string): void {
  if (providerId) entries.delete(providerId);
  else entries.clear();
}

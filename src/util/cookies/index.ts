/**
 * Unified browser-cookie lookup.
 *
 * Entry point for any provider that wants to auto-import a
 * browser-stored cookie on Windows. Tries Chromium-family browsers
 * first (Chrome / Edge / Brave / Vivaldi / Opera / Chromium), then
 * Firefox. Returns the first plaintext cookie value found or
 * `undefined` if nothing is configured or decryption fails.
 *
 * Callers:
 *   src/providers/claude-web.ts — auto-import the claude.ai
 *   `sessionKey` cookie when global settings say
 *   `cookieSource === "auto"`.
 */

import { findChromiumClaudeCookie } from "./chromium.ts";
import { findFirefoxClaudeCookie } from "./firefox.ts";

export interface FindCookieOptions {
  /** Cookie name to search for. Defaults to `sessionKey`. */
  cookieName?: string;
  /** Hosts to match. Defaults to claude.ai + anthropic.com variants. */
  hostPatterns?: string[];
  /** Optional logger hook so the plugin can surface decisions. */
  onLog?: (message: string) => void;
}

/**
 * Simple in-memory cache so repeated calls (every poll cycle) don't
 * re-scan every browser. The cache holds the plaintext value for a
 * fixed TTL; once it expires we re-scan from scratch in case the
 * user logged out or switched accounts. The cache is cleared by
 * `clearCookieCache()` whenever Cookie source settings change.
 */
interface CacheEntry {
  value: string;
  capturedAt: number;
}
const CACHE_TTL_MS = 30 * 60 * 1000; // 30 min
const cache = new Map<string, CacheEntry>();

function cacheKey(opts: FindCookieOptions): string {
  return `${opts.cookieName ?? "sessionKey"}|${(opts.hostPatterns ?? []).join(",")}`;
}

export function clearCookieCache(): void {
  cache.clear();
}

export async function findClaudeCookie(
  opts: FindCookieOptions = {},
): Promise<string | undefined> {
  const key = cacheKey(opts);
  const now = Date.now();
  const hit = cache.get(key);
  if (hit && now - hit.capturedAt < CACHE_TTL_MS) return hit.value;

  const chromium = await findChromiumClaudeCookie(opts);
  if (chromium) {
    cache.set(key, { value: chromium, capturedAt: now });
    return chromium;
  }
  const firefox = await findFirefoxClaudeCookie(opts);
  if (firefox) {
    cache.set(key, { value: firefox, capturedAt: now });
    return firefox;
  }
  return undefined;
}

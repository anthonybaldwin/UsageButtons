/**
 * Global plugin settings — shared across every "Usage Stat" key and
 * persisted by the Stream Deck software itself (not in the .sdPlugin
 * folder, so they survive plugin rebuilds and ride along with the
 * user's Stream Deck profile).
 *
 * Stream Deck's protocol:
 *
 *   plugin → SD: { event: "getGlobalSettings", context: pluginUUID }
 *   SD → plugin: { event: "didReceiveGlobalSettings", payload: { settings } }
 *   plugin → SD: { event: "setGlobalSettings", context: pluginUUID, payload: {...} }
 *   PI     → SD: same, but with context = the PI's UUID
 *
 * We keep a single in-memory copy and let the key loop read it via
 * `getGlobalSettings()`. Defaults apply until Stream Deck sends us
 * the real settings on connect.
 */

export const REFRESH_PRESETS = [5, 10, 15, 30, 60] as const;
export type RefreshMinutes = (typeof REFRESH_PRESETS)[number];

export const DEFAULT_REFRESH_MINUTES: RefreshMinutes = 15;

/**
 * Per-provider source preference. Mirrors CodexBar's "Source" picker
 * (Settings → Providers → <Name> → Usage source).
 *
 *   - "auto" (default): provider picks the best available data path.
 *     For Claude that means OAuth primary + Web supplement.
 *   - "oauth": only hit api.anthropic.com/api/oauth/usage.
 *     No cookie calls, even if a cookie is set. Useful for users
 *     who don't want any claude.ai traffic from the plugin.
 *   - "web": only hit claude.ai/api/*. Cookie required.
 *     Not shipping yet — needs session/weekly parity first.
 */
export type ProviderSource = "auto" | "oauth" | "web";

/**
 * Where claude.ai session cookies come from, mirroring CodexBar's
 * "Cookie source" picker:
 *   - "auto" (default): scan installed browsers on this machine
 *     (Chrome/Edge/Brave/Vivaldi/Opera/Firefox) and auto-import the
 *     claude.ai sessionKey cookie. No paste required.
 *   - "manual": only use the cookie header the user pasted into the
 *     `cookieHeader` field. Useful if the user runs a browser we
 *     don't know about or the decryption fails.
 *   - "off": don't use cookies at all. OAuth-only.
 */
export type CookieSource = "auto" | "manual" | "off";

/** Per-provider config, keyed by provider id. */
export interface ClaudeProviderSettings {
  source?: ProviderSource;
  cookieSource?: CookieSource;
  /** Raw cookie header pasted from claude.ai DevTools. Normalised on read. */
  cookieHeader?: string;
}

export interface ProviderSettingsMap {
  claude?: ClaudeProviderSettings;
  // Future: codex, cursor, droid, kimi, ollama, openrouter, …
}

export interface GlobalSettings {
  /** Refresh cadence applied to any key that doesn't set its own. */
  defaultRefreshMinutes?: RefreshMinutes;
  /** Per-provider source preferences + credentials. */
  providers?: ProviderSettingsMap;
}

let current: GlobalSettings = {
  defaultRefreshMinutes: DEFAULT_REFRESH_MINUTES,
  providers: {},
};

function normaliseSource(raw: unknown): ProviderSource {
  if (raw === "oauth" || raw === "web") return raw;
  return "auto";
}

function normaliseCookieSource(raw: unknown): CookieSource {
  if (raw === "manual" || raw === "off") return raw;
  return "auto";
}

export function setGlobalSettings(next: GlobalSettings): void {
  const minutes = next.defaultRefreshMinutes;
  const refresh: RefreshMinutes =
    typeof minutes === "number" &&
    (REFRESH_PRESETS as readonly number[]).includes(minutes)
      ? (minutes as RefreshMinutes)
      : DEFAULT_REFRESH_MINUTES;

  const rawProviders = next.providers ?? {};
  const providers: ProviderSettingsMap = {};

  const claude = rawProviders.claude;
  if (claude) {
    const entry: ClaudeProviderSettings = {
      source: normaliseSource(claude.source),
      cookieSource: normaliseCookieSource(claude.cookieSource),
    };
    const cookie =
      typeof claude.cookieHeader === "string"
        ? claude.cookieHeader.trim()
        : "";
    if (cookie.length > 0) entry.cookieHeader = cookie;
    providers.claude = entry;
  }

  current = {
    defaultRefreshMinutes: refresh,
    providers,
  };
}

export function getGlobalSettings(): Readonly<GlobalSettings> {
  return current;
}

/** Convenience: Claude provider settings with defaults applied. */
export function getClaudeSettings(): Readonly<ClaudeProviderSettings> {
  return (
    current.providers?.claude ?? { source: "auto", cookieSource: "auto" }
  );
}

/**
 * Resolve a key's effective refresh interval in milliseconds,
 * falling back to the global default if the key hasn't set one.
 */
export function resolveRefreshMs(keySettings: {
  refreshMinutes?: number;
}): number {
  const perKey = keySettings.refreshMinutes;
  if (
    typeof perKey === "number" &&
    (REFRESH_PRESETS as readonly number[]).includes(perKey)
  ) {
    return perKey * 60 * 1000;
  }
  const global = current.defaultRefreshMinutes ?? DEFAULT_REFRESH_MINUTES;
  return global * 60 * 1000;
}

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
/**
 * Where Claude metrics come from:
 *
 *   - "oauth"   — OAuth API only (~/.claude/.credentials.json +
 *                 api.anthropic.com/api/oauth/usage). Never calls
 *                 claude.ai's web API. Extras are populated only
 *                 when Anthropic returns them on the OAuth endpoint
 *                 (most accounts do not — `is_enabled: false`).
 *
 *   - "cookie"  — Cookie-primary for extras. OAuth still fetches
 *                 session/weekly/sonnet (the OAuth endpoint is the
 *                 only source for those). For the extra-usage
 *                 block we go directly to claude.ai's web API
 *                 using the pasted Cookie header, skipping OAuth's
 *                 `is_enabled` check entirely.
 *
 *   - "both"    — Default. OAuth for everything first, then cookie
 *                 supplement for extras only when OAuth's extras
 *                 block is empty / disabled. This is what you
 *                 want unless you know why you want something else.
 */
export type ProviderSource = "oauth" | "cookie" | "both";

/** Per-provider config, keyed by provider id. */
export interface ClaudeProviderSettings {
  source?: ProviderSource;
  /**
   * Raw cookie header pasted from claude.ai DevTools. When source is
   * "cookie" or "both", this value is used for claude.ai web API
   * calls. Normalised on read.
   */
  cookieHeader?: string;
}

/** Cursor requires a cookie header from cursor.com DevTools. */
export interface CursorProviderSettings {
  cookieHeader?: string;
}

export interface ProviderSettingsMap {
  claude?: ClaudeProviderSettings;
  cursor?: CursorProviderSettings;
  // Future: droid, kimi, ollama, …
}

/**
 * Text size presets for per-button value/subvalue text. Each button
 * can override the plugin-wide default from its own settings; when
 * omitted there, the global value here applies.
 */
export type TextSize = "small" | "medium" | "large";

export interface GlobalSettings {
  /** Refresh cadence applied to any key that doesn't set its own. */
  defaultRefreshMinutes?: RefreshMinutes;
  /** Plugin-wide default big-number size, overridable per-button. */
  defaultValueSize?: TextSize;
  /** Plugin-wide default reset-countdown size, overridable per-button. */
  defaultSubvalueSize?: TextSize;
  /**
   * Plugin-wide invert-fill toggle. Applies to every button and is
   * NOT overridable per-button — "% used" vs "% remaining" is a
   * taste choice users want to make once for the whole plugin, not
   * babysit on every key individually.
   */
  invertFill?: boolean;
  /**
   * Plugin-wide "show provider glyph watermark on every button"
   * toggle. Default true. Per-button `showGlyph: false` can opt
   * out individually; setting this to false hides every glyph
   * globally regardless of per-button settings.
   */
  showGlyphs?: boolean;
  /** Per-provider source preferences + credentials. */
  providers?: ProviderSettingsMap;
}

let current: GlobalSettings = {
  defaultRefreshMinutes: DEFAULT_REFRESH_MINUTES,
  defaultValueSize: "large",
  defaultSubvalueSize: "large",
  invertFill: false,
  showGlyphs: true,
  providers: {},
};

function normaliseTextSize(raw: unknown, fallback: TextSize): TextSize {
  if (raw === "small" || raw === "medium" || raw === "large") return raw;
  return fallback;
}

function normaliseSource(raw: unknown): ProviderSource {
  if (raw === "oauth" || raw === "cookie") return raw;
  return "both";
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
    };
    const cookie =
      typeof claude.cookieHeader === "string"
        ? claude.cookieHeader.trim()
        : "";
    if (cookie.length > 0) entry.cookieHeader = cookie;
    providers.claude = entry;
  }

  const cursor = rawProviders.cursor;
  if (cursor) {
    const entry: CursorProviderSettings = {};
    const cookie =
      typeof cursor.cookieHeader === "string"
        ? cursor.cookieHeader.trim()
        : "";
    if (cookie.length > 0) entry.cookieHeader = cookie;
    providers.cursor = entry;
  }

  current = {
    defaultRefreshMinutes: refresh,
    defaultValueSize: normaliseTextSize(next.defaultValueSize, "large"),
    defaultSubvalueSize: normaliseTextSize(next.defaultSubvalueSize, "large"),
    invertFill: next.invertFill === true,
    showGlyphs: next.showGlyphs !== false,
    providers,
  };
}

export function getDefaultValueSize(): TextSize {
  return current.defaultValueSize ?? "large";
}
export function getDefaultSubvalueSize(): TextSize {
  return current.defaultSubvalueSize ?? "large";
}
export function getInvertFill(): boolean {
  return current.invertFill === true;
}
export function getShowGlyphs(): boolean {
  return current.showGlyphs !== false;
}

export function getGlobalSettings(): Readonly<GlobalSettings> {
  return current;
}

/** Convenience: Claude provider settings with defaults applied. */
export function getClaudeSettings(): Readonly<ClaudeProviderSettings> {
  return current.providers?.claude ?? { source: "both" };
}

/** Convenience: Cursor provider settings. */
export function getCursorSettings(): Readonly<CursorProviderSettings> {
  return current.providers?.cursor ?? {};
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

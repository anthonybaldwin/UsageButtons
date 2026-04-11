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

export interface GlobalSettings {
  /** Refresh cadence applied to any key that doesn't set its own. */
  defaultRefreshMinutes?: RefreshMinutes;
}

let current: GlobalSettings = {
  defaultRefreshMinutes: DEFAULT_REFRESH_MINUTES,
};

export function setGlobalSettings(next: GlobalSettings): void {
  // Validate: keep only known refresh presets.
  const minutes = next.defaultRefreshMinutes;
  if (
    typeof minutes === "number" &&
    (REFRESH_PRESETS as readonly number[]).includes(minutes)
  ) {
    current = { defaultRefreshMinutes: minutes as RefreshMinutes };
  } else {
    current = { defaultRefreshMinutes: DEFAULT_REFRESH_MINUTES };
  }
}

export function getGlobalSettings(): Readonly<GlobalSettings> {
  return current;
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

/**
 * Provider framework.
 *
 * A "provider" is a source of usage data (Claude, Codex, Cursor, …).
 * Each provider exposes one or more named "metrics" — session %,
 * weekly %, credits, code-review % — which the plugin can bind to a
 * Stream Deck button.
 *
 * The initial shape mirrors the CodexBar CLI JSON contract (see
 * `tmp/CodexBar/docs/cli.md`): `primary`/`secondary`/`tertiary` usage
 * windows plus optional credits. We intentionally stay open for
 * extension — each provider may add extra metrics beyond those.
 */

/** Direction the button fill should animate as the value moves. */
export type FillDirection = "up" | "down" | "right" | "left";

export interface MetricValue {
  /** Stable metric id, e.g. `"session-percent"`. */
  id: string;
  /** Short uppercase label for the button, e.g. `"SESSION"`. */
  label: string;
  /** Longer human name, e.g. `"Session window (5 hours)"`. */
  name: string;
  /** Current value (number or preformatted string). */
  value: number | string;
  /**
   * Raw numeric value — useful when `value` is preformatted
   * (e.g. `"$204.80"`). Lets the render layer compare thresholds
   * (warn/critical colors) without parsing the display string.
   */
  numericValue?: number;
  /** Unit the `numericValue` is expressed in, one of a known set. */
  numericUnit?: "percent" | "dollars" | "cents" | "count";
  /**
   * Which direction is "good" for the threshold logic.
   *
   *   - "high" (default): high value = good, low = bad. Used for
   *     "% remaining", balance, available credits, etc. Warn/
   *     critical thresholds fire when value ≤ thresholds.
   *   - "low": low value = good, high = bad. Used for "amount
   *     spent", "% used", requests consumed, etc. Warn/critical
   *     fire when value ≥ thresholds.
   */
  numericGoodWhen?: "high" | "low";
  /**
   * The "100%" reference for this metric, when applicable. For
   * dollar metrics that are bounded by a limit (spent vs. monthly
   * limit), setting `numericMax` lets the renderer pick sensible
   * default thresholds as percentages of the max (80% warn, 100%
   * critical for "low-is-good" metrics).
   */
  numericMax?: number;
  /** Optional unit string for display, e.g. `"%"`, `" credits"`. */
  unit?: string;
  /** Optional 0..1 ratio driving the button fill. */
  ratio?: number;
  /** Fill direction for the button icon. */
  direction?: FillDirection;
  /** Seconds until this value resets/refills, if applicable. */
  resetInSeconds?: number;
  /** When the source last produced this value. */
  updatedAt?: Date;
  /** True if the value is stale / came from cache / errored. */
  stale?: boolean;
}

export interface ProviderSnapshot {
  providerId: string;
  providerName: string;
  /** Source label, e.g. `"mock"`, `"oauth"`, `"web"`, `"cli"`, `"cache"`. */
  source: string;
  /** All metrics currently known for this provider. */
  metrics: MetricValue[];
  /** Provider-level status, if available. */
  status?: "operational" | "degraded" | "outage" | "unknown";
  /** Human error message describing the most recent upstream failure. */
  error?: string;
}

export interface ProviderContext {
  /** Suggested poll cadence in milliseconds. */
  pollIntervalMs: number;
  /** Signal to abort a long-running fetch. */
  signal?: AbortSignal;
  /**
   * User-initiated refresh (e.g. keyDown on a Stream Deck key).
   * Providers may use this to bypass sub-caches and secondary
   * rate-limit policies that they'd normally respect.
   */
  force?: boolean;
}

export interface Provider {
  readonly id: string;
  readonly name: string;
  /**
   * Default fill color for buttons bound to this provider, as a
   * lowercase `#rrggbb` string. Borrowed from CodexBar's
   * `ProviderBranding.color` so our buttons look visually familiar
   * to anyone already using CodexBar. New providers should pick
   * their value from `src/providers/brand-colors.ts` rather than
   * hand-rolling one.
   */
  readonly brandColor: string;
  /** All metric ids this provider *may* emit. Used for the PI dropdown. */
  readonly metricIds: readonly string[];
  fetch(ctx: ProviderContext): Promise<ProviderSnapshot>;
}

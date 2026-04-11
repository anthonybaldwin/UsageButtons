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
  /** Optional unit string, e.g. `"%"`, `" credits"`. */
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
}

export interface Provider {
  readonly id: string;
  readonly name: string;
  /** All metric ids this provider *may* emit. Used for the PI dropdown. */
  readonly metricIds: readonly string[];
  fetch(ctx: ProviderContext): Promise<ProviderSnapshot>;
}

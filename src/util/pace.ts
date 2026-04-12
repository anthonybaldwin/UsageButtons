/**
 * Usage pace calculation.
 *
 * Computes how a user's actual usage compares to a linear "even burn"
 * expectation across a rate-limit window. Ported from CodexBar's
 * `UsagePace.swift` and Win-CodexBar's `usage_pace.rs`.
 *
 * The math:
 *   elapsed  = windowDuration - timeUntilReset
 *   expected = (elapsed / windowDuration) * 100
 *   delta    = actualUsedPercent - expected
 *
 * A positive delta means the user is burning faster than the linear
 * rate (ahead of pace / at risk of hitting the cap early). A negative
 * delta means they're burning slower (behind pace / will have headroom
 * left when the window resets).
 *
 * Threshold bands (matching CodexBar):
 *   |delta| <= 2  → "On pace"
 *   delta  < -2   → "Behind (-X%)"   — under budget, good
 *   delta  >  2   → "Ahead (+X%)"    — over budget, risky
 */

export interface PaceResult {
  /** Signed delta: positive = ahead (burning fast), negative = behind (safe). */
  delta: number;
  /** Human label: "On pace", "Behind (-12%)", "Ahead (+8%)". */
  label: string;
  /** Qualitative band for optional color coding. */
  band: "on-pace" | "behind" | "ahead";
}

const ON_PACE_THRESHOLD = 2;

/**
 * Compute pace for a rate-limit window.
 *
 * @param usedPercent   How much of the window has been consumed (0–100).
 * @param resetInSeconds  Seconds until the window resets.
 * @param windowSeconds   Total window duration in seconds (e.g. 18000 for 5h, 604800 for 7d).
 * @returns PaceResult, or undefined if inputs are missing / nonsensical.
 */
export function computePace(
  usedPercent: number | undefined,
  resetInSeconds: number | undefined,
  windowSeconds: number,
): PaceResult | undefined {
  if (
    typeof usedPercent !== "number" ||
    typeof resetInSeconds !== "number" ||
    windowSeconds <= 0
  ) {
    return undefined;
  }

  const elapsed = Math.max(0, windowSeconds - resetInSeconds);
  const expected = (elapsed / windowSeconds) * 100;
  const actual = Math.max(0, Math.min(100, usedPercent));
  const delta = actual - expected;
  const absDelta = Math.abs(delta);
  const rounded = Math.round(absDelta);

  if (absDelta <= ON_PACE_THRESHOLD) {
    return { delta: 0, label: "On pace", band: "on-pace" };
  }

  if (delta < 0) {
    return {
      delta: Math.round(delta),
      label: `Behind (-${rounded}%)`,
      band: "behind",
    };
  }

  return {
    delta: Math.round(delta),
    label: `Ahead (+${rounded}%)`,
    band: "ahead",
  };
}

/**
 * Stream Deck plugin entry point.
 *
 * Compiled into a native binary via `bun build --compile`, dropped
 * into `io.github.anthonybaldwin.usage-buttons.sdPlugin/bin/plugin-<os>`, and
 * launched by the Stream Deck software with registration args.
 */

import { parseArgs, StreamDeckConnection } from "./streamdeck.ts";
import type { InboundEvent, WillAppearEvent } from "./streamdeck.ts";
import { renderButtonSvg, renderLoadingSvg } from "./render.ts";
import { PROVIDER_ICONS } from "./providers/provider-icons.generated.ts";
import { getProvider } from "./providers/registry.ts";
import { getSnapshot, setCacheLogSink } from "./providers/cache.ts";
import { setClaudeDebugLogSink } from "./providers/claude.ts";
import { setClaudeWebLogSink } from "./providers/claude-web.ts";
import type { MetricValue, Provider } from "./providers/types.ts";
import {
  getDefaultSubvalueSize,
  getDefaultValueSize,
  getInvertFill,
  getShowGlyphs,
  resolveRefreshMs,
  setGlobalSettings,
  type GlobalSettings,
} from "./settings.ts";

interface KeySettings {
  providerId?: string;
  metricId?: string;
  /**
   * Threshold at which to paint the fill in the "warning" color
   * (default orange) because the value is getting low.
   *
   * Interpretation depends on the metric's `numericUnit`:
   *   - "percent" → warn when numericValue ≤ this (e.g. 20 = warn at 20%)
   *   - "dollars" → warn when numericValue ≤ this (e.g. 10 = warn below $10)
   *   - "cents"   → warn when numericValue ≤ this
   *   - "count"   → warn when numericValue ≤ this
   *
   * Default handled at render time if omitted — 20 for percent,
   * 10 for dollars.
   */
  warnBelow?: number;
  /** Same semantic as `warnBelow` but for the critical/red threshold. */
  criticalBelow?: number;
  /** Hex color for the warning state. Default "#f59e0b" (amber). */
  warnColor?: string;
  /** Hex color for the critical state. Default "#ef4444" (red). */
  criticalColor?: string;
  /** Optional override for the label rendered inside the SVG. Blank = use the metric's default. Multi-line OK. */
  labelOverride?: string;
  /** Hide the inner SVG label entirely (e.g. when using the Stream Deck native title). */
  hideLabel?: boolean;
  /** Fill color hex, e.g. "#10b981". Blank = metric default. */
  fillColor?: string;
  /** Background color hex. Blank = dark default. */
  bgColor?: string;
  /** Text (foreground) color hex. Blank = default white. */
  textColor?: string;
  /** Direction the fill grows in as the value climbs toward 100%. */
  fillDirection?: "up" | "down" | "right" | "left";
  /** Big-number size. Overrides the plugin-wide default. */
  valueSize?: "small" | "medium" | "large";
  /** Reset-countdown subvalue size. Overrides the plugin-wide default. */
  subvalueSize?: "small" | "medium" | "large";
  /** Render the outer rounded-rect border. Default true. */
  showBorder?: boolean;
  /** Render the provider logo as a top-right corner badge. Default true. */
  showGlyph?: boolean;
  /** Show the reset countdown as a subvalue under the big number. Default true. */
  showResetTimer?: boolean;
  /**
   * Per-key refresh cadence in minutes. One of 5, 10, 15, 30, 60.
   * When undefined, the key uses the plugin's global default.
   */
  refreshMinutes?: number;
}

interface VisibleKey {
  context: string;
  settings: KeySettings;
  /** Epoch ms of the most recent refresh (or 0 before the first one). */
  lastPollAt: number;
}

const ACTION_UUID = "io.github.anthonybaldwin.usage-buttons.stat";
const DEFAULT_PROVIDER = "mock";
const DEFAULT_METRIC = "session-percent";
/**
 * How often the plugin's master scheduler ticks. On every tick we
 * iterate all visible keys and call refreshKey() on any whose
 * per-key interval has elapsed since the last poll. The tick is
 * short (30s) so a freshly-added key with a 5m interval sees its
 * first data within at most 30s after appearing, and so per-key
 * interval changes feel responsive.
 */
const SCHEDULER_TICK_MS = 30_000;

const visibleKeys = new Map<string, VisibleKey>();

async function main(): Promise<void> {
  // Do NOT slice(2) — when `bun build --compile` produces a standalone
  // binary, Bun.argv is [exePath, ...cliArgs] (single leading entry,
  // not bun+script), so slicing past the program name would eat the
  // first flag. parseArgs uses indexOf so it doesn't care about the
  // leading exe path.
  const args = parseArgs(Bun.argv);
  const connection = new StreamDeckConnection(args);
  await connection.connect();

  // Wire the cache's log events through to Stream Deck's per-plugin
  // log file so we can see hit/miss/cool-down decisions in
  // %APPDATA%/Elgato/StreamDeck/logs/io.github.anthonybaldwin.usage-buttons*.log.
  setCacheLogSink((msg) => connection.log(msg));
  setClaudeDebugLogSink((msg) => connection.log(msg));
  setClaudeWebLogSink((msg) => connection.log(msg));

  connection.onEvent((event) => handleEvent(connection, event));

  // Ask Stream Deck for our plugin-wide settings so we know the
  // user's preferred default refresh interval before the first
  // scheduler tick fires.
  connection.send({ event: "getGlobalSettings", context: args.pluginUUID });

  setInterval(() => void scheduleDueKeys(connection), SCHEDULER_TICK_MS);
}

/**
 * Scheduler tick: fire refreshKey on any visible key whose per-key
 * interval has elapsed. Each key tracks its own `lastPollAt` so
 * keys with different intervals tick independently.
 */
/**
 * Re-render every currently visible key from its cached provider
 * snapshot. Used after a global-settings change (font size, invert
 * fill, etc.) so the UI updates without waiting for the next poll.
 * Doesn't force a fresh provider.fetch(); the existing cached
 * snapshot is enough because the change is display-only.
 */
async function refreshAllVisibleKeys(conn: StreamDeckConnection): Promise<void> {
  await Promise.all(
    [...visibleKeys.keys()].map((ctx) => refreshKey(conn, ctx, { force: false })),
  );
}

/**
 * Clear per-key valueSize + subvalueSize overrides from every
 * visible key so the plugin-wide defaults take over. Invoked by
 * the "Reset per-button text-size overrides" action in the PI.
 *
 * Only touches keys currently visible on a Stream Deck device —
 * keys on inactive profiles aren't tracked by the plugin and will
 * continue to carry their old overrides until they reappear.
 */
async function resetTextSizeOverrides(
  conn: StreamDeckConnection,
): Promise<void> {
  conn.log(
    `resetTextSizeOverrides: clearing per-key text size overrides on ${visibleKeys.size} visible key(s)`,
  );
  for (const [ctx, key] of visibleKeys) {
    // Mutate in-memory and persist via setSettings so a Stream
    // Deck restart won't bring the old values back.
    delete key.settings.valueSize;
    delete key.settings.subvalueSize;
    conn.send({
      event: "setSettings",
      context: ctx,
      payload: key.settings as Record<string, unknown>,
    });
  }
  await refreshAllVisibleKeys(conn);
}

async function scheduleDueKeys(conn: StreamDeckConnection): Promise<void> {
  const now = Date.now();
  const due: string[] = [];
  for (const [ctx, key] of visibleKeys) {
    const intervalMs = resolveRefreshMs(key.settings);
    if (now - key.lastPollAt >= intervalMs) due.push(ctx);
  }
  if (due.length === 0) return;
  await Promise.all(due.map((ctx) => refreshKey(conn, ctx, { force: false })));
}

function handleEvent(conn: StreamDeckConnection, event: InboundEvent): void {
  switch (event.event) {
    case "willAppear": {
      const e = event as WillAppearEvent;
      if (e.action !== ACTION_UUID) return;
      const settings = e.payload.settings as KeySettings;
      visibleKeys.set(e.context, {
        context: e.context,
        settings,
        lastPollAt: 0,
      });
      // Paint a loading face synchronously so the button doesn't
      // show the static manifest placeholder while the first
      // async fetch resolves. This is what the user sees for ~1-2s
      // on plugin start / key drag — had to be clean.
      conn.setImage(e.context, loadingFaceFor(settings));
      conn.log(
        `key appeared (now tracking ${visibleKeys.size} visible key(s))`,
      );
      // Kick off the real fetch. refreshKey will overwrite the
      // loading face with the real data (or an error face) when
      // the snapshot arrives.
      void refreshKey(conn, e.context, { force: false });
      return;
    }
    case "willDisappear": {
      // Stream Deck fires willDisappear when a key leaves the
      // currently-visible page (profile switch, page swap, app
      // hide). Removing it from visibleKeys is what stops the
      // poll scheduler from touching it — the scheduler only
      // iterates visibleKeys, so off-page keys never poll. This
      // is the page-aware-refresh behaviour the user explicitly
      // asked for.
      const ctx = event.context;
      if (ctx && visibleKeys.has(ctx)) {
        visibleKeys.delete(ctx);
        conn.log(
          `key disappeared (now tracking ${visibleKeys.size} visible key(s))`,
        );
      }
      return;
    }
    case "didReceiveSettings": {
      const ctx = event.context;
      const payload = (event as { payload?: { settings?: KeySettings } }).payload;
      if (!ctx || !payload) return;
      const existing = visibleKeys.get(ctx);
      if (!existing) return;
      existing.settings = payload.settings ?? {};
      // Settings changes re-render immediately from the cached
      // snapshot — same provider, just a different metric / color /
      // interval. We also reset lastPollAt so an interval-reduction
      // takes effect on the very next scheduler tick.
      existing.lastPollAt = 0;
      void refreshKey(conn, ctx, { force: false });
      return;
    }
    case "didReceiveGlobalSettings": {
      const payload = (event as { payload?: { settings?: GlobalSettings } })
        .payload;
      setGlobalSettings(payload?.settings ?? {});
      conn.log(
        `global settings updated: default=${payload?.settings?.defaultRefreshMinutes ?? "default"}m`,
      );
      // A global font-size change should re-render every visible
      // key whose settings don't explicitly override (so the new
      // default kicks in immediately rather than on the next poll).
      void refreshAllVisibleKeys(conn);
      return;
    }
    case "sendToPlugin": {
      // Custom Property Inspector → plugin events. Right now the
      // only one is "resetTextSizeOverrides" which clears
      // valueSize / subvalueSize from every visible key so the
      // plugin-wide default takes over everywhere.
      const payload = (event as { payload?: { action?: string } }).payload;
      if (payload?.action === "resetTextSizeOverrides") {
        void resetTextSizeOverrides(conn);
      }
      return;
    }
    case "keyDown": {
      // User-initiated refresh: force a cache bypass (still gated
      // by the per-provider cool-down so we don't fight rate limits).
      const ctx = event.context;
      if (ctx) void refreshKey(conn, ctx, { force: true });
      return;
    }
    default:
      return;
  }
}

async function refreshKey(
  conn: StreamDeckConnection,
  context: string,
  opts: { force: boolean },
): Promise<void> {
  const key = visibleKeys.get(context);
  if (!key) return;
  key.lastPollAt = Date.now();

  const providerId = key.settings.providerId ?? DEFAULT_PROVIDER;
  const metricId = key.settings.metricId ?? DEFAULT_METRIC;
  const provider = getProvider(providerId);
  if (!provider) {
    conn.setImage(
      context,
      renderButtonSvg({
        label: "ERR",
        value: "?",
        subvalue: providerId,
        stale: true,
      }),
    );
    return;
  }

  // The per-provider cache handles HTTP coalescing, TTL reuse, and
  // 429 cool-down — we never call provider.fetch() directly from the
  // key loop anymore.
  const snapshot = await getSnapshot(provider, { force: opts.force });

  if (snapshot.error && snapshot.metrics.length === 0) {
    // Cool-down state with nothing to show. Render a friendly
    // "WAIT" / "rate limit" face rather than the raw provider name
    // so the user can tell the button from a hard error. Uses the
    // placeholderFace() helper so the rate-limit tile still carries
    // the provider's brand color + logo watermark — otherwise a
    // rate-limited Claude button is indistinguishable from a
    // rate-limited Codex button.
    const rate = isRateLimit(snapshot.error);
    conn.setImage(
      context,
      placeholderFace({
        provider,
        label: provider.name.toUpperCase(),
        value: rate ? "WAIT" : "ERR",
        subvalue: rate ? "rate limit" : undefined,
        keySettings: key.settings,
      }),
    );
    return;
  }

  const metric = snapshot.metrics.find((m) => m.id === metricId);
  if (!metric) {
    // The "synthesize a 0%-remaining fake" exhaustion fallback
    // ONLY applies when the REQUESTED metric is itself a percent
    // metric (`*-percent`). For non-percent metrics like
    // `credits-balance`, `extra-usage-balance`, etc., synthesizing
    // a percent-style 0% fake produces a confusing button (e.g.
    // "CREDITS 100%" because the user has no credits → metric
    // gated out → fake fired → percent-shaped output that doesn't
    // describe anything real).
    //
    // Behaviour:
    //   percent metric missing + companion percent exhausted →
    //     synthesize a fake 0% remaining to mirror the cap
    //   percent metric missing + no exhausted companion →
    //     quiet "—" placeholder
    //   non-percent metric missing → ALWAYS quiet "—" placeholder
    //     (we have no honest way to fake a balance/spend value)
    const isRequestedPercent = /-percent$/.test(metricId);
    if (isRequestedPercent) {
      const exhausted = snapshot.metrics.find(
        (m) => /-percent$/.test(m.id) && (m.ratio ?? 1) <= 0.01,
      );
      if (exhausted) {
        const fake: MetricValue = {
          id: metricId,
          label: deriveLabelFromMetricId(metricId),
          name: `${deriveLabelFromMetricId(metricId)} (capped)`,
          value: 0,
          numericValue: 0,
          numericUnit: "percent",
          unit: "%",
          ratio: 0,
          direction: "up",
        };
        if (exhausted.resetInSeconds !== undefined) {
          fake.resetInSeconds = exhausted.resetInSeconds;
        }
        conn.setImage(
          context,
          renderMetric(provider, snapshot.providerName, fake, key.settings),
        );
        return;
      }
    }

    // Genuinely missing metric (provider doesn't expose it for
    // this account / plan) — fall back to a quiet placeholder
    // with the metric's natural label so the user knows which
    // button is the empty one. placeholderFace() keeps the
    // provider's glyph + brand color visible so the tile still
    // reads as "this is an assigned Claude/Codex button with no
    // data right now" instead of a black tile with no context.
    conn.setImage(
      context,
      placeholderFace({
        provider,
        label: deriveLabelFromMetricId(metricId),
        value: "—",
        keySettings: key.settings,
      }),
    );
    return;
  }
  conn.setImage(
    context,
    renderMetric(provider, snapshot.providerName, metric, key.settings),
  );
}

function isRateLimit(errorMessage: string): boolean {
  return /429|rate.?limit/i.test(errorMessage);
}

/**
 * Render a "no data right now" placeholder tile that still carries
 * the provider's identity. Used for two cases:
 *
 *   1. The metric a button is configured for doesn't exist in the
 *      current snapshot (e.g. CODEX credits-balance on a Plus
 *      plan). Shows the metric label + "—" value.
 *
 *   2. The provider is in cool-down from a rate limit and has no
 *      cached data. Shows the provider name + "WAIT" / "ERR" with
 *      an optional "rate limit" subvalue.
 *
 * Both cases used to render as a flat dark tile (label + value on
 * a stale-dimmed #111827 background, no glyph, no brand color),
 * which made them visually indistinguishable from each other AND
 * from a broken/unassigned button. Now every placeholder carries:
 *
 *   - the provider's watermark glyph (same layout as a live tile)
 *   - the provider's brand color as the fill (even though ratio=0
 *     suppresses the meter rect, keeping it in the input lets the
 *     glyph sit against the expected color family)
 *   - no `stale: true` — the whole-tile 0.45 opacity dim turned
 *     the glyph into an invisible ghost. The "—" value + empty
 *     meter already signal "nothing to show" without dimming
 *     everything into a blur.
 *
 * Respects the global/per-key showGlyph toggle so a user who
 * explicitly hid glyphs on a button sees a glyph-less placeholder
 * too (consistency with the live metric view).
 */
function placeholderFace(opts: {
  provider: Provider;
  label: string;
  value: string;
  subvalue?: string | undefined;
  keySettings: KeySettings;
}): string {
  const input: Parameters<typeof renderButtonSvg>[0] = {
    label: opts.label,
    value: opts.value,
    fill: opts.provider.brandColor,
  };
  if (opts.subvalue) input.subvalue = opts.subvalue;
  if (opts.keySettings.bgColor && /^#[0-9a-fA-F]{3,8}$/.test(opts.keySettings.bgColor)) {
    input.bg = opts.keySettings.bgColor;
  }
  if (opts.keySettings.textColor && /^#[0-9a-fA-F]{3,8}$/.test(opts.keySettings.textColor)) {
    input.fg = opts.keySettings.textColor;
  }
  if (opts.keySettings.showBorder === false) input.border = false;
  input.valueSize = opts.keySettings.valueSize ?? getDefaultValueSize();
  input.subvalueSize = opts.keySettings.subvalueSize ?? getDefaultSubvalueSize();
  const wantGlyph = getShowGlyphs() && opts.keySettings.showGlyph !== false;
  if (wantGlyph) {
    const glyph = PROVIDER_ICONS[opts.provider.id];
    if (glyph) {
      input.glyph = glyph;
      input.glyphMode = "watermark";
    }
  } else {
    input.showGlyph = false;
  }
  return renderButtonSvg(input);
}

/**
 * Pick a button label for a metric id when the metric isn't in
 * the snapshot (e.g. provider hid it because a sibling quota was
 * exhausted). Used by the synth-metric fallback in refreshKey so
 * a "blocked" SESSION button still shows "SESSION" rather than
 * the provider name.
 */
function deriveLabelFromMetricId(metricId: string): string {
  const KNOWN: Record<string, string> = {
    "session-percent": "SESSION",
    "weekly-percent": "WEEKLY",
    "weekly-sonnet-percent": "SONNET",
    "weekly-opus-percent": "OPUS",
    "extra-usage-percent": "EXTRA USAGE",
    "extra-usage-limit": "LIMIT",
    "extra-usage-remaining": "LEFT",
    "extra-usage-spent": "SPENT",
    "extra-usage-balance": "BALANCE",
    "extra-usage-enabled": "EXTRA USAGE",
    "credits-balance": "CREDITS",
    "credits": "CREDITS",
  };
  if (KNOWN[metricId]) return KNOWN[metricId];
  // Fallback: first hyphen-segment uppercase.
  const first = metricId.split("-")[0] ?? metricId;
  return first.toUpperCase();
}

/**
 * Compute whether a metric should render in its warn / critical
 * color based on per-button threshold settings.
 *
 * Returns "normal" when no thresholds apply (metric lacks a
 * `numericValue`, or thresholds are unset / out of range).
 *
 * Direction:
 *   - metric.numericGoodWhen === "high" (default) — value is
 *     "good" when high. Warn/critical fire when value <=
 *     threshold. E.g. a balance or "% remaining" dropping toward
 *     zero.
 *   - metric.numericGoodWhen === "low" — value is "good" when low.
 *     Warn/critical fire when value >= threshold. E.g. an "amount
 *     spent" or "% used" climbing toward a cap.
 *
 * Default thresholds depend on direction + unit:
 *   - "high" + percent → warn ≤ 20, critical ≤ 10
 *   - "high" + dollars → warn ≤ 10, critical ≤ 0  (negative balance)
 *   - "low"  + percent → warn ≥ 80, critical ≥ 95
 *   - "low"  + dollars, numericMax known → warn ≥ 80% of max,
 *                                          critical ≥ 100% of max
 *   - "low"  + dollars, no numericMax → no color change (can't
 *                                       guess without a budget)
 *
 * The user's per-key `warnBelow` / `criticalBelow` settings still
 * override these — they're interpreted as threshold numbers in the
 * metric's natural unit, applied with whichever direction the
 * metric declares.
 */
function computeThresholdState(
  metric: MetricValue,
  settings: KeySettings,
): "normal" | "warn" | "critical" {
  if (typeof metric.numericValue !== "number") return "normal";

  // Threshold colors are restricted to MONEY metrics only.
  // Percent / count metrics communicate "low headroom" via their
  // own value text — a 5% session window is already obviously
  // low without us painting it red. Money metrics get colors
  // because $ approaching a budget cap is a thing users want a
  // visual alarm for.
  if (metric.numericUnit !== "dollars" && metric.numericUnit !== "cents") {
    return "normal";
  }

  const n = metric.numericValue;
  const direction = metric.numericGoodWhen ?? "high";

  let defaultWarn: number | undefined;
  let defaultCritical: number | undefined;

  if (direction === "high") {
    // High balance / remaining is good — warn / red as it drops.
    defaultWarn = 10;
    defaultCritical = 0;
  } else {
    // Low spend is good — warn / red as it climbs toward the cap.
    if (typeof metric.numericMax === "number" && metric.numericMax > 0) {
      defaultWarn = metric.numericMax * 0.8;
      defaultCritical = metric.numericMax;
    }
    // else: no defaults — without a max we can't tell "high" from
    // "runaway". User can still set explicit thresholds in settings.
  }

  const warn = settings.warnBelow ?? defaultWarn;
  const critical = settings.criticalBelow ?? defaultCritical;

  if (direction === "high") {
    if (typeof critical === "number" && n <= critical) return "critical";
    if (typeof warn === "number" && n <= warn) return "warn";
  } else {
    if (typeof critical === "number" && n >= critical) return "critical";
    if (typeof warn === "number" && n >= warn) return "warn";
  }
  return "normal";
}

/**
 * Render a "loading" face for a brand-new key that hasn't had its
 * first fetch yet. Shows just the provider's logo glyph (CodexBar's
 * MIT-licensed SVG paths) centered on the canvas, in the provider's
 * brand color. Falls back to a neutral dot if the provider has no
 * known glyph.
 */
function loadingFaceFor(settings: KeySettings): string {
  const providerId = settings.providerId ?? DEFAULT_PROVIDER;
  const provider = getProvider(providerId);
  const glyph = PROVIDER_ICONS[providerId];
  const input: Parameters<typeof renderLoadingSvg>[0] = {};
  if (glyph) input.glyph = glyph;
  if (settings.fillColor && /^#[0-9a-fA-F]{3,8}$/.test(settings.fillColor)) {
    input.fill = settings.fillColor;
  } else if (provider?.brandColor) {
    input.fill = provider.brandColor;
  }
  if (settings.bgColor && /^#[0-9a-fA-F]{3,8}$/.test(settings.bgColor)) {
    input.bg = settings.bgColor;
  }
  if (settings.textColor && /^#[0-9a-fA-F]{3,8}$/.test(settings.textColor)) {
    input.fg = settings.textColor;
  }
  if (settings.showBorder === false) input.border = false;
  return renderLoadingSvg(input);
}

function renderMetric(
  provider: Provider,
  providerName: string,
  metric: MetricValue,
  settings: KeySettings,
): string {
  // Inverted view — when the plugin-wide `invertFill` setting is on,
  // every PERCENT metric renders as "X% used" instead of
  // "X% remaining" (or vice versa). Flips BOTH the displayed number
  // AND the fill ratio together; flipping just the ratio would
  // produce a 2% bar with "98%" text on top, which is nonsense.
  //
  // Gate: `numericUnit === "percent"` ONLY.
  //
  // Deliberately NOT flipping three other classes of metric, each
  // for its own reason:
  //
  //   1. Reference tiles (BALANCE, LIMIT, CREDITS) — pinned to
  //      ratio: 1 by design. Inverting ratio 1 → 0 just empties
  //      the tile, which doesn't express any valid alternate
  //      viewpoint. A "static reference tile, but now invisible"
  //      isn't the flip of anything useful. The numericUnit check
  //      catches these today because they're all dollar metrics.
  //
  //   2. Dollar progress meters (SPENT) — they DO have a progress
  //      axis (spent / limit), so technically they could flip to
  //      a "headroom in dollars" view. We deliberately don't,
  //      because the display number flip is semantically ugly:
  //      "$0.00 SPENT" flipping to "$50.00 SPENT" reads as a lie
  //      about the label's meaning. The HEADROOM metric already
  //      provides the exact "how much left" viewpoint this would
  //      be trying to express — users who want that view should
  //      add a HEADROOM tile instead of toggling a global flip
  //      that mangles dollar figures.
  //
  //   3. Binary state tiles (EXTRA USAGE ON/OFF, RELOAD ON/OFF) —
  //      ratio is 1 or 0 based on a state flag, not progress.
  //      Inverting "ON" to "OFF" would lie about the provider's
  //      actual state. The numericUnit check catches these because
  //      they have no numericUnit at all.
  //
  // Using `numericUnit === "percent"` rather than the old
  // `unit === "%"` display-string match, because numericUnit is
  // the semantic enum field — a future metric could legitimately
  // want a display unit of "%" without being conceptually a
  // percent metric, and we shouldn't flip it just because it
  // matched a display string.
  const invert = getInvertFill();
  let effectiveValue: number | string = metric.value;
  let effectiveRatio = metric.ratio;
  if (invert && metric.numericUnit === "percent") {
    if (typeof effectiveValue === "number") {
      effectiveValue = Math.max(0, Math.min(100, 100 - effectiveValue));
    }
    if (effectiveRatio !== undefined) {
      effectiveRatio = 1 - effectiveRatio;
    }
  }

  const valueStr =
    typeof effectiveValue === "number"
      ? `${effectiveValue}${metric.unit ?? ""}`
      : effectiveValue;

  const input: Parameters<typeof renderButtonSvg>[0] = { value: valueStr };

  // Label: blank override = provider default; explicit hide = drop it.
  if (!settings.hideLabel) {
    const override = settings.labelOverride?.trim();
    if (override && override.length > 0) {
      input.label = override;
    } else {
      input.label = (metric.label ?? providerName).toUpperCase();
    }
  }

  if (effectiveRatio !== undefined) {
    input.ratio = effectiveRatio;
  }

  if (settings.fillDirection) {
    input.direction = settings.fillDirection;
  } else if (metric.direction !== undefined) {
    input.direction = metric.direction;
  }

  // Fill color priority:
  //   1. critical threshold hit (numericValue ≤ criticalBelow)
  //   2. warn threshold hit (numericValue ≤ warnBelow)
  //   3. user fillColor override
  //   4. provider brand color
  //
  // Thresholds compare against the metric's raw numericValue so
  // the display format ($204.80 / 42% / "OUT") doesn't need to be
  // parsed. If a metric has no numericValue (e.g. ON/OFF), the
  // threshold check is skipped and we fall through to 3/4.
  const thresholdState = computeThresholdState(metric, settings);
  if (thresholdState === "critical") {
    input.fill = settings.criticalColor && /^#[0-9a-fA-F]{3,8}$/.test(settings.criticalColor)
      ? settings.criticalColor
      : "#ef4444";
  } else if (thresholdState === "warn") {
    input.fill = settings.warnColor && /^#[0-9a-fA-F]{3,8}$/.test(settings.warnColor)
      ? settings.warnColor
      : "#f59e0b";
  } else if (settings.fillColor && /^#[0-9a-fA-F]{3,8}$/.test(settings.fillColor)) {
    input.fill = settings.fillColor;
  } else {
    input.fill = provider.brandColor;
  }
  if (settings.bgColor && /^#[0-9a-fA-F]{3,8}$/.test(settings.bgColor)) {
    input.bg = settings.bgColor;
  }
  if (settings.textColor && /^#[0-9a-fA-F]{3,8}$/.test(settings.textColor)) {
    input.fg = settings.textColor;
  }

  // Text sizes: per-key override falls through to the plugin-wide
  // default so one change in Plugin Settings can re-style every
  // button without touching each one.
  input.valueSize = settings.valueSize ?? getDefaultValueSize();
  input.subvalueSize = settings.subvalueSize ?? getDefaultSubvalueSize();
  if (settings.showBorder === false) input.border = false;

  // Provider glyph watermark. Wire the logo through to the renderer
  // whenever both the global and per-key toggles say yes. Without
  // this pass-through the renderer silently skips glyph drawing
  // (see renderButtonSvg — the glyph block is gated on `input.glyph`
  // being truthy), so the logo NEVER appears on a live metric face,
  // only on the loading face. This was the "I can't see any logos
  // on my buttons" bug.
  const wantGlyph =
    getShowGlyphs() && settings.showGlyph !== false;
  if (wantGlyph) {
    const glyph = PROVIDER_ICONS[provider.id];
    if (glyph) {
      input.glyph = glyph;
      input.glyphMode = "watermark";
    }
  } else {
    input.showGlyph = false;
  }

  // Subvalue slot priority:
  //   1. live reset-countdown (`resetInSeconds`) if the user hasn't
  //      hidden it via showResetTimer
  //   2. static caption (e.g. "Prepaid" on BALANCE) as a fallback
  //      label for metrics that have no countdown but want a
  //      descriptive line under the big number
  //
  // This lets the Claude extra-usage-balance tile render
  // "$204.80" + "Prepaid" even though the balance doesn't expire
  // on a timer, without needing a brand-new rendering path.
  if (
    metric.resetInSeconds !== undefined &&
    settings.showResetTimer !== false
  ) {
    input.subvalue = formatCountdown(metric.resetInSeconds);
  } else if (metric.caption && metric.caption.trim().length > 0) {
    input.subvalue = metric.caption;
  }
  if (metric.stale !== undefined) input.stale = metric.stale;
  return renderButtonSvg(input);
}

function formatCountdown(seconds: number): string {
  if (seconds < 60) return `${Math.floor(seconds)}s`;
  const mins = Math.floor(seconds / 60);
  if (mins < 60) return `${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 48) return `${hours}h ${mins % 60}m`;
  const days = Math.floor(hours / 24);
  return `${days}d`;
}

main().catch((err: unknown) => {
  // eslint-disable-next-line no-console
  console.error(`[usage-buttons] fatal: ${String(err)}`);
  process.exit(1);
});

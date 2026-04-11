/**
 * Stream Deck plugin entry point.
 *
 * Compiled into a native binary via `bun build --compile`, dropped
 * into `com.baldwin.usage-buttons.sdPlugin/bin/plugin-<os>`, and
 * launched by the Stream Deck software with registration args.
 */

import { parseArgs, StreamDeckConnection } from "./streamdeck.ts";
import type { InboundEvent, WillAppearEvent } from "./streamdeck.ts";
import { renderButtonSvg } from "./render.ts";
import { getProvider } from "./providers/registry.ts";
import { getSnapshot, setCacheLogSink } from "./providers/cache.ts";
import { setClaudeDebugLogSink } from "./providers/claude.ts";
import { setClaudeWebLogSink } from "./providers/claude-web.ts";
import type { MetricValue } from "./providers/types.ts";
import {
  resolveRefreshMs,
  setGlobalSettings,
  type GlobalSettings,
} from "./settings.ts";

interface KeySettings {
  providerId?: string;
  metricId?: string;
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
  /** Flip the fill ratio (remaining ↔ used). Useful when the metric is "used %". */
  invertFill?: boolean;
  /** Big-number size. Default "large". */
  valueSize?: "small" | "medium" | "large";
  /** Render the outer rounded-rect border. Default true. */
  showBorder?: boolean;
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

const ACTION_UUID = "com.baldwin.usage-buttons.stat";
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
  // %APPDATA%/Elgato/StreamDeck/logs/com.baldwin.usage-buttons*.log.
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
      visibleKeys.set(e.context, {
        context: e.context,
        settings: e.payload.settings as KeySettings,
        lastPollAt: 0,
      });
      // First appearance: fire an immediate cached-or-fetched refresh
      // so the button has data as soon as the user drops it on a key.
      // lastPollAt is updated inside refreshKey on success.
      void refreshKey(conn, e.context, { force: false });
      return;
    }
    case "willDisappear": {
      const ctx = event.context;
      if (ctx) visibleKeys.delete(ctx);
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
    // so the user can tell the button from a hard error.
    const rate = isRateLimit(snapshot.error);
    const errInput: Parameters<typeof renderButtonSvg>[0] = {
      label: provider.name.toUpperCase(),
      value: rate ? "WAIT" : "ERR",
      stale: true,
    };
    if (rate) errInput.subvalue = "rate limit";
    conn.setImage(context, renderButtonSvg(errInput));
    return;
  }

  const metric = snapshot.metrics.find((m) => m.id === metricId);
  if (!metric) {
    conn.setImage(
      context,
      renderButtonSvg({
        label: provider.name.toUpperCase(),
        value: "—",
        subvalue: metricId,
        stale: true,
      }),
    );
    return;
  }
  conn.setImage(
    context,
    renderMetric(snapshot.providerName, metric, key.settings),
  );
}

function isRateLimit(errorMessage: string): boolean {
  return /429|rate.?limit/i.test(errorMessage);
}

function renderMetric(
  providerName: string,
  metric: MetricValue,
  settings: KeySettings,
): string {
  // Inverted view: the user wants "X% used" instead of "X% remaining"
  // (or vice versa). Flip BOTH the displayed number and the fill
  // ratio together — flipping just the ratio would render a 2% bar
  // with "98%" text, which is nonsense.
  //
  // We only flip the numeric value when the metric looks like a
  // percentage (unit === "%"). For dollar or count metrics we can't
  // sensibly compute a complement — those still flip the ratio only.
  let effectiveValue: number | string = metric.value;
  let effectiveRatio = metric.ratio;
  if (settings.invertFill) {
    if (typeof effectiveValue === "number" && metric.unit === "%") {
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

  if (settings.fillColor && /^#[0-9a-fA-F]{3,8}$/.test(settings.fillColor)) {
    input.fill = settings.fillColor;
  }
  if (settings.bgColor && /^#[0-9a-fA-F]{3,8}$/.test(settings.bgColor)) {
    input.bg = settings.bgColor;
  }
  if (settings.textColor && /^#[0-9a-fA-F]{3,8}$/.test(settings.textColor)) {
    input.fg = settings.textColor;
  }

  if (settings.valueSize) input.valueSize = settings.valueSize;
  if (settings.showBorder === false) input.border = false;

  // Reset-countdown subvalue — user can hide it (e.g. they prefer
  // maximum value-text space on a "minimal" button style).
  if (
    metric.resetInSeconds !== undefined &&
    settings.showResetTimer !== false
  ) {
    input.subvalue = formatCountdown(metric.resetInSeconds);
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

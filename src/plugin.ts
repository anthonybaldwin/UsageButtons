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
import { getSnapshot } from "./providers/cache.ts";
import type { MetricValue } from "./providers/types.ts";

interface KeySettings {
  providerId?: string;
  metricId?: string;
  /** Optional override for the label rendered inside the SVG. Blank = use the metric's default. */
  labelOverride?: string;
  /** Hide the inner SVG label entirely (e.g. when using the Stream Deck native title). */
  hideLabel?: boolean;
  /** Fill color hex, e.g. "#10b981". Blank = metric default. */
  fillColor?: string;
  /** Background color hex. Blank = dark default. */
  bgColor?: string;
  /** Direction the fill grows in as the value climbs toward 100%. */
  fillDirection?: "up" | "down" | "right" | "left";
  /** Flip the fill ratio (remaining ↔ used). Useful when the metric is "used %". */
  invertFill?: boolean;
}

interface VisibleKey {
  context: string;
  settings: KeySettings;
}

const ACTION_UUID = "com.baldwin.usage-buttons.stat";
const DEFAULT_PROVIDER = "mock";
const DEFAULT_METRIC = "session-percent";
/**
 * How often the plugin's own poll loop ticks. The per-provider
 * cache in `src/providers/cache.ts` enforces a longer TTL on top of
 * this, so the actual upstream HTTP rate is gated by whichever is
 * more conservative. We poll more often here so that click-refresh
 * and settings changes feel snappy, while the cache keeps the
 * real upstream rate sane.
 */
const POLL_INTERVAL_MS = 15_000;

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

  connection.onEvent((event) => handleEvent(connection, event));

  // Start the poll loop. We refresh all visible keys together so a
  // single provider fetch can fan out to multiple keys bound to the
  // same stat.
  setInterval(() => void refreshAllKeys(connection), POLL_INTERVAL_MS);
}

function handleEvent(conn: StreamDeckConnection, event: InboundEvent): void {
  switch (event.event) {
    case "willAppear": {
      const e = event as WillAppearEvent;
      if (e.action !== ACTION_UUID) return;
      visibleKeys.set(e.context, {
        context: e.context,
        settings: e.payload.settings as KeySettings,
      });
      // First appearance: use the cached snapshot if one exists so
      // we render instantly without hitting the upstream. The poll
      // loop will refresh on its own cadence.
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
      // snapshot — same provider, just a different metric / color.
      void refreshKey(conn, ctx, { force: false });
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

async function refreshAllKeys(conn: StreamDeckConnection): Promise<void> {
  await Promise.all(
    [...visibleKeys.keys()].map((ctx) => refreshKey(conn, ctx, { force: false })),
  );
}

async function refreshKey(
  conn: StreamDeckConnection,
  context: string,
  opts: { force: boolean },
): Promise<void> {
  const key = visibleKeys.get(context);
  if (!key) return;

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
  const valueStr =
    typeof metric.value === "number"
      ? `${metric.value}${metric.unit ?? ""}`
      : metric.value;

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

  // Ratio: apply invertFill flip if the user asked for "used" display.
  if (metric.ratio !== undefined) {
    input.ratio = settings.invertFill ? 1 - metric.ratio : metric.ratio;
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

  if (metric.resetInSeconds !== undefined) {
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

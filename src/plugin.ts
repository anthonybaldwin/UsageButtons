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
const POLL_INTERVAL_MS = 10_000;

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
      void refreshKey(conn, e.context);
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
      void refreshKey(conn, ctx);
      return;
    }
    case "keyDown": {
      // For now, a key-press forces an immediate refresh.
      const ctx = event.context;
      if (ctx) void refreshKey(conn, ctx);
      return;
    }
    default:
      return;
  }
}

async function refreshAllKeys(conn: StreamDeckConnection): Promise<void> {
  await Promise.all(
    [...visibleKeys.keys()].map((ctx) => refreshKey(conn, ctx)),
  );
}

async function refreshKey(
  conn: StreamDeckConnection,
  context: string,
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

  try {
    const snapshot = await provider.fetch({ pollIntervalMs: POLL_INTERVAL_MS });
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
    conn.setImage(context, renderMetric(provider.name, metric, key.settings));
  } catch (err) {
    conn.log(`fetch failed ${providerId}/${metricId}: ${String(err)}`);
    conn.setImage(
      context,
      renderButtonSvg({
        label: provider.name.toUpperCase(),
        value: "ERR",
        stale: true,
      }),
    );
  }
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

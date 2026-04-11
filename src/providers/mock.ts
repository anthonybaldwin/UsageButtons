/**
 * Mock provider — produces deterministic-ish stats for wiring up the
 * plugin end-to-end before any real fetcher is built.
 *
 * The values drift slowly over time so the button fill visibly
 * animates during development.
 */

import type { MetricValue, Provider, ProviderSnapshot } from "./types.ts";

export class MockProvider implements Provider {
  readonly id = "mock";
  readonly name = "Mock";
  readonly brandColor = "#3b82f6"; // neutral dev-blue; not a CodexBar-branded hue
  readonly metricIds = [
    "session-percent",
    "weekly-percent",
    "credits",
  ] as const;

  async fetch(): Promise<ProviderSnapshot> {
    const now = Date.now();
    // 10-minute sine sweep so the button is obviously animating.
    const t = (now / 1000 / 60 / 10) * Math.PI * 2;
    const session = 50 + Math.sin(t) * 45;
    const weekly = 50 + Math.sin(t * 0.3) * 40;
    const credits = 250 + Math.cos(t) * 100;

    const metrics: MetricValue[] = [
      {
        id: "session-percent",
        label: "SESSION",
        name: "Session window remaining",
        value: Math.round(session),
        unit: "%",
        ratio: session / 100,
        direction: "up",
        resetInSeconds: 60 * 60 * 3,
        updatedAt: new Date(now),
      },
      {
        id: "weekly-percent",
        label: "WEEKLY",
        name: "Weekly window remaining",
        value: Math.round(weekly),
        unit: "%",
        ratio: weekly / 100,
        direction: "up",
        resetInSeconds: 60 * 60 * 24 * 4,
        updatedAt: new Date(now),
      },
      {
        id: "credits",
        label: "CREDITS",
        name: "Credits remaining",
        value: Math.round(credits),
        ratio: credits / 400,
        direction: "up",
        updatedAt: new Date(now),
      },
    ];

    return {
      providerId: this.id,
      providerName: this.name,
      source: "mock",
      metrics,
      status: "operational",
    };
  }
}

/**
 * Kimi K2 credits provider.
 *
 * Auth: KIMI_K2_API_KEY (or KIMI_API_KEY / KIMI_KEY) environment variable.
 * Endpoint: GET https://kimi-k2.ai/api/user/credits
 * Returns: credit balance with flexible response parsing.
 *
 * Reference: Win-CodexBar rust/src/providers/kimi_k2/
 */

import { httpJson, HttpError } from "../util/http.ts";
import { CODEXBAR_BRAND_COLORS } from "./brand-colors.ts";
import type {
  MetricValue,
  Provider,
  ProviderContext,
  ProviderSnapshot,
} from "./types.ts";

const CREDITS_URL = "https://kimi-k2.ai/api/user/credits";

function getApiKey(): string | undefined {
  return (
    process.env["KIMI_K2_API_KEY"] ??
    process.env["KIMI_API_KEY"] ??
    process.env["KIMI_KEY"]
  )?.trim() || undefined;
}

/**
 * Flexible response parser — the Kimi K2 API response shape varies.
 * We search multiple possible field paths for consumed and remaining
 * credits, matching Win-CodexBar's lenient approach.
 */
function extractCredits(body: Record<string, unknown>): {
  consumed: number | undefined;
  remaining: number | undefined;
} {
  const consumedPaths = [
    ["total_credits_consumed"],
    ["totalCreditsConsumed"],
    ["credits_consumed"],
    ["creditsConsumed"],
    ["consumedCredits"],
    ["usedCredits"],
    ["total"],
    ["usage", "total"],
    ["usage", "consumed"],
    ["data", "usage", "total_credits_consumed"],
    ["data", "total_credits_consumed"],
  ];

  const remainingPaths = [
    ["credits_remaining"],
    ["creditsRemaining"],
    ["remaining_credits"],
    ["remainingCredits"],
    ["available_credits"],
    ["availableCredits"],
    ["credits_left"],
    ["creditsLeft"],
    ["usage", "credits_remaining"],
    ["usage", "remaining"],
    ["data", "usage", "credits_remaining"],
    ["data", "credits_remaining"],
  ];

  function dig(obj: unknown, path: string[]): number | undefined {
    let current: unknown = obj;
    for (const key of path) {
      if (current == null || typeof current !== "object") return undefined;
      current = (current as Record<string, unknown>)[key];
    }
    if (typeof current === "number") return current;
    if (typeof current === "string") {
      const n = Number.parseFloat(current);
      return Number.isFinite(n) ? n : undefined;
    }
    return undefined;
  }

  let consumed: number | undefined;
  for (const path of consumedPaths) {
    consumed = dig(body, path);
    if (consumed !== undefined) break;
  }

  let remaining: number | undefined;
  for (const path of remainingPaths) {
    remaining = dig(body, path);
    if (remaining !== undefined) break;
  }

  return { consumed, remaining };
}

export class KimiK2Provider implements Provider {
  readonly id = "kimi-k2";
  readonly name = "Kimi K2";
  readonly brandColor = CODEXBAR_BRAND_COLORS.kimiK2 ?? "#ff5722";
  readonly metricIds = ["credits-balance"] as const;

  async fetch(_ctx?: ProviderContext): Promise<ProviderSnapshot> {
    const apiKey = getApiKey();
    if (!apiKey) {
      return {
        providerId: this.id,
        providerName: this.name,
        source: "none",
        metrics: [],
        status: "unknown",
        error: "Set KIMI_K2_API_KEY environment variable.",
      };
    }

    let body: Record<string, unknown>;
    try {
      body = await httpJson<Record<string, unknown>>({
        url: CREDITS_URL,
        headers: {
          authorization: `Bearer ${apiKey}`,
          accept: "application/json",
        },
        timeoutMs: 15_000,
      });
    } catch (err) {
      if (err instanceof HttpError && (err.status === 401 || err.status === 403)) {
        return {
          providerId: this.id,
          providerName: this.name,
          source: "api-key",
          metrics: [],
          status: "unknown",
          error: "Kimi K2 API key unauthorized. Check KIMI_K2_API_KEY.",
        };
      }
      throw err;
    }

    const { consumed, remaining } = extractCredits(body);
    const metrics: MetricValue[] = [];
    const now = new Date();

    if (consumed !== undefined || remaining !== undefined) {
      const c = consumed ?? 0;
      const r = remaining ?? 0;
      const total = c + r;

      if (total > 0) {
        const remainPct = (r / total) * 100;
        metrics.push({
          id: "credits-balance",
          label: "CREDITS",
          name: "Kimi K2 credits remaining",
          value: Math.round(remainPct),
          numericValue: remainPct,
          numericUnit: "percent",
          unit: "%",
          ratio: remainPct / 100,
          direction: "up",
          caption: `${Math.round(r)}/${Math.round(total)}`,
          updatedAt: now,
        });
      } else if (r > 0) {
        metrics.push({
          id: "credits-balance",
          label: "CREDITS",
          name: "Kimi K2 credits",
          value: `${Math.round(r)}`,
          numericValue: r,
          numericUnit: "count",
          numericGoodWhen: "high",
          // Reference card — no ratio.
          caption: "Available",
          updatedAt: now,
        });
      }
    }

    return {
      providerId: this.id,
      providerName: this.name,
      source: "api-key",
      metrics,
      status: "operational",
    };
  }
}

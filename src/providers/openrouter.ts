/**
 * OpenRouter API provider.
 *
 * Auth: OPENROUTER_API_KEY environment variable or settings.
 * Endpoint: GET https://openrouter.ai/api/v1/auth/credits
 * Returns: total_credits, total_usage → we display balance.
 *
 * Reference: tmp/CodexBar docs + Win-CodexBar rust/src/providers/openrouter/
 */

import { httpJson, HttpError } from "../util/http.ts";
import { CODEXBAR_BRAND_COLORS } from "./brand-colors.ts";
import type {
  MetricValue,
  Provider,
  ProviderContext,
  ProviderSnapshot,
} from "./types.ts";

const CREDITS_URL = "https://openrouter.ai/api/v1/auth/credits";

interface CreditsResponse {
  data?: {
    total_credits?: number;
    total_usage?: number;
  };
}

function getApiKey(): string | undefined {
  return process.env["OPENROUTER_API_KEY"]?.trim() || undefined;
}

export class OpenRouterProvider implements Provider {
  readonly id = "openrouter";
  readonly name = "OpenRouter";
  readonly brandColor = CODEXBAR_BRAND_COLORS.openRouter ?? "#6467f2";
  readonly metricIds = ["credits-balance", "credits-used"] as const;

  async fetch(_ctx?: ProviderContext): Promise<ProviderSnapshot> {
    const apiKey = getApiKey();
    if (!apiKey) {
      return {
        providerId: this.id,
        providerName: this.name,
        source: "none",
        metrics: [],
        status: "unknown",
        error: "Set OPENROUTER_API_KEY environment variable.",
      };
    }

    const res = await httpJson<CreditsResponse>({
      url: CREDITS_URL,
      headers: {
        authorization: `Bearer ${apiKey}`,
        accept: "application/json",
      },
      timeoutMs: 15_000,
    });

    const totalCredits = res.data?.total_credits ?? 0;
    const totalUsage = res.data?.total_usage ?? 0;
    const balance = totalCredits - totalUsage;
    const now = new Date();
    const metrics: MetricValue[] = [];

    metrics.push({
      id: "credits-balance",
      label: "CREDITS",
      name: "OpenRouter credit balance",
      value: `$${balance.toFixed(2)}`,
      numericValue: balance,
      numericUnit: "dollars",
      numericGoodWhen: "high",
      // Reference card — no ratio.
      caption: "Balance",
      updatedAt: now,
    });

    if (totalUsage > 0) {
      metrics.push({
        id: "credits-used",
        label: "USED",
        name: "OpenRouter total usage",
        value: `$${totalUsage.toFixed(2)}`,
        numericValue: totalUsage,
        numericUnit: "dollars",
        numericGoodWhen: "low",
        numericMax: totalCredits > 0 ? totalCredits : undefined,
        // Real meter when totalCredits is known; reference card otherwise.
        ...(totalCredits > 0
          ? { ratio: Math.min(1, totalUsage / totalCredits), direction: "up" as const }
          : {}),
        caption: "Lifetime",
        updatedAt: now,
      });
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

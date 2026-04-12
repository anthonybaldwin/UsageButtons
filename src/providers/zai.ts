/**
 * z.ai usage provider.
 *
 * Auth: ZAI_API_TOKEN environment variable.
 * Endpoint: GET https://api.z.ai/api/monitor/usage/quota/limit
 * Returns: quota limits with tokens + MCP usage.
 *
 * Reference: Win-CodexBar rust/src/providers/zai/
 */

import { httpJson } from "../util/http.ts";
import { CODEXBAR_BRAND_COLORS } from "./brand-colors.ts";
import type {
  MetricValue,
  Provider,
  ProviderContext,
  ProviderSnapshot,
} from "./types.ts";

const QUOTA_URL = "https://api.z.ai/api/monitor/usage/quota/limit";

interface QuotaLimit {
  type?: string;
  used?: number;
  limit?: number;
  resetAt?: string;
  // Extended fields from the detailed response
  unit?: number;       // 1=Days, 3=Hours, 5=Minutes
  number?: number;     // multiplier for unit
  usage?: number;
  currentValue?: number;
  remaining?: number;
  percentage?: number;
  nextResetTime?: number; // epoch ms
}

interface QuotaResponse {
  limits?: QuotaLimit[];
  data?: {
    limits?: QuotaLimit[];
    plan_name?: string;
    plan?: string;
    plan_type?: string;
  };
}

function getApiToken(): string | undefined {
  return (process.env["ZAI_API_TOKEN"] ?? process.env["ZAI_API_KEY"])?.trim() || undefined;
}

function windowSeconds(limit: QuotaLimit): number | undefined {
  if (!limit.unit || !limit.number) return undefined;
  const multiplier = limit.number;
  switch (limit.unit) {
    case 1: return multiplier * 24 * 60 * 60; // days
    case 3: return multiplier * 60 * 60;       // hours
    case 5: return multiplier * 60;            // minutes
    default: return undefined;
  }
}

function resetSecondsFromLimit(limit: QuotaLimit): number | undefined {
  // Try nextResetTime (epoch ms) first
  if (typeof limit.nextResetTime === "number") {
    const delta = Math.floor((limit.nextResetTime - Date.now()) / 1000);
    return delta > 0 ? delta : 0;
  }
  // Fall back to resetAt (ISO string)
  if (limit.resetAt) {
    const d = new Date(limit.resetAt);
    if (!Number.isNaN(d.getTime())) {
      const delta = Math.floor((d.getTime() - Date.now()) / 1000);
      return delta > 0 ? delta : 0;
    }
  }
  return undefined;
}

export class ZaiProvider implements Provider {
  readonly id = "zai";
  readonly name = "z.ai";
  readonly brandColor = CODEXBAR_BRAND_COLORS.zai ?? "#ff6b35";
  readonly metricIds = ["tokens-percent", "mcp-percent"] as const;

  async fetch(_ctx?: ProviderContext): Promise<ProviderSnapshot> {
    const apiToken = getApiToken();
    if (!apiToken) {
      return {
        providerId: this.id,
        providerName: this.name,
        source: "none",
        metrics: [],
        status: "unknown",
        error: "Set ZAI_API_TOKEN environment variable.",
      };
    }

    const res = await httpJson<QuotaResponse>({
      url: QUOTA_URL,
      headers: {
        authorization: `Bearer ${apiToken}`,
        accept: "application/json",
      },
      timeoutMs: 15_000,
    });

    // Limits can be at root or nested under data
    const limits = res.limits ?? res.data?.limits ?? [];
    const planName = res.data?.plan_name ?? res.data?.plan ?? res.data?.plan_type;
    const metrics: MetricValue[] = [];
    const now = new Date();

    for (const limit of limits) {
      const type = (limit.type ?? "").toLowerCase();
      const isTokens = type.includes("token");
      const isMcp = type.includes("mcp") || type.includes("time");

      const used = limit.used ?? limit.usage ?? limit.currentValue ?? 0;
      const cap = limit.limit ?? 0;
      if (cap <= 0) continue;

      const usedPct = Math.min(100, (used / cap) * 100);
      const remainPct = 100 - usedPct;
      const resetSecs = resetSecondsFromLimit(limit);

      const remaining = cap - used;
      const m: MetricValue = {
        id: isTokens ? "tokens-percent" : isMcp ? "mcp-percent" : `${type}-percent`,
        label: isTokens ? "TOKENS" : isMcp ? "MCP" : type.toUpperCase(),
        name: `${isTokens ? "Token" : isMcp ? "MCP" : type} usage remaining`,
        value: Math.round(remainPct),
        numericValue: remainPct,
        numericUnit: "percent",
        unit: "%",
        ratio: remainPct / 100,
        direction: "up",
        rawCount: remaining,
        rawMax: cap,
        updatedAt: now,
      };
      if (resetSecs !== undefined) m.resetInSeconds = resetSecs;

      if (isTokens) {
        metrics.unshift(m); // tokens first
      } else {
        metrics.push(m);
      }
    }

    return {
      providerId: this.id,
      providerName: planName ? `z.ai ${planName}` : this.name,
      source: "api-key",
      metrics,
      status: "operational",
    };
  }
}

/**
 * Cursor usage provider.
 *
 * Auth: Browser cookie pasted from cursor.com DevTools. Accepts any
 *       of the session cookie formats Cursor uses (WorkosCursorSessionToken,
 *       __Secure-next-auth.session-token, wos-session, etc.).
 *
 * Endpoint: GET https://cursor.com/api/usage-summary
 * Returns: plan usage (total/auto/api percent), on-demand spend.
 *
 * Reference: tmp/CodexBar Sources/CodexBarCore/Providers/Cursor/
 */

import { httpJson, HttpError } from "../util/http.ts";
import { getCursorSettings } from "../settings.ts";
import { CODEXBAR_BRAND_COLORS } from "./brand-colors.ts";
import type {
  MetricValue,
  Provider,
  ProviderContext,
  ProviderSnapshot,
} from "./types.ts";

const USAGE_SUMMARY_URL = "https://cursor.com/api/usage-summary";

interface PlanUsage {
  enabled?: boolean;
  used?: number;       // cents
  limit?: number;      // cents
  remaining?: number;  // cents
  breakdown?: {
    included?: number;
    bonus?: number;
    total?: number;
  };
  totalPercentUsed?: number;
  autoPercentUsed?: number;
  apiPercentUsed?: number;
}

interface OnDemandUsage {
  enabled?: boolean;
  used?: number;      // cents
  limit?: number;     // cents
  remaining?: number; // cents
}

interface UsageSummaryResponse {
  billingCycleStart?: string;
  billingCycleEnd?: string;
  membershipType?: string;
  individualUsage?: {
    plan?: PlanUsage;
    onDemand?: OnDemandUsage;
  };
}

function resetFromCycleEnd(cycleEnd: string | undefined): number | undefined {
  if (!cycleEnd) return undefined;
  const d = new Date(cycleEnd);
  if (Number.isNaN(d.getTime())) return undefined;
  const delta = Math.floor((d.getTime() - Date.now()) / 1000);
  return delta > 0 ? delta : 0;
}

export class CursorProvider implements Provider {
  readonly id = "cursor";
  readonly name = "Cursor";
  readonly brandColor = CODEXBAR_BRAND_COLORS.cursor ?? "#00d4aa";
  readonly metricIds = [
    "total-percent",
    "auto-percent",
    "api-percent",
    "ondemand-spent",
  ] as const;

  async fetch(_ctx?: ProviderContext): Promise<ProviderSnapshot> {
    const cs = getCursorSettings();
    if (!cs.cookieHeader) {
      return {
        providerId: this.id,
        providerName: this.name,
        source: "none",
        metrics: [],
        status: "unknown",
        error: "Paste a Cookie header from cursor.com in Plugin Settings.",
      };
    }

    let response: UsageSummaryResponse;
    try {
      response = await httpJson<UsageSummaryResponse>({
        url: USAGE_SUMMARY_URL,
        headers: {
          cookie: cs.cookieHeader,
          accept: "application/json",
        },
        timeoutMs: 15_000,
      });
    } catch (err) {
      if (err instanceof HttpError && (err.status === 401 || err.status === 403)) {
        return {
          providerId: this.id,
          providerName: this.name,
          source: "cookie",
          metrics: [],
          status: "unknown",
          error: "Cursor cookie expired. Paste a fresh one from cursor.com.",
        };
      }
      throw err;
    }

    const metrics: MetricValue[] = [];
    const now = new Date();
    const plan = response.individualUsage?.plan;
    const resetSecs = resetFromCycleEnd(response.billingCycleEnd);

    if (plan) {
      // Total plan usage
      if (typeof plan.totalPercentUsed === "number") {
        const remaining = 100 - plan.totalPercentUsed;
        const m: MetricValue = {
          id: "total-percent",
          label: "TOTAL",
          name: "Total plan usage remaining",
          value: Math.round(remaining),
          numericValue: remaining,
          numericUnit: "percent",
          unit: "%",
          ratio: remaining / 100,
          direction: "up",
          updatedAt: now,
        };
        if (resetSecs !== undefined) m.resetInSeconds = resetSecs;
        metrics.push(m);
      }

      // Auto / Composer usage
      if (typeof plan.autoPercentUsed === "number") {
        const remaining = 100 - plan.autoPercentUsed;
        const m: MetricValue = {
          id: "auto-percent",
          label: "AUTO",
          name: "Auto usage remaining",
          value: Math.round(remaining),
          numericValue: remaining,
          numericUnit: "percent",
          unit: "%",
          ratio: remaining / 100,
          direction: "up",
          updatedAt: now,
        };
        if (resetSecs !== undefined) m.resetInSeconds = resetSecs;
        metrics.push(m);
      }

      // API / Named model usage
      if (typeof plan.apiPercentUsed === "number") {
        const remaining = 100 - plan.apiPercentUsed;
        const m: MetricValue = {
          id: "api-percent",
          label: "API",
          name: "API usage remaining",
          value: Math.round(remaining),
          numericValue: remaining,
          numericUnit: "percent",
          unit: "%",
          ratio: remaining / 100,
          direction: "up",
          updatedAt: now,
        };
        if (resetSecs !== undefined) m.resetInSeconds = resetSecs;
        metrics.push(m);
      }
    }

    // On-demand spend
    const onDemand = response.individualUsage?.onDemand;
    if (onDemand?.enabled && typeof onDemand.used === "number") {
      const spentDollars = onDemand.used / 100;
      const limitDollars = typeof onDemand.limit === "number" ? onDemand.limit / 100 : undefined;
      metrics.push({
        id: "ondemand-spent",
        label: "ON-DEMAND",
        name: "On-demand spend",
        value: `$${spentDollars.toFixed(2)}`,
        numericValue: spentDollars,
        numericUnit: "dollars",
        numericGoodWhen: "low",
        numericMax: limitDollars,
        // Real meter when a spending limit is set; reference card otherwise.
        ...(limitDollars
          ? { ratio: Math.min(1, spentDollars / limitDollars), direction: "up" as const }
          : {}),
        caption: limitDollars ? `of $${limitDollars.toFixed(0)}` : "Unlimited",
        updatedAt: now,
      });
    }

    const planLabel = response.membershipType
      ? `Cursor ${response.membershipType.charAt(0).toUpperCase()}${response.membershipType.slice(1)}`
      : this.name;

    return {
      providerId: this.id,
      providerName: planLabel,
      source: "cookie",
      metrics,
      status: "operational",
    };
  }
}

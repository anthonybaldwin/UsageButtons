/**
 * Warp AI usage provider.
 *
 * Auth: WARP_API_KEY environment variable.
 * Endpoint: POST https://app.warp.dev/graphql/v2?op=GetRequestLimitInfo
 * Returns: request usage + bonus credits via GraphQL.
 *
 * Reference: Win-CodexBar rust/src/providers/warp/
 */

import { httpJson, HttpError } from "../util/http.ts";
import { CODEXBAR_BRAND_COLORS } from "./brand-colors.ts";
import type {
  MetricValue,
  Provider,
  ProviderContext,
  ProviderSnapshot,
} from "./types.ts";

const GRAPHQL_URL = "https://app.warp.dev/graphql/v2?op=GetRequestLimitInfo";

const GRAPHQL_QUERY = `
query GetRequestLimitInfo($requestContext: RequestContext!) {
  user(requestContext: $requestContext) {
    ... on AuthenticatedUser {
      user {
        requestLimitInfo {
          isUnlimited
          nextRefreshTime
          requestLimit
          requestsUsedSinceLastRefresh
        }
        bonusGrants {
          requestCreditsGranted
          requestCreditsRemaining
        }
      }
    }
  }
}`;

interface RequestLimitInfo {
  isUnlimited?: boolean;
  nextRefreshTime?: string;
  requestLimit?: number;
  requestsUsedSinceLastRefresh?: number;
}

interface BonusGrant {
  requestCreditsGranted?: number;
  requestCreditsRemaining?: number;
}

interface GraphQLResponse {
  data?: {
    user?: {
      user?: {
        requestLimitInfo?: RequestLimitInfo;
        bonusGrants?: BonusGrant[];
      };
    };
  };
  errors?: Array<{ message: string }>;
}

function getApiKey(): string | undefined {
  return process.env["WARP_API_KEY"]?.trim() || undefined;
}

export class WarpProvider implements Provider {
  readonly id = "warp";
  readonly name = "Warp";
  readonly brandColor = CODEXBAR_BRAND_COLORS.warp ?? "#01a4ff";
  readonly metricIds = ["credits-percent", "bonus-credits"] as const;

  async fetch(_ctx?: ProviderContext): Promise<ProviderSnapshot> {
    const apiKey = getApiKey();
    if (!apiKey) {
      return {
        providerId: this.id,
        providerName: this.name,
        source: "none",
        metrics: [],
        status: "unknown",
        error: "Set WARP_API_KEY environment variable.",
      };
    }

    const res = await httpJson<GraphQLResponse>({
      url: GRAPHQL_URL,
      method: "POST",
      headers: {
        authorization: `Bearer ${apiKey}`,
        "content-type": "application/json",
        "x-warp-client-id": "warp-app",
        "user-agent": "Warp/1.0",
      },
      json: {
        query: GRAPHQL_QUERY,
        variables: {
          requestContext: {
            clientContext: { clientType: "DESKTOP" },
            osContext: { osName: "Windows", osCategory: "DESKTOP" },
          },
        },
      },
      timeoutMs: 15_000,
    });

    if (res.errors?.length) {
      return {
        providerId: this.id,
        providerName: this.name,
        source: "api-key",
        metrics: [],
        status: "unknown",
        error: `Warp GraphQL error: ${res.errors[0]!.message}`,
      };
    }

    const info = res.data?.user?.user?.requestLimitInfo;
    const grants = res.data?.user?.user?.bonusGrants;
    const metrics: MetricValue[] = [];
    const now = new Date();

    if (info) {
      if (info.isUnlimited) {
        metrics.push({
          id: "credits-percent",
          label: "CREDITS",
          name: "Warp credits (unlimited)",
          value: "∞",
          // Reference card — no ratio.
          caption: "Unlimited",
          updatedAt: now,
        });
      } else if (typeof info.requestLimit === "number" && info.requestLimit > 0) {
        const used = info.requestsUsedSinceLastRefresh ?? 0;
        const remaining = Math.max(0, info.requestLimit - used);
        const remainingPct = (remaining / info.requestLimit) * 100;

        let resetSecs: number | undefined;
        if (info.nextRefreshTime) {
          const d = new Date(info.nextRefreshTime);
          if (!Number.isNaN(d.getTime())) {
            const delta = Math.floor((d.getTime() - Date.now()) / 1000);
            if (delta > 0) resetSecs = delta;
          }
        }

        const m: MetricValue = {
          id: "credits-percent",
          label: "CREDITS",
          name: "Warp credits remaining",
          value: Math.round(remainingPct),
          numericValue: remainingPct,
          numericUnit: "percent",
          unit: "%",
          ratio: remainingPct / 100,
          direction: "up",
          rawCount: remaining,
          rawMax: info.requestLimit,
          updatedAt: now,
        };
        if (resetSecs !== undefined) m.resetInSeconds = resetSecs;
        metrics.push(m);
      }
    }

    // Bonus credits — aggregate all grants
    if (grants && grants.length > 0) {
      let totalGranted = 0;
      let totalRemaining = 0;
      for (const g of grants) {
        totalGranted += g.requestCreditsGranted ?? 0;
        totalRemaining += g.requestCreditsRemaining ?? 0;
      }
      if (totalGranted > 0) {
        const usedPct = ((totalGranted - totalRemaining) / totalGranted) * 100;
        const remainPct = 100 - usedPct;
        metrics.push({
          id: "bonus-credits",
          label: "BONUS",
          name: "Warp bonus credits remaining",
          value: Math.round(remainPct),
          numericValue: remainPct,
          numericUnit: "percent",
          unit: "%",
          ratio: remainPct / 100,
          direction: "up",
          rawCount: totalRemaining,
          rawMax: totalGranted,
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

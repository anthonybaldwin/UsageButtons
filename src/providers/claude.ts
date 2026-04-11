/**
 * Claude OAuth API usage provider.
 *
 * Port of CodexBar's `ClaudeOAuth/` Swift implementation, Windows-first
 * (we read the `~/.claude/.credentials.json` file fallback directly
 * rather than going through macOS Keychain). Reference:
 *
 *   tmp/CodexBar/Sources/CodexBarCore/Providers/Claude/ClaudeOAuth/
 *     ClaudeOAuthCredentials.swift    — credential file shape
 *     ClaudeOAuthUsageFetcher.swift   — HTTP request + response decode
 *   tmp/CodexBar/Sources/CodexBarCore/Providers/Claude/ClaudePlan.swift
 *   docs/claude.md
 *
 * Credential file contract (from CodexBar + CLAUDE.md):
 *
 *   {
 *     "claudeAiOauth": {
 *       "accessToken":  "sk-ant-oat-…",     // required
 *       "refreshToken": "…",                 // optional
 *       "expiresAt":    1767400000000,       // MILLISECONDS since epoch
 *       "scopes":       ["user:profile"],   // need user:profile for /usage
 *       "rateLimitTier": "default_claude_pro"
 *     }
 *   }
 *
 * Usage API contract:
 *
 *   GET https://api.anthropic.com/api/oauth/usage
 *   Authorization: Bearer <accessToken>
 *   anthropic-beta: oauth-2025-04-20          <-- required, exact string
 *   User-Agent: claude-code/<version>
 *
 *   200 → {
 *     "five_hour":            { "utilization": 12.5, "resets_at": "…Z" },
 *     "seven_day":            { "utilization": 30,   "resets_at": "…Z" },
 *     "seven_day_sonnet":     { "utilization": 5,    "resets_at": "…Z" },
 *     "seven_day_opus":       { "utilization": 80,   "resets_at": "…Z" },
 *     "seven_day_oauth_apps": { "utilization": 0 },
 *     "extra_usage": {
 *       "is_enabled":    true,
 *       "monthly_limit": 2050,   // cents
 *       "used_credits":  325,    // cents
 *       "utilization":   15.85,
 *       "currency":      "USD"
 *     }
 *   }
 *
 * `utilization` is **percent used** (0..100). We invert to "percent
 * remaining" for the button so the classic "tank of gas" fill feels
 * right — the button drains as you use quota.
 *
 * This first cut does NOT implement token refresh. If the access
 * token is expired, we surface a clear "run `claude` to
 * re-authenticate" error and mark the metric stale. Refresh is a
 * follow-up (`platform.claude.com/v1/oauth/token`, form-urlencoded,
 * client_id `9d1c250a-e61b-44d9-88ed-5944d1962f5e`).
 */

import { claudeCredentialsPath } from "../util/paths.ts";
import {
  readJsonCredential,
  CredentialNotFoundError,
} from "../util/credentials.ts";
import { httpJson, HttpError } from "../util/http.ts";
import type {
  MetricValue,
  Provider,
  ProviderSnapshot,
} from "./types.ts";

const USAGE_URL = "https://api.anthropic.com/api/oauth/usage";
const BETA_HEADER = "oauth-2025-04-20";
const USER_AGENT = "claude-code/2.1.70";

/** Raw credential file shape — see doc comment above. */
interface ClaudeCredentialsFile {
  claudeAiOauth?: {
    accessToken?: string;
    refreshToken?: string | null;
    expiresAt?: number | null; // milliseconds since epoch
    scopes?: string[] | null;
    rateLimitTier?: string | null;
  };
}

interface ClaudeCredentials {
  accessToken: string;
  refreshToken?: string;
  expiresAt?: Date;
  scopes: string[];
  rateLimitTier?: string;
}

/** Raw API response shape — each window's `utilization` is "percent used". */
interface UsageWindow {
  utilization?: number | null;
  resets_at?: string | null;
}

interface ExtraUsage {
  is_enabled?: boolean | null;
  monthly_limit?: number | null; // cents
  used_credits?: number | null; // cents
  utilization?: number | null;
  currency?: string | null;
}

interface UsageResponse {
  five_hour?: UsageWindow | null;
  seven_day?: UsageWindow | null;
  seven_day_sonnet?: UsageWindow | null;
  seven_day_opus?: UsageWindow | null;
  seven_day_oauth_apps?: UsageWindow | null;
  extra_usage?: ExtraUsage | null;
}

export class ClaudeOAuthError extends Error {
  constructor(
    message: string,
    public readonly kind:
      | "not-configured"
      | "expired"
      | "scope-missing"
      | "unauthorized"
      | "server"
      | "network",
  ) {
    super(message);
    this.name = "ClaudeOAuthError";
  }
}

async function loadCredentials(): Promise<ClaudeCredentials> {
  const path = claudeCredentialsPath();
  let raw: ClaudeCredentialsFile;
  try {
    raw = await readJsonCredential<ClaudeCredentialsFile>(path);
  } catch (err) {
    if (err instanceof CredentialNotFoundError) {
      throw new ClaudeOAuthError(
        `Claude credentials not found at ${path}. Run \`claude\` in a terminal to sign in.`,
        "not-configured",
      );
    }
    throw err;
  }

  const oauth = raw.claudeAiOauth;
  if (!oauth || !oauth.accessToken || oauth.accessToken.trim() === "") {
    throw new ClaudeOAuthError(
      `Claude credentials at ${path} missing claudeAiOauth.accessToken.`,
      "not-configured",
    );
  }

  const creds: ClaudeCredentials = {
    accessToken: oauth.accessToken.trim(),
    scopes: oauth.scopes && oauth.scopes.length > 0 ? oauth.scopes : ["user:profile"],
  };
  if (oauth.refreshToken) creds.refreshToken = oauth.refreshToken;
  if (typeof oauth.expiresAt === "number") {
    creds.expiresAt = new Date(oauth.expiresAt);
  }
  if (oauth.rateLimitTier) creds.rateLimitTier = oauth.rateLimitTier;
  return creds;
}

/** Human plan name inferred from `rateLimitTier` per CodexBar's ClaudePlan.swift. */
function planFromTier(tier: string | undefined): string | undefined {
  if (!tier) return undefined;
  const t = tier.trim().toLowerCase();
  if (t.includes("max")) return "Claude Max";
  if (t.includes("pro")) return "Claude Pro";
  if (t.includes("team")) return "Claude Team";
  if (t.includes("enterprise")) return "Claude Enterprise";
  if (t.includes("ultra")) return "Claude Ultra";
  return undefined;
}

function parseIsoDate(input: string | null | undefined): Date | undefined {
  if (!input) return undefined;
  const d = new Date(input);
  return Number.isNaN(d.getTime()) ? undefined : d;
}

function resetInSeconds(when: Date | undefined): number | undefined {
  if (!when) return undefined;
  const delta = Math.floor((when.getTime() - Date.now()) / 1000);
  return delta > 0 ? delta : 0;
}

/**
 * Convert a "percent used" window into a "percent remaining" metric.
 * Returns undefined if the window or its utilization is missing.
 */
function remainingMetric(
  id: string,
  label: string,
  name: string,
  window: UsageWindow | null | undefined,
): MetricValue | undefined {
  if (!window || typeof window.utilization !== "number") return undefined;
  const used = Math.max(0, Math.min(100, window.utilization));
  const remaining = 100 - used;
  const resetsAt = parseIsoDate(window.resets_at);
  const metric: MetricValue = {
    id,
    label,
    name,
    value: Math.round(remaining),
    unit: "%",
    ratio: remaining / 100,
    direction: "up",
    updatedAt: new Date(),
  };
  const reset = resetInSeconds(resetsAt);
  if (reset !== undefined) metric.resetInSeconds = reset;
  return metric;
}

/** Claude's extra_usage monthly spend cap: fill represents headroom. */
function extraUsageMetrics(extra: ExtraUsage | null | undefined): MetricValue[] {
  if (!extra || extra.is_enabled !== true) return [];
  const limitCents = extra.monthly_limit ?? 0;
  const spentCents = extra.used_credits ?? 0;
  if (limitCents <= 0) return [];
  const limit = limitCents / 100;
  const spent = spentCents / 100;
  const remaining = Math.max(0, limit - spent);
  const usedPct = Math.min(100, (spent / limit) * 100);
  const remPct = 100 - usedPct;
  const now = new Date();
  const currency = extra.currency ?? "USD";
  return [
    {
      id: "extra-usage-percent",
      label: "EXTRA",
      name: "Extra usage headroom",
      value: Math.round(remPct),
      unit: "%",
      ratio: remPct / 100,
      direction: "up",
      updatedAt: now,
    },
    {
      id: "extra-usage-remaining",
      label: "EXTRA",
      name: `Extra usage remaining (${currency})`,
      value: `$${remaining.toFixed(2)}`,
      ratio: remPct / 100,
      direction: "up",
      updatedAt: now,
    },
    {
      id: "extra-usage-spent",
      label: "EXTRA$",
      name: `Extra usage spent (${currency})`,
      value: `$${spent.toFixed(2)}`,
      ratio: usedPct / 100,
      direction: "up",
      updatedAt: now,
    },
  ];
}

export class ClaudeProvider implements Provider {
  readonly id = "claude";
  readonly name = "Claude";
  readonly metricIds = [
    "session-percent",
    "weekly-percent",
    "weekly-sonnet-percent",
    "weekly-opus-percent",
    "extra-usage-percent",
    "extra-usage-remaining",
    "extra-usage-spent",
  ] as const;

  async fetch(): Promise<ProviderSnapshot> {
    const creds = await loadCredentials();

    if (!creds.scopes.includes("user:profile")) {
      throw new ClaudeOAuthError(
        `Claude OAuth token missing \`user:profile\` scope (has: ${creds.scopes.join(", ") || "(none)"}). Run \`claude setup-token\` to regenerate.`,
        "scope-missing",
      );
    }
    if (creds.expiresAt && creds.expiresAt.getTime() <= Date.now()) {
      throw new ClaudeOAuthError(
        `Claude OAuth access token expired at ${creds.expiresAt.toISOString()}. Run \`claude\` to re-authenticate. (Token refresh is not yet implemented in this plugin.)`,
        "expired",
      );
    }

    let response: UsageResponse;
    try {
      response = await httpJson<UsageResponse>({
        url: USAGE_URL,
        headers: {
          authorization: `Bearer ${creds.accessToken}`,
          "anthropic-beta": BETA_HEADER,
          "user-agent": USER_AGENT,
          "content-type": "application/json",
        },
        timeoutMs: 30_000,
      });
    } catch (err) {
      if (err instanceof HttpError) {
        if (err.status === 401) {
          throw new ClaudeOAuthError(
            "Claude OAuth request unauthorized. Run `claude` to re-authenticate.",
            "unauthorized",
          );
        }
        if (err.status === 403 && err.body.includes("user:profile")) {
          throw new ClaudeOAuthError(
            "Claude OAuth token missing `user:profile` scope. Run `claude setup-token` to regenerate.",
            "scope-missing",
          );
        }
        throw new ClaudeOAuthError(
          `Claude OAuth server error: HTTP ${err.status}`,
          "server",
        );
      }
      throw new ClaudeOAuthError(
        `Claude OAuth network error: ${String(err)}`,
        "network",
      );
    }

    const metrics: MetricValue[] = [];
    const session = remainingMetric(
      "session-percent",
      "SESSION",
      "Session window remaining (5h)",
      response.five_hour,
    );
    if (session) metrics.push(session);

    const weekly = remainingMetric(
      "weekly-percent",
      "WEEKLY",
      "Weekly window remaining",
      response.seven_day,
    );
    if (weekly) metrics.push(weekly);

    const sonnet = remainingMetric(
      "weekly-sonnet-percent",
      "SONNET",
      "Weekly Sonnet remaining",
      response.seven_day_sonnet,
    );
    if (sonnet) metrics.push(sonnet);

    const opus = remainingMetric(
      "weekly-opus-percent",
      "OPUS",
      "Weekly Opus remaining",
      response.seven_day_opus,
    );
    if (opus) metrics.push(opus);

    metrics.push(...extraUsageMetrics(response.extra_usage));

    return {
      providerId: this.id,
      providerName: planFromTier(creds.rateLimitTier) ?? this.name,
      source: "oauth",
      metrics,
      status: "operational",
    };
  }
}

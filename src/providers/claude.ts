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
import { getClaudeSettings } from "../settings.ts";
import { fetchClaudeExtraUsage } from "./claude-web.ts";
import { CODEXBAR_BRAND_COLORS } from "./brand-colors.ts";
import type {
  MetricValue,
  Provider,
  ProviderSnapshot,
} from "./types.ts";

function truncate(s: string, n: number): string {
  if (!s) return "";
  const clean = s.replace(/\s+/g, " ").trim();
  return clean.length <= n ? clean : `${clean.slice(0, n)}…`;
}

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

/**
 * Extra usage block — field names tolerant of both snake_case and
 * camelCase because we haven't yet confirmed which Anthropic actually
 * returns for this nested object. CodexBar's Swift decoder uses
 * camelCase property names but its CodingKeys enum wasn't in the
 * reference report I pulled, so we accept either until we've seen a
 * live response. `normaliseExtraUsage` below picks whichever is set.
 */
interface ExtraUsageRaw {
  is_enabled?: boolean | null;
  isEnabled?: boolean | null;
  monthly_limit?: number | null;
  monthlyLimit?: number | null;
  used_credits?: number | null;
  usedCredits?: number | null;
  utilization?: number | null;
  currency?: string | null;
}

interface ExtraUsage {
  isEnabled: boolean;
  monthlyLimit: number | undefined; // cents
  usedCredits: number | undefined;  // cents
  utilization: number | undefined;
  currency: string | undefined;
}

function normaliseExtraUsage(raw: ExtraUsageRaw | null | undefined): ExtraUsage | undefined {
  if (!raw) return undefined;
  const isEnabled = raw.isEnabled ?? raw.is_enabled ?? false;
  const result: ExtraUsage = {
    isEnabled: isEnabled === true,
    monthlyLimit: raw.monthlyLimit ?? raw.monthly_limit ?? undefined,
    usedCredits: raw.usedCredits ?? raw.used_credits ?? undefined,
    utilization: raw.utilization ?? undefined,
    currency: raw.currency ?? undefined,
  };
  return result;
}

interface UsageResponse {
  five_hour?: UsageWindow | null;
  seven_day?: UsageWindow | null;
  seven_day_sonnet?: UsageWindow | null;
  seven_day_opus?: UsageWindow | null;
  seven_day_oauth_apps?: UsageWindow | null;
  extra_usage?: ExtraUsageRaw | null;
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

/**
 * Claude's extra_usage monthly spend cap: fill represents headroom.
 *
 * Takes a unified `ExtraUsageSource` that may have come from the
 * OAuth response OR from the Web API supplement — both produce the
 * same shape after normalisation (cents + currency + enabled flag).
 *
 * When `isEnabled === false`, CodexBar's OAuth handler bails out.
 * We keep that behaviour for OAuth-only data, but the Web API path
 * returns the *real* monthly_credit_limit / used_credits even for
 * disabled accounts, so we also render the dollar metrics when a
 * limit is known regardless of the enabled flag. This matches what
 * CodexBar shows in its menu ("$0.00 / $X.XX" even when the toggle
 * is off on claude.ai).
 */
interface ExtraUsageSource {
  isEnabled: boolean;
  /** Monthly spend limit in cents. */
  monthlyLimitCents: number;
  /** Amount spent this month in cents. */
  usedCreditsCents: number;
  currency: string;
}

function extraUsageMetrics(extra: ExtraUsageSource | undefined): MetricValue[] {
  if (!extra) return [];
  const limitCents = extra.monthlyLimitCents;
  if (limitCents <= 0) return [];
  const spentCents = extra.usedCreditsCents;
  const limit = limitCents / 100;
  const spent = spentCents / 100;
  const remaining = Math.max(0, limit - spent);
  const usedPct = Math.min(100, (spent / limit) * 100);
  const remPct = 100 - usedPct;
  const now = new Date();
  const currency = extra.currency;
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
  readonly brandColor = CODEXBAR_BRAND_COLORS.claude;
  readonly metricIds = [
    "session-percent",
    "weekly-percent",
    "weekly-sonnet-percent",
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
            `Claude OAuth request unauthorized. Run \`claude\` to re-authenticate. body=${truncate(err.body, 200)}`,
            "unauthorized",
          );
        }
        if (err.status === 403 && err.body.includes("user:profile")) {
          throw new ClaudeOAuthError(
            `Claude OAuth token missing \`user:profile\` scope. Run \`claude setup-token\` to regenerate. body=${truncate(err.body, 200)}`,
            "scope-missing",
          );
        }
        // Surface everything we can: status, key headers, body.
        // This is the difference between "some 429" and
        // "actually a rate limit with a Retry-After".
        const retryAfter = err.header("retry-after");
        const reqId = err.header("request-id") ?? err.header("x-request-id");
        const meta = [
          `HTTP ${err.status}`,
          retryAfter ? `retry-after=${retryAfter}` : "",
          reqId ? `req=${reqId}` : "",
        ]
          .filter(Boolean)
          .join(" ");
        throw new ClaudeOAuthError(
          `Claude OAuth server error: ${meta} body=${truncate(err.body, 200)}`,
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

    // Match CodexBar: there is ONE model-specific slot, labeled
    // "Sonnet", populated from `seven_day_sonnet` first and falling
    // back to `seven_day_opus` if sonnet is missing. CodexBar never
    // exposes a separate "Opus" line — the fallback-chain design is
    // intentional because the two fields describe the same weekly
    // model-specific quota from different sides.
    // (ref: tmp/CodexBar/Sources/CodexBarCore/Providers/Claude/
    //       ClaudeUsageFetcher.swift lines 841-843)
    const modelSpecific = remainingMetric(
      "weekly-sonnet-percent",
      "SONNET",
      "Weekly Sonnet remaining",
      response.seven_day_sonnet ?? response.seven_day_opus,
    );
    if (modelSpecific) metrics.push(modelSpecific);

    // Extra usage resolution order:
    //   1. OAuth response's `extra_usage` block, if enabled and has numbers
    //   2. claude.ai Web API `/overage_spend_limit`, if user pasted a cookie
    // OAuth has no HTTP cost beyond the /usage call we already made,
    // so we try it first. The web call only happens when (a) OAuth
    // didn't give us usable data and (b) a cookie header is configured
    // in global settings.
    const oauthExtra = normaliseExtraUsage(response.extra_usage);
    let extraSource: ExtraUsageSource | undefined;
    if (
      oauthExtra &&
      oauthExtra.isEnabled &&
      (oauthExtra.monthlyLimit ?? 0) > 0
    ) {
      extraSource = {
        isEnabled: true,
        monthlyLimitCents: oauthExtra.monthlyLimit ?? 0,
        usedCreditsCents: oauthExtra.usedCredits ?? 0,
        currency: oauthExtra.currency ?? "USD",
      };
    } else {
      // OAuth didn't give us usable extras — try the claude.ai cookie
      // path as a supplement. The fetcher reads its own cookie source
      // from global settings (auto-import from browsers, manual paste,
      // or off) so we just call it with no args. This whole block
      // runs INSIDE the per-provider cache's single in-flight fetch,
      // so all visible keys bound to Claude share the same web call.
      const cs = getClaudeSettings();
      if (cs.source !== "oauth") {
        debugLogSink?.("claude: OAuth extras missing → trying claude.ai cookie path");
        const web = await fetchClaudeExtraUsage();
        if (web) {
          extraSource = web;
          debugLogSink?.(
            `claude: web path returned monthlyLimit=${web.monthlyLimitCents}c used=${web.usedCreditsCents}c isEnabled=${web.isEnabled}`,
          );
        } else {
          debugLogSink?.("claude: web path returned no data (cookie missing / scan failed / 401)");
        }
      }
    }
    metrics.push(...extraUsageMetrics(extraSource));

    // Schema observability. Key names + numeric stat values only —
    // no secrets. This tells us whether the "empty extra usage" face
    // is (a) OAuth returning disabled, (b) web cookie missing, or
    // (c) web call failed.
    const topKeys = Object.keys(response as Record<string, unknown>).sort().join(",");
    const rawExtra = response.extra_usage as Record<string, unknown> | null | undefined;
    const extraKeys = rawExtra
      ? Object.keys(rawExtra).sort().join(",")
      : "(missing)";
    const extraValues = extraSource
      ? `source=${oauthExtra?.isEnabled ? "oauth" : "web"} isEnabled=${extraSource.isEnabled} monthlyLimitCents=${extraSource.monthlyLimitCents} usedCreditsCents=${extraSource.usedCreditsCents}`
      : "(none)";
    debugLogSink?.(
      `claude response: topKeys=[${topKeys}] extra_usage keys=[${extraKeys}] values=${extraValues}`,
    );

    // Per-window utilization dump — tells us definitively whether
    // Opus is missing-from-response, null-utilization, or simply
    // parsed-but-dropped by our fetcher. "absent" = window object
    // not returned at all; "null" = window returned but utilization
    // is null; a number = Anthropic is tracking this window.
    const windowLabel = (w: UsageWindow | null | undefined): string => {
      if (w === null || w === undefined) return "absent";
      if (typeof w.utilization !== "number") return "null";
      return String(w.utilization);
    };
    // seven_day_cowork isn't in our UsageResponse type yet — read it
    // via a loose cast just for the diagnostic line.
    const cowork = (response as Record<string, UsageWindow | null | undefined>)["seven_day_cowork"];
    debugLogSink?.(
      `claude windows: five_hour=${windowLabel(response.five_hour)} seven_day=${windowLabel(response.seven_day)} sonnet=${windowLabel(response.seven_day_sonnet)} opus=${windowLabel(response.seven_day_opus)} cowork=${windowLabel(cowork)}`,
    );

    return {
      providerId: this.id,
      providerName: planFromTier(creds.rateLimitTier) ?? this.name,
      source: "oauth",
      metrics,
      status: "operational",
    };
  }
}

/**
 * Optional debug sink the plugin wires to Stream Deck's log file on
 * startup. Kept as an injected callback so this module stays
 * dependency-free. Called once per successful fetch with schema info.
 */
let debugLogSink: ((msg: string) => void) | undefined;
export function setClaudeDebugLogSink(fn: (msg: string) => void): void {
  debugLogSink = fn;
}

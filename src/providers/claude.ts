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
import { fetchClaudeExtraUsage, type WebExtraUsage } from "./claude-web.ts";
import { CODEXBAR_BRAND_COLORS } from "./brand-colors.ts";
import type {
  MetricValue,
  Provider,
  ProviderContext,
  ProviderSnapshot,
} from "./types.ts";

/**
 * Extras-refresh policy constants.
 *
 * Claude extras are a supplementary metric that rarely change (you
 * can only rack up charges after you've actually hit your weekly
 * limit AND flipped the extras toggle on). Hitting
 * `/overage_spend_limit` + `/prepaid/credits` every 5 minutes for
 * most users is wasteful — Anthropic has no idea how to give us a
 * different answer than "is_enabled: false, balance: $X".
 *
 * Refresh policy:
 *   - Default:          once every 60 minutes
 *   - Near a limit
 *     (session/weekly
 *      usage ≥ 80%):    bump to every 15 minutes, in case the
 *                        user flips extras on
 *   - Extras is ON:     every 15 minutes (spend is actively moving)
 *   - `force=true`
 *     (user keyDown):   always refresh, bypasses the policy
 *
 * Manual refresh via keyDown sets `ctx.force = true` all the way
 * through cache → provider.fetch so the user can always demand a
 * fresh answer by pressing the key.
 */
const EXTRAS_DEFAULT_TTL_MS = 60 * 60 * 1000; // 60 min
const EXTRAS_ACTIVE_TTL_MS = 15 * 60 * 1000; // 15 min (near limit / extras on)
const NEAR_LIMIT_THRESHOLD_PERCENT = 80;

interface CachedExtras {
  source: ExtraUsageSource;
  capturedAt: number;
}
let cachedExtras: CachedExtras | undefined;

/**
 * Decide whether to re-fetch extras from the web endpoint on this
 * tick. Returns `{ refresh: true }` to fire the web calls, or
 * `{ refresh: false, reason }` to reuse `cachedExtras`.
 */
function shouldRefreshExtras(
  response: UsageResponse,
  force: boolean,
): { refresh: true; reason: string } | { refresh: false; reason: string } {
  if (force) return { refresh: true, reason: "forced" };
  if (!cachedExtras) return { refresh: true, reason: "no cache" };

  const age = Date.now() - cachedExtras.capturedAt;
  const sessionPct = response.five_hour?.utilization ?? 0;
  const weeklyPct = response.seven_day?.utilization ?? 0;
  const nearLimit =
    sessionPct >= NEAR_LIMIT_THRESHOLD_PERCENT ||
    weeklyPct >= NEAR_LIMIT_THRESHOLD_PERCENT;
  const extrasOn = cachedExtras.source.isEnabled;

  if (extrasOn) {
    return age >= EXTRAS_ACTIVE_TTL_MS
      ? { refresh: true, reason: `extras on, age=${Math.round(age / 1000)}s` }
      : { refresh: false, reason: `extras on but fresh (${Math.round(age / 1000)}s)` };
  }

  if (nearLimit) {
    return age >= EXTRAS_ACTIVE_TTL_MS
      ? { refresh: true, reason: `near limit (${Math.round(Math.max(sessionPct, weeklyPct))}%), age=${Math.round(age / 1000)}s` }
      : { refresh: false, reason: `near limit but fresh (${Math.round(age / 1000)}s)` };
  }

  return age >= EXTRAS_DEFAULT_TTL_MS
    ? { refresh: true, reason: `default TTL elapsed (${Math.round(age / 1000)}s)` }
    : { refresh: false, reason: `far from limits + extras off, reusing (${Math.round(age / 60000)}m old)` };
}

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
    numericValue: remaining,
    numericUnit: "percent",
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
  /** Optional prepaid credit balance in cents (from the balance probe). */
  balanceCents?: number;
  /** True if auto-reload is configured on the balance. */
  autoReloadEnabled?: boolean;
  /** True if account is out of extras credits. */
  outOfCredits?: boolean;
  /** Account email for claude.ai web scope. */
  accountEmail?: string;
}

function extraUsageMetrics(extra: ExtraUsageSource | undefined): MetricValue[] {
  if (!extra) return [];

  const now = new Date();
  const out: MetricValue[] = [];

  // Always emit the ON/OFF metric when we have an extras block at
  // all — even if the monthly limit is zero / not set.
  out.push({
    id: "extra-usage-enabled",
    label: "EXTRA USAGE",
    name: "Extra usage enabled",
    value: extra.isEnabled ? "ON" : "OFF",
    ratio: extra.isEnabled ? 1 : 0,
    direction: "up",
    updatedAt: now,
  });

  // (Removed the out-of-credits "OK" / "OUT" status metric — it
  // was a dumb boolean restating what the BALANCE button already
  // shows. When balance hits $0 the user sees that directly on the
  // balance tile; a second "CREDITS OK" button alongside it was
  // pure redundancy.)

  // Prepaid balance from /api/organizations/{orgId}/prepaid/credits.
  // Rendered as a dollar amount with the raw numericValue attached
  // so the render layer can apply threshold-based colors (orange
  // when low, red when negative) without parsing the display string.
  // numericGoodWhen defaults to "high" — a low balance = bad.
  if (typeof extra.balanceCents === "number") {
    const bal = extra.balanceCents / 100;
    out.push({
      id: "extra-usage-balance",
      label: "BALANCE",
      name: "Extra usage prepaid balance",
      value: `$${bal.toFixed(2)}`,
      numericValue: bal,
      numericUnit: "dollars",
      numericGoodWhen: "high",
      // Full-tile fill. The balance isn't a progress meter (no
      // natural "100%" reference for a standalone dollar figure),
      // but rendering it as a flat dark text-only tile looked
      // anemic next to the meter tiles. With ratio=1 the whole
      // tile carries the provider's brand color, and the
      // threshold logic still repaints it amber (low balance)
      // or red ($0 / negative) so the tile is also an alarm
      // surface — you just can't miss it when the balance drops.
      ratio: 1,
      direction: "up",
      // Static subvalue label — the prepaid balance isn't a
      // countdown so the subvalue slot would otherwise be empty.
      // "Prepaid" makes the tile's meaning obvious at a glance
      // and visually balances the layout against other tiles
      // that carry a reset timer.
      caption: "Prepaid",
      updatedAt: now,
    });
  }

  // Auto-reload on/off. Only emit when we actually have a signal
  // from the balance endpoint.
  if (extra.autoReloadEnabled !== undefined) {
    out.push({
      id: "extra-usage-auto-reload",
      label: "RELOAD",
      name: "Extras auto-reload",
      value: extra.autoReloadEnabled ? "ON" : "OFF",
      ratio: extra.autoReloadEnabled ? 1 : 0,
      direction: "up",
      updatedAt: now,
    });
  }

  // NOTE: account email is intentionally not a button metric —
  // too long to fit a 144x144 tile and not actionable glanceably.
  // It's still captured on ExtraUsageSource so a future PI tweak
  // can display "Signed in as: user@example.com" in the Plugin
  // Settings tab as informational text.

  const limitCents = extra.monthlyLimitCents;
  if (limitCents <= 0) return out;
  const spentCents = extra.usedCreditsCents;
  const limit = limitCents / 100;
  const spent = spentCents / 100;
  const remaining = Math.max(0, limit - spent);
  const usedPct = Math.min(100, (spent / limit) * 100);
  const remPct = 100 - usedPct;
  const currency = extra.currency;
  out.push(
    {
      id: "extra-usage-percent",
      label: "HEADROOM",
      name: "Extra usage headroom",
      value: Math.round(remPct),
      numericValue: remPct,
      numericUnit: "percent",
      numericGoodWhen: "high",
      unit: "%",
      ratio: remPct / 100,
      direction: "up",
      // Static "Monthly" caption disambiguates what the percentage
      // is relative to (monthly extras cap, not the session or
      // weekly windows which are on their own countdowns).
      caption: "Monthly",
      updatedAt: now,
    },
    {
      // The "limit" view: a static reference tile that displays
      // the constant monthly cap ($50.00 in the user's case).
      // The whole tile is filled in the provider's brand color
      // (ratio: 1) so it reads as "this is the ceiling" rather
      // than a progress bar — SPENT already covers the progress
      // semantic and having both tiles show the same meter was
      // redundant (and at $0 spent both looked empty).
      //
      // Thresholds still fire based on spent vs. limit: at 80%
      // of the cap the tile repaints amber, at 100% it repaints
      // red. numericValue carries the SPENT amount and numericMax
      // is the limit, with numericGoodWhen: "low" — meaning the
      // tile stays in brand color while headroom is healthy and
      // flips to the warn / critical colors as spending climbs.
      id: "extra-usage-limit",
      label: "LIMIT",
      name: `Extra usage monthly limit (${currency})`,
      value: `$${limit.toFixed(2)}`,
      numericValue: spent,
      numericUnit: "dollars",
      numericGoodWhen: "low",
      numericMax: limit,
      ratio: 1,
      direction: "up",
      caption: "Monthly",
      updatedAt: now,
    },
    {
      id: "extra-usage-spent",
      label: "SPENT",
      name: `Extra usage spent (${currency})`,
      value: `$${spent.toFixed(2)}`,
      numericValue: spent,
      numericUnit: "dollars",
      // Low-is-good: we WANT spent to be close to $0. Warn when
      // spent climbs to 80% of the monthly limit, red at the cap.
      numericGoodWhen: "low",
      numericMax: limit,
      ratio: usedPct / 100,
      direction: "up",
      updatedAt: now,
    },
  );
  return out;
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
    "extra-usage-limit",
    "extra-usage-spent",
    "extra-usage-enabled",
    "extra-usage-balance",
    "extra-usage-auto-reload",
  ] as const;

  async fetch(ctx?: ProviderContext): Promise<ProviderSnapshot> {
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

    // Extra usage resolution — three-way branch driven by
    // `providers.claude.source`:
    //
    //   "oauth"  → OAuth only. Ignores any pasted cookie. Extras
    //              populate only when Anthropic's OAuth endpoint
    //              actually returned them (rare — most accounts
    //              see `is_enabled: false`).
    //
    //   "cookie" → Cookie-primary. OAuth still fetches session /
    //              weekly / sonnet (no choice — it's the only
    //              source for those). For extras we skip OAuth's
    //              `is_enabled` check entirely and go straight to
    //              claude.ai's /overage_spend_limit endpoint.
    //
    //   "both"   → (default) OAuth first for everything, cookie
    //              supplement for extras only when OAuth's extras
    //              block is missing / disabled.
    //
    // Whichever branch runs, it runs INSIDE the per-provider
    // snapshot cache's single in-flight fetch, so every visible
    // Claude key shares the same HTTP calls.
    const cs = getClaudeSettings();
    const oauthExtra = normaliseExtraUsage(response.extra_usage);
    let extraSource: ExtraUsageSource | undefined;
    let extraSourceLabel: "oauth" | "web" | "cache" | "none" = "none";

    const oauthUsable =
      !!oauthExtra &&
      oauthExtra.isEnabled &&
      (oauthExtra.monthlyLimit ?? 0) > 0;

    // Policy: do we actually need to hit claude.ai's web endpoint
    // this tick, or can we reuse last-fetched extras? keyDown
    // force=true always re-fetches, bypassing the policy.
    const policy = shouldRefreshExtras(response, ctx?.force === true);
    debugLogSink?.(
      `claude: extras policy → ${policy.refresh ? "REFRESH" : "REUSE"} (${policy.reason})`,
    );

    function useOauthExtras(): void {
      if (!oauthExtra) return;
      extraSource = {
        isEnabled: true,
        monthlyLimitCents: oauthExtra.monthlyLimit ?? 0,
        usedCreditsCents: oauthExtra.usedCredits ?? 0,
        currency: oauthExtra.currency ?? "USD",
      };
      extraSourceLabel = "oauth";
      cachedExtras = { source: extraSource, capturedAt: Date.now() };
    }

    async function tryWebExtras(why: string): Promise<void> {
      debugLogSink?.(`claude: ${why} → trying claude.ai cookie path`);
      const web = await fetchClaudeExtraUsage();
      if (web) {
        extraSource = web;
        extraSourceLabel = "web";
        cachedExtras = { source: web, capturedAt: Date.now() };
        debugLogSink?.(
          `claude: web path returned monthlyLimit=${web.monthlyLimitCents}c used=${web.usedCreditsCents}c isEnabled=${web.isEnabled}`,
        );
      } else {
        debugLogSink?.("claude: web path returned no data (cookie missing / scan failed / 401)");
      }
    }

    if (!policy.refresh && cachedExtras) {
      // Reuse cached extras from a prior fetch — no claude.ai call.
      extraSource = cachedExtras.source;
      extraSourceLabel = "cache";
    } else if (cs.source === "oauth") {
      if (oauthUsable) useOauthExtras();
    } else if (cs.source === "cookie") {
      await tryWebExtras("source=cookie");
    } else {
      if (oauthUsable) {
        useOauthExtras();
      } else {
        await tryWebExtras("source=both, OAuth extras missing");
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
      ? `source=${extraSourceLabel} isEnabled=${extraSource.isEnabled} monthlyLimitCents=${extraSource.monthlyLimitCents} usedCreditsCents=${extraSource.usedCreditsCents} balanceCents=${extraSource.balanceCents ?? "—"}`
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

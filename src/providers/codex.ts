/**
 * Codex OAuth API usage provider.
 *
 * Port of CodexBar's `CodexOAuth/` Swift implementation. We use the
 * OAuth-API path exclusively (no WebKit dashboard scraping, no PTY,
 * no CLI RPC for v1) because it's the only path that's both
 * Windows-native and doesn't require the `codex` CLI to be installed.
 *
 * References:
 *   tmp/CodexBar/Sources/CodexBarCore/Providers/Codex/CodexOAuth/
 *     CodexOAuthCredentials.swift      — auth.json shape
 *     CodexOAuthUsageFetcher.swift     — HTTP request / response
 *     CodexTokenRefresher.swift        — refresh flow (deferred)
 *   tmp/CodexBar/Sources/CodexBarCore/Providers/Codex/CodexRateWindowNormalizer.swift
 *   docs/codex.md
 *
 * Credential file contract — `~/.codex/auth.json` (or `$CODEX_HOME/auth.json`):
 *
 *   {
 *     "OPENAI_API_KEY": null,             // optional legacy API-key auth
 *     "tokens": {
 *       "access_token":  "eyJ…",
 *       "refresh_token": "…",
 *       "id_token":      "eyJ…",          // JWT with email + plan claims
 *       "account_id":    "account-…"       // optional ChatGPT account id
 *     },
 *     "last_refresh":     "2025-12-28T12:34:56Z"
 *   }
 *
 *   CodexBar accepts both snake_case and camelCase keys — we match
 *   that tolerance because the underlying file can have been written
 *   by older versions of the Codex CLI.
 *
 * Usage API contract:
 *
 *   GET https://chatgpt.com/backend-api/wham/usage
 *   Authorization:      Bearer <access_token>
 *   ChatGPT-Account-Id: <account_id>            (optional)
 *   User-Agent:         UsageButtons
 *   Accept:             application/json
 *
 *   200 → {
 *     "plan_type": "pro" | "plus" | "team" | "enterprise" | "free" | …,
 *     "rate_limit": {
 *       "primary_window":   { "used_percent": 15,  "reset_at": 1735401600, "limit_window_seconds": 18000  },
 *       "secondary_window": { "used_percent": 5,   "reset_at": 1735920000, "limit_window_seconds": 604800 }
 *     },
 *     "credits": {
 *       "has_credits": true,
 *       "unlimited":   false,
 *       "balance":     150.0               // may be number OR string
 *     }
 *   }
 *
 * reset_at is a **Unix epoch in seconds** (not milliseconds, not
 * ISO-8601). limit_window_seconds = 18000 means session (5h),
 * 604800 means weekly (7d) — CodexBar's normaliser uses this to
 * classify windows when the server hands them back in a different
 * slot than expected; we do the same.
 *
 * This first cut does NOT implement token refresh. If the access
 * token is expired (401/403), we surface a clear "run `codex` to
 * re-authenticate" error and mark the metric stale. Refresh is a
 * follow-up — CodexBar refreshes against
 * `https://auth.openai.com/oauth/token` with client_id
 * `app_EMoamEEZ73f0CkXaXp7hrann` when `last_refresh` is > 8 days old.
 */

import { codexAuthPath } from "../util/paths.ts";
import {
  readJsonCredential,
  CredentialNotFoundError,
} from "../util/credentials.ts";
import { httpJson, HttpError } from "../util/http.ts";
import { computePace } from "../util/pace.ts";
import { getCodexCosts, formatTokenCount } from "./cost-scanner.ts";
import { CODEXBAR_BRAND_COLORS } from "./brand-colors.ts";
import type {
  MetricValue,
  Provider,
  ProviderSnapshot,
} from "./types.ts";

const USAGE_URL = "https://chatgpt.com/backend-api/wham/usage";
const USER_AGENT = "UsageButtons/0.0.1";

const SESSION_WINDOW_SECONDS = 5 * 60 * 60; // 18000
const WEEKLY_WINDOW_SECONDS = 7 * 24 * 60 * 60; // 604800

/** Raw auth.json shape, tolerant of snake_case + camelCase. */
interface CodexAuthFile {
  OPENAI_API_KEY?: string | null;
  openai_api_key?: string | null;
  tokens?: {
    access_token?: string;
    accessToken?: string;
    refresh_token?: string;
    refreshToken?: string;
    id_token?: string;
    idToken?: string;
    account_id?: string;
    accountId?: string;
  };
  last_refresh?: string;
  lastRefresh?: string;
}

interface CodexCredentials {
  accessToken: string;
  refreshToken?: string;
  idToken?: string;
  accountId?: string;
  lastRefresh?: Date;
  /** True when an OPENAI_API_KEY was provided directly (no refresh flow). */
  isApiKey: boolean;
}

/** Raw API response shape. */
interface UsageWindow {
  used_percent?: number | null;
  reset_at?: number | null; // epoch seconds
  limit_window_seconds?: number | null;
}

interface UsageResponse {
  plan_type?: string | null;
  rate_limit?: {
    primary_window?: UsageWindow | null;
    secondary_window?: UsageWindow | null;
  } | null;
  credits?: {
    has_credits?: boolean | null;
    unlimited?: boolean | null;
    balance?: number | string | null;
  } | null;
}

export class CodexOAuthError extends Error {
  constructor(
    message: string,
    public readonly kind:
      | "not-configured"
      | "unauthorized"
      | "server"
      | "network",
  ) {
    super(message);
    this.name = "CodexOAuthError";
  }
}

async function loadCredentials(): Promise<CodexCredentials> {
  const path = codexAuthPath();
  let raw: CodexAuthFile;
  try {
    raw = await readJsonCredential<CodexAuthFile>(path);
  } catch (err) {
    if (err instanceof CredentialNotFoundError) {
      throw new CodexOAuthError(
        `Codex credentials not found at ${path}. Run \`codex\` in a terminal to sign in.`,
        "not-configured",
      );
    }
    throw err;
  }

  // Legacy API key auth: if OPENAI_API_KEY is set in auth.json, it's
  // used as the bearer token directly and no refresh is needed.
  const apiKey = raw.OPENAI_API_KEY ?? raw.openai_api_key;
  if (apiKey && apiKey.trim() !== "") {
    return { accessToken: apiKey.trim(), isApiKey: true };
  }

  const tokens = raw.tokens;
  const accessToken = tokens?.access_token ?? tokens?.accessToken;
  if (!tokens || !accessToken || accessToken.trim() === "") {
    throw new CodexOAuthError(
      `Codex credentials at ${path} missing tokens.access_token.`,
      "not-configured",
    );
  }

  const creds: CodexCredentials = {
    accessToken: accessToken.trim(),
    isApiKey: false,
  };
  const refresh = tokens.refresh_token ?? tokens.refreshToken;
  if (refresh) creds.refreshToken = refresh;
  const idTok = tokens.id_token ?? tokens.idToken;
  if (idTok) creds.idToken = idTok;
  const acct = tokens.account_id ?? tokens.accountId;
  if (acct) creds.accountId = acct;
  const lr = raw.last_refresh ?? raw.lastRefresh;
  if (lr) {
    const d = new Date(lr);
    if (!Number.isNaN(d.getTime())) creds.lastRefresh = d;
  }
  return creds;
}

/**
 * Best-effort plan-name lookup. We match CodexBar's ChatGPT plan
 * strings. Unknown plan types are returned verbatim.
 */
function humanPlan(planType: string | null | undefined): string | undefined {
  if (!planType) return undefined;
  const key = planType.toLowerCase();
  const map: Record<string, string> = {
    guest: "ChatGPT Guest",
    free: "ChatGPT Free",
    go: "ChatGPT Go",
    plus: "ChatGPT Plus",
    pro: "ChatGPT Pro",
    free_workspace: "ChatGPT Free (Workspace)",
    team: "ChatGPT Team",
    business: "ChatGPT Business",
    education: "ChatGPT Edu",
    edu: "ChatGPT Edu",
    enterprise: "ChatGPT Enterprise",
    quorum: "ChatGPT (Quorum)",
    k12: "ChatGPT (K12)",
  };
  return map[key] ?? planType;
}

/** Decode a JWT payload without signature verification. */
function decodeJwtPayload(token: string): Record<string, unknown> | undefined {
  const parts = token.split(".");
  if (parts.length < 2 || !parts[1]) return undefined;
  try {
    // base64url → base64 → utf8
    const b64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    const padded = b64.padEnd(b64.length + ((4 - (b64.length % 4)) % 4), "=");
    const json = Buffer.from(padded, "base64").toString("utf8");
    return JSON.parse(json) as Record<string, unknown>;
  } catch {
    return undefined;
  }
}

function emailFromIdToken(idToken: string | undefined): string | undefined {
  if (!idToken) return undefined;
  const payload = decodeJwtPayload(idToken);
  if (!payload) return undefined;
  const direct = payload["email"];
  if (typeof direct === "string") return direct;
  const ns = payload["https://api.openai.com/profile"];
  if (ns && typeof ns === "object") {
    const nested = (ns as Record<string, unknown>)["email"];
    if (typeof nested === "string") return nested;
  }
  return undefined;
}

/** Classify a window by its `limit_window_seconds` field. */
type WindowRole = "session" | "weekly" | "unknown";
function classifyWindow(seconds: number | null | undefined): WindowRole {
  if (seconds === SESSION_WINDOW_SECONDS) return "session";
  if (seconds === WEEKLY_WINDOW_SECONDS) return "weekly";
  return "unknown";
}

/**
 * Normalise the `primary_window`/`secondary_window` pair into
 * `{ session, weekly }` regardless of which slot the server returned
 * each window in — matches CodexBar's CodexRateWindowNormalizer.swift.
 */
function normaliseWindows(
  primary: UsageWindow | null | undefined,
  secondary: UsageWindow | null | undefined,
): { session?: UsageWindow; weekly?: UsageWindow } {
  const result: { session?: UsageWindow; weekly?: UsageWindow } = {};
  const assign = (window: UsageWindow | null | undefined): void => {
    if (!window) return;
    const role = classifyWindow(window.limit_window_seconds);
    if (role === "session" && !result.session) result.session = window;
    else if (role === "weekly" && !result.weekly) result.weekly = window;
    else if (role === "unknown") {
      if (!result.session) result.session = window;
      else if (!result.weekly) result.weekly = window;
    }
  };
  assign(primary);
  assign(secondary);
  return result;
}

function remainingMetric(
  id: string,
  label: string,
  name: string,
  window: UsageWindow | undefined,
): MetricValue | undefined {
  if (!window || typeof window.used_percent !== "number") return undefined;
  const used = Math.max(0, Math.min(100, window.used_percent));
  const remaining = 100 - used;
  const metric: MetricValue = {
    id,
    label,
    name,
    value: Math.round(remaining),
    // numericValue + numericUnit were missing from the original
    // Codex port — the old invertFill gate (`unit === "%"`) was
    // covering for it by matching on the display-string unit, but
    // the tightened gate (`numericUnit === "percent"`) would
    // otherwise skip Codex percent metrics entirely. Matches
    // Claude's remainingMetric shape so both providers go through
    // the same threshold / invert / render code paths uniformly.
    numericValue: remaining,
    numericUnit: "percent",
    unit: "%",
    ratio: remaining / 100,
    direction: "up",
    updatedAt: new Date(),
  };
  if (typeof window.reset_at === "number") {
    const delta = Math.floor(window.reset_at - Date.now() / 1000);
    metric.resetInSeconds = delta > 0 ? delta : 0;
  }
  return metric;
}

function parseBalance(raw: number | string | null | undefined): number | undefined {
  if (raw === null || raw === undefined) return undefined;
  if (typeof raw === "number") return Number.isFinite(raw) ? raw : undefined;
  const parsed = Number.parseFloat(raw);
  return Number.isFinite(parsed) ? parsed : undefined;
}

export class CodexProvider implements Provider {
  readonly id = "codex";
  readonly name = "Codex";
  readonly brandColor = CODEXBAR_BRAND_COLORS.codex;
  readonly metricIds = [
    "session-percent",
    "weekly-percent",
    "credits-balance",
    "cost-today",
    "cost-30d",
  ] as const;

  async fetch(): Promise<ProviderSnapshot> {
    const creds = await loadCredentials();

    const headers: Record<string, string> = {
      authorization: `Bearer ${creds.accessToken}`,
      "user-agent": USER_AGENT,
      accept: "application/json",
    };
    if (creds.accountId) headers["chatgpt-account-id"] = creds.accountId;

    let response: UsageResponse;
    try {
      response = await httpJson<UsageResponse>({
        url: USAGE_URL,
        headers,
        timeoutMs: 30_000,
      });
    } catch (err) {
      if (err instanceof HttpError) {
        if (err.status === 401 || err.status === 403) {
          throw new CodexOAuthError(
            "Codex OAuth request unauthorized. Run `codex` to re-authenticate. (Token refresh is not yet implemented in this plugin.)",
            "unauthorized",
          );
        }
        throw new CodexOAuthError(
          `Codex OAuth server error: HTTP ${err.status}`,
          "server",
        );
      }
      throw new CodexOAuthError(
        `Codex OAuth network error: ${String(err)}`,
        "network",
      );
    }

    const windows = normaliseWindows(
      response.rate_limit?.primary_window,
      response.rate_limit?.secondary_window,
    );

    const metrics: MetricValue[] = [];
    const session = remainingMetric(
      "session-percent",
      "SESSION",
      "Session window remaining (5h)",
      windows.session,
    );
    if (session) metrics.push(session);

    const weekly = remainingMetric(
      "weekly-percent",
      "WEEKLY",
      "Weekly window remaining",
      windows.weekly,
    );
    if (weekly) {
      const weeklyUsed = windows.weekly?.used_percent ?? 0;
      const pace = computePace(weeklyUsed, weekly.resetInSeconds, WEEKLY_WINDOW_SECONDS);
      if (pace) weekly.caption = pace.label;
      metrics.push(weekly);
    }

    // Credits metric: only emit for accounts that ACTUALLY have a
    // credits-based plan with a positive balance. Free-plan users
    // see `has_credits === false` (or unset) and the credits
    // button rendering "$0" / "100%" / similar makes no sense in
    // their context. Plus-tier users get `unlimited === true` and
    // also shouldn't see a credits readout. Both gated out.
    //
    // We require BOTH: an explicit `has_credits: true` AND a
    // numeric balance > 0. Anything else → no credits metric.
    const balance = parseBalance(response.credits?.balance);
    const hasCredits = response.credits?.has_credits === true;
    const unlimited = response.credits?.unlimited === true;
    if (balance !== undefined && balance > 0 && hasCredits && !unlimited) {
      metrics.push({
        id: "credits-balance",
        label: "CREDITS",
        name: "Credits remaining",
        value: `$${balance.toFixed(2)}`,
        numericValue: balance,
        numericUnit: "dollars",
        numericGoodWhen: "high",
        // Full-tile fill + "Prepaid" caption — mirrors how the
        // Claude extras balance tile is rendered. The dollar
        // figure alone on a dark background looked anemic next
        // to the session/weekly meter tiles; with ratio=1 the
        // whole tile carries Codex's brand teal and the
        // threshold logic repaints it amber/red when the
        // balance runs low.
        ratio: 1,
        direction: "up",
        caption: "Prepaid",
        updatedAt: new Date(),
      });
    }

    // Cost metrics from local JSONL session logs.
    try {
      const costs = await getCodexCosts();
      const now = new Date();
      if (costs.todayTokens > 0 || costs.last30DaysTokens > 0) {
        metrics.push({
          id: "cost-today",
          label: "TODAY",
          name: "Cost today (local logs)",
          value: `$${costs.todayCostUsd.toFixed(2)}`,
          numericValue: costs.todayCostUsd,
          numericUnit: "dollars",
          numericGoodWhen: "low",
          ratio: 1,
          direction: "up",
          caption: `${formatTokenCount(costs.todayTokens)} tokens`,
          updatedAt: now,
        });
        metrics.push({
          id: "cost-30d",
          label: "30 DAYS",
          name: "Cost last 30 days (local logs)",
          value: `$${costs.last30DaysCostUsd.toFixed(2)}`,
          numericValue: costs.last30DaysCostUsd,
          numericUnit: "dollars",
          numericGoodWhen: "low",
          ratio: 1,
          direction: "up",
          caption: `${formatTokenCount(costs.last30DaysTokens)} tokens`,
          updatedAt: now,
        });
      }
    } catch {
      // Cost scanning is best-effort
    }

    const planName = humanPlan(response.plan_type) ?? this.name;
    const email = emailFromIdToken(creds.idToken);

    return {
      providerId: this.id,
      providerName: email ? `${planName} — ${email}` : planName,
      source: creds.isApiKey ? "api-key" : "oauth",
      metrics,
      status: "operational",
    };
  }
}

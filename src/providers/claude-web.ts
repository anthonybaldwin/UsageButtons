/**
 * Claude **Web API** supplementary fetcher.
 *
 * This is the claude.ai browser-cookie path. CodexBar uses it as a
 * fallback/enrichment for Claude usage data — specifically, it's the
 * only way to get the `extra_usage` block for an account whose
 * `/api/oauth/usage` response returns `is_enabled: false`. Anthropic's
 * OAuth endpoint gates the extra-usage fields behind a scope we
 * can't request from the Claude CLI token, but claude.ai's own web
 * API (same account, different auth) returns them unconditionally.
 *
 * We use this as a SUPPLEMENT to the OAuth path: the OAuth fetcher
 * still produces the primary session/weekly/sonnet metrics. This
 * module only fills in the extra-usage block when a session cookie
 * is provided in global settings.
 *
 * References:
 *   tmp/CodexBar/Sources/CodexBarCore/Providers/Claude/ClaudeWeb/
 *     ClaudeWebAPIFetcher.swift           — endpoint, headers, response parsing
 *     CookieHeaderNormalizer.swift        — input format flexibility
 *
 * Endpoints (authenticated via Cookie header, no Bearer):
 *   GET https://claude.ai/api/organizations
 *     → [{ uuid, name?, capabilities?: string[] }, …]
 *   GET https://claude.ai/api/organizations/{orgId}/overage_spend_limit
 *     → { is_enabled, monthly_credit_limit (cents), used_credits (cents), currency }
 *
 * Note the wire-format difference from the OAuth endpoint:
 *   OAuth:  extra_usage.monthly_limit          (cents)
 *   Web:    overage_spend_limit.monthly_credit_limit  (cents)  ← different field name
 */

import { HttpError, httpJson } from "../util/http.ts";
import { getClaudeSettings } from "../settings.ts";

const BASE_URL = "https://claude.ai/api";

/** Normalized extra-usage block. Matches the shape src/providers/claude.ts consumes. */
export interface WebExtraUsage {
  isEnabled: boolean;
  monthlyLimitCents: number;
  usedCreditsCents: number;
  currency: string;
  /** Claude.ai org-scoped account email (from /overage_spend_limit). */
  accountEmail?: string;
  /** True when the account has run out of extras credits. */
  outOfCredits?: boolean;
  /** Free-form reason text when extras are disabled, e.g. "manual". */
  disabledReason?: string;
  /** ISO timestamp when extras will auto-re-enable (optional). */
  disabledUntil?: string;
  /** Seat tier from claude.ai's web layer. Alternative to OAuth rateLimitTier. */
  seatTier?: string;
  /** Prepaid balance in currency minor units (cents), if discovered. */
  balanceCents?: number;
  /** True if the user has auto-reload enabled on extras credits. */
  autoReloadEnabled?: boolean;
}

/** Raw Claude.ai organization list item. */
interface OrgRow {
  uuid?: string;
  name?: string;
  capabilities?: string[];
}

/** Raw /overage_spend_limit response (all the fields we've seen). */
interface OverageResponse {
  is_enabled?: boolean;
  monthly_credit_limit?: number; // cents
  used_credits?: number; // cents
  currency?: string;
  account_email?: string;
  account_name?: string;
  account_uuid?: string;
  out_of_credits?: boolean;
  disabled_reason?: string | null;
  disabled_until?: string | null;
  seat_tier?: string;
  limit_type?: string;
  resolved_group_limit?: number | null;
  organization_uuid?: string;
}

/**
 * Raw shape of claude.ai's `/api/organizations/{orgId}/prepaid/credits`
 * endpoint — the one that returns the "Current balance" shown on
 * claude.ai's settings page. Discovered by the user pointing at a
 * real live response on 2026-04-11:
 *
 *   {
 *     "amount": 20480,
 *     "currency": "USD",
 *     "auto_reload_settings": null,
 *     "pending_invoice_amount_cents": null
 *   }
 *
 * `amount` is already in currency minor units (cents). A null
 * `auto_reload_settings` means auto-reload is off; a populated
 * object means it's configured (shape of the object itself is not
 * yet decoded — we only read its truthiness for a boolean signal).
 */
interface PrepaidCreditsResponse {
  amount?: number;
  currency?: string;
  auto_reload_settings?: unknown;
  pending_invoice_amount_cents?: number | null;
}

/**
 * Normalize a raw cookie header string the user pasted from
 * claude.ai DevTools into a plain `name=value; ...` form ready to
 * stick into a `Cookie:` header. Accepts a bunch of copy-paste
 * variants because this is a hand-entered field and users will do
 * anything:
 *
 *   - `Cookie: sessionKey=sk-ant-sid01-...; other=stuff`
 *   - `sessionKey=sk-ant-sid01-...`
 *   - `-H 'Cookie: sessionKey=sk-ant-sid01-...'`  (from curl)
 *   - `-H "Cookie: sessionKey=sk-ant-sid01-..."`
 *
 * Returns the normalized cookie string (never including the `Cookie:`
 * prefix), or `null` if we couldn't find a `sessionKey=sk-ant-*` in
 * the input. We don't log or echo the contents anywhere.
 */
export function normalizeCookieHeader(raw: string): string | null {
  if (!raw) return null;
  let value = raw.trim();
  if (value === "") return null;

  // Strip curl -H / --header prefixes.
  value = value
    .replace(/^\s*-H\s+/i, "")
    .replace(/^\s*--header\s+/i, "")
    .trim();

  // Strip wrapping single or double quotes (curl -H style).
  if (
    (value.startsWith('"') && value.endsWith('"')) ||
    (value.startsWith("'") && value.endsWith("'"))
  ) {
    value = value.slice(1, -1).trim();
  }

  // Strip leading `Cookie:` header name.
  value = value.replace(/^\s*cookie\s*:\s*/i, "").trim();

  // Forgiving input: users often paste just the raw session key
  // without the `sessionKey=` prefix. If the whole input looks
  // like a bare `sk-ant-sid01-...` token with no `=` anywhere,
  // wrap it into a proper cookie pair so downstream code can use
  // it as a Cookie header unchanged.
  if (/^sk-ant-[A-Za-z0-9_-]+$/.test(value)) {
    return `sessionKey=${value}`;
  }

  // Otherwise it must contain a sessionKey=sk-ant-... pair.
  if (!/sessionKey=sk-ant-[^;\s]+/i.test(value)) return null;

  return value;
}

/**
 * In-memory org-UUID cache. The list rarely changes so we only fetch
 * it once per plugin process and reuse the UUID forever. Keyed by
 * the cookie header value so switching accounts invalidates.
 */
const orgIdCache = new Map<string, string>();

/**
 * Fetch the org UUID for a given cookie. CodexBar's selection rule:
 * prefer an org with `"chat"` in `capabilities`, then any non-api-only
 * org, else the first one. We mirror that.
 */
async function fetchOrgId(cookieHeader: string): Promise<string> {
  const cached = orgIdCache.get(cookieHeader);
  if (cached) return cached;

  const orgs = await httpJson<OrgRow[]>({
    url: `${BASE_URL}/organizations`,
    headers: {
      cookie: cookieHeader,
      accept: "application/json",
    },
    timeoutMs: 15_000,
  });

  if (!Array.isArray(orgs) || orgs.length === 0) {
    throw new Error("claude.ai returned no organizations for this cookie");
  }

  const capsSet = (row: OrgRow): Set<string> =>
    new Set((row.capabilities ?? []).map((c) => c.toLowerCase()));

  const chatOrg = orgs.find((o) => o.uuid && capsSet(o).has("chat"));
  const nonApiOnly = orgs.find((o) => {
    if (!o.uuid) return false;
    const caps = capsSet(o);
    if (caps.size === 0) return false;
    if (caps.size === 1 && caps.has("api")) return false;
    return true;
  });
  const firstWithUuid = orgs.find((o) => o.uuid);

  const selected = chatOrg ?? nonApiOnly ?? firstWithUuid;
  if (!selected || !selected.uuid) {
    throw new Error("claude.ai organizations response has no usable uuid");
  }

  orgIdCache.set(cookieHeader, selected.uuid);
  return selected.uuid;
}

/**
 * Optional debug log sink. Wired by the plugin on startup so cookie
 * auto-import decisions show up in Stream Deck's per-plugin log.
 */
let logSink: ((message: string) => void) | undefined;
export function setClaudeWebLogSink(fn: (message: string) => void): void {
  logSink = fn;
}

/**
 * Resolve a cookie header for claude.ai from global settings:
 *
 *   source === "oauth"           → never use cookies, return undefined
 *   source === "cookie" | "both" → use the pasted cookieHeader if set
 *
 * We used to also run a browser auto-scan via @steipete/sweet-cookie
 * here, scraping `sessionKey` out of Edge / Firefox / closed-Chrome
 * legacy cookie stores. It was removed because modern Chrome (127+)
 * uses App-Bound Encryption (v20) that's impossible to decrypt from
 * outside the browser process, which made the auto-scan useless for
 * the primary dev/test environment. The dep + ~400 lines of staged-
 * copy / DPAPI / sweet-cookie plumbing weren't worth keeping alive
 * for a path users weren't hitting. Paste-only is simpler and works
 * on every browser.
 */
function resolveCookieHeader(): string | undefined {
  const cs = getClaudeSettings();
  if (cs.source === "oauth") return undefined;
  const pasted = normalizeCookieHeader(cs.cookieHeader ?? "");
  return pasted ?? undefined;
}

/**
 * Call `/organizations/{orgId}/overage_spend_limit` and normalize
 * the result. Returns `undefined` on any error (missing cookie,
 * 401, 404, network fail, etc.) — extras are best-effort, we never
 * fail the whole Claude fetch because of a web-path hiccup.
 *
 * On success, returns a `WebExtraUsage` that callers can fold into
 * their snapshot. If `is_enabled === false`, we still return the
 * block — the user wants to SEE the limit/balance even when the
 * overage feature is toggled off in claude.ai.
 *
 * The `rawCookieHeader` parameter is kept for back-compat and
 * direct injection (tests); when omitted, we resolve a cookie from
 * global settings via `resolveCookieHeader()`.
 */
export async function fetchClaudeExtraUsage(
  rawCookieHeader?: string | undefined,
): Promise<WebExtraUsage | undefined> {
  const cookieHeader =
    rawCookieHeader !== undefined
      ? normalizeCookieHeader(rawCookieHeader)
      : resolveCookieHeader();
  if (!cookieHeader) return undefined;

  let orgId: string;
  try {
    orgId = await fetchOrgId(cookieHeader);
  } catch {
    return undefined;
  }

  let overage: OverageResponse;
  try {
    overage = await httpJson<OverageResponse>({
      url: `${BASE_URL}/organizations/${orgId}/overage_spend_limit`,
      headers: {
        cookie: cookieHeader,
        accept: "application/json",
      },
      timeoutMs: 15_000,
    });
  } catch (err) {
    // 401 on the overage endpoint usually means the cookie
    // expired. Clear the cached org so the next poll re-fetches.
    // (Previously also invalidated a sweet-cookie cache; that path
    // was removed when we dropped the auto-scan.)
    if (err instanceof HttpError && err.status === 401) {
      orgIdCache.delete(cookieHeader);
    }
    return undefined;
  }

  // Fetch the prepaid credits balance. One known HTTP call to
  // `/api/organizations/{orgId}/prepaid/credits`, best-effort —
  // if it fails (401, 404, network) we just don't include the
  // balance metrics. Runs concurrently with nothing else at this
  // point so we don't bother parallelising.
  const balance = await fetchPrepaidCredits(cookieHeader, orgId);

  const result: WebExtraUsage = {
    isEnabled: overage.is_enabled === true,
    monthlyLimitCents: overage.monthly_credit_limit ?? 0,
    usedCreditsCents: overage.used_credits ?? 0,
    currency: overage.currency ?? "USD",
  };
  if (overage.account_email) result.accountEmail = overage.account_email;
  if (typeof overage.out_of_credits === "boolean") {
    result.outOfCredits = overage.out_of_credits;
  }
  if (overage.disabled_reason) result.disabledReason = overage.disabled_reason;
  if (overage.disabled_until) result.disabledUntil = overage.disabled_until;
  if (overage.seat_tier) result.seatTier = overage.seat_tier;
  if (balance) {
    if (typeof balance.balanceCents === "number") {
      result.balanceCents = balance.balanceCents;
    }
    if (typeof balance.autoReloadEnabled === "boolean") {
      result.autoReloadEnabled = balance.autoReloadEnabled;
    }
  }
  return result;
}

/**
 * Fetch the prepaid credits balance from claude.ai.
 *
 * Endpoint discovered 2026-04-11 against a real live account:
 *   GET https://claude.ai/api/organizations/{orgId}/prepaid/credits
 * Response shape is a flat `PrepaidCreditsResponse` above.
 *
 * Best-effort — any failure quietly returns undefined so the rest
 * of the extras fetch is unaffected.
 */
async function fetchPrepaidCredits(
  cookieHeader: string,
  orgId: string,
): Promise<{ balanceCents?: number; autoReloadEnabled?: boolean } | undefined> {
  let parsed: PrepaidCreditsResponse;
  try {
    parsed = await httpJson<PrepaidCreditsResponse>({
      url: `${BASE_URL}/organizations/${orgId}/prepaid/credits`,
      headers: {
        cookie: cookieHeader,
        accept: "application/json",
      },
      timeoutMs: 10_000,
    });
  } catch (err) {
    if (err instanceof HttpError) {
      logSink?.(
        `claude-web: /prepaid/credits ${err.status} — balance metrics disabled`,
      );
    }
    return undefined;
  }

  const result: { balanceCents?: number; autoReloadEnabled?: boolean } = {};
  if (typeof parsed.amount === "number") {
    result.balanceCents = parsed.amount;
  }
  // auto_reload_settings is an object when auto-reload is configured,
  // null when it's disabled. Use truthiness as a boolean signal.
  if (parsed.auto_reload_settings !== undefined) {
    result.autoReloadEnabled = parsed.auto_reload_settings !== null;
  }

  const displayAmount = result.balanceCents !== undefined
    ? `${(result.balanceCents / 100).toFixed(2)}${parsed.currency ? " " + parsed.currency : ""}`
    : "—";
  logSink?.(
    `claude-web: /prepaid/credits balance=${displayAmount} autoReload=${result.autoReloadEnabled ?? "?"}`,
  );
  return result;
}

/** Clear the org-UUID cache (used when cookie settings change). */
export function clearClaudeWebCache(): void {
  orgIdCache.clear();
}

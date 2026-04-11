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

const BASE_URL = "https://claude.ai/api";

/** Normalized extra-usage block. Matches the shape src/providers/claude.ts consumes. */
export interface WebExtraUsage {
  isEnabled: boolean;
  monthlyLimitCents: number;
  usedCreditsCents: number;
  currency: string;
}

/** Raw Claude.ai organization list item. */
interface OrgRow {
  uuid?: string;
  name?: string;
  capabilities?: string[];
}

/** Raw /overage_spend_limit response. */
interface OverageResponse {
  is_enabled?: boolean;
  monthly_credit_limit?: number; // cents
  used_credits?: number; // cents
  currency?: string;
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

  // Must contain a sessionKey=sk-ant-... pair to be usable.
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
 * Call `/organizations/{orgId}/overage_spend_limit` and normalize
 * the result. Returns `undefined` on any error (missing cookie,
 * 401, 404, network fail, etc.) — extras are best-effort, we never
 * fail the whole Claude fetch because of a web-path hiccup.
 *
 * On success, returns a `WebExtraUsage` that callers can fold into
 * their snapshot. If `is_enabled === false`, we still return the
 * block — the user wants to SEE the limit/balance even when the
 * overage feature is toggled off in claude.ai.
 */
export async function fetchClaudeExtraUsage(
  rawCookieHeader: string | undefined,
): Promise<WebExtraUsage | undefined> {
  if (!rawCookieHeader) return undefined;
  const cookieHeader = normalizeCookieHeader(rawCookieHeader);
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
    // 401 on the overage endpoint usually means the cookie expired.
    // Clear the cached org so a new cookie triggers a fresh lookup.
    if (err instanceof HttpError && err.status === 401) {
      orgIdCache.delete(cookieHeader);
    }
    return undefined;
  }

  return {
    isEnabled: overage.is_enabled === true,
    monthlyLimitCents: overage.monthly_credit_limit ?? 0,
    usedCreditsCents: overage.used_credits ?? 0,
    currency: overage.currency ?? "USD",
  };
}

/** Clear the org-UUID cache (used when cookie settings change). */
export function clearClaudeWebCache(): void {
  orgIdCache.clear();
}

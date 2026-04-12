/**
 * GitHub Copilot usage provider.
 *
 * Auth: GitHub OAuth token — checks ~/.config/github-copilot/hosts.json
 *       and apps.json (written by `gh auth login` / `gh copilot`).
 *       Falls back to GITHUB_TOKEN env var.
 *
 * Endpoint: GET https://api.github.com/copilot_internal/user
 * Returns: quota_snapshots with premium_interactions + chat usage.
 *
 * Reference: tmp/CodexBar Sources/CodexBarCore/Providers/Copilot/
 */

import { copilotHostsPath, copilotAppsPath } from "../util/paths.ts";
import { httpJson, HttpError } from "../util/http.ts";
import { CODEXBAR_BRAND_COLORS } from "./brand-colors.ts";
import type {
  MetricValue,
  Provider,
  ProviderContext,
  ProviderSnapshot,
} from "./types.ts";
import { existsSync } from "node:fs";
import { readFile } from "node:fs/promises";

const USAGE_URL = "https://api.github.com/copilot_internal/user";

interface QuotaSnapshot {
  entitlement?: number;
  remaining?: number;
  percent_remaining?: number;
  quota_id?: string;
}

interface CopilotUsageResponse {
  copilot_plan?: string;
  quota_reset_date?: string;
  quota_snapshots?: {
    premium_interactions?: QuotaSnapshot | null;
    chat?: QuotaSnapshot | null;
  } | null;
}

/**
 * Try to extract a GitHub token from copilot credential files.
 * These files map `github.com` → an oauth_token.
 */
async function loadGitHubToken(): Promise<string | undefined> {
  // Try env var first
  const envToken = process.env["GITHUB_TOKEN"]?.trim();
  if (envToken) return envToken;

  // Try hosts.json (older gh CLI)
  for (const path of [copilotHostsPath(), copilotAppsPath()]) {
    if (!existsSync(path)) continue;
    try {
      const raw = await readFile(path, "utf8");
      const parsed = JSON.parse(raw) as Record<string, unknown>;
      // hosts.json shape: { "github.com": { "oauth_token": "gho_..." } }
      // Also check { "github.com:oauth_token": "gho_..." } (flat variant)
      for (const [key, val] of Object.entries(parsed)) {
        if (!key.includes("github.com")) continue;
        if (typeof val === "string") return val;
        if (val && typeof val === "object") {
          const obj = val as Record<string, unknown>;
          const token = obj["oauth_token"] ?? obj["token"];
          if (typeof token === "string" && token.trim()) return token.trim();
        }
      }
    } catch {
      continue;
    }
  }

  return undefined;
}

export class CopilotProvider implements Provider {
  readonly id = "copilot";
  readonly name = "Copilot";
  readonly brandColor = CODEXBAR_BRAND_COLORS.copilot ?? "#6e40c9";
  readonly metricIds = [
    "premium-percent",
    "chat-percent",
  ] as const;

  async fetch(_ctx?: ProviderContext): Promise<ProviderSnapshot> {
    const token = await loadGitHubToken();
    if (!token) {
      return {
        providerId: this.id,
        providerName: this.name,
        source: "none",
        metrics: [],
        status: "unknown",
        error: "No GitHub token found. Run `gh auth login` or set GITHUB_TOKEN.",
      };
    }

    let response: CopilotUsageResponse;
    try {
      response = await httpJson<CopilotUsageResponse>({
        url: USAGE_URL,
        headers: {
          authorization: `token ${token}`,
          accept: "application/json",
          "editor-version": "vscode/1.96.2",
          "editor-plugin-version": "copilot-chat/0.26.7",
          "user-agent": "GitHubCopilotChat/0.26.7",
          "x-github-api-version": "2025-04-01",
        },
        timeoutMs: 15_000,
      });
    } catch (err) {
      if (err instanceof HttpError && (err.status === 401 || err.status === 403)) {
        return {
          providerId: this.id,
          providerName: this.name,
          source: "token",
          metrics: [],
          status: "unknown",
          error: "GitHub token unauthorized for Copilot. Run `gh auth login`.",
        };
      }
      throw err;
    }

    const metrics: MetricValue[] = [];
    const now = new Date();
    const snapshots = response.quota_snapshots;

    if (snapshots?.premium_interactions) {
      const q = snapshots.premium_interactions;
      const used = typeof q.percent_remaining === "number"
        ? 100 - q.percent_remaining
        : 0;
      const remaining = 100 - used;
      const m: MetricValue = {
        id: "premium-percent",
        label: "PREMIUM",
        name: "Premium interactions remaining",
        value: Math.round(remaining),
        numericValue: remaining,
        numericUnit: "percent",
        unit: "%",
        ratio: remaining / 100,
        direction: "up",
        updatedAt: now,
      };
      // Copilot exposes raw entitlement/remaining counts
      if (typeof q.entitlement === "number" && typeof q.remaining === "number") {
        m.rawCount = q.remaining;
        m.rawMax = q.entitlement;
      }
      metrics.push(m);
    }

    if (snapshots?.chat) {
      const q = snapshots.chat;
      const used = typeof q.percent_remaining === "number"
        ? 100 - q.percent_remaining
        : 0;
      const remaining = 100 - used;
      const m: MetricValue = {
        id: "chat-percent",
        label: "CHAT",
        name: "Chat interactions remaining",
        value: Math.round(remaining),
        numericValue: remaining,
        numericUnit: "percent",
        unit: "%",
        ratio: remaining / 100,
        direction: "up",
        updatedAt: now,
      };
      if (typeof q.entitlement === "number" && typeof q.remaining === "number") {
        m.rawCount = q.remaining;
        m.rawMax = q.entitlement;
      }
      metrics.push(m);
    }

    const planName = response.copilot_plan
      ? `Copilot ${response.copilot_plan.charAt(0).toUpperCase()}${response.copilot_plan.slice(1)}`
      : this.name;

    return {
      providerId: this.id,
      providerName: planName,
      source: "token",
      metrics,
      status: "operational",
    };
  }
}

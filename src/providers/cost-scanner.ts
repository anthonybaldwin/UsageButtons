/**
 * Local cost scanner.
 *
 * Scans JSONL session logs written by the Claude and Codex CLIs to
 * compute "Today" and "Last 30 days" token counts and dollar costs.
 * This is the same data source CodexBar and Win-CodexBar use for
 * their "Cost" section — it's all local, no API calls.
 *
 * Log locations:
 *   Claude: ~/.claude/projects/ ** /*.jsonl
 *           (events with type:"assistant", message.usage.{input_tokens, ...})
 *   Codex:  ~/.codex/sessions/YYYY/MM/DD/*.jsonl
 *           (events with event_msg.type:"token_count")
 *
 * Pricing is hardcoded per-model (matches Win-CodexBar's tables).
 * Cached per-provider with a 10-minute TTL so we don't re-scan the
 * filesystem on every poll tick.
 */

import { readdir, stat, readFile } from "node:fs/promises";
import { resolve, join } from "node:path";
import { existsSync } from "node:fs";
import {
  claudeProjectsDir,
  codexSessionsDir,
} from "../util/paths.ts";

// ── Pricing tables (USD per 1M tokens) ─────────────────────────

interface ModelPricing {
  input: number;
  cachedInput: number;
  cacheWrite: number;
  output: number;
}

const CLAUDE_PRICING: Record<string, ModelPricing> = {
  opus: { input: 15, cachedInput: 1.5, cacheWrite: 18.75, output: 75 },
  sonnet: { input: 3, cachedInput: 0.3, cacheWrite: 3.75, output: 15 },
  haiku: { input: 0.25, cachedInput: 0.03, cacheWrite: 0.3, output: 1.25 },
};

const CODEX_PRICING: Record<string, ModelPricing> = {
  "gpt-4o": { input: 2.5, cachedInput: 1.25, cacheWrite: 0, output: 10 },
  "gpt-4o-mini": { input: 0.15, cachedInput: 0.075, cacheWrite: 0, output: 0.6 },
  "gpt-4-turbo": { input: 10, cachedInput: 5, cacheWrite: 0, output: 30 },
  "gpt-4": { input: 30, cachedInput: 15, cacheWrite: 0, output: 60 },
  o1: { input: 15, cachedInput: 7.5, cacheWrite: 0, output: 60 },
  "o1-mini": { input: 3, cachedInput: 1.5, cacheWrite: 0, output: 12 },
  "o3": { input: 10, cachedInput: 2.5, cacheWrite: 0, output: 40 },
  "o3-mini": { input: 1.1, cachedInput: 0.55, cacheWrite: 0, output: 4.4 },
  "o4-mini": { input: 1.1, cachedInput: 0.275, cacheWrite: 0, output: 4.4 },
};

function resolveClaudePricing(model: string): ModelPricing {
  const lower = model.toLowerCase();
  if (lower.includes("opus")) return CLAUDE_PRICING["opus"]!;
  if (lower.includes("haiku")) return CLAUDE_PRICING["haiku"]!;
  // Default to sonnet for any unrecognized Claude model
  return CLAUDE_PRICING["sonnet"]!;
}

function resolveCodexPricing(model: string): ModelPricing {
  const lower = model.toLowerCase();
  for (const [key, pricing] of Object.entries(CODEX_PRICING)) {
    if (lower.includes(key)) return pricing;
  }
  // Default to gpt-4o for unrecognized
  return CODEX_PRICING["gpt-4o"]!;
}

function tokenCost(tokens: number, pricePerMillion: number): number {
  return (tokens / 1_000_000) * pricePerMillion;
}

// ── Cost summary types ─────────────────────────────────────────

export interface CostSummary {
  todayCostUsd: number;
  todayTokens: number;
  last30DaysCostUsd: number;
  last30DaysTokens: number;
  updatedAt: Date;
}

const EMPTY_SUMMARY: CostSummary = {
  todayCostUsd: 0,
  todayTokens: 0,
  last30DaysCostUsd: 0,
  last30DaysTokens: 0,
  updatedAt: new Date(),
};

// ── Internal cache ─────────────────────────────────────────────

const SCAN_CACHE_TTL_MS = 10 * 60 * 1000; // 10 min

interface CachedScan {
  summary: CostSummary;
  scannedAt: number;
}

const scanCache = new Map<string, CachedScan>();

// ── File scanning ──────────────────────────────────────────────

/** Recursively collect all .jsonl files under a directory. */
async function collectJsonlFiles(
  dir: string,
  maxDepth: number = 6,
): Promise<string[]> {
  if (maxDepth <= 0) return [];
  const results: string[] = [];
  let entries: Awaited<ReturnType<typeof readdir>>;
  try {
    entries = await readdir(dir, { withFileTypes: true });
  } catch {
    return [];
  }
  for (const entry of entries) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      results.push(...(await collectJsonlFiles(full, maxDepth - 1)));
    } else if (entry.isFile() && entry.name.endsWith(".jsonl")) {
      results.push(full);
    }
  }
  return results;
}

/** Filter files by modification time (skip files older than cutoff). */
async function filterByMtime(
  files: string[],
  cutoffMs: number,
): Promise<string[]> {
  const results: string[] = [];
  for (const f of files) {
    try {
      const s = await stat(f);
      if (s.mtimeMs >= cutoffMs) results.push(f);
    } catch {
      // Skip inaccessible files
    }
  }
  return results;
}

// ── Claude JSONL parsing ───────────────────────────────────────

interface ClaudeUsageEvent {
  type?: string;
  message?: {
    model?: string;
    usage?: {
      input_tokens?: number;
      output_tokens?: number;
      cache_creation_input_tokens?: number;
      cache_read_input_tokens?: number;
    };
  };
}

interface TokenBucket {
  tokens: number;
  costUsd: number;
}

function parseClaudeFile(
  content: string,
  todayCutoffMs: number,
  fileMtimeMs: number,
): { today: TokenBucket; monthly: TokenBucket } {
  const today: TokenBucket = { tokens: 0, costUsd: 0 };
  const monthly: TokenBucket = { tokens: 0, costUsd: 0 };
  // Heuristic: if the file was last modified today, its events
  // contribute to both buckets. Otherwise only to the 30-day bucket.
  // (We don't have per-event timestamps in Claude JSONL, so we use
  // file mtime as a proxy — same approach as Win-CodexBar.)
  const isToday = fileMtimeMs >= todayCutoffMs;

  for (const line of content.split("\n")) {
    if (!line.trim()) continue;
    let event: ClaudeUsageEvent;
    try {
      event = JSON.parse(line);
    } catch {
      continue;
    }
    if (event.type !== "assistant" || !event.message?.usage) continue;

    const usage = event.message.usage;
    const model = event.message.model ?? "sonnet";
    const pricing = resolveClaudePricing(model);

    const inputTokens = usage.input_tokens ?? 0;
    const outputTokens = usage.output_tokens ?? 0;
    const cacheCreate = usage.cache_creation_input_tokens ?? 0;
    const cacheRead = usage.cache_read_input_tokens ?? 0;
    const totalTokens = inputTokens + outputTokens + cacheCreate + cacheRead;

    const cost =
      tokenCost(inputTokens, pricing.input) +
      tokenCost(outputTokens, pricing.output) +
      tokenCost(cacheCreate, pricing.cacheWrite) +
      tokenCost(cacheRead, pricing.cachedInput);

    monthly.tokens += totalTokens;
    monthly.costUsd += cost;
    if (isToday) {
      today.tokens += totalTokens;
      today.costUsd += cost;
    }
  }

  return { today, monthly };
}

// ── Codex JSONL parsing ────────────────────────────────────────

interface CodexUsageEvent {
  type?: string;
  model?: string;
  event_msg?: {
    type?: string;
    input_tokens?: number;
    cached_input_tokens?: number;
    output_tokens?: number;
  };
}

function parseCodexFile(
  content: string,
  todayCutoffMs: number,
  fileMtimeMs: number,
): { today: TokenBucket; monthly: TokenBucket } {
  const today: TokenBucket = { tokens: 0, costUsd: 0 };
  const monthly: TokenBucket = { tokens: 0, costUsd: 0 };
  const isToday = fileMtimeMs >= todayCutoffMs;

  for (const line of content.split("\n")) {
    if (!line.trim()) continue;
    let event: CodexUsageEvent;
    try {
      event = JSON.parse(line);
    } catch {
      continue;
    }
    if (event.event_msg?.type !== "token_count") continue;

    const msg = event.event_msg;
    const model = event.model ?? "gpt-4o";
    const pricing = resolveCodexPricing(model);

    const inputTokens = msg.input_tokens ?? 0;
    const cachedTokens = msg.cached_input_tokens ?? 0;
    const outputTokens = msg.output_tokens ?? 0;
    const totalTokens = inputTokens + cachedTokens + outputTokens;

    const cost =
      tokenCost(inputTokens, pricing.input) +
      tokenCost(outputTokens, pricing.output) +
      tokenCost(cachedTokens, pricing.cachedInput);

    monthly.tokens += totalTokens;
    monthly.costUsd += cost;
    if (isToday) {
      today.tokens += totalTokens;
      today.costUsd += cost;
    }
  }

  return { today, monthly };
}

// ── Public API ─────────────────────────────────────────────────

async function scanProvider(
  logDir: string | undefined,
  parser: (
    content: string,
    todayCutoffMs: number,
    fileMtimeMs: number,
  ) => { today: TokenBucket; monthly: TokenBucket },
): Promise<CostSummary> {
  if (!logDir || !existsSync(logDir)) return { ...EMPTY_SUMMARY, updatedAt: new Date() };

  const now = Date.now();
  const todayCutoff = new Date();
  todayCutoff.setHours(0, 0, 0, 0);
  const todayCutoffMs = todayCutoff.getTime();
  const thirtyCutoffMs = now - 30 * 24 * 60 * 60 * 1000;

  const allFiles = await collectJsonlFiles(logDir);
  const recentFiles = await filterByMtime(allFiles, thirtyCutoffMs);

  const totals = { today: { tokens: 0, costUsd: 0 }, monthly: { tokens: 0, costUsd: 0 } };

  // Read files in parallel batches to avoid overwhelming I/O
  const BATCH_SIZE = 20;
  for (let i = 0; i < recentFiles.length; i += BATCH_SIZE) {
    const batch = recentFiles.slice(i, i + BATCH_SIZE);
    const results = await Promise.all(
      batch.map(async (filePath) => {
        try {
          const [content, s] = await Promise.all([
            readFile(filePath, "utf8"),
            stat(filePath),
          ]);
          return parser(content, todayCutoffMs, s.mtimeMs);
        } catch {
          return { today: { tokens: 0, costUsd: 0 }, monthly: { tokens: 0, costUsd: 0 } };
        }
      }),
    );
    for (const r of results) {
      totals.today.tokens += r.today.tokens;
      totals.today.costUsd += r.today.costUsd;
      totals.monthly.tokens += r.monthly.tokens;
      totals.monthly.costUsd += r.monthly.costUsd;
    }
  }

  return {
    todayCostUsd: totals.today.costUsd,
    todayTokens: totals.today.tokens,
    last30DaysCostUsd: totals.monthly.costUsd,
    last30DaysTokens: totals.monthly.tokens,
    updatedAt: new Date(),
  };
}

/**
 * Get Claude cost summary from local JSONL logs.
 * Cached for 10 minutes.
 */
export async function getClaudeCosts(): Promise<CostSummary> {
  const cached = scanCache.get("claude");
  if (cached && Date.now() - cached.scannedAt < SCAN_CACHE_TTL_MS) {
    return cached.summary;
  }

  const dir = claudeProjectsDir();
  const summary = await scanProvider(dir, parseClaudeFile);
  scanCache.set("claude", { summary, scannedAt: Date.now() });
  return summary;
}

/**
 * Get Codex cost summary from local JSONL logs.
 * Cached for 10 minutes.
 */
export async function getCodexCosts(): Promise<CostSummary> {
  const cached = scanCache.get("codex");
  if (cached && Date.now() - cached.scannedAt < SCAN_CACHE_TTL_MS) {
    return cached.summary;
  }

  const dir = codexSessionsDir();
  const summary = await scanProvider(dir, parseCodexFile);
  scanCache.set("codex", { summary, scannedAt: Date.now() });
  return summary;
}

/** Format a token count for display: "15K", "218M", "1.2B". */
export function formatTokenCount(tokens: number): string {
  if (tokens >= 1_000_000_000) return `${(tokens / 1_000_000_000).toFixed(1)}B`;
  if (tokens >= 1_000_000) return `${Math.round(tokens / 1_000_000)}M`;
  if (tokens >= 1_000) return `${Math.round(tokens / 1_000)}K`;
  return String(Math.round(tokens));
}

/** Optional debug sink. */
let logSink: ((msg: string) => void) | undefined;
export function setCostScannerLogSink(fn: (msg: string) => void): void {
  logSink = fn;
}

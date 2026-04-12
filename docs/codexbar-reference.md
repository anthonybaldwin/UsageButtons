# CodexBar reference — implementation plan input

Distilled from a thorough read of the CodexBar repo at
`tmp/CodexBar/` (upstream:
https://github.com/steipete/CodexBar). This file is our north star for
porting CodexBar's provider concepts into the Stream Deck plugin. It is
**not** a spec — CodexBar evolves, and we should refresh this doc by
running `./scripts/sync-codexbar.sh` and re-reviewing before implementing a
new provider.

> **Don't vendor CodexBar code.** Everything here is design guidance.
> Every provider fetcher in `src/providers/` is to be written from
> scratch in TypeScript against public endpoints / local files.

## 1. Provider matrix — what we can ship on Windows

| Provider | Primary data path | Cross-platform | Notes |
|---|---|---|---|
| **OpenRouter** | API token → `GET https://openrouter.ai/api/v1/credits`, `/key` | ✅ | easiest first provider; no macOS deps |
| **Copilot** | GitHub device flow → `GET https://api.github.com/copilot_internal/user` | ✅ | `quotaSnapshots.premiumInteractions.remainingFraction`, `quotaSnapshots.chat.remainingFraction` |
| **z.ai** | API key → `GET https://api.z.ai/api/monitor/usage/quota/limit` | ✅ | region-aware host (`Z_AI_API_HOST`) |
| **Kimi K2** | API key → `GET https://kimi-k2.ai/api/user/credits` | ✅ | |
| **Warp** | API key → `POST https://app.warp.dev/graphql/v2?op=GetRequestLimitInfo` | ✅ | |
| **Kilo** | API key → `POST https://app.kilo.ai/api/trpc` + CLI fallback | ✅ | CLI fallback reads `~/.local/share/kilo/auth.json` |
| **Gemini** | `~/.gemini/oauth_creds.json` + `POST https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota` | ✅ | depends on Gemini CLI being logged in |
| **Vertex AI** | gcloud ADC → Cloud Monitoring API | ✅ | needs `gcloud` on PATH |
| **Claude (OAuth API)** | token from `%USERPROFILE%/.claude/.credentials.json` → `GET https://api.anthropic.com/api/oauth/usage` (header `anthropic-beta: oauth-2025-04-20`) | ✅ | macOS reads the token from Keychain first; on Windows the file fallback **is** our primary source |
| **Codex (OAuth API)** | token from `%USERPROFILE%/.codex/auth.json` → `GET https://chatgpt.com/backend-api/wham/usage` | ✅ | |
| **Kimi** | `KIMI_AUTH_TOKEN` env (manual) → `POST https://www.kimi.com/apiv2/.../GetUsages` | ✅ | auto-import of `kimi-auth` cookie is macOS-only for now |
| **Alibaba Coding Plan** | `ALIBABA_CODING_PLAN_API_KEY` (manual) → remains API | ✅ | |
| **MiniMax** | API / manual `MINIMAX_COOKIE` env → remains API | ✅ | |
| **JetBrains AI** | local XML `%APPDATA%/JetBrains/*/options/AIAssistantQuotaManager2.xml` | ⚠️ | path differs from macOS; detection logic is portable |
| **Codex (CLI RPC)** | `codex ... app-server` JSON-RPC | ⚠️ | requires `codex` CLI on PATH and ConPTY for the PTY fallback |
| **Claude (CLI PTY)** | `claude /usage` in a PTY | ⚠️ | needs a Windows-capable PTY (node-pty/ConPTY); low priority while OAuth API works |
| **Cursor** | browser cookies → `cursor.com/api/usage-summary` | ⚠️ | Windows Chrome cookie DB parse is doable but out of scope; manual cookie header first |
| **OpenCode** | browser cookies → `opencode.ai/_server` RPC | ⚠️ | manual cookie header first |
| **Droid/Factory** | WorkOS cookies / tokens | ⚠️ | manual header first |
| **Augment** | `auggie account status` CLI | ⚠️ | works if `auggie` installed |
| **Amp** | browser cookies → settings HTML scrape | ⚠️ | manual only |
| **Ollama** | browser cookies → settings HTML scrape | ⚠️ | manual only |
| **Perplexity** | browser cookies | ⚠️ | manual only |
| **Antigravity** | LSP port detection via `lsof`/`ps` — probes `language_server_macos` process | ❌ | **macOS-only.** Hardcodes process name `language_server_macos`, uses `ps`/`lsof` for port detection + localhost TLS probe. No Windows equivalent exists. CodexBar's 2026-04-12 commit (69a715f) added TLS certificate handling but didn't change the macOS-only architecture. |

**Shipping order:** the ✅ rows first, in roughly the order listed
above (start with OpenRouter — cleanest contract — then Claude and
Codex since they're the main use case).

## 2. Per-provider stats we care about

Every provider exposes some subset of these metrics. Our plugin's
`MetricValue` schema in `src/providers/types.ts` is deliberately loose
so any of these can be bound to a button.

| Metric id | Meaning | Fill direction | Reset cadence |
|---|---|---|---|
| `session-percent` | 5h / session window remaining | up (as remaining) | 5h |
| `weekly-percent` | 7-day window remaining | up | 7d |
| `weekly-opus-percent` / `weekly-sonnet-percent` | Claude model-specific weekly | up | 7d |
| `pro-percent` / `flash-percent` / `flash-lite-percent` | Gemini model-family remaining | up | 1d |
| `credits-remaining` | credits left as an absolute number | up | n/a or purchase |
| `credits-percent` | credits / limit | up | monthly or n/a |
| `plan-percent` | monthly plan usage | up | 30d |
| `code-review-percent` | Codex OpenAI dashboard extras | up | 5h |
| `cost-today` / `cost-30d` | local JSONL log scan — token costs in USD | none (ratio=1) | n/a |
| `overage-spent-usd` / `overage-limit-usd` | Claude Extra usage | down | monthly |
| `premium-percent` / `chat-percent` | Copilot premium interactions / chat remaining | up | monthly |
| `total-percent` / `auto-percent` / `api-percent` | Cursor plan usage remaining | up | billing cycle |
| `ondemand-spent` | Cursor on-demand spend in USD | up | billing cycle |
| `tokens-percent` / `mcp-percent` | z.ai token / MCP usage remaining | up | variable |
| `credits-balance` / `credits-used` | OpenRouter / Kimi K2 credit balance | up | n/a |
| `credits-percent` / `bonus-credits` | Warp request credits + bonus | up | refresh cycle |
| `status-indicator` | operational / degraded / outage | none | n/a |
| `reset-countdown` | seconds until next reset | down | matches window |

### Pace (derived metric)

The weekly meter's `caption` field now carries a pace label when the
reset countdown is hidden: "On pace", "Behind (-X%)", or "Ahead (+X%)".
Computed as `delta = actualUsed% - (elapsed / windowDuration * 100)`.
Applied to both Claude and Codex weekly metrics.

All "remaining" metrics render with fill **up** (button fills as you
have more runway) — the intuitive "tank of gas" feel. "Used" metrics
can render inverted or flipped to **down**. This is configurable per
key via the Property Inspector.

## 3. JSON contract to stay compatible with

We deliberately mirror the CodexBar CLI JSON schema
(`tmp/CodexBar/docs/cli.md`) so our fetchers can be drop-in replacements
and, optionally, we can read the CLI JSON directly on macOS.

```ts
interface UsageSnapshot {
  primary?:   RateWindow;   // session / 5h
  secondary?: RateWindow;   // weekly
  tertiary?:  RateWindow;   // model-specific / extra
  updatedAt: string;        // ISO-8601
  identity?: {
    providerID: string;
    accountEmail?: string;
    accountOrganization?: string;
    loginMethod?: string;
  };
}
interface RateWindow {
  usedPercent: number;        // 0..100
  windowMinutes?: number;     // 300 (5h), 10080 (7d), …
  resetsAt?: string;          // ISO-8601
  resetDescription?: string;  // human
}
interface CreditsSnapshot { remaining: number; updatedAt: string; }
interface ProviderStatusPayload {
  indicator: "none" | "minor" | "major" | "critical" | "maintenance" | "unknown";
  description?: string;
  updatedAt?: string;
  url?: string;
}
```

Our internal `ProviderSnapshot` (see `src/providers/types.ts`) is a
flatter "list of MetricValues" — when we translate from a CodexBar-shape
payload, we fan out `primary`/`secondary`/`tertiary` into individual
metrics with stable ids like `session-percent` and `weekly-percent`.

## 4. Config sharing with CodexBar

CodexBar stores per-provider config at `~/.codexbar/config.json`
(Windows equivalent: `%USERPROFILE%\.codexbar\config.json`). We will
**read-only** consume this file when it exists — it already contains
the user's enabled providers, API keys, cookie headers, and token
accounts. If missing, our plugin falls back to Stream Deck action
settings (per-button, stored by the SD software) and env vars.

```jsonc
{
  "version": 1,
  "providers": [
    {
      "id": "claude",
      "enabled": true,
      "source": "auto",
      "cookieSource": "auto",
      "cookieHeader": null,
      "apiKey": null,
      "tokenAccounts": {
        "version": 1,
        "activeIndex": 0,
        "accounts": [
          { "id": "...", "label": "user@example.com", "token": "sk-ant-…", "addedAt": 0, "lastUsed": 0 }
        ]
      }
    }
  ]
}
```

## 5. Icon rendering — adapt to 144×144

CodexBar renders an 18×18-point monochrome template image with two
horizontal bars (top thicker = session, bottom hairline = weekly). The
Stream Deck canvas is ~8× the area, so we:

1. Use one button = one stat (we don't squeeze session + weekly onto
   the same face; that's what the second button on the stream deck is
   for).
2. Render a rounded square card (`src/render.ts`) with a solid fill
   rectangle whose height is `ratio * 144`. That's the "background
   slowly fills or de-fills" effect.
3. Show the label on top, the big number in the middle, and the reset
   countdown as sub-value.
4. Dim the entire SVG (`opacity=0.45`) when data is stale.
5. Optional status-indicator overlay (small colored dot top-right)
   when the provider's status page reports an incident.

**Colors:** default palette is a dark card (`#111827`) with blue fill
(`#3b82f6`) and green for remaining metrics (`#10b981`). Per-provider
branding lives in `src/providers/<id>.ts` — e.g., Claude orange,
OpenAI green, Gemini blue. Keep it configurable via action settings.

## 6. Refresh loop

- **Default poll cadence:** 60s (CodexBar defaults to 5 minutes; we
  poll faster because stream deck feels broken when the button doesn't
  reflect immediate user activity, but every provider fetcher caps its
  own rate at whatever its API allows).
- **Staggering:** multiple keys bound to the same (provider, metric)
  pair share the same fetch result — we cache snapshots per-provider
  for `pollIntervalMs * 0.8` before a re-fetch.
- **On keyDown:** force-refresh that single key's provider bypassing
  cache.
- **Cache on disk:** `%APPDATA%/UsageButtons/cache/<provider>.json`
  (and the macOS equivalent under `~/Library/Caches/`). Used to bridge
  plugin restarts so the button isn't blank for the first poll cycle.
- **Stale detection:** if `updatedAt` is > 12h old or the last fetch
  errored, mark `stale: true` and render dimmed.

## 7. Status polling — keep it cheap

CodexBar polls Statuspage.io and the Google Workspace incident feed
for each enabled provider. We'll do the same but on a **separate, much
slower** timer (every 5 minutes is fine) since incidents don't shift
by the second. Status is a tiny colored dot overlay on the button, not
its own row.

## 8. Gotchas to remember (from code review)

1. **Claude OAuth Keychain prompt storm** — on Windows we avoid this
   entirely by reading `~/.claude/.credentials.json` directly. No
   popups.
2. **Cost-scan double-counting** — when we implement local JSONL
   scanning for Claude cost, dedup on `message.id + requestId` across
   both `~/.claude/projects/` and `~/.config/claude/projects/`.
3. **Vertex AI model names** — use `@20251101` vs `-20251101` as the
   Vertex-vs-Anthropic signal when parsing cost logs.
4. **Cursor percent fields ≠ request-count fields** — trust the
   dashboard's percent field when it's present; only derive from
   request counts as a fallback.
5. **Codex CLI PTY timing is brittle** — give it ≥5s and don't retry
   in a tight loop; keep session alive where possible.
6. **Browser cookie imports are slow & fragile** — cache aggressively
   (15 min TTL) and prefer API-key / env-var paths on Windows. This is
   one of our explicit deprioritizations.
7. **Gemini model names change between versions** — fuzzy-match by
   family (`pro`, `flash`, `flash-lite`) not exact string.
8. **Rate-limit parsing from CLI `/usage` output breaks on ANSI** —
   strip ANSI first and use flexible regex; prefer API paths.
9. **Multi-account switches don't invalidate cached tokens** — always
   key the cache by (providerId, accountLabel).
10. **Perplexity has three credit pools** — recurring, bonus,
    purchased — sum them for the top-line but expose each as its own
    metric id so buttons can target individually.

## 9. What we deliberately do NOT copy

- Sparkle auto-updates (Stream Deck plugins are updated via the Store)
- WidgetKit widgets (Stream Deck buttons ARE our widget)
- WebKit and Safari binarycookies parsing
- Keychain/CredentialManager storage — we start with plaintext config
  and env vars; secrets handling comes later
- The Merge-Icons + Overview Tab UI mode — buttons are discrete
- The subtle "critter face" icon art — nice but not load-bearing
- Battery-saver polling gates — Stream Deck is always plugged in

## 10. Implementation phases

1. **Phase 0** _(this commit)_: scaffold + mock provider — button
   renders fill animation end-to-end.
2. **Phase 1**: Claude OAuth API fetcher (reads
   `~/.claude/.credentials.json`). First real stat on a button.
3. **Phase 2**: Codex OAuth API fetcher, OpenRouter, Copilot, Warp.
   These are all pure-HTTP/API-key providers with no macOS baggage.
4. **Phase 3**: Status polling subsystem + per-button status overlay.
5. **Phase 4**: Config-file integration — read `~/.codexbar/config.json`
   if present.
6. **Phase 5**: Local cost-scan (`~/.claude/projects/` and
   `~/.codex/sessions/`) for USD-per-day metrics.
7. **Phase 6**: CLI-fallback providers (Codex CLI via ConPTY, Kiro,
   Augment `auggie`).
8. **Phase 7**: Manual-cookie-header providers for the rest
   (Cursor, OpenCode, Droid, Amp, Ollama, Perplexity).
9. **Phase 8 _(maybe)_**: browser cookie auto-import on Windows.

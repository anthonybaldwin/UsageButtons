# Providers

Usage Buttons has one Stream Deck action per provider. Each action can show
one of that provider's metrics; the metric labels below match the Property
Inspector dropdown.

Browser-backed auth means the metric is fetched through the Usage Buttons
Helper extension using the user's logged-in browser session. Cookies stay in
the browser; the plugin only receives response bodies.

Some providers only return a subset of their possible metrics when the upstream
account or API response includes that quota lane.

| Provider | Auth methods | Available metrics |
|---|---|---|
| Abacus AI | Usage Buttons Helper from `apps.abacus.ai`. | Monthly compute credits remaining %. |
| Alibaba | Usage Buttons Helper from the Alibaba Cloud console, or Alibaba Coding Plan API key from the Provider tab / `ALIBABA_CODING_PLAN_API_KEY`. Optional region/endpoint overrides: `ALIBABA_CODING_PLAN_REGION`, `ALIBABA_CODING_PLAN_HOST`, `ALIBABA_CODING_PLAN_QUOTA_URL`. | 5-hour quota remaining %, weekly quota remaining %, monthly quota remaining %. |
| Amp | Usage Buttons Helper from `ampcode.com/settings`. | Amp Free remaining %. |
| Antigravity | Local Antigravity language server. Launch Antigravity first; optional overrides include `ANTIGRAVITY_PORT` and `ANTIGRAVITY_CSRF_TOKEN`. | Claude quota remaining %, Gemini Pro quota remaining %, Gemini Flash quota remaining %. |
| Augment | `auggie account status`, falling back to Usage Buttons Helper from `app.augmentcode.com`. | Credits remaining %. |
| Claude | Local OAuth credentials from `~/.claude/.credentials.json`; Claude web extras use the Usage Buttons Helper when connected. | Session % remaining (5h), session pace (5h burn rate), weekly % remaining, weekly pace (7d burn rate), Sonnet % remaining (weekly), Sonnet pace (7d burn rate), Opus % remaining (weekly), Opus pace (7d burn rate), Claude Design % remaining (weekly), Claude Design pace (7d burn rate), Daily Routines % remaining (weekly), Daily Routines pace (7d burn rate), Extra usage ON/OFF, Extra usage headroom %, Extra usage monthly limit ($), Extra usage spent ($), prepaid balance ($), Auto-reload ON/OFF, cost today (local logs), cost last 30 days (local logs). |
| Codex | Usage Buttons Helper when connected, otherwise OAuth from `~/.codex/auth.json`. Optional ChatGPT API host override in the Provider tab / `CODEX_CHATGPT_BASE_URL`. | Session % remaining (5h), session pace (5h burn rate), weekly % remaining, weekly pace (7d burn rate), Code Review session % remaining (5h), Code Review pace (5h burn rate), Code Review weekly % remaining, Code Review weekly pace (7d burn rate), GPT-5.3 Codex Spark session % remaining (5h), GPT-5.3 Codex Spark pace (5h burn rate), GPT-5.3 Codex Spark weekly % remaining, GPT-5.3 Codex Spark weekly pace (7d), credits remaining ($, paid plans only), cost today (local logs), cost last 30 days (local logs). |
| Copilot | GitHub token from the Provider tab, `GITHUB_TOKEN`, or GitHub Copilot local auth files under `~/.config/github-copilot/`. | Premium interactions remaining %, chat interactions remaining %. |
| Cursor | Usage Buttons Helper from the signed-in Cursor web session. | Total plan usage remaining %, Auto / Composer usage remaining %, API / named model usage remaining %, on-demand spend ($), team on-demand spend ($). |
| Droid / Factory | Droid bearer token from the Provider tab, `FACTORY_TOKEN`, or Usage Buttons Helper from `app.factory.ai`. Optional API base URL override in the Provider tab / `FACTORY_BASE_URL`. | Standard tokens remaining %, premium tokens remaining %. |
| Gemini | Gemini CLI OAuth from `~/.gemini/oauth_creds.json`. Run `gemini` and sign in with Google. | Pro quota remaining %, Flash quota remaining %, Flash Lite quota remaining %. |
| Grok | Usage Buttons Helper from `grok.com`. | Grok 3 queries remaining %, Grok 3 tokens remaining %, Grok 4 Heavy queries remaining %. |
| Hermes Agent | Self-hosted dashboard at the user's Hermes Agent base URL (default `http://127.0.0.1:9119`; user-configurable per provider / per button). Session token auto-scraped from `<base>/index.html`; optional paste fallback. | Input / output / cache / total tokens and estimated cost ($), each sliced daily / weekly / monthly. Plus active sessions in the last 5 minutes. |
| OpenClaw | Self-hosted gateway at the user's OpenClaw URL (default `ws://127.0.0.1:18789`; user-configurable per provider / per button; `http(s)://` auto-converted to `ws(s)://`). Operator gateway token from PI / `OPENCLAW_GATEWAY_TOKEN`. WebSocket JSON-RPC; `usage.cost` method. | Input / output / cache / total tokens and total cost ($), each sliced daily / weekly / monthly. |
| Nous Research | Usage Buttons Helper from `portal.nousresearch.com`. | Subscription credits ($, Hermes Agent + Nous Chat pool), API credits balance ($), all-time totals (spend $, requests, tokens, input/output/cache-read/cache-write tokens) — combined or split by allowance (api / sub). |
| JetBrains AI | Local JetBrains IDE quota files. Optional overrides: `CODEXBAR_JETBRAINS_IDE_BASE_PATH` or `JETBRAINS_QUOTA_FILE`. | Current credits remaining %. |
| Kilo | Kilo API key from the Provider tab, `KILO_API_KEY`, or `~/.local/share/kilo/auth.json`. | Credits remaining %, Kilo Pass remaining %. |
| Kimi | Usage Buttons Helper from `kimi.com`. | Weekly coding quota remaining %, 5-hour rate limit remaining %. |
| Kimi K2 | Kimi K2 API key from the Provider tab or `KIMI_K2_API_KEY`. | Credits remaining. |
| Kiro | `kiro-cli`; run `kiro-cli login` first. | Monthly credits remaining %, bonus credits remaining %. |
| MiniMax | MiniMax API key from the Provider tab / `MINIMAX_API_KEY`, or Usage Buttons Helper from `minimax.io`. Optional region override: `MINIMAX_REGION`. | Coding prompts remaining %. |
| Mistral | Usage Buttons Helper from `admin.mistral.ai`. | Monthly billing usage. |
| Ollama | Usage Buttons Helper from the signed-in Ollama web session. | Session usage remaining %, session pace (burn rate), weekly usage remaining %, weekly pace (burn rate). |
| OpenCode | Usage Buttons Helper from `opencode.ai`. Optional workspace override: `CODEXBAR_OPENCODE_WORKSPACE_ID`. | 5-hour usage remaining %, weekly usage remaining %. |
| OpenCode Go | Usage Buttons Helper from `opencode.ai`. Optional workspace override: `CODEXBAR_OPENCODEGO_WORKSPACE_ID`. | 5-hour usage remaining %, weekly usage remaining %, monthly usage remaining %. |
| OpenRouter | OpenRouter API key from the Provider tab or `OPENROUTER_API_KEY`. Optional API base URL override in the Provider tab. | Credit balance ($), total usage ($), per-key quota remaining %, rate limit (requests / interval). |
| Perplexity | Usage Buttons Helper from `perplexity.ai`. | Recurring credits remaining %, bonus credits remaining %, purchased credits remaining %. |
| Synthetic | Synthetic API key from the Provider tab or `SYNTHETIC_API_KEY`. | Five-hour quota remaining %, weekly tokens remaining %, search hourly remaining %. |
| Vertex AI | gcloud Application Default Credentials. Run `gcloud auth application-default login` and configure a project with `gcloud config set project PROJECT_ID` or standard Google Cloud project env vars. | Request quota remaining %, token quota remaining %. |
| Warp | Warp API key from the Provider tab, `WARP_API_KEY`, or `WARP_TOKEN`. | Credits remaining %, bonus credits remaining %. |
| z.ai | z.ai API key from the Provider tab, `Z_AI_API_KEY`, `ZAI_API_TOKEN`, or `ZAI_API_KEY`. Optional region/endpoint overrides: `Z_AI_API_HOST`, `Z_AI_QUOTA_URL`. | Token usage remaining %, 5-hour token usage remaining %, MCP usage remaining %. |

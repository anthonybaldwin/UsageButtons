/**
 * Provider registry. Maps provider IDs to their implementations.
 * Every provider that ships in the plugin is registered here.
 */

import { ClaudeProvider } from "./claude.ts";
import { CodexProvider } from "./codex.ts";
import { CopilotProvider } from "./copilot.ts";
import { CursorProvider } from "./cursor.ts";
import { KimiK2Provider } from "./kimi-k2.ts";
import { MockProvider } from "./mock.ts";
import { OpenRouterProvider } from "./openrouter.ts";
import { WarpProvider } from "./warp.ts";
import { ZaiProvider } from "./zai.ts";
import type { Provider } from "./types.ts";

const providers = new Map<string, Provider>();

function register(provider: Provider): void {
  providers.set(provider.id, provider);
}

// Dev / test
register(new MockProvider());

// Primary providers (OAuth / credential-file auth)
register(new ClaudeProvider());
register(new CodexProvider());

// GitHub token auth
register(new CopilotProvider());

// Cookie-based auth (manual paste)
register(new CursorProvider());

// API key / env var auth
register(new OpenRouterProvider());
register(new WarpProvider());
register(new ZaiProvider());
register(new KimiK2Provider());

export function getProvider(id: string): Provider | undefined {
  return providers.get(id);
}

export function listProviders(): Provider[] {
  return [...providers.values()];
}

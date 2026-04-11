/**
 * Single registry the plugin talks to. Real providers will be added
 * here once their fetchers exist; for now only the mock is wired up.
 */

import { MockProvider } from "./mock.ts";
import type { Provider } from "./types.ts";

const providers = new Map<string, Provider>();

function register(provider: Provider): void {
  providers.set(provider.id, provider);
}

register(new MockProvider());

export function getProvider(id: string): Provider | undefined {
  return providers.get(id);
}

export function listProviders(): Provider[] {
  return [...providers.values()];
}

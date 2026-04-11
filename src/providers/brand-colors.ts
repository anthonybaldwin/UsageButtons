/**
 * CodexBar brand-color table.
 *
 * Mirrors `ProviderBranding.color` from CodexBar's provider
 * descriptors so our button fills default to the same hues the user
 * already associates with each provider in CodexBar's menu bar.
 *
 * Every new provider we port should read its default `brandColor`
 * from this table rather than making one up. If CodexBar adds a
 * new provider or changes a color, refresh `tmp/CodexBar/` and
 * update the matching row here.
 *
 * Source of truth (cite when adding/changing a row):
 *   tmp/CodexBar/Sources/CodexBarCore/Providers/<Name>/<Name>ProviderDescriptor.swift
 *   — the `branding: ProviderBranding(... color: ProviderColor(red:, green:, blue:))`
 *     constructor in each file.
 *
 * CodexBar uses 0..1 Double components; we convert to #RRGGBB hex
 * strings rounded the same way Swift would. Hex values are lower-
 * case to match the rest of the plugin's color conventions.
 */
export const CODEXBAR_BRAND_COLORS = {
  claude: "#cc7c5e",        // rgb(204, 124, 94)  — warm coral
  codex: "#49a3b0",         // rgb( 73, 163, 176) — teal
  cursor: "#00bfa5",        // rgb(  0, 191, 165)
  gemini: "#ab87ea",        // rgb(171, 135, 234)
  antigravity: "#60ba7e",   // rgb( 96, 186, 126)
  factory: "#ff6b35",       // rgb(255, 107,  53) — Droid / Factory
  copilot: "#a855f7",       // rgb(168,  85, 247)
  zai: "#e85a6a",           // rgb(232,  90, 106)
  kimi: "#fe603c",          // rgb(254,  96,  60)
  kimiK2: "#4c00ff",        // rgb( 76,   0, 255)
  kilo: "#f27027",          // rgb(242, 112,  39)
  kiro: "#ff9900",          // rgb(255, 153,   0)
  vertexAI: "#4285f4",      // rgb( 66, 133, 244)
  augment: "#6366f1",       // rgb( 99, 102, 241)
  amp: "#dc2626",           // rgb(220,  38,  38)
  jetbrains: "#ff3399",     // rgb(255,  51, 153)
  openRouter: "#6467f2",    // rgb(100, 103, 242)
  warp: "#938bb4",          // rgb(147, 139, 180)
  openCode: "#3b82f6",      // rgb( 59, 130, 246)
  openCodeGo: "#3b82f6",    // rgb( 59, 130, 246)
  alibaba: "#ff6a00",       // rgb(255, 106,   0)
  minimax: "#fe603c",       // rgb(254,  96,  60)
  ollama: "#888888",        // rgb(136, 136, 136)
  perplexity: "#20b2aa",    // rgb( 32, 178, 170)
  synthetic: "#141414",     // rgb( 20,  20,  20)
} as const satisfies Record<string, string>;

export type BrandColorKey = keyof typeof CODEXBAR_BRAND_COLORS;

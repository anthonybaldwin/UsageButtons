/**
 * Generate `src/providers/provider-icons.generated.ts` by extracting
 * the single `<path d="...">` from each `ProviderIcon-*.svg` file in
 * `tmp/CodexBar/Sources/CodexBar/Resources/`.
 *
 * These SVGs are CodexBar's simplified provider glyphs (Claude's
 * spark, Codex's OpenAI hex, Cursor's cube, Gemini's diamond, etc.)
 * — CodexBar is MIT licensed and the file header of the generated
 * output carries the license attribution for redistribution.
 *
 * Run: `bun run scripts/generate-provider-icons.ts`
 *
 * The output file is checked into git. Re-run when tmp/CodexBar/ is
 * refreshed and a new provider has been added upstream. This is a
 * build-time script — it's NOT imported by the compiled plugin.
 */

import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";

const ROOT = resolve(import.meta.dir, "..");
const SVG_DIR = resolve(ROOT, "tmp/CodexBar/Sources/CodexBar/Resources");
const OUT = resolve(ROOT, "src/providers/provider-icons.generated.ts");

// File-id → keyof BRAND_COLORS mapping. Most match exactly after we
// strip `ProviderIcon-` and `.svg`, but a couple need normalisation
// (factory → factory, kimi-k2 → kimiK2, etc.).
const ALIAS: Record<string, string> = {
  kimik2: "kimiK2",
  opencodego: "openCodeGo",
  opencode: "openCode",
  openrouter: "openRouter",
  vertexai: "vertexAI",
  zai: "zai",
};

const files = readdirSync(SVG_DIR)
  .filter((f) => f.startsWith("ProviderIcon-") && f.endsWith(".svg"))
  .sort();

interface Entry {
  id: string;
  viewBox: string;
  d: string;
}
const entries: Entry[] = [];

for (const file of files) {
  const raw = readFileSync(resolve(SVG_DIR, file), "utf8");
  const viewBoxMatch = raw.match(/viewBox="([^"]+)"/);
  const pathMatch = raw.match(/<path\s+[^>]*\bd="([^"]+)"/);
  if (!pathMatch) {
    // eslint-disable-next-line no-console
    console.warn(`skip ${file} — no <path d="...">`);
    continue;
  }
  const rawId = file
    .replace(/^ProviderIcon-/, "")
    .replace(/\.svg$/, "")
    .toLowerCase();
  const id = ALIAS[rawId] ?? rawId;
  entries.push({
    id,
    viewBox: viewBoxMatch?.[1] ?? "0 0 100 100",
    d: pathMatch[1] ?? "",
  });
}

// Build the TS source. The icon map is exported as-is — callers
// embed the `d` attribute into a larger SVG with their own sizing,
// positioning, and fill color.
const header = `/**
 * Provider icon paths — generated from CodexBar's ProviderIcon-*.svg
 * resources by scripts/generate-provider-icons.ts.
 *
 * DO NOT EDIT BY HAND. Regenerate when tmp/CodexBar/ is refreshed.
 *
 * Each entry is the raw \`d\` attribute from a single-path SVG plus
 * its original viewBox (always 0 0 100 100 in practice). Wrap in
 * your own <svg> to render at whatever size / color you want.
 *
 * ---
 * Upstream: https://github.com/steipete/CodexBar (MIT)
 * Copyright (c) 2026 Peter Steinberger
 *
 * Used in usage-buttons under the MIT license terms. Full license
 * text: see tmp/CodexBar/LICENSE or
 * https://github.com/steipete/CodexBar/blob/main/LICENSE
 * ---
 */

export interface ProviderIcon {
  /** SVG viewBox, e.g. "0 0 100 100". */
  viewBox: string;
  /** Raw path \`d\` attribute. */
  d: string;
}

export const PROVIDER_ICONS: Record<string, ProviderIcon> = {
`;

const body = entries
  .map(
    (e) =>
      `  ${JSON.stringify(e.id)}: {\n    viewBox: ${JSON.stringify(e.viewBox)},\n    d: ${JSON.stringify(e.d)},\n  },`,
  )
  .join("\n");

const footer = `\n};\n`;

writeFileSync(OUT, header + body + footer);

// eslint-disable-next-line no-console
console.log(`✓ wrote ${entries.length} provider icon(s) to ${OUT}`);

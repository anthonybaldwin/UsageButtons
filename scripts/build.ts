/**
 * Compile the plugin to a standalone native binary via
 * `bun build --compile`, and drop it into the .sdPlugin/bin folder
 * where the Stream Deck software expects it.
 *
 * Usage:
 *   bun run build           # current OS
 *   bun run build:win       # force Windows target (x64)
 *   bun run build:mac       # force macOS target (universal)
 */

import { $ } from "bun";
import { existsSync, mkdirSync } from "node:fs";
import { resolve } from "node:path";

const ROOT = resolve(import.meta.dir, "..");
const SDPLUGIN = resolve(ROOT, "com.baldwin.usage-buttons.sdPlugin");
const BIN_DIR = resolve(SDPLUGIN, "bin");
const ENTRY = resolve(ROOT, "src/plugin.ts");

interface Target {
  name: string;
  bunTarget: string; // --target value
  outfile: string;   // relative to BIN_DIR
}

const TARGETS: Record<string, Target> = {
  win: {
    name: "Windows x64",
    bunTarget: "bun-windows-x64",
    outfile: "plugin-win.exe",
  },
  mac: {
    name: "macOS (universal)",
    bunTarget: "bun-darwin-x64",
    outfile: "plugin-mac",
  },
};

function currentTargetKey(): keyof typeof TARGETS {
  if (process.platform === "win32") return "win";
  if (process.platform === "darwin") return "mac";
  throw new Error(`unsupported host platform: ${process.platform}`);
}

async function build(targetKey: keyof typeof TARGETS): Promise<void> {
  const target = TARGETS[targetKey];
  if (!target) throw new Error(`unknown target: ${targetKey}`);

  if (!existsSync(BIN_DIR)) mkdirSync(BIN_DIR, { recursive: true });

  const out = resolve(BIN_DIR, target.outfile);
  // eslint-disable-next-line no-console
  console.log(`→ compiling for ${target.name} → ${out}`);

  await $`bun build --compile --target=${target.bunTarget} --minify --sourcemap ${ENTRY} --outfile ${out}`;

  // eslint-disable-next-line no-console
  console.log(`✓ built ${target.outfile}`);
}

const arg = process.argv[2];
const key: keyof typeof TARGETS =
  arg && arg in TARGETS ? (arg as keyof typeof TARGETS) : currentTargetKey();

await build(key);

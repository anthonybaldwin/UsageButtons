/**
 * Compile the plugin to a standalone native binary via
 * `bun build --compile`, drop it into the .sdPlugin/bin folder,
 * and (on Windows) optionally hot-reload Stream Deck so the new
 * binary takes effect without a manual quit/relaunch.
 *
 * Stream Deck holds an exclusive lock on the running plugin's .exe,
 * so a build while the plugin is live would fail with "could not
 * open file for writing". The reload dance is:
 *
 *   1. Kill Stream Deck if it's running (releases the .exe lock)
 *   2. Compile the new binary in place
 *   3. Relaunch Stream Deck (Start-Process, detached)
 *
 * This is the default because we almost always want it. Pass
 * `--no-reload` to skip the kill/relaunch (useful for CI or when
 * chained with `install:dev --restart`).
 *
 * Usage:
 *   bun run build                 # kill → compile → relaunch
 *   bun run build --no-reload     # compile only
 *   bun run build:win / :mac      # explicit target
 */

import { $ } from "bun";
import { existsSync, mkdirSync } from "node:fs";
import { platform } from "node:os";
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

function log(line: string): void {
  // eslint-disable-next-line no-console
  console.log(line);
}

async function isStreamDeckRunning(): Promise<boolean> {
  if (platform() === "win32") {
    const res = Bun.spawnSync({
      cmd: ["tasklist", "/FI", "IMAGENAME eq StreamDeck.exe"],
      stdout: "pipe",
      stderr: "pipe",
    });
    return res.stdout.toString().toLowerCase().includes("streamdeck.exe");
  }
  const res = await $`pgrep -x "Stream Deck"`.nothrow().quiet().text();
  return res.trim().length > 0;
}

async function killStreamDeck(): Promise<void> {
  if (platform() === "win32") {
    Bun.spawnSync({
      cmd: ["taskkill", "/F", "/IM", "StreamDeck.exe"],
      stdout: "ignore",
      stderr: "ignore",
    });
  } else {
    await $`pkill -x "Stream Deck"`.nothrow().quiet();
  }
}

function startStreamDeck(): void {
  if (platform() === "win32") {
    const candidate = resolve(
      process.env["ProgramFiles"] ?? "C:/Program Files",
      "Elgato",
      "StreamDeck",
      "StreamDeck.exe",
    );
    if (!existsSync(candidate)) {
      log(`! could not locate ${candidate} — launch Stream Deck manually`);
      return;
    }
    // PowerShell Start-Process is the only reliable way to detach
    // a GUI child from our bun process without the Git Bash argv
    // mangling that blew up `cmd /c start ""` earlier in the session.
    Bun.spawnSync({
      cmd: [
        "powershell",
        "-NoProfile",
        "-ExecutionPolicy",
        "Bypass",
        "-Command",
        `Start-Process -FilePath '${candidate.replace(/'/g, "''")}'`,
      ],
      stdout: "ignore",
      stderr: "ignore",
    });
  } else {
    // macOS
    Bun.spawnSync({ cmd: ["open", "-a", "Stream Deck"], stdout: "ignore", stderr: "ignore" });
  }
}

async function build(targetKey: keyof typeof TARGETS): Promise<void> {
  const target = TARGETS[targetKey];
  if (!target) throw new Error(`unknown target: ${targetKey}`);

  if (!existsSync(BIN_DIR)) mkdirSync(BIN_DIR, { recursive: true });

  const out = resolve(BIN_DIR, target.outfile);
  log(`→ compiling for ${target.name} → ${out}`);

  await $`bun build --compile --target=${target.bunTarget} --minify --sourcemap ${ENTRY} --outfile ${out}`;

  log(`✓ built ${target.outfile}`);
}

// Arg parsing: positional target + --no-reload flag
const rawArgs = process.argv.slice(2);
const noReload = rawArgs.includes("--no-reload");
const positional = rawArgs.find((a) => !a.startsWith("--"));
const key: keyof typeof TARGETS =
  positional && positional in TARGETS
    ? (positional as keyof typeof TARGETS)
    : currentTargetKey();

const reload = !noReload;
const wasRunning = reload && (await isStreamDeckRunning());

if (wasRunning) {
  log("→ stopping Stream Deck to release the plugin .exe lock");
  await killStreamDeck();
}

await build(key);

if (reload && wasRunning) {
  log("→ relaunching Stream Deck");
  startStreamDeck();
  log("✓ Stream Deck relaunched; the new plugin binary will load on its own");
}

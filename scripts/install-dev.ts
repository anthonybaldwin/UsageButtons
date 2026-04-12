/**
 * Dev-install: link the repo's .sdPlugin folder into the Stream Deck
 * software's plugin directory so a `bun run build` is picked up on
 * the next Stream Deck restart without copying files around.
 *
 * Windows: uses a directory junction (`mklink /J`) — no admin needed.
 * macOS:   uses a symlink (`ln -s`).
 *
 * This does NOT restart Stream Deck automatically. Stream Deck being
 * open is visible shared state — we warn and tell the user to quit +
 * relaunch themselves, unless `--restart` is passed.
 *
 * Usage:
 *   bun run install:dev            # link only
 *   bun run install:dev --restart  # also kill + relaunch Stream Deck
 */

import { $ } from "bun";
import { existsSync, mkdirSync, rmSync, lstatSync } from "node:fs";
import { homedir, platform } from "node:os";
import { resolve } from "node:path";

const ROOT = resolve(import.meta.dir, "..");
const PLUGIN_NAME = "io.github.anthonybaldwin.UsageButtons.sdPlugin";
const SDPLUGIN = resolve(ROOT, PLUGIN_NAME);

const restart = process.argv.includes("--restart");

function log(line: string): void {
  // eslint-disable-next-line no-console
  console.log(line);
}

function pluginsDir(): string {
  const os = platform();
  if (os === "win32") {
    const appdata = process.env["APPDATA"];
    if (!appdata) throw new Error("%APPDATA% is not set");
    return resolve(appdata, "Elgato", "StreamDeck", "Plugins");
  }
  if (os === "darwin") {
    return resolve(
      homedir(),
      "Library",
      "Application Support",
      "com.elgato.StreamDeck",
      "Plugins",
    );
  }
  throw new Error(`unsupported host platform: ${os}`);
}

async function isStreamDeckRunning(): Promise<boolean> {
  if (platform() === "win32") {
    const res = await $`tasklist /FI "IMAGENAME eq StreamDeck.exe"`
      .nothrow()
      .quiet()
      .text();
    return res.toLowerCase().includes("streamdeck.exe");
  }
  const res = await $`pgrep -x "Stream Deck"`.nothrow().quiet().text();
  return res.trim().length > 0;
}

async function killStreamDeck(): Promise<void> {
  if (platform() === "win32") {
    await $`taskkill /F /IM StreamDeck.exe`.nothrow().quiet();
  } else {
    await $`pkill -x "Stream Deck"`.nothrow().quiet();
  }
}

async function startStreamDeck(): Promise<void> {
  if (platform() === "win32") {
    const candidate = resolve(
      process.env["ProgramFiles"] ?? "C:/Program Files",
      "Elgato",
      "StreamDeck",
      "StreamDeck.exe",
    );
    if (!existsSync(candidate)) {
      log(
        "! could not locate StreamDeck.exe under Program Files — relaunch it yourself",
      );
      return;
    }
    // PowerShell Start-Process is the reliable way to launch a
    // detached Windows GUI app from a script. `cmd /c start ""
    // <path>` looks equivalent but Git Bash / MSYS rewrites argv in
    // ways that can cause ShellExecute to fire a ghost "\\" open
    // that pops a Windows Explorer error dialog. Start-Process
    // takes one quoted path argument and does the right thing.
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
    await $`open -a "Stream Deck"`.nothrow();
  }
}

function linkPlugin(): void {
  const target = pluginsDir();
  mkdirSync(target, { recursive: true });
  const dest = resolve(target, PLUGIN_NAME);

  // Clean up anything already at the destination (prior install,
  // stale symlink, etc.). rmSync on a junction unlinks it; on a real
  // directory it recursively deletes. We only touch *our* subfolder.
  if (existsSync(dest)) {
    try {
      const stat = lstatSync(dest);
      if (stat.isSymbolicLink() || stat.isDirectory()) {
        rmSync(dest, { recursive: true, force: true });
      }
    } catch (err) {
      log(`! could not remove existing ${dest}: ${String(err)}`);
    }
  }

  if (platform() === "win32") {
    // Directory junction — no admin, no dev-mode toggle required.
    const result = Bun.spawnSync({
      cmd: ["cmd", "/c", "mklink", "/J", dest, SDPLUGIN],
      stdout: "pipe",
      stderr: "pipe",
    });
    if (result.exitCode !== 0) {
      throw new Error(
        `mklink /J failed: ${result.stderr.toString() || result.stdout.toString()}`,
      );
    }
  } else {
    Bun.spawnSync({ cmd: ["ln", "-s", SDPLUGIN, dest] });
  }

  log(`✓ linked ${dest} → ${SDPLUGIN}`);
}

async function main(): Promise<void> {
  // Sanity: the binary the manifest points to must exist, otherwise
  // Stream Deck will fail to start the plugin and silently hide it.
  // On Mac the "binary" is actually a shell wrapper that dispatches
  // to the matching arm64 / x64 bun-compiled binary at runtime.
  const binName = platform() === "win32" ? "plugin-win.exe" : "plugin-mac";
  const bin = resolve(SDPLUGIN, "bin", binName);
  if (!existsSync(bin)) {
    log(`✗ missing ${bin}`);
    log("  run \`bun run build\` first.");
    process.exit(1);
  }

  // Mac-only: make sure the wrapper AND both compiled binaries
  // are executable AND don't carry a Gatekeeper quarantine flag.
  // Cross-compilation from Windows strips the executable bit, and
  // downloading / unzipping on the Mac side stamps files with
  // `com.apple.quarantine` which makes Stream Deck silently
  // refuse to launch the plugin the first time. chmod +x + xattr
  // -cr together fix both; they're no-ops when the flags are
  // already clean.
  if (platform() === "darwin") {
    for (const f of ["plugin-mac", "plugin-mac-arm64", "plugin-mac-x64"]) {
      const p = resolve(SDPLUGIN, "bin", f);
      if (!existsSync(p)) continue;
      try {
        Bun.spawnSync({ cmd: ["chmod", "+x", p] });
      } catch {
        // non-fatal
      }
      try {
        Bun.spawnSync({ cmd: ["xattr", "-d", "com.apple.quarantine", p] });
      } catch {
        // non-fatal — the attribute may not be present
      }
    }
    // Strip quarantine from the whole plugin directory recursively
    // so any bundled assets (icons, ui/stat.html, manifest.json)
    // also pass Gatekeeper without prompts.
    try {
      Bun.spawnSync({ cmd: ["xattr", "-cr", SDPLUGIN] });
    } catch {
      // non-fatal
    }
  }

  const running = await isStreamDeckRunning();
  if (running && !restart) {
    log(
      "! Stream Deck is currently running. It will keep its current plugin cache",
    );
    log(
      "  until you quit + relaunch it. Pass --restart to this script to do that",
    );
    log("  automatically, or quit Stream Deck yourself after the link.");
  }
  if (running && restart) {
    log("→ stopping Stream Deck");
    await killStreamDeck();
  }

  log("→ linking plugin");
  linkPlugin();

  if (restart) {
    log("→ starting Stream Deck");
    await startStreamDeck();
    log("✓ done — Stream Deck will pick up the plugin on launch");
  } else {
    log("✓ link created. Quit + relaunch Stream Deck to load the plugin.");
  }
}

await main();

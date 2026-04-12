/**
 * Checks the GitHub Releases API for a newer version of the plugin.
 *
 * - Compares the latest published tag against the version baked into
 *   manifest.json at build time.
 * - Caches the result so we hit GitHub at most once per check interval
 *   (default 6 hours).
 * - Never throws — returns the cached state on network failure.
 */

import { existsSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

const REPO = "anthonybaldwin/UsageButtons";
const RELEASES_URL = `https://github.com/${REPO}/releases/latest`;
const REPO_URL = `https://github.com/${REPO}`;
const CHECK_INTERVAL_MS = 6 * 60 * 60 * 1000; // 6 hours

interface UpdateState {
  current: string;
  latest: string | null;
  updateAvailable: boolean;
  lastCheckedAt: number;
}

const state: UpdateState = {
  current: readCurrentVersion(),
  latest: null,
  updateAvailable: false,
  lastCheckedAt: 0,
};

let logSink: ((msg: string) => void) | undefined;

export function setUpdateCheckerLogSink(fn: (msg: string) => void): void {
  logSink = fn;
}

function log(msg: string): void {
  logSink?.(`[update-checker] ${msg}`);
}

/**
 * Read the 3-part semver from manifest.json (strips the 4th ".0").
 *
 * The compiled binary lives at .sdPlugin/bin/plugin-win.exe (or the
 * macOS equivalent). manifest.json is one level up from the bin/
 * directory, i.e. ../manifest.json relative to the executable.
 *
 * In dev mode (bun --watch src/plugin.ts), import.meta.dir points at
 * the src/ folder, so we fall back to the repo-relative path.
 */
function readCurrentVersion(): string {
  const candidates = [
    // Production: resolve from the running binary's location
    // (bin/plugin-win.exe → ../manifest.json)
    resolve(dirname(process.execPath), "..", "manifest.json"),
    // Dev mode: resolve from the source tree
    resolve(
      import.meta.dir,
      "..",
      "io.github.anthonybaldwin.UsageButtons.sdPlugin",
      "manifest.json",
    ),
  ];
  for (const p of candidates) {
    try {
      const manifest = JSON.parse(readFileSync(p, "utf-8"));
      const parts = (manifest.Version as string).split(".");
      return parts.slice(0, 3).join(".");
    } catch {
      // try next candidate
    }
  }
  return "0.0.0";
}

/** Compare two semver strings. Returns >0 if b is newer than a. */
function compareSemver(a: string, b: string): number {
  const pa = a.split(".").map(Number);
  const pb = b.split(".").map(Number);
  for (let i = 0; i < 3; i++) {
    const diff = (pb[i] ?? 0) - (pa[i] ?? 0);
    if (diff !== 0) return diff;
  }
  return 0;
}

/**
 * Fetch the latest release tag from GitHub. Returns null on any
 * failure (network error, rate limit, no releases, etc.).
 */
async function fetchLatestVersion(): Promise<string | null> {
  try {
    const res = await fetch(
      `https://api.github.com/repos/${REPO}/releases/latest`,
      {
        headers: { Accept: "application/vnd.github+json" },
        signal: AbortSignal.timeout(10_000),
      },
    );
    if (!res.ok) {
      log(`GitHub API returned ${res.status}`);
      return null;
    }
    const data = (await res.json()) as { tag_name?: string };
    const tag = data.tag_name;
    if (!tag) return null;
    return tag.replace(/^v/, "");
  } catch (err) {
    log(`fetch failed: ${err}`);
    return null;
  }
}

/**
 * Run an update check if the cache has expired. Safe to call on
 * every scheduler tick — it no-ops when the cache is still warm.
 */
export async function checkForUpdate(): Promise<void> {
  const now = Date.now();
  if (now - state.lastCheckedAt < CHECK_INTERVAL_MS) return;

  state.lastCheckedAt = now;
  const latest = await fetchLatestVersion();

  if (latest === null) {
    // Network failure — keep previous state, don't flip the flag.
    log("check failed, keeping previous state");
    return;
  }

  state.latest = latest;
  state.updateAvailable = compareSemver(state.current, latest) > 0;

  if (state.updateAvailable) {
    log(`update available: ${state.current} → ${latest}`);
  } else {
    log(`up to date (current=${state.current}, latest=${latest})`);
  }
}

/** Whether an update is available (from the last successful check). */
export function isUpdateAvailable(): boolean {
  return state.updateAvailable;
}

/** The latest version string, or null if never checked. */
export function getLatestVersion(): string | null {
  return state.latest;
}

/** The current plugin version. */
export function getCurrentVersion(): string {
  return state.current;
}

/**
 * Detect whether the plugin was installed from a dev build (git clone +
 * bun run build) vs. a release bundle. Dev installs have a .git
 * directory in the repo root (two levels up from the binary in
 * .sdPlugin/bin/). Release bundles only contain the .sdPlugin folder.
 */
let devInstall: boolean | undefined;
function isDevInstall(): boolean {
  if (devInstall === undefined) {
    // Binary at .sdPlugin/bin/plugin-win.exe → repo root is ../../
    const repoRoot = resolve(dirname(process.execPath), "..", "..");
    devInstall = existsSync(resolve(repoRoot, ".git"));
    log(`install type: ${devInstall ? "dev (git repo)" : "release bundle"}`);
  }
  return devInstall;
}

/**
 * URL to direct the user to when an update is available.
 * Dev installs → repo (they need to git pull + build).
 * Release installs → releases/latest (download the new bundle).
 */
export function getUpdateUrl(): string {
  return isDevInstall() ? REPO_URL : RELEASES_URL;
}

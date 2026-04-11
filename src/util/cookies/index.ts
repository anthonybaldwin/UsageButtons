/**
 * Browser cookie extraction — `@steipete/sweet-cookie` on the
 * outside, with a Windows-specific pre-copy dance on the inside to
 * route around Chrome's live file-lock on `Cookies`.
 *
 * ### Why this module exists
 *
 * sweet-cookie is robust across macOS Keychain, Linux secret-tool,
 * DPAPI, Chrome MV3 extension fallback, and every cookie-encoding
 * evolution Chromium has shipped. The one gap on Windows: its
 * internal `copyFileSync(liveDb, tempDb)` fails with `EBUSY` if
 * Chrome is running, because Chrome holds the Cookies SQLite with
 * exclusive access. `copyFileSync` uses `CopyFileW` which doesn't
 * negotiate sharing flags.
 *
 * Fix: when the direct scan returns no match on Windows, we build
 * a fake "User Data" directory in temp, pre-copy `Local State`
 * (not locked) and the Cookies DB (locked, via our PowerShell
 * FileStream helper that opens with FileShare.ReadWrite|Delete),
 * then re-call sweet-cookie with `chromeProfile: <our-temp-path>`.
 * sweet-cookie's own `copyFileSync` then operates on our pre-copied
 * (non-locked) file and the scan succeeds.
 *
 * ### Caching
 *
 * We wrap the scan in a 30-minute in-memory cache so repeated poll
 * cycles don't re-scan browsers every 5 minutes. Cache is keyed by
 * (url, cookie name) and cleared when cookie settings change.
 */

import { getCookies } from "@steipete/sweet-cookie";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { copyLockedFileWindows, LockedCopyError } from "./locked-copy.ts";

export interface FindCookieOptions {
  /** URL we want cookies for, e.g. `https://claude.ai/`. */
  url: string;
  /** Cookie name to search for. */
  cookieName?: string;
  /** Optional logger hook. */
  onLog?: (message: string) => void;
}

interface CacheEntry {
  value: string;
  capturedAt: number;
}
/**
 * Cookie cache TTL. Long on purpose (24h):
 *   - claude.ai sessionKeys are stable for weeks; the browser
 *     itself reuses the same value for days at a time.
 *   - Chrome on Windows blocks us from re-reading its Cookies
 *     file while Chrome is running. If the user catches the
 *     one-time window where Chrome is closed (say, a morning
 *     restart), we want the captured cookie to last until the
 *     next restart rather than forcing another close after 30m.
 *   - If the cookie goes bad mid-day, claude.ai returns 401 on
 *     the /overage_spend_limit call and `invalidateCookieCache`
 *     clears the entry so the next poll rescans.
 */
const CACHE_TTL_MS = 24 * 60 * 60 * 1000; // 24h
const cache = new Map<string, CacheEntry>();

function cacheKey(opts: FindCookieOptions): string {
  return `${opts.url}|${opts.cookieName ?? "*"}`;
}

export function clearCookieCache(): void {
  cache.clear();
}

/**
 * Invalidate one cached cookie — called by providers on a 401
 * response so the next poll tries to re-scan. We match on URL
 * prefix so callers don't need to know the exact cache key
 * format.
 */
export function invalidateCookieCache(url: string): void {
  for (const key of cache.keys()) {
    if (key.startsWith(`${url}|`)) cache.delete(key);
  }
}

/** Pick the first non-empty matching cookie out of a sweet-cookie result. */
function pickMatch(
  result: Awaited<ReturnType<typeof getCookies>>,
  cookieName: string,
): string | undefined {
  const match = result.cookies?.find(
    (c) => c.name === cookieName && c.value && c.value.length > 0,
  );
  return match?.value;
}

/**
 * Copy one Chromium-family profile's Cookies DB (plus Local State)
 * into a fresh temp directory using the Windows-safe PowerShell
 * FileStream helper. Returns the path to pass to sweet-cookie's
 * `chromeProfile` / `edgeProfile` option, or undefined on failure.
 *
 * Layout written:
 *
 *   <tempRoot>/
 *     Local State                     ← plain copy (never locked)
 *     <profileName>/
 *       Network/
 *         Cookies                     ← PowerShell locked-file copy
 *         Cookies-wal                 ← best effort
 *         Cookies-shm                 ← best effort
 *
 * Matches sweet-cookie's `resolveChromiumPathsWindows()` expectations:
 * it walks up from the Cookies path to find the first `Local State`
 * sibling, which is our tempRoot.
 */
function stageChromiumProfile(
  liveUserDataDir: string,
  profileName: string,
  onLog?: (message: string) => void,
): { tempRoot: string; stagedProfile: string } | undefined {
  const liveLocalState = join(liveUserDataDir, "Local State");
  const liveCookies = join(liveUserDataDir, profileName, "Network", "Cookies");
  if (!existsSync(liveLocalState) || !existsSync(liveCookies)) return undefined;

  const tempRoot = mkdtempSync(join(tmpdir(), "usage-buttons-ch-"));
  try {
    // Local State is not held with exclusive access, plain copy works.
    copyFileSync(liveLocalState, join(tempRoot, "Local State"));

    const networkDir = join(tempRoot, profileName, "Network");
    mkdirSync(networkDir, { recursive: true });

    const stagedCookies = join(networkDir, "Cookies");
    copyLockedFileWindows(liveCookies, stagedCookies);

    // Best-effort copy of the SQLite WAL / SHM sidecars so the
    // snapshot reflects recent writes.
    for (const suffix of ["-wal", "-shm"]) {
      const src = liveCookies + suffix;
      if (!existsSync(src)) continue;
      try {
        copyLockedFileWindows(src, stagedCookies + suffix);
      } catch {
        // ignore — main DB is usable without the sidecars
      }
    }

    return {
      tempRoot,
      stagedProfile: join(tempRoot, profileName),
    };
  } catch (err) {
    try {
      rmSync(tempRoot, { recursive: true, force: true });
    } catch {
      /* ignore */
    }
    if (err instanceof LockedCopyError) {
      onLog?.(`cookies: staging ${profileName} failed — ${err.message}`);
    } else {
      onLog?.(
        `cookies: staging ${profileName} failed — ${String((err as Error).message ?? err)}`,
      );
    }
    return undefined;
  }
}

/** List profile subdirectory names (`Default`, `Profile 1`, …) under a User Data dir. */
function listProfileDirs(userDataDir: string): string[] {
  if (!existsSync(userDataDir)) return [];
  const out: string[] = [];
  if (existsSync(join(userDataDir, "Default", "Network", "Cookies"))) {
    out.push("Default");
  }
  for (const name of readdirSync(userDataDir)) {
    if (!name.startsWith("Profile ")) continue;
    if (existsSync(join(userDataDir, name, "Network", "Cookies"))) {
      out.push(name);
    }
  }
  return out;
}

/**
 * Probe a staged Chromium Cookies SQLite to find out whether the
 * target cookie row exists and, critically, which encryption
 * version prefix it uses. Returns one of:
 *
 *   "missing"  — row not present (account not logged in here)
 *   "absent"   — row present but empty encrypted_value
 *   "v20"      — row present, uses Chrome 127+ App-Bound
 *                Encryption which cannot be decrypted from any
 *                non-Chrome process. Sweet-cookie's AES-256-GCM
 *                path silently fails on these and returns an
 *                empty value — callers should surface a clear
 *                "use manual paste / companion extension"
 *                message rather than "no cookie found".
 *   "v10" | "v11" | … — sweet-cookie can likely decrypt it.
 */
async function probeCookiePrefix(
  stagedCookiesDb: string,
  cookieName: string,
  hostLike: string,
): Promise<"missing" | "absent" | string> {
  // Lazy-import bun:sqlite so a non-Bun runtime (unit tests) can
  // still import this module without the binding.
  const { Database } = await import("bun:sqlite");
  const db = new Database(stagedCookiesDb, { readonly: true });
  try {
    const row = db
      .query<{ encrypted_value: Uint8Array }, [string, string]>(
        `SELECT encrypted_value FROM cookies WHERE name = ? AND host_key LIKE ? LIMIT 1`,
      )
      .get(cookieName, hostLike);
    if (!row) return "missing";
    const bytes = new Uint8Array(row.encrypted_value);
    if (bytes.length < 3) return "absent";
    return Buffer.from(bytes.slice(0, 3)).toString("ascii");
  } finally {
    db.close();
  }
}

/**
 * Windows fallback: for every installed Chromium-family browser's
 * profile, stage it to temp and let sweet-cookie scan the staged
 * copy. Returns the first hit, or undefined.
 *
 * Also probes each staged DB for the target cookie's encryption
 * prefix so we can emit a clear "v20 / app-bound encryption"
 * diagnostic when sweet-cookie silently fails to decrypt modern
 * Chrome cookies.
 */
async function windowsStagedScan(
  opts: FindCookieOptions & { cookieName: string },
  log: (message: string) => void,
): Promise<string | undefined> {
  const localAppData = process.env["LOCALAPPDATA"];
  if (!localAppData) return undefined;

  const browsers: Array<{
    name: string;
    userDataDir: string;
    sweetCookieBrowser: "chrome" | "edge";
  }> = [
    {
      name: "Chrome",
      userDataDir: join(localAppData, "Google", "Chrome", "User Data"),
      sweetCookieBrowser: "chrome",
    },
    {
      name: "Edge",
      userDataDir: join(localAppData, "Microsoft", "Edge", "User Data"),
      sweetCookieBrowser: "edge",
    },
  ];

  for (const browser of browsers) {
    if (!existsSync(browser.userDataDir)) continue;
    const profiles = listProfileDirs(browser.userDataDir);
    if (profiles.length === 0) continue;

    // For Chrome specifically, `esentutl /y` fails with
    // JET_errFileAccessDenied while Chrome is running (Chrome holds
    // the file with FileShare.None). It succeeds when Chrome is
    // closed. For Edge/Brave/etc. it succeeds either way.
    // Either way we try — a single failed esentutl is cheap and
    // the staging loop catches it cleanly with a log line.
    log(
      `cookies[${browser.name}] staging ${profiles.length} profile(s) for lock-bypass scan`,
    );

    for (const profile of profiles) {
      const staged = stageChromiumProfile(browser.userDataDir, profile, log);
      if (!staged) continue;
      try {
        // Probe the raw cookie row BEFORE handing off to sweet-cookie
        // so we can detect the v20 / app-bound encryption case. If
        // sweet-cookie returns no match on a v20 row, we know why
        // instead of reporting a generic "no cookie found".
        const stagedCookiesDb = join(
          staged.stagedProfile,
          "Network",
          "Cookies",
        );
        let probe: string | undefined;
        try {
          probe = await probeCookiePrefix(
            stagedCookiesDb,
            opts.cookieName,
            "%" + new URL(opts.url).hostname + "%",
          );
          log(
            `cookies[${browser.name}/${profile}] row probe: ${probe} for ${opts.cookieName}`,
          );
        } catch (err) {
          log(
            `cookies[${browser.name}/${profile}] probe failed: ${String((err as Error).message ?? err)}`,
          );
        }

        const result = await getCookies({
          url: opts.url,
          names: [opts.cookieName],
          browsers: [browser.sweetCookieBrowser],
          ...(browser.sweetCookieBrowser === "chrome"
            ? { chromeProfile: staged.stagedProfile }
            : { edgeProfile: staged.stagedProfile }),
        });
        for (const warning of result.warnings ?? []) {
          log(`cookies[${browser.name}/${profile}] warning: ${warning}`);
        }
        const match = pickMatch(result, opts.cookieName);
        if (match) {
          log(
            `cookies[${browser.name}/${profile}] found ${opts.cookieName} via staged scan`,
          );
          return match;
        }

        // sweet-cookie returned nothing but we know the row exists.
        // The only common cause on modern Chrome is the v20 prefix
        // (App-Bound Encryption from Chrome 127+) which sweet-cookie
        // parses via its AES-256-GCM path using the legacy DPAPI
        // key — the decryption silently fails because v20 rows are
        // actually wrapped with the `app_bound_encrypted_key` from
        // Local State, which requires a COM elevation-service hop
        // that is impossible to bypass from a user-mode process.
        if (probe === "v20") {
          log(
            `cookies[${browser.name}/${profile}] ${opts.cookieName} is v20 App-Bound Encrypted (Chrome 127+) — cannot decrypt from outside Chrome. Use Manual paste in the Property Inspector, or switch claude.ai to Edge / Firefox.`,
          );
        }
      } finally {
        try {
          rmSync(staged.tempRoot, { recursive: true, force: true });
        } catch {
          /* ignore */
        }
      }
    }
  }
  return undefined;
}

/**
 * Top-level lookup: try sweet-cookie's default scan first (which
 * covers macOS / Linux / non-locked browsers cleanly), then fall
 * through to the Windows staged-copy fallback if no match and we're
 * on Windows.
 */
export async function findClaudeCookie(
  opts: FindCookieOptions = { url: "https://claude.ai/" },
): Promise<string | undefined> {
  const cookieName = opts.cookieName ?? "sessionKey";
  const key = cacheKey({ ...opts, cookieName });
  const now = Date.now();
  const hit = cache.get(key);
  if (hit && now - hit.capturedAt < CACHE_TTL_MS) return hit.value;

  const log = opts.onLog ?? (() => {});
  log(`cookies: scanning for ${cookieName} on ${opts.url}`);

  // Attempt 1: sweet-cookie's native scan.
  try {
    const result = await getCookies({
      url: opts.url,
      names: [cookieName],
      browsers: ["chrome", "edge", "firefox", "safari"],
    });
    for (const warning of result.warnings ?? []) {
      log(`cookies: ${warning}`);
    }
    const match = pickMatch(result, cookieName);
    if (match) {
      log(`cookies: found ${cookieName} via sweet-cookie default scan`);
      cache.set(key, { value: match, capturedAt: now });
      return match;
    }
  } catch (err) {
    log(`cookies: getCookies threw: ${String((err as Error).message ?? err)}`);
  }

  // Attempt 2: Windows staged-copy fallback. Handles the Chrome
  // EBUSY case that sweet-cookie doesn't yet — we pre-copy the
  // locked DB via PowerShell FileStream then hand the staged copy
  // back to sweet-cookie to parse.
  if (process.platform === "win32") {
    const staged = await windowsStagedScan({ ...opts, cookieName }, log);
    if (staged) {
      cache.set(key, { value: staged, capturedAt: now });
      return staged;
    }
  }

  log(`cookies: no ${cookieName} found anywhere`);
  return undefined;
}

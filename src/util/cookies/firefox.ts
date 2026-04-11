/**
 * Firefox cookie reader for Windows.
 *
 * Firefox stores cookies in a plain SQLite database at
 *   %APPDATA%\Mozilla\Firefox\Profiles\<profile>\cookies.sqlite
 * with NO encryption — it relies on file-system permissions instead.
 * Compared to Chromium this is trivial: find the profile dir,
 * open the DB read-only, SELECT the row.
 */

import { Database } from "bun:sqlite";
import { copyFileSync, existsSync, mkdtempSync, readdirSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/** Return absolute paths to every Firefox profile we can see. */
function listFirefoxProfiles(): string[] {
  const appData = process.env["APPDATA"];
  if (!appData) return [];
  const profilesRoot = join(appData, "Mozilla", "Firefox", "Profiles");
  if (!existsSync(profilesRoot)) return [];

  const profiles: string[] = [];
  for (const name of readdirSync(profilesRoot)) {
    const full = join(profilesRoot, name);
    try {
      if (!statSync(full).isDirectory()) continue;
    } catch {
      continue;
    }
    if (existsSync(join(full, "cookies.sqlite"))) {
      profiles.push(full);
    }
  }
  // Put `.default-release` / `.default` profiles first since those
  // are the ones the user actually uses most of the time.
  profiles.sort((a, b) => {
    const rank = (s: string) =>
      s.includes(".default-release") ? 0 : s.includes(".default") ? 1 : 2;
    return rank(a) - rank(b);
  });
  return profiles;
}

/**
 * High-level: try every Firefox profile, return the first
 * claude.ai sessionKey value we find. Returns undefined if not
 * found so the caller can fall through.
 */
export async function findFirefoxClaudeCookie(opts: {
  cookieName?: string;
  hostPatterns?: string[];
  onLog?: (message: string) => void;
} = {}): Promise<string | undefined> {
  const cookieName = opts.cookieName ?? "sessionKey";
  const hostPatterns = opts.hostPatterns ?? [
    ".claude.ai",
    "claude.ai",
    "www.claude.ai",
  ];
  const log = opts.onLog ?? (() => {});

  const profiles = listFirefoxProfiles();
  if (profiles.length === 0) {
    log("cookies: no firefox profiles found");
    return undefined;
  }
  log(`cookies: scanning ${profiles.length} firefox profile(s)`);

  for (const profile of profiles) {
    const src = join(profile, "cookies.sqlite");
    const tmp = mkdtempSync(join(tmpdir(), "usage-buttons-ffcookies-"));
    const tmpDb = join(tmp, "cookies.sqlite");
    try {
      try {
        copyFileSync(src, tmpDb);
      } catch (err) {
        log(`cookies[firefox] copy failed: ${String((err as Error).message ?? err)}`);
        continue;
      }
      const db = new Database(tmpDb, { readonly: true });
      try {
        const placeholders = hostPatterns.map(() => "?").join(", ");
        const rows = db
          .query<{ value: string; host: string }, string[]>(
            `SELECT value, host FROM moz_cookies
             WHERE name = ? AND host IN (${placeholders})`,
          )
          .all(cookieName, ...hostPatterns);
        for (const row of rows) {
          if (row.value && row.value.length > 0) {
            log(`cookies[firefox] found ${cookieName} at ${row.host}`);
            return row.value;
          }
        }
      } finally {
        db.close();
      }
    } finally {
      try {
        rmSync(tmp, { recursive: true, force: true });
      } catch {
        // not fatal
      }
    }
  }
  return undefined;
}

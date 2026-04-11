/**
 * Chromium-family cookie reader for Windows (Chrome, Edge, Brave,
 * Chromium itself, Vivaldi, Arc-Windows, …). All of these use the
 * same on-disk format that Google ships in Chromium:
 *
 *   <USER DATA>/Local State                   — JSON; os_crypt.encrypted_key
 *   <USER DATA>/<profile>/Network/Cookies     — SQLite database
 *
 * Older versions store Cookies in `<profile>/Cookies` (no `Network`
 * subfolder); we try both. The cookie's `encrypted_value` column is
 * a binary blob that starts with a `v10` or `v11` ASCII prefix,
 * followed by a 12-byte nonce, the AES-GCM ciphertext, and a
 * 16-byte auth tag at the end. The AES-256-GCM key lives in the
 * Local State JSON, DPAPI-encrypted — see `dpapi.ts`.
 *
 * We only need *one* cookie (the claude.ai sessionKey), so the code
 * is intentionally narrow: copy the Cookies DB to a temp file to
 * dodge Chrome's exclusive lock, open it with bun:sqlite, SELECT
 * the matching row, AES-GCM decrypt the value, return the
 * plaintext. No chrome-specific caching or iteration over
 * arbitrary cookies.
 *
 * Reference: https://chromium.googlesource.com/chromium/src/+/main/components/os_crypt/sync/os_crypt_win.cc
 */

import { Database } from "bun:sqlite";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve, join } from "node:path";
import { dpapiDecrypt, stripChromiumKeyPrefix, DpapiError } from "./dpapi.ts";

/**
 * Copy a file that may be held open by another process (Chrome's
 * Cookies DB while Chrome is running). Bun's fs.copyFileSync uses
 * CopyFileW under the hood which fails with EBUSY when the source
 * is locked.
 *
 * Workaround: shell out to PowerShell and open the source with
 * `FileShare.ReadWrite | FileShare.Delete` via .NET's FileStream.
 * That matches the sharing mode Chrome uses on its own handle, so
 * both processes can coexist without one blocking the other.
 *
 * Chrome 127+ tightens this up as part of App-Bound Encryption, so
 * this trick may stop working on the newest versions. It continues
 * to work for the Network/Cookies DB on current stable channels as
 * of 2025-Q4.
 */
function copyLockedFile(src: string, dst: string): void {
  const script = [
    `$src = New-Object System.IO.FileStream('${src.replace(/'/g, "''")}',`,
    `  [System.IO.FileMode]::Open,`,
    `  [System.IO.FileAccess]::Read,`,
    `  ([System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete))`,
    `$dst = New-Object System.IO.FileStream('${dst.replace(/'/g, "''")}',`,
    `  [System.IO.FileMode]::Create,`,
    `  [System.IO.FileAccess]::Write,`,
    `  [System.IO.FileShare]::None)`,
    `$src.CopyTo($dst)`,
    `$src.Close()`,
    `$dst.Close()`,
  ].join(" ");

  const result = Bun.spawnSync({
    cmd: [
      "powershell",
      "-NoProfile",
      "-NonInteractive",
      "-ExecutionPolicy",
      "Bypass",
      "-Command",
      script,
    ],
    stdout: "pipe",
    stderr: "pipe",
  });
  if (result.exitCode !== 0) {
    const err = result.stderr.toString().trim() || "unknown error";
    throw new Error(`locked-file copy failed: ${err}`);
  }
}

export interface ChromiumBrowser {
  /** Human label ("Chrome", "Edge", "Brave", …) used for logging. */
  name: string;
  /** Absolute path to the "User Data" directory. */
  userDataDir: string;
}

/**
 * Candidate Chromium installs on the current user. Paths are just
 * guesses — we return them all and the loader skips any that don't
 * actually exist. Order matters only for logging and first-hit
 * shortcut behaviour.
 */
export function listChromiumInstalls(): ChromiumBrowser[] {
  const localAppData = process.env["LOCALAPPDATA"];
  const appData = process.env["APPDATA"];
  if (!localAppData) return [];

  return [
    { name: "Chrome", userDataDir: join(localAppData, "Google", "Chrome", "User Data") },
    { name: "Edge", userDataDir: join(localAppData, "Microsoft", "Edge", "User Data") },
    { name: "Brave", userDataDir: join(localAppData, "BraveSoftware", "Brave-Browser", "User Data") },
    { name: "Vivaldi", userDataDir: join(localAppData, "Vivaldi", "User Data") },
    { name: "Chromium", userDataDir: join(localAppData, "Chromium", "User Data") },
    // Arc (Windows beta) lives under %LOCALAPPDATA%\Packages\… which
    // needs a pattern walk; skipping for v1.
    ...(appData
      ? [{ name: "Opera", userDataDir: join(appData, "Opera Software", "Opera Stable") }]
      : []),
  ].filter((b) => existsSync(b.userDataDir));
}

/**
 * Return the AES-256-GCM key for a given Chromium "User Data"
 * directory by reading the `Local State` JSON and DPAPI-decrypting
 * `os_crypt.encrypted_key`.
 */
async function loadChromiumAesKey(userDataDir: string): Promise<Uint8Array> {
  const localState = resolve(userDataDir, "Local State");
  if (!existsSync(localState)) {
    throw new DpapiError(`Local State not found at ${localState}`);
  }
  const raw = readFileSync(localState, "utf8");
  const json = JSON.parse(raw) as {
    os_crypt?: { encrypted_key?: string };
  };
  const b64 = json.os_crypt?.encrypted_key;
  if (!b64) {
    throw new DpapiError("Local State has no os_crypt.encrypted_key");
  }
  const blob = new Uint8Array(Buffer.from(b64, "base64"));
  const dpapiBlob = stripChromiumKeyPrefix(blob);
  const key = await dpapiDecrypt(dpapiBlob);
  if (key.length !== 32) {
    throw new DpapiError(
      `DPAPI-decrypted AES key has unexpected length ${key.length} (expected 32)`,
    );
  }
  return key;
}

/**
 * Given an encrypted cookie blob (from Cookies.db `encrypted_value`
 * column) and the profile's AES key, return the plaintext.
 *
 * Format for modern Chromium (v10 onwards):
 *   bytes 0..2   : "v10" / "v11" / … prefix (ascii)
 *   bytes 3..14  : 12-byte nonce
 *   bytes 15..N-16: ciphertext
 *   bytes N-16..N: 16-byte GCM auth tag
 *
 * Web Crypto's AES-GCM wants the tag appended to the ciphertext,
 * which is exactly how Chromium stores it, so we just slice off the
 * prefix + nonce and feed the rest directly.
 */
async function decryptCookieValue(
  encrypted: Uint8Array,
  aesKey: Uint8Array,
): Promise<string> {
  if (encrypted.length < 3 + 12 + 16) {
    throw new Error("encrypted cookie value too short");
  }
  const prefix = Buffer.from(encrypted.slice(0, 3)).toString("ascii");
  if (prefix !== "v10" && prefix !== "v11") {
    throw new Error(`unexpected cookie value prefix: ${JSON.stringify(prefix)}`);
  }
  const nonce = encrypted.slice(3, 15);
  const ciphertextAndTag = encrypted.slice(15);

  // Copy into a tight ArrayBuffer so the Web Crypto type narrows
  // cleanly (the strict TS target rejects a shared-buffer Uint8Array).
  const keyBuf = new ArrayBuffer(aesKey.byteLength);
  new Uint8Array(keyBuf).set(aesKey);
  const cryptoKey = await crypto.subtle.importKey(
    "raw",
    keyBuf,
    { name: "AES-GCM" },
    false,
    ["decrypt"],
  );
  const nonceBuf = new ArrayBuffer(nonce.byteLength);
  new Uint8Array(nonceBuf).set(nonce);
  const ctBuf = new ArrayBuffer(ciphertextAndTag.byteLength);
  new Uint8Array(ctBuf).set(ciphertextAndTag);
  const plaintext = await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: nonceBuf, tagLength: 128 },
    cryptoKey,
    ctBuf,
  );
  return new TextDecoder("utf-8").decode(plaintext);
}

/**
 * Find a profile's Cookies SQLite path. Modern Chromium stores it
 * at `<profile>/Network/Cookies`; older versions used `<profile>/Cookies`.
 * We try the newer path first.
 */
function profileCookiesPath(profileDir: string): string | undefined {
  const newer = join(profileDir, "Network", "Cookies");
  if (existsSync(newer)) return newer;
  const older = join(profileDir, "Cookies");
  if (existsSync(older)) return older;
  return undefined;
}

/**
 * Read the AGENTS / Default / Profile N cookie row for a given
 * host. Returns the plaintext cookie value (just the value, not
 * name=value) or undefined if not found.
 *
 * Chrome holds an exclusive lock on the Cookies file while it's
 * running, so we copy the DB to a temp path, open the copy, and
 * clean up afterwards.
 */
async function readCookieFromProfile(
  profileDir: string,
  aesKey: Uint8Array,
  hostPatterns: string[],
  cookieName: string,
): Promise<string | undefined> {
  const cookiesPath = profileCookiesPath(profileDir);
  if (!cookiesPath) return undefined;

  // Copy to a sibling temp file to sidestep Chrome's exclusive
  // lock on the live Cookies DB. We use a PowerShell-backed file
  // copy that opens the source with FileShare.ReadWrite|Delete so
  // Chrome's held handle doesn't EBUSY us.
  const tmp = mkdtempSync(join(tmpdir(), "usage-buttons-cookies-"));
  const tmpDb = join(tmp, "Cookies.sqlite");
  try {
    copyLockedFile(cookiesPath, tmpDb);
    // Also copy the WAL + SHM if present so the DB is readable.
    for (const suffix of ["-wal", "-shm"]) {
      const src = cookiesPath + suffix;
      if (existsSync(src)) {
        try {
          copyLockedFile(src, tmpDb + suffix);
        } catch {
          // ignore — WAL file may be locked but main DB is usable
        }
      }
    }

    const db = new Database(tmpDb, { readonly: true });
    try {
      // Chromium uses SQL glob-style LIKE with host_key LIKE '%claude.ai'
      // — "%.claude.ai" for subdomain matches, ".claude.ai" or
      // "claude.ai" for exact matches depending on how the site set it.
      const placeholders = hostPatterns.map(() => "?").join(", ");
      const rows = db
        .query<
          { host_key: string; name: string; encrypted_value: Uint8Array },
          string[]
        >(
          `SELECT host_key, name, encrypted_value FROM cookies
           WHERE name = ? AND host_key IN (${placeholders})`,
        )
        .all(cookieName, ...hostPatterns);

      for (const row of rows) {
        try {
          const value = await decryptCookieValue(
            new Uint8Array(row.encrypted_value),
            aesKey,
          );
          if (value && value.length > 0) return value;
        } catch {
          // Try the next row; one bad entry doesn't doom the whole lookup.
        }
      }
      return undefined;
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

/**
 * List profile directories inside a Chromium User Data folder.
 * Returns `Default`, `Profile 1`, `Profile 2`, ... — whichever
 * exist. Order is stable (Default first).
 */
function listProfiles(userDataDir: string): string[] {
  const { readdirSync, statSync } = require("node:fs") as typeof import("node:fs");
  const names = readdirSync(userDataDir);
  const profiles: string[] = [];
  const defaultDir = join(userDataDir, "Default");
  if (existsSync(defaultDir)) profiles.push(defaultDir);
  for (const name of names) {
    if (!name.startsWith("Profile ")) continue;
    const full = join(userDataDir, name);
    try {
      if (statSync(full).isDirectory()) profiles.push(full);
    } catch {
      // skip
    }
  }
  return profiles;
}

/**
 * High-level: try every Chromium-family browser + every profile,
 * return the first plaintext claude.ai sessionKey we find. Returns
 * undefined (not an exception) when nothing is found so the caller
 * can fall through to the next source (Firefox, manual paste, …).
 *
 * Pass `cookieName` to change which cookie gets extracted; defaults
 * to `sessionKey` which is the one claude.ai uses.
 */
export async function findChromiumClaudeCookie(opts: {
  cookieName?: string;
  hostPatterns?: string[];
  onLog?: (message: string) => void;
} = {}): Promise<string | undefined> {
  const cookieName = opts.cookieName ?? "sessionKey";
  const hostPatterns = opts.hostPatterns ?? [
    ".claude.ai",
    "claude.ai",
    "www.claude.ai",
    ".anthropic.com",
    "anthropic.com",
  ];
  const log = opts.onLog ?? (() => {});

  const installs = listChromiumInstalls();
  if (installs.length === 0) {
    log("cookies: no chromium installs found");
    return undefined;
  }
  log(`cookies: scanning ${installs.length} chromium install(s)`);

  for (const browser of installs) {
    let aesKey: Uint8Array;
    try {
      aesKey = await loadChromiumAesKey(browser.userDataDir);
    } catch (err) {
      log(`cookies[${browser.name}] key load failed: ${String((err as Error).message ?? err)}`);
      continue;
    }
    const profiles = listProfiles(browser.userDataDir);
    for (const profile of profiles) {
      try {
        const value = await readCookieFromProfile(profile, aesKey, hostPatterns, cookieName);
        if (value) {
          log(`cookies[${browser.name}] found ${cookieName} in ${profile.split(/[\\/]/).pop()}`);
          return value;
        }
      } catch (err) {
        log(`cookies[${browser.name}] read failed: ${String((err as Error).message ?? err)}`);
      }
    }
  }
  return undefined;
}

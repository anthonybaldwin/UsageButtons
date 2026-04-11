/**
 * Credential-file helpers.
 *
 * Providers that read a JSON file for their OAuth tokens (Claude,
 * Codex, Gemini, …) share this reader. The reader is deliberately
 * lenient — missing files are a normal state ("user hasn't logged in
 * yet"), not an error condition the plugin should crash on.
 *
 * Writes are atomic: we write a `<file>.tmp` and rename over the
 * original so we never leave a half-written credentials file if the
 * process dies mid-refresh. This matters because losing the refresh
 * token would force the user to re-authenticate in the vendor CLI.
 */

import { existsSync } from "node:fs";
import { readFile, rename, writeFile, mkdir } from "node:fs/promises";
import { dirname } from "node:path";

export class CredentialNotFoundError extends Error {
  constructor(public readonly path: string) {
    super(`credential file not found: ${path}`);
    this.name = "CredentialNotFoundError";
  }
}

export class CredentialParseError extends Error {
  constructor(public readonly path: string, cause: unknown) {
    super(`failed to parse credential file ${path}: ${String(cause)}`);
    this.name = "CredentialParseError";
  }
}

/**
 * Read and parse a JSON credential file. Throws
 * `CredentialNotFoundError` if the file does not exist — callers
 * should treat that as "provider not configured", not a fatal error.
 */
export async function readJsonCredential<T>(path: string): Promise<T> {
  if (!existsSync(path)) throw new CredentialNotFoundError(path);
  let raw: string;
  try {
    raw = await readFile(path, "utf8");
  } catch (err) {
    throw new CredentialParseError(path, err);
  }
  try {
    return JSON.parse(raw) as T;
  } catch (err) {
    throw new CredentialParseError(path, err);
  }
}

/**
 * Write a JSON credential file atomically. Creates parent directories
 * if missing. Used by token-refresh flows that need to persist a new
 * access token without risking loss of the refresh token.
 */
export async function writeJsonCredential(
  path: string,
  data: unknown,
): Promise<void> {
  const dir = dirname(path);
  await mkdir(dir, { recursive: true });
  const tmp = `${path}.tmp`;
  await writeFile(tmp, JSON.stringify(data, null, 2), "utf8");
  await rename(tmp, path);
}

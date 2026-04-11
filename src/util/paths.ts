/**
 * Path helpers. Every file path a provider reads goes through here so
 * there is exactly one place to reason about Windows vs macOS vs
 * $HOME-override semantics.
 */

import { homedir } from "node:os";
import { resolve } from "node:path";

/** Resolve `~/relative` to an absolute path under the current user's home. */
export function userHome(...parts: string[]): string {
  return resolve(homedir(), ...parts);
}

/** ~/.claude/.credentials.json (Anthropic Claude CLI OAuth credentials). */
export function claudeCredentialsPath(): string {
  return userHome(".claude", ".credentials.json");
}

/** ~/.codex/auth.json (OpenAI Codex CLI OAuth credentials). */
export function codexAuthPath(): string {
  return userHome(".codex", "auth.json");
}

/**
 * ~/.codexbar/config.json — the CodexBar shared config file. We read
 * it opportunistically so a user who already runs CodexBar on a Mac
 * shares their provider toggles / tokens across both.
 */
export function codexbarConfigPath(): string {
  return userHome(".codexbar", "config.json");
}

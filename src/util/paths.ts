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

/**
 * ~/.codex/auth.json (OpenAI Codex CLI OAuth credentials).
 * Respects `CODEX_HOME` the same way the `codex` CLI does.
 */
export function codexAuthPath(): string {
  const override = process.env["CODEX_HOME"];
  if (override && override.trim() !== "") {
    return resolve(override, "auth.json");
  }
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

// ── Cost scanner log directories ───────────────────────────────

/**
 * Claude JSONL session log root. The Claude CLI writes project-scoped
 * logs under `~/.claude/projects/` (or `$CLAUDE_CONFIG_DIR/projects/`).
 */
export function claudeProjectsDir(): string {
  const override = process.env["CLAUDE_CONFIG_DIR"];
  if (override && override.trim() !== "") {
    return resolve(override, "projects");
  }
  return userHome(".claude", "projects");
}

/**
 * Codex JSONL session log root. The Codex CLI writes per-day session
 * logs under `~/.codex/sessions/` (or `$CODEX_HOME/sessions/`).
 */
export function codexSessionsDir(): string {
  const override = process.env["CODEX_HOME"];
  if (override && override.trim() !== "") {
    return resolve(override, "sessions");
  }
  return userHome(".codex", "sessions");
}

// ── Provider credential / config directories ───────────────────

/**
 * Copilot hosts.json — written by `gh auth login` or `gh copilot`.
 * Lives at `~/.config/github-copilot/hosts.json` on all platforms.
 */
export function copilotHostsPath(): string {
  return userHome(".config", "github-copilot", "hosts.json");
}

/**
 * Copilot apps.json — fallback OAuth file written by newer gh versions.
 */
export function copilotAppsPath(): string {
  return userHome(".config", "github-copilot", "apps.json");
}
